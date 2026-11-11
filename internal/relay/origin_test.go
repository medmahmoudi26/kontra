package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/google/uuid"

	"kontra.sh/kontra/internal/lifecycle"
	"kontra.sh/kontra/internal/store"
)

// originLabel is an event's origin as "kind actor·type[→converted]@method", "kind -" when it has none.
func originLabel(t *testing.T, ev store.Event) string {
	t.Helper()
	if ev.Origin == nil {
		return string(ev.Kind) + " -"
	}
	var o store.Origin
	if err := json.Unmarshal(ev.Origin, &o); err != nil {
		t.Fatalf("seq %d origin %s: %v", ev.Seq, ev.Origin, err)
	}
	s := string(ev.Kind) + " " + o.Actor + "·" + o.Type
	if o.ConvertedTo != "" {
		s += "→" + o.ConvertedTo
	}
	if o.Method != "" {
		s += "@" + o.Method
	}
	return s
}

func (e *env) origins(id a2a.TaskID) []string {
	var out []string
	for _, ev := range e.events(id)[1:] { // seq 1, the task row, is the work queue's (lifecycle)
		out = append(out, originLabel(e.t, ev))
	}
	return out
}

// A streamed task: the agent's first Task is logged as its status and artifact, converted; its status and artifact
// updates as themselves; each with the method that carried it and, with the traffic record on, its exchange.
func TestOriginStreamed(t *testing.T) {
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
	want := []string{
		"status callee·Task→TaskStatusUpdateEvent@SendStreamingMessage",
		"status callee·TaskStatusUpdateEvent@SendStreamingMessage",
		"artifact callee·TaskArtifactUpdateEvent@SendStreamingMessage",
		"artifact callee·TaskArtifactUpdateEvent@SendStreamingMessage",
		"status callee·TaskStatusUpdateEvent@SendStreamingMessage",
	}
	if got := e.origins(id); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("origins\n got %v\nwant %v", got, want)
	}
	exchange := e.callee(id)
	var ex string
	if err := e.pool.QueryRow(context.Background(), `select id::text from kontra.a2a_exchanges where gate_task_id = $1`, uid(t, id)).Scan(&ex); err != nil || len(exchange) != 1 {
		t.Fatalf("exchange %v (%v)", exchange, err)
	}
	for _, ev := range e.events(id)[1:] {
		var o store.Origin
		_ = json.Unmarshal(ev.Origin, &o)
		if o.ExchangeID != ex {
			t.Errorf("seq %d: exchange %q, want %s", ev.Seq, o.ExchangeID, ex)
		}
	}
}

// D5: the agent's Message is logged as a completed status, marked converted.
func TestOriginMessageConverted(t *testing.T) {
	e := newEnv(t, envOptions{})
	exec := a2asrv.AgentExecutorFunc(func(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
		return func(yield func(a2a.Event, error) bool) {
			yield(a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("stateless")), nil)
		}
	})
	_, b, info := newAgent(t, exec, agentConfig{})
	e.backends.set(b, info, nil)
	id, wid := e.newTask(taskSpec{owner: buyerA, text: "q"})
	e.dispatch(context.Background(), id, 1, wid, false)
	if got := e.origins(id); fmt.Sprint(got) != "[status callee·Message→TaskStatusUpdateEvent@SendMessage]" {
		t.Errorf("origins %v", got)
	}
}

// Polling: what GetTask shows is logged as its difference, converted from Task, with GetTask as the method.
func TestOriginPolled(t *testing.T) {
	e := newEnv(t, envOptions{})
	exec := a2asrv.AgentExecutorFunc(func(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
		return func(yield func(a2a.Event, error) bool) {
			if !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
				return
			}
			time.Sleep(150 * time.Millisecond)
			yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, nil), nil)
		}
	})
	_, b, info := newAgent(t, exec, agentConfig{})
	e.backends.set(b, info, nil)
	id, wid := e.newTask(taskSpec{owner: buyerA, text: "q"})
	e.dispatch(context.Background(), id, 1, wid, false)
	got := e.origins(id)
	if len(got) < 2 || got[0] != "status callee·Task→TaskStatusUpdateEvent@SendMessage" ||
		got[len(got)-1] != "status callee·Task→TaskStatusUpdateEvent@GetTask" {
		t.Errorf("origins %v", got)
	}
}

// What the gate writes itself is the gate's: a failure through write_terminal and a reconnect wait; a completed state
// relayed from the agent's answer to a forwarded CancelTask is the callee's Task, converted.
func TestOriginGateWrites(t *testing.T) {
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	id, _ := e.newTask(taskSpec{owner: buyerA, text: "q"})
	since := time.Now()
	if _, err := e.log.LogReconnectWait(ctx, uid(t, id), since, since.Add(15*time.Minute), "waiting"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.log.WriteTerminal(ctx, store.TerminalWrite{TaskID: uid(t, id), State: store.StateFailed,
		Reason: store.ReasonUpstreamError, Text: "no"}); err != nil {
		t.Fatal(err)
	}
	if got := e.origins(id); fmt.Sprint(got) != "[status gate·TaskStatusUpdateEvent status gate·TaskStatusUpdateEvent]" {
		t.Errorf("origins %v", got)
	}
	id2, _ := e.newTask(taskSpec{owner: buyerA, text: "q"})
	if _, err := e.log.WriteTerminal(ctx, store.TerminalWrite{TaskID: uid(t, id2), State: store.StateCompleted, Text: "the agent ended it"}); err != nil {
		t.Fatal(err)
	}
	if got := e.origins(id2); fmt.Sprint(got) != "[status callee·Task→TaskStatusUpdateEvent@CancelTask]" {
		t.Errorf("origins %v", got)
	}
}

// A follow-up turn: a2a-go logs the caller's message with the origin its work item carries; the agent's answer is
// the callee's.
func TestOriginFollowUp(t *testing.T) {
	e := newEnv(t, envOptions{})
	_, b, info := newAgent(t, askingAgent(), agentConfig{streaming: true})
	info.Streaming = true
	e.backends.set(b, info, nil)
	id, wid := e.newTask(taskSpec{owner: buyerA, text: "book a room"})
	if res := e.dispatch(context.Background(), id, 1, wid, false); res.Outcome != lifecycle.DispatchInterrupted {
		t.Fatalf("turn 1: %+v", res)
	}
	wid2 := e.followUp(id, buyerA, "the blue one")
	ex := uuid.Must(uuid.NewV7()).String()
	e.exec(`update kontra.work_items set payload = jsonb_set(payload::jsonb, '{origin}', $2::jsonb)::json where id = $1`, wid2,
		`{"actor":"caller","type":"SendStreamingMessage","method":"SendStreamingMessage","exchange_id":"`+ex+`"}`)
	if res := e.dispatch(context.Background(), id, 1, wid2, false); res.Outcome != lifecycle.DispatchTerminal {
		t.Fatalf("turn 2: %+v", res)
	}
	got := strings.Join(e.origins(id), "\n")
	if !strings.Contains(got, "message caller·SendStreamingMessage@SendStreamingMessage") {
		t.Errorf("origins:\n%s", got)
	}
	for _, ev := range e.events(id) {
		if ev.Kind == store.KindMessage && !strings.Contains(string(ev.Origin), ex) {
			t.Errorf("the follow-up's message names no exchange: %s", ev.Origin)
		}
	}
	if strings.Contains(got, " -") {
		t.Errorf("an event without origin:\n%s", got)
	}
}
