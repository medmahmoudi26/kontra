package relay

import (
	"context"
	"fmt"
	"iter"
	"net"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"kontra.sh/kontra/internal/lifecycle"
	"kontra.sh/kontra/internal/traffic"
	"kontra.sh/kontra/internal/upstream"
)

// calleeRow is one callee-leg exchange as a2a_exchanges holds it.
type calleeRow struct {
	method, calleeTask, version, reason string
	status                              int
	request                             bool
	frames                              int
}

// recordTraffic gives the env's dispatcher a recorder writing to its database (bodies on, D355, D358); flush stops
// it once everything queued is written.
func (e *env) recordTraffic() (flush func()) {
	rec := traffic.New(e.pool, nil).KeepBodies(64 << 10)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = rec.Run(ctx); close(done) }()
	e.disp.Traffic = rec
	stopped := false
	flush = func() {
		if !stopped {
			stopped = true
			cancel()
			<-done
		}
	}
	e.t.Cleanup(flush)
	return flush
}

// callee is the callee-leg exchanges of a gate task, in order.
func (e *env) callee(id a2a.TaskID) []calleeRow {
	e.t.Helper()
	rows, err := e.pool.Query(context.Background(), `select method, coalesce(callee_task_id, ''), coalesce(a2a_version, ''),
		coalesce(reason, ''), http_status, request_body is not null,
		(select count(*) from kontra.a2a_exchange_frames f where f.exchange_id = x.id)
		from kontra.a2a_exchanges x where leg = 'callee' and gate_task_id = $1 and task_id is null order by at, id`, uid(e.t, id))
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []calleeRow
	for rows.Next() {
		var r calleeRow
		var status int16
		if err := rows.Scan(&r.method, &r.calleeTask, &r.version, &r.reason, &status, &r.request, &r.frames); err != nil {
			e.t.Fatal(err)
		}
		r.status = int(status)
		out = append(out, r)
	}
	return out
}

func methods(rows []calleeRow) []string {
	var out []string
	for _, r := range rows {
		out = append(out, r.method)
	}
	return out
}

// A streaming callee: the dispatch's one SendStreamingMessage is one callee-leg exchange naming the agent's own task,
// with its request and its frames.
func TestCalleeLegStreaming(t *testing.T) {
	e := newEnv(t, envOptions{})
	flush := e.recordTraffic()
	_, b, info := newAgent(t, spoofer(2), agentConfig{streaming: true})
	info.Streaming = true
	e.backends.set(b, info, nil)
	id, wid := e.newTask(taskSpec{owner: buyerA, text: "go"})
	if res := e.dispatch(context.Background(), id, 1, wid, false); res.Outcome != lifecycle.DispatchTerminal {
		t.Fatalf("result %+v", res)
	}
	flush()
	rows := e.callee(id)
	if len(rows) != 1 {
		t.Fatalf("callee exchanges %+v, want one SendStreamingMessage", rows)
	}
	r, a := rows[0], e.attempt(id, 1)
	if r.method != "SendStreamingMessage" || r.calleeTask != a.UpstreamTaskID || r.version != "1.0" || r.status != 200 ||
		!r.request || r.frames < 4 {
		t.Errorf("exchange %+v (upstream task %s)", r, a.UpstreamTaskID)
	}
}

// A polling callee: SendMessage, then GetTask until the task ends, each an exchange.
func TestCalleeLegPolling(t *testing.T) {
	e := newEnv(t, envOptions{})
	flush := e.recordTraffic()
	exec := a2asrv.AgentExecutorFunc(func(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
		return func(yield func(a2a.Event, error) bool) {
			if !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
				return
			}
			time.Sleep(200 * time.Millisecond)
			yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, nil), nil)
		}
	})
	_, b, info := newAgent(t, exec, agentConfig{})
	e.backends.set(b, info, nil)
	id, wid := e.newTask(taskSpec{owner: buyerA, text: "hi"})
	if res := e.dispatch(context.Background(), id, 1, wid, false); res.Outcome != lifecycle.DispatchTerminal {
		t.Fatalf("result %+v", res)
	}
	flush()
	rows := e.callee(id)
	ms := methods(rows)
	if len(ms) < 2 || ms[0] != "SendMessage" {
		t.Fatalf("callee exchanges %v, want SendMessage then GetTask", ms)
	}
	for _, r := range rows[1:] {
		if r.method != "GetTask" || r.calleeTask != e.attempt(id, 1).UpstreamTaskID {
			t.Errorf("poll %+v", r)
		}
	}
}

// A forwarded cancel is a CancelTask exchange of the task.
func TestCalleeLegCancel(t *testing.T) {
	e := newEnv(t, envOptions{})
	flush := e.recordTraffic()
	g := &gatedStreamer{release: make(chan struct{}, 16), chunks: 3}
	_, b, info := newAgent(t, g, agentConfig{streaming: true})
	info.Streaming = true
	e.backends.set(b, info, nil)
	id, wid := e.newTask(taskSpec{owner: buyerA, text: "q"})
	done := make(chan lifecycle.DispatchResult, 1)
	go func() { done <- e.dispatch(context.Background(), id, 1, wid, false) }()
	waitFor(t, "the first upstream id", func() bool { return e.attempt(id, 1).FirstUpstreamIDAt != nil })
	if res, err := e.disp.ForwardCancel(context.Background(), lifecycle.AttemptRef{TaskID: string(id), N: 1}); err != nil || res.Outcome != lifecycle.CancelCanceled {
		t.Fatalf("%+v %v", res, err)
	}
	<-done
	flush()
	if ms := methods(e.callee(id)); fmt.Sprint(ms) != "[SendStreamingMessage CancelTask]" {
		t.Errorf("callee exchanges %v", ms)
	}
}

// An agent nobody answers for: the failed call is still an exchange, unreachable.
func TestCalleeLegUnreachable(t *testing.T) {
	e := newEnv(t, envOptions{})
	flush := e.recordTraffic()
	closed, _ := net.Listen("tcp", "127.0.0.1:0")
	url := "http://" + closed.Addr().String()
	closed.Close()
	b, _ := upstream.NewDirect(url, true)
	e.backends.set(b, rpcInfo, nil)
	id, wid := e.newTask(taskSpec{owner: buyerA, text: "q"})
	if res := e.dispatch(context.Background(), id, 1, wid, false); res.Outcome != lifecycle.DispatchRetryable {
		t.Fatalf("result %+v", res)
	}
	flush()
	rows := e.callee(id)
	if len(rows) != 1 || rows[0].method != "SendMessage" || rows[0].status != 502 || rows[0].reason != traffic.ReasonUnreachable {
		t.Fatalf("callee exchanges %+v", rows)
	}
}

// Without a recorder nothing is recorded, and the dispatch is unchanged.
func TestCalleeLegOff(t *testing.T) {
	e := newEnv(t, envOptions{})
	_, b, info := newAgent(t, spoofer(1), agentConfig{streaming: true})
	info.Streaming = true
	e.backends.set(b, info, nil)
	id, wid := e.newTask(taskSpec{owner: buyerA, text: "go"})
	if res := e.dispatch(context.Background(), id, 1, wid, false); res.Outcome != lifecycle.DispatchTerminal {
		t.Fatalf("result %+v", res)
	}
	var n int
	if err := e.pool.QueryRow(context.Background(), `select count(*) from kontra.a2a_exchanges`).Scan(&n); err != nil || n != 0 {
		t.Errorf("%d exchanges (%v) without a recorder", n, err)
	}
}
