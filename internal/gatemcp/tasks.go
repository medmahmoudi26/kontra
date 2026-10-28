package gatemcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"

	"kontra.sh/kontra/internal/anon"
	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/blob"
	"kontra.sh/kontra/internal/lifecycle"
	"kontra.sh/kontra/internal/mcprelay"
	"kontra.sh/kontra/internal/mcpwire"
	"kontra.sh/kontra/internal/store"
	"kontra.sh/kontra/internal/strictjson"
)

// HireTasks is the MCP Tasks surface of hire tasks (spec §12.2, §12.4, §12.5): §11.6 for a legacy reader and
// §11.12's tasks/get, tasks/update and tasks/cancel for a modern one, over A2A gate tasks (protocol a2a) marked
// tasks.hired, scoped to the principal's buyer_id (another buyer's or an unhired task is -32602, as an unknown id).
// It reads only Postgres, on any replica; its follow-ups go through the Hirer, its cancels to TaskLifecycle.
type HireTasks struct {
	log       *store.Log
	waker     store.TaskWaker
	lc        HireLifecycle
	hirer     Hirer
	render    *blob.Renderer
	logger    *slog.Logger
	keepAlive time.Duration // a gate-served stream's comment interval (§2.2)
	reread    time.Duration // how often a blocked tasks/result reads the log without a wake
	settle    time.Duration // how long after its acceptance a detached cancel stands for the task's later ones

	// The replica's detached work (cancelDetached, hold): the cancels being sent or accepted less than settle ago, by
	// task (zero: not accepted yet), and how many cancels and modern hires are in flight, which Wait awaits.
	mu      sync.Mutex
	cancels map[uuid.UUID]time.Time
	busy    int
	idle    chan struct{} // closed while busy is 0
}

// HireLifecycle is what the MCP Tasks of hire tasks ask of TaskLifecycle (*lifecycle.Client): MCP's tasks/cancel on an
// A2A task (§11.6, D57; the cancel's MCP mode), awaited (legacy) or accepted only (the cancels nobody waits for).
type HireLifecycle interface {
	CancelMCP(ctx context.Context, taskID a2a.TaskID) (lifecycle.CancelResult, bool, error)
	RequestCancelMCP(ctx context.Context, taskID a2a.TaskID) error
}

// CancelSettle is how long a hire task's detached kontra.cancel, once accepted, stands for every later one this replica
// would send it (cancelDetached): D57's bound on the cancel writing canceled.
const CancelSettle = 10 * time.Second

// DetachedWait bounds the replica drain's wait for its detached cancels (HireTasks.Wait): one cancel's acceptance
// (lifecycle.AcceptWait) and a second.
const DetachedWait = lifecycle.AcceptWait + time.Second

// HireTasksConfig configures HireTasks.
type HireTasksConfig struct {
	Log       *store.Log
	Waker     store.TaskWaker
	Lifecycle HireLifecycle
	Hirer     Hirer          // §12.4's accepted answer is the §7 follow-up, as reply sends it
	Render    *blob.Renderer // kontra-blob: parts as presigned GETs; nil renders none
	Logger    *slog.Logger
}

// NewHireTasks checks cfg.
func NewHireTasks(cfg HireTasksConfig) (*HireTasks, error) {
	if cfg.Log == nil || cfg.Waker == nil || cfg.Lifecycle == nil || cfg.Hirer == nil {
		return nil, errors.New("gatemcp: hire tasks need the log, the waker, the lifecycle client and the hirer")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	idle := make(chan struct{})
	close(idle)
	return &HireTasks{log: cfg.Log, waker: cfg.Waker, lc: cfg.Lifecycle, hirer: cfg.Hirer, render: cfg.Render,
		logger: cfg.Logger.With("component", "gate-mcp-tasks"), keepAlive: keepAliveEvery, reread: 5 * time.Second,
		settle: CancelSettle, cancels: map[uuid.UUID]time.Time{}, idle: idle}, nil
}

// Reader is who reads a task and what it declared: the reader's era and, for §12.4, whether its session (legacy)
// or its request (modern) declared form elicitation (DM4-7).
type Reader struct {
	Principal       auth.Principal
	Era             mcpwire.Era
	FormElicitation bool
	SessionHash     *[32]byte // legacy only
}

// errTaskNotFound is the answer for a task id that is unknown, malformed, unhired or of another buyer (§12.2):
// -32602, as MCP answers an unknown task.
var errTaskNotFound = &mcpwire.Error{Code: mcpwire.CodeInvalidParams, Message: "Failed to retrieve task: Task not found"}

// errLifecycleUnavailable is a kontra.cancel that no lifecycle worker accepted in time (§14.1): -32603, HTTP 503.
var errLifecycleUnavailable = errors.New("gatemcp: the task's lifecycle is unavailable")

// load is the reader's hire task taskID names, nil when it names none of them, and asked, the seq of the task or status
// event its state comes from, read with it (store.Log.LoadHiredAsked): at input-required, the event that asked the
// question the row carries, whose gate id k.<taskId>.<asked> names it (§12.4). A principal that may not hire (spec
// §2.3) has none: the handler refuses it before (403), and this holds for every caller of HireTasks.
func (t *HireTasks) load(ctx context.Context, rd Reader, taskID uuid.UUID) (*store.TaskRow, int64, error) {
	if taskID == uuid.Nil || !rd.Principal.MayHire() {
		return nil, 0, nil
	}
	row, asked, err := t.log.LoadHiredAsked(ctx, rd.Principal.BuyerID, taskID)
	if errors.Is(err, a2a.ErrTaskNotFound) {
		return nil, 0, nil
	}
	return row, asked, err
}

// taskIDParam reads params.taskId as a gate task id (uuid.Nil when it is none).
func taskIDParam(params json.RawMessage) uuid.UUID {
	s, ok := stringOf(params, "taskId")
	if !ok {
		return uuid.Nil
	}
	id, err := uuid.Parse(s)
	if err != nil || id.String() != s {
		return uuid.Nil
	}
	return id
}

// service is the policy a task's ttl is computed with (§11.6).
func (t *HireTasks) service(ctx context.Context, row *store.TaskRow) (store.ServiceRef, error) {
	return t.log.LoadService(ctx, row.TenantID, row.Service)
}

// Get answers tasks/get: the task in the reader's era's vocabulary (§5.1, §12.4), quoted as working with the
// approve URL (D21), and sh.kontra/failure or sh.kontra/canceled in _meta for an ended one (§12.2). A modern reader
// gets the tasks extension's DetailedTask (modernGet).
func (t *HireTasks) Get(ctx context.Context, rd Reader, taskID uuid.UUID) (json.RawMessage, *mcpwire.Error, error) {
	row, asked, err := t.load(ctx, rd, taskID)
	if err != nil || row == nil {
		return nil, errTaskNotFound, err
	}
	svc, err := t.service(ctx, row)
	if err != nil {
		return nil, nil, err
	}
	if rd.Era == mcpwire.EraModern {
		res, err := t.modernGet(ctx, row, asked, svc, rd)
		return res, nil, err
	}
	res, err := taskResult(row, svc, rd)
	return res, nil, err
}

// modernGet is a modern tasks/get of a hire task (§12.5, §11.12's table, DM4-7): resultType "complete" and the Task
// as modernView presents it; a question the reader's request may get as a form (a text-only question, form
// elicitation declared) is input_required with inputRequests holding that one elicitation/create under the gate id
// k.<taskId>.<asked> of the event that asked it (asked is read with row: the key never pairs one question's text with
// a later question's event); an ended task, but a canceled one, carries its result, the CallToolResult tasks/result
// renders (§12.2), with resultType "complete".
func (t *HireTasks) modernGet(ctx context.Context, row *store.TaskRow, asked int64, svc store.ServiceRef, rd Reader) (json.RawMessage, error) {
	v := modernView(row, rd)
	o := modernTaskObject(row, svc, v, resultComplete)
	switch {
	case v.status == "input_required":
		o["inputRequests"] = map[string]any{store.GateRequestID(row.ID, asked): formRequest(v.statusMessage)}
	case row.State.Terminal() && row.State != store.StateCanceled:
		task := row.Snapshot
		if t.render != nil {
			r, err := t.render.Task(ctx, task)
			if err != nil {
				return nil, err
			}
			task = r
		}
		res, err := Render(task, row.State)
		if err != nil {
			return nil, err
		}
		o["result"] = withResultType(res, resultComplete)
	}
	return json.Marshal(o)
}

// Result answers a legacy tasks/result: blocks (on the waker) until the task ends, delivering §12.4's form
// elicitation on its own SSE response stream first (a text-only question, to a reader that declared form
// elicitation: elicitation/create {mode: form, message, requestedSchema {reply: string}} with the related-task _meta,
// its JSON-RPC and event id the gate id k.<taskId>.<seq> of the event that asked), then the rendered outcome (Render)
// with the related-task _meta. A task that has ended when asked is answered in JSON. sid is the session's
// Mcp-Session-Id.
func (t *HireTasks) Result(ctx context.Context, w http.ResponseWriter, sid string, rd Reader, m *mcpwire.Message) {
	row, _, err := t.load(ctx, rd, taskIDParam(m.Params))
	if err != nil {
		t.logger.Warn("tasks/result: the task could not be read", "error", err)
		writeError(w, http.StatusServiceUnavailable, m.ID, mcpwire.CodeInternal, "the gate could not serve the request", nil)
		return
	}
	if row == nil {
		writeRPCError(w, sid, m.ID, errTaskNotFound)
		return
	}
	id := row.ID
	wake, stop := t.waker.WaitTask(ctx, id)
	defer stop()
	s := mcprelay.NewGateStream(w, sid)
	delivered := map[int64]bool{}
	tick := time.NewTicker(t.keepAlive)
	defer tick.Stop()
	reread := time.NewTicker(t.reread)
	defer reread.Stop()
	for {
		var asked int64
		row, asked, err = t.log.LoadHiredAsked(ctx, rd.Principal.BuyerID, id)
		if err != nil {
			if !s.Started() && ctx.Err() == nil {
				writeError(w, http.StatusServiceUnavailable, m.ID, mcpwire.CodeInternal, "the gate could not serve the request", nil)
			}
			return
		}
		if row.State.Terminal() {
			msg, seq, err := t.outcome(ctx, row, m.ID)
			if err != nil {
				t.logger.Warn("tasks/result: the outcome could not be rendered", "task_id", id, "error", err)
				if !s.Started() {
					writeError(w, http.StatusServiceUnavailable, m.ID, mcpwire.CodeInternal, "the gate could not serve the request", nil)
				}
				return
			}
			if !s.Started() {
				w.Header().Set("Content-Type", "application/json")
				if sid != "" {
					w.Header().Set(mcpwire.HeaderSessionID, sid)
				}
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(msg)
				return
			}
			_ = s.Event(store.GateRequestID(id, seq), msg)
			return
		}
		if err := s.Start(); err != nil {
			return
		}
		if row.State == store.StateInputRequired {
			// asked is read with row: the request's id names the event whose text it carries.
			if ir, text, _ := Question(row.Snapshot, rd); ir && !delivered[asked] {
				gateID := store.GateRequestID(id, asked)
				if err := s.Event(gateID, elicitationRequest(gateID, id, text)); err != nil {
					return
				}
				delivered[asked] = true
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-wake:
		case <-reread.C:
		case <-tick.C:
			if s.KeepAlive() != nil {
				return
			}
		}
	}
}

// elicitationRequest is §12.4's form elicitation of a text-only question, as a JSON-RPC request whose id is the gate
// id of the event that asked.
func elicitationRequest(gateID string, task uuid.UUID, text string) []byte {
	idb, _ := json.Marshal(gateID)
	params := map[string]any{
		"mode": "form", "message": text, "requestedSchema": replySchema(),
		"_meta": map[string]any{mcpwire.MetaRelatedTask: relatedTask(task)},
	}
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(idb), "method": mcpwire.MethodElicit, "params": params})
	return b
}

// outcome is an ended task's tasks/result answer to the request id (§12.2): the rendered CallToolResult (never a
// JSON-RPC error) with the related-task key, and the seq of the event that ended it.
func (t *HireTasks) outcome(ctx context.Context, row *store.TaskRow, reqID json.RawMessage) ([]byte, int64, error) {
	task := row.Snapshot
	if t.render != nil {
		r, err := t.render.Task(ctx, task)
		if err != nil {
			return nil, 0, err
		}
		task = r
	}
	res := render(task, row.State)
	if res.Meta == nil {
		res.Meta = map[string]any{}
	}
	res.Meta[mcpwire.MetaRelatedTask] = relatedTask(row.ID)
	b, err := json.Marshal(res)
	if err != nil {
		return nil, 0, err
	}
	seq, err := t.log.StatusSeq(ctx, row.ID)
	if err != nil {
		return nil, 0, err
	}
	return responseBytes(reqID, "result", b), seq, nil
}

// List answers a legacy tasks/list: the buyer's hire tasks across services, newest first, gate-minted cursors
// (§12.2).
func (t *HireTasks) List(ctx context.Context, rd Reader, params json.RawMessage) (json.RawMessage, *mcpwire.Error, error) {
	cursor := ""
	if v, ok := mcpwire.Member(params, "cursor"); ok && string(bytes.TrimSpace(v)) != "null" {
		s, ok := stringOf(params, "cursor")
		if !ok {
			return nil, &mcpwire.Error{Code: mcpwire.CodeInvalidParams, Message: "Invalid cursor"}, nil
		}
		cursor = s
	}
	if !rd.Principal.MayHire() {
		return json.RawMessage(`{"tasks":[]}`), nil, nil // as load: a principal that may not hire has no hire tasks
	}
	rows, next, err := t.log.ListHired(ctx, rd.Principal.BuyerID, cursor)
	if errors.Is(err, store.ErrBadCursor) {
		return nil, &mcpwire.Error{Code: mcpwire.CodeInvalidParams, Message: "Invalid cursor"}, nil
	}
	if err != nil {
		return nil, nil, err
	}
	svcs := map[string]store.ServiceRef{}
	tasks := make([]any, 0, len(rows))
	for _, r := range rows {
		k := r.TenantID.String() + "/" + r.Service
		svc, ok := svcs[k]
		if !ok {
			if svc, err = t.service(ctx, r); err != nil {
				return nil, nil, err
			}
			svcs[k] = svc
		}
		obj, err := taskResult(r, svc, rd)
		if err != nil {
			return nil, nil, err
		}
		tasks = append(tasks, json.RawMessage(obj))
	}
	out := map[string]any{"tasks": tasks}
	if next != "" {
		out["nextCursor"] = next
	}
	b, err := json.Marshal(out)
	return b, nil, err
}

// Cancel answers tasks/cancel (§12.2, §11.6, D57): -32602 for an ended task; otherwise kontra.cancel in MCP mode,
// which writes canceled within 10 s whatever the upstream answers (the upstream receiving §8's CancelTask), and the
// task as it is then, status cancelled. (M4 WP-04, the legacy tasks/cancel.)
//
// A modern reader's (§12.5, §11.12's table; M4 WP-05) is acknowledged with an empty result, resultType "complete",
// without waiting: kontra.cancel in MCP mode is sent, detached, on a task that is not terminal (D57 still writes
// canceled within 10 s), and nothing on a terminal one (the tasks extension: cancellation is eventually consistent).
func (t *HireTasks) Cancel(ctx context.Context, rd Reader, taskID uuid.UUID) (json.RawMessage, *mcpwire.Error, error) {
	row, _, err := t.load(ctx, rd, taskID)
	if err != nil || row == nil {
		return nil, errTaskNotFound, err
	}
	if rd.Era == mcpwire.EraModern {
		if !row.State.Terminal() {
			t.cancelDetached(row.ID, "tasks/cancel")
		}
		return ackComplete, nil, nil
	}
	if row.State.Terminal() {
		return nil, notCancelable(row.State), nil
	}
	_, _, err = t.lc.CancelMCP(ctx, a2a.TaskID(row.ID.String()))
	switch {
	case errors.Is(err, a2a.ErrTaskNotCancelable):
	case errors.Is(err, lifecycle.ErrUnavailable):
		return nil, nil, fmt.Errorf("%w: %w", errLifecycleUnavailable, err)
	case err != nil:
		return nil, nil, err
	}
	if row, err = t.log.Load(ctx, row.ID); err != nil {
		return nil, nil, err
	}
	if row.State.Terminal() && row.State != store.StateCanceled {
		return nil, notCancelable(row.State), nil
	}
	svc, err := t.service(ctx, row)
	if err != nil {
		return nil, nil, err
	}
	res, err := taskResult(row, svc, rd)
	return res, nil, err
}

func notCancelable(s store.State) *mcpwire.Error {
	return &mcpwire.Error{Code: mcpwire.CodeInvalidParams, Message: "Cannot cancel task in terminal status: " + s.MCPStatus()}
}

// ackComplete is the empty result of a modern tasks/update and tasks/cancel (the tasks extension).
var ackComplete = json.RawMessage(`{"resultType":"complete"}`)

// Update answers a modern tasks/update (§12.4, §12.5, §11.12's table, DM4-7; M4 WP-05): params.inputResponses must be
// an object (else -32602), and the task a hire task of the reader's buyer (else -32602, as an unknown one). The one
// key that can be answered is the gate id k.<taskId>.<seq> of the question outstanding now, as the reader's own request
// would see it in tasks/get's inputRequests (follow): its accept {action: "accept", content: {reply: string}} becomes
// the §7 follow-up, once (the workflow takes one follow-up per turn end, §7, D105). Every other key, decline and
// cancel, and every answer while the task is quoted, working or ended, is ignored and forwards nothing, and the result
// is the empty acknowledgement, resultType "complete" (the extension: a server SHOULD ignore inputResponses for a key
// that is not outstanding): nothing a client answers approves a quote (D38, D45). So is the outstanding key's answer
// when the workflow refuses it because the question stopped being outstanding meanwhile (another follow-up of that turn
// end was accepted first, the task moved on or ended: a2a.ErrUnsupportedOperation, §7). Any other refusal is not
// acknowledged, since the extension tells the client never to answer a key twice: the answer would be lost while the
// task waits on. A follow-up the lifecycle did not accept in time is 503 with -32603, an anonymous tenant's limit 429,
// another A2A refusal (the anonymous storage limit, a required extension, DM4-27) its JSON-RPC error, and a failure of
// the gate's own 503 with -32603 (*followUpRefused, answered by writeTaskAnswer as hire and reply are), so the client
// answers again.
func (t *HireTasks) Update(ctx context.Context, rd Reader, params json.RawMessage) (json.RawMessage, *mcpwire.Error, error) {
	resp, ok := mcpwire.Member(params, "inputResponses")
	if !ok || firstByte(resp) != '{' {
		return nil, &mcpwire.Error{Code: mcpwire.CodeInvalidParams, Message: "Invalid params: inputResponses is not an object"}, nil
	}
	answers, err := strictjson.Members(resp)
	if err != nil {
		return nil, &mcpwire.Error{Code: mcpwire.CodeInvalidParams, Message: "Invalid params: inputResponses"}, nil
	}
	row, asked, err := t.load(ctx, rd, taskIDParam(params))
	if err != nil || row == nil {
		return nil, errTaskNotFound, err
	}
	if row.State != store.StateInputRequired {
		if len(answers) > 0 {
			t.logger.Info("tasks/update ignored: the task asks no question now", "task_id", row.ID, "state", row.State)
		}
		return ackComplete, nil, nil
	}
	value, ok := answers[store.GateRequestID(row.ID, asked)]
	if !ok {
		if len(answers) > 0 {
			t.logger.Info("tasks/update ignored: no key names the question outstanding now", "task_id", row.ID, "request_seq", asked)
		}
		return ackComplete, nil, nil
	}
	var refusal *followUpRefused
	switch _, err := t.follow(ctx, rd, row, asked, asked, value); {
	case err == nil:
	case errors.As(err, &refusal) && refusal.settled():
		// The question is no longer outstanding: as an answer to a key that is not.
	default:
		return nil, nil, err
	}
	return ackComplete, nil, nil
}

// followUpRefused is the A2A route's refusal of a §12.4 follow-up (the Hirer's error, as Reply returned it).
type followUpRefused struct{ err error }

func (e *followUpRefused) Error() string { return "gatemcp: follow-up refused: " + e.err.Error() }

func (e *followUpRefused) Unwrap() error { return e.err }

// settled reports whether the refusal says the question stopped being outstanding: §7's refusal of a follow-up
// (a2a.ErrUnsupportedOperation: another follow-up of that turn end was accepted first, the task is no longer
// input-required, its workflow ended), not a lifecycle that did not answer in time or a limit.
func (e *followUpRefused) settled() bool {
	return errors.Is(e.err, a2a.ErrUnsupportedOperation) && !errors.Is(e.err, lifecycle.ErrUnavailable) &&
		!errors.Is(e.err, anon.ErrRateLimited)
}

// Respond takes a legacy client's response to a gate-written elicitation/create (id k.<taskId>.<seq>, §11.7,
// §12.4): an accept whose content is {reply: string} becomes the §7 follow-up, once, and only when it answers the
// question outstanding now (the task, a hire task of the reader's buyer, is at input-required and seq is the event
// that asked, a text-only question this reader's session would have been sent as a form); decline and cancel leave
// the task waiting. A response about a quoted task, an earlier turn, an unknown or foreign task, or one in any other
// shape is dropped: nothing a client answers approves a quote (D38, D45). It reports whether the response was the
// follow-up.
func (t *HireTasks) Respond(ctx context.Context, rd Reader, m *mcpwire.Message) bool {
	var s string
	if json.Unmarshal(m.ID, &s) != nil || m.Error != nil {
		return false
	}
	taskID, seq, ok := store.ParseGateRequestID(s)
	if !ok {
		return false
	}
	row, asked, err := t.load(ctx, rd, taskID)
	if err != nil || row == nil {
		return false
	}
	sent, _ := t.follow(ctx, rd, row, asked, seq, m.Result)
	return sent
}

// follow is §12.4's answer to the question of a hire task of rd's buyer, in either era: result, an ElicitResult,
// becomes the §7 follow-up (through the Hirer, as reply sends it) only when the task is at input-required, seq, the
// event the answer names, is asked, the event row's question comes from (load reads them together), and is still the
// task's last (the question outstanding now), the question is one rd gets as a form (text only, form elicitation
// declared: Question), and result is an accept whose content is {reply: string}. It reports whether the follow-up was
// sent; err is a *followUpRefused carrying the Hirer's refusal of it (another follow-up of that turn taken first, a
// required extension, a limit, a lifecycle that did not accept it in time), logged here, or the gate's failure to
// read the log.
func (t *HireTasks) follow(ctx context.Context, rd Reader, row *store.TaskRow, asked, seq int64, result json.RawMessage) (bool, error) {
	if row.State != store.StateInputRequired || seq != asked {
		return false, nil
	}
	if cur, err := t.log.StatusSeq(ctx, row.ID); err != nil || cur != seq {
		return false, err
	}
	if ir, _, _ := Question(row.Snapshot, rd); !ir {
		return false, nil
	}
	reply, ok := acceptedReply(result)
	if !ok {
		return false, nil
	}
	if _, err := t.hirer.Reply(ctx, rd.Principal, row, a2a.ContentParts{a2a.NewTextPart(reply)}); err != nil {
		t.logger.Info("an answer to a hire task's question was not taken", "task_id", row.ID, "request_seq", seq, "error", err)
		return false, &followUpRefused{err: err}
	}
	return true, nil
}

// cancelDetached sends a hire task kontra.cancel in MCP mode without waiting for its result (§11.6, D57: the workflow
// writes canceled within 10 s, the upstream receiving §8's CancelTask): a modern tasks/cancel, and a modern hire whose
// client left before its answer (D178). It waits only for the workflow's acceptance (RequestCancelMCP), on a goroutine
// the replica's drain awaits (Wait). One per task at a time on this replica: while a cancel of the task is being sent,
// and for CancelSettle after it was accepted, another is not sent (the workflow would join it, §8; each would be an
// Update in the task's history, counted against Temporal's per-workflow limits, which then refuse the task's own
// kontra.message and kontra.approval). A cancel not accepted in time is forgotten, so a later one is sent again.
func (t *HireTasks) cancelDetached(id uuid.UUID, why string) {
	t.mu.Lock()
	now := time.Now()
	for k, at := range t.cancels {
		if !at.IsZero() && now.Sub(at) >= t.settle {
			delete(t.cancels, k)
		}
	}
	if _, ok := t.cancels[id]; ok {
		t.mu.Unlock()
		t.logger.Debug("a hire task's cancel is already sent", "task_id", id, "why", why)
		return
	}
	t.cancels[id] = time.Time{}
	release := t.holdLocked()
	t.mu.Unlock()
	t.logger.Info("a hire task is cancelled", "task_id", id, "why", why)
	go func() {
		defer release()
		ctx, cancel := context.WithTimeout(context.Background(), DetachedWait)
		defer cancel()
		err := t.lc.RequestCancelMCP(ctx, a2a.TaskID(id.String()))
		t.mu.Lock()
		if err == nil {
			t.cancels[id] = time.Now()
		} else {
			delete(t.cancels, id)
		}
		t.mu.Unlock()
		if err != nil && !errors.Is(err, a2a.ErrTaskNotCancelable) {
			t.logger.Warn("kontra.cancel was not accepted", "task_id", id, "error", err)
		}
	}()
}

// hold counts one piece of the replica's detached work in flight until release is called: a detached cancel, or a
// modern hire, which may still send one when its client leaves (answerModernHire).
func (t *HireTasks) hold() (release func()) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.holdLocked()
}

// holdLocked is hold with t.mu held.
func (t *HireTasks) holdLocked() (release func()) {
	if t.busy == 0 {
		t.idle = make(chan struct{})
	}
	t.busy++
	var once sync.Once
	return func() {
		once.Do(func() {
			t.mu.Lock()
			if t.busy--; t.busy == 0 {
				close(t.idle)
			}
			t.mu.Unlock()
		})
	}
}

// Wait returns once no detached cancel and no modern hire is in flight on this replica, or with ctx's error when ctx
// ends first. The replica's drain calls it after its listener has stopped (a modern hire whose stream that closed
// cancels its task) and before it closes its Temporal client, bounded by DetachedWait: a cancel whose Update had not
// reached Temporal when the client closed would be lost, and the task would run unseen (D178); one that has been
// accepted is in the task's history.
func (t *HireTasks) Wait(ctx context.Context) error {
	t.mu.Lock()
	idle := t.idle
	t.mu.Unlock()
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// acceptedReply reads an ElicitResult {action: "accept", content: {reply: string}} with exact keys: the reply, ok
// false for decline, cancel, or anything else.
func acceptedReply(result json.RawMessage) (string, bool) {
	m, err := strictjson.Members(result)
	if err != nil {
		return "", false
	}
	var action string
	if json.Unmarshal(m["action"], &action) != nil || action != "accept" {
		return "", false
	}
	c, err := strictjson.Members(m["content"])
	if err != nil {
		return "", false
	}
	var reply string
	if raw, ok := c["reply"]; !ok || firstByte(raw) != '"' || json.Unmarshal(raw, &reply) != nil || reply == "" {
		return "", false
	}
	return reply, true
}

// writeRPCError writes a JSON-RPC error answer with status 200 and the gate's session id.
func writeRPCError(w http.ResponseWriter, sid string, id json.RawMessage, e *mcpwire.Error) {
	if sid != "" {
		w.Header().Set(mcpwire.HeaderSessionID, sid)
	}
	writeError(w, http.StatusOK, id, e.Code, e.Message, e.Data)
}

// tasks serves the legacy tasks/* of a 2025-11-25 session over hire tasks (§12.2, §11.6).
func (h *Handler) tasks(rq *request) {
	m, t, rd := rq.msg, h.deps.Tasks, rq.reader()
	var (
		res    json.RawMessage
		rpcErr *mcpwire.Error
		err    error
	)
	switch m.Method {
	case "tasks/result":
		t.Result(rq.ctx(), rq.w, rq.sid, rd, m)
		return
	case "tasks/get":
		res, rpcErr, err = t.Get(rq.ctx(), rd, taskIDParam(m.Params))
	case "tasks/list":
		res, rpcErr, err = t.List(rq.ctx(), rd, m.Params)
	case "tasks/cancel":
		res, rpcErr, err = t.Cancel(rq.ctx(), rd, taskIDParam(m.Params))
	}
	h.writeTaskAnswer(rq, res, rpcErr, err)
}

// writeTaskAnswer writes the answer to a tasks/* request of either era: a lifecycle that did not accept the request
// in time is 503 with -32603; a refused follow-up (tasks/update) as hire and reply answer the Hirer's refusals
// (refused); the gate's own failure 503 (internal); a JSON-RPC error with status 200; else the result.
func (h *Handler) writeTaskAnswer(rq *request, res json.RawMessage, rpcErr *mcpwire.Error, err error) {
	m := rq.msg
	var refusal *followUpRefused
	switch {
	case errors.Is(err, errLifecycleUnavailable):
		h.log.Warn("mcp: a task's lifecycle did not accept the request in time", "method", m.Method, "error", err)
		writeError(rq.w, http.StatusServiceUnavailable, m.ID, mcpwire.CodeInternal, "the task's lifecycle is unavailable", nil)
	case errors.As(err, &refusal):
		h.refused(rq, "send the follow-up", refusal.err)
	case err != nil:
		h.internal(rq, "serve "+m.Method, err)
	case rpcErr != nil:
		writeRPCError(rq.w, rq.sid, m.ID, rpcErr)
	default:
		rq.result(res)
	}
}
