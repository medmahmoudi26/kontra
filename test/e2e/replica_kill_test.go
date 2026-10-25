package e2e

import (
	"context"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"

	"kontra.sh/kontra/test/e2e/slowagent"
)

// killUpstream is the slow upstream of replica_kill: 60 chunks a second apart, long enough to outlive a
// dispatch heartbeat timeout (30 s) with chunks still to come.
var killUpstream = slowagent.Config{Chunks: 60, Interval: time.Second, Streaming: true, Binding: "JSONRPC"}

// cookieCall sends a caller cookie with every buyer request: it must never reach the upstream.
var cookieCall = Call{Header: http.Header{"Cookie": {"session=e2e-cookie"}}}

// dispatching starts a streaming task on replica A and reads until the dispatch has its first upstream task
// id and three artifact rows have arrived. It returns the stream, the events read, the task and the
// replica X holding the dispatch.
func dispatching(t *testing.T, ctx context.Context, h *Harness, text string) (*Stream, []Event, a2a.TaskID, *Gate, Attempt) {
	t.Helper()
	s, err := h.BuyerA().SendStreaming(ctx, h.Gates[0], newMessage(text), cookieCall)
	if err != nil {
		t.Fatal(err)
	}
	artifacts := 0
	evs := readUntil(t, ctx, s, func(ev Event) bool {
		if ev.Kind == "artifact" {
			artifacts++
		}
		return artifacts == 3
	})
	taskID := s.TaskID()
	var att Attempt
	eventually(t, 10*time.Second, "attempts.first_upstream_id_at", func() bool {
		as, err := h.Attempts(ctx, taskID)
		if err != nil || len(as) != 1 || as[0].FirstUpstreamIDAt == nil {
			return false
		}
		att = as[0]
		return true
	})
	x := h.GateByReplica(att.ReplicaID)
	if x == nil {
		t.Fatalf("attempts.replica_id %q is neither replica", att.ReplicaID)
	}
	return s, evs, taskID, x, att
}

// watchReattach polls attempts until replica_id is want, and sends the time it saw it.
func watchReattach(ctx context.Context, h *Harness, taskID a2a.TaskID, want string) <-chan time.Time {
	seen := make(chan time.Time, 1)
	go func() {
		for ctx.Err() == nil {
			if r, _, err := h.AttemptReplica(ctx, taskID); err == nil && r == want {
				seen <- time.Now()
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
	}()
	return seen
}

// lastID is the id of the last event that has one.
func lastID(t *testing.T, evs ...[]Event) int64 {
	t.Helper()
	var all []Event
	for _, e := range evs {
		all = append(all, e...)
	}
	got := ids(all)
	if len(got) == 0 {
		t.Fatal("no event with an id")
	}
	return got[len(got)-1]
}

// resumeOn subscribes with Last-Event-ID on g, retrying until g answers with a stream, and checks the
// leading snapshot against Log.SnapshotAt.
func resumeOn(t *testing.T, ctx context.Context, h *Harness, g *Gate, taskID a2a.TaskID, lei int64) (*Stream, Event) {
	t.Helper()
	var (
		s   *Stream
		err error
	)
	deadline := time.Now().Add(2 * time.Minute)
	for {
		if s, err = h.BuyerA().Subscribe(ctx, g, taskID, &lei, cookieCall); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("resume on %s with Last-Event-ID %d: %v", g.Name, lei, err)
		}
		time.Sleep(time.Second)
	}
	lead, err := s.Next(ctx)
	if err != nil {
		t.Fatalf("resume on %s: %v", g.Name, err)
	}
	if lead.ID != nil || lead.Kind != "task" {
		t.Fatalf("resume on %s: the first event is %s id %v, want the snapshot without an id", g.Name, lead.Kind, lead.ID)
	}
	snap, err := h.Log().SnapshotAt(ctx, uuid.MustParse(string(taskID)), lei)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := canonical(t, lead.Task()), canonical(t, snap); got != want {
		t.Fatalf("resume on %s: the snapshot is not Log.SnapshotAt(%d):\n got %s\nwant %s", g.Name, lei, got, want)
	}
	return s, lead
}

// drainBroken reads a stream whose replica is gone until it breaks, keeping what arrived before.
func drainBroken(ctx context.Context, s *Stream) []Event {
	var evs []Event
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		ev, err := s.Next(rctx)
		if err != nil {
			return evs
		}
		evs = append(evs, ev)
	}
}

// checkCompletedOnce checks the end state both replica tests share: the task completed on one attempt run
// by survivor, the upstream got exactly one send, the projection's artifacts are the upstream task's part
// for part with no part twice, and the log holds exactly one terminal status.
func checkCompletedOnce(t *testing.T, ctx context.Context, h *Harness, taskID a2a.TaskID, survivor *Gate, before Attempt) {
	t.Helper()
	task, err := h.BuyerA().Client(t, survivor, "").GetTask(ctx, &a2a.GetTaskRequest{ID: taskID})
	if err != nil {
		t.Fatalf("GetTask on %s: %v", survivor.Name, err)
	}
	if task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("the task ended %s, want completed", task.Status.State)
	}
	as, err := h.Attempts(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if len(as) != 1 || as[0].N != 1 {
		t.Fatalf("attempts = %+v, want exactly one row with n = 1 (a re-attach is not a new attempt)", as)
	}
	if as[0].ReplicaID != survivor.ReplicaID {
		t.Fatalf("attempts.replica_id = %s, want the survivor %s", as[0].ReplicaID, survivor.ReplicaID)
	}
	if as[0].DispatchID == nil || before.DispatchID == nil || *as[0].DispatchID == *before.DispatchID {
		t.Fatalf("attempts.dispatch_id %v did not change from %v: the re-attach must hold a fresh fence (D77)", as[0].DispatchID, before.DispatchID)
	}
	if as[0].UpstreamTaskID != before.UpstreamTaskID || as[0].Outcome != "completed" {
		t.Fatalf("attempt after the re-attach: upstream task %s outcome %q, want %s completed", as[0].UpstreamTaskID, as[0].Outcome, before.UpstreamTaskID)
	}
	if n := h.Upstream.Sends(); n != 1 {
		t.Fatalf("the upstream received %d sends, want exactly 1 (no retry after the first upstream task id)", n)
	}
	if h.Upstream.Count("GetTask") == 0 {
		t.Fatal("the upstream received no GetTask: the survivor did not re-attach (spec §6.4)")
	}
	up, err := h.Upstream.Task(ctx, a2a.TaskID(before.UpstreamTaskID))
	if err != nil {
		t.Fatal(err)
	}
	got, want := artifactParts(task), artifactParts(up)
	if !slices.Equal(got, want) {
		t.Fatalf("the projection's artifacts are not the upstream task's, part for part:\n got %v\nwant %v", got, want)
	}
	if len(want) != killUpstream.Chunks {
		t.Fatalf("the upstream task has %d parts, want %d", len(want), killUpstream.Chunks)
	}
	seen := map[string]bool{}
	for _, p := range got {
		if seen[p] {
			t.Fatalf("part %q appears twice in the projection", p)
		}
		seen[p] = true
	}
	rows, err := h.Rows(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	terminals := 0
	for _, r := range rows {
		if r.Kind != "status" {
			continue
		}
		var sr a2a.StreamResponse
		if err := sr.UnmarshalJSON(r.Payload); err != nil {
			t.Fatalf("row %d: %v", r.Seq, err)
		}
		if su, ok := sr.Event.(*a2a.TaskStatusUpdateEvent); ok && su.Status.State.Terminal() {
			terminals++
		}
	}
	if terminals != 1 {
		t.Fatalf("the log holds %d terminal status rows, want exactly 1", terminals)
	}
	checkUpstreamHeaders(t, h, h.Upstream)
}

// TestReplicaKillDirect is replica_kill, direct-backend variant (spec/relay.md §6.4, §14.2, §16; D42), the
// M1 gate: two replicas; a streaming task on the slow upstream; the buyer follows it on the replica that
// holds the dispatch (attempts.replica_id); that replica is SIGKILLed mid-stream after the first upstream
// task id. The dispatch's heartbeat times out (within 60 s), the workflow reads attempt_state and
// re-attaches on the surviving replica with a new dispatch of the same attempt, which writes its replica
// id, GetTasks the upstream, logs only the difference and follows it to completed. The buyer resumes with
// Last-Event-ID against the surviving replica. Expected: the task completes; attempt n is still 1 and the
// upstream received exactly one send; the buyer's carried rows across all its streams equal the log's
// carried rows, with no gap and no duplicate.
func TestReplicaKillDirect(t *testing.T) {
	h := start(t, Options{Upstream: killUpstream})
	ctx := ctxFor(t, 8*time.Minute)

	s1, evs1, taskID, x, before := dispatching(t, ctx, h, "replica_kill")
	y := h.Other(x)
	t.Logf("task %s: the dispatch runs on %s (%s); %s (%s) survives", taskID, x.Name, x.ReplicaID, y.Name, y.ReplicaID)

	// Resume 1: the buyer drops its first stream and follows the task on X.
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}
	s2, _ := resumeOn(t, ctx, h, x, taskID, lastID(t, evs1))
	var evs2 []Event
	for len(ids(evs2)) < 3 {
		evs2 = append(evs2, readUntil(t, ctx, s2, func(Event) bool { return true })...)
	}

	// SIGKILL X, no drain; its stream breaks.
	watch, stopWatch := context.WithCancel(ctx)
	defer stopWatch()
	reattached := watchReattach(watch, h, taskID, y.ReplicaID)
	killed := time.Now()
	if err := x.Kill(); err != nil {
		t.Fatal(err)
	}
	evs2 = append(evs2, drainBroken(ctx, s2)...)
	t.Logf("%s killed after %d events on resume 1", x.Name, len(evs2))

	// Resume 2 on Y, from the last id received, to the end.
	s3, _ := resumeOn(t, ctx, h, y, taskID, lastID(t, evs1, evs2))
	evs3, err := s3.ReadAll(ctx)
	if err != nil {
		t.Fatalf("resume 2: %v", err)
	}
	if len(evs3) == 0 || evs3[len(evs3)-1].State != a2a.TaskStateCompleted {
		t.Fatalf("resume 2 must end right after the completed row: %d events", len(evs3))
	}

	select {
	case at := <-reattached:
		d := at.Sub(killed)
		t.Logf("re-attached on %s %.1f s after the SIGKILL", y.Name, d.Seconds())
		if d > 60*time.Second {
			t.Fatalf("the dispatch was re-attached %v after the kill, want within 60 s (spec §14.2)", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("attempts.replica_id never became the survivor's")
	}

	// The carried ids across the three streams are the log's, in order, each once.
	checkCarried(t, h, taskID, append(append(ids(evs1), ids(evs2)...), ids(evs3)...))
	checkCompletedOnce(t, ctx, h, taskID, y, before)
	checkHistory(t, h, taskID, "e2e_replica_kill_direct", slowagent.ChunkMarker, h.APIKey, h.APIKeyB, "e2e-cookie")
}

// TestReplicaStallDirect is replica_kill's stalled variant (audit: a dispatch whose heartbeat timed out
// while its process lives, D77): the replica X holding the dispatch is SIGSTOPped for 40 s and then
// resumed. Its heartbeat times out and the task is re-attached with a fresh dispatch_id. The re-attach
// lands on whichever replica polls kontra-direct first: the survivor, or X itself once it runs again,
// because a stopped process keeps its long polls open and the server may hand one of them the resume
// dispatch (D77 names this case: the fence is per dispatch, not per replica). When X runs again its old
// dispatch learns at its next heartbeat that it is gone, and an append it makes before that is fenced
// (which of the two stops it is a race the test cannot steer; the fence itself is tested in
// internal/store): the task completes once, on one attempt, with no part logged twice, the buyer's
// resumed stream has no gap and no duplicate, and X serves again.
func TestReplicaStallDirect(t *testing.T) {
	h := start(t, Options{Upstream: killUpstream})
	ctx := ctxFor(t, 8*time.Minute)

	s1, evs1, taskID, x, before := dispatching(t, ctx, h, "replica_stall")
	y := h.Other(x)
	t.Logf("task %s: the dispatch runs on %s (%s); %s is stopped for 40 s", taskID, x.Name, x.ReplicaID, x.Name)

	stopped := time.Now()
	if err := x.Stop(); err != nil {
		t.Fatal(err)
	}
	continued := false
	defer func() {
		if !continued {
			_ = x.Continue()
		}
	}()
	// The buyer's stream on A stalls if A is X: it drops it either way.
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Until(stopped.Add(40 * time.Second)))
	if err := x.Continue(); err != nil {
		t.Fatal(err)
	}
	continued = true

	// The re-attach writes a fresh dispatch_id on attempt 1.
	var after Attempt
	eventually(t, 3*time.Minute, "a re-attach with a fresh dispatch_id", func() bool {
		as, err := h.Attempts(ctx, taskID)
		if err != nil || len(as) != 1 || as[0].DispatchID == nil || *as[0].DispatchID == *before.DispatchID {
			return false
		}
		after = as[0]
		return true
	})
	holder := h.GateByReplica(after.ReplicaID)
	if holder == nil {
		t.Fatalf("the re-attach runs on %q, neither replica", after.ReplicaID)
	}
	t.Logf("re-attached on %s %.1f s after SIGSTOP", holder.Name, time.Since(stopped).Seconds())

	s2, _ := resumeOn(t, ctx, h, y, taskID, lastID(t, evs1))
	evs2, err := s2.ReadAll(ctx)
	if err != nil {
		t.Fatalf("resume on %s: %v", y.Name, err)
	}
	if len(evs2) == 0 || evs2[len(evs2)-1].State != a2a.TaskStateCompleted {
		t.Fatalf("the resumed stream must end right after the completed row")
	}
	checkCarried(t, h, taskID, append(ids(evs1), ids(evs2)...))
	checkCompletedOnce(t, ctx, h, taskID, holder, before)

	for _, line := range grepLog(x.LogPath, "fenc") {
		t.Logf("%s: %s", x.Name, line)
	}

	// The stalled replica lives on and serves: its /healthz comes back and it answers GetTask.
	if x.Exited() {
		t.Fatalf("%s exited after SIGCONT:\n%s", x.Name, tail(x.LogPath, 40))
	}
	eventually(t, 60*time.Second, x.Name+" healthy again", func() bool {
		return status(ctx, http.MethodGet, x.URL+"/healthz", "") == http.StatusOK
	})
	task, err := h.BuyerA().Client(t, x, "").GetTask(ctx, &a2a.GetTaskRequest{ID: taskID})
	if err != nil || task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("GetTask on the resumed %s: %v %v", x.Name, task, err)
	}
	checkHistory(t, h, taskID, "e2e_replica_stall_direct", slowagent.ChunkMarker, h.APIKey, h.APIKeyB, "e2e-cookie")
}

// grepLog returns the lines of a replica's log that contain substr.
func grepLog(path, substr string) []string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.Contains(l, substr) {
			out = append(out, l)
		}
	}
	return out
}
