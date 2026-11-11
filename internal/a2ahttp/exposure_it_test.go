package a2ahttp

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"

	"kontra.sh/kontra/internal/store"
)

// D362: a provisional record (a new message the callee has not answered with a task, or answered with a Message) is
// TaskNotFound to every caller method on both bindings, as an unknown id is, and ListTasks never lists it.
func TestProvisionalRecordIsNotFound(t *testing.T) {
	s := newStack(t)
	ctx := context.Background()
	svc, err := s.log.LoadService(ctx, mustTenant(t, s), "hello")
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.Must(uuid.NewV7())
	m := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi"))
	m.TaskID, m.ContextID = a2a.TaskID(id.String()), "conv"
	snap := &a2a.Task{ID: a2a.TaskID(id.String()), ContextID: "conv", Status: a2a.TaskStatus{State: a2a.TaskStateSubmitted}, History: []*a2a.Message{m}}
	if _, err := s.log.CreateTask(ctx, nil, store.NewTask{ID: id, Service: svc, Protocol: store.ProtocolA2A, Owner: ownerOf(alice),
		ContextID: "conv", State: store.StateSubmitted, Snapshot: snap, Provisional: true}); err != nil {
		t.Fatal(err)
	}
	tid := id.String()
	follow := `{"message":{"messageId":"` + uuid.NewString() + `","role":"ROLE_USER","taskId":"` + tid + `","parts":[{"text":"x"}]}}`
	push := `{"taskId":"` + tid + `","url":"https://example.com/hook"}`
	for _, c := range []struct {
		name         string
		method, path string
		body         string
	}{
		{"rpc GetTask", "POST", "/agents/hello", rpc("GetTask", `{"id":"`+tid+`"}`, 1)},
		{"rpc CancelTask", "POST", "/agents/hello", rpc("CancelTask", `{"id":"`+tid+`"}`, 2)},
		{"rpc SubscribeToTask", "POST", "/agents/hello", rpc("SubscribeToTask", `{"id":"`+tid+`"}`, 3)},
		{"rpc follow-up", "POST", "/agents/hello", rpc("SendMessage", follow, 4)},
		{"rpc ListTaskPushNotificationConfigs", "POST", "/agents/hello", rpc("ListTaskPushNotificationConfigs", `{"taskId":"`+tid+`"}`, 5)},
		{"rpc CreateTaskPushNotificationConfig", "POST", "/agents/hello", rpc("CreateTaskPushNotificationConfig", push, 6)},
		{"rest GetTask", "GET", "/agents/hello/tasks/" + tid, ""},
		{"rest CancelTask", "POST", "/agents/hello/tasks/" + tid + ":cancel", "{}"},
		{"rest SubscribeToTask", "POST", "/agents/hello/tasks/" + tid + ":subscribe", ""},
		{"rest follow-up", "POST", "/agents/hello/message:send", follow},
		{"rest ListTaskPushNotificationConfigs", "GET", "/agents/hello/tasks/" + tid + "/pushNotificationConfigs", ""},
	} {
		resp, err := http.DefaultClient.Do(s.request(t, c.method, c.path, c.body, "alice"))
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !strings.Contains(string(b), "TASK_NOT_FOUND") && !strings.Contains(string(b), `"code":-32001`) {
			t.Errorf("%s: %d %s, want TaskNotFound", c.name, resp.StatusCode, b)
		}
	}
	for _, r := range []*http.Request{
		s.request(t, "POST", "/agents/hello", rpc("ListTasks", `{}`, 9), "alice"),
		s.request(t, "GET", "/agents/hello/tasks", "", "alice"),
	} {
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || strings.Contains(string(b), tid) {
			t.Errorf("ListTasks %s: %d %s, want no provisional record", r.URL.Path, resp.StatusCode, b)
		}
	}
}

func mustTenant(t *testing.T, s *stack) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := s.pool.QueryRow(context.Background(), `select id from kontra.tenants where slug = 'default'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}
