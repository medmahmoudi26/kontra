package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"github.com/google/uuid"
)

func TestStateMapping(t *testing.T) {
	for _, s := range []State{StateQuoted, StateSubmitted, StateWorking, StateAuthRequired, StateInputRequired,
		StateCompleted, StateFailed, StateCanceled, StateRejected} {
		if !s.Valid() {
			t.Errorf("%s not valid", s)
		}
		back, err := StateFromA2A(s.A2A())
		if err != nil {
			t.Fatalf("%s: %v", s, err)
		}
		want := s
		if s == StateQuoted {
			want = StateInputRequired // a quote shows as input-required on A2A (spec §5.1)
		}
		if back != want {
			t.Errorf("%s -> %s -> %s", s, s.A2A(), back)
		}
		if s.Terminal() != s.A2A().Terminal() {
			t.Errorf("%s: Terminal %v, A2A Terminal %v", s, s.Terminal(), s.A2A().Terminal())
		}
	}
	if _, err := StateFromA2A(a2a.TaskStateUnspecified); !errors.Is(err, a2a.ErrInvalidParams) {
		t.Errorf("unspecified state: %v", err)
	}
	if State("running").Valid() {
		t.Error("an unknown state is valid")
	}
}

func TestPageToken(t *testing.T) {
	at := time.Date(2026, 10, 25, 12, 0, 0, 123456999, time.UTC)
	id := uuid.Must(uuid.NewV7())
	tok := EncodePageToken(at, id)
	gotAt, gotID, err := DecodePageToken(tok)
	if err != nil {
		t.Fatal(err)
	}
	if !gotAt.Equal(at.Truncate(time.Microsecond)) || gotID != id {
		t.Errorf("round trip: %v %v, want %v %v", gotAt, gotID, at.Truncate(time.Microsecond), id)
	}
	long := base64.RawURLEncoding.EncodeToString(append([]byte{pageTokenVersion}, make([]byte, 30)...))
	v2 := base64.RawURLEncoding.EncodeToString(append([]byte{2}, make([]byte, 24)...))
	for _, bad := range []string{"", "!!!", "zzzz", long, v2, tok + "A", base64.URLEncoding.EncodeToString([]byte("2026-10-25T12:00:00Z_x"))} {
		if _, _, err := DecodePageToken(bad); !errors.Is(err, a2a.ErrInvalidParams) {
			t.Errorf("token %q: %v, want InvalidParams", bad, err)
		}
	}
}

func TestFailedStatusShape(t *testing.T) {
	info := a2a.TaskInfo{TaskID: "t1", ContextID: "c1"}
	ev := FailedStatus(info, ReasonTimeout, "")
	if ev.Status.State != a2a.TaskStateFailed || ev.TaskID != "t1" || ev.ContextID != "c1" || ev.Status.Timestamp == nil {
		t.Fatalf("event %+v", ev)
	}
	m := ev.Status.Message
	if m == nil || m.Role != a2a.MessageRoleAgent || len(m.Parts) != 1 || m.Parts[0].Text() != ReasonText(ReasonTimeout) {
		t.Fatalf("message %+v", m)
	}
	if m.TaskID != "t1" || m.ContextID != "c1" {
		t.Errorf("message ids %q %q", m.TaskID, m.ContextID)
	}
	if ev.Metadata[MetaFailure] != "timeout" || m.Metadata[MetaFailure] != "timeout" {
		t.Errorf("kontra.failure: event %v, message %v", ev.Metadata, m.Metadata)
	}
	if got := FailedStatus(info, ReasonUpstreamError, "boom").Status.Message.Parts[0].Text(); got != "boom" {
		t.Errorf("text %q", got)
	}
	c := CanceledStatus(info, CanceledDeclined, "")
	if c.Status.State != a2a.TaskStateCanceled || c.Metadata[MetaCanceled] != "declined" || c.Status.Message.Parts[0].Text() != "the quote was declined" {
		t.Errorf("canceled %+v", c)
	}
	for _, r := range []FailureReason{ReasonNoSession, ReasonTunnelLost, ReasonUpstreamUnreachable, ReasonUpstreamError,
		ReasonServiceUnavailable, ReasonTimeout, ReasonSealedRejected, ReasonLifecycleUnavailable} {
		if !r.Valid() || ReasonText(r) == "" {
			t.Errorf("reason %s: valid %v, text %q", r, r.Valid(), ReasonText(r))
		}
	}
	if FailureReason("oops").Valid() {
		t.Error("an unknown reason is valid")
	}
	// A reason outside spec §6.4's closed set (relay.Failed takes a string) is never written as
	// kontra.failure, and the one text part is never empty.
	for _, text := range []string{"", "boom"} {
		ev := FailedStatus(info, FailureReason("oops"), text)
		m := ev.Status.Message
		want := text
		if want == "" {
			want = ReasonText(ReasonUpstreamError)
		}
		if ev.Metadata[MetaFailure] != string(ReasonUpstreamError) || m.Metadata[MetaFailure] != string(ReasonUpstreamError) ||
			len(m.Parts) != 1 || m.Parts[0].Text() != want {
			t.Errorf("unknown reason, text %q: metadata %v, parts %+v", text, ev.Metadata, m.Parts)
		}
	}
	if c := CanceledStatus(info, "made_up", ""); c.Metadata[MetaCanceled] != nil || c.Status.Message.Parts[0].Text() != "the task was canceled" {
		t.Errorf("an unknown kontra.canceled value: %+v", c)
	}
	for _, st := range []a2a.TaskState{a2a.TaskStateCompleted, a2a.TaskStateRejected, a2a.TaskStateFailed, a2a.TaskStateCanceled} {
		if got := TerminalStatus(info, st, nil, "").Status.Message.Parts[0].Text(); got == "" {
			t.Errorf("TerminalStatus %s without a text has an empty text part", st)
		}
	}
}

func TestRequestHash(t *testing.T) {
	req := func() *a2a.SendMessageRequest {
		m := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi"))
		m.ID = "m1"
		return &a2a.SendMessageRequest{Message: m, Metadata: map[string]any{"b": 1, "a": "x"}}
	}
	h1, err := RequestHash(req())
	if err != nil {
		t.Fatal(err)
	}
	h2, _ := RequestHash(req())
	if h1 != h2 {
		t.Error("equal requests hash differently")
	}
	withPush := req()
	withPush.Config = &a2a.SendMessageConfig{PushConfig: &a2a.PushConfig{URL: "https://hook.example/a"}}
	h3, _ := RequestHash(withPush)
	if h3 == h1 {
		t.Error("an inline push config does not change the hash (D74: the whole request is hashed)")
	}
	other := req()
	other.Message.Parts = a2a.ContentParts{a2a.NewTextPart("hello")}
	if h4, _ := RequestHash(other); h4 == h1 {
		t.Error("a different message has the same hash")
	}
	if _, err := RequestHash(nil); err == nil {
		t.Error("a nil request hashed")
	}
}

func TestFenceContext(t *testing.T) {
	ctx := context.Background()
	if FenceFrom(ctx) != nil {
		t.Fatal("a fence from nothing")
	}
	f := &Fence{TaskID: uuid.New(), N: 1, DispatchID: uuid.New()}
	if FenceFrom(WithFence(ctx, f)) != f {
		t.Fatal("fence lost in the context")
	}
	if f.Fenced() {
		t.Fatal("fenced before any write")
	}
	f.markFenced()
	if !f.Fenced() {
		t.Fatal("markFenced")
	}
	var nilFence *Fence
	if nilFence.Fenced() {
		t.Fatal("a nil fence is fenced")
	}
	if !errors.Is(ErrFenced, taskstore.ErrConcurrentModification) {
		t.Error("ErrFenced does not wrap ErrConcurrentModification")
	}
}

func TestWriteValidate(t *testing.T) {
	ok := Write{TaskID: uuid.New(), PrevVersion: 1, Kind: KindStatus, State: StateWorking, Snapshot: &a2a.Task{}, Payload: []byte(`{}`)}
	if err := ok.validate(); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*Write){
		"prev 0":      func(w *Write) { w.PrevVersion = 0 },
		"kind":        func(w *Write) { w.Kind = "nope" },
		"state":       func(w *Write) { w.State = "nope" },
		"no snapshot": func(w *Write) { w.Snapshot = nil },
		"no body":     func(w *Write) { w.Payload = nil },
		"negative":    func(w *Write) { w.Timeout = -time.Second },
	} {
		w := ok
		mut(&w)
		err := w.validate()
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
		if name == "prev 0" && !errors.Is(err, taskstore.ErrConcurrentModification) {
			t.Errorf("prev 0: %v, want ErrConcurrentModification", err)
		}
	}
}

func row(t *testing.T, seq int64, kind EventKind, ev any) Event {
	t.Helper()
	var b []byte
	var err error
	if e, ok := ev.(a2a.Event); ok {
		b, err = json.Marshal(a2a.StreamResponse{Event: e})
	} else {
		b, err = json.Marshal(ev)
	}
	if err != nil {
		t.Fatal(err)
	}
	return Event{Seq: seq, Kind: kind, Payload: b}
}

// The fold past the terminal row (D75): message rows are appended, metadata rows merged; any other row is
// a corrupt log.
func TestFolderPastTerminal(t *testing.T) {
	info := a2a.TaskInfo{TaskID: "t1", ContextID: "c1"}
	first := a2a.NewMessageForTask(a2a.MessageRoleUser, info, a2a.NewTextPart("q"))
	var f Folder
	steps := []Event{
		row(t, 1, KindTask, &a2a.Task{ID: "t1", ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateSubmitted}, History: []*a2a.Message{first}}),
		row(t, 2, KindMetadata, metadataRow{Metadata: map[string]any{"a": "1", "b": "1"}}),
		row(t, 3, KindStatus, a2a.NewStatusUpdateEvent(info, a2a.TaskStateCompleted, nil)),
		row(t, 4, KindMessage, a2a.NewMessageForTask(a2a.MessageRoleUser, info, a2a.NewTextPart("review"))),
		row(t, 5, KindMetadata, metadataRow{Metadata: map[string]any{"b": "2"}}),
	}
	for _, ev := range steps {
		if err := f.Apply(ev); err != nil {
			t.Fatalf("seq %d: %v", ev.Seq, err)
		}
	}
	snap, err := f.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snap.Status.State != a2a.TaskStateCompleted || len(snap.History) != 2 || snap.History[1].Parts[0].Text() != "review" {
		t.Fatalf("snapshot %+v", snap)
	}
	if snap.Metadata["a"] != "1" || snap.Metadata["b"] != "2" {
		t.Errorf("metadata %v", snap.Metadata)
	}
	if err := f.Apply(row(t, 6, KindStatus, a2a.NewStatusUpdateEvent(info, a2a.TaskStateWorking, nil))); err == nil {
		t.Error("a status row after the terminal row folded")
	}
	if err := f.Apply(row(t, 9, KindMetadata, metadataRow{Metadata: map[string]any{}})); err == nil || !strings.Contains(err.Error(), "seq 9 after 5") {
		t.Errorf("a gap folded: %v", err)
	}
	var g Folder
	if err := g.Apply(row(t, 1, KindStatus, a2a.NewStatusUpdateEvent(info, a2a.TaskStateWorking, nil))); err == nil {
		t.Error("a log not starting with the task row folded")
	}
}
