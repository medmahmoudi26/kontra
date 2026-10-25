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
	// The dispatch ends the attempt (ended_at, outcome) after it logged the terminal row the buyer saw.
	var as []Attempt
	eventually(t, 30*time.Second, "the attempt ended", func() bool {
		var err error
		as, err = h.Attempts(ctx, taskID)
		return err == nil && (len(as) != 1 || as[0].EndedAt != nil)
	})
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

// stall is how long TestReplicaStallDirect keeps X stopped: two heartbeat rounds (2 x 30 s, spec §14.2)
// and a margin. A stopped process keeps its open kontra-direct long polls, which the server still counts
// as pollers, so the first resume dispatch (30-40 s after SIGSTOP) can be handed to X itself; it then times
// out in turn, and the second resume (60-70 s after SIGSTOP, measured) is the first one sure to reach the
// survivor, X's polls having expired by then (the server ends a long poll after a minute).
const stall = 100 * time.Second

// takeover bounds how long after SIGSTOP the survivor may take a stalled replica's dispatch over: two
// heartbeat rounds and their timer delays. A dead replica's is one round (TestReplicaKillDirect, 60 s).
const takeover = 90 * time.Second

// TestReplicaStallDirect is replica_kill's stalled variant (audit: a dispatch whose heartbeat timed out
// while its process lives, D77): the replica X holding the dispatch is SIGSTOPped long enough for the
// survivor Y to take the attempt over while X is still stopped (stall), and then resumed. While X is
// stopped, Y writes a fresh dispatch_id on attempt 1 (within takeover of the SIGSTOP: a stalled replica is
// detected only after two heartbeat rounds, against one for a dead one). Once X runs again, its old
// dispatch, still alive and reading the upstream, is superseded: it logs "dispatch superseded by a newer
// dispatch" and appends nothing (D77). The task completes once, on one attempt, with no part logged
// twice, the buyer's resumed stream has no gap and no duplicate, and X serves again.
func TestReplicaStallDirect(t *testing.T) {
	h := start(t, Options{Upstream: killUpstream})
	ctx := ctxFor(t, 10*time.Minute)

	s1, evs1, taskID, x, before := dispatching(t, ctx, h, "replica_stall")
	y := h.Other(x)
	t.Logf("task %s: the dispatch runs on %s (%s); %s is stopped for %v", taskID, x.Name, x.ReplicaID, x.Name, stall)

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

	// While X is stopped, the survivor takes the attempt over with a fresh dispatch_id.
	var after Attempt
	for {
		as, err := h.Attempts(ctx, taskID)
		if err == nil && len(as) == 1 && as[0].DispatchID != nil && *as[0].DispatchID != *before.DispatchID && as[0].ReplicaID == y.ReplicaID {
			after = as[0]
			break
		}
		if time.Since(stopped) > takeover {
			t.Fatalf("%s did not take the attempt over within %v of SIGSTOP while %s was stopped: %+v %v", y.Name, takeover, x.Name, as, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Logf("re-attached on %s %.1f s after SIGSTOP, %s still stopped", y.Name, time.Since(stopped).Seconds(), x.Name)
	if !x.Stopped() {
		t.Fatalf("%s is not stopped", x.Name)
	}
	time.Sleep(time.Until(stopped.Add(stall)))
	if err := x.Continue(); err != nil {
		t.Fatal(err)
	}
	continued = true

	s2, _ := resumeOn(t, ctx, h, y, taskID, lastID(t, evs1))
	evs2, err := s2.ReadAll(ctx)
	if err != nil {
		t.Fatalf("resume on %s: %v", y.Name, err)
	}
	if len(evs2) == 0 || evs2[len(evs2)-1].State != a2a.TaskStateCompleted {
		t.Fatalf("the resumed stream must end right after the completed row")
	}
	checkCarried(t, h, taskID, append(ids(evs1), ids(evs2)...))
	checkCompletedOnce(t, ctx, h, taskID, y, before)
	if as, err := h.Attempts(ctx, taskID); err != nil || len(as) != 1 || *as[0].DispatchID != *after.DispatchID {
		t.Fatalf("attempts %+v %v: the dispatch that completed the task is not the one %s took over with (%s)", as, err, y.Name, after.DispatchID)
	}

	// X's old dispatch, alive again, was superseded (its fence, or the registry, D77) and logged so.
	eventually(t, 30*time.Second, x.Name+" logs its old dispatch superseded", func() bool {
		return len(grepLog(x.LogPath, "dispatch superseded by a newer dispatch")) > 0
	})
	for _, line := range grepLog(x.LogPath, "superseded") {
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
