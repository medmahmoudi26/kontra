package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	a2apush "github.com/a2aproject/a2a-go/v2/a2asrv/push"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"github.com/a2aproject/a2a-go/v2/a2asrv/workqueue"
	"github.com/google/uuid"

	"kontra.sh/kontra/internal/lifecycle"
	"kontra.sh/kontra/internal/store"
	"kontra.sh/kontra/internal/upstream"
)

// upWorking is the scripted upstream's task up-1, working.
const upWorking = `{"id":"up-1","contextId":"up-ctx","status":{"state":"TASK_STATE_WORKING"}}`

// reattachable makes attempt 1 of a task look like a dispatch that recorded its first upstream id and then
// lost its context: the upstream id is recorded and the turn's work item consumed.
func (e *env) reattachable(id a2a.TaskID, wid uuid.UUID) {
	e.t.Helper()
	e.firstID(id, 1, "up-1", "up-ctx")
	if err := e.log.ConsumeWorkItem(context.Background(), wid, uid(e.t, id)); err != nil {
		e.t.Fatal(err)
	}
}

// firstID records upstream as attempt n's first upstream task id, as a dispatch of the attempt does: under
// the fence of a dispatch begun for it (SetFirstUpstreamID is fenced, D77).
func (e *env) firstID(id a2a.TaskID, n int, upstream, upstreamCtx string) {
	e.t.Helper()
	ctx := context.Background()
	fence, err := e.log.BeginDispatch(ctx, uid(e.t, id), n, "test-replica")
	if err != nil {
		e.t.Fatal(err)
	}
	if err := e.log.SetFirstUpstreamID(ctx, fence, upstream, upstreamCtx); err != nil {
		e.t.Fatal(err)
	}
}

// After the first upstream id a GetTask answer is classified by §6.4's table, on the polling path and on a
// re-attach alike. A protocol error fails the task upstream_error with the upstream's message, and a body
// over its limit (§6.6) fails it upstream_error, both at the first such answer. Only a GetTask that does not
// reach the service (refused, reset, 502/503/504, no answer) counts toward the 30 s rule and then fails it
// upstream_unreachable; a throttled one (408/429) counts toward it too and then fails it upstream_error,
// the table's reason for 408/429.
func TestGetTaskFailures(t *testing.T) {
	const grace = 300 * time.Millisecond
	// One call may take longer than the grace (an 8 MiB body under -race): only TestGetTaskNeverAnswered is
	// about the per-call deadline.
	e := newEnv(t, envOptions{opts: ExecutorOptions{GetTaskGrace: grace, GetTaskTimeout: 10 * time.Second, PollInterval: 50 * time.Millisecond}})
	big := strings.Repeat("a", upstream.MaxBody)
	completed := `"result":{"id":"up-1","contextId":"up-ctx","status":{"state":"TASK_STATE_COMPLETED","message":{"messageId":"a1","role":"ROLE_AGENT","parts":[{"text":"done"}]}}}`
	for _, tc := range []struct {
		name   string
		get    func(w http.ResponseWriter, id json.RawMessage, n int64)
		reason string // "" for a task that completes
		text   string
		once   bool // failed at the first GetTask, not after the grace
	}{
		{"JSON-RPC error", func(w http.ResponseWriter, id json.RawMessage, _ int64) {
			rpcResult(w, id, `"error":{"code":-32001,"message":"seller lost the task"}`)
		}, "upstream_error", "seller lost the task", true},
		{"HTTP 404", func(w http.ResponseWriter, _ json.RawMessage, _ int64) { w.WriteHeader(404) },
			"upstream_error", "HTTP 404", true},
		{"body over 8 MiB", func(w http.ResponseWriter, id json.RawMessage, _ int64) {
			rpcResult(w, id, `"result":{"id":"up-1","contextId":"up-ctx","status":{"state":"TASK_STATE_WORKING"},"metadata":{"x":"`+big+`"}}`)
		}, "upstream_error", "more than the gate accepts", true},
		{"HTTP 502", func(w http.ResponseWriter, _ json.RawMessage, _ int64) { w.WriteHeader(502) },
			"upstream_unreachable", "did not answer GetTask", false},
		{"HTTP 429", func(w http.ResponseWriter, _ json.RawMessage, _ int64) { w.WriteHeader(429) },
			"upstream_error", "HTTP 429", false},
		{"HTTP 503, then the answer", func(w http.ResponseWriter, id json.RawMessage, n int64) {
			if n < 3 {
				w.WriteHeader(503)
				return
			}
			rpcResult(w, id, completed)
		}, "", "done", false},
	} {
		for _, resume := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/resume=%v", tc.name, resume), func(t *testing.T) {
				var gets atomic.Int64
				b, _ := scripted(t, func(method string, id json.RawMessage, w http.ResponseWriter, _ *http.Request) {
					if method == "SendMessage" {
						rpcResult(w, id, `"result":{"task":`+upWorking+`}`)
						return
					}
					tc.get(w, id, gets.Add(1))
				})
				e.backends.set(b, rpcInfo, nil)
				id, wid := e.newTask(taskSpec{owner: buyerA, text: "q"})
				if resume {
					e.reattachable(id, wid)
				}
				start := time.Now()
				res := e.dispatch(context.Background(), id, 1, wid, resume)
				took := time.Since(start)
				p := projection(t, e, id)
				if tc.reason == "" {
					if res.Outcome != lifecycle.DispatchTerminal || p.Status.State != a2a.TaskStateCompleted ||
						p.Status.Message.Parts[0].Text() != tc.text {
						t.Fatalf("%+v %+v", res, p.Status)
					}
					return
				}
				if res.Outcome != lifecycle.DispatchTerminal || p.Status.State != a2a.TaskStateFailed ||
					p.Metadata[store.MetaFailure] != tc.reason || !strings.Contains(p.Status.Message.Parts[0].Text(), tc.text) {
					t.Fatalf("after %d GetTask calls: %+v %v %q", gets.Load(), res, p.Metadata[store.MetaFailure], p.Status.Message.Parts[0].Text())
				}
				if tc.once && gets.Load() != 1 {
					t.Errorf("failed after %d GetTask calls, not at the first", gets.Load())
				}
				if !tc.once && (gets.Load() < 2 || took < grace) {
					t.Errorf("failed after %d GetTask calls and %v, before the grace", gets.Load(), took)
				}
				if a := e.attempt(id, 1); string(a.Outcome) != tc.reason || a.EndedAt == nil {
					t.Errorf("attempt %+v", a)
				}
			})
		}
	}
}

// A GetTask the upstream accepts and never answers is bounded by a deadline of its own and counts toward
// the 30 s rule, on the polling path and on a re-attach (spec §6.4, D42): the task fails
// upstream_unreachable after the grace, long before the dispatch's own context ends.
func TestGetTaskNeverAnswered(t *testing.T) {
	const grace = 300 * time.Millisecond
	e := newEnv(t, envOptions{opts: ExecutorOptions{GetTaskGrace: grace, PollInterval: 50 * time.Millisecond}})
	for _, resume := range []bool{false, true} {
		t.Run(fmt.Sprintf("resume=%v", resume), func(t *testing.T) {
			var gets atomic.Int64
			b, _ := scripted(t, func(method string, id json.RawMessage, w http.ResponseWriter, r *http.Request) {
				if method == "SendMessage" {
					rpcResult(w, id, `"result":{"task":`+upWorking+`}`)
					return
				}
				gets.Add(1)
				<-r.Context().Done() // accepted, never answered
			})
			e.backends.set(b, rpcInfo, nil)
			id, wid := e.newTask(taskSpec{owner: buyerA, text: "q"})
			if resume {
				e.reattachable(id, wid)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			start := time.Now()
			res := e.dispatch(ctx, id, 1, wid, resume)
			took := time.Since(start)
			p := projection(t, e, id)
			if res.Outcome != lifecycle.DispatchTerminal || p.Metadata[store.MetaFailure] != "upstream_unreachable" {
				t.Fatalf("after %v and %d GetTask calls: %+v %s", took, gets.Load(), res, p.Status.State)
			}
			if took < grace || took > 5*time.Second {
				t.Errorf("failed after %v", took)
			}
		})
	}
}

// abortingBackends aborts the dispatch from inside Backend, after it registered and before the executor
// reads the log and calls the upstream.
type abortingBackends struct {
	*staticBackends
	abort func()
}

func (a *abortingBackends) Backend(ctx context.Context, svc store.ServiceRef) (upstream.Backend, upstream.Info, error) {
	a.abort()
	return a.staticBackends.Backend(ctx, svc)
}

// An abort (spec §8) that reaches this replica once the dispatch has recorded it in attempts.replica_id,
// which is what routes abort here (§14.2), is never lost: one that lands before the dispatch registered, and
// one that lands after it while the executor still reads the log, both stop the dispatch before it calls
// the upstream; the dispatch returns aborted and the attempt is recorded aborted.
func TestAbortBeforeTheUpstreamCall(t *testing.T) {
	for _, when := range []string{"before register", "after register"} {
		t.Run(when, func(t *testing.T) {
			e := newEnv(t, envOptions{})
			var sends atomic.Int64
			b, _ := scripted(t, func(_ string, id json.RawMessage, w http.ResponseWriter, _ *http.Request) {
				sends.Add(1)
				rpcResult(w, id, `"result":{"task":{"id":"up-1","contextId":"up-ctx","status":{"state":"TASK_STATE_COMPLETED"}}}`)
			})
			e.backends.set(b, rpcInfo, nil)
			id, wid := e.newTask(taskSpec{owner: buyerA, text: "q"})
			ref := lifecycle.AttemptRef{TaskID: string(id), N: 1}
			var abortErr error
			abort := func() {
				if a := e.attempt(id, 1); a.ReplicaID != replica {
					t.Errorf("abort routed before the dispatch recorded its replica: %+v", a)
				}
				abortErr = e.disp.Abort(context.Background(), ref)
			}
			if when == "before register" {
				e.disp.Rebuild = func(taskID a2a.TaskID, wi *store.WorkItem) (*workqueue.Payload, error) {
					abort()
					return rebuild(taskID, wi)
				}
			} else {
				ab := &abortingBackends{staticBackends: e.backends, abort: abort}
				e.handlers.deps.Backends = ab // before the service's handler is built
			}
			res := e.dispatch(context.Background(), id, 1, wid, false)
			if abortErr != nil || res.Outcome != lifecycle.DispatchAborted || sends.Load() != 0 {
				t.Fatalf("abort err %v, result %+v, %d upstream calls", abortErr, res, sends.Load())
			}
			if a := e.attempt(id, 1); a.Outcome != store.OutcomeAborted || a.EndedAt == nil {
				t.Errorf("attempt %+v", a)
			}
			if row := e.row(id); row.State != store.StateSubmitted {
				t.Errorf("state %s", row.State)
			}
			if e.reg.Running(Key{TaskID: id, N: 1}) != nil {
				t.Error("the aborted dispatch is still registered")
			}
		})
	}
}

// A follow-up turn whose upstream answers first with its task as it still is (input-required, the status
// the projection already shows) keeps following the upstream to the turn's own end: A2A 1.0.1 lets
// SendMessage{returnImmediately} return any in-progress state, input-required included (spec §6.2, §7).
// The same holds when the stale task opens a stream, and when the upstream writes timestamps finer than the
// milliseconds the log keeps.
func TestFollowUpStaleSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name      string
		streaming bool
		ts        string
	}{
		{"polling", false, "2026-10-25T10:00:00Z"},
		{"polling, microseconds", false, "2026-10-25T10:00:00.123456Z"},
		{"streaming", true, "2026-10-25T10:00:00.5Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, envOptions{opts: ExecutorOptions{PollInterval: 50 * time.Millisecond}})
			ask := `{"id":"up-1","contextId":"up-ctx","status":{"state":"TASK_STATE_INPUT_REQUIRED","timestamp":"` + tc.ts +
				`","message":{"messageId":"q1","role":"ROLE_AGENT","parts":[{"text":"which one?"}]}}}`
			done := `{"taskId":"up-1","contextId":"up-ctx","status":{"state":"TASK_STATE_COMPLETED","message":{"messageId":"a1","role":"ROLE_AGENT","parts":[{"text":"booked"}]}}}`
			var turns, gets atomic.Int64
			b, _ := scripted(t, func(method string, id json.RawMessage, w http.ResponseWriter, _ *http.Request) {
				switch method {
				case "SendStreamingMessage":
					if turns.Add(1) == 1 {
						sse(w, id, `"result":{"task":`+ask+`}`)
						return
					}
					sse(w, id, `"result":{"task":`+ask+`}`,
						`"result":{"statusUpdate":{"taskId":"up-1","contextId":"up-ctx","status":{"state":"TASK_STATE_WORKING"}}}`,
						`"result":{"statusUpdate":`+done+`}`)
				case "SendMessage":
					turns.Add(1)
					rpcResult(w, id, `"result":{"task":`+ask+`}`)
				default:
					if gets.Add(1) < 3 {
						rpcResult(w, id, `"result":`+upWorking)
						return
					}
					rpcResult(w, id, `"result":{"id":"up-1","contextId":"up-ctx","status":{"state":"TASK_STATE_COMPLETED","message":{"messageId":"a1","role":"ROLE_AGENT","parts":[{"text":"booked"}]}}}`)
				}
			})
			info := rpcInfo
			info.Streaming = tc.streaming
			e.backends.set(b, info, nil)
			id, wid := e.newTask(taskSpec{owner: buyerA, text: "book"})
			if res := e.dispatch(context.Background(), id, 1, wid, false); res.Outcome != lifecycle.DispatchInterrupted {
				t.Fatalf("turn 1: %+v", res)
			}
			fid := e.followUp(id, buyerA, "the blue one")
			res := e.dispatch(context.Background(), id, 1, fid, false)
			p := projection(t, e, id)
			if res.Outcome != lifecycle.DispatchTerminal || p.Status.State != a2a.TaskStateCompleted || p.Status.Message.Parts[0].Text() != "booked" {
				t.Fatalf("the follow-up turn: %+v, log %v, %d GetTask calls", res, e.states(id), gets.Load())
			}
			asked := 0
			for _, s := range e.states(id) {
				if s == "status:TASK_STATE_INPUT_REQUIRED" {
					asked++
				}
			}
			if asked != 1 || turns.Load() != 2 {
				t.Errorf("log %v after %d sends", e.states(id), turns.Load())
			}
		})
	}
}

// A re-attach compares the upstream task with the projection at the precision the log keeps (the A2A wire
// writes milliseconds): an upstream writing microseconds does not get its status logged twice (spec §6.3:
// nothing already in the log is logged twice).
func TestReattachFineTimestamps(t *testing.T) {
	e := newEnv(t, envOptions{opts: ExecutorOptions{PollInterval: 50 * time.Millisecond}})
	var resumed atomic.Bool
	var resumeGets atomic.Int64
	const working = `{"id":"up-1","contextId":"up-ctx","status":{"state":"TASK_STATE_WORKING","timestamp":"2026-10-25T10:00:00.123456Z"}}`
	b, _ := scripted(t, func(method string, id json.RawMessage, w http.ResponseWriter, _ *http.Request) {
		switch {
		case method == "SendMessage":
			rpcResult(w, id, `"result":{"task":`+working+`}`)
		case resumed.Load() && resumeGets.Add(1) > 2:
			rpcResult(w, id, `"result":{"id":"up-1","contextId":"up-ctx","status":{"state":"TASK_STATE_COMPLETED","timestamp":"2026-10-25T10:00:01.654321Z","message":{"messageId":"m","role":"ROLE_AGENT","parts":[{"text":"ok"}]}}}`)
		default:
			rpcResult(w, id, `"result":`+working)
		}
	})
	e.backends.set(b, rpcInfo, nil)
	id, wid := e.newTask(taskSpec{owner: buyerA, text: "q"})
	ctx, lose := context.WithCancel(context.Background())
	first := make(chan lifecycle.DispatchResult, 1)
	go func() { first <- e.dispatch(ctx, id, 1, wid, false) }()
	waitFor(t, "working logged", func() bool { return e.row(id).State == store.StateWorking })
	time.Sleep(150 * time.Millisecond) // a few unchanged polls
	lose()
	if res := <-first; res.Outcome != lifecycle.DispatchContextLost {
		t.Fatalf("first dispatch %+v", res)
	}
	resumed.Store(true) // the resume's first two GetTask calls see the same working status
	res := e.dispatch(context.Background(), id, 1, wid, true)
	if res.Outcome != lifecycle.DispatchTerminal {
		t.Fatalf("resume %+v", res)
	}
	if got := fmt.Sprint(e.states(id)); got != "[task:TASK_STATE_SUBMITTED status:TASK_STATE_WORKING status:TASK_STATE_COMPLETED]" {
		t.Errorf("log %s", got)
	}
}

// flakyPushStore fails List once when armed: the push config read a2a-go's processor makes after every
// logged event.
type flakyPushStore struct {
	a2apush.ConfigStore
	fail atomic.Bool
}

func (s *flakyPushStore) List(ctx context.Context, taskID a2a.TaskID) ([]*a2a.PushConfig, error) {
	if s.fail.CompareAndSwap(true, false) {
		return nil, errors.New("push configs: conn closed")
	}
	return s.ConfigStore.List(ctx, taskID)
}

// A store that fails while the dispatch logs a non-final event (a write that runs out of its time on a row
// lock; the push config read that follows every event) is the gate's failure, not the upstream's: the
// healthy upstream task is not failed upstream_error. The dispatch returns context_lost with the attempt
// open, and a re-attach logs what was missed and follows the task to its end (spec §6.4, D42).
func TestStoreFailureIsALostContext(t *testing.T) {
	for _, what := range []string{"append times out", "push config read fails"} {
		t.Run(what, func(t *testing.T) {
			push := &flakyPushStore{ConfigStore: a2apush.NewInMemoryStore()}
			e := newEnv(t, envOptions{writeTimeout: 300 * time.Millisecond, push: push})
			g := &gatedStreamer{release: make(chan struct{}, 16), chunks: 3}
			agent, b, info := newAgent(t, g, agentConfig{streaming: true})
			info.Streaming = true
			e.backends.set(b, info, nil)
			id, wid := e.newTask(taskSpec{owner: buyerA, text: "q"})
			done := make(chan lifecycle.DispatchResult, 1)
			go func() { done <- e.dispatch(context.Background(), id, 1, wid, false) }()
			waitFor(t, "working", func() bool { return e.row(id).State == store.StateWorking })

			var unlock func()
			if what == "append times out" {
				tx, err := e.pool.Begin(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if _, err := tx.Exec(context.Background(), `select 1 from kontra.tasks where id = $1 for update`, uid(t, id)); err != nil {
					t.Fatal(err)
				}
				unlock = func() { _ = tx.Rollback(context.Background()) }
			} else {
				push.fail.Store(true)
				unlock = func() {}
			}
			g.release <- struct{}{} // chunk 0: its event cannot be logged
			var res lifecycle.DispatchResult
			select {
			case res = <-done:
			case <-time.After(15 * time.Second):
				unlock()
				t.Fatal("the dispatch did not end")
			}
			unlock()
			p := projection(t, e, id)
			if res.Outcome != lifecycle.DispatchContextLost || p.Status.State == a2a.TaskStateFailed {
				t.Fatalf("a store failure on a healthy upstream task: %+v, %s %v", res, p.Status.State, p.Metadata[store.MetaFailure])
			}
			if a := e.attempt(id, 1); a.EndedAt != nil || a.FirstUpstreamIDAt == nil {
				t.Fatalf("attempt %+v", a)
			}
			// The workflow re-attaches the direct backend: the difference is logged and the task completes.
			second := make(chan lifecycle.DispatchResult, 1)
			go func() { second <- e.dispatch(context.Background(), id, 1, wid, true) }()
			waitFor(t, "chunk 0 logged", func() bool { return e.count(id, store.KindArtifact) >= 1 })
			for range 3 {
				g.release <- struct{}{}
			}
			if res := <-second; res.Outcome != lifecycle.DispatchTerminal {
				t.Fatalf("resume %+v", res)
			}
			p = projection(t, e, id)
			var parts []string
			for _, part := range p.Artifacts[0].Parts {
				parts = append(parts, part.Text())
			}
			if p.Status.State != a2a.TaskStateCompleted || strings.Join(parts, ",") != "c0,c1,c2" || agent.sends.Load() != 1 {
				t.Errorf("after the re-attach: %s %v, %d sends", p.Status.State, parts, agent.sends.Load())
			}
		})
	}
}

// A dispatch that Temporal timed out but whose process is still alive (D77): the workflow read
// attempt_state (no first upstream id), decided to retry and prepared attempt 2 while the old dispatch's
// SendMessage was in flight. When the upstream then answers, the old dispatch's first upstream id is
// refused (SetFirstUpstreamID is fenced): it logs nothing and signals nothing, and attempt 1 keeps no
// upstream id, so the retry is the only send the gate follows (spec §6.4: a retry is only ever made before
// the first upstream task id).
func TestStaleDispatchFirstIDIsFenced(t *testing.T) {
	e := newEnv(t, envOptions{})
	id, wid := e.newTask(taskSpec{owner: buyerA, text: "q"})
	sent, answer := make(chan struct{}), make(chan struct{})
	b, _ := scripted(t, func(method string, rid json.RawMessage, w http.ResponseWriter, _ *http.Request) {
		if method != "SendMessage" {
			rpcResult(w, rid, `"result":`+upWorking)
			return
		}
		close(sent)
		<-answer
		rpcResult(w, rid, `"result":{"task":`+upWorking+`}`)
	})
	e.backends.set(b, rpcInfo, nil)
	done := make(chan lifecycle.DispatchResult, 1)
	go func() { done <- e.dispatch(context.Background(), id, 1, wid, false) }()
	<-sent
	// prepare(2): the retry ends attempt 1 and writes attempt 2 in one transaction.
	if err := e.log.BeginRetry(context.Background(), store.Attempt{TaskID: uid(t, id), N: 2, Backend: store.BackendDirect},
		store.OutcomeUpstreamUnreachable); err != nil {
		t.Fatal(err)
	}
	close(answer)
	var res lifecycle.DispatchResult
	select {
	case res = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the stale dispatch did not stop")
	}
	if res.Outcome != lifecycle.DispatchContextLost {
		t.Errorf("outcome %s, want context_lost (superseded)", res.Outcome)
	}
	if a := e.attempt(id, 1); a.UpstreamTaskID != "" || a.FirstUpstreamIDAt != nil || a.Outcome != store.OutcomeUpstreamUnreachable {
		t.Errorf("attempt 1 %+v: the stale dispatch recorded its upstream id on an ended attempt", a)
	}
	if got := e.states(id); len(got) != 1 {
		t.Errorf("log %v: the stale dispatch logged the upstream's answer", got)
	}
	if n := e.signals.count(); n != 0 {
		t.Errorf("%d kontra.dispatched signals from a fenced dispatch", n)
	}
}

// failFirstIDWrites makes the next n writes of an attempt's first upstream task id fail on the server as a
// statement timeout does (SQLSTATE 57014): a sequence, not a table, counts them, because a failed write's
// transaction rolls back everything it wrote.
func (e *env) failFirstIDWrites(n int) {
	e.t.Helper()
	e.exec(`create sequence kontra.zz_fail_first_id`)
	e.exec(fmt.Sprintf(`create function kontra.zz_fail_first_id() returns trigger language plpgsql as $$
		begin
			if nextval('kontra.zz_fail_first_id') <= %d then
				raise exception 'simulated statement timeout' using errcode = '57014';
			end if;
			return new;
		end $$`, n))
	e.exec(`create trigger zz_fail_first_id before update on kontra.attempts for each row
		when (old.upstream_task_id is null and new.upstream_task_id is not null) execute function kontra.zz_fail_first_id()`)
}

// The upstream answered with its task id and the gate's store failed to record it (a statement timeout).
// The dispatch knows the id, so it records it again, backing off, instead of handing the turn back as a lost
// context, which the workflow would retry by sending the caller's message upstream a second time (spec
// §6.4: retries only before the first upstream task id).
func TestFirstIDRecordedAgainAfterAStoreFailure(t *testing.T) {
	e := newEnv(t, envOptions{opts: ExecutorOptions{RecordBackoff: 20 * time.Millisecond}})
	id, wid := e.newTask(taskSpec{owner: buyerA, text: "q"})
	const done = `{"id":"up-1","contextId":"up-ctx","status":{"state":"TASK_STATE_COMPLETED","message":{"messageId":"m1","role":"ROLE_AGENT","parts":[{"text":"ok"}]}}}`
	var sends atomic.Int64
	b, _ := scripted(t, func(method string, rid json.RawMessage, w http.ResponseWriter, _ *http.Request) {
		if method == "SendMessage" {
			sends.Add(1)
			rpcResult(w, rid, `"result":{"task":`+done+`}`)
			return
		}
		rpcResult(w, rid, `"result":`+done)
	})
	e.backends.set(b, rpcInfo, nil)
	e.failFirstIDWrites(2)
	res := e.dispatch(context.Background(), id, 1, wid, false)
	if res.Outcome != lifecycle.DispatchTerminal {
		t.Errorf("outcome %+v, want terminal: the turn was handed back with its upstream task unrecorded", res)
	}
	if a := e.attempt(id, 1); a.UpstreamTaskID != "up-1" || a.FirstUpstreamIDAt == nil {
		t.Errorf("attempt 1 %+v, want up-1 recorded", a)
	}
	if row := e.row(id); row.State != store.StateCompleted {
		t.Errorf("state %s", row.State)
	}
	if n := sends.Load(); n != 1 {
		t.Errorf("%d sends", n)
	}
	if n := e.signals.count(); n != 1 {
		t.Errorf("%d kontra.dispatched signals", n)
	}
}

// An abort while the first upstream task id cannot be recorded ends the dispatch: it does not wait for the
// store forever.
func TestFirstIDRecordingStopsAtAbort(t *testing.T) {
	e := newEnv(t, envOptions{opts: ExecutorOptions{RecordBackoff: 20 * time.Millisecond}})
	id, wid := e.newTask(taskSpec{owner: buyerA, text: "q"})
	b, _ := scripted(t, func(method string, rid json.RawMessage, w http.ResponseWriter, _ *http.Request) {
		rpcResult(w, rid, `"result":{"task":`+upWorking+`}`)
	})
	e.backends.set(b, rpcInfo, nil)
	e.failFirstIDWrites(1 << 30)
	done := make(chan lifecycle.DispatchResult, 1)
	go func() { done <- e.dispatch(context.Background(), id, 1, wid, false) }()
	time.Sleep(300 * time.Millisecond)
	if err := e.disp.Abort(context.Background(), lifecycle.AttemptRef{TaskID: string(id), N: 1}); err != nil {
		t.Fatal(err)
	}
	select {
	case res := <-done:
		if res.Outcome != lifecycle.DispatchAborted {
			t.Errorf("outcome %+v, want aborted", res)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the dispatch did not stop at the abort")
	}
}

// An upstream task id the store refuses as data (U+0000) fails the task upstream_error at once.
func TestFirstIDTheStoreRefusesFails(t *testing.T) {
	e := newEnv(t, envOptions{opts: ExecutorOptions{RecordBackoff: 20 * time.Millisecond}})
	id, wid := e.newTask(taskSpec{owner: buyerA, text: "q"})
	b, _ := scripted(t, func(method string, rid json.RawMessage, w http.ResponseWriter, _ *http.Request) {
		rpcResult(w, rid, `"result":{"task":{"id":"up\u0000x","contextId":"c","status":{"state":"TASK_STATE_WORKING"}}}`)
	})
	e.backends.set(b, rpcInfo, nil)
	res := e.dispatch(context.Background(), id, 1, wid, false)
	if p := projection(t, e, id); res.Outcome != lifecycle.DispatchTerminal || p.Metadata[store.MetaFailure] != "upstream_error" {
		t.Errorf("outcome %+v, task %s %v", res, p.Status.State, p.Metadata)
	}
}

// beatRecorder is a Heartbeater that counts beats and, at beat number cancelAt, cancels the activity's
// context as the Temporal SDK does when the server answers a heartbeat NotFound (the activity timed out).
type beatRecorder struct {
	beats    atomic.Int64
	cancelAt int64
	cancel   context.CancelFunc
}

func (b *beatRecorder) HeartbeatInterval() time.Duration { return 5 * time.Second }

func (b *beatRecorder) Heartbeat(ctx context.Context) error {
	if n := b.beats.Add(1); n == b.cancelAt && b.cancel != nil {
		b.cancel()
	}
	return ctx.Err()
}

// A dispatch heartbeats when it starts and again right before a2a-go's handler, so the upstream call
// never comes before a heartbeat (a2a-go's own heartbeater first beats 5 s after the handler starts) and
// a dispatch Temporal already gave up on (its heartbeat timeout passed while it waited on Postgres) stops
// without calling the upstream (spec §14.2: load never looks like a dead replica; §6.4).
func TestDispatchHeartbeatsBeforeTheUpstreamCall(t *testing.T) {
	e := newEnv(t, envOptions{})
	var atSend atomic.Int64
	hb := &beatRecorder{}
	b, calls := scripted(t, func(method string, rid json.RawMessage, w http.ResponseWriter, _ *http.Request) {
		if method == "SendMessage" {
			atSend.Store(hb.beats.Load())
		}
		rpcResult(w, rid, `"result":{"task":{"id":"up-1","contextId":"c","status":{"state":"TASK_STATE_COMPLETED"}}}`)
	})
	e.backends.set(b, rpcInfo, nil)
	e.disp.Heartbeater = hb
	id, wid := e.newTask(taskSpec{owner: buyerA, text: "q"})
	if res := e.dispatch(context.Background(), id, 1, wid, false); res.Outcome != lifecycle.DispatchTerminal {
		t.Fatalf("outcome %+v", res)
	}
	if calls.Load() == 0 || atSend.Load() < 2 {
		t.Errorf("%d heartbeats before the upstream call, want the dispatch's two", atSend.Load())
	}

	for _, at := range []int64{1, 2} {
		ctx, cancel := context.WithCancel(context.Background())
		hb := &beatRecorder{cancelAt: at, cancel: cancel}
		e.disp.Heartbeater = hb
		before := calls.Load()
		id, wid := e.newTask(taskSpec{owner: buyerA, text: "q"})
		res, err := e.disp.Dispatch(ctx, lifecycle.DispatchInput{TaskID: string(id), N: 1, WorkItemID: wid.String()})
		cancel()
		if err != nil || res.Outcome != lifecycle.DispatchContextLost {
			t.Errorf("Temporal gave up at beat %d: %+v %v, want context_lost", at, res, err)
		}
		if n := calls.Load() - before; n != 0 {
			t.Errorf("Temporal gave up at beat %d: %d upstream calls", at, n)
		}
		if a := e.attempt(id, 1); a.UpstreamTaskID != "" {
			t.Errorf("attempt %+v", a)
		}
	}
}

// The reads a dispatch makes before a2a-go's handler are bounded (PreambleTimeout, well under the 30 s
// heartbeat timeout): a Postgres that does not answer them fails the dispatch before Temporal takes it for
// dead, and nothing is sent upstream.
func TestDispatchPreambleIsBounded(t *testing.T) {
	e := newEnv(t, envOptions{})
	b, calls := scripted(t, func(method string, rid json.RawMessage, w http.ResponseWriter, _ *http.Request) {
		rpcResult(w, rid, `"result":{"task":{"id":"up-1","contextId":"c","status":{"state":"TASK_STATE_COMPLETED"}}}`)
	})
	e.backends.set(b, rpcInfo, nil)
	e.disp.PreambleTimeout = 300 * time.Millisecond
	id, wid := e.newTask(taskSpec{owner: buyerA, text: "q"})
	ctx := context.Background()
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `lock table kontra.work_items in access exclusive mode`); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = e.disp.Dispatch(ctx, lifecycle.DispatchInput{TaskID: string(id), N: 1, WorkItemID: wid.String()})
	if d := time.Since(start); err == nil || d > 5*time.Second {
		t.Errorf("dispatch behind a stalled Postgres: %v after %v, want an error within the preamble's bound", err, d)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("%d upstream calls", n)
	}
}

// flakyGet is a task store whose next Get fails as a broken pooled connection does, once armed.
type flakyGet struct {
	*store.Store
	fail atomic.Bool
}

func (f *flakyGet) Get(ctx context.Context, id a2a.TaskID) (*taskstore.StoredTask, error) {
	if f.fail.CompareAndSwap(true, false) {
		return nil, errors.New("simulated: conn closed (a Postgres switchover)")
	}
	return f.Store.Get(ctx, id)
}

// a2a-go's handler reads the task (loadExecutionContext's Get) on every dispatch in cluster mode, before
// the executor runs. A read the gate's store fails (a broken connection, a statement timeout) is the gate's
// failure, not the upstream's, which was never contacted: the dispatch returns context_lost (the workflow
// retries before the first upstream task id), and the task is not failed upstream_error (spec §6.4, D42).
func TestStoreReadFailureIsALostContext(t *testing.T) {
	var flaky *flakyGet
	e := newEnv(t, envOptions{wrapTaskStore: func(s *store.Store) taskstore.Store {
		flaky = &flakyGet{Store: s}
		return flaky
	}})
	b, calls := scripted(t, func(method string, rid json.RawMessage, w http.ResponseWriter, _ *http.Request) {
		rpcResult(w, rid, `"result":{"task":{"id":"up-1","contextId":"c","status":{"state":"TASK_STATE_COMPLETED"}}}`)
	})
	e.backends.set(b, rpcInfo, nil)
	id, wid := e.newTask(taskSpec{owner: buyerA, text: "q"})
	if _, err := e.handlers.For(context.Background(), e.svc); err != nil {
		t.Fatal(err)
	}
	flaky.fail.Store(true)
	res := e.dispatch(context.Background(), id, 1, wid, false)
	if flaky.fail.Load() {
		t.Fatal("the dispatch never read the task through the store")
	}
	if res.Outcome != lifecycle.DispatchContextLost {
		t.Errorf("outcome %+v, want context_lost", res)
	}
	if p := projection(t, e, id); p.Status.State == a2a.TaskStateFailed {
		t.Errorf("the task failed %v on a failed read of the gate's store", p.Metadata[store.MetaFailure])
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("%d upstream calls", n)
	}
	// The retry (attempt 2, the same work item) then runs the turn.
	if err := e.log.BeginRetry(context.Background(), store.Attempt{TaskID: uid(t, id), N: 2, Backend: store.BackendDirect}, store.OutcomeUpstreamUnreachable); err != nil {
		t.Fatal(err)
	}
	if res := e.dispatch(context.Background(), id, 2, wid, false); res.Outcome != lifecycle.DispatchTerminal {
		t.Errorf("the retry: %+v", res)
	}
}

// A dispatch of a task that already ended (an earlier dispatch logged the terminal event and died before
// ending its attempt) ends the attempt with the task's outcome and calls nothing upstream (spec §4, D77).
func TestDispatchOfAnEndedTaskEndsItsAttempt(t *testing.T) {
	e := newEnv(t, envOptions{})
	b, calls := scripted(t, func(method string, rid json.RawMessage, w http.ResponseWriter, _ *http.Request) {
		rpcResult(w, rid, `"result":{"task":`+upWorking+`}`)
	})
	e.backends.set(b, rpcInfo, nil)
	id, wid := e.newTask(taskSpec{owner: buyerA, text: "q"})
	e.firstID(id, 1, "up-1", "up-ctx")
	if _, err := e.log.WriteTerminal(context.Background(), store.TerminalWrite{TaskID: uid(t, id), State: store.StateFailed, Reason: store.ReasonUpstreamError}); err != nil {
		t.Fatal(err)
	}
	if res := e.dispatch(context.Background(), id, 1, wid, true); res.Outcome != lifecycle.DispatchTerminal {
		t.Errorf("outcome %+v", res)
	}
	if a := e.attempt(id, 1); a.EndedAt == nil || a.Outcome != store.OutcomeUpstreamError {
		t.Errorf("attempt %+v, want ended upstream_error", a)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("%d upstream calls", n)
	}
}
