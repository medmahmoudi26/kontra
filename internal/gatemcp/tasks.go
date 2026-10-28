package gatemcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"

	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/blob"
	"kontra.sh/kontra/internal/lifecycle"
	"kontra.sh/kontra/internal/mcprelay"
	"kontra.sh/kontra/internal/mcpwire"
	"kontra.sh/kontra/internal/store"
	"kontra.sh/kontra/internal/strictjson"
	"kontra.sh/kontra/internal/unimpl"
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
}

// HireLifecycle is what the MCP Tasks of hire tasks ask of TaskLifecycle (*lifecycle.Client): MCP's tasks/cancel on an
// A2A task (§11.6, D57; the cancel's MCP mode).
type HireLifecycle interface {
	CancelMCP(ctx context.Context, taskID a2a.TaskID) (lifecycle.CancelResult, bool, error)
}

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
	return &HireTasks{log: cfg.Log, waker: cfg.Waker, lc: cfg.Lifecycle, hirer: cfg.Hirer, render: cfg.Render,
		logger: cfg.Logger.With("component", "gate-mcp-tasks"), keepAlive: keepAliveEvery, reread: 5 * time.Second}, nil
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

// load is the reader's hire task taskID names, nil when it names none of them.
func (t *HireTasks) load(ctx context.Context, rd Reader, taskID uuid.UUID) (*store.TaskRow, error) {
	if taskID == uuid.Nil {
		return nil, nil
	}
	row, err := t.log.LoadHired(ctx, rd.Principal.BuyerID, taskID)
	if errors.Is(err, a2a.ErrTaskNotFound) {
		return nil, nil
	}
	return row, err
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
// approve URL (D21), and sh.kontra/failure or sh.kontra/canceled in _meta for an ended one (§12.2).
func (t *HireTasks) Get(ctx context.Context, rd Reader, taskID uuid.UUID) (json.RawMessage, *mcpwire.Error, error) {
	row, err := t.load(ctx, rd, taskID)
	if err != nil || row == nil {
		return nil, errTaskNotFound, err
	}
	svc, err := t.service(ctx, row)
	if err != nil {
		return nil, nil, err
	}
	res, err := taskResult(row, svc, rd)
	return res, nil, err
}

// Result answers a legacy tasks/result: blocks (on the waker) until the task ends, delivering §12.4's form
// elicitation on its own SSE response stream first (a text-only question, to a reader that declared form
// elicitation: elicitation/create {mode: form, message, requestedSchema {reply: string}} with the related-task _meta,
// its JSON-RPC and event id the gate id k.<taskId>.<seq> of the event that asked), then the rendered outcome (Render)
// with the related-task _meta. A task that has ended when asked is answered in JSON. sid is the session's
// Mcp-Session-Id.
func (t *HireTasks) Result(ctx context.Context, w http.ResponseWriter, sid string, rd Reader, m *mcpwire.Message) {
	row, err := t.load(ctx, rd, taskIDParam(m.Params))
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
		row, err = t.log.Load(ctx, id)
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
			if ir, text, _ := Question(row.Snapshot, rd); ir {
				seq, err := t.log.StatusSeq(ctx, id)
				if err == nil && !delivered[seq] {
					gateID := store.GateRequestID(id, seq)
					if err := s.Event(gateID, elicitationRequest(gateID, id, text)); err != nil {
						return
					}
					delivered[seq] = true
				}
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
		"mode": "form", "message": text,
		"requestedSchema": map[string]any{"type": "object", "properties": map[string]any{"reply": map[string]any{"type": "string"}},
			"required": []string{"reply"}},
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
// task as it is then, status cancelled. (M4 WP-04, the legacy tasks/cancel; WP-05 adds the modern one, answered
// without waiting.)
func (t *HireTasks) Cancel(ctx context.Context, rd Reader, taskID uuid.UUID) (json.RawMessage, *mcpwire.Error, error) {
	row, err := t.load(ctx, rd, taskID)
	if err != nil || row == nil {
		return nil, errTaskNotFound, err
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

// Update answers a modern tasks/update: an accept answer to the outstanding form elicitation becomes the §7
// follow-up, once (§11.12's rule); other keys are ignored, and so is any answer about a quoted task (§12.4, DM4-7).
func (t *HireTasks) Update(ctx context.Context, rd Reader, params json.RawMessage) (json.RawMessage, *mcpwire.Error, error) {
	return nil, nil, unimpl.Error("gatemcp.HireTasks.Update", "M4 WP-05")
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
	row, err := t.load(ctx, rd, taskID)
	if err != nil || row == nil || row.State != store.StateInputRequired {
		return false
	}
	if cur, err := t.log.StatusSeq(ctx, taskID); err != nil || cur != seq {
		return false
	}
	if ir, _, _ := Question(row.Snapshot, rd); !ir {
		return false
	}
	reply, ok := acceptedReply(m.Result)
	if !ok {
		return false
	}
	if _, err := t.hirer.Reply(ctx, rd.Principal, row, a2a.ContentParts{a2a.NewTextPart(reply)}); err != nil {
		t.logger.Info("an answer to a hire task's question was not taken", "task_id", taskID, "request_seq", seq, "error", err)
		return false
	}
	return true
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
	switch {
	case errors.Is(err, errLifecycleUnavailable):
		h.log.Warn("mcp: a task's lifecycle did not accept the request in time", "method", m.Method, "error", err)
		writeError(rq.w, http.StatusServiceUnavailable, m.ID, mcpwire.CodeInternal, "the task's lifecycle is unavailable", nil)
	case err != nil:
		h.internal(rq, "serve "+m.Method, err)
	case rpcErr != nil:
		writeRPCError(rq.w, rq.sid, m.ID, rpcErr)
	default:
		writeResult(rq.w, rq.sid, m.ID, res)
	}
}
