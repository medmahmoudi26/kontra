package relay

import (
	"context"
	"errors"
	"iter"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/google/uuid"
)

// messageAgent answers every message with a bare Message in the context it was given (a stateless agent).
func messageAgent() a2asrv.AgentExecutor {
	return a2asrv.AgentExecutorFunc(func(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
		return func(yield func(a2a.Event, error) bool) {
			m := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("echo: "+ec.Message.Parts[0].Text()))
			m.ContextID = ec.ContextID
			yield(m, nil)
		}
	})
}

// useAgent points the rig's service at a new agent.
func (r *resendRig) useAgent(exec a2asrv.AgentExecutor) *upstreamAgent {
	r.t.Helper()
	agent, b, info := newAgent(r.t, exec, agentConfig{streaming: true})
	info.Streaming = true
	r.backends.set(b, info, nil)
	return agent
}

// call is a blocking SendMessage bounded by 10 s, answering what the caller received.
func (r *resendRig) call(req *a2a.SendMessageRequest) (a2a.SendMessageResult, error) {
	cctx, cancel := context.WithTimeout(asCaller(buyerA), 10*time.Second)
	defer cancel()
	return r.h.SendMessage(cctx, req)
}

// lastRecord is the id of the newest record of the rig's service: the gate's own id of an interaction, which a
// message-only caller is never sent.
func (r *resendRig) lastRecord() a2a.TaskID {
	r.t.Helper()
	var id uuid.UUID
	if err := r.log.DB().QueryRow(context.Background(), `select id from kontra.tasks where tenant_id = $1 and service = $2
		order by created_at desc, id desc limit 1`, r.svc.TenantID, r.svc.Name).Scan(&id); err != nil {
		r.t.Fatal(err)
	}
	return a2a.TaskID(id.String())
}

// D362: a callee that answers with a Message is answered to the caller as that Message (the gate's contextId, no
// taskId); its record stays hidden from every caller read; a resend gets the same Message without dispatching; the
// Message's upstream context continues for the next message in the gate context (spec §4).
func TestMessagePassthrough(t *testing.T) {
	r := newResendRig(t, nil)
	agent := r.useAgent(messageAgent())

	req := resendRequest("msg-only-0001", "hi")
	res, err := r.call(req)
	if err != nil {
		t.Fatal(err)
	}
	msg, ok := res.(*a2a.Message)
	if !ok || msg.TaskID != "" || msg.ContextID == "" || msg.Role != a2a.MessageRoleAgent || msg.Parts[0].Text() != "echo: hi" {
		t.Fatalf("answer %T %+v, want the callee's Message with a gate contextId and no taskId", res, res)
	}
	record := r.lastRecord()
	// The agent's context, which its Message answered in, is recorded on the attempt (no upstream task id).
	var upCtx string
	if err := r.log.DB().QueryRow(context.Background(), `select coalesce(upstream_context_id, ''), upstream_task_id is null
		from kontra.attempts where task_id = $1 and n = 1`, uuid.MustParse(string(record))).Scan(&upCtx, new(bool)); err != nil || upCtx == "" {
		t.Fatalf("attempt 1's upstream context %q: %v", upCtx, err)
	}
	if _, err := r.h.GetTask(asCaller(buyerA), &a2a.GetTaskRequest{ID: record}); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Errorf("GetTask of the message-only record: %v, want TaskNotFound", err)
	}
	list, err := r.h.ListTasks(asCaller(buyerA), &a2a.ListTasksRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Tasks) != 0 {
		t.Errorf("ListTasks lists %d tasks, want none", len(list.Tasks))
	}
	if _, err := r.h.CancelTask(asCaller(buyerA), &a2a.CancelTaskRequest{ID: record}); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Errorf("CancelTask of the message-only record: %v, want TaskNotFound", err)
	}

	// A resend is answered the same Message, and the agent receives nothing more.
	again, err := r.call(resendRequest("msg-only-0001", "hi"))
	if err != nil {
		t.Fatal(err)
	}
	if m, ok := again.(*a2a.Message); !ok || m.ID != msg.ID || m.TaskID != "" {
		t.Fatalf("resend answered %T %+v, want the same Message", again, again)
	}
	sent := agent.sentMessages(t)
	if len(sent) != 1 {
		t.Fatalf("the agent received %d messages, want 1", len(sent))
	}

	// The next message in the gate context reaches the agent in the context its Message answered in.
	next := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("again"))
	next.ContextID = msg.ContextID
	if res, err := r.call(&a2a.SendMessageRequest{Message: next}); err != nil {
		t.Fatal(err)
	} else if m, ok := res.(*a2a.Message); !ok || m.ContextID != msg.ContextID {
		t.Fatalf("second answer %T %+v, want a Message in the same gate context", res, res)
	}
	sent = agent.sentMessages(t)
	if len(sent) != 2 || sent[1].ContextID != upCtx {
		t.Fatalf("the agent received %d messages, the second in context %q, want it in %q", len(sent), sent[len(sent)-1].ContextID, upCtx)
	}
}

// D362: a callee that answers with a task is answered the task as before; a gate failure before any callee answer
// is answered the failed task; returnImmediately is answered the submitted task at once, and a later Message ends
// that task completed carrying it (D5).
func TestTaskAnswersStay(t *testing.T) {
	r := newResendRig(t, nil)

	r.useAgent(echoAgent())
	res, err := r.call(resendRequest("task-0001", "hi"))
	if task, ok := res.(*a2a.Task); err != nil || !ok || task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("a task callee: %T %+v %v, want the completed task", res, res, err)
	}

	r.useAgent(messageAgent())
	req := resendRequest("immediate-0001", "hi")
	req.Config = &a2a.SendMessageConfig{ReturnImmediately: true}
	res, err = r.call(req)
	task, ok := res.(*a2a.Task)
	if err != nil || !ok || task.Status.State.Terminal() {
		t.Fatalf("returnImmediately: %T %+v %v, want the task at once", res, res, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		got, err := r.h.GetTask(asCaller(buyerA), &a2a.GetTaskRequest{ID: task.ID})
		if err != nil {
			t.Fatal(err)
		}
		if got.Status.State == a2a.TaskStateCompleted {
			if got.Status.Message == nil || got.Status.Message.Parts[0].Text() != "echo: hi" {
				t.Fatalf("returnImmediately's task ended %+v, want the Message as its status message", got.Status)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("returnImmediately's task is %s after 10 s", got.Status.State)
		}
		time.Sleep(50 * time.Millisecond)
	}

	r.backends.set(nil, r.backends.info, errors.New("the service is gone"))
	res, err = r.call(resendRequest("failed-0001", "hi"))
	if task, ok := res.(*a2a.Task); err != nil || !ok || task.Status.State != a2a.TaskStateFailed {
		t.Fatalf("a gate failure before any answer: %T %+v %v, want the failed task", res, res, err)
	}
}
