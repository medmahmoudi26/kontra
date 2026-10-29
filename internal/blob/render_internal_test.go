package blob

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
)

// fakeStore presigns as https://bucket.example/<object>?sig and records what it presigned.
type fakeStore struct {
	presigned []Key
	fail      error
}

func (f *fakeStore) Put(context.Context, Key, io.Reader, int64) error { return nil }
func (f *fakeStore) Open(context.Context, Key) (io.ReadCloser, int64, error) {
	return nil, 0, ErrNotFound
}
func (f *fakeStore) DeleteTask(context.Context, uuid.UUID, uuid.UUID) error { return nil }
func (f *fakeStore) Delete(context.Context, Key) error                      { return nil }
func (f *fakeStore) Presign(_ context.Context, k Key, ttl time.Duration) (string, error) {
	if f.fail != nil {
		return "", f.fail
	}
	if ttl != PresignTTL {
		return "", errors.New("unexpected ttl")
	}
	f.presigned = append(f.presigned, k)
	return "https://bucket.example/" + ObjectName("", k) + "?sig", nil
}

func newTestRenderer(s Store, rows ...Row) (*Renderer, *strings.Builder) {
	var logs strings.Builder
	r := NewRenderer(s, nil, slog.New(slog.NewTextHandler(&logs, nil)))
	r.lookup = func(_ context.Context, refs []Ref) (map[Ref]Row, error) {
		out := map[Ref]Row{}
		for _, ref := range refs {
			for _, row := range rows {
				if row.Ref == ref {
					out[ref] = row
				}
			}
		}
		return out, nil
	}
	return r, &logs
}

func blobPart(ref Ref) *a2a.Part {
	return &a2a.Part{Content: a2a.URL(ref.URL()), MediaType: "image/png", Filename: "x.png"}
}

// D181: a ref is rendered only for the task its blobs row names. A part naming another task's ref, or a ref
// without a row, stays kontra-blob:<ref> and is logged; the object is never presigned for it.
func TestRendererOnlyTheTasksOwnRefs(t *testing.T) {
	tenant := uuid.Must(uuid.NewV7())
	task, other := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	own, theirs, unknown := Ref("aaaaaaaaaaaaaaaaaaaaaaaaaa"), Ref("bbbbbbbbbbbbbbbbbbbbbbbbbb"), Ref("cccccccccccccccccccccccccc")
	fs := &fakeStore{}
	r, logs := newTestRenderer(fs, Row{Ref: own, Tenant: tenant, Task: task, Size: 1}, Row{Ref: theirs, Tenant: tenant, Task: other, Size: 1})
	ctx := context.Background()
	text := a2a.NewTextPart("hello")
	in := &a2a.Task{ID: a2a.TaskID(task.String()), ContextID: "c",
		Status:    a2a.TaskStatus{State: a2a.TaskStateCompleted, Message: &a2a.Message{ID: "s", Role: a2a.MessageRoleAgent, Parts: a2a.ContentParts{blobPart(own)}}},
		History:   []*a2a.Message{{ID: "m", Role: a2a.MessageRoleUser, Parts: a2a.ContentParts{text, blobPart(theirs), blobPart(unknown)}}},
		Artifacts: []*a2a.Artifact{{ID: "a", Parts: a2a.ContentParts{blobPart(own)}}},
	}
	before, _ := json.Marshal(in)
	out, err := r.Task(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(in)
	if string(before) != string(after) {
		t.Fatal("Task modified its input")
	}
	want := "https://bucket.example/" + tenant.String() + "/" + task.String() + "/" + string(own) + "?sig"
	if got := out.Status.Message.Parts[0]; got.Content != a2a.URL(want) || got.MediaType != "image/png" || got.Filename != "x.png" {
		t.Errorf("status part = %+v, want the presigned URL with its media type and filename", got)
	}
	if got := out.Artifacts[0].Parts[0].Content; got != a2a.URL(want) {
		t.Errorf("artifact part = %v", got)
	}
	h := out.History[0].Parts
	if h[0] != text || h[1].Content != a2a.URL(theirs.URL()) || h[2].Content != a2a.URL(unknown.URL()) {
		t.Errorf("history parts = %v %v %v: another task's ref and an unknown ref must stay unrendered", h[0].Content, h[1].Content, h[2].Content)
	}
	for _, k := range fs.presigned {
		if k.Ref != own || k.Task != task {
			t.Errorf("presigned %+v: only the task's own object may be presigned", k)
		}
	}
	if !strings.Contains(logs.String(), "another task") || !strings.Contains(logs.String(), "without a blobs row") {
		t.Errorf("logs %q: want both refusals logged", logs.String())
	}

	// The same through Event, Message, Parts and a stored stream response.
	ev, err := r.Event(ctx, &a2a.TaskArtifactUpdateEvent{TaskID: a2a.TaskID(other.String()), Artifact: &a2a.Artifact{ID: "a", Parts: a2a.ContentParts{blobPart(own), blobPart(theirs)}}})
	if err != nil {
		t.Fatal(err)
	}
	ap := ev.(*a2a.TaskArtifactUpdateEvent).Artifact.Parts
	if ap[0].Content != a2a.URL(own.URL()) || !strings.HasPrefix(string(ap[1].Content.(a2a.URL)), "https://bucket.example/"+tenant.String()+"/"+other.String()+"/") {
		t.Errorf("an event of the other task: %v %v", ap[0].Content, ap[1].Content)
	}
	raw, _ := json.Marshal(a2a.StreamResponse{Event: &a2a.TaskStatusUpdateEvent{TaskID: a2a.TaskID(task.String()), ContextID: "c",
		Status: a2a.TaskStatus{State: a2a.TaskStateWorking, Message: &a2a.Message{ID: "s", Role: a2a.MessageRoleAgent, Parts: a2a.ContentParts{blobPart(own), blobPart(theirs)}}}}})
	rendered, err := r.StreamResponse(ctx, task, raw)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rendered), want) || !strings.Contains(string(rendered), theirs.URL()) || strings.Contains(string(rendered), own.URL()) {
		t.Errorf("stream response %s", rendered)
	}
	plain := []byte(`{"statusUpdate":{"taskId":"x","status":{"state":"TASK_STATE_WORKING"}}}`)
	if got, err := r.StreamResponse(ctx, task, plain); err != nil || &got[0] != &plain[0] {
		t.Error("a payload without refs was not passed as it is")
	}
	// A stream response of another task than the one the stream serves is not rendered for it.
	if got, _ := r.StreamResponse(ctx, other, raw); string(got) != string(raw) {
		t.Error("a stream response was rendered for another task")
	}
	// A request that arrived on a hiring stream keeps refs (WP-03).
	if got, _ := r.Task(WithKeepRefs(ctx), in); got != in {
		t.Error("WithKeepRefs rendered")
	}
	// A nil renderer and one without a bucket pass everything.
	var nilR *Renderer
	if got, _ := nilR.Task(ctx, in); got != in {
		t.Error("a nil renderer rendered")
	}
	nb, _ := newTestRenderer(nil, Row{Ref: own, Tenant: tenant, Task: task, Size: 1})
	if got, _ := nb.Task(ctx, in); got.Status.Message.Parts[0].Content != a2a.URL(own.URL()) {
		t.Error("a renderer without a bucket rendered")
	}
	// A presign that fails is the caller's error.
	fs.fail = errors.New("boom")
	if _, err := r.Task(ctx, in); err == nil {
		t.Error("a failed presign was swallowed")
	}
}
