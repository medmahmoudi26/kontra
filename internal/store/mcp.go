package store

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Gate-owned MCP tasks in the store (spec/relay.md §5, §11.5–§11.9; M3 WP-06).
//
// An MCP task's projection (tasks.snapshot) is kept in the shape of an A2A Task, so that every writer and reader
// of the store, the lifecycle's included, reads one shape: its id is the gate task id and so is its context id (an
// MCP task has no context), its status is tasks.state with the statusMessage as an agent message of one text part,
// and its metadata holds the gate's keys (kontra.failure, kontra.canceled), the operator's metadata, and under
// MetaMCPMeta the operator's mcpMeta (§13.3: rendered into the _meta of CreateTaskResult, tasks/get and
// tasks/result). The MCP surface renders it; no A2A route reaches an MCP service.
//
// Its task_events rows carry MCP payloads (§11.9), written by the gate and read by Go only:
//
//   - task (seq 1): {"task": {"taskId", "status", "statusMessage"?, "lastUpdatedAt", "metadata"?}};
//   - status: {"status", "statusMessage"?, "lastUpdatedAt", "metadata"?}, in tasks.state's vocabulary (§5.1);
//   - input_request: {"inputRequest": <the upstream's request, its JSON-RPC id the gate id k.<taskId>.<seq> and
//     its related-task key the gate task id>, "upstream": {"id": <the upstream's own JSON-RPC id>}}: the era-neutral
//     form (D159), from which a legacy reader shows a JSON-RPC request and a modern one an inputRequests entry,
//     the upstream's id kept in the event and never shown (D175);
//   - input_response: {"inputResponse": <the client's JSON-RPC response as it arrived, after §2.5>, "requestSeq"};
//   - result (the terminal event of an upstream's answer, §11.8 case 1): {"status", "lastUpdatedAt", "metadata"?,
//     "result" | "error"}, the upstream's result or JSON-RPC error with ids rewritten;
//   - metadata: {"metadata", "mcpMeta"?}.
//
// Raw members (an upstream's params, a client's response, a result) are copied into the payload byte for byte, so
// a sealed envelope inside them is never re-encoded (D174).

// MetaMCPMeta is the projection's metadata key under which an MCP task keeps its operator mcpMeta (§13.3).
const MetaMCPMeta = "kontra.mcpMeta"

// MCPPollInterval is the pollInterval the gate gives its MCP tasks (spec §11.5 step 2), in milliseconds.
const MCPPollInterval = 1000

// MCPStatus is a state as an MCP 2025-11-25 Task.status shows it (spec §5.1): quoted, submitted, working and
// auth-required are working, input-required input_required, rejected failed, canceled cancelled.
func (s State) MCPStatus() string {
	switch s {
	case StateInputRequired:
		return "input_required"
	case StateCompleted:
		return "completed"
	case StateFailed, StateRejected:
		return "failed"
	case StateCanceled:
		return "cancelled"
	}
	return "working"
}

// StateFromMCP is the tasks.state an upstream's MCP 2025-11-25 Task.status maps to; ok false for a value MCP does
// not define.
func StateFromMCP(status string) (State, bool) {
	switch status {
	case "working":
		return StateWorking, true
	case "input_required":
		return StateInputRequired, true
	case "completed":
		return StateCompleted, true
	case "failed":
		return StateFailed, true
	case "cancelled":
		return StateCanceled, true
	}
	return "", false
}

// GateRequestID is the gate request id of an MCP task's input request at seq (spec §11.7): k.<taskId>.<seq>.
func GateRequestID(taskID uuid.UUID, seq int64) string {
	return "k." + taskID.String() + "." + strconv.FormatInt(seq, 10)
}

// ParseGateRequestID reads a gate request id: exactly k.<taskId>.<seq>, the task id as the gate writes it (a
// lowercase hyphenated UUID) and a decimal seq without sign or leading zero.
func ParseGateRequestID(s string) (uuid.UUID, int64, bool) {
	rest, ok := strings.CutPrefix(s, "k.")
	if !ok {
		return uuid.Nil, 0, false
	}
	task, seq, ok := strings.Cut(rest, ".")
	if !ok {
		return uuid.Nil, 0, false
	}
	id, err := uuid.Parse(task)
	if err != nil || id.String() != task {
		return uuid.Nil, 0, false
	}
	n, err := strconv.ParseInt(seq, 10, 64)
	if err != nil || n < 1 || strconv.FormatInt(n, 10) != seq {
		return uuid.Nil, 0, false
	}
	return id, n, true
}

// mcpRow is an MCP payload as the gate wrote it (decoded in Go only; D49).
type mcpRow struct {
	Task          *mcpRow         `json:"task,omitempty"`
	TaskID        string          `json:"taskId,omitempty"`
	Status        State           `json:"status,omitempty"`
	StatusMessage string          `json:"statusMessage,omitempty"`
	LastUpdatedAt *time.Time      `json:"lastUpdatedAt,omitempty"`
	Metadata      map[string]any  `json:"metadata,omitempty"`
	MCPMeta       map[string]any  `json:"mcpMeta,omitempty"`
	Result        json.RawMessage `json:"result,omitempty"`
	Error         json.RawMessage `json:"error,omitempty"`
	InputRequest  json.RawMessage `json:"inputRequest,omitempty"`
	Upstream      json.RawMessage `json:"upstream,omitempty"`
	InputResponse json.RawMessage `json:"inputResponse,omitempty"`
	RequestSeq    int64           `json:"requestSeq,omitempty"`
}

// rawObject writes a JSON object member by member, raw values copied as they are.
type rawObject struct {
	b bytes.Buffer
	n int
}

func (o *rawObject) raw(key string, v []byte) {
	if o.n == 0 {
		o.b.WriteByte('{')
	} else {
		o.b.WriteByte(',')
	}
	k, _ := json.Marshal(key)
	o.b.Write(k)
	o.b.WriteByte(':')
	o.b.Write(v)
	o.n++
}

func (o *rawObject) value(key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	o.raw(key, b)
	return nil
}

func (o *rawObject) bytes() []byte {
	if o.n == 0 {
		return []byte("{}")
	}
	return append(bytes.Clone(o.b.Bytes()), '}')
}

// statusMembers writes a status's members: status, statusMessage, lastUpdatedAt, metadata.
func (o *rawObject) statusMembers(state State, text string, at time.Time, meta map[string]any) error {
	if err := o.value("status", state); err != nil {
		return err
	}
	if text != "" {
		if err := o.value("statusMessage", text); err != nil {
			return err
		}
	}
	if err := o.value("lastUpdatedAt", at); err != nil {
		return err
	}
	if len(meta) > 0 {
		return o.value("metadata", meta)
	}
	return nil
}

// MCPStatusText is the statusMessage of an MCP task's projection (the text of its status message), "" when none.
func MCPStatusText(t *a2a.Task) string {
	if t == nil || t.Status.Message == nil {
		return ""
	}
	var b strings.Builder
	for _, p := range t.Status.Message.Parts {
		if p != nil {
			b.WriteString(p.Text())
		}
	}
	return b.String()
}

// MCPMeta is the operator mcpMeta an MCP task's projection keeps (MetaMCPMeta), nil when none.
func MCPMeta(t *a2a.Task) map[string]any {
	if t == nil {
		return nil
	}
	m, _ := t.Metadata[MetaMCPMeta].(map[string]any)
	return m
}

// mcpApply is the projection after a task, status or result row at seq: the state, the statusMessage as an agent
// message whose id is derived from seq (so the fold of the rows equals the projection), the time, and meta merged
// at the top level of the metadata (an operator key replaces a same-named key, §2.5). prev is not modified.
func mcpApply(prev *a2a.Task, taskID string, seq int64, state State, text string, at time.Time, meta map[string]any) *a2a.Task {
	next := &a2a.Task{ID: a2a.TaskID(taskID), ContextID: taskID}
	if prev != nil {
		*next = *prev
		next.Metadata = maps.Clone(prev.Metadata)
	}
	ts := at.UTC()
	next.Status = a2a.TaskStatus{State: state.A2A(), Timestamp: &ts}
	if text != "" {
		next.Status.Message = &a2a.Message{
			ID: "kontra-status-" + strconv.FormatInt(seq, 10), Role: a2a.MessageRoleAgent,
			TaskID: next.ID, ContextID: next.ContextID, Parts: a2a.ContentParts{a2a.NewTextPart(text)},
		}
	}
	if len(meta) > 0 {
		if next.Metadata == nil {
			next.Metadata = make(map[string]any, len(meta))
		}
		maps.Copy(next.Metadata, meta)
	}
	return next
}

// mergeMCPMeta returns task with mcpMeta merged into its MetaMCPMeta map (an operator key replaces a same-named
// key). The input is not modified.
func mergeMCPMeta(task *a2a.Task, mcpMeta map[string]any) *a2a.Task {
	if len(mcpMeta) == 0 {
		return task
	}
	next := *task
	next.Metadata = maps.Clone(task.Metadata)
	if next.Metadata == nil {
		next.Metadata = map[string]any{}
	}
	m := maps.Clone(MCPMeta(task))
	if m == nil {
		m = make(map[string]any, len(mcpMeta))
	}
	maps.Copy(m, mcpMeta)
	next.Metadata[MetaMCPMeta] = m
	return &next
}

// MCPNow is the time the gate stamps an MCP status with: UTC, to the millisecond (as the A2A writers do).
func MCPNow() time.Time { return time.Now().UTC().Truncate(time.Millisecond) }

// NewMCPTask is a gate-owned MCP task as the relay creates it (spec §11.5 step 2, §13.2): id and context id the
// task id, submitted (or quoted, with the approve URL as its statusMessage and in its mcpMeta under
// sh.kontra/approve-url, D21), the Admission decision's metadata and mcpMeta, and the task row's MCP payload.
func NewMCPTask(id uuid.UUID, svc ServiceRef, owner Owner, state State, quoteExpires *time.Time, approveURL string,
	meta, mcpMeta map[string]any) (NewTask, error) {
	md := maps.Clone(meta)
	if len(mcpMeta) > 0 || approveURL != "" {
		mm := maps.Clone(mcpMeta)
		if mm == nil {
			mm = map[string]any{}
		}
		if approveURL != "" {
			mm["sh.kontra/approve-url"] = approveURL
		}
		if md == nil {
			md = map[string]any{}
		}
		md[MetaMCPMeta] = mm
	}
	at := MCPNow()
	snap := mcpApply(nil, id.String(), 1, state, approveURL, at, md)
	var task rawObject
	if err := task.value("taskId", id.String()); err != nil {
		return NewTask{}, err
	}
	if err := task.statusMembers(state, approveURL, at, md); err != nil {
		return NewTask{}, err
	}
	var row rawObject
	row.raw("task", task.bytes())
	return NewTask{
		ID: id, Service: svc, Protocol: ProtocolMCP, Owner: owner, ContextID: id.String(), State: state,
		Snapshot: snap, QuoteExpiresAt: quoteExpires, Payload: row.bytes(),
	}, nil
}

// MCPStatusWrite is a status row of an MCP task at its current version: the state, its statusMessage and the
// metadata the event merges (kontra.failure on a gate-written failure). A terminal state carries the task's calls
// row (Log.MCPCall), which the caller sets.
func MCPStatusWrite(row *TaskRow, state State, text string, meta map[string]any) (Write, error) {
	at := MCPNow()
	var o rawObject
	if err := o.statusMembers(state, text, at, meta); err != nil {
		return Write{}, err
	}
	return Write{
		TaskID: row.ID, PrevVersion: row.Version, Kind: KindStatus, State: state, StatusAt: at,
		Snapshot: mcpApply(row.Snapshot, row.ID.String(), row.Version+1, state, text, at, meta), Payload: o.bytes(),
	}, nil
}

// MCPResultWrite is the terminal result row of an MCP task (spec §11.8 case 1): the upstream's result (resultMember
// "result") or JSON-RPC error ("error"), raw, ids already rewritten, with the state it ends the task in and the
// metadata that merges (kontra.failure: upstream_error for a JSON-RPC error). The caller sets its calls row.
func MCPResultWrite(row *TaskRow, state State, resultMember string, raw json.RawMessage, meta map[string]any) (Write, error) {
	if resultMember != "result" && resultMember != "error" {
		return Write{}, fmt.Errorf("store: an MCP result row holds a result or an error, not %q", resultMember)
	}
	if !state.Terminal() {
		return Write{}, fmt.Errorf("store: an MCP result row ends the task, not %s", state)
	}
	at := MCPNow()
	var o rawObject
	if err := o.statusMembers(state, "", at, meta); err != nil {
		return Write{}, err
	}
	o.raw(resultMember, raw)
	return Write{
		TaskID: row.ID, PrevVersion: row.Version, Kind: KindResult, State: state, StatusAt: at,
		Snapshot: mcpApply(row.Snapshot, row.ID.String(), row.Version+1, state, "", at, meta), Payload: o.bytes(),
	}, nil
}

// mcpTerminal builds the terminal Write of a gate-written end of an MCP task (write_terminal, the orphan sweep):
// failed with kontra.failure, canceled with kontra.canceled, its text the reason's when none is given, and its
// calls row with is_error true unless it completed (spec §11.8 cases 2 and 3).
func (l *Log) mcpTerminal(ctx context.Context, q querier, row *TaskRow, w TerminalWrite) (Write, error) {
	var meta map[string]any
	text := w.Text
	switch w.State {
	case StateFailed:
		if !w.Reason.Valid() {
			return Write{}, fmt.Errorf("store: failure reason %q is not one of spec §6.4's", w.Reason)
		}
		meta = map[string]any{MetaFailure: string(w.Reason)}
		if text == "" {
			text = ReasonText(w.Reason)
		}
	case StateCanceled:
		if _, ok := canceledText[w.Canceled]; w.Canceled != "" && !ok {
			return Write{}, fmt.Errorf("store: kontra.canceled %q", w.Canceled)
		}
		if w.Canceled != "" {
			meta = map[string]any{MetaCanceled: w.Canceled}
		}
		if text == "" {
			if text = canceledText[w.Canceled]; text == "" {
				text = canceledText[CanceledClient]
			}
		}
	case StateCompleted, StateRejected:
		if text == "" {
			return Write{}, fmt.Errorf("store: a %s terminal write needs its text", w.State)
		}
	default:
		return Write{}, fmt.Errorf("store: %q is not a terminal state", w.State)
	}
	wr, err := MCPStatusWrite(row, w.State, text, meta)
	if err != nil {
		return Write{}, err
	}
	if wr.Call, err = l.mcpCall(ctx, q, row.ID, w.State != StateCompleted, int64(len(wr.Payload))); err != nil {
		return Write{}, err
	}
	return wr, nil
}

// MCPCall is the calls row of an MCP task's terminal event (Write.Call): the tool, the creating session's hash and
// the request's bytes from the task's first work item, bytesOut and isError as given. A task without one (none is
// created so) gets a row naming no session.
func (l *Log) MCPCall(ctx context.Context, taskID uuid.UUID, isError bool, bytesOut int64) (*Call, error) {
	return l.mcpCall(ctx, l.db, taskID, isError, bytesOut)
}

func (l *Log) mcpCall(ctx context.Context, q querier, taskID uuid.UUID, isError bool, bytesOut int64) (*Call, error) {
	var raw []byte
	err := q.QueryRow(ctx, `select payload from kontra.work_items where task_id = $1 and kind = 'execute'
		order by created_at, id limit 1`, taskID).Scan(&raw)
	c := &Call{Tool: "unknown", BytesOut: bytesOut, IsError: isError}
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return c, nil
	case err != nil:
		return nil, fmt.Errorf("store: the work item of MCP task %s: %w", taskID, err)
	}
	wp, err := UnmarshalPayload(raw)
	if err != nil {
		return nil, err
	}
	if m := wp.MCP; m != nil {
		c.Tool, c.BytesIn = m.Tool, int64(len(m.Request))
		if h, err := hexSession(m.Session); err == nil {
			c.MCPSession = h
		}
	}
	return c, nil
}

func hexSession(s string) ([]byte, error) {
	if len(s) != 64 {
		return nil, errors.New("no session")
	}
	b := make([]byte, 32)
	for i := range b {
		v, err := strconv.ParseUint(s[2*i:2*i+2], 16, 8)
		if err != nil {
			return nil, err
		}
		b[i] = byte(v)
	}
	return b, nil
}

// MCPWorkItem returns an MCP task's work item payload (its first execute item), ErrWorkItemNotFound when there is
// none.
func (l *Log) MCPWorkItem(ctx context.Context, taskID uuid.UUID) (*MCPWork, error) {
	id, err := l.FirstWorkItemID(ctx, taskID)
	if err != nil {
		return nil, err
	}
	wi, err := l.LoadWorkItem(ctx, id, taskID)
	if err != nil {
		return nil, err
	}
	if wi.Payload.MCP == nil {
		return nil, fmt.Errorf("store: task %s's work item is not an MCP call: %w", taskID, ErrWorkItemNotFound)
	}
	return wi.Payload.MCP, nil
}

// SetUpstreamSession records the MCP session the dispatch opened on its attempt (attempts.upstream_session, spec
// §4, §11.5 step 3), under the dispatch's fence: a fenced dispatch's write is ErrFenced.
func (l *Log) SetUpstreamSession(ctx context.Context, f *Fence, sid string) error {
	if f == nil {
		return errors.New("store: an upstream session is recorded under a dispatch's fence")
	}
	ctx, cancel := l.writeCtx(ctx)
	defer cancel()
	tag, err := l.writeExec(ctx, `update kontra.attempts a set upstream_session = $3
			where a.task_id = $1 and a.n = $2 and a.ended_at is null and a.dispatch_id = $4
			  and not exists (select 1 from kontra.attempts b where b.task_id = a.task_id and b.n > a.n)`,
		f.TaskID, f.N, nullString(sid), f.DispatchID)
	if err != nil {
		return fmt.Errorf("store: upstream session of attempt %d of task %s: %w", f.N, f.TaskID, err)
	}
	if tag.RowsAffected() == 0 {
		f.markFenced()
		return fmt.Errorf("store: upstream session of attempt %d of task %s: %w", f.N, f.TaskID, ErrFenced)
	}
	return nil
}

// InputRequest is one input_request event of an MCP task (spec §11.5 step 5): its seq, the request as clients see
// it (JSON-RPC id the gate id, related-task key the gate task id), its method, and the upstream's own id (never
// shown to a client).
type InputRequest struct {
	Seq        int64
	GateID     string
	Method     string
	Request    json.RawMessage
	UpstreamID json.RawMessage
}

// InputResponse is one input_response event: its seq, the input request's seq it answers, and the client's
// response as it arrived (after §2.5).
type InputResponse struct {
	Seq        int64
	RequestSeq int64
	Response   json.RawMessage
}

// AppendInputRequest logs an upstream's request (elicitation/create, sampling/createMessage) as an input_request
// event under the dispatch's fence (spec §11.5 step 5, D159): build gets the gate id the event will carry,
// k.<taskId>.<seq> with seq the event's own, and returns the request with that id; the upstream's id is kept in the
// event. The task's state does not change. It retries on a concurrent write (a client's answer, a patch) and is
// ErrConcurrentModification once the task is terminal.
func (l *Log) AppendInputRequest(ctx context.Context, f *Fence, taskID uuid.UUID, upstreamID json.RawMessage,
	build func(gateID string) (json.RawMessage, error)) (int64, error) {
	var up rawObject
	up.raw("id", upstreamID)
	var last error
	for range conflictRetries {
		row, err := l.Load(ctx, taskID)
		if err != nil {
			return 0, err
		}
		if row.State.Terminal() {
			return 0, fmt.Errorf("store: task %s is %s: %w", taskID, row.State, taskstore.ErrConcurrentModification)
		}
		seq := row.Version + 1
		req, err := build(GateRequestID(taskID, seq))
		if err != nil {
			return 0, err
		}
		var o rawObject
		o.raw("inputRequest", req)
		o.raw("upstream", up.bytes())
		a, err := l.Append(ctx, nil, Write{
			TaskID: taskID, PrevVersion: row.Version, Kind: KindInputRequest, Snapshot: row.Snapshot,
			Payload: o.bytes(), Fence: f,
		})
		if err == nil {
			return a.Seq, nil
		}
		if !errors.Is(err, taskstore.ErrConcurrentModification) {
			return 0, err
		}
		last = err
	}
	return 0, last
}

// AppendInputResponse logs a client's response to the input request at reqSeq as an input_response event, once
// (spec §11.5 step 5, §11.7): in one transaction, under the task's row lock and at its version, only if the task,
// in the caller's scope, is not terminal, reqSeq is one of its input_request events, and no input_response answers
// it yet. ok is false when the response is not logged (the client gets 202 and it is dropped); a task outside the
// scope is a2a.ErrTaskNotFound.
func (l *Log) AppendInputResponse(ctx context.Context, scope Scope, taskID uuid.UUID, reqSeq int64, response json.RawMessage) (seq int64, ok bool, err error) {
	ctx, cancel := l.writeCtx(ctx)
	defer cancel()
	err = l.inTx(ctx, func(tx pgx.Tx) error {
		seq, ok = 0, false
		row, err := l.load(ctx, tx, taskID, " for update")
		if err != nil {
			return err
		}
		if row.TenantID != scope.TenantID || row.Service != scope.Service || row.Owner.BuyerID != scope.BuyerID || row.Protocol != ProtocolMCP {
			return notFound()
		}
		if row.State.Terminal() {
			return nil
		}
		var kind string
		if err := tx.QueryRow(ctx, `select kind from kontra.task_events where task_id = $1 and seq = $2`, taskID, reqSeq).Scan(&kind); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		if EventKind(kind) != KindInputRequest {
			return nil
		}
		answered, err := inputResponses(ctx, tx, taskID)
		if err != nil {
			return err
		}
		for _, r := range answered {
			if r.RequestSeq == reqSeq {
				return nil
			}
		}
		var o rawObject
		o.raw("inputResponse", response)
		if err := o.value("requestSeq", reqSeq); err != nil {
			return err
		}
		a, err := l.Append(ctx, tx, Write{
			TaskID: taskID, PrevVersion: row.Version, Kind: KindInputResponse, Snapshot: row.Snapshot, Payload: o.bytes(),
		})
		if err != nil {
			return err
		}
		seq, ok = a.Seq, true
		return nil
	})
	return seq, ok, err
}

// InputRequests returns the task's input_request events with seq > after, in seq order.
func (l *Log) InputRequests(ctx context.Context, taskID uuid.UUID, after int64) ([]InputRequest, error) {
	rows, err := l.db.Query(ctx, `select seq, payload from kontra.task_events
		where task_id = $1 and kind = 'input_request' and seq > $2 order by seq`, taskID, after)
	if err != nil {
		return nil, fmt.Errorf("store: input requests of task %s: %w", taskID, err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (InputRequest, error) {
		var (
			x   InputRequest
			raw []byte
		)
		if err := r.Scan(&x.Seq, &raw); err != nil {
			return x, err
		}
		var p mcpRow
		if err := json.Unmarshal(raw, &p); err != nil {
			return x, fmt.Errorf("store: input request %d of task %s: %w", x.Seq, taskID, err)
		}
		var req struct {
			ID     string `json:"id"`
			Method string `json:"method"`
		}
		var up struct {
			ID json.RawMessage `json:"id"`
		}
		_ = json.Unmarshal(p.InputRequest, &req)
		_ = json.Unmarshal(p.Upstream, &up)
		x.GateID, x.Method, x.Request, x.UpstreamID = req.ID, req.Method, p.InputRequest, up.ID
		return x, nil
	})
}

// InputResponses returns the task's input_response events with seq > after, in seq order.
func (l *Log) InputResponses(ctx context.Context, taskID uuid.UUID, after int64) ([]InputResponse, error) {
	all, err := inputResponses(ctx, l.db, taskID)
	if err != nil {
		return nil, err
	}
	out := all[:0:0]
	for _, r := range all {
		if r.Seq > after {
			out = append(out, r)
		}
	}
	return out, nil
}

func inputResponses(ctx context.Context, q querier, taskID uuid.UUID) ([]InputResponse, error) {
	rows, err := q.Query(ctx, `select seq, payload from kontra.task_events
		where task_id = $1 and kind = 'input_response' order by seq`, taskID)
	if err != nil {
		return nil, fmt.Errorf("store: input responses of task %s: %w", taskID, err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (InputResponse, error) {
		var (
			x   InputResponse
			raw []byte
		)
		if err := r.Scan(&x.Seq, &raw); err != nil {
			return x, err
		}
		var p mcpRow
		if err := json.Unmarshal(raw, &p); err != nil {
			return x, fmt.Errorf("store: input response %d of task %s: %w", x.Seq, taskID, err)
		}
		x.RequestSeq, x.Response = p.RequestSeq, p.InputResponse
		return x, nil
	})
}

// PendingInputRequests are the task's input requests that no input_response answers, in seq order.
func (l *Log) PendingInputRequests(ctx context.Context, taskID uuid.UUID) ([]InputRequest, error) {
	reqs, err := l.InputRequests(ctx, taskID, 0)
	if err != nil || len(reqs) == 0 {
		return nil, err
	}
	answered, err := inputResponses(ctx, l.db, taskID)
	if err != nil {
		return nil, err
	}
	done := make(map[int64]bool, len(answered))
	for _, r := range answered {
		done[r.RequestSeq] = true
	}
	out := reqs[:0]
	for _, r := range reqs {
		if !done[r.Seq] {
			out = append(out, r)
		}
	}
	return out, nil
}

// MCPOutcome is how an MCP task ended (spec §11.8): its terminal row's seq and kind, and for a result row the
// upstream's result or JSON-RPC error as stored.
type MCPOutcome struct {
	Seq    int64
	Kind   EventKind // result, or status for a failure or a cancel the gate wrote
	Result json.RawMessage
	Error  json.RawMessage
}

// MCPTerminal returns the terminal row of a terminal MCP task (the last task, status or result row; only metadata
// rows follow it, §5.1), ok false while the task is not terminal.
func (l *Log) MCPTerminal(ctx context.Context, taskID uuid.UUID) (MCPOutcome, bool, error) {
	var (
		o   MCPOutcome
		raw []byte
	)
	err := l.db.QueryRow(ctx, `select e.seq, e.kind, e.payload from kontra.task_events e
		join kontra.tasks t on t.id = e.task_id
		where e.task_id = $1 and t.terminal_at is not null and e.kind in ('task', 'status', 'result')
		order by e.seq desc limit 1`, taskID).Scan(&o.Seq, &o.Kind, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return MCPOutcome{}, false, nil
	}
	if err != nil {
		return MCPOutcome{}, false, fmt.Errorf("store: the terminal row of task %s: %w", taskID, err)
	}
	if o.Kind == KindResult {
		var p mcpRow
		if err := json.Unmarshal(raw, &p); err != nil {
			return MCPOutcome{}, false, fmt.Errorf("store: the result of task %s: %w", taskID, err)
		}
		o.Result, o.Error = p.Result, p.Error
	}
	return o, true, nil
}

// PayloadBytes is the bytes the task's events hold (their payloads' encoding), which limits.task_bytes bounds with
// the task's bucket objects (spec §6.6).
func (l *Log) PayloadBytes(ctx context.Context, taskID uuid.UUID) (int64, error) {
	var n int64
	err := l.db.QueryRow(ctx, `select coalesce(sum(octet_length(payload::text)), 0) + coalesce((select sum(size) from kontra.blobs where task_id = $1), 0)
		from kontra.task_events where task_id = $1`, taskID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: bytes of task %s: %w", taskID, err)
	}
	return n, nil
}

// MCPPageSize is the most tasks one MCP tasks/list page holds (spec §11.6).
const MCPPageSize = 50

// mcpCursorVersion is the first byte of every MCP tasks/list cursor: base64url(version, created_at in Unix
// microseconds as 8 big-endian bytes, the task id's 16 bytes).
const mcpCursorVersion byte = 2

// EncodeMCPCursor is the cursor of the page after the task (createdAt, id).
func EncodeMCPCursor(createdAt time.Time, id uuid.UUID) string {
	b := make([]byte, 0, 25)
	b = append(b, mcpCursorVersion)
	b = binary.BigEndian.AppendUint64(b, uint64(createdAt.UTC().Truncate(time.Microsecond).UnixMicro()))
	b = append(b, id[:]...)
	return base64.RawURLEncoding.EncodeToString(b)
}

// ErrBadCursor is a tasks/list cursor the gate did not mint.
var ErrBadCursor = errors.New("store: not a cursor this gate minted")

// ListMCP returns one page of the buyer's own MCP tasks of the service, newest first (created_at desc, id desc),
// after cursor ("" for the first page), and the next page's cursor ("" when it is the last) (spec §11.6).
func (l *Log) ListMCP(ctx context.Context, scope Scope, cursor string) ([]*TaskRow, string, error) {
	var at, after any
	if cursor != "" {
		b, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil || len(b) != 25 || b[0] != mcpCursorVersion {
			return nil, "", ErrBadCursor
		}
		id, err := uuid.FromBytes(b[9:])
		if err != nil {
			return nil, "", ErrBadCursor
		}
		at, after = time.UnixMicro(int64(binary.BigEndian.Uint64(b[1:9]))).UTC(), id
	}
	rows, err := l.db.Query(ctx, `select `+taskCols+` from kontra.tasks t
		where t.tenant_id = $1 and t.service = $2 and t.buyer_id = $3 and t.protocol = 'mcp'
		  and ($4::timestamptz is null or (t.created_at, t.id) < ($4::timestamptz, $5::uuid))
		order by t.created_at desc, t.id desc limit $6`, scope.TenantID, scope.Service, scope.BuyerID, at, after, MCPPageSize+1)
	if err != nil {
		return nil, "", fmt.Errorf("store: list MCP tasks: %w", err)
	}
	defer rows.Close()
	var out []*TaskRow
	for rows.Next() {
		r, err := scanTask(rows)
		if err != nil {
			return nil, "", fmt.Errorf("store: list MCP tasks: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("store: list MCP tasks: %w", err)
	}
	next := ""
	if len(out) > MCPPageSize {
		out = out[:MCPPageSize]
		last := out[len(out)-1]
		next = EncodeMCPCursor(last.CreatedAt, last.ID)
	}
	return out, next, nil
}

// foldMCP folds one row of an MCP task (Folder with MCP set): the task row is the creation snapshot; status and
// result rows set the state, statusMessage, time and metadata as their writers did (mcpApply); input requests and
// responses change nothing of the projection; metadata rows merge their metadata and mcpMeta. After the terminal
// row only metadata rows may follow.
func foldMCP(task *a2a.Task, ev Event) (*a2a.Task, error) {
	var p mcpRow
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return nil, err
	}
	if task == nil {
		if ev.Kind != KindTask || p.Task == nil || p.Task.LastUpdatedAt == nil {
			return nil, errors.New("the first row is not the task row")
		}
		return mcpApply(nil, p.Task.TaskID, ev.Seq, p.Task.Status, p.Task.StatusMessage, *p.Task.LastUpdatedAt, p.Task.Metadata), nil
	}
	if ev.Kind == KindMetadata {
		if p.Metadata == nil {
			return nil, errors.New(`a metadata row without "metadata"`)
		}
		return mergeMCPMeta(mergeMetadata(task, p.Metadata), p.MCPMeta), nil
	}
	if task.Status.State.Terminal() {
		return nil, fmt.Errorf("a %s row follows the terminal row", ev.Kind)
	}
	switch ev.Kind {
	case KindStatus, KindResult:
		if !p.Status.Valid() || p.LastUpdatedAt == nil {
			return nil, fmt.Errorf("a %s row without its status", ev.Kind)
		}
		return mcpApply(task, string(task.ID), ev.Seq, p.Status, p.StatusMessage, *p.LastUpdatedAt, p.Metadata), nil
	case KindInputRequest, KindInputResponse:
		return task, nil
	}
	return nil, fmt.Errorf("%s rows are an A2A task's, not an MCP task's", ev.Kind)
}
