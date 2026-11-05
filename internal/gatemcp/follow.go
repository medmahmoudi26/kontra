package gatemcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"

	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/blob"
	"kontra.sh/kontra/internal/lifecycle"
	"kontra.sh/kontra/internal/mcpwire"
	"kontra.sh/kontra/internal/store"
)

// The tools that follow, list, cancel and read a hire task for a client that sees only tools (spec §12.2; D332..D337):
// status, tasks, cancel and artifact, over the same hire tasks as the MCP Tasks of /mcp, with the same scoping (a hire
// task of the principal's buyer whose service this replica serves to the principal: HireTasks.load, D303), the same
// renderers and the same waker. The MCP Tasks stay as they are.

// Bounds of status's events (D332): at most StatusEvents rows and about StatusBytes of their payloads per call; more is
// true when a call was cut.
const (
	StatusEvents = 50
	StatusBytes  = 256 << 10
)

// TaskTextMax bounds an entry's text in the tasks tool (D333).
const TaskTextMax = 200

// UnknownTask is the one answer of the tools for a task id that is unknown, malformed, another buyer's, unhired, or of
// a service no longer served to the principal (§12.2, D303).
const UnknownTask = "unknown task"

// TaskEvent is one row of a hire task's log as status renders it (mcp-tools.json#/$defs/event): a task, status,
// artifact or message row, compactly: the state a task or status row sets, the role of a message, the text, data and
// file links of its parts, an artifact's id and name, and when it was logged.
type TaskEvent struct {
	Seq        int64     `json:"seq"`
	Kind       string    `json:"kind"`
	State      string    `json:"state,omitempty"`
	Role       string    `json:"role,omitempty"`
	Text       string    `json:"text,omitempty"`
	Data       []any     `json:"data,omitempty"`
	Files      []FileRef `json:"files,omitempty"`
	ArtifactID string    `json:"artifactId,omitempty"`
	Name       string    `json:"name,omitempty"`
	At         string    `json:"at"`
}

// StatusResult is status's structuredContent (mcp-tools.json#/$defs/statusResult): the hire result, the events after
// the cursor asked when after was given, and the cursor to ask from next.
type StatusResult struct {
	HireResult
	Events []TaskEvent `json:"events,omitempty"`
	Cursor int64       `json:"cursor"`
	More   bool        `json:"more,omitempty"`
}

// TaskEntry is one task of the tasks tool's answer.
type TaskEntry struct {
	TaskID    string `json:"taskId"`
	Service   string `json:"service"`
	ContextID string `json:"contextId"`
	State     string `json:"state"`
	StatusAt  string `json:"statusAt"`
	Text      string `json:"text,omitempty"`
}

// TasksResult is the tasks tool's structuredContent.
type TasksResult struct {
	Tasks      []TaskEntry `json:"tasks"`
	NextCursor string      `json:"nextCursor,omitempty"`
}

// ArtifactResult is the artifact tool's structuredContent.
type ArtifactResult struct {
	TaskID      string    `json:"taskId"`
	ArtifactID  string    `json:"artifactId"`
	Name        string    `json:"name,omitempty"`
	Description string    `json:"description,omitempty"`
	Text        string    `json:"text,omitempty"`
	Data        []any     `json:"data,omitempty"`
	Files       []FileRef `json:"files,omitempty"`
}

// loadTool is the principal's hire task id names for a tool (HireTasks.load's scoping); nil when it names none of
// them, which the caller answers UnknownTask.
func (h *Handler) loadTool(rq *request, id uuid.UUID) (*store.TaskRow, error) {
	row, _, err := h.deps.Tasks.load(rq.ctx(), rq.reader(), id)
	return row, err
}

// answerOf is the CallToolResult of a hire task as row has it, as a plain hire, reply, status and cancel answer it
// (§12.2): an ended task as tasks/result renders it; input-required as §12.4's reply-tool text; quoted as the approve
// URL; a task still running with the text saying how to follow it. Its structuredContent is the hire result, with the
// task's contextId and service ref (D336).
func (h *Handler) answerOf(ctx context.Context, row *store.TaskRow, p auth.Principal) callToolResult {
	var res callToolResult
	switch {
	case row.State.Terminal():
		task := row.Snapshot
		if h.deps.Render != nil {
			if t, err := h.deps.Render.Task(blob.ForCaller(ctx), task); err == nil {
				task = t
			}
		}
		res = render(task, row.State)
	case row.State == store.StateQuoted:
		res = quotedCTR(row)
	case row.State == store.StateInputRequired:
		_, text, _ := Question(row.Snapshot, Reader{Principal: p, Era: mcpwire.EraLegacy})
		res = callToolResult{Content: []content{{Type: "text", Text: text}},
			StructuredContent: HireResult{TaskID: row.ID.String(), State: string(row.State.A2A()), URL: threadURL(row.Snapshot), Text: text},
			Meta:              map[string]any{MetaInputRequired: true}}
	default:
		follow := strings.TrimRight(h.deps.PublicURL, "/") + "/agents/" + h.serviceRef(row)
		text := "The task " + row.ID.String() + " is still " + string(row.State) + ". Follow it here with status " +
			"{taskId: \"" + row.ID.String() + "\", after: 0, wait: 25}, or with GetTask at " + follow +
			" (the same credential); call reply when it asks a question."
		res = callToolResult{Content: []content{{Type: "text", Text: text}},
			StructuredContent: HireResult{TaskID: row.ID.String(), State: string(row.State.A2A()), URL: threadURL(row.Snapshot), Text: text}}
	}
	if hr, ok := res.StructuredContent.(HireResult); ok {
		hr.ContextID, hr.Service = row.ContextID, h.serviceRef(row)
		res.StructuredContent = hr
	}
	return res
}

// status answers the status tool (§12.2, D332): the hire result of the principal's hire task, its contextId and
// service, and when after is given its task, status, artifact and message rows with seq > after (at most StatusEvents
// rows or about StatusBytes, more when cut) and the cursor, the last seq read (else the log's tail). With wait, when the
// task has not ended and its log holds nothing after the cursor asked (or after its tail when after is absent), it
// first waits on the waker until a row lands or wait elapses, cut by the plain wait's budget (D295); with a
// progressToken each status and artifact row that lands meanwhile is a notifications/progress (D337).
func (h *Handler) status(rq *request, args json.RawMessage) {
	a, err := parseStatusArgs(args)
	if err != nil {
		h.toolError(rq, err)
		return
	}
	row, err := h.loadTool(rq, a.TaskID)
	switch {
	case err != nil:
		h.internal(rq, "read the task", err)
		return
	case row == nil:
		rq.result(errorResult(UnknownTask, nil))
		return
	}
	ctx := rq.ctx()
	base := a.After
	if base < 0 {
		if base, err = h.deps.Log.Tail(ctx, row.ID); err != nil {
			h.internal(rq, "read the task's log", err)
			return
		}
	}
	aw := newAnswerWriter(rq)
	if a.Wait > 0 && !row.State.Terminal() {
		wait := min(a.Wait, h.plainWait(rq))
		if !h.awaitRow(rq, aw, row.ID, base, wait) {
			return
		}
		if row, _, err = h.deps.Log.LoadHiredAsked(ctx, rq.p.BuyerID, row.ID); err != nil {
			aw.internal(h, "read the task", err)
			return
		}
	}
	res := h.answerOf(ctx, row, rq.p)
	sr := StatusResult{HireResult: res.StructuredContent.(HireResult)}
	if a.After >= 0 {
		evs, more, err := h.deps.Log.EventsWithin(ctx, row.ID, a.After, StatusEvents, StatusBytes)
		if err != nil {
			aw.internal(h, "read the task's log", err)
			return
		}
		sr.Cursor, sr.More = a.After, more
		for _, ev := range evs {
			sr.Cursor = ev.Seq
			if te, ok := h.taskEvent(ctx, row, ev); ok {
				sr.Events = append(sr.Events, te)
			}
		}
	} else if sr.Cursor, err = h.deps.Log.Tail(ctx, row.ID); err != nil {
		aw.internal(h, "read the task's log", err)
		return
	}
	res.StructuredContent = sr
	// A client that reads only content gets the cursor and the events as a text block of their own.
	tail := map[string]any{"cursor": sr.Cursor}
	if a.After >= 0 {
		tail["events"] = sr.Events
		if sr.More {
			tail["more"] = true
		}
	}
	b, _ := json.Marshal(tail)
	res.Content = append(res.Content, content{Type: "text", Text: string(b)})
	out, err := json.Marshal(res)
	if err != nil {
		aw.internal(h, "write the answer", err)
		return
	}
	aw.result(out)
}

// awaitRow waits up to wait for a row of the task's log with seq > base (status's wait): on the waker, reading the
// log's tail at each wake and every second, writing keep-alives and, with a progressToken, notifications/progress. It
// reports false when the client left or the answer was written (an error).
func (h *Handler) awaitRow(rq *request, aw *answerWriter, id uuid.UUID, base int64, wait time.Duration) bool {
	ctx := rq.ctx()
	wake, stop := h.deps.Waker.WaitTask(ctx, id)
	defer stop()
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	reread := time.NewTicker(time.Second)
	defer reread.Stop()
	keep := time.NewTicker(keepAliveEvery)
	defer keep.Stop()
	aw.begin()
	pr := aw.progress
	if pr != nil {
		pr.seen = base
	}
	for {
		tail, err := h.deps.Log.Tail(ctx, id)
		if err != nil {
			if ctx.Err() != nil {
				return false
			}
			aw.internal(h, "read the task's log", err)
			return false
		}
		if tail > base {
			h.notifyProgress(rq, aw, id)
			return ctx.Err() == nil
		}
		select {
		case <-ctx.Done():
			return false
		case <-deadline.C:
			return true
		case <-wake:
		case <-reread.C:
		case <-keep.C:
			aw.keepAlive()
		}
	}
}

// taskEvent renders one row of row's log for status (D332), its kontra-blob parts as presigned links for the caller
// (§10.4); ok false for a row status does not carry (metadata, and anything that does not decode).
func (h *Handler) taskEvent(ctx context.Context, row *store.TaskRow, ev store.Event) (TaskEvent, bool) {
	switch ev.Kind {
	case store.KindTask, store.KindStatus, store.KindArtifact, store.KindMessage:
	default:
		return TaskEvent{}, false
	}
	raw := []byte(ev.Payload)
	if h.deps.Render != nil {
		if b, err := h.deps.Render.StreamResponse(blob.ForCaller(ctx), row.ID, raw); err == nil {
			raw = b
		}
	}
	var sr a2a.StreamResponse
	if err := json.Unmarshal(raw, &sr); err != nil {
		return TaskEvent{}, false
	}
	te := TaskEvent{Seq: ev.Seq, Kind: string(ev.Kind), At: mcpTime(ev.CreatedAt)}
	n := 0
	switch v := sr.Event.(type) {
	case *a2a.Task:
		te.State = string(v.Status.State)
		if len(v.History) > 0 && v.History[len(v.History)-1] != nil {
			m := v.History[len(v.History)-1]
			te.Role = roleOf(m.Role)
			te.Text, te.Data, te.Files = partsOf(row.ID, m.Parts, &n)
		}
	case *a2a.TaskStatusUpdateEvent:
		te.State = string(v.Status.State)
		if v.Status.Message != nil {
			te.Role = roleOf(v.Status.Message.Role)
			te.Text, te.Data, te.Files = partsOf(row.ID, v.Status.Message.Parts, &n)
		}
	case *a2a.TaskArtifactUpdateEvent:
		if v.Artifact != nil {
			te.ArtifactID, te.Name = string(v.Artifact.ID), v.Artifact.Name
			te.Text, te.Data, te.Files = partsOf(row.ID, v.Artifact.Parts, &n)
		}
	case *a2a.Message:
		te.Role = roleOf(v.Role)
		te.Text, te.Data, te.Files = partsOf(row.ID, v.Parts, &n)
	default:
		return TaskEvent{}, false
	}
	return te, true
}

// roleOf is an A2A role as an event names it: user or agent.
func roleOf(r a2a.MessageRole) string {
	switch r {
	case a2a.MessageRoleUser:
		return "user"
	case a2a.MessageRoleAgent:
		return "agent"
	}
	return ""
}

// partsOf is what an event shows of parts: their text parts joined, their data parts' values, and their file parts as
// links (a URL part's URL; a raw part's kontra:task/… name, whose bytes the artifact tool gives).
func partsOf(task uuid.UUID, parts a2a.ContentParts, n *int) (string, []any, []FileRef) {
	var (
		text  strings.Builder
		data  []any
		files []FileRef
	)
	t := &a2a.Task{ID: a2a.TaskID(task.String())}
	for _, p := range parts {
		if p == nil {
			continue
		}
		*n++
		switch v := p.Content.(type) {
		case a2a.Text:
			text.WriteString(string(v))
		case a2a.Data:
			data = append(data, v.Value)
		case a2a.Raw:
			files = append(files, FileRef{URI: partURI(t, p, *n), Name: p.Filename, MediaType: p.MediaType})
		case a2a.URL:
			files = append(files, FileRef{URI: string(v), Name: p.Filename, MediaType: p.MediaType})
		}
	}
	return text.String(), data, files
}

// tasksTool answers the tasks tool (§12.2, D333): the principal's hire tasks, newest first by status_at, 50 a page,
// of one service, one contextId and the states the state argument selects, read through tasks_hired, at most 1000 rows
// per request (store.Log.ListHiredFiltered), less those whose service this replica does not serve to the principal
// (D303); nextCursor an opaque keyset position.
func (h *Handler) tasksTool(rq *request, args json.RawMessage) {
	a, err := parseTasksArgs(args)
	if err != nil {
		h.toolError(rq, err)
		return
	}
	ctx := rq.ctx()
	f := store.HiredFilter{ContextID: a.ContextID}
	if a.State != "" {
		f.States = statesOf(a.State)
	}
	if a.Service != "" {
		ref, err := ParseRef(a.Service, h.deps.Multi)
		if err != nil {
			rq.result(textResult(`{"tasks":[]}`, TasksResult{Tasks: []TaskEntry{}}, nil))
			return
		}
		s, err := h.deps.Catalog.lookup(ctx, ref)
		if err != nil {
			h.internal(rq, "look up the service", err)
			return
		}
		if s == nil {
			rq.result(textResult(`{"tasks":[]}`, TasksResult{Tasks: []TaskEntry{}}, nil))
			return
		}
		f.TenantID, f.Service = s.ref.TenantID, s.ref.Name
	}
	rows, next, err := h.deps.Log.ListHiredFiltered(ctx, rq.p.BuyerID, f, a.Cursor)
	switch {
	case errors.Is(err, store.ErrBadCursor):
		rq.result(errorResult("invalid arguments: cursor is not one the gate gave", nil))
		return
	case err != nil:
		h.internal(rq, "list the tasks", err)
		return
	}
	out := TasksResult{Tasks: make([]TaskEntry, 0, len(rows)), NextCursor: next}
	refs := map[string]string{}
	for _, r := range rows {
		if !admitsTask(ctx, h.deps.Served, rq.p, r) {
			continue
		}
		k := r.TenantID.String() + "/" + r.Service
		ref, ok := refs[k]
		if !ok {
			ref = h.serviceRef(r)
			refs[k] = ref
		}
		text := statusText(r.Snapshot)
		if r.State == store.StateQuoted {
			text = "waiting for a person to approve its quote at " + approveURL(r.Snapshot)
		}
		out.Tasks = append(out.Tasks, TaskEntry{TaskID: r.ID.String(), Service: ref, ContextID: r.ContextID,
			State: string(r.State.A2A()), StatusAt: mcpTime(r.StatusAt), Text: truncateRunes(text, TaskTextMax)})
	}
	b, _ := json.Marshal(out)
	rq.result(textResult(string(b), out, nil))
}

// truncateRunes is s cut to at most n characters.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}

// cancelTool answers the cancel tool (§12.2, D334): MCP tasks/cancel's legacy semantics (§11.6, D57: kontra.cancel in
// MCP mode, the task canceled within 10 s, the upstream receiving §8's CancelTask; a quoted task is canceled by the
// client), awaited, and the task after it as hire answers it (a canceled task's hire result, which is no error here:
// the cancel did what was asked). A task that has ended is isError "the task already ended (<state>)".
func (h *Handler) cancelTool(rq *request, args json.RawMessage) {
	id, err := parseTaskOnly(args)
	if err != nil {
		h.toolError(rq, err)
		return
	}
	row, err := h.loadTool(rq, id)
	switch {
	case err != nil:
		h.internal(rq, "read the task", err)
		return
	case row == nil:
		rq.result(errorResult(UnknownTask, nil))
		return
	case row.State.Terminal():
		rq.result(errorResult("the task already ended ("+string(row.State)+")", nil))
		return
	}
	ctx := rq.ctx()
	_, _, err = h.deps.Tasks.lc.CancelMCP(ctx, a2a.TaskID(row.ID.String()))
	switch {
	case err == nil, errors.Is(err, a2a.ErrTaskNotCancelable):
	case errors.Is(err, lifecycle.ErrUnavailable):
		h.refused(rq, "cancel the task", err)
		return
	default:
		h.internal(rq, "cancel the task", err)
		return
	}
	if row, err = h.deps.Log.Load(ctx, row.ID); err != nil {
		h.internal(rq, "read the task", err)
		return
	}
	if row.State.Terminal() && row.State != store.StateCanceled {
		rq.result(errorResult("the task already ended ("+string(row.State)+")", nil))
		return
	}
	res := h.answerOf(ctx, row, rq.p)
	if row.State == store.StateCanceled {
		res.IsError = false
	}
	b, err := json.Marshal(res)
	if err != nil {
		h.internal(rq, "write the answer", err)
		return
	}
	rq.result(b)
}

// artifactTool answers the artifact tool (§12.2, D335): one artifact of the principal's hire task, in full, as the
// hire result renders an artifact: text parts as text, data parts as JSON text and in data, file parts as resources (raw
// bytes embedded; a URL, kontra-blob parts presigned for the caller now, as a resource_link) and in files, each part
// bounded as §10.4 bounds it. An artifact id the task does not hold is isError "unknown artifact".
func (h *Handler) artifactTool(rq *request, args json.RawMessage) {
	a, err := parseArtifactArgs(args)
	if err != nil {
		h.toolError(rq, err)
		return
	}
	row, err := h.loadTool(rq, a.TaskID)
	switch {
	case err != nil:
		h.internal(rq, "read the task", err)
		return
	case row == nil:
		rq.result(errorResult(UnknownTask, nil))
		return
	}
	task := row.Snapshot
	var art *a2a.Artifact
	if task != nil {
		for _, x := range task.Artifacts {
			if x != nil && string(x.ID) == a.ArtifactID {
				art = x
				break
			}
		}
	}
	if art == nil {
		rq.result(errorResult("unknown artifact", nil))
		return
	}
	if h.deps.Render != nil {
		parts, err := h.deps.Render.Parts(blob.ForCaller(rq.ctx()), row.ID, art.Parts)
		if err != nil {
			h.internal(rq, "render the artifact's parts", err)
			return
		}
		cp := *art
		cp.Parts = parts
		art = &cp
	}
	out := ArtifactResult{TaskID: row.ID.String(), ArtifactID: string(art.ID), Name: art.Name, Description: art.Description}
	var blocks []content
	var texts []string
	n := 0
	for _, p := range art.Parts {
		if p == nil {
			continue
		}
		n++
		switch v := p.Content.(type) {
		case a2a.Text:
			blocks = append(blocks, content{Type: "text", Text: string(v)})
			texts = append(texts, string(v))
		case a2a.Data:
			js, err := json.Marshal(v.Value)
			if err != nil {
				js = []byte("null")
			}
			blocks = append(blocks, content{Type: "text", Text: string(js)})
			out.Data = append(out.Data, v.Value)
		case a2a.Raw:
			uri := partURI(task, p, n)
			blocks = append(blocks, content{Type: "resource", Resource: &resource{URI: uri, MIMEType: p.MediaType,
				Blob: base64.StdEncoding.EncodeToString(v)}})
			out.Files = append(out.Files, FileRef{URI: uri, Name: p.Filename, MediaType: p.MediaType})
		case a2a.URL:
			name := p.Filename
			if name == "" {
				name = string(v)
			}
			blocks = append(blocks, content{Type: "resource_link", URI: string(v), Name: name, MIMEType: p.MediaType})
			out.Files = append(out.Files, FileRef{URI: string(v), Name: p.Filename, MediaType: p.MediaType})
		}
	}
	out.Text = strings.Join(texts, "")
	if len(blocks) == 0 {
		blocks = []content{{Type: "text", Text: "the artifact " + strconv.Quote(string(art.ID)) + " has no parts"}}
	}
	b, err := json.Marshal(callToolResult{Content: blocks, StructuredContent: out})
	if err != nil {
		h.internal(rq, "write the answer", err)
		return
	}
	rq.result(b)
}

// stateWord is an A2A state as the gate's state words say it (working, completed …), for a progress message.
func stateWord(ts a2a.TaskState) string {
	if s, err := store.StateFromA2A(ts); err == nil {
		return string(s)
	}
	return string(ts)
}

// keyedNS is the namespace of the messageIds a hire's idempotencyKey mints (D338).
var keyedNS = uuid.NewSHA1(uuid.NameSpaceURL, []byte("https://kontra.sh/ns/v1/hire-idempotency-key"))

// KeyedMessageID is the A2A messageId a hire with idempotencyKey key mints for p (D338): a UUIDv5 of the principal's
// tenant, buyer and id and the key, each length-prefixed, the same for every hire of that key by that principal, so that
// the task the first one created is found by its first_message_id (store.Log.FindHireKeyed).
func KeyedMessageID(p auth.Principal, key string) uuid.UUID {
	var b strings.Builder
	for _, part := range []string{tenantOf(p).String(), p.BuyerID.String(), p.ID, key} {
		b.WriteString(strconv.Itoa(len(part)))
		b.WriteByte(':')
		b.WriteString(part)
		b.WriteByte(',')
	}
	return uuid.NewSHA1(keyedNS, []byte(b.String()))
}
