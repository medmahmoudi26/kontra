package gatemcp

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"kontra.sh/kontra/internal/mcpwire"
	"kontra.sh/kontra/internal/store"
	"kontra.sh/kontra/internal/version"
)

// The 2026-07-28 era on /mcp (spec/relay.md §12.5, §11.12; D24, D159, D176, D178, D269, D274, D295; M4 WP-05).
// A modern request names no session and opens none: the gate is the only server, so nothing is pinned, nothing hops
// and no era is learnt; no virtual session is opened and Admission's session operation is not asked (/mcp is not a
// service, D269). Each hire is an Admission with operation task, through the Hirer, as in the legacy era. Every
// result carries resultType (2026-07-28: "complete", a CreateTaskResult's "task").

// ModernInlineWait is how long a modern hire that declares the tasks extension waits for its task to end before it is
// answered with a CreateTaskResult (§11.12 step 4, D159).
const ModernInlineWait = 2 * time.Second

// ModernListTTL is the ttlMs of the gate's server/discover result and of its modern tools/list (2026-07-28
// CacheableResult): §11.12's bound on a relayed list. The four tools change only with the gate's version.
const ModernListTTL = 10 * time.Minute

// modernHireBound bounds a modern hire's creation, which runs on a context the client cannot cancel: a client that
// leaves while its task is created has cancelled the request, and the gate must learn the task's id to cancel it
// (§11.12 step 4, D178). Admission (at most 30 s, hooks.admission.timeout) and the lifecycle's acceptance
// (lifecycle.AcceptWait) end first.
const modernHireBound = time.Minute

// resultType values (2026-07-28 Result; the tasks extension's "task").
const (
	resultComplete = "complete"
	resultTask     = "task"
)

// modern serves a 2026-07-28 POST on /mcp (§12.5), after §11.1's one-message rule, strict decoding, Origin and §11.12's
// classification (rule 2: its Mcp-Session-Id and Last-Event-ID are never read): CheckModern's refusals first (a
// response 400 -32600, a notification 202, a missing _meta field 400 -32602, a version, Mcp-Method or Mcp-Name that
// does not match the body 400 -32020, D176); then server/discover answered by the gate, the modern tools/list, the
// four tools (a hire declaring the tasks extension is answered as §11.12 step 4), tasks/get, tasks/update and
// tasks/cancel over hire tasks (-32021 without the extension, D176), tasks/result and tasks/list -32601 (the
// extension removes them), and any other method 404 with -32601 (2026-07-28 Streamable HTTP: a method the server
// does not implement), subscriptions/listen included (D295).
func (h *Handler) modern(rq *request) {
	m := rq.msg
	if ref := mcpwire.CheckModern(rq.r.Header, m); ref != nil {
		writeRefusal(rq.w, ref)
		return
	}
	switch m.Method {
	case mcpwire.MethodServerDiscover:
		res, err := DiscoverResult()
		if err != nil {
			h.internal(rq, "write the server/discover result", err)
			return
		}
		rq.result(res)
	case mcpwire.MethodToolsList:
		res, err := ToolsList(mcpwire.EraModern, mcpwire.V20260728)
		if err != nil {
			h.internal(rq, "list the tools", err)
			return
		}
		rq.result(res)
	case mcpwire.MethodToolsCall:
		h.toolsCall(rq)
	case "tasks/get", "tasks/update", "tasks/cancel":
		h.modernTasks(rq)
	case mcpwire.MethodListen:
		if mcpwire.ListensForTasks(m.Params) && !mcpwire.DeclaresTasks(m.Params) {
			// The tasks extension: a listen asking for task notifications without declaring it is -32021 (D176).
			writeRefusal(rq.w, mcpwire.MissingTasks(m.ID))
			return
		}
		// /mcp offers no notification: its tools never change while it runs, and notifications/tasks is not offered
		// to modern clients (D161), whose hire tasks are polled with tasks/get (D295).
		writeError(rq.w, http.StatusNotFound, m.ID, mcpwire.CodeMethodNotFound, "Method not found", nil)
	default:
		// tasks/result and tasks/list (the extension removes them, §12.5), ping (2026-07-28 removed it), and every
		// method /mcp does not serve: 404 with -32601 (2026-07-28 Streamable HTTP, "Protocol Version Header").
		writeError(rq.w, http.StatusNotFound, m.ID, mcpwire.CodeMethodNotFound, "Method not found", nil)
	}
}

// DiscoverResult is /mcp's answer to server/discover (§12.5, D269): the three versions the gate speaks, newest
// first; capabilities tools and the tasks extension; the gate's serverInfo in _meta; instructions; ttlMs
// ModernListTTL and cacheScope private (2026-07-28 CacheableResult); resultType complete. It is the same on every
// replica and for every principal.
func DiscoverResult() (json.RawMessage, error) {
	return json.Marshal(map[string]any{
		"resultType":        resultComplete,
		"supportedVersions": mcpwire.Supported,
		"capabilities": map[string]any{
			"tools":      map[string]any{},
			"extensions": map[string]any{mcpwire.ExtTasks: map[string]any{}},
		},
		"_meta":        map[string]any{mcpwire.MetaServerInfo: serverInfo()},
		"instructions": instructions,
		"ttlMs":        ModernListTTL.Milliseconds(),
		"cacheScope":   "private",
	})
}

// serverInfo names the gate as an MCP server (initialize's serverInfo, server/discover's _meta).
func serverInfo() map[string]any {
	return map[string]any{"name": "kontra-gate", "title": "Kontra gate", "version": version.Get()}
}

// instructions is what /mcp tells a model about itself (initialize, server/discover).
const instructions = "Find agents with search, read one with describe, and hire it with hire; answer a hired task's " +
	"question with reply. A priced task waits for a person to approve its quote at the URL hire gives."

// modernTasks serves a modern tasks/get, tasks/update or tasks/cancel over the principal's hire tasks (§12.5, §11.12
// "The client side"), from Postgres on any replica: a request that does not declare the tasks extension is 400 with
// -32021 (D176), before the task is read.
func (h *Handler) modernTasks(rq *request) {
	m := rq.msg
	if !mcpwire.DeclaresTasks(m.Params) {
		writeRefusal(rq.w, mcpwire.MissingTasks(m.ID))
		return
	}
	t, rd := h.deps.Tasks, rq.reader()
	var (
		res    json.RawMessage
		rpcErr *mcpwire.Error
		err    error
	)
	switch m.Method {
	case "tasks/get":
		res, rpcErr, err = t.Get(rq.ctx(), rd, taskIDParam(m.Params))
	case "tasks/update":
		res, rpcErr, err = t.Update(rq.ctx(), rd, m.Params)
	case "tasks/cancel":
		res, rpcErr, err = t.Cancel(rq.ctx(), rd, taskIDParam(m.Params))
	}
	h.writeTaskAnswer(rq, res, rpcErr, err)
}

// answerModernHire answers a modern hire whose task the Hirer created or found again (§12.5, §11.12 step 4):
//
//   - without the tasks extension it is a plain call (§12.2): quoted at once with the approve URL, else after the
//     plain wait;
//   - with it, a quoted task at once as CreateTaskResult working with the approve URL in statusMessage and
//     _meta["sh.kontra/approve-url"] (D21; never -32042, which 2026-07-28 removed: D24, D269); any other is waited
//     for up to ModernInlineWait and, once ended, answered in line as tasks/result renders it (a CallToolResult with
//     resultType complete: the client never sees a task); else (still running, or asking a question, which the client
//     cannot answer before it learns the task's id) as CreateTaskResult with pollIntervalMs 1000.
//
// Either answer is written after the task row is committed (the Hirer returns then), so a tasks/get for it resolves.
// A client that closes its response stream before the answer is written has cancelled the request (2026-07-28
// Streamable HTTP, "Cancellation": a server MUST treat a disconnect so) and never learned the task's id: a task the
// request created is sent kontra.cancel in MCP mode, not awaited (D57: canceled within 10 s; D178). A task the request
// found again (a retry, §12.3) was the earlier request's, and is left as it is.
//
// SPEC-CONFLICT (relay §11.12 step 4 "a client that closes its response stream before either answer is sent has
// cancelled the request: the gate sends the task kontra.cancel", which §12.5 applies to hire, quoted tasks included):
// a quoted hire task runs nothing until a person approves it, and §12.3's retry is how a client gets it again: a
// hire with the same arguments within 10 minutes of the task leaving quoted answers that task. A quoted task
// cancelled because its client left would answer the client's re-issued request (2026-07-28: a broken stream's
// request MUST be re-issued) with "the task was canceled" for 10 minutes. The closest faithful behaviour: a quoted
// task is not cancelled when its client leaves; it ends on its own when its quote expires, and the re-issued hire
// gets it with its approve URL. Recorded as D295, and §12.5 now says so; §11.12 step 4 stands for /mcp/{svc}, whose
// MCP tasks have no retry.
func (h *Handler) answerModernHire(rq *request, out HireOutcome, svc store.ServiceRef, augmented bool) {
	row := out.Task
	cancelIfLeft := func(why string) bool {
		if rq.ctx().Err() == nil {
			return false
		}
		if !out.Existing && row.State != store.StateQuoted && !row.State.Terminal() {
			h.deps.Tasks.cancelDetached(row.ID, why)
		}
		return true
	}
	if !augmented {
		if row.State == store.StateQuoted {
			rq.result(quotedResult(row))
			return
		}
		if !h.waitAndAnswer(rq, row.ID, 0) {
			if r, err := h.deps.Log.Load(context.WithoutCancel(rq.ctx()), row.ID); err == nil {
				row = r
			}
			cancelIfLeft("the client closed its stream before a plain hire was answered (2026-07-28 cancellation)")
		}
		return
	}
	const left = "the client closed its stream before the hire was answered (D178)"
	ctx := rq.ctx()
	if row.State != store.StateQuoted {
		wake, stop := h.deps.Waker.WaitTask(ctx, row.ID)
		defer stop()
		until := time.NewTimer(ModernInlineWait)
		defer until.Stop()
		reread := time.NewTicker(250 * time.Millisecond)
		defer reread.Stop()
	wait:
		for !row.State.Terminal() && row.State != store.StateInputRequired {
			select {
			case <-ctx.Done():
				break wait
			case <-until.C:
				break wait
			case <-wake:
			case <-reread.C:
			}
			if r, err := h.deps.Log.Load(ctx, row.ID); err == nil {
				row = r
			}
		}
	}
	if cancelIfLeft(left) {
		return
	}
	if row.State.Terminal() {
		res := h.plainAnswer(ctx, row, rq.p)
		if cancelIfLeft(left) {
			return
		}
		rq.result(res)
		return
	}
	res, err := modernCreateTaskResult(row, svc, rq.reader())
	if err != nil {
		if cancelIfLeft(left) {
			return
		}
		h.internal(rq, "write the task", err)
		return
	}
	if cancelIfLeft(left) {
		return
	}
	writeResult(rq.w, "", rq.msg.ID, res)
}

// modernView is a hire task as a modern reader sees it (§12.4, D274, and §11.12's table): the legacy view, except
// that a task that ended failed or rejected is completed, its result the CallToolResult with isError true that §12.2
// gives every such hire task (the tasks extension: "failed" is for a JSON-RPC error and MUST NOT carry a tool result
// with isError, which is "completed"), its sh.kontra/failure kept in _meta. canceled is cancelled.
func modernView(row *store.TaskRow, rd Reader) taskView {
	v := view(row, rd)
	if row.State == store.StateFailed || row.State == store.StateRejected {
		v.status = "completed"
	}
	return v
}

// modernTaskObject is a hire task as the tasks extension's Task (ext-tasks 2026-07-28, "Tasks"): taskId, status,
// statusMessage when there is one, createdAt, lastUpdatedAt, ttlMs (§11.6's ttl), pollIntervalMs 1000, and the view's
// _meta, with resultType.
func modernTaskObject(row *store.TaskRow, svc store.ServiceRef, v taskView, resultType string) map[string]any {
	o := map[string]any{
		"resultType": resultType, "taskId": row.ID.String(), "status": v.status, "createdAt": mcpTime(row.CreatedAt),
		"lastUpdatedAt": mcpTime(row.StatusAt), "ttlMs": ttlOf(row, svc), "pollIntervalMs": store.MCPPollInterval,
	}
	if v.statusMessage != "" {
		o["statusMessage"] = v.statusMessage
	}
	if len(v.meta) > 0 {
		o["_meta"] = v.meta
	}
	return o
}

// modernCreateTaskResult is a modern hire's CreateTaskResult (§11.12 step 4): resultType "task" and the Task, a
// quoted task working with the approve URL in statusMessage and _meta["sh.kontra/approve-url"] (D21).
func modernCreateTaskResult(row *store.TaskRow, svc store.ServiceRef, rd Reader) (json.RawMessage, error) {
	return json.Marshal(modernTaskObject(row, svc, modernView(row, rd), resultTask))
}

// formRequest is §12.4's question as a modern input request (D274): elicitation/create in form mode, the agent's
// text as its message and one required string, reply.
func formRequest(text string) map[string]any {
	return map[string]any{"method": mcpwire.MethodElicit, "params": map[string]any{
		"mode": "form", "message": text, "requestedSchema": replySchema(),
	}}
}

// replySchema is the requestedSchema of §12.4's form: {reply: string}, required.
func replySchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"reply": map[string]any{"type": "string"}},
		"required": []string{"reply"}}
}

// withResultType is res, a JSON object, with resultType set to rt when it has none; res unchanged otherwise (and
// when it is not an object).
func withResultType(res json.RawMessage, rt string) json.RawMessage {
	if firstByte(res) != '{' {
		return res
	}
	if _, ok := mcpwire.Member(res, "resultType"); ok {
		return res
	}
	b, _ := json.Marshal(rt)
	out, err := mcpwire.SetMember(res, "resultType", b)
	if err != nil {
		return res
	}
	return out
}
