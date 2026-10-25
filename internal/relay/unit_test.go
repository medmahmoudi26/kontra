package relay

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/medmahmoudi26/kontra/internal/hooks"
	"github.com/medmahmoudi26/kontra/internal/lifecycle"
	"github.com/medmahmoudi26/kontra/internal/store"
)

const (
	upTask = a2a.TaskID("upstream-task-1")
	upCtx  = "upstream-ctx-1"
)

var gateIDs = IDs{
	GateTaskID: "0192f0b4-0000-7000-8000-0000000000aa", GateContextID: "gate-ctx",
	UpstreamTaskID: upTask, UpstreamContextID: upCtx,
}

func upInfo() a2a.TaskInfo { return a2a.TaskInfo{TaskID: upTask, ContextID: upCtx} }

// spoofed is metadata a seller writes to look like the gate or the operator.
func spoofed() map[string]any {
	return map[string]any{
		"kontra.approve.url": "https://evil.example/approve", "kontra.escrow.state": "released",
		"Kontra.Failure": "none", "seller": "kept",
	}
}

func noUpstream(t *testing.T, ev a2a.Event) {
	t.Helper()
	b, err := json.Marshal(a2a.StreamResponse{Event: ev})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{string(upTask), upCtx, "upstream-other", "kontra.approve", "kontra.escrow", "Kontra.Failure"} {
		if strings.Contains(string(b), s) {
			t.Errorf("%q survived the rewrite: %s", s, b)
		}
	}
}

func TestToGateDeep(t *testing.T) {
	statusMsg := a2a.NewMessageForTask(a2a.MessageRoleAgent, upInfo(), a2a.NewTextPart("q"))
	statusMsg.Metadata = spoofed()
	statusMsg.ReferenceTasks = []a2a.TaskID{upTask, "upstream-other"}
	part := a2a.NewTextPart("p")
	part.Metadata = spoofed()
	sealed := a2a.NewRawPart([]byte{1, 2})
	sealed.MediaType = hooks.SealedMediaType
	sealed.Metadata = map[string]any{hooks.KeySealed: map[string]any{"original": "text/plain"}, "kontra.approve.url": "x"}
	task := &a2a.Task{
		ID: upTask, ContextID: upCtx, Metadata: spoofed(),
		Status: a2a.TaskStatus{State: a2a.TaskStateWorking, Message: statusMsg},
		History: []*a2a.Message{
			a2a.NewMessageForTask(a2a.MessageRoleUser, upInfo(), a2a.NewTextPart("h1")),
			{ID: "m2", Role: a2a.MessageRoleAgent, TaskID: "upstream-other", ContextID: "upstream-other", Parts: a2a.ContentParts{a2a.NewTextPart("h2")}},
		},
		Artifacts: []*a2a.Artifact{{ID: "a1", Metadata: spoofed(), Parts: a2a.ContentParts{part, sealed}}},
	}
	before, _ := json.Marshal(task)
	got, err := gateIDs.ToGate(task)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(task)
	if string(before) != string(after) {
		t.Error("ToGate modified the upstream event")
	}
	g := got.(*a2a.Task)
	if g.ID != gateIDs.GateTaskID || g.ContextID != "gate-ctx" {
		t.Errorf("ids %s %s", g.ID, g.ContextID)
	}
	for _, m := range append(g.History, g.Status.Message) {
		if m.TaskID != gateIDs.GateTaskID || m.ContextID != "gate-ctx" {
			t.Errorf("nested message ids %s %s", m.TaskID, m.ContextID)
		}
	}
	if !reflect.DeepEqual(g.Status.Message.ReferenceTasks, []a2a.TaskID{gateIDs.GateTaskID}) {
		t.Errorf("references %v: the attempt's own maps to the gate task, another upstream task is dropped", g.Status.Message.ReferenceTasks)
	}
	if g.Metadata["seller"] != "kept" || g.Artifacts[0].Parts[0].Metadata["seller"] != "kept" {
		t.Error("seller metadata was lost")
	}
	if sm := g.Artifacts[0].Parts[1].Metadata; sm[hooks.KeySealed] == nil || sm["kontra.approve.url"] != nil {
		t.Errorf("sealed part metadata %v: kontra.sealed stays on a sealed envelope part, nothing else", sm)
	}
	// kontra.sealed on a part that is not the envelope type is the seller's spoof: stripped.
	plain := a2a.NewTextPart("x")
	plain.Metadata = map[string]any{hooks.KeySealed: "spoof"}
	ev, _ := gateIDs.ToGate(&a2a.TaskArtifactUpdateEvent{TaskID: upTask, ContextID: upCtx, Artifact: &a2a.Artifact{ID: "a", Parts: a2a.ContentParts{plain}}})
	if ev.(*a2a.TaskArtifactUpdateEvent).Artifact.Parts[0].Metadata != nil {
		t.Error("kontra.sealed kept on a plain part")
	}
	noUpstream(t, got)

	st := a2a.NewStatusUpdateEvent(upInfo(), a2a.TaskStateCompleted, statusMsg)
	st.Metadata = spoofed()
	got, err = gateIDs.ToGate(st)
	if err != nil {
		t.Fatal(err)
	}
	noUpstream(t, got)

	// An event naming another upstream task is refused, never rewritten.
	other := a2a.NewStatusUpdateEvent(a2a.TaskInfo{TaskID: "upstream-other", ContextID: upCtx}, a2a.TaskStateWorking, nil)
	if _, err := gateIDs.ToGate(other); !errors.Is(err, ErrForeignTask) {
		t.Errorf("foreign task: %v", err)
	}
	if _, err := gateIDs.ToGate(&a2a.TaskStatusUpdateEvent{}); !errors.Is(err, ErrForeignTask) {
		t.Errorf("no task id: %v", err)
	}
}

func TestToUpstream(t *testing.T) {
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi"))
	msg.TaskID, msg.ContextID = gateIDs.GateTaskID, "gate-ctx"
	msg.ReferenceTasks = []a2a.TaskID{"gate-ref-1", "gate-ref-2"}
	got, err := gateIDs.ToUpstream(msg, map[a2a.TaskID]a2a.TaskID{"gate-ref-1": "up-ref-1"})
	if err != nil {
		t.Fatal(err)
	}
	if got.TaskID != upTask || got.ContextID != upCtx || got.ID != msg.ID || !reflect.DeepEqual(got.ReferenceTasks, []a2a.TaskID{"up-ref-1"}) {
		t.Errorf("follow-up %+v", got)
	}
	if msg.TaskID != gateIDs.GateTaskID {
		t.Error("ToUpstream modified the caller's message")
	}
	first := IDs{GateTaskID: "g", GateContextID: "caller-ctx"}
	got, _ = first.ToUpstream(msg, nil)
	if got.TaskID != "" || got.ContextID != "" || got.ReferenceTasks != nil {
		t.Errorf("a first message carries no ids: %+v", got)
	}
}

func proj(state a2a.TaskState, artifacts ...*a2a.Artifact) *a2a.Task {
	return &a2a.Task{ID: gateIDs.GateTaskID, ContextID: "gate-ctx", Status: a2a.TaskStatus{State: state}, Artifacts: artifacts}
}

func gateInfo() a2a.TaskInfo { return a2a.TaskInfo{TaskID: gateIDs.GateTaskID, ContextID: "gate-ctx"} }

func TestTranslate(t *testing.T) {
	art := func(id a2a.ArtifactID, texts ...string) *a2a.Artifact {
		a := &a2a.Artifact{ID: id}
		for _, s := range texts {
			a.Parts = append(a.Parts, a2a.NewTextPart(s))
		}
		return a
	}
	ts := time.Date(2026, 10, 25, 1, 2, 3, 0, time.UTC)
	// First Task of an attempt: artifacts, then the status.
	up := &a2a.Task{ID: gateIDs.GateTaskID, ContextID: "gate-ctx", Status: a2a.TaskStatus{State: a2a.TaskStateCompleted, Timestamp: &ts},
		Artifacts: []*a2a.Artifact{art("a1", "x"), art("empty")}, Metadata: map[string]any{"seller": 1}}
	evs, err := Translate(up, proj(a2a.TaskStateSubmitted), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 {
		t.Fatalf("events %d", len(evs))
	}
	if a, ok := evs[0].(*a2a.TaskArtifactUpdateEvent); !ok || a.Artifact.ID != "a1" || a.Append {
		t.Errorf("first %+v", evs[0])
	}
	st := evs[1].(*a2a.TaskStatusUpdateEvent)
	if st.Status.State != a2a.TaskStateCompleted || !st.Status.Timestamp.Equal(ts) || st.Metadata["seller"] != 1 {
		t.Errorf("status %+v", st)
	}
	if st.Status.Message == nil || !strings.Contains(st.Status.Message.Parts[0].Text(), "completed") || st.Status.Message.Role != a2a.MessageRoleAgent {
		t.Errorf("a terminal status without a message gets the gate's: %+v", st.Status.Message)
	}

	// A later Task logs only the difference from the projection.
	p := proj(a2a.TaskStateWorking, art("a1", "x"))
	later := &a2a.Task{ID: gateIDs.GateTaskID, ContextID: "gate-ctx", Status: a2a.TaskStatus{State: a2a.TaskStateWorking},
		Artifacts: []*a2a.Artifact{art("a1", "x", "y"), art("a2", "z")}}
	evs, err = Translate(later, p, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 {
		t.Fatalf("diff events %d: %+v", len(evs), evs)
	}
	if a := evs[0].(*a2a.TaskArtifactUpdateEvent); a.Artifact.ID != "a1" || !a.Append || len(a.Artifact.Parts) != 1 || a.Artifact.Parts[0].Text() != "y" {
		t.Errorf("grown artifact %+v", a)
	}
	if a := evs[1].(*a2a.TaskArtifactUpdateEvent); a.Artifact.ID != "a2" || a.Append {
		t.Errorf("new artifact %+v", a)
	}
	// Nothing changed: nothing logged.
	if evs, _ := Translate(&a2a.Task{ID: p.ID, ContextID: p.ContextID, Status: p.Status, Artifacts: p.Artifacts}, p, false); len(evs) != 0 {
		t.Errorf("an unchanged task logged %d events", len(evs))
	}

	// A first append chunk for an unseen artifact is logged append: false; an empty artifact is dropped.
	chunk := &a2a.TaskArtifactUpdateEvent{TaskID: gateIDs.GateTaskID, ContextID: "gate-ctx", Append: true, Artifact: art("new", "c1")}
	evs, _ = Translate(chunk, proj(a2a.TaskStateWorking), false)
	if len(evs) != 1 || evs[0].(*a2a.TaskArtifactUpdateEvent).Append || !chunk.Append {
		t.Errorf("first chunk %+v (input append %v)", evs, chunk.Append)
	}
	evs, _ = Translate(chunk, proj(a2a.TaskStateWorking, art("new", "c0")), false)
	if !evs[0].(*a2a.TaskArtifactUpdateEvent).Append {
		t.Error("a later chunk lost its append")
	}
	if evs, _ := Translate(&a2a.TaskArtifactUpdateEvent{TaskID: gateIDs.GateTaskID, Artifact: art("e")}, proj(a2a.TaskStateWorking), false); len(evs) != 0 {
		t.Error("an empty artifact was logged")
	}

	// A Message becomes a completed status carrying it (D5).
	reply := a2a.NewMessageForTask(a2a.MessageRoleAgent, gateInfo(), a2a.NewTextPart("hello"))
	evs, _ = Translate(reply, proj(a2a.TaskStateSubmitted), true)
	if st := evs[0].(*a2a.TaskStatusUpdateEvent); st.Status.State != a2a.TaskStateCompleted || st.Status.Message.Parts[0].Text() != "hello" || st.Status.Message.TaskID != gateIDs.GateTaskID {
		t.Errorf("message reply %+v", st)
	}

	// failed and rejected pass as they are, with a message when they had none.
	for _, s := range []a2a.TaskState{a2a.TaskStateFailed, a2a.TaskStateRejected, a2a.TaskStateCanceled} {
		evs, _ = Translate(a2a.NewStatusUpdateEvent(gateInfo(), s, nil), proj(a2a.TaskStateWorking), false)
		name, _ := store.StateFromA2A(s)
		if st := evs[0].(*a2a.TaskStatusUpdateEvent); st.Status.State != s || st.Status.Message == nil || !strings.HasSuffix(st.Status.Message.Parts[0].Text(), ": "+string(name)) {
			t.Errorf("%s: %+v", s, st)
		}
	}
	if _, err := Translate(&a2a.TaskStatusUpdateEvent{TaskID: gateIDs.GateTaskID, Status: a2a.TaskStatus{State: "TASK_STATE_BOGUS"}}, proj(a2a.TaskStateWorking), false); !errors.Is(err, ErrMalformed) {
		t.Errorf("unknown state: %v", err)
	}
}

func TestFailedShape(t *testing.T) {
	ev := Failed(gateInfo(), "upstream_error", "the service returned an error: boom")
	if ev.Status.State != a2a.TaskStateFailed || ev.Metadata[store.MetaFailure] != "upstream_error" ||
		ev.Status.Message.Metadata[store.MetaFailure] != "upstream_error" || ev.Status.Message.Parts[0].Text() != "the service returned an error: boom" {
		t.Errorf("%+v", ev)
	}
	if Failed(gateInfo(), "made_up", "").Metadata[store.MetaFailure] != "upstream_error" {
		t.Error("a reason outside the closed set is upstream_error")
	}
}

func TestRegistry(t *testing.T) {
	r := NewRegistry()
	k := Key{TaskID: "t1", N: 1}
	ctx1, cancel1 := context.WithCancelCause(context.Background())
	d1, err := r.Register(k, cancel1)
	if err != nil {
		t.Fatal(err)
	}
	synth := a2a.NewStatusUpdateEvent(a2a.TaskInfo{TaskID: "t1"}, a2a.TaskStateFailed, nil)
	mine := Failed(a2a.TaskInfo{TaskID: "t1"}, "upstream_error", "x")
	d1.RecordTerminal(mine)
	if r.AllowFailed("t1", synth) || !r.AllowFailed("t1", mine) || !r.AllowFailed("other", synth) {
		t.Error("guard: a2a-go's failure refused, the relay's allowed, an unregistered task's allowed")
	}
	// The same pointer only: an equal copy is not the relay's.
	cp := *mine
	if r.AllowFailed("t1", &cp) {
		t.Error("the guard matched by value")
	}
	if !r.Abort(k) || !errors.Is(context.Cause(ctx1), ErrAborted) {
		t.Error("abort did not cancel the dispatch")
	}
	if res := d1.Done(); res.Outcome != lifecycle.DispatchAborted {
		t.Errorf("aborted dispatch: %+v", res)
	}
	if r.Abort(k) || !r.AllowFailed("t1", synth) {
		t.Error("a done dispatch is still registered")
	}

	// A newer dispatch of the task takes over: the older is canceled and cannot unregister the newer.
	ctxA, cancelA := context.WithCancelCause(context.Background())
	dA, _ := r.Register(Key{TaskID: "t2", N: 1}, cancelA)
	_, cancelB := context.WithCancelCause(context.Background())
	dB, _ := r.Register(Key{TaskID: "t2", N: 1}, cancelB)
	if !errors.Is(context.Cause(ctxA), ErrSuperseded) || !dA.Superseded() {
		t.Error("the older dispatch was not superseded")
	}
	if res := dA.Done(); res.Outcome != lifecycle.DispatchContextLost {
		t.Errorf("superseded: %+v", res)
	}
	if r.Running(Key{TaskID: "t2", N: 1}) != dB {
		t.Error("the older dispatch's Done unregistered the newer")
	}
	dB.SetOutcome(lifecycle.DispatchRetryable, store.ReasonUpstreamError)
	dB.SetRetryAfter(1500 * time.Millisecond)
	if res := dB.Done(); res.Outcome != lifecycle.DispatchRetryable || res.Reason != "upstream_error" || res.RetryAfterSeconds != 2 {
		t.Errorf("retryable %+v", res)
	}
	if _, err := r.Register(k, nil); err == nil {
		t.Error("registered without a cancel function")
	}
}

func TestCallerOwner(t *testing.T) {
	ctx, cc := a2asrv.NewCallContext(context.Background(), nil)
	if _, err := CallerOwner(ctx); !errors.Is(err, a2a.ErrUnauthenticated) {
		t.Errorf("no user: %v", err)
	}
	cc.User = &a2asrv.User{Name: "0192f0b4-0000-7000-8000-00000000000a", Authenticated: true,
		Attributes: map[string]any{AttrPrincipal: "key:kk_a_00000001", AttrVia: "api_key", AttrTenant: "0192f0b4-0000-7000-8000-0000000000ff"}}
	o, err := CallerOwner(ctx)
	if err != nil || o.Principal != "key:kk_a_00000001" || o.Via != "api_key" || o.TenantID == nil || o.BuyerID.String() != cc.User.Name {
		t.Errorf("%+v %v", o, err)
	}
	cc.User.Name = "not-a-uuid"
	if _, err := CallerOwner(ctx); !errors.Is(err, a2a.ErrUnauthenticated) {
		t.Errorf("bad buyer: %v", err)
	}
}
