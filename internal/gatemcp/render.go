package gatemcp

import (
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"

	"kontra.sh/kontra/internal/lifecycle"
	"kontra.sh/kontra/internal/mcpwire"
	"kontra.sh/kontra/internal/store"
)

// _meta keys of /mcp's answers (spec §5.1, §11.8, §12.2, §12.4; D21, D58).
const (
	MetaApproveURL    = mcpwire.KontraPrefix + "approve-url"
	MetaFailure       = mcpwire.KontraPrefix + "failure"
	MetaCanceled      = mcpwire.KontraPrefix + "canceled"
	MetaInputRequired = mcpwire.KontraPrefix + "input-required"
)

// content is one MCP content block of a rendered result.
type content struct {
	Type     string         `json:"type"`
	Text     string         `json:"text,omitempty"`
	Resource *resource      `json:"resource,omitempty"`
	URI      string         `json:"uri,omitempty"`  // resource_link
	Name     string         `json:"name,omitempty"` // resource_link
	MIMEType string         `json:"mimeType,omitempty"`
	Meta     map[string]any `json:"_meta,omitempty"`
}

// resource is an embedded resource's contents (BlobResourceContents).
type resource struct {
	URI      string `json:"uri"`
	MIMEType string `json:"mimeType,omitempty"`
	Blob     string `json:"blob"`
}

// callToolResult is a CallToolResult as /mcp writes it.
type callToolResult struct {
	Content           []content      `json:"content"`
	StructuredContent any            `json:"structuredContent,omitempty"`
	IsError           bool           `json:"isError,omitempty"`
	Meta              map[string]any `json:"_meta,omitempty"`
}

// errorResult is a CallToolResult with isError and one text block: a tool execution error (MCP 2025-11-25: an
// input validation error, an unknown service, a refusal).
func errorResult(text string, meta map[string]any) json.RawMessage {
	b, _ := json.Marshal(callToolResult{Content: []content{{Type: "text", Text: text}}, IsError: true, Meta: meta})
	return b
}

// textResult is a CallToolResult with one text block and structuredContent.
func textResult(text string, structured any, meta map[string]any) json.RawMessage {
	b, _ := json.Marshal(callToolResult{Content: []content{{Type: "text", Text: text}}, StructuredContent: structured, Meta: meta})
	return b
}

// Render is an ended hire task's outcome as MCP content (§12.2, DM4-22): text parts as text, data parts as text (the
// value's JSON) and in structuredContent.data, file parts as resource blocks (raw bytes embedded, a URL as a
// resource_link), from the final status message and then each artifact; structuredContent {taskId, state, data?}
// with the A2A state. A CallToolResult whatever the state: isError for failed, rejected and canceled, with the final
// status message as text (the failure's reason text when it has none) and sh.kontra/failure (§11.8's reason; a
// rejected task's is "rejected") or sh.kontra/canceled in _meta. Kontra-blob parts must have been rendered as
// presigned URLs before (blob.Renderer.Task).
func Render(task *a2a.Task, state store.State) (json.RawMessage, error) {
	res := render(task, state)
	return json.Marshal(res)
}

func render(task *a2a.Task, state store.State) callToolResult {
	var (
		blocks []content
		data   []any
		n      int
	)
	add := func(parts a2a.ContentParts) {
		for _, p := range parts {
			if p == nil {
				continue
			}
			n++
			switch v := p.Content.(type) {
			case a2a.Text:
				blocks = append(blocks, content{Type: "text", Text: string(v)})
			case a2a.Data:
				js, err := json.Marshal(v.Value)
				if err != nil {
					js = []byte("null")
				}
				blocks = append(blocks, content{Type: "text", Text: string(js)})
				data = append(data, v.Value)
			case a2a.Raw:
				blocks = append(blocks, content{Type: "resource", Resource: &resource{
					URI: partURI(task, p, n), MIMEType: p.MediaType, Blob: base64.StdEncoding.EncodeToString(v)}})
			case a2a.URL:
				name := p.Filename
				if name == "" {
					name = string(v)
				}
				blocks = append(blocks, content{Type: "resource_link", URI: string(v), Name: name, MIMEType: p.MediaType})
			}
		}
	}
	var meta map[string]any
	var md map[string]any
	if task != nil {
		md = task.Metadata
		if task.Status.Message != nil {
			add(task.Status.Message.Parts)
		}
		for _, a := range task.Artifacts {
			if a != nil {
				add(a.Parts)
			}
		}
	}
	isError := false
	switch state {
	case store.StateFailed, store.StateRejected:
		isError = true
		meta = map[string]any{MetaFailure: failureOf(state, md)}
		if statusText(task) == "" {
			text := rejectedText
			if state == store.StateFailed {
				text = store.ReasonText(store.FailureReason(failureOf(state, md)))
				if text == "" {
					text = "the task failed"
				}
			}
			blocks = append([]content{{Type: "text", Text: text}}, blocks...)
		}
	case store.StateCanceled:
		isError = true
		why, _ := md[store.MetaCanceled].(string)
		if why == "" {
			why = store.CanceledClient
		}
		meta = map[string]any{MetaCanceled: why}
		if statusText(task) == "" {
			blocks = append([]content{{Type: "text", Text: canceledText(why)}}, blocks...)
		}
	}
	if len(blocks) == 0 {
		blocks = []content{{Type: "text", Text: "the task " + string(state)}}
	}
	id := ""
	if task != nil {
		id = string(task.ID)
	}
	return callToolResult{Content: blocks, IsError: isError, Meta: meta,
		StructuredContent: PlainState{TaskID: id, State: string(state.A2A()), Data: data}}
}

// FailureRejected is sh.kontra/failure of a hire task the agent rejected (A2A rejected, MCP "failed"): §12.2 gives
// every failed, rejected or canceled hire task sh.kontra/failure or sh.kontra/canceled, and §11.8's reasons are the
// gate's failures, none of which a rejection is (DM4-26).
const FailureRejected = "rejected"

// rejectedText is a rejected task's text when its status message has none.
const rejectedText = "the task was rejected"

// failureOf is sh.kontra/failure of a task that ended failed (its kontra.failure reason, upstream_error when it has
// none) or rejected (FailureRejected).
func failureOf(state store.State, md map[string]any) string {
	if state == store.StateRejected {
		return FailureRejected
	}
	reason, _ := md[store.MetaFailure].(string)
	if reason == "" {
		reason = string(store.ReasonUpstreamError)
	}
	return reason
}

// canceledText says why a task was canceled (relay §13.2: a declined quote, an expired one, the client's cancel).
func canceledText(why string) string {
	switch why {
	case store.CanceledDeclined:
		return "the quote was declined: the task was canceled and nothing was charged"
	case store.CanceledQuoteExpired:
		return "the quote expired before it was approved: the task was canceled and nothing was charged"
	}
	return "the task was canceled"
}

// partURI names a file part's embedded resource: the task and the part's file name, or its place.
func partURI(task *a2a.Task, p *a2a.Part, n int) string {
	id := ""
	if task != nil {
		id = string(task.ID)
	}
	name := p.Filename
	if name == "" {
		name = "part-" + strconv.Itoa(n)
	}
	return "kontra:task/" + id + "/" + name
}

// statusText is the text parts of the task's status message (§12.4: a hire task's statusMessage), "" when none.
func statusText(task *a2a.Task) string {
	if task == nil || task.Status.Message == nil {
		return ""
	}
	var b strings.Builder
	for _, p := range task.Status.Message.Parts {
		if p == nil {
			continue
		}
		if t, ok := p.Content.(a2a.Text); ok {
			b.WriteString(string(t))
		}
	}
	return b.String()
}

// textOnly reports whether the task's status message has parts, all of them text (§12.4).
func textOnly(task *a2a.Task) bool {
	if task == nil || task.Status.Message == nil || len(task.Status.Message.Parts) == 0 {
		return false
	}
	for _, p := range task.Status.Message.Parts {
		if p == nil {
			return false
		}
		if _, ok := p.Content.(a2a.Text); !ok {
			return false
		}
	}
	return true
}

// Question is §12.4's reading of an input-required hire task for a reader: whether it is input_required (a
// text-only question and a reader that declared form elicitation) and the text the reply-tool form shows otherwise:
// the agent's text and " (answer with the reply tool, taskId <id>)", with _meta["sh.kontra/input-required"].
func Question(task *a2a.Task, rd Reader) (inputRequired bool, statusMessage string, err error) {
	text := statusText(task)
	if textOnly(task) && rd.FormElicitation {
		return true, text, nil
	}
	id := ""
	if task != nil {
		id = string(task.ID)
	}
	hint := "(answer with the reply tool, taskId " + id + ")"
	if text == "" {
		return false, hint, nil
	}
	return false, text + " " + hint, nil
}

// approveURL is a quoted task's approve URL (metadata kontra.approve.url, §13.2), "" when none.
func approveURL(task *a2a.Task) string {
	if task == nil {
		return ""
	}
	u, _ := task.Metadata[lifecycle.MetaApproveURL].(string)
	return u
}

// mcpTime is an MCP timestamp: ISO 8601 in UTC, to the millisecond.
func mcpTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// ttlOf is a task's ttl in milliseconds (§11.6): retention_until − created_at once terminal; before, an upper bound:
// the deadline (a quoted task's: its quote's expiry plus the task timeout) plus the retention.
func ttlOf(row *store.TaskRow, svc store.ServiceRef) int64 {
	var end time.Time
	switch {
	case row.RetentionUntil != nil:
		end = *row.RetentionUntil
	case row.Deadline != nil:
		end = row.Deadline.Add(svc.Retention)
	case row.QuoteExpiresAt != nil:
		end = row.QuoteExpiresAt.Add(svc.Timeout + svc.Retention)
	default:
		end = row.CreatedAt.Add(svc.Timeout + svc.Retention)
	}
	return max(end.Sub(row.CreatedAt).Milliseconds(), 0)
}

// taskView is a hire task as an MCP reader sees it (§5.1, §12.2, §12.4): its status in MCP's vocabulary, its
// statusMessage, and the _meta the gate adds.
type taskView struct {
	status        string
	statusMessage string
	meta          map[string]any
}

// view presents a hire task to rd (§5.1, §12.4): quoted is working with the approve URL as statusMessage and in
// _meta (D21); input-required is input_required for a text-only question when rd declared form elicitation, else
// working with the reply-tool text and _meta sh.kontra/input-required (D58); failed has sh.kontra/failure (§11.8),
// rejected sh.kontra/failure "rejected" (FailureRejected), canceled sh.kontra/canceled; the statusMessage is otherwise
// the text of the status message.
func view(row *store.TaskRow, rd Reader) taskView {
	t := row.Snapshot
	v := taskView{status: row.State.MCPStatus(), statusMessage: statusText(t)}
	var md map[string]any
	if t != nil {
		md = t.Metadata
	}
	switch row.State {
	case store.StateQuoted:
		u := approveURL(t)
		v.statusMessage = u
		v.meta = map[string]any{MetaApproveURL: u}
	case store.StateInputRequired:
		ir, text, _ := Question(t, rd)
		v.statusMessage = text
		if !ir {
			v.status = "working"
			v.meta = map[string]any{MetaInputRequired: true}
		}
	case store.StateFailed:
		reason := failureOf(row.State, md)
		v.meta = map[string]any{MetaFailure: reason}
		if v.statusMessage == "" {
			v.statusMessage = store.ReasonText(store.FailureReason(reason))
		}
	case store.StateRejected:
		v.meta = map[string]any{MetaFailure: FailureRejected}
		if v.statusMessage == "" {
			v.statusMessage = rejectedText
		}
	case store.StateCanceled:
		why, _ := md[store.MetaCanceled].(string)
		if why == "" {
			why = store.CanceledClient
		}
		v.meta = map[string]any{MetaCanceled: why}
	}
	return v
}

// mcpTask is an MCP 2025-11-25 Task.
type mcpTask struct {
	TaskID        string `json:"taskId"`
	Status        string `json:"status"`
	StatusMessage string `json:"statusMessage,omitempty"`
	CreatedAt     string `json:"createdAt"`
	LastUpdatedAt string `json:"lastUpdatedAt"`
	TTL           int64  `json:"ttl"`
	PollInterval  int    `json:"pollInterval"`
}

func taskObject(row *store.TaskRow, svc store.ServiceRef, v taskView) mcpTask {
	return mcpTask{TaskID: row.ID.String(), Status: v.status, StatusMessage: v.statusMessage, CreatedAt: mcpTime(row.CreatedAt),
		LastUpdatedAt: mcpTime(row.StatusAt), TTL: ttlOf(row, svc), PollInterval: store.MCPPollInterval}
}

// taskResult is tasks/get's (and tasks/cancel's) result: the Task with its _meta.
func taskResult(row *store.TaskRow, svc store.ServiceRef, rd Reader) (json.RawMessage, error) {
	v := view(row, rd)
	o := map[string]any{}
	b, err := json.Marshal(taskObject(row, svc, v))
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &o); err != nil {
		return nil, err
	}
	if len(v.meta) > 0 {
		o["_meta"] = v.meta
	}
	return json.Marshal(o)
}

// createTaskResult is the answer to a task-augmented hire (§11.5 step 2, §12.3): {"task": Task, "_meta"}.
func createTaskResult(row *store.TaskRow, svc store.ServiceRef, rd Reader) (json.RawMessage, error) {
	v := view(row, rd)
	o := map[string]any{"task": taskObject(row, svc, v)}
	if len(v.meta) > 0 {
		o["_meta"] = v.meta
	}
	return json.Marshal(o)
}

// relatedTask is the related-task _meta value of a task (§11.6).
func relatedTask(id uuid.UUID) map[string]any { return map[string]any{"taskId": id.String()} }
