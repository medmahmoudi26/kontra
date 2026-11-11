package store

import (
	"context"
	"errors"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
)

// provisional pre-creates a new message's record unexposed, as the work queue does for a caller awaiting the answer
// (D362).
func (e *env) provisional(o Owner, text string) *a2a.Task {
	e.t.Helper()
	id := uuid.Must(uuid.NewV7())
	contextID := uuid.Must(uuid.NewV7()).String()
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text))
	msg.TaskID, msg.ContextID = a2a.TaskID(id.String()), contextID
	task := &a2a.Task{ID: a2a.TaskID(id.String()), ContextID: contextID,
		Status: a2a.TaskStatus{State: a2a.TaskStateSubmitted}, History: []*a2a.Message{msg}}
	if _, err := e.log.CreateTask(context.Background(), nil, NewTask{
		ID: id, Service: e.svc, Protocol: ProtocolA2A, Owner: o, ContextID: contextID, State: StateSubmitted, Snapshot: task,
		Provisional: true,
	}); err != nil {
		e.t.Fatal(err)
	}
	return task
}

// fromOrigin is ctx carrying origin for ev, as a dispatch names the origin of each event it yields (D357).
func fromOrigin(ctx context.Context, ev a2a.Event, origin *Origin) context.Context {
	ctx, o := WithOrigins(ctx)
	o.Set(ev, origin)
	return ctx
}

func (e *env) exposed(task *a2a.Task) bool {
	e.t.Helper()
	id := uuid.MustParse(string(task.ID))
	row, err := e.log.LoadScopedAll(context.Background(), e.svc, buyerA, id)
	if err != nil {
		e.t.Fatal(err)
	}
	_, scopedErr := e.log.LoadScoped(context.Background(), e.svc, buyerA, id)
	if (row.ExposedAt != nil) != (scopedErr == nil) {
		e.t.Fatalf("LoadScoped %v disagrees with exposed_at %v", scopedErr, row.ExposedAt)
	}
	if scopedErr != nil && !errors.Is(scopedErr, a2a.ErrTaskNotFound) {
		e.t.Fatalf("LoadScoped of a provisional record: %v, want TaskNotFound", scopedErr)
	}
	return row.ExposedAt != nil
}

// D362's exposure rule, written in the transaction of the event that causes it: a provisional record stays hidden
// through gate statuses that keep the turn going and through a callee's Message (logged as a completed status, D5),
// and is exposed by the callee's task event, or by a gate status that ends or answers the turn first. A task created
// for any other request is exposed at creation.
func TestExposure(t *testing.T) {
	e := newEnv(t)
	s := e.store(nil)
	ctx := asOwner(context.Background(), buyerA)
	callee := func(typ string) *Origin { return &Origin{Actor: ActorCallee, Type: typ, Method: "SendMessage"} }

	if task := e.newTask(e.svc, buyerA, "", "plain"); !e.exposed(task) {
		t.Error("a task created without Provisional is not exposed")
	}

	t.Run("message answer", func(t *testing.T) {
		task := e.provisional(buyerA, "hi")
		working := status(task, a2a.TaskStateWorking, "")
		mustApply(t, fromOrigin(ctx, working, GateOrigin()), s, task.ID, working) // the gate's own working status
		if e.exposed(task) {
			t.Fatal("a gate working status exposed the record")
		}
		ev := status(task, a2a.TaskStateCompleted, "hello")
		mustApply(t, fromOrigin(ctx, ev, &Origin{Actor: ActorCallee, Type: TypeMessage, ConvertedTo: TypeStatusUpdate}), s, task.ID, ev)
		if e.exposed(task) {
			t.Fatal("a callee's Message exposed the record")
		}
		row, err := e.log.LoadScopedAll(ctx, e.svc, buyerA, uuid.MustParse(string(task.ID)))
		if err != nil {
			t.Fatal(err)
		}
		msg, ok := MessageAnswer(row)
		if !ok || msg.ID != ev.Status.Message.ID || msg.ContextID != task.ContextID {
			t.Fatalf("MessageAnswer %+v %v", msg, ok)
		}
		list, err := s.List(ctx, &a2a.ListTasksRequest{})
		if err != nil {
			t.Fatal(err)
		}
		for _, lt := range list.Tasks {
			if lt.ID == task.ID {
				t.Fatal("ListTasks lists a message-only record")
			}
		}
	})

	t.Run("task answer", func(t *testing.T) {
		task := e.provisional(buyerA, "hi")
		ev := status(task, a2a.TaskStateWorking, "")
		mustApply(t, fromOrigin(ctx, ev, callee(TypeTask)), s, task.ID, ev)
		if !e.exposed(task) {
			t.Fatal("the callee's Task did not expose the record")
		}
		mustApply(t, ctx, s, task.ID, status(task, a2a.TaskStateCompleted, "done"))
		row, _ := e.log.LoadScopedAll(ctx, e.svc, buyerA, uuid.MustParse(string(task.ID)))
		if _, ok := MessageAnswer(row); ok {
			t.Fatal("an exposed task is answered with a Message")
		}
	})

	t.Run("artifact answer", func(t *testing.T) {
		task := e.provisional(buyerA, "hi")
		ev := &a2a.TaskArtifactUpdateEvent{TaskID: task.ID, ContextID: task.ContextID,
			Artifact: &a2a.Artifact{ID: "a", Parts: a2a.ContentParts{a2a.NewTextPart("x")}}}
		mustApply(t, fromOrigin(ctx, ev, callee(TypeArtifactUpdate)), s, task.ID, ev)
		if !e.exposed(task) {
			t.Fatal("the callee's artifact event did not expose the record")
		}
	})

	t.Run("origin not named", func(t *testing.T) {
		task := e.provisional(buyerA, "hi")
		mustApply(t, ctx, s, task.ID, status(task, a2a.TaskStateWorking, ""))
		if !e.exposed(task) {
			t.Fatal("an event of unknown origin left the record hidden: it is shown rather than hidden")
		}
	})

	for _, st := range []a2a.TaskState{a2a.TaskStateFailed, a2a.TaskStateCanceled, a2a.TaskStateInputRequired} {
		t.Run("gate "+string(st), func(t *testing.T) {
			task := e.provisional(buyerA, "hi")
			ev := status(task, st, "the gate says so")
			mustApply(t, fromOrigin(ctx, ev, GateOrigin()), s, task.ID, ev)
			if !e.exposed(task) {
				t.Fatalf("a gate %s status before any callee answer did not expose the record", st)
			}
		})
	}
}
