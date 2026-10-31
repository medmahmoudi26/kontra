package blob

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
)

// PartSize is a part's size as spec/relay.md §10.4 measures it, on its decoded bytes: a text part's UTF-8
// bytes, a raw part's bytes (before base64; a sealed part's ciphertext), a url part's URL, a data part's JSON.
func PartSize(p *a2a.Part) int64 {
	if p == nil {
		return 0
	}
	switch c := p.Content.(type) {
	case a2a.Text:
		return int64(len(c))
	case a2a.Raw:
		return int64(len(c))
	case a2a.URL:
		return int64(len(c))
	case a2a.Data:
		b, _ := json.Marshal(c.Value)
		return int64(len(b))
	}
	return 0
}

// Content is the bytes a part's object holds and the media type its url part keeps: the part's own, or, where
// the part's kind said what the bytes are and the url part would not, text/plain for text, application/json
// for data and text/uri-list for a URL.
func Content(p *a2a.Part) ([]byte, string, error) {
	mt := p.MediaType
	switch c := p.Content.(type) {
	case a2a.Raw:
		return []byte(c), mt, nil
	case a2a.Text:
		if mt == "" {
			mt = "text/plain; charset=utf-8"
		}
		return []byte(c), mt, nil
	case a2a.Data:
		b, err := json.Marshal(c.Value)
		if err != nil {
			return nil, "", fmt.Errorf("blob: a data part: %w", err)
		}
		if mt == "" {
			mt = "application/json"
		}
		return b, mt, nil
	case a2a.URL:
		if mt == "" {
			mt = "text/uri-list"
		}
		return []byte(c), mt, nil
	}
	return nil, "", fmt.Errorf("blob: a part of content %T", p.Content)
}

// Externalize stores p's content as a new object of the task and returns the url part that replaces p in the
// log (kontra-blob:<ref>, with p's filename, media type and metadata, so a sealed part keeps its envelope type
// and kontra.sealed) and the object's row, which the caller adds to its Pending set. p is not modified.
func Externalize(ctx context.Context, s Store, tenant, task uuid.UUID, p *a2a.Part) (*a2a.Part, Row, error) {
	if s == nil {
		return nil, Row{}, fmt.Errorf("blob: no bucket")
	}
	b, mt, err := Content(p)
	if err != nil {
		return nil, Row{}, err
	}
	if len(b) == 0 {
		return nil, Row{}, fmt.Errorf("blob: an empty part is never stored in the bucket")
	}
	ref, err := NewRef()
	if err != nil {
		return nil, Row{}, err
	}
	row := Row{Ref: ref, Tenant: tenant, Task: task, Size: int64(len(b))}
	if err := s.Put(ctx, row.Key(), bytes.NewReader(b), row.Size); err != nil {
		return nil, Row{}, err
	}
	out := &a2a.Part{Content: a2a.URL(ref.URL()), Filename: p.Filename, MediaType: mt, Metadata: p.Metadata}
	return out, row, nil
}

type keepKey struct{}

// WithKeepRefs marks ctx as a request whose answers keep kontra-blob:<ref> (spec/relay.md §10.4: "to nodes"):
// one that arrived on a hiring node's stream (spec/tunnel.md §9; M3 WP-03). Every Renderer method passes what it
// is given unchanged under such a context.
func WithKeepRefs(ctx context.Context) context.Context {
	return context.WithValue(ctx, keepKey{}, true)
}

// ForCaller undoes WithKeepRefs: what is rendered on the returned context is a caller's, presigned GETs. The gate's
// own /mcp renders a hire task's parts so for every reader, a request that arrived on a hiring stream included: its
// answer is an MCP result, which a hiring node relays as it is (it inlines the parts of A2A answers only), so its
// client is a caller and the node fetches nothing from it (spec/relay.md §10.4, §12.2; D56 holds; D303).
func ForCaller(ctx context.Context) context.Context {
	return context.WithValue(ctx, keepKey{}, false)
}

// KeepRefs reports whether ctx is marked by WithKeepRefs.
func KeepRefs(ctx context.Context) bool { v, _ := ctx.Value(keepKey{}).(bool); return v }

// Renderer is egress rendering (spec/relay.md §10.4, D167, D181): the one function every path that hands parts
// out of the gate uses (a2a-go's answers through the relay's interceptors, the gate's SSE writer, push
// deliveries, the dispatch to a direct backend). A url part kontra-blob:<ref> becomes a presigned GET valid
// PresignTTL, for callers, push receivers and direct backends; it stays kontra-blob:<ref> for a node (a tunnel
// dispatch, which does not render, and a request that arrived on a hiring stream: WithKeepRefs).
//
// A ref is resolved only through the blobs table, and rendered only when its row names the task being
// rendered (D181). A ref without a row, or whose row names another task, is passed as kontra-blob:<ref> (a
// name nobody dials) and logged as an error. A nil Renderer, or one without a bucket, passes everything as it
// is.
type Renderer struct {
	store  Store
	lookup func(ctx context.Context, refs []Ref) (map[Ref]Row, error) // the blobs table
	ttl    time.Duration
	logger *slog.Logger
}

// NewRenderer returns the renderer over the bucket s (nil: none configured) and the blobs table in db.
func NewRenderer(s Store, db Querier, logger *slog.Logger) *Renderer {
	if logger == nil {
		logger = slog.Default()
	}
	r := &Renderer{store: s, ttl: PresignTTL, logger: logger}
	if db != nil {
		r.lookup = func(ctx context.Context, refs []Ref) (map[Ref]Row, error) { return Lookup(ctx, db, refs) }
	}
	return r
}

// active reports whether ctx's answers are rendered at all.
func (r *Renderer) active(ctx context.Context) bool {
	return r != nil && !KeepRefs(ctx)
}

// urls resolves refs for task: the presigned URL of each ref whose row names task.
func (r *Renderer) urls(ctx context.Context, task uuid.UUID, refs []Ref) (map[Ref]string, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	if r.store == nil || r.lookup == nil {
		r.logger.Error("blob: a part names a bucket object but no bucket is configured (blobs:); it is passed unrendered",
			"task", task, "refs", len(refs))
		return nil, nil
	}
	rows, err := r.lookup(ctx, refs)
	if err != nil {
		return nil, err
	}
	out := make(map[Ref]string, len(refs))
	for _, ref := range refs {
		row, ok := rows[ref]
		switch {
		case !ok:
			r.logger.Error("blob: a part names a ref without a blobs row; it is passed unrendered", "task", task, "ref", ref)
			continue
		case row.Task != task:
			r.logger.Error("blob: a part names another task's object; it is passed unrendered (D181)", "task", task, "ref", ref, "owner", row.Task)
			continue
		}
		u, err := r.store.Presign(ctx, row.Key(), r.ttl)
		if err != nil {
			return nil, err
		}
		out[ref] = u
	}
	return out, nil
}

// refsOf collects the gate's refs in parts.
func refsOf(dst []Ref, ps a2a.ContentParts) []Ref {
	for _, p := range ps {
		if p == nil {
			continue
		}
		if u, ok := p.Content.(a2a.URL); ok {
			if ref, ok := RefOf(string(u)); ok {
				dst = append(dst, ref)
			}
		}
	}
	return dst
}

func refsOfMessage(dst []Ref, m *a2a.Message) []Ref {
	if m == nil {
		return dst
	}
	return refsOf(dst, m.Parts)
}

func refsOfTask(t *a2a.Task) []Ref {
	if t == nil {
		return nil
	}
	refs := refsOfMessage(nil, t.Status.Message)
	for _, m := range t.History {
		refs = refsOfMessage(refs, m)
	}
	for _, a := range t.Artifacts {
		if a != nil {
			refs = refsOf(refs, a.Parts)
		}
	}
	return refs
}

// apply returns ps with each part naming a resolved ref replaced by a url part with its presigned URL (the
// part's other members kept), and whether anything changed; ps is not modified.
func apply(ps a2a.ContentParts, urls map[Ref]string) (a2a.ContentParts, bool) {
	var out a2a.ContentParts
	for i, p := range ps {
		if p == nil {
			continue
		}
		u, ok := p.Content.(a2a.URL)
		if !ok {
			continue
		}
		ref, ok := RefOf(string(u))
		if !ok {
			continue
		}
		signed, ok := urls[ref]
		if !ok {
			continue
		}
		if out == nil {
			out = append(a2a.ContentParts(nil), ps...)
		}
		cp := *p
		cp.Content = a2a.URL(signed)
		out[i] = &cp
	}
	if out == nil {
		return ps, false
	}
	return out, true
}

func applyMessage(m *a2a.Message, urls map[Ref]string) *a2a.Message {
	if m == nil {
		return nil
	}
	ps, changed := apply(m.Parts, urls)
	if !changed {
		return m
	}
	cp := *m
	cp.Parts = ps
	return &cp
}

func applyTask(t *a2a.Task, urls map[Ref]string) *a2a.Task {
	cp := *t
	cp.Status.Message = applyMessage(t.Status.Message, urls)
	if t.History != nil {
		cp.History = make([]*a2a.Message, len(t.History))
		for i, m := range t.History {
			cp.History[i] = applyMessage(m, urls)
		}
	}
	if t.Artifacts != nil {
		cp.Artifacts = make([]*a2a.Artifact, len(t.Artifacts))
		for i, a := range t.Artifacts {
			cp.Artifacts[i] = applyArtifact(a, urls)
		}
	}
	return &cp
}

func applyArtifact(a *a2a.Artifact, urls map[Ref]string) *a2a.Artifact {
	if a == nil {
		return nil
	}
	ps, changed := apply(a.Parts, urls)
	if !changed {
		return a
	}
	cp := *a
	cp.Parts = ps
	return &cp
}

// Parts renders the parts of a message or artifact of task.
func (r *Renderer) Parts(ctx context.Context, task uuid.UUID, ps a2a.ContentParts) (a2a.ContentParts, error) {
	if !r.active(ctx) {
		return ps, nil
	}
	urls, err := r.urls(ctx, task, refsOf(nil, ps))
	if err != nil || len(urls) == 0 {
		return ps, err
	}
	out, _ := apply(ps, urls)
	return out, nil
}

// Message renders a message of task.
func (r *Renderer) Message(ctx context.Context, task uuid.UUID, m *a2a.Message) (*a2a.Message, error) {
	if !r.active(ctx) || m == nil {
		return m, nil
	}
	urls, err := r.urls(ctx, task, refsOfMessage(nil, m))
	if err != nil || len(urls) == 0 {
		return m, err
	}
	return applyMessage(m, urls), nil
}

// Task renders a task: its status message, history and artifacts. t is not modified.
func (r *Renderer) Task(ctx context.Context, t *a2a.Task) (*a2a.Task, error) {
	if !r.active(ctx) || t == nil {
		return t, nil
	}
	refs := refsOfTask(t)
	if len(refs) == 0 {
		return t, nil
	}
	task, err := uuid.Parse(string(t.ID))
	if err != nil {
		return t, nil // not a gate task: none of its refs can be the task's
	}
	urls, err := r.urls(ctx, task, refs)
	if err != nil || len(urls) == 0 {
		return t, err
	}
	return applyTask(t, urls), nil
}

// Event renders an event of a task: a Task, a Message, a status update or an artifact update. ev is not
// modified; another type is returned as it is.
func (r *Renderer) Event(ctx context.Context, ev a2a.Event) (a2a.Event, error) {
	if !r.active(ctx) || ev == nil {
		return ev, nil
	}
	var refs []Ref
	switch e := ev.(type) {
	case *a2a.Task:
		return r.Task(ctx, e)
	case *a2a.Message:
		refs = refsOfMessage(nil, e)
	case *a2a.TaskStatusUpdateEvent:
		refs = refsOfMessage(nil, e.Status.Message)
	case *a2a.TaskArtifactUpdateEvent:
		if e.Artifact != nil {
			refs = refsOf(nil, e.Artifact.Parts)
		}
	default:
		return ev, nil
	}
	if len(refs) == 0 {
		return ev, nil
	}
	task, err := uuid.Parse(string(ev.TaskInfo().TaskID))
	if err != nil {
		return ev, nil
	}
	urls, err := r.urls(ctx, task, refs)
	if err != nil || len(urls) == 0 {
		return ev, err
	}
	switch e := ev.(type) {
	case *a2a.Message:
		return applyMessage(e, urls), nil
	case *a2a.TaskStatusUpdateEvent:
		cp := *e
		cp.Status.Message = applyMessage(e.Status.Message, urls)
		return &cp, nil
	case *a2a.TaskArtifactUpdateEvent:
		cp := *e
		cp.Artifact = applyArtifact(e.Artifact, urls)
		return &cp, nil
	}
	return ev, nil
}

// StreamResponse renders a stored StreamResponse of task (a task_events payload, sent by the SSE writer as
// bytes): a payload that names no ref is returned as it is, without being decoded; one that does is decoded,
// rendered and encoded again.
func (r *Renderer) StreamResponse(ctx context.Context, task uuid.UUID, raw []byte) ([]byte, error) {
	if !r.active(ctx) || !bytes.Contains(raw, []byte(URIScheme)) {
		return raw, nil
	}
	var sr a2a.StreamResponse
	if err := json.Unmarshal(raw, &sr); err != nil {
		return nil, fmt.Errorf("blob: a stored stream response: %w", err)
	}
	if sr.Event == nil || sr.Event.TaskInfo().TaskID != a2a.TaskID(task.String()) {
		return raw, nil
	}
	ev, err := r.Event(ctx, sr.Event)
	if err != nil {
		return nil, err
	}
	if ev == sr.Event {
		return raw, nil
	}
	return json.Marshal(a2a.StreamResponse{Event: ev})
}
