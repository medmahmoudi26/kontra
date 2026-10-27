package mcprelay

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"kontra.sh/kontra/internal/config"
	"kontra.sh/kontra/internal/hooks"
	"kontra.sh/kontra/internal/ids"
	"kontra.sh/kontra/internal/lifecycle"
	"kontra.sh/kontra/internal/mcpwire"
	"kontra.sh/kontra/internal/store"
	"kontra.sh/kontra/internal/strictjson"
)

// Tasks is the client side of gate-owned MCP tasks (spec/relay.md §11.6–§11.8 for legacy readers, §11.12 for
// modern ones). It reads only Postgres, on any replica; a task of another buyer is answered as an unknown one
// (-32602). A task may be read from either era by its buyer, and each era presents the same log:
//
//   - legacy: tasks/get (the projection, operator mcpMeta), tasks/result (blocks on LISTEN until terminal,
//     delivering pending input requests first on its own SSE stream, event ids k.<taskId>.<seq>; then §11.8's
//     stored outcome with the related-task key), tasks/list (the principal's own, newest first, gate cursors;
//     refused for anonymous), tasks/cancel (kontra.cancel, D57: canceled within 10 s whatever the upstream
//     answers). A client response whose id is a gate request id k.<taskId>.<seq> is an input_response, accepted
//     once and only from the buyer (else 202 and dropped). A GET whose Last-Event-ID is k.<taskId>.<n> is served
//     from task_events, for the same principal only.
//   - modern: tasks/get (resultType complete, inputRequests keyed by gate id, a terminal task presented by its
//     terminal event: a stored CallToolResult is completed even with isError), tasks/update (each outstanding
//     key logged once, kontra.input_response signalled), tasks/cancel (kontra.cancel, not awaited, empty ack);
//     tasks/result and tasks/list are -32601; a request that does not declare the extension is 400 with -32021
//     (data.requiredCapabilities naming it, D176). A client that closes its stream during the 2 s inline wait,
//     before the outcome or the CreateTaskResult is sent, makes the gate send the task kontra.cancel (D178).
//
// Work packages: M3 WP-06 (legacy, and Create for both eras), M3 WP-07 (modern).
type Tasks struct {
	log       *store.Log
	waker     store.TaskWaker
	lc        TaskLifecycle
	hooks     lifecycle.HooksSnapshot
	rules     hooks.KeyRules
	logger    *slog.Logger
	keepAlive time.Duration // a gate-served stream's comment interval (§2.2)
	reread    time.Duration // how often a blocked tasks/result reads the log without a wake
}

// TaskLifecycle is what the task side asks of TaskLifecycle (*lifecycle.Client).
type TaskLifecycle interface {
	StartWithMessage(ctx context.Context, in lifecycle.Input, workItemID string) (lifecycle.MessageResult, error)
	Cancel(ctx context.Context, taskID a2a.TaskID) (lifecycle.CancelResult, bool, error)
	SignalInputResponse(ctx context.Context, taskID a2a.TaskID, seq int64) error
}

// TasksConfig configures Tasks.
type TasksConfig struct {
	Log       *store.Log
	Waker     store.TaskWaker // the replica's LISTEN connection (*pg.Listener)
	Lifecycle TaskLifecycle
	Hooks     lifecycle.HooksSnapshot // the hook configuration a new task's workflow snapshots (D46)
	Rules     hooks.KeyRules          // §13.2's rules for an Admission decision's metadata and mcpMeta
	Logger    *slog.Logger
}

// NewTasks checks cfg.
func NewTasks(cfg TasksConfig) (*Tasks, error) {
	if cfg.Log == nil || cfg.Waker == nil || cfg.Lifecycle == nil {
		return nil, errors.New("mcprelay: gate-owned tasks need the log, the waker and the lifecycle client")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Tasks{log: cfg.Log, waker: cfg.Waker, lc: cfg.Lifecycle, hooks: cfg.Hooks, rules: cfg.Rules,
		logger: cfg.Logger.With("component", "mcp-tasks"), keepAlive: keepAliveEvery, reread: 5 * time.Second}, nil
}

// CreateTask is what a task-augmented call stores.
type CreateTask struct {
	// TaskID is the id Admission was asked with (operation task, §13.1); minted when Nil.
	TaskID    uuid.UUID
	Service   store.ServiceRef
	BuyerID   uuid.UUID
	Principal string
	Via       string
	Era       mcpwire.Era
	Version   string          // the creating request's protocol version
	Request   []byte          // the tools/call as received after §2.5 (the work item, never in Temporal history)
	Tool      string          // its params.name
	RequestID json.RawMessage // the client's JSON-RPC id as sent, reused upstream (§11.5 step 3)
	// MCPSession is the creating client session's hash (legacy), for the preamble (tunnel.md §8.1).
	MCPSession *[32]byte
	// ClientCapabilities are the capabilities the client declared, whose elicitation and sampling the dispatch's own
	// initialize declares (§11.5 step 3).
	ClientCapabilities json.RawMessage
	Sealed             bool           // the service's sealed flag (D174)
	Decision           hooks.Decision // Admission's: allow, or quote with its terms; its metadata and mcpMeta
}

// errAdmission is an Admission decision a task cannot be created with (an unparsable quote, keys outside §13.2):
// the request is answered "admission unavailable" (it fails closed, §13.1).
var errAdmission = errors.New("mcprelay: admission unavailable")

// Create creates a gate-owned task for an admitted task-augmented call (§11.5 steps 1–2, §11.12 steps 2–4): the
// task row (protocol mcp; created quoted on a quote, with the approve URL, D21), the work item holding the request
// with the client session's hash and JSON-RPC id, in one transaction, and the TaskLifecycle start. It returns once
// the row is committed and the start was issued. A start that fails leaves the row (the orphan sweep ends it
// lifecycle_unavailable after 2 minutes, D63) and is an error.
func (t *Tasks) Create(ctx context.Context, req CreateTask) (uuid.UUID, error) {
	if len(req.Request) == 0 || req.Tool == "" || len(req.RequestID) == 0 {
		return uuid.Nil, errors.New("mcprelay: a gate-owned task needs its tools/call, its tool and its id")
	}
	svc, err := t.log.LoadService(ctx, req.Service.TenantID, req.Service.Name)
	if err != nil {
		return uuid.Nil, err
	}
	d := req.Decision
	if err := t.rules.CheckOperatorMaps(d.Metadata, d.MCPMeta); err != nil {
		t.logger.Warn("an Admission decision's metadata was refused; the task is not created", "service", svc.Name, "error", err)
		return uuid.Nil, errAdmission
	}
	state, approveURL := store.StateSubmitted, ""
	var quoteExpires *time.Time
	if d.Outcome == hooks.Quote {
		if d.Quote == nil || d.Quote.ApproveURL == "" {
			return uuid.Nil, errAdmission
		}
		exp, err := time.Parse(time.RFC3339, d.Quote.ExpiresAt)
		if err != nil {
			return uuid.Nil, errAdmission
		}
		exp = exp.UTC()
		state, approveURL, quoteExpires = store.StateQuoted, d.Quote.ApproveURL, &exp
	}
	id := req.TaskID
	if id == uuid.Nil {
		if id, err = uuid.NewV7(); err != nil {
			return uuid.Nil, err
		}
	}
	owner := store.Owner{BuyerID: req.BuyerID, Principal: req.Principal, Via: req.Via}
	nt, err := store.NewMCPTask(id, svc, owner, state, quoteExpires, approveURL, d.Metadata, d.MCPMeta)
	if err != nil {
		return uuid.Nil, err
	}
	work := &store.MCPWork{Request: bytes.Clone(req.Request), Tool: req.Tool, RequestID: req.RequestID, Version: req.Version,
		ClientCapabilities: req.ClientCapabilities, Sealed: req.Sealed}
	if req.MCPSession != nil {
		work.Session = hex.EncodeToString(req.MCPSession[:])
	}
	wid, err := ids.NewWorkItemID()
	if err != nil {
		return uuid.Nil, err
	}
	err = pgx.BeginFunc(ctx, t.log.DB(), func(tx pgx.Tx) error {
		if _, err := t.log.CreateTask(ctx, tx, nt); err != nil {
			return err
		}
		return t.log.InsertWorkItem(ctx, tx, store.WorkItem{ID: wid, TaskID: id, Kind: store.WorkExecute,
			Payload: store.WorkPayload{Owner: owner, MCP: work}})
	})
	if err != nil {
		return uuid.Nil, err
	}
	in := lifecycle.Input{
		TaskID: id.String(), TimeoutSeconds: int64(svc.Timeout / time.Second), Retries: svc.Retries, Hooks: t.hooks,
		QuoteExpiresAt: quoteExpires, Protocol: lifecycle.ProtocolMCP,
	}
	if _, err := t.lc.StartWithMessage(context.WithoutCancel(ctx), in, wid.String()); err != nil {
		return id, fmt.Errorf("mcprelay: start the lifecycle of task %s: %w", id, err)
	}
	return id, nil
}

// Caller is who asks about a task: the route's service and the authenticated principal. Tasks are bound to the
// principal's buyer (§11.6); a resumed stream to the principal itself (§11.7).
type Caller struct {
	Service   store.ServiceRef
	BuyerID   uuid.UUID
	Principal string
	Anonymous bool
}

func (c Caller) scope() store.Scope {
	return store.Scope{TenantID: c.Service.TenantID, Service: c.Service.Name, BuyerID: c.BuyerID}
}

// errTaskNotFound is the answer for a task id that is unknown, malformed, of another buyer, or not an MCP task of
// this service (§4, §11.6): -32602, as MCP answers an unknown task.
var errTaskNotFound = &mcpwire.Error{Code: mcpwire.CodeInvalidParams, Message: "Failed to retrieve task: Task not found"}

// errLifecycleUnavailable is a kontra.cancel that no lifecycle worker accepted in time (§14.1): -32603, HTTP 503.
var errLifecycleUnavailable = errors.New("mcprelay: the task's lifecycle is unavailable")

// load is the caller's MCP task named by params.taskId, nil when there is none.
func (t *Tasks) load(ctx context.Context, who Caller, params json.RawMessage) (*store.TaskRow, error) {
	s, ok := stringMember(params, "taskId")
	if !ok {
		return nil, nil
	}
	return t.loadID(ctx, who, s)
}

func (t *Tasks) loadID(ctx context.Context, who Caller, s string) (*store.TaskRow, error) {
	id, err := ids.ParseTaskID(s)
	if err != nil {
		return nil, nil
	}
	row, err := t.log.LoadScoped(ctx, who.Service, store.Owner{BuyerID: who.BuyerID}, id)
	switch {
	case errors.Is(err, a2a.ErrTaskNotFound):
		return nil, nil
	case err != nil:
		return nil, err
	case row.Protocol != store.ProtocolMCP:
		return nil, nil
	}
	return row, nil
}

// policy is the service's timeout and retention as its row has them (a task's ttl, §11.6).
func (t *Tasks) policy(ctx context.Context, svc store.ServiceRef) (store.ServiceRef, error) {
	if svc.Retention > 0 {
		return svc, nil
	}
	return t.log.LoadService(ctx, svc.TenantID, svc.Name)
}

// Serve answers a legacy tasks/get, tasks/list or tasks/cancel (§11.6) for the caller: the result, or the
// JSON-RPC error to answer with (an error is the gate's own failure). tasks/result is Result's: it may stream.
func (t *Tasks) Serve(ctx context.Context, who Caller, m *mcpwire.Message) (json.RawMessage, *mcpwire.Error, error) {
	switch m.Method {
	case "tasks/get":
		row, err := t.load(ctx, who, m.Params)
		if err != nil || row == nil {
			return nil, errTaskNotFound, err
		}
		pol, err := t.policy(ctx, who.Service)
		if err != nil {
			return nil, nil, err
		}
		res, err := taskResult(row, pol)
		return res, nil, err
	case "tasks/list":
		return t.list(ctx, who, m.Params)
	case "tasks/cancel":
		return t.cancel(ctx, who, m.Params)
	}
	return nil, &mcpwire.Error{Code: mcpwire.CodeMethodNotFound, Message: "Method not found"}, nil
}

// list is tasks/list: the buyer's own MCP tasks of this service, newest first, a gate-minted cursor; refused for
// anonymous principals, whose tasks share one buyer (§2.3, D20).
func (t *Tasks) list(ctx context.Context, who Caller, params json.RawMessage) (json.RawMessage, *mcpwire.Error, error) {
	if who.Anonymous {
		return nil, &mcpwire.Error{Code: mcpwire.CodeMethodNotFound, Message: "tasks/list is not offered to anonymous callers"}, nil
	}
	cursor := ""
	if v, ok := mcpwire.Member(params, "cursor"); ok && string(bytes.TrimSpace(v)) != "null" {
		s, ok := stringMember(params, "cursor")
		if !ok {
			return nil, &mcpwire.Error{Code: mcpwire.CodeInvalidParams, Message: "Invalid cursor"}, nil
		}
		cursor = s
	}
	rows, next, err := t.log.ListMCP(ctx, who.scope(), cursor)
	if errors.Is(err, store.ErrBadCursor) {
		return nil, &mcpwire.Error{Code: mcpwire.CodeInvalidParams, Message: "Invalid cursor"}, nil
	}
	if err != nil {
		return nil, nil, err
	}
	pol, err := t.policy(ctx, who.Service)
	if err != nil {
		return nil, nil, err
	}
	tasks := make([]json.RawMessage, 0, len(rows))
	for _, r := range rows {
		obj, err := taskObject(r, pol)
		if err != nil {
			return nil, nil, err
		}
		tasks = append(tasks, obj)
	}
	out := map[string]any{"tasks": tasks}
	if next != "" {
		out["nextCursor"] = next
	}
	b, err := json.Marshal(out)
	return b, nil, err
}

// cancel is tasks/cancel (§11.6, D57): -32602 for a terminal task; otherwise kontra.cancel, which writes canceled
// within 10 s whatever the upstream answers, and the task as it is then (status cancelled).
func (t *Tasks) cancel(ctx context.Context, who Caller, params json.RawMessage) (json.RawMessage, *mcpwire.Error, error) {
	row, err := t.load(ctx, who, params)
	if err != nil || row == nil {
		return nil, errTaskNotFound, err
	}
	if row.State.Terminal() {
		return nil, notCancelable(row.State), nil
	}
	_, _, err = t.lc.Cancel(ctx, a2a.TaskID(row.ID.String()))
	switch {
	case errors.Is(err, a2a.ErrTaskNotCancelable):
	case errors.Is(err, lifecycle.ErrUnavailable):
		return nil, nil, fmt.Errorf("%w: %w", errLifecycleUnavailable, err)
	case err != nil:
		return nil, nil, err
	}
	// A cancel that did not complete in time (ok false) answers the task as it is (§8).
	if row, err = t.log.Load(ctx, row.ID); err != nil {
		return nil, nil, err
	}
	if row.State.Terminal() && row.State != store.StateCanceled {
		return nil, notCancelable(row.State), nil
	}
	pol, err := t.policy(ctx, who.Service)
	if err != nil {
		return nil, nil, err
	}
	res, err := taskResult(row, pol)
	return res, nil, err
}

func notCancelable(s store.State) *mcpwire.Error {
	return &mcpwire.Error{Code: mcpwire.CodeInvalidParams, Message: "Cannot cancel task in terminal status: " + s.MCPStatus()}
}

// mcpTime is an MCP timestamp: ISO 8601 in UTC, to the millisecond.
func mcpTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// ttlOf is a task's ttl in milliseconds (§11.6): retention_until − created_at once terminal; before, an upper
// bound: the deadline (a quoted task's: its quote's expiry plus the task timeout) plus the retention.
func ttlOf(row *store.TaskRow, pol store.ServiceRef) int64 {
	var end time.Time
	switch {
	case row.RetentionUntil != nil:
		end = *row.RetentionUntil
	case row.Deadline != nil:
		end = row.Deadline.Add(pol.Retention)
	case row.QuoteExpiresAt != nil:
		end = row.QuoteExpiresAt.Add(pol.Timeout + pol.Retention)
	default:
		end = row.CreatedAt.Add(pol.Timeout + pol.Retention)
	}
	return max(end.Sub(row.CreatedAt).Milliseconds(), 0)
}

// taskObject is an MCP 2025-11-25 Task as the gate shows its task (§11.6): taskId, status (§5.1's mapping),
// statusMessage (a quoted task's approve URL, D21; a failure's reason text, §11.8), createdAt, lastUpdatedAt, ttl
// and pollInterval 1000.
func taskObject(row *store.TaskRow, pol store.ServiceRef) (json.RawMessage, error) {
	type task struct {
		TaskID        string `json:"taskId"`
		Status        string `json:"status"`
		StatusMessage string `json:"statusMessage,omitempty"`
		CreatedAt     string `json:"createdAt"`
		LastUpdatedAt string `json:"lastUpdatedAt"`
		TTL           int64  `json:"ttl"`
		PollInterval  int    `json:"pollInterval"`
	}
	return json.Marshal(task{
		TaskID: row.ID.String(), Status: row.State.MCPStatus(), StatusMessage: store.MCPStatusText(row.Snapshot),
		CreatedAt: mcpTime(row.CreatedAt), LastUpdatedAt: mcpTime(row.StatusAt), TTL: ttlOf(row, pol),
		PollInterval: store.MCPPollInterval,
	})
}

// taskMeta is the _meta the gate renders for a task (§11.8, §13.3): the operator's mcpMeta verbatim, and for a
// failed task sh.kontra/failure.
func taskMeta(row *store.TaskRow) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	for k, v := range store.MCPMeta(row.Snapshot) {
		if b, err := json.Marshal(v); err == nil {
			out[k] = b
		}
	}
	if row.State == store.StateFailed && row.Snapshot != nil {
		if r, ok := row.Snapshot.Metadata[store.MetaFailure].(string); ok && r != "" {
			b, _ := json.Marshal(r)
			out[mcpwire.KontraPrefix+"failure"] = b
		}
	}
	return out
}

// taskResult is tasks/get's (and tasks/cancel's) result: the Task with the task's _meta.
func taskResult(row *store.TaskRow, pol store.ServiceRef) (json.RawMessage, error) {
	obj, err := taskObject(row, pol)
	if err != nil {
		return nil, err
	}
	return mcpwire.MergeMeta(obj, taskMeta(row))
}

// createTaskResult is the answer to a task-augmented tools/call (§11.5 step 2): {"task": Task, "_meta"}.
func createTaskResult(row *store.TaskRow, pol store.ServiceRef) (json.RawMessage, error) {
	obj, err := taskObject(row, pol)
	if err != nil {
		return nil, err
	}
	res, err := json.Marshal(map[string]json.RawMessage{"task": obj})
	if err != nil {
		return nil, err
	}
	return mcpwire.MergeMeta(res, taskMeta(row))
}

// Result answers a legacy tasks/result (§11.6, §11.7): it blocks, on the replica's LISTEN connection, until the
// task is terminal, delivering every input request no answer has been logged for on its own event stream first
// (event and JSON-RPC ids k.<taskId>.<seq>), then §11.8's outcome. A task that is terminal when asked is answered
// in JSON. sid is the client's gate session id, for the response's Mcp-Session-Id.
func (t *Tasks) Result(ctx context.Context, w http.ResponseWriter, sid string, who Caller, m *mcpwire.Message) {
	row, err := t.load(ctx, who, m.Params)
	if err != nil {
		t.logger.Warn("tasks/result: the task could not be read", "error", err)
		writeError(w, http.StatusServiceUnavailable, m.ID, mcpwire.CodeInternal, "the gate could not serve the request", nil)
		return
	}
	if row == nil {
		writeRPCError(w, sid, m.ID, errTaskNotFound)
		return
	}
	wake, stop := t.waker.WaitTask(ctx, row.ID)
	defer stop()
	stream := newGateStream(w, sid)
	delivered := map[int64]bool{}
	tick := time.NewTicker(t.keepAlive)
	defer tick.Stop()
	reread := time.NewTicker(t.reread)
	defer reread.Stop()
	for {
		row, err = t.log.Load(ctx, row.ID)
		if err != nil {
			if !stream.started {
				writeError(w, http.StatusServiceUnavailable, m.ID, mcpwire.CodeInternal, "the gate could not serve the request", nil)
			}
			return
		}
		if row.State.Terminal() {
			msg, seq, err := t.outcome(ctx, row, m.ID)
			if err != nil {
				t.logger.Warn("tasks/result: the outcome could not be read", "task_id", row.ID, "error", err)
				if !stream.started {
					writeError(w, http.StatusServiceUnavailable, m.ID, mcpwire.CodeInternal, "the gate could not serve the request", nil)
				}
				return
			}
			if !stream.started {
				w.Header().Set("Content-Type", "application/json")
				if sid != "" {
					w.Header().Set(mcpwire.HeaderSessionID, sid)
				}
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(msg)
				return
			}
			_ = stream.event(store.GateRequestID(row.ID, seq), msg)
			return
		}
		if err := stream.start(); err != nil {
			return
		}
		pending, err := t.log.PendingInputRequests(ctx, row.ID)
		if err != nil {
			return
		}
		for _, r := range pending {
			if delivered[r.Seq] {
				continue
			}
			if err := stream.event(r.GateID, r.Request); err != nil {
				return
			}
			delivered[r.Seq] = true
		}
		select {
		case <-ctx.Done():
			return
		case <-wake:
		case <-reread.C:
		case <-tick.C:
			if stream.keepAlive() != nil {
				return
			}
		}
	}
}

// outcome is a terminal task's tasks/result answer to the request id (§11.8): the upstream's stored result (with
// the related-task key and the task's _meta) or JSON-RPC error, verbatim; a failure the gate wrote as -32603 with
// its reason text and data sh.kontra/failure; a cancel as -32603 "task cancelled" with data sh.kontra/canceled.
func (t *Tasks) outcome(ctx context.Context, row *store.TaskRow, reqID json.RawMessage) ([]byte, int64, error) {
	o, ok, err := t.log.MCPTerminal(ctx, row.ID)
	if err != nil {
		return nil, 0, err
	}
	if !ok {
		return nil, 0, fmt.Errorf("mcprelay: task %s is %s without its terminal row", row.ID, row.State)
	}
	b, err := outcomeMessage(row, o, reqID)
	return b, o.Seq, err
}

// outcomeMessage is outcome's answer for the terminal row o.
func outcomeMessage(row *store.TaskRow, o store.MCPOutcome, reqID json.RawMessage) ([]byte, error) {
	if o.Kind == store.KindResult {
		if len(o.Error) > 0 {
			return messageBytes(reqID, "error", o.Error)
		}
		meta := taskMeta(row)
		delete(meta, mcpwire.KontraPrefix+"failure")
		rel, _ := json.Marshal(map[string]string{"taskId": row.ID.String()})
		meta[mcpwire.MetaRelatedTask] = rel
		res, err := mcpwire.MergeMeta(o.Result, meta)
		if err != nil {
			return nil, err
		}
		return messageBytes(reqID, "result", res)
	}
	md := map[string]any{}
	if row.Snapshot != nil {
		md = row.Snapshot.Metadata
	}
	switch row.State {
	case store.StateCanceled:
		var data any
		if c, ok := md[store.MetaCanceled].(string); ok && c != "" {
			data = map[string]any{mcpwire.KontraPrefix + "canceled": c}
		}
		return mcpwire.ErrorBody(reqID, mcpwire.CodeInternal, "task cancelled", data)
	default:
		reason, _ := md[store.MetaFailure].(string)
		if reason == "" {
			reason = string(store.ReasonUpstreamError)
		}
		text := store.MCPStatusText(row.Snapshot)
		if text == "" {
			text = store.ReasonText(store.FailureReason(reason))
		}
		return mcpwire.ErrorBody(reqID, mcpwire.CodeInternal, text, map[string]any{mcpwire.KontraPrefix + "failure": reason})
	}
}

// messageBytes is a JSON-RPC response with member ("result" or "error") raw.
func messageBytes(id json.RawMessage, member string, raw json.RawMessage) ([]byte, error) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	var b bytes.Buffer
	b.WriteString(`{"jsonrpc":"2.0","id":`)
	b.Write(id)
	b.WriteString(`,"` + member + `":`)
	b.Write(raw)
	b.WriteByte('}')
	return b.Bytes(), nil
}

// writeRPCError writes a JSON-RPC error answer with status 200 and the gate's session id.
func writeRPCError(w http.ResponseWriter, sid string, id json.RawMessage, e *mcpwire.Error) {
	if sid != "" {
		w.Header().Set(mcpwire.HeaderSessionID, sid)
	}
	writeError(w, http.StatusOK, id, e.Code, e.Message, e.Data)
}

// Respond takes a client's JSON-RPC response whose id is a gate request id (§11.5 step 5, §11.7): the answer to
// the input request at that seq of a task of the caller's buyer is logged as an input_response event once, under
// the task's version, with the sealed envelope its result carries kept on a sealed service (D174), and
// kontra.input_response is signalled. The client gets 202 whatever happens (an unknown, foreign or already
// answered id is dropped); it reports whether the response was logged.
func (t *Tasks) Respond(ctx context.Context, who Caller, m *mcpwire.Message, body []byte, sealed bool) bool {
	var s string
	if json.Unmarshal(m.ID, &s) != nil {
		return false
	}
	taskID, seq, ok := store.ParseGateRequestID(s)
	if !ok {
		return false
	}
	row, err := t.loadID(ctx, who, taskID.String())
	if err != nil || row == nil || row.State.Terminal() {
		return false
	}
	reqs, err := t.log.InputRequests(ctx, taskID, seq-1)
	if err != nil || len(reqs) == 0 || reqs[0].Seq != seq {
		return false
	}
	scrubbed, err := mcpwire.ScrubMessage(mcpwire.FromClient, body, m, reqs[0].Method, sealed)
	if err != nil {
		return false
	}
	at, ok, err := t.log.AppendInputResponse(ctx, who.scope(), taskID, seq, scrubbed)
	if err != nil {
		t.logger.Warn("an answer to an input request was not logged", "task_id", taskID, "error", err)
		return false
	}
	if !ok {
		t.logger.Info("an answer to an input request was dropped: already answered", "task_id", taskID, "request_seq", seq)
		return false
	}
	if err := t.lc.SignalInputResponse(context.WithoutCancel(ctx), a2a.TaskID(taskID.String()), at); err != nil {
		// The running dispatch tails the log; the signal only updates workflow state (§14.1).
		t.logger.Warn("kontra.input_response was not signalled", "task_id", taskID, "error", err)
	}
	return true
}

// Resume serves a GET whose Last-Event-ID is a gate event id k.<taskId>.<n> (§11.7), from task_events, on any
// replica, for the principal of the task only (404 otherwise, opening nothing): the input requests logged after n
// that no answer has been logged for, then each new one, until the task is terminal, when the stream ends.
//
// SPEC-CONFLICT (relay §11.7 with §11.6): the stream resumed is a tasks/result response stream, whose last event is
// the answer to that tasks/result request; the event id k.<taskId>.<seq> names the task and the seq, not the
// request, and the gate keeps no per-stream state on any replica, so a resumed stream cannot write that answer
// with its request's id. The closest faithful behaviour: the resumed stream carries every pending and new input
// request with its gate id and ends when the task is terminal; the client's tasks/result, asked again, returns the
// stored outcome at once.
func (t *Tasks) Resume(ctx context.Context, w http.ResponseWriter, sid string, who Caller, lastEventID string) {
	taskID, after, ok := store.ParseGateRequestID(lastEventID)
	var row *store.TaskRow
	if ok {
		var err error
		if row, err = t.loadID(ctx, who, taskID.String()); err != nil {
			writeEmpty(w, http.StatusServiceUnavailable)
			return
		}
	}
	if row == nil || row.Owner.Principal != who.Principal {
		writeEmpty(w, http.StatusNotFound)
		return
	}
	wake, stop := t.waker.WaitTask(ctx, taskID)
	defer stop()
	stream := newGateStream(w, sid)
	if stream.start() != nil {
		return
	}
	delivered := map[int64]bool{}
	tick := time.NewTicker(t.keepAlive)
	defer tick.Stop()
	reread := time.NewTicker(t.reread)
	defer reread.Stop()
	for {
		pending, err := t.log.PendingInputRequests(ctx, taskID)
		if err != nil {
			return
		}
		for _, r := range pending {
			if r.Seq <= after || delivered[r.Seq] {
				continue
			}
			if stream.event(r.GateID, r.Request) != nil {
				return
			}
			delivered[r.Seq] = true
		}
		row, err := t.log.Load(ctx, taskID)
		if err != nil || row.State.Terminal() {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-wake:
		case <-reread.C:
		case <-tick.C:
			if stream.keepAlive() != nil {
				return
			}
		}
	}
}

// --- the legacy handler's gate-owned tasks (§11.5–§11.7) ---

// caller is the request's Caller.
func (h *Handler) caller(rq *request) Caller {
	return Caller{Service: rq.svc.Ref, BuyerID: rq.p.BuyerID, Principal: rq.p.ID, Anonymous: !rq.p.Authenticated()}
}

// serveTask answers tasks/* on a durable service's session from Postgres (§11.6): tasks/result through
// Tasks.Result, which may stream; the rest through Tasks.Serve.
func (h *Handler) serveTask(rq *request) {
	m := rq.msg
	if m.Method == "tasks/result" {
		h.deps.Tasks.Result(rq.ctx(), rq.w, rq.sid, h.caller(rq), m)
		return
	}
	res, rpcErr, err := h.deps.Tasks.Serve(rq.ctx(), h.caller(rq), m)
	switch {
	case errors.Is(err, errLifecycleUnavailable):
		h.log.Warn("mcp: a task's lifecycle did not accept the request in time", "service", rq.svc.Ref.Name, "method", m.Method, "error", err)
		writeError(rq.w, http.StatusServiceUnavailable, m.ID, mcpwire.CodeInternal, "the task's lifecycle is unavailable", nil)
	case err != nil:
		h.internal(rq, "serve "+m.Method, err)
	case rpcErr != nil:
		writeRPCError(rq.w, rq.sid, m.ID, rpcErr)
	default:
		writeResult(rq.w, rq.sid, m.ID, res)
	}
}

// writeResult writes a JSON-RPC result answer with status 200 and the gate's session id.
func writeResult(w http.ResponseWriter, sid string, id json.RawMessage, res json.RawMessage) {
	b, _ := messageBytes(id, "result", res)
	w.Header().Set("Content-Type", "application/json")
	if sid != "" {
		w.Header().Set(mcpwire.HeaderSessionID, sid)
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

// durableCall is a task-augmented tools/call on a durable service (§11.5 steps 0–2), the tool already resolved
// against the cached tools/list (an unknown or denied one was -32602): a tool whose execution.taskSupport is absent
// or forbidden is -32601, with no Admission and no task; then Admission with operation task (a quote creates the
// task quoted, §13.2), the gate task and its work item, the TaskLifecycle start, and the CreateTaskResult, with
// pollInterval 1000, once the row is committed. Nothing is forwarded on the client's session: the dispatch opens
// its own upstream session (D22).
func (h *Handler) durableCall(rq *request, name string, tool json.RawMessage) {
	m := rq.msg
	if !taskSupported(tool) {
		writeError(rq.w, http.StatusOK, m.ID, mcpwire.CodeMethodNotFound, "Tool "+name+" does not support task-augmented execution", nil)
		return
	}
	taskID, d, ref := h.admitTask(rq, name)
	if ref != nil {
		writeRefusal(rq.w, ref)
		return
	}
	body, err := mcpwire.ScrubMessage(mcpwire.FromClient, rq.body, m, "", h.sealed(rq))
	if err != nil {
		h.internal(rq, "scrub the request", err)
		return
	}
	hash := rq.sess.Hash
	version := rq.r.Header.Get(mcpwire.HeaderProtocolVersion)
	if version == "" {
		version = rq.sess.ProtocolVersion
	}
	id, err := h.deps.Tasks.Create(rq.ctx(), CreateTask{
		TaskID: taskID, Service: rq.svc.Ref, BuyerID: rq.p.BuyerID, Principal: rq.p.ID, Via: string(rq.p.Via),
		Era: mcpwire.EraLegacy, Version: version, Request: body, Tool: name, RequestID: m.ID, MCPSession: &hash,
		ClientCapabilities: rq.sess.ClientCaps, Sealed: h.sealed(rq), Decision: d,
	})
	switch {
	case errors.Is(err, errAdmission):
		writeError(rq.w, http.StatusServiceUnavailable, m.ID, mcpwire.CodeInternal, "admission unavailable", nil)
		return
	case errors.Is(err, lifecycle.ErrUnavailable):
		h.log.Warn("mcp: a task's lifecycle did not start in time; the orphan sweep ends it", "service", rq.svc.Ref.Name, "task_id", id, "error", err)
		writeError(rq.w, http.StatusServiceUnavailable, m.ID, mcpwire.CodeInternal, "the task's lifecycle is unavailable", nil)
		return
	case err != nil:
		h.internal(rq, "create the task", err)
		return
	}
	row, err := h.deps.Tasks.log.Load(rq.ctx(), id)
	if err != nil {
		h.internal(rq, "read the task", err)
		return
	}
	pol, err := h.deps.Tasks.policy(rq.ctx(), rq.svc.Ref)
	if err != nil {
		h.internal(rq, "read the service", err)
		return
	}
	res, err := createTaskResult(row, pol)
	if err != nil {
		h.internal(rq, "write the task", err)
		return
	}
	h.log.Info("mcp task created", "service", rq.svc.Ref.Name, "task_id", id, "state", row.State, "principal", rq.p.ID)
	writeResult(rq.w, rq.sid, m.ID, res)
}

// admitTask consults Admission with operation task for a durable task (§13.1): the pre-minted task id, and the
// decision (allow, or a quote); a denial or a hook that fails is the refusal to write. Without the hook for task,
// the built-in allows and the id is minted here.
func (h *Handler) admitTask(rq *request, tool string) (uuid.UUID, hooks.Decision, *mcpwire.Refusal) {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.Nil, hooks.Decision{}, refusal(http.StatusServiceUnavailable, rq.msg.ID, mcpwire.CodeInternal, "admission unavailable")
	}
	if !slices.Contains(h.deps.AdmissionOps, hooks.OpTask) {
		return id, hooks.Decision{Outcome: hooks.Allow}, nil
	}
	ref := rq.svc.Ref
	ar := hooks.AdmitRequest{
		Operation: hooks.OpTask, TaskID: id.String(),
		Service:   hooks.Service{TenantID: ref.TenantID.String(), Tenant: ref.Tenant, Name: ref.Name, Kind: config.KindMCP},
		Principal: hooks.Principal{Kind: string(rq.p.Kind), ID: rq.p.ID, BuyerID: rq.p.BuyerID.String(), Via: string(rq.p.Via)},
		Protocol:  "mcp",
		Tool:      tool,
	}
	if rq.p.TenantID != nil {
		ar.Principal.TenantID = rq.p.TenantID.String()
	}
	d, err := h.deps.Admission.Admit(rq.ctx(), ar)
	if err != nil {
		h.log.Warn("mcp: admission failed; refused (it fails closed)", "service", ref.Name, "operation", hooks.OpTask, "error", err)
		return uuid.Nil, d, refusal(http.StatusServiceUnavailable, rq.msg.ID, mcpwire.CodeInternal, "admission unavailable")
	}
	switch d.Outcome {
	case hooks.Allow, hooks.Quote:
		return id, d, nil
	case hooks.Deny:
		code, msg := mcpwire.CodeInternal, "admission denied"
		if d.Refusal != nil {
			if d.Refusal.Code != 0 {
				code = d.Refusal.Code
			}
			if d.Refusal.Message != "" {
				msg = d.Refusal.Message
			}
		}
		return uuid.Nil, d, refusal(http.StatusOK, rq.msg.ID, code, msg)
	}
	return uuid.Nil, d, refusal(http.StatusServiceUnavailable, rq.msg.ID, mcpwire.CodeInternal, "admission unavailable")
}

// taskSupported reports whether a cached tool's execution.taskSupport is optional or required (§11.5 step 0). The
// execution object is read with exact keys: one with a repeated or case-variant key, or a lone case variant of
// taskSupport, is taken for absent, which refuses the task (D172's rule, failing toward the refusal).
func taskSupported(tool json.RawMessage) bool {
	exec, ok := mcpwire.Member(tool, "execution")
	if !ok || firstByte(exec) != '{' {
		return false
	}
	ms, err := strictjson.Members(exec)
	if err != nil {
		return false
	}
	for k := range ms {
		if k != "taskSupport" && strings.EqualFold(k, "taskSupport") {
			return false
		}
	}
	v, ok := ms["taskSupport"]
	if !ok {
		return false
	}
	var s string
	if json.Unmarshal(v, &s) != nil {
		return false
	}
	return s == "optional" || s == "required"
}
