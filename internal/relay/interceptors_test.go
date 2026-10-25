package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aevent"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	a2apush "github.com/a2aproject/a2a-go/v2/a2asrv/push"
	"github.com/google/uuid"

	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/hooks"
	"kontra.sh/kontra/internal/lifecycle"
	"kontra.sh/kontra/internal/ssrf"
	"kontra.sh/kontra/internal/store"
)

func (e *env) handler() a2asrv.RequestHandler {
	e.t.Helper()
	sh, err := e.handlers.For(context.Background(), e.svc)
	if err != nil {
		e.t.Fatal(err)
	}
	return sh.Handler
}

// setState logs a status event moving the task to st, as a relay or the workflow would.
func (e *env) setState(id a2a.TaskID, st a2a.TaskState) {
	e.t.Helper()
	row := e.row(id)
	ev := a2a.NewStatusUpdateEvent(row.Snapshot, st, nil)
	next, err := a2aevent.ApplyShallowUpdate(row.Snapshot, ev)
	if err != nil {
		e.t.Fatal(err)
	}
	payload, _ := json.Marshal(a2a.StreamResponse{Event: ev})
	state, _ := store.StateFromA2A(st)
	if _, err := e.log.Append(context.Background(), nil, store.Write{TaskID: uid(e.t, id), PrevVersion: row.Version,
		Kind: store.KindStatus, State: state, Snapshot: next, Payload: payload}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) countRows(table string) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), "select count(*) from kontra."+table).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// recordingAdmission records what Admission was asked and answers with a fixed decision.
type recordingAdmission struct {
	mu       sync.Mutex
	got      []hooks.AdmitRequest
	decision hooks.Decision
	err      error
}

func (r *recordingAdmission) Admit(_ context.Context, req hooks.AdmitRequest) (hooks.Decision, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, req)
	if r.decision.Outcome == "" && r.err == nil {
		return hooks.Decision{Outcome: hooks.Allow}, nil
	}
	return r.decision, r.err
}

func echoAgent() a2asrv.AgentExecutor {
	return a2asrv.AgentExecutorFunc(func(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
		return func(yield func(a2a.Event, error) bool) {
			if !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
				return
			}
			yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, a2a.NewMessageForTask(a2a.MessageRoleAgent, ec, a2a.NewTextPart("echo: "+ec.Message.Parts[0].Text()))), nil)
		}
	})
}

// A new task end to end through a2a-go's handler: identity, caller keys stripped (sealing keys and
// caller_metadata kept), Admission asked with the pre-minted task id the work queue then uses, the relay
// dispatched, the caller answered with the completed gate task.
func TestNewTaskThroughTheHandler(t *testing.T) {
	adm := &recordingAdmission{}
	e := newEnv(t, envOptions{admit: adm, rules: hooks.KeyRules{CallerMetadata: []string{"kontra.escrow.dispute"}}})
	agent, b, info := newAgent(t, echoAgent(), agentConfig{})
	e.backends.set(b, info, nil)
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi"))
	msg.Metadata = map[string]any{"kontra.approve.url": "https://evil", "kontra.sealed.replyTo": "k1_x", "kontra.escrow.dispute": true, "note": "kept"}
	msg.Parts[0].Metadata = map[string]any{"kontra.failure": "spoof", "p": 1}
	res, err := e.handler().SendMessage(asCaller(buyerA), &a2a.SendMessageRequest{Message: msg, Metadata: map[string]any{"kontra.x": 1, "r": 2}})
	if err != nil {
		t.Fatal(err)
	}
	task, ok := res.(*a2a.Task)
	if !ok || task.Status.State != a2a.TaskStateCompleted || task.Status.Message.Parts[0].Text() != "echo: hi" {
		t.Fatalf("%T %+v", res, res)
	}
	e.queue.wait(t)
	adm.mu.Lock()
	if len(adm.got) != 1 || adm.got[0].TaskID != string(task.ID) || adm.got[0].Operation != hooks.OpTask ||
		adm.got[0].Principal.ID != buyerA.Principal || len(adm.got[0].Message.Parts) != 1 || adm.got[0].Message.Parts[0].Size != 2 {
		t.Errorf("admission %+v (task %s)", adm.got, task.ID)
	}
	adm.mu.Unlock()
	// What was stored and what went upstream carry no caller-written kontra. key but the allowed ones.
	row := e.row(task.ID)
	stored := row.Snapshot.History[0]
	for _, m := range []*a2a.Message{stored, agent.sentMessages(t)[0]} {
		md := m.Metadata
		if md["kontra.approve.url"] != nil || md["kontra.sealed.replyTo"] != "k1_x" || md["kontra.escrow.dispute"] != true || md["note"] != "kept" {
			t.Errorf("message metadata %v", md)
		}
		if pm := m.Parts[0].Metadata; pm["kontra.failure"] != nil || pm["p"] == nil {
			t.Errorf("part metadata %v", pm)
		}
	}
	if row.Owner.BuyerID != buyerA.BuyerID || row.Owner.Principal != buyerA.Principal {
		t.Errorf("owner %+v", row.Owner)
	}
}

// §7's follow-up table, in the interceptor: only input-required takes a message; the rest are refused
// before a2a-go or the work queue sees them.
func TestFollowUpTable(t *testing.T) {
	e := newEnv(t, envOptions{})
	_, b, info := newAgent(t, askingAgent(), agentConfig{})
	e.backends.set(b, info, nil)
	h := e.handler()
	follow := func(id a2a.TaskID, ctxID string, o store.Owner) error {
		m := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("more"))
		m.TaskID, m.ContextID = id, ctxID
		_, err := h.SendMessage(asCaller(o), &a2a.SendMessageRequest{Message: m})
		return err
	}
	for _, tc := range []struct {
		state a2a.TaskState
		want  error
	}{
		{"", a2a.ErrUnsupportedOperation}, // submitted
		{a2a.TaskStateWorking, a2a.ErrUnsupportedOperation},
		{a2a.TaskStateAuthRequired, a2a.ErrUnsupportedOperation},
		{a2a.TaskStateCompleted, a2a.ErrUnsupportedOperation}, // no review window in open source
		{a2a.TaskStateFailed, a2a.ErrUnsupportedOperation},
		{a2a.TaskStateCanceled, a2a.ErrUnsupportedOperation},
		{a2a.TaskStateRejected, a2a.ErrUnsupportedOperation},
	} {
		id, _ := e.newTask(taskSpec{owner: buyerA, text: "q"})
		if tc.state != "" {
			e.setState(id, tc.state)
		}
		before := len(e.queue.written())
		if err := follow(id, "", buyerA); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", tc.state, err, tc.want)
		}
		if len(e.queue.written()) != before {
			t.Errorf("%s: the refused follow-up reached the work queue", tc.state)
		}
	}
	// quoted (only an Admission quote creates it).
	qid := uuid.Must(uuid.NewV7())
	exp := time.Now().Add(time.Hour)
	qsnap := &a2a.Task{ID: a2a.TaskID(qid.String()), ContextID: "qc", Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired}}
	if _, err := e.log.CreateTask(context.Background(), nil, store.NewTask{ID: qid, Service: e.svc, Protocol: store.ProtocolA2A, Owner: buyerA,
		ContextID: "qc", State: store.StateQuoted, Snapshot: qsnap, QuoteExpiresAt: &exp}); err != nil {
		t.Fatal(err)
	}
	if err := follow(a2a.TaskID(qid.String()), "", buyerA); !errors.Is(err, a2a.ErrUnsupportedOperation) {
		t.Errorf("quoted: %v", err)
	}
	// Disagreeing ids, a malformed id, another buyer's task.
	id, _ := e.newTask(taskSpec{owner: buyerA, contextID: "c-1", text: "q"})
	e.setState(id, a2a.TaskStateInputRequired)
	if err := follow(id, "c-2", buyerA); !errors.Is(err, a2a.ErrInvalidParams) {
		t.Errorf("taskId/contextId disagree: %v", err)
	}
	if err := follow("not-a-task", "", buyerA); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Errorf("malformed id: %v", err)
	}
	if err := follow(id, "", buyerB); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Errorf("another buyer's task: %v", err)
	}
}

// A follow-up turn through the whole stack: input-required, then the caller's answer goes to the same
// upstream task and the turn completes; a second follow-up to the completed task is refused.
func TestFollowUpThroughTheHandler(t *testing.T) {
	e := newEnv(t, envOptions{})
	agent, b, info := newAgent(t, askingAgent(), agentConfig{streaming: true})
	info.Streaming = true
	e.backends.set(b, info, nil)
	h := e.handler()
	res, err := h.SendMessage(asCaller(buyerA), &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("book"))})
	if err != nil {
		t.Fatal(err)
	}
	task := res.(*a2a.Task)
	if task.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("turn 1 %s", task.Status.State)
	}
	if d := e.queue.wait(t); d.res.Outcome != lifecycle.DispatchInterrupted {
		t.Fatalf("turn 1 dispatch %+v", d.res)
	}
	m := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("blue"))
	m.TaskID = task.ID
	res, err = h.SendMessage(asCaller(buyerA), &a2a.SendMessageRequest{Message: m})
	if err != nil {
		t.Fatal(err)
	}
	if final := res.(*a2a.Task); final.Status.State != a2a.TaskStateCompleted || final.Status.Message.Parts[0].Text() != "done: blue" {
		t.Fatalf("turn 2 %+v", final.Status)
	}
	e.queue.wait(t)
	a := e.attempt(task.ID, 1)
	sent := agent.sentMessages(t)
	if len(sent) != 2 || sent[1].TaskID != a2a.TaskID(a.UpstreamTaskID) || sent[1].ContextID != a.UpstreamContextID {
		t.Errorf("follow-up upstream %+v", sent)
	}
	m2 := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("again"))
	m2.TaskID = task.ID
	if _, err := h.SendMessage(asCaller(buyerA), &a2a.SendMessageRequest{Message: m2}); !errors.Is(err, a2a.ErrUnsupportedOperation) {
		t.Errorf("a message to a completed task: %v", err)
	}
}

// The anonymous principal shares one buyer: ListTasks and push configs are refused (spec §2.3).
func TestAnonymousRefusals(t *testing.T) {
	e := newEnv(t, envOptions{push: a2apush.NewInMemoryStore()})
	h := e.handler()
	anon := auth.WithPrincipal(context.Background(), auth.Anonymous(uuid.Must(uuid.NewV7())))
	id, _ := e.newTask(taskSpec{owner: buyerA, text: "q"})
	if _, err := h.ListTasks(anon, &a2a.ListTasksRequest{}); !errors.Is(err, a2a.ErrUnsupportedOperation) {
		t.Errorf("ListTasks: %v", err)
	}
	if _, err := h.CreateTaskPushConfig(anon, &a2a.PushConfig{TaskID: id, URL: "https://hooks.example/x"}); !errors.Is(err, a2a.ErrUnsupportedOperation) {
		t.Errorf("CreateTaskPushConfig: %v", err)
	}
	if _, err := h.ListTaskPushConfigs(anon, &a2a.ListTaskPushConfigRequest{TaskID: id}); !errors.Is(err, a2a.ErrUnsupportedOperation) {
		t.Errorf("ListTaskPushConfigs: %v", err)
	}
	if _, err := h.GetTaskPushConfig(anon, &a2a.GetTaskPushConfigRequest{TaskID: id, ID: "x"}); !errors.Is(err, a2a.ErrUnsupportedOperation) {
		t.Errorf("GetTaskPushConfig: %v", err)
	}
	if err := h.DeleteTaskPushConfig(anon, &a2a.DeleteTaskPushConfigRequest{TaskID: id, ID: "x"}); !errors.Is(err, a2a.ErrUnsupportedOperation) {
		t.Errorf("DeleteTaskPushConfig: %v", err)
	}
	inline := &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("q")),
		Config: &a2a.SendMessageConfig{PushConfig: &a2a.PushConfig{URL: "https://hooks.example/x"}}}
	if _, err := h.SendMessage(anon, inline); !errors.Is(err, a2a.ErrUnsupportedOperation) {
		t.Errorf("inline push config: %v", err)
	}
	// An authenticated caller lists its own tasks.
	if res, err := h.ListTasks(asCaller(buyerA), &a2a.ListTasksRequest{}); err != nil || len(res.Tasks) != 1 {
		t.Errorf("buyer A's ListTasks: %+v %v", res, err)
	}
}

// An inline push config to a refused URL is a synchronous InvalidParams, and nothing is stored (§10.1).
func TestInlinePushURLRefused(t *testing.T) {
	e := newEnv(t, envOptions{push: a2apush.NewInMemoryStore()})
	h := e.handler()
	for _, u := range []string{"https://10.0.0.1/hook", "https://169.254.169.254/latest", "http://hooks.example/x", "https://[::ffff:10.0.0.1]/h", "not a url"} {
		req := &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("q")),
			Config: &a2a.SendMessageConfig{PushConfig: &a2a.PushConfig{URL: u}}}
		if _, err := h.SendMessage(asCaller(buyerA), req); !errors.Is(err, a2a.ErrInvalidParams) {
			t.Errorf("%s: %v", u, err)
		}
	}
	if n, w := e.countRows("tasks"), e.countRows("work_items"); n != 0 || w != 0 || len(e.queue.written()) != 0 {
		t.Errorf("a refused push config stored %d tasks, %d work items", n, w)
	}
	id, _ := e.newTask(taskSpec{owner: buyerA, text: "q"})
	if _, err := h.CreateTaskPushConfig(asCaller(buyerA), &a2a.PushConfig{TaskID: id, URL: "https://10.0.0.1/hook"}); !errors.Is(err, a2a.ErrInvalidParams) {
		t.Errorf("CreateTaskPushConfig to 10.0.0.1: %v", err)
	}
}

// Caller limits are refused before any task row (spec §4, §10.4).
func TestCallerLimits(t *testing.T) {
	e := newEnv(t, envOptions{})
	h := e.handler()
	big := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(strings.Repeat("x", 256<<10+1)))
	if _, err := h.SendMessage(asCaller(buyerA), &a2a.SendMessageRequest{Message: big}); !errors.Is(err, a2a.ErrInvalidParams) ||
		!strings.Contains(err.Error(), "part too large: configure blobs") {
		t.Errorf("a part over 256 KiB: %v", err)
	}
	long := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("q"))
	long.ContextID = strings.Repeat("c", 257)
	if _, err := h.SendMessage(asCaller(buyerA), &a2a.SendMessageRequest{Message: long}); !errors.Is(err, a2a.ErrInvalidParams) {
		t.Errorf("a contextId over 256 characters: %v", err)
	}
	if e.countRows("tasks") != 0 {
		t.Error("a refused request created a task")
	}
	if _, err := h.GetExtendedAgentCard(asCaller(buyerA), &a2a.GetExtendedAgentCardRequest{}); !errors.Is(err, a2a.ErrUnsupportedOperation) {
		t.Errorf("GetExtendedAgentCard: %v", err)
	}
	enrollOnly := principal(buyerA)
	enrollOnly.Scopes = []auth.Scope{auth.ScopeEnrollAll}
	if _, err := h.GetTask(auth.WithPrincipal(context.Background(), enrollOnly), &a2a.GetTaskRequest{ID: "x"}); !errors.Is(err, a2a.ErrUnauthorized) {
		t.Errorf("a key without hire: %v", err)
	}
	if _, err := h.GetTask(context.Background(), &a2a.GetTaskRequest{ID: "x"}); !errors.Is(err, a2a.ErrUnauthenticated) {
		t.Errorf("no principal: %v", err)
	}
}

// Admission decides before any task row: a deny carries the operator's refusal, a failure fails closed.
func TestAdmissionRefusals(t *testing.T) {
	adm := &recordingAdmission{decision: hooks.Decision{Outcome: hooks.Deny, Refusal: &hooks.Refusal{Code: -32602, Message: "over budget"}}}
	e := newEnv(t, envOptions{admit: adm, rules: hooks.KeyRules{OperatorPrefixes: []string{"acme."}}})
	h := e.handler()
	send := func() error {
		_, err := h.SendMessage(asCaller(buyerA), &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("q"))})
		return err
	}
	if err := send(); !errors.Is(err, a2a.ErrInvalidParams) || !strings.Contains(err.Error(), "over budget") {
		t.Errorf("deny: %v", err)
	}
	adm.decision, adm.err = hooks.Decision{}, errors.New("hook down")
	if err := send(); !errors.Is(err, a2a.ErrInternalError) || !strings.Contains(err.Error(), "admission unavailable") {
		t.Errorf("admission error: %v", err)
	}
	adm.decision, adm.err = hooks.Decision{Outcome: hooks.Allow, Metadata: map[string]any{"kontra.approve.url": "x"}}, nil
	if err := send(); !errors.Is(err, a2a.ErrInternalError) {
		t.Errorf("a decision with a forbidden key: %v", err)
	}
	if e.countRows("tasks") != 0 {
		t.Error("a refused admission created a task")
	}
}

// fakeCanceller is the kontra.cancel Update with a scripted answer.
type fakeCanceller struct {
	mu    sync.Mutex
	res   lifecycle.CancelResult
	ok    bool
	err   error
	calls int
	apply func(a2a.TaskID) // what the workflow did before answering
}

func (f *fakeCanceller) Cancel(_ context.Context, id a2a.TaskID) (lifecycle.CancelResult, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.apply != nil {
		f.apply(id)
	}
	return f.res, f.ok, f.err
}

// CancelInterceptor maps each kontra.cancel outcome to §8's answer.
func TestCancelInterceptor(t *testing.T) {
	fc := &fakeCanceller{}
	e := newEnv(t, envOptions{canceler: fc})
	h := e.handler()
	cancel := func(id a2a.TaskID, o store.Owner) (*a2a.Task, error) {
		return h.CancelTask(asCaller(o), &a2a.CancelTaskRequest{ID: id})
	}
	// Terminal: TaskNotCancelable, without asking the workflow.
	id, _ := e.newTask(taskSpec{owner: buyerA, text: "q"})
	e.setState(id, a2a.TaskStateCompleted)
	if _, err := cancel(id, buyerA); !errors.Is(err, a2a.ErrTaskNotCancelable) || fc.calls != 0 {
		t.Errorf("terminal: %v (calls %d)", err, fc.calls)
	}
	// Canceled: the stored canceled task.
	id, _ = e.newTask(taskSpec{owner: buyerA, text: "q"})
	fc.res, fc.ok, fc.err = lifecycle.CancelResult{Outcome: lifecycle.CancelCanceled}, true, nil
	fc.apply = func(id a2a.TaskID) {
		if _, err := e.log.WriteTerminal(context.Background(), store.TerminalWrite{TaskID: uid(t, id), State: store.StateCanceled, Canceled: store.CanceledClient}); err != nil {
			t.Error(err)
		}
	}
	if task, err := cancel(id, buyerA); err != nil || task.Status.State != a2a.TaskStateCanceled {
		t.Errorf("canceled: %+v %v", task, err)
	}
	fc.apply = nil
	// The upstream refuses: its error, and the task continues.
	id, _ = e.newTask(taskSpec{owner: buyerA, text: "q"})
	e.setState(id, a2a.TaskStateWorking)
	fc.res = lifecycle.CancelResult{Outcome: lifecycle.CancelNotCancelable, UpstreamError: &lifecycle.UpstreamError{Code: -32002, Message: "too late"}}
	if _, err := cancel(id, buyerA); !errors.Is(err, a2a.ErrTaskNotCancelable) || !strings.Contains(err.Error(), "too late") {
		t.Errorf("not cancelable: %v", err)
	}
	fc.res = lifecycle.CancelResult{Outcome: lifecycle.CancelUnsupported, UpstreamError: &lifecycle.UpstreamError{Code: -32004, Message: "no cancel here"}}
	if _, err := cancel(id, buyerA); !errors.Is(err, a2a.ErrUnsupportedOperation) || !strings.Contains(err.Error(), "no cancel here") {
		t.Errorf("unsupported: %v", err)
	}
	if e.row(id).State != store.StateWorking {
		t.Error("a refused cancel changed the task")
	}
	// Past 60 s: the stored task as it is.
	fc.res, fc.ok = lifecycle.CancelResult{}, false
	if task, err := cancel(id, buyerA); err != nil || task.Status.State != a2a.TaskStateWorking {
		t.Errorf("not completed in time: %+v %v", task, err)
	}
	// Not accepted: -32603; refused by the validator as terminal: TaskNotCancelable.
	fc.ok, fc.err = true, errors.New("update not accepted within 10s")
	if _, err := cancel(id, buyerA); !errors.Is(err, a2a.ErrInternalError) || errors.Is(err, lifecycle.ErrUnavailable) {
		t.Errorf("not accepted: %v", err)
	}
	// The lifecycle's own "not accepted in time" keeps its sentinel, so HTTP+JSON answers 503 (spec §14.1).
	fc.err = fmt.Errorf("update: %w", lifecycle.ErrUnavailable)
	if _, err := cancel(id, buyerA); !errors.Is(err, a2a.ErrInternalError) || !errors.Is(err, lifecycle.ErrUnavailable) ||
		err.Error() != "the cancel was not accepted in time" {
		t.Errorf("not accepted, unavailable: %v", err)
	}
	fc.err = fmt.Errorf("refused: %w", a2a.ErrTaskNotCancelable)
	if _, err := cancel(id, buyerA); !errors.Is(err, a2a.ErrTaskNotCancelable) {
		t.Errorf("refused: %v", err)
	}
	// Another buyer's task does not exist for them.
	if _, err := cancel(id, buyerB); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Errorf("another buyer: %v", err)
	}
}

// fakeReviews is the kontra.review_message Update; when it accepts it appends the message as the
// workflow's handler does.
type fakeReviews struct {
	e      *env
	mode   string // accept | refuse | reject | timeout
	got    []lifecycle.ReviewMessageUpdate
	taskID a2a.TaskID
}

func (f *fakeReviews) ReviewMessage(ctx context.Context, id a2a.TaskID, u lifecycle.ReviewMessageUpdate) (lifecycle.ReviewMessageResult, error) {
	f.got = append(f.got, u)
	switch f.mode {
	case "refuse":
		return lifecycle.ReviewMessageResult{}, fmt.Errorf("validator: %w", a2a.ErrUnsupportedOperation)
	case "reject":
		return lifecycle.ReviewMessageResult{Accepted: false}, nil
	case "timeout":
		return lifecycle.ReviewMessageResult{}, context.DeadlineExceeded
	case "unavailable":
		return lifecycle.ReviewMessageResult{}, fmt.Errorf("update: %w", lifecycle.ErrUnavailable)
	}
	seq, err := f.e.log.AppendReviewMessage(ctx, uid(f.e.t, id), uuid.MustParse(u.WorkItemID))
	if err != nil {
		return lifecycle.ReviewMessageResult{}, err
	}
	return lifecycle.ReviewMessageResult{Accepted: true, Seq: &seq}, nil
}

// A message to a completed task is offered to the review window: accepted, it is appended and the caller
// gets the task; refused, its work item is deleted; not accepted in time, the item is kept (spec §7, D66).
// It never goes upstream.
func TestReviewWindowMessages(t *testing.T) {
	fr := &fakeReviews{}
	e := newEnv(t, envOptions{reviews: fr, rules: hooks.KeyRules{CallerMetadata: []string{"kontra.escrow.dispute"}}})
	fr.e = e
	h := e.handler()
	id, _ := e.newTask(taskSpec{owner: buyerA, text: "q"})
	e.setState(id, a2a.TaskStateCompleted)
	items := func() int {
		var n int
		_ = e.pool.QueryRow(context.Background(), `select count(*) from kontra.work_items where task_id = $1`, uid(t, id)).Scan(&n)
		return n
	}
	base := items()
	send := func(text string) (a2a.SendMessageResult, error) {
		m := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text))
		m.TaskID = id
		m.Metadata = map[string]any{"kontra.escrow.dispute": true, "kontra.approve.url": "x"}
		return h.SendMessage(asCaller(buyerA), &a2a.SendMessageRequest{Message: m})
	}
	fr.mode = "accept"
	res, err := send("this is not what I ordered")
	if err != nil {
		t.Fatal(err)
	}
	task := res.(*a2a.Task)
	last := task.History[len(task.History)-1]
	if task.Status.State != a2a.TaskStateCompleted || last.Parts[0].Text() != "this is not what I ordered" {
		t.Errorf("accepted: %+v", task)
	}
	if md := fr.got[0].Metadata; len(md) != 1 || md["kontra.escrow.dispute"] != true {
		t.Errorf("the hook got metadata %v (caller_metadata keys only)", md)
	}
	fr.mode = "refuse"
	if _, err := send("again"); !errors.Is(err, a2a.ErrUnsupportedOperation) || items() != base+1 {
		t.Errorf("refused: %v, items %d", err, items())
	}
	fr.mode = "reject"
	if _, err := send("again"); !errors.Is(err, a2a.ErrUnsupportedOperation) || items() != base+1 {
		t.Errorf("rejected: %v, items %d", err, items())
	}
	fr.mode = "timeout"
	if _, err := send("again"); !errors.Is(err, a2a.ErrInternalError) || items() != base+2 {
		t.Errorf("not accepted in time: %v, items %d", err, items())
	}
	fr.mode = "unavailable"
	if _, err := send("again"); !errors.Is(err, a2a.ErrInternalError) || !errors.Is(err, lifecycle.ErrUnavailable) || items() != base+3 {
		t.Errorf("not accepted in time (lifecycle unavailable): %v, items %d", err, items())
	}
	if len(e.queue.written()) != 0 {
		t.Error("a review-window message reached the work queue")
	}
}

// The a2a-go race of P15 through the gate: the turn ends at input-required before the blocking
// SendMessage's subscription reads the store, and the caller still gets the task, new or follow-up.
func TestFinishedTurnAnswersTheTask(t *testing.T) {
	e := newEnv(t, envOptions{})
	_, b, info := newAgent(t, askingAgent(), agentConfig{})
	e.backends.set(b, info, nil)
	e.queue.sync = true
	h := e.handler()
	res, err := h.SendMessage(asCaller(buyerA), &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("book"))})
	if err != nil {
		t.Fatalf("new task: %v", err)
	}
	task := res.(*a2a.Task)
	if task.Status.State != a2a.TaskStateInputRequired || task.Status.Message.Parts[0].Text() != "which one?" {
		t.Fatalf("new task %+v", task.Status)
	}
	e.queue.wait(t)
	// A turn that ends terminal needs no help, and a follow-up that ends input-required again is answered.
	m := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("blue"))
	m.TaskID = task.ID
	res, err = h.SendMessage(asCaller(buyerA), &a2a.SendMessageRequest{Message: m})
	if err != nil || res.(*a2a.Task).Status.State != a2a.TaskStateCompleted {
		t.Fatalf("follow-up: %+v %v", res, err)
	}
	e.queue.wait(t)
}

// pushResolver answers the push URL checks' lookups from a table; a name in servfail fails as a resolver
// that answers SERVFAIL does.
type pushResolver struct {
	mu       sync.Mutex
	addrs    map[string][]string
	servfail map[string]bool
}

func (r *pushResolver) set(host string, addrs ...string) {
	r.mu.Lock()
	r.addrs[host] = addrs
	r.mu.Unlock()
}

func (r *pushResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.servfail[host] {
		return nil, &net.DNSError{Err: "server misbehaving", Name: host, Server: "10.0.0.53:53", IsTemporary: true}
	}
	var out []netip.Addr
	for _, a := range r.addrs[host] {
		out = append(out, netip.MustParseAddr(a))
	}
	if len(out) == 0 {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	return out, nil
}

// A follow-up that sends its inline push config again, unchanged, is not refused for a DNS change: the URL
// was checked when it was set (and every delivery checks every address when it dials). A URL new for the
// task, and a new task's inline config, are checked; a lookup that fails for a reason of the gate's own
// (SERVFAIL) is an internal error, never the caller's InvalidParams (spec §10.1, §10.3).
func TestFollowUpInlinePushConfigNotResolvedAgain(t *testing.T) {
	res := &pushResolver{addrs: map[string][]string{"hooks.example": {"93.184.215.14"}}, servfail: map[string]bool{"flaky.example": true}}
	e := newEnv(t, envOptions{policy: ssrf.Policy{Resolver: res}})
	id, _ := e.newTask(taskSpec{owner: buyerA, text: "q"})
	e.setState(id, a2a.TaskStateInputRequired)
	// The config set with the task, as the work queue's inline save stored it.
	e.exec(`insert into kontra.push_configs (task_id, id, config) values ($1, 'c1', $2)`, uid(t, id),
		`{"url": "https://hooks.example/cb", "token": "tok"}`)
	res.set("hooks.example", "10.0.0.7") // the name resolves privately now
	check := func(taskID a2a.TaskID, cfg *a2a.PushConfig) error {
		m := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("more"))
		m.TaskID = taskID
		req := &a2asrv.Request{Payload: &a2a.SendMessageRequest{Message: m, Config: &a2a.SendMessageConfig{PushConfig: cfg}}}
		_, _, err := e.intc.pushURLs(asCaller(buyerA), nil, req)
		return err
	}
	if err := check(id, &a2a.PushConfig{URL: "https://hooks.example/cb", Token: "tok"}); err != nil {
		t.Errorf("a follow-up's unchanged inline config: %v", err)
	}
	if err := check(id, &a2a.PushConfig{URL: "https://hooks.example/other"}); !errors.Is(err, a2a.ErrInvalidParams) {
		t.Errorf("a URL new for the task: %v, want InvalidParams", err)
	}
	if err := check("", &a2a.PushConfig{URL: "https://hooks.example/cb", Token: "tok"}); !errors.Is(err, a2a.ErrInvalidParams) {
		t.Errorf("a new task's inline config: %v, want InvalidParams", err)
	}
	for _, taskID := range []a2a.TaskID{id, ""} {
		err := check(taskID, &a2a.PushConfig{URL: "https://flaky.example/cb"})
		if !errors.Is(err, a2a.ErrInternalError) || errors.Is(err, a2a.ErrInvalidParams) {
			t.Errorf("a lookup that failed for the gate's own reason (task %q): %v, want an internal error", taskID, err)
		}
		if err != nil && strings.Contains(err.Error(), "10.0.0.53") {
			t.Errorf("the error names the resolver: %v", err)
		}
	}
	// CreateTaskPushNotificationConfig is a config being set: checked.
	if _, _, err := e.intc.pushURLs(asCaller(buyerA), nil, &a2asrv.Request{Payload: &a2a.PushConfig{TaskID: id, URL: "https://hooks.example/cb"}}); !errors.Is(err, a2a.ErrInvalidParams) {
		t.Errorf("CreateTaskPushNotificationConfig to a private name: %v", err)
	}
}

// A message to a completed task (the review window) keeps no inline push config: its webhook credentials
// never reach work_items, which hold no credentials (0001), and the message is not refused for it.
func TestReviewMessageStoresNoPushCredentials(t *testing.T) {
	fr := &fakeReviews{mode: "accept"}
	e := newEnv(t, envOptions{reviews: fr})
	fr.e = e
	id, _ := e.newTask(taskSpec{owner: buyerA, text: "q"})
	e.setState(id, a2a.TaskStateCompleted)
	m := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("this is not what I ordered"))
	m.TaskID = id
	const secret = "whsec-review-5ecret"
	historyLength := 5
	req := &a2a.SendMessageRequest{Message: m, Config: &a2a.SendMessageConfig{HistoryLength: &historyLength, PushConfig: &a2a.PushConfig{
		URL: "https://hooks.example/" + secret, Token: secret, Auth: &a2a.PushAuthInfo{Scheme: "Bearer", Credentials: secret}}}}
	if _, err := e.handler().SendMessage(asCaller(buyerA), req); err != nil {
		t.Fatal(err)
	}
	var payloads []string
	rows, err := e.pool.Query(context.Background(), `select payload::text from kontra.work_items where task_id = $1`, uid(t, id))
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatal(err)
		}
		payloads = append(payloads, p)
	}
	if rows.Err() != nil || len(payloads) < 2 {
		t.Fatalf("work items %d %v", len(payloads), rows.Err())
	}
	for _, p := range payloads {
		if strings.Contains(p, secret) {
			t.Fatalf("a work item stores the review message's push credentials: %s", p)
		}
	}
	if !strings.Contains(payloads[len(payloads)-1], `"historyLength":5`) {
		t.Errorf("the rest of the request's configuration is kept: %s", payloads[len(payloads)-1])
	}
}
