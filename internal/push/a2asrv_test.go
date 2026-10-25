package push

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/medmahmoudi26/kontra/internal/store"
)

type noExecutor struct{}

func (noExecutor) Execute(context.Context, *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(func(a2a.Event, error) bool) {}
}

func (noExecutor) Cancel(context.Context, *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(func(a2a.Event, error) bool) {}
}

type ownerKey struct{}

func asOwner(ctx context.Context, o store.Owner) context.Context {
	return context.WithValue(ctx, ownerKey{}, o)
}

func ctxOwner(ctx context.Context) (store.Owner, error) {
	o, ok := ctx.Value(ownerKey{}).(store.Owner)
	if !ok {
		return store.Owner{}, a2a.ErrUnauthenticated
	}
	return o, nil
}

// TestThroughA2AGoHandler drives the push config calls through a2a-go's request handler with the gate's
// task store (spec §15: WithPushNotifications(pgPushConfigStore, noopSender)): a2a-go authorizes each call
// with the caller-scoped task store, so buyer B never reaches buyer A's configs; a created config comes
// back with its id and the gate task id; and a missing config is TaskNotFound (-32001) on JSON-RPC.
func TestThroughA2AGoHandler(t *testing.T) {
	e := newEnv(t)
	sig := &signals{}
	cs := NewConfigStore(e.pool, sig, private, nil)
	h := a2asrv.NewHandler(noExecutor{},
		a2asrv.WithTaskStore(store.New(e.pool, e.svc, ctxOwner, nil, e.log)),
		a2asrv.WithPushNotifications(cs, NoopSender{}))
	id := e.newTask(buyerA)
	a := asOwner(context.Background(), buyerA)
	b := asOwner(context.Background(), buyerB)

	created, err := h.CreateTaskPushConfig(a, &a2a.PushConfig{TaskID: tid(id), URL: "http://127.0.0.1:9/hook", Token: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || created.TaskID != tid(id) || created.URL != "http://127.0.0.1:9/hook" {
		t.Fatalf("created %+v", created)
	}
	if len(sig.list()) != 1 {
		t.Errorf("kontra.push sent %d times", len(sig.list()))
	}
	got, err := h.GetTaskPushConfig(a, &a2a.GetTaskPushConfigRequest{TaskID: tid(id), ID: created.ID})
	if err != nil || got.ID != created.ID || got.Token != "t" {
		t.Fatalf("get: %+v %v", got, err)
	}
	list, err := h.ListTaskPushConfigs(a, &a2a.ListTaskPushConfigRequest{TaskID: tid(id)})
	if err != nil || len(list.Configs) != 1 {
		t.Fatalf("list: %+v %v", list, err)
	}
	// Buyer B: TaskNotFound on every call, and nothing changes.
	if _, err := h.CreateTaskPushConfig(b, &a2a.PushConfig{TaskID: tid(id), URL: "http://127.0.0.1:9/b"}); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Errorf("buyer B create: %v", err)
	}
	if _, err := h.GetTaskPushConfig(b, &a2a.GetTaskPushConfigRequest{TaskID: tid(id), ID: created.ID}); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Errorf("buyer B get: %v", err)
	}
	if err := h.DeleteTaskPushConfig(b, &a2a.DeleteTaskPushConfigRequest{TaskID: tid(id), ID: created.ID}); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Errorf("buyer B delete: %v", err)
	}
	if n := e.count(`select count(*) from kontra.push_configs where task_id = $1`, id); n != 1 {
		t.Fatalf("%d configs after buyer B's calls", n)
	}
	// The 11th config through the handler is InvalidParams (-32602).
	for i := range MaxPerTask - 1 {
		if _, err := h.CreateTaskPushConfig(a, &a2a.PushConfig{TaskID: tid(id), URL: "http://127.0.0.1:9/n" + string(rune('a'+i))}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.CreateTaskPushConfig(a, &a2a.PushConfig{TaskID: tid(id), URL: "http://127.0.0.1:9/eleventh"}); !errors.Is(err, a2a.ErrInvalidParams) {
		t.Errorf("the 11th config through the handler: %v", err)
	}

	// JSON-RPC: a missing config is TaskNotFoundError (A2A §3.1.8), deleting it is not an error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a2asrv.NewJSONRPCHandler(h).ServeHTTP(w, r.WithContext(asOwner(r.Context(), buyerA)))
	}))
	defer srv.Close()
	call := func(method, params string) map[string]any {
		t.Helper()
		body := `{"jsonrpc":"2.0","id":1,"method":"` + method + `","params":` + params + `}`
		req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("A2A-Version", "1.0")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	out := call("GetTaskPushNotificationConfig", `{"taskId":"`+id.String()+`","id":"missing"}`)
	errObj, _ := out["error"].(map[string]any)
	if errObj == nil || errObj["code"] != float64(-32001) {
		t.Errorf("GetTaskPushNotificationConfig of a missing config answered %v, want -32001", out)
	}
	out = call("DeleteTaskPushNotificationConfig", `{"taskId":"`+id.String()+`","id":"missing"}`)
	if out["error"] != nil {
		t.Errorf("DeleteTaskPushNotificationConfig of a missing config answered %v", out)
	}
	out = call("CreateTaskPushNotificationConfig", `{"taskId":"`+id.String()+`","url":"http://127.0.0.1:9/twelfth"}`)
	errObj, _ = out["error"].(map[string]any)
	if errObj == nil || errObj["code"] != float64(-32602) {
		t.Errorf("a config over the task's limit on JSON-RPC answered %v, want -32602", out)
	}
}
