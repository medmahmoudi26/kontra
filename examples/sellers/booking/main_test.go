package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/eventqueue"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
)

// serve starts the agent with binding on an httptest server and returns its root URL.
func serve(t *testing.T, binding string) string {
	t.Helper()
	var h http.Handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(w, r) }))
	t.Cleanup(srv.Close)
	h = newHandler(srv.URL, binding, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return srv.URL
}

// client talks to the agent at base over one binding only.
func client(t *testing.T, base string, protocol a2a.TransportProtocol) *a2aclient.Client {
	t.Helper()
	url := base
	if protocol == a2a.TransportProtocolHTTPJSON {
		url += restPrefix
	}
	c, err := a2aclient.NewFromEndpoints(context.Background(), []*a2a.AgentInterface{a2a.NewAgentInterface(url, protocol)})
	if err != nil {
		t.Fatalf("client for %s %s: %v", protocol, url, err)
	}
	t.Cleanup(func() { _ = c.Destroy() })
	return c
}

func ctxT(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func message(text string, task *a2a.Task) *a2a.SendMessageRequest {
	m := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text))
	if task != nil {
		m.TaskID, m.ContextID = task.ID, task.ContextID
	}
	return &a2a.SendMessageRequest{Message: m}
}

func send(t *testing.T, c *a2aclient.Client, text string, task *a2a.Task) *a2a.Task {
	t.Helper()
	res, err := c.SendMessage(ctxT(t), message(text, task))
	if err != nil {
		t.Fatalf("SendMessage %q: %v", text, err)
	}
	got, ok := res.(*a2a.Task)
	if !ok {
		t.Fatalf("SendMessage %q returned %T, want a task", text, res)
	}
	return got
}

func statusText(task *a2a.Task) string {
	return text(task.Status.Message)
}

func artifactText(task *a2a.Task) string {
	var b strings.Builder
	for _, a := range task.Artifacts {
		for _, p := range a.Parts {
			b.WriteString(p.Text())
		}
	}
	return b.String()
}

func bindings() map[string]a2a.TransportProtocol {
	return map[string]a2a.TransportProtocol{"JSONRPC": a2a.TransportProtocolJSONRPC, "HTTP+JSON": a2a.TransportProtocolHTTPJSON}
}

// The first message stops at input-required asking for a date; a reply with a date completes the same task
// with the booking as an artifact. Both bindings.
func TestAskThenConfirm(t *testing.T) {
	for name, protocol := range bindings() {
		t.Run(name, func(t *testing.T) {
			c := client(t, serve(t, bindingBoth), protocol)

			first := send(t, c, "a table for two", nil)
			if first.Status.State != a2a.TaskStateInputRequired {
				t.Fatalf("first turn: state %s, want input-required", first.Status.State)
			}
			q := statusText(first)
			if !strings.Contains(q, "For which date") || !strings.Contains(q, `"a table for two"`) || !strings.Contains(q, "YYYY-MM-DD") {
				t.Fatalf("first turn asks %q, want the date of \"a table for two\" as YYYY-MM-DD", q)
			}
			if len(first.Artifacts) != 0 {
				t.Fatalf("first turn has artifacts %v, want none", first.Artifacts)
			}

			done := send(t, c, "on 2026-11-02 please", first)
			if done.ID != first.ID || done.ContextID != first.ContextID {
				t.Fatalf("follow-up answered task %s/%s, want %s/%s", done.ID, done.ContextID, first.ID, first.ContextID)
			}
			if done.Status.State != a2a.TaskStateCompleted {
				t.Fatalf("follow-up: state %s (%q), want completed", done.Status.State, statusText(done))
			}
			const booked = `Booked "a table for two" on Monday, 2026-11-02.`
			if got := artifactText(done); got != booked {
				t.Fatalf("artifact %q, want %q", got, booked)
			}
			if len(done.Artifacts) != 1 || done.Artifacts[0].Name != "booking" {
				t.Fatalf("artifacts %+v, want one named booking", done.Artifacts)
			}
			if got := statusText(done); got != "Confirmed: "+booked {
				t.Fatalf("final status message %q", got)
			}
			// The history holds both user turns and the question, in order.
			var users []string
			for _, m := range done.History {
				if m.Role == a2a.MessageRoleUser {
					users = append(users, text(m))
				}
			}
			if strings.Join(users, "|") != "a table for two|on 2026-11-02 please" {
				t.Fatalf("user history %q", users)
			}
		})
	}
}

// A reply without a readable date (or with an impossible one) asks again and keeps the task waiting.
func TestReplyWithoutDateAsksAgain(t *testing.T) {
	c := client(t, serve(t, bindingBoth), a2a.TransportProtocolJSONRPC)
	first := send(t, c, "a haircut", nil)
	for _, reply := range []string{"next tuesday", "2026-02-30", ""} {
		again := send(t, c, reply, first)
		if again.Status.State != a2a.TaskStateInputRequired {
			t.Fatalf("reply %q: state %s, want input-required", reply, again.Status.State)
		}
		if q := statusText(again); !strings.Contains(q, "I could not read a date") {
			t.Fatalf("reply %q: asked %q", reply, q)
		}
		if len(again.Artifacts) != 0 {
			t.Fatalf("reply %q booked %v", reply, again.Artifacts)
		}
	}
	done := send(t, c, "2026-12-24", first)
	if done.Status.State != a2a.TaskStateCompleted || artifactText(done) != `Booked "a haircut" on Thursday, 2026-12-24.` {
		t.Fatalf("after a valid date: %s, %q", done.Status.State, artifactText(done))
	}
}

// CancelTask on a task waiting for its date ends it canceled; the agent then refuses a reply to it. A
// completed task is not cancelable. Both bindings.
func TestCancel(t *testing.T) {
	for name, protocol := range bindings() {
		t.Run(name, func(t *testing.T) {
			c := client(t, serve(t, bindingBoth), protocol)
			ctx := ctxT(t)

			waiting := send(t, c, "a room", nil)
			canceled, err := c.CancelTask(ctx, &a2a.CancelTaskRequest{ID: waiting.ID})
			if err != nil {
				t.Fatalf("CancelTask: %v", err)
			}
			if canceled.Status.State != a2a.TaskStateCanceled {
				t.Fatalf("CancelTask returned %s, want canceled", canceled.Status.State)
			}
			got, err := c.GetTask(ctx, &a2a.GetTaskRequest{ID: waiting.ID})
			if err != nil || got.Status.State != a2a.TaskStateCanceled {
				t.Fatalf("GetTask after the cancel: %v, %v", got, err)
			}
			if res, err := c.SendMessage(ctx, message("2026-11-02", waiting)); err == nil {
				t.Fatalf("a reply to the canceled task was accepted: %+v", res)
			}
			if got, err := c.GetTask(ctx, &a2a.GetTaskRequest{ID: waiting.ID}); err != nil || got.Status.State != a2a.TaskStateCanceled || len(got.Artifacts) != 0 {
				t.Fatalf("the canceled task changed after a reply: %+v, %v", got, err)
			}

			booked := send(t, c, "a car", nil)
			booked = send(t, c, "2026-11-03", booked)
			if booked.Status.State != a2a.TaskStateCompleted {
				t.Fatalf("booking: %s", booked.Status.State)
			}
			if _, err := c.CancelTask(ctx, &a2a.CancelTaskRequest{ID: booked.ID}); !errors.Is(err, a2a.ErrTaskNotCancelable) {
				t.Fatalf("CancelTask of a completed task: %v, want TaskNotCancelable", err)
			}
		})
	}
}

// A cancel right after a turn ended at input-required cancels the task. a2a-go v2.7.0 may still be cleaning up
// the turn's execution then and send the cancel to it, which used to fail TaskNotCancelableError or "queue is
// closed" for a task that is cancelable (cancelRetry). Many tries, so that the window is hit.
func TestCancelRightAfterInputRequired(t *testing.T) {
	for name, protocol := range bindings() {
		t.Run(name, func(t *testing.T) {
			c := client(t, serve(t, bindingBoth), protocol)
			ctx := ctxT(t)
			for i := range 100 {
				waiting := send(t, c, "a seat", nil)
				canceled, err := c.CancelTask(ctx, &a2a.CancelTaskRequest{ID: waiting.ID})
				if err != nil {
					t.Fatalf("try %d: CancelTask right after input-required: %v", i, err)
				}
				if canceled.Status.State != a2a.TaskStateCanceled {
					t.Fatalf("try %d: CancelTask returned %s", i, canceled.Status.State)
				}
			}
		})
	}
}

// scriptedCancels answers CancelTask with errs in turn, then with a canceled task, and counts the calls.
type scriptedCancels struct {
	a2asrv.RequestHandler
	errs  []error
	calls int
}

func (s *scriptedCancels) CancelTask(_ context.Context, req *a2a.CancelTaskRequest) (*a2a.Task, error) {
	s.calls++
	if s.calls <= len(s.errs) {
		return nil, s.errs[s.calls-1]
	}
	return &a2a.Task{ID: req.ID, Status: a2a.TaskStatus{State: a2a.TaskStateCanceled}}, nil
}

// cancelRetry makes a cancel again only when it failed the way the race fails and the task is still waiting,
// and only cancelRetries times.
func TestCancelRetry(t *testing.T) {
	ctx := ctxT(t)
	store := taskstore.NewInMemory(nil)
	for id, state := range map[a2a.TaskID]a2a.TaskState{"waiting": a2a.TaskStateInputRequired, "done": a2a.TaskStateCompleted} {
		if _, err := store.Create(ctx, &a2a.Task{ID: id, ContextID: "c", Status: a2a.TaskStatus{State: state}}); err != nil {
			t.Fatal(err)
		}
	}
	notCancelable := fmt.Errorf("failed to cancel: %w", a2a.ErrTaskNotCancelable)
	closed := fmt.Errorf("cancelation failed: %w", eventqueue.ErrQueueClosed)
	for _, c := range []struct {
		name      string
		id        a2a.TaskID
		errs      []error
		wantCalls int
		wantErr   error
	}{
		{"lost the race twice, then canceled", "waiting", []error{notCancelable, closed}, 3, nil},
		{"a completed task is not retried", "done", []error{notCancelable}, 1, a2a.ErrTaskNotCancelable},
		{"another error is not retried", "waiting", []error{a2a.ErrTaskNotFound}, 1, a2a.ErrTaskNotFound},
		{"an unknown task is not retried", "nope", []error{notCancelable}, 1, a2a.ErrTaskNotCancelable},
		{"bounded", "waiting", slices.Repeat([]error{closed}, cancelRetries+5), cancelRetries, eventqueue.ErrQueueClosed},
	} {
		s := &scriptedCancels{errs: c.errs}
		task, err := cancelRetry{RequestHandler: s, store: store}.CancelTask(ctx, &a2a.CancelTaskRequest{ID: c.id})
		if s.calls != c.wantCalls {
			t.Errorf("%s: %d calls, want %d", c.name, s.calls, c.wantCalls)
		}
		switch {
		case c.wantErr == nil && (err != nil || task == nil || task.Status.State != a2a.TaskStateCanceled):
			t.Errorf("%s: %v, %v, want the canceled task", c.name, task, err)
		case c.wantErr != nil && !errors.Is(err, c.wantErr):
			t.Errorf("%s: error %v, want %v", c.name, err, c.wantErr)
		}
	}

	// A caller that gives up stops the retries.
	gone, cancel := context.WithCancel(ctx)
	cancel()
	s := &scriptedCancels{errs: slices.Repeat([]error{closed}, cancelRetries)}
	if _, err := (cancelRetry{RequestHandler: s, store: store}).CancelTask(gone, &a2a.CancelTaskRequest{ID: "waiting"}); err == nil || s.calls != 1 {
		t.Errorf("after the caller left: %d calls, error %v", s.calls, err)
	}
}

// Streaming: the first turn is the submitted task then input-required; the reply streams working, the
// artifact and completed.
func TestStreaming(t *testing.T) {
	for name, protocol := range bindings() {
		t.Run(name, func(t *testing.T) {
			c := client(t, serve(t, bindingBoth), protocol)
			ctx := ctxT(t)

			var task *a2a.Task
			var states []string
			for ev, err := range c.SendStreamingMessage(ctx, message("a table for four", nil)) {
				if err != nil {
					t.Fatalf("stream: %v", err)
				}
				switch e := ev.(type) {
				case *a2a.Task:
					task = e
					states = append(states, "task:"+string(e.Status.State))
				case *a2a.TaskStatusUpdateEvent:
					states = append(states, "status:"+string(e.Status.State))
				default:
					states = append(states, "other")
				}
			}
			want := "task:" + string(a2a.TaskStateSubmitted) + " status:" + string(a2a.TaskStateInputRequired)
			if strings.Join(states, " ") != want || task == nil {
				t.Fatalf("first turn streamed %v, want %s", states, want)
			}

			states = nil
			var artifact string
			for ev, err := range c.SendStreamingMessage(ctx, message("2026-11-02", task)) {
				if err != nil {
					t.Fatalf("stream: %v", err)
				}
				switch e := ev.(type) {
				case *a2a.TaskStatusUpdateEvent:
					states = append(states, "status:"+string(e.Status.State))
				case *a2a.TaskArtifactUpdateEvent:
					states = append(states, "artifact")
					for _, p := range e.Artifact.Parts {
						artifact += p.Text()
					}
				case *a2a.Task:
					states = append(states, "task:"+string(e.Status.State))
				}
			}
			want = "status:" + string(a2a.TaskStateWorking) + " artifact status:" + string(a2a.TaskStateCompleted)
			if strings.Join(states, " ") != want {
				t.Fatalf("reply streamed %v, want %s", states, want)
			}
			if artifact != `Booked "a table for four" on Monday, 2026-11-02.` {
				t.Fatalf("streamed artifact %q", artifact)
			}
		})
	}
}

func getCard(t *testing.T, base string) *a2a.AgentCard {
	t.Helper()
	resp, err := http.Get(base + "/.well-known/agent-card.json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("card: HTTP %d", resp.StatusCode)
	}
	var card a2a.AgentCard
	if err := json.NewDecoder(resp.Body).Decode(&card); err != nil {
		t.Fatalf("card: %v", err)
	}
	return &card
}

func interfaces(card *a2a.AgentCard) string {
	var s []string
	for _, i := range card.SupportedInterfaces {
		s = append(s, string(i.ProtocolBinding)+" "+i.URL+" "+string(i.ProtocolVersion))
	}
	return strings.Join(s, ", ")
}

// The card lists the bindings served, every interface under the agent's root URL, and says streaming.
// --binding narrows both the card and the routes.
func TestCardAndBindings(t *testing.T) {
	base := serve(t, bindingBoth)
	card := getCard(t, base)
	if got, want := interfaces(card), "JSONRPC "+base+" 1.0, HTTP+JSON "+base+"/rest 1.0"; got != want {
		t.Fatalf("interfaces %q, want %q", got, want)
	}
	if card.Name != "Booking Agent" || !card.Capabilities.Streaming || len(card.Skills) != 1 {
		t.Fatalf("card %+v", card)
	}

	only := serve(t, bindingJSONRPC)
	if got, want := interfaces(getCard(t, only)), "JSONRPC "+only+" 1.0"; got != want {
		t.Fatalf("--binding jsonrpc: interfaces %q, want %q", got, want)
	}
	resp, err := http.Post(only+"/rest/message:send", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("--binding jsonrpc serves HTTP+JSON: HTTP %d", resp.StatusCode)
	}

	rest := serve(t, bindingHTTPJSON)
	if got, want := interfaces(getCard(t, rest)), "HTTP+JSON "+rest+"/rest 1.0"; got != want {
		t.Fatalf("--binding http+json: interfaces %q, want %q", got, want)
	}
	if task := send(t, client(t, rest, a2a.TransportProtocolHTTPJSON), "a desk", nil); task.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("--binding http+json: %s", task.Status.State)
	}
	resp, err = http.Post(rest+"/", "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"GetTask","params":{"id":"x"}}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("--binding http+json serves JSON-RPC at /: HTTP %d", resp.StatusCode)
	}
}

// The command: --port 0 picks a port, the first stdout line is the root URL the card advertises, and the
// agent stops cleanly when its context ends (SIGINT/SIGTERM in main).
func TestRun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pr, pw := io.Pipe()
	code := make(chan int, 1)
	go func() {
		code <- run(ctx, []string{"--port", "0"}, pw, io.Discard)
		pw.Close()
	}()
	line, err := bufio.NewReader(pr).ReadString('\n')
	if err != nil {
		t.Fatalf("first stdout line: %v", err)
	}
	go func() { _, _ = io.Copy(io.Discard, pr) }()
	base := strings.TrimSpace(line)
	if !strings.HasPrefix(base, "http://127.0.0.1:") || strings.HasSuffix(base, ":0") {
		t.Fatalf("root URL %q", base)
	}
	if got := getCard(t, base).SupportedInterfaces[0].URL; got != base {
		t.Fatalf("card interface %q, want %q", got, base)
	}
	if task := send(t, client(t, base, a2a.TransportProtocolJSONRPC), "a table", nil); task.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("state %s", task.Status.State)
	}
	cancel()
	select {
	case c := <-code:
		if c != 0 {
			t.Fatalf("exit code %d after the context ended, want 0", c)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the agent did not stop")
	}
	if _, err := http.Get(base + "/.well-known/agent-card.json"); err == nil {
		t.Fatal("the agent still answers after it stopped")
	}
}

func TestRunUsage(t *testing.T) {
	for _, args := range [][]string{{"--binding", "grpc"}, {"extra"}, {"--port", "70000"}, {"--nope"}} {
		var stderr strings.Builder
		if code := run(context.Background(), args, io.Discard, &stderr); code != 2 {
			t.Errorf("%q: exit %d, want 2 (stderr %q)", args, code, stderr.String())
		}
	}
}

func TestParseDate(t *testing.T) {
	for in, want := range map[string]string{
		"2026-11-02":                "2026-11-02",
		"on 2026-11-02, at noon":    "2026-11-02",
		"2026-02-30 or 2026-03-01":  "2026-03-01",
		"2026-13-01":                "",
		"tomorrow":                  "",
		"20261102":                  "",
		"x2026-11-02":               "",
		"2024-02-29 (a leap day)":   "2024-02-29",
		"2026-11-02 and 2026-11-03": "2026-11-02",
	} {
		d, ok := parseDate(in)
		got := ""
		if ok {
			got = d.Format(dateLayout)
		}
		if got != want {
			t.Errorf("parseDate(%q) = %q, want %q", in, got, want)
		}
	}
}
