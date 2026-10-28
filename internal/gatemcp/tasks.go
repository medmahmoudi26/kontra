package gatemcp

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"

	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/mcpwire"
	"kontra.sh/kontra/internal/store"
	"kontra.sh/kontra/internal/unimpl"
)

// HireTasks is the MCP Tasks surface of hire tasks (spec §12.2, §12.4, §12.5): §11.6 for a legacy reader and
// §11.12's tasks/get, tasks/update and tasks/cancel for a modern one, over A2A gate tasks (protocol a2a) marked
// tasks.hired, scoped to the principal's buyer_id (another buyer's or an unhired task is -32602, as an unknown id).
// It reads only Postgres, on any replica.
type HireTasks struct {
	log   *store.Log
	waker store.TaskWaker
}

// NewHireTasks reads from log and waits on waker.
func NewHireTasks(log *store.Log, waker store.TaskWaker) *HireTasks {
	return &HireTasks{log: log, waker: waker}
}

// Reader is who reads a task and what it declared: the reader's era and, for §12.4, whether its session (legacy)
// or its request (modern) declared form elicitation (DM4-7).
type Reader struct {
	Principal       auth.Principal
	Era             mcpwire.Era
	FormElicitation bool
	SessionHash     *[32]byte // legacy only
}

// Get answers tasks/get: the task in the reader's era's vocabulary (§5.1, §12.4), quoted as working with the
// approve URL (D21), and sh.kontra/failure or sh.kontra/canceled in _meta for an ended one (§12.2).
func (t *HireTasks) Get(ctx context.Context, rd Reader, taskID uuid.UUID) (json.RawMessage, *mcpwire.Error, error) {
	return nil, nil, unimpl.Error("gatemcp.HireTasks.Get", "M4 WP-04")
}

// Result answers a legacy tasks/result: blocks (on the waker) until the task ends, delivering §12.4's form
// elicitation on its own SSE response stream first, then the rendered outcome (Render) with the related-task _meta.
func (t *HireTasks) Result(ctx context.Context, w http.ResponseWriter, rd Reader, m *mcpwire.Message) {
	panic(unimpl.Error("gatemcp.HireTasks.Result", "M4 WP-04"))
}

// List answers a legacy tasks/list: the buyer's hire tasks across services, newest first, gate-minted cursors.
func (t *HireTasks) List(ctx context.Context, rd Reader, params json.RawMessage) (json.RawMessage, *mcpwire.Error, error) {
	return nil, nil, unimpl.Error("gatemcp.HireTasks.List", "M4 WP-04")
}

// Cancel answers tasks/cancel: §11.6's rule (D57: canceled within 10 s, whatever the upstream answers; the upstream
// receives §8's CancelTask); a modern reader is answered without waiting (§11.12).
func (t *HireTasks) Cancel(ctx context.Context, rd Reader, taskID uuid.UUID) (json.RawMessage, *mcpwire.Error, error) {
	return nil, nil, unimpl.Error("gatemcp.HireTasks.Cancel", "M4 WP-05")
}

// Update answers a modern tasks/update: an accept answer to the outstanding form elicitation becomes the §7
// follow-up, once (§11.12's rule); other keys are ignored (§12.4, DM4-7).
func (t *HireTasks) Update(ctx context.Context, rd Reader, params json.RawMessage) (json.RawMessage, *mcpwire.Error, error) {
	return nil, nil, unimpl.Error("gatemcp.HireTasks.Update", "M4 WP-05")
}

// Respond takes a legacy client's response to a gate-written elicitation/create (id k.<taskId>.<seq>, §11.7):
// accept becomes the §7 follow-up, once and only from a session of the task's buyer; decline and cancel leave the
// task waiting. It reports whether the response was the gate's to consume.
func (t *HireTasks) Respond(ctx context.Context, rd Reader, m *mcpwire.Message) bool {
	panic(unimpl.Error("gatemcp.HireTasks.Respond", "M4 WP-04"))
}

// Render is an ended hire task's outcome as MCP content (§12.2): text parts as text, data parts as text (JSON) and
// in structuredContent, file parts as resource blocks, from the final status message and the artifacts; a CallToolResult
// whatever the state, isError for failed, rejected and canceled, with the final status message as text and
// sh.kontra/failure or sh.kontra/canceled in _meta.
func Render(task *a2a.Task, state store.State) (json.RawMessage, error) {
	return nil, unimpl.Error("gatemcp.Render", "M4 WP-04")
}

// Question is §12.4's reading of an input-required hire task for a reader: whether it is input_required (a
// text-only question and a reader that declared form elicitation) and the text the reply-tool form shows otherwise:
// the agent's text and " (answer with the reply tool, taskId <id>)", with _meta["sh.kontra/input-required"].
func Question(task *a2a.Task, rd Reader) (inputRequired bool, statusMessage string, err error) {
	return false, "", unimpl.Error("gatemcp.Question", "M4 WP-04")
}
