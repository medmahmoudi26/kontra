package e2e

import (
	"context"
	"encoding/json"
	"iter"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"

	"kontra.sh/kontra/test/e2e/slowagent"
)

// messageAgent is an A2A 1.0 agent that answers every message with a bare Message and no task (A2A allows either).
func messageAgent(t *testing.T) string {
	t.Helper()
	exec := a2asrv.AgentExecutorFunc(func(_ context.Context, _ *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
		return func(yield func(a2a.Event, error) bool) {
			yield(a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("a stateless answer")), nil)
		}
	})
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	base := "http://" + l.Addr().String()
	card := &a2a.AgentCard{Name: "Plain", Version: "1", Description: "Answers with a Message.",
		SupportedInterfaces: []*a2a.AgentInterface{a2a.NewAgentInterface(base, a2a.TransportProtocolJSONRPC)},
		DefaultInputModes:   []string{"text/plain"}, DefaultOutputModes: []string{"text/plain"},
		Skills: []a2a.AgentSkill{{ID: "plain", Name: "Plain", Description: "Plain.", Tags: []string{"x"}}}}
	h := a2asrv.NewHandler(exec, a2asrv.WithTaskStore(taskstore.NewInMemory(nil)))
	mux := http.NewServeMux()
	mux.Handle("GET "+a2asrv.WellKnownAgentCardPath, a2asrv.NewStaticAgentCardHandler(card))
	mux.Handle("/", a2asrv.NewJSONRPCHandler(h))
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return base
}

// The A2A wire in the local development profile (OpenSpec change console-a2a-protocol-view, 8.1): a stock a2a-go
// client, holding a credential the profile ignores, calls a streaming agent, a polling-only agent and one that answers
// with a bare Message, through one node with no enrollment; it lists its tasks and sets a push config, which the
// profile admits (D361). Every logged event names its origin; both legs of every task are recorded, with their
// bodies; no stored row holds the credential, a header or the push config's token.
func TestLocalProfileWire(t *testing.T) {
	h, g := startLocal(t, "")
	ctx := ctxFor(t, 3*time.Minute)
	streaming := slowagent.New(slowagent.Config{Chunks: 2, Interval: 30 * time.Millisecond, Streaming: true})
	polling := slowagent.New(slowagent.Config{Chunks: 1, Interval: 30 * time.Millisecond})
	urls := map[string]string{"msg": messageAgent(t)}
	for name, a := range map[string]*slowagent.Agent{"stream": streaming, "poll": polling} {
		u, err := a.Start(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		h.agents = append(h.agents, a)
		urls[name] = u
	}
	if _, err := h.StartNode(ctx, NodeOptions{Name: "wire", Gate: g.URL, Serves: urls}); err != nil {
		t.Fatal(err)
	}
	if err := h.waitReady(ctx, g, []string{"stream", "poll", "msg"}); err != nil {
		t.Fatal(err)
	}
	const secret, pushToken = "kk_wire_secret_00000001", "push-token-wire-1"
	buyer := &Buyer{Key: secret}
	tasks := map[string]a2a.TaskID{}
	msgContext := "" // the Message agent's interaction is no task: its caller leg names its context
	for _, svc := range []string{"stream", "poll", "msg"} {
		c := buyer.Client(t, g, svc)
		res, err := c.SendMessage(ctx, &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("wire "+svc))})
		if err != nil {
			t.Fatalf("%s: SendMessage: %v", svc, err)
		}
		if svc == "msg" {
			// D362: the Message agent's answer is its Message, and the gate's record of it is no task.
			m, ok := res.(*a2a.Message)
			if !ok || m.TaskID != "" {
				t.Fatalf("msg: answer %T %+v, want the agent's Message with no taskId", res, res)
			}
			var id string
			if err := h.Pool.QueryRow(ctx, `select id::text from kontra.tasks where context_id = $1 and exposed_at is null`, m.ContextID).Scan(&id); err != nil {
				t.Fatalf("msg: the gate's record of the interaction: %v", err)
			}
			tasks[svc], msgContext = a2a.TaskID(id), m.ContextID
			continue
		}
		task, ok := res.(*a2a.Task)
		if !ok {
			t.Fatalf("%s: answer %T", svc, res)
		}
		tasks[svc] = task.ID
		eventually(t, 30*time.Second, svc+" completes", func() bool {
			st, err := h.TaskState(ctx, task.ID)
			return err == nil && st == "completed"
		})
	}
	// ListTasks and a push config, as the shared caller (D361).
	c := buyer.Client(t, g, "poll")
	list, err := c.ListTasks(ctx, &a2a.ListTasksRequest{})
	if err != nil || len(list.Tasks) == 0 {
		t.Fatalf("ListTasks in the local profile: %+v %v", list, err)
	}
	if _, err := c.CreateTaskPushConfig(ctx, &a2a.PushConfig{TaskID: tasks["poll"], URL: "http://127.0.0.1:9/hook", Token: pushToken}); err != nil {
		t.Fatalf("CreateTaskPushNotificationConfig in the local profile: %v", err)
	}

	// Every event names its origin; the Message agent's answer is the callee's Message, logged converted (D5, D357).
	for svc, id := range tasks {
		rows, err := h.Pool.Query(ctx, `select seq, origin::text from kontra.task_events where task_id = $1 order by seq`, string(id))
		if err != nil {
			t.Fatal(err)
		}
		var origins []map[string]any
		for rows.Next() {
			var seq int64
			var raw *string
			if err := rows.Scan(&seq, &raw); err != nil {
				t.Fatal(err)
			}
			if raw == nil {
				t.Errorf("%s seq %d has no origin", svc, seq)
				continue
			}
			var o map[string]any
			_ = json.Unmarshal([]byte(*raw), &o)
			origins = append(origins, o)
		}
		rows.Close()
		if len(origins) == 0 || origins[0]["actor"] != "caller" || origins[0]["type"] != "SendMessage" {
			t.Errorf("%s: first origin %v", svc, origins)
		}
		if svc == "msg" {
			last := origins[len(origins)-1]
			if last["actor"] != "callee" || last["type"] != "Message" || last["converted_to"] != "TaskStatusUpdateEvent" {
				t.Errorf("msg: last origin %v", last)
			}
		}
		if svc == "stream" {
			found := false
			for _, o := range origins {
				found = found || (o["actor"] == "callee" && o["method"] == "SendStreamingMessage")
			}
			if !found {
				t.Errorf("stream: no callee event in SendStreamingMessage: %v", origins)
			}
		}
		// Both legs, with bodies.
		eventually(t, 10*time.Second, svc+": both legs recorded", func() bool {
			var caller, callee int
			_ = h.Pool.QueryRow(ctx, `select count(*) filter (where leg = 'caller' and request_body is not null),
				count(*) filter (where leg = 'callee') from kontra.a2a_exchanges
				where task_id = $1 or gate_task_id = $1 or ($2 <> '' and leg = 'caller' and context_id = $2)`, string(id), map[bool]string{true: msgContext}[svc == "msg"]).Scan(&caller, &callee)
			return caller > 0 && callee > 0
		})
	}

	// The exchange writer is batched: wait for the push config call's row before reading what is stored.
	eventually(t, 10*time.Second, "the push config call recorded", func() bool {
		var n int
		_ = h.Pool.QueryRow(ctx, `select count(*) from kontra.a2a_exchanges
			where method like '%PushNotificationConfig%' and request_body is not null`).Scan(&n)
		return n > 0
	})

	// No stored row holds the credential, a header, or the push config's token.
	var dump string
	if err := h.Pool.QueryRow(ctx, `select coalesce(string_agg(x::text, ' '), '') from (
		select row_to_json(e)::text as x from kontra.a2a_exchanges e
		union all select convert_from(data, 'UTF8') from kontra.a2a_exchange_frames
		union all select payload::text || coalesce(origin::text, '') from kontra.task_events) s`).Scan(&dump); err != nil {
		t.Fatal(err)
	}
	var bodies string
	if err := h.Pool.QueryRow(ctx, `select coalesce(string_agg(convert_from(request_body, 'UTF8') || coalesce(convert_from(response_body, 'UTF8'), ''), ' '), '')
		from kontra.a2a_exchanges where request_body is not null`).Scan(&bodies); err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{secret, pushToken, "Bearer", "Authorization"} {
		if strings.Contains(dump+bodies, leak) {
			t.Errorf("a stored row holds %q", leak)
		}
	}
	if !strings.Contains(bodies, `"[redacted]"`) {
		t.Errorf("the push config's body was not kept redacted")
	}
}
