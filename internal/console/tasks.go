package console

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"kontra.sh/kontra/internal/blob"
	"kontra.sh/kontra/internal/store"
)

// Bounds of the list routes.
const (
	defaultLimit = 50
	maxLimit     = 100
)

// taskView is a task as the console shows it (Kontra Cloud's TaskView, without money): its metadata, what the console
// may do on it, and its snapshot (the task view only).
type taskView struct {
	TaskID         string          `json:"task_id"`
	Service        string          `json:"service"`
	Kind           string          `json:"kind"` // a2a | mcp (tasks.protocol)
	State          string          `json:"state"`
	Caller         string          `json:"caller"`    // the principal
	CallerID       string          `json:"caller_id"` // the buyer id: the conversation's scope (spec §4)
	Via            string          `json:"via"`       // "console" for a task the console started
	ContextID      string          `json:"context_id"`
	CreatedAt      time.Time       `json:"created_at"`
	StatusAt       time.Time       `json:"status_at"`
	TerminalAt     *time.Time      `json:"terminal_at"`
	RetentionUntil *time.Time      `json:"retention_until"`
	Retention      int64           `json:"retention"` // the service's, in seconds; 0: kept forever
	Version        int64           `json:"version"`   // the seq of its last event
	Sealed         bool            `json:"sealed"`
	Content        bool            `json:"content"` // the events carry readable parts (not sealed)
	Summary        *string         `json:"summary"` // the first words of its first message
	Console        bool            `json:"console"` // the console started it, and may act on it
	Actions        []string        `json:"actions"`
	Snapshot       json.RawMessage `json:"snapshot,omitempty"`
}

// taskColumns are what every task read selects: never a payload of another table, never first_request.
const taskColumns = `t.id, t.service, t.protocol, t.state, t.principal, t.buyer_id, t.via, t.context_id, t.created_at,
	t.status_at, t.terminal_at, t.retention_until, t.version, coalesce(s.sealed, 'off'), coalesce(s.retention_s, 0),
	ct.task_id is not null,
	case when coalesce(s.sealed, 'off') = 'off' and json_typeof(t.snapshot -> 'history' -> 0 -> 'parts') = 'array' then
		(select left(p ->> 'text', ` + "160" + `) from json_array_elements(t.snapshot -> 'history' -> 0 -> 'parts') p
		  where p ->> 'text' is not null limit 1) end`

const taskFrom = ` from kontra.tasks t
	left join kontra.services s on s.tenant_id = t.tenant_id and s.name = t.service
	left join kontra.console_tasks ct on ct.task_id = t.id`

func scanTask(row pgx.Row, extra ...any) (*taskView, error) {
	var (
		v      taskView
		id     uuid.UUID
		buyer  uuid.UUID
		sealed string
	)
	dst := append([]any{&id, &v.Service, &v.Kind, &v.State, &v.Caller, &buyer, &v.Via, &v.ContextID, &v.CreatedAt,
		&v.StatusAt, &v.TerminalAt, &v.RetentionUntil, &v.Version, &sealed, &v.Retention, &v.Console, &v.Summary}, extra...)
	if err := row.Scan(dst...); err != nil {
		return nil, err
	}
	v.TaskID, v.CallerID = id.String(), buyer.String()
	v.Sealed = sealed != "off"
	v.Content = !v.Sealed
	if v.Console {
		v.Via = "console"
	}
	v.Actions = actionsOf(&v)
	return &v, nil
}

// terminalStates are A2A's terminal states (spec §5.1).
var terminalStates = map[string]bool{"completed": true, "failed": true, "canceled": true, "rejected": true}

// actionsOf is what the console may do on a task (design §5): only on an A2A task it started, of a service that is not
// sealed: reply while it waits at input-required, cancel while it has not ended, continue once it has.
func actionsOf(v *taskView) []string {
	out := []string{}
	if !v.Console || v.Kind != "a2a" || v.Sealed {
		return out
	}
	switch {
	case v.State == "input-required":
		out = append(out, "reply", "cancel")
	case terminalStates[v.State]:
		out = append(out, "continue")
	default:
		out = append(out, "cancel")
	}
	return out
}

func limitOf(r *http.Request) (int, error) {
	s := r.URL.Query().Get("limit")
	if s == "" {
		return defaultLimit, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > maxLimit {
		return 0, fail(http.StatusUnprocessableEntity, "invalid_request", fmt.Sprintf("limit: 1 to %d", maxLimit))
	}
	return n, nil
}

// ---------------------------------------------------------------- GET /v1/tasks

func (c *Console) tasks(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := readCtx(r)
	defer cancel()
	q := r.URL.Query()
	limit, err := limitOf(r)
	if err != nil {
		c.answer(w, r, nil, err)
		return
	}
	var before *uuid.UUID
	if s := q.Get("before"); s != "" {
		id, err := uuid.Parse(s)
		if err != nil {
			c.answer(w, r, nil, fail(http.StatusUnprocessableEntity, "invalid_request", "before: a task id"))
			return
		}
		before = &id
	}
	kind := q.Get("kind")
	if kind != "" && kind != "a2a" && kind != "mcp" {
		c.answer(w, r, nil, fail(http.StatusUnprocessableEntity, "invalid_request", "kind: a2a or mcp"))
		return
	}
	rows, err := c.d.DB.Query(ctx, `select `+taskColumns+taskFrom+`
		where ($1::text = '' or t.service = $1) and ($2::text = '' or t.state = $2) and ($3::text = '' or t.protocol = $3)
		  and ($4::uuid is null or t.id < $4)
		order by t.id desc limit $5`, q.Get("service"), q.Get("state"), kind, before, limit+1)
	if err != nil {
		c.answer(w, r, nil, err)
		return
	}
	items, err := collectTasks(rows)
	if err != nil {
		c.answer(w, r, nil, err)
		return
	}
	var next *string
	if len(items) > limit {
		items = items[:limit]
		n := items[limit-1].TaskID
		next = &n
	}
	c.answer(w, r, map[string]any{"items": items, "next": next}, nil)
}

func collectTasks(rows pgx.Rows) ([]*taskView, error) {
	defer rows.Close()
	items := []*taskView{}
	for rows.Next() {
		v, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, v)
	}
	return items, rows.Err()
}

// ---------------------------------------------------------------- GET /v1/tasks/{task_id}

func taskIDOf(r *http.Request) (uuid.UUID, error) {
	id, err := uuid.Parse(r.PathValue("task_id"))
	if err != nil {
		return uuid.Nil, errNotFound
	}
	return id, nil
}

// loadTask reads one task's view, with its snapshot when snapshot is true; errNotFound when there is none.
func (c *Console) loadTask(ctx context.Context, id uuid.UUID, snapshot bool) (*taskView, error) {
	var snap []byte
	cols := taskColumns
	var extra []any
	if snapshot {
		cols += `, t.snapshot::text`
		extra = append(extra, &snap)
	}
	v, err := scanTask(c.d.DB.QueryRow(ctx, `select `+cols+taskFrom+` where t.id = $1`, id), extra...)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	if snapshot && len(snap) > 0 {
		v.Snapshot = json.RawMessage(snap)
	}
	return v, nil
}

func (c *Console) task(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := readCtx(r)
	defer cancel()
	id, err := taskIDOf(r)
	if err == nil {
		var v *taskView
		v, err = c.loadTask(ctx, id, true)
		if err == nil {
			c.answer(w, r, v, nil)
			return
		}
	}
	c.answer(w, r, nil, err)
}

// ---------------------------------------------------------------- GET /v1/tasks/{task_id}/events

// Event stream timing (Kontra Cloud's console stream): a stream lasts at most streamFor, then ends for the reader to
// reconnect with Last-Event-ID; a keep-alive comment every keepAlive; the log is read again at least every pollEvery
// (the LISTEN wake is the fast path).
const (
	streamFor   = 5 * time.Minute
	keepAlive   = 15 * time.Second
	pollEvery   = 2 * time.Second
	eventBatch  = 100
	eventBytes  = 1 << 20
	writeBudget = 30 * time.Second
)

// afterOf is where a stream starts: Last-Event-ID (an EventSource's reconnect), else ?after, else 0.
func afterOf(r *http.Request) (int64, error) {
	s := r.Header.Get("Last-Event-ID")
	if s == "" {
		s = r.URL.Query().Get("after")
	}
	if s == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, fail(http.StatusUnprocessableEntity, "invalid_request", "Last-Event-ID or after: an event's seq")
	}
	return n, nil
}

// eventFrame is one SSE event: id its seq, event its kind, data {seq, kind, at, stored_apart, origin, caller_type,
// validation, payload} with the stored payload as it is (on one line): where it came from, the A2A object the caller
// received for it, and whether it is valid A2A 1.0 (D357, D359, D360).
func eventFrame(ev store.Event) []byte {
	payload := []byte(ev.Payload)
	if bytes.ContainsAny(payload, "\r\n") {
		var b bytes.Buffer
		if json.Compact(&b, payload) == nil {
			payload = b.Bytes()
		}
	}
	it := newEventItem(ev)
	head, _ := json.Marshal(map[string]any{"seq": ev.Seq, "kind": string(ev.Kind), "at": ev.CreatedAt.UTC().Format(time.RFC3339Nano),
		"stored_apart": ev.PayloadRef != "", "task_id": it.TaskID, "origin": it.Origin, "caller_type": it.CallerType,
		"validation": it.Validation})
	var data []byte
	if len(payload) > 0 {
		data = append(append(append(head[:len(head)-1:len(head)-1], `,"payload":`...), payload...), '}')
	} else {
		data = head
	}
	return []byte(fmt.Sprintf("id: %d\nevent: %s\ndata: %s\n\n", ev.Seq, ev.Kind, data))
}

func (c *Console) taskEvents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := taskIDOf(r)
	if err != nil {
		c.answer(w, r, nil, err)
		return
	}
	after, err := afterOf(r)
	if err != nil {
		c.answer(w, r, nil, err)
		return
	}
	row, err := c.d.Log.Load(ctx, id)
	if errors.Is(err, a2a.ErrTaskNotFound) || (err == nil && row == nil) {
		c.answer(w, r, nil, errNotFound)
		return
	}
	if err != nil {
		c.answer(w, r, nil, err)
		return
	}
	var wake <-chan struct{}
	if c.d.Waker != nil {
		ch, release := c.d.Waker.WaitTask(ctx, id)
		defer release()
		wake = ch
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	write := func(b []byte) bool {
		_ = rc.SetWriteDeadline(time.Now().Add(writeBudget))
		if _, err := w.Write(b); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	if !write([]byte("retry: 3000\n\n")) {
		return
	}
	last, started, quiet := after, time.Now(), time.Now()
	for {
		evs, more, err := c.d.Log.EventsWithin(ctx, id, last, eventBatch, eventBytes)
		if err != nil {
			if ctx.Err() == nil {
				c.d.Logger.Warn("console: events stream read failed", "task_id", id.String(), "error", err)
			}
			return
		}
		for _, ev := range evs {
			if !write(eventFrame(ev)) {
				return
			}
			last = ev.Seq
		}
		if len(evs) > 0 {
			quiet = time.Now()
		}
		if more {
			continue
		}
		row, err := c.d.Log.Load(ctx, id)
		if err != nil || row == nil {
			return // deleted at its retention: a reconnect is 404
		}
		if row.TerminalAt != nil && last >= row.Version {
			end, _ := json.Marshal(map[string]any{"state": string(row.State), "version": row.Version})
			write([]byte("event: end\ndata: " + string(end) + "\n\n"))
			return
		}
		if time.Since(started) >= streamFor {
			return
		}
		if time.Since(quiet) >= keepAlive {
			if !write([]byte(": keep-alive\n\n")) {
				return
			}
			quiet = time.Now()
		}
		t := time.NewTimer(pollEvery)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-wake:
		case <-t.C:
		}
		t.Stop()
	}
}

// ---------------------------------------------------------------- GET /v1/tasks/{task_id}/files/{ref}

// fileMax bounds a file the console streams back.
const fileMax = 64 << 20

func (c *Console) taskFile(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := taskIDOf(r)
	if err != nil {
		c.answer(w, r, nil, err)
		return
	}
	notFound := fail(http.StatusNotFound, "file_not_found", "no such file of this task")
	ref := blob.Ref(strings.TrimPrefix(r.PathValue("ref"), blob.URIScheme))
	if !ref.Valid() || c.d.Blobs == nil {
		c.answer(w, r, nil, notFound)
		return
	}
	// Only a part this task's events name, stored for this task: a ref of another task is 404.
	got, err := blob.Lookup(ctx, c.d.DB, []blob.Ref{ref})
	if err != nil {
		c.answer(w, r, nil, err)
		return
	}
	row, ok := got[ref]
	if !ok || row.Task != id {
		c.answer(w, r, nil, notFound)
		return
	}
	body, size, err := c.d.Blobs.Open(ctx, row.Key())
	if errors.Is(err, blob.ErrNotFound) {
		c.answer(w, r, nil, notFound)
		return
	}
	if err != nil {
		c.answer(w, r, nil, err)
		return
	}
	defer body.Close()
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Disposition", `attachment; filename="`+string(ref)+`"`)
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	if size >= 0 {
		h.Set("Content-Length", strconv.FormatInt(min(size, fileMax), 10))
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.CopyN(w, body, fileMax)
}
