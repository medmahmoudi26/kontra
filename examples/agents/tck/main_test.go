package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
)

// serve starts the agent on an httptest server and returns its root URL.
func serve(t *testing.T, a agent) string {
	t.Helper()
	var h http.Handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(w, r) }))
	t.Cleanup(srv.Close)
	var err error
	if h, err = newHandler(a, srv.URL, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
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

func bindings() map[string]a2a.TransportProtocol {
	return map[string]a2a.TransportProtocol{"JSONRPC": a2a.TransportProtocolJSONRPC, "HTTP+JSON": a2a.TransportProtocolHTTPJSON}
}

// tckID is a message id as the TCK makes it: tck-<name>-<8 hex digits> (a2a-tck tck/requirements/base.py).
func tckID(name string) string { return "tck-" + name + "-0a1b2c3d" }

// request is the TCK's message: one text part, the scenario in the id.
func request(id string, task *a2a.Task) *a2a.SendMessageRequest {
	m := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("TCK prerequisite task creation"))
	m.ID = id
	if task != nil {
		m.TaskID, m.ContextID = task.ID, task.ContextID
	}
	return &a2a.SendMessageRequest{Message: m}
}

func sendTask(t *testing.T, c *a2aclient.Client, req *a2a.SendMessageRequest) *a2a.Task {
	t.Helper()
	res, err := c.SendMessage(ctxT(t), req)
	if err != nil {
		t.Fatalf("SendMessage %s: %v", req.Message.ID, err)
	}
	task, ok := res.(*a2a.Task)
	if !ok {
		t.Fatalf("SendMessage %s returned %T, want a task", req.Message.ID, res)
	}
	return task
}

func text(m *a2a.Message) string {
	if m == nil {
		return ""
	}
	var b strings.Builder
	for _, p := range m.Parts {
		b.WriteString(p.Text())
	}
	return b.String()
}

// The scenarios that complete at once, over both bindings: the state, the status message and the one artifact's
// part that scenarios/core_operations.feature names.
func TestCompletingScenarios(t *testing.T) {
	cases := []struct {
		name    string
		state   a2a.TaskState
		message string
		part    func(t *testing.T, p *a2a.Part) // the one artifact's one part; nil: no artifact
	}{
		{name: "complete-task", state: a2a.TaskStateCompleted, message: textComplete},
		{name: "artifact-text", state: a2a.TaskStateCompleted, part: func(t *testing.T, p *a2a.Part) {
			if p.Text() != textArtifact {
				t.Errorf("text part %q, want %q", p.Text(), textArtifact)
			}
		}},
		{name: "artifact-file", state: a2a.TaskStateCompleted, part: func(t *testing.T, p *a2a.Part) {
			if !bytes.Equal(p.Raw(), fileContent) || p.MediaType != fileMediaType || p.Filename != fileName {
				t.Errorf("file part %q %s %s, want %q %s %s", p.Raw(), p.MediaType, p.Filename, fileContent, fileMediaType, fileName)
			}
		}},
		{name: "artifact-file-url", state: a2a.TaskStateCompleted, part: func(t *testing.T, p *a2a.Part) {
			if u, ok := p.Content.(a2a.URL); !ok || string(u) != fileURL || p.MediaType != fileMediaType || p.Filename != fileName {
				t.Errorf("url part %#v %s %s, want %s %s %s", p.Content, p.MediaType, p.Filename, fileURL, fileMediaType, fileName)
			}
		}},
		{name: "artifact-data", state: a2a.TaskStateCompleted, part: func(t *testing.T, p *a2a.Part) {
			d, ok := p.Content.(a2a.Data)
			got, _ := json.Marshal(d.Value)
			if !ok || string(got) != `{"count":42,"key":"value"}` {
				t.Errorf("data part %#v, want {\"key\": \"value\", \"count\": 42}", p.Content)
			}
		}},
		{name: "reject-task", state: a2a.TaskStateRejected, message: textRejected},
		{name: "input-required", state: a2a.TaskStateInputRequired},
		{name: "multi-004", state: a2a.TaskStateCompleted, message: unhandled + tckID("multi-004")},
	}
	for bname, protocol := range bindings() {
		t.Run(bname, func(t *testing.T) {
			c := client(t, serve(t, agent{long: time.Millisecond}), protocol)
			for _, tc := range cases {
				task := sendTask(t, c, request(tckID(tc.name), nil))
				if task.Status.State != tc.state || text(task.Status.Message) != tc.message {
					t.Errorf("%s: %s %q, want %s %q", tc.name, task.Status.State, text(task.Status.Message), tc.state, tc.message)
				}
				if len(task.History) == 0 || task.History[0].ID != tckID(tc.name) {
					t.Errorf("%s: history %+v, want the caller's message first", tc.name, task.History)
				}
				if tc.part == nil {
					if len(task.Artifacts) != 0 {
						t.Errorf("%s: artifacts %+v, want none", tc.name, task.Artifacts)
					}
					continue
				}
				if len(task.Artifacts) != 1 || len(task.Artifacts[0].Parts) != 1 {
					t.Errorf("%s: artifacts %+v, want one with one part", tc.name, task.Artifacts)
					continue
				}
				tc.part(t, task.Artifacts[0].Parts[0])
			}
		})
	}
}

// tck-message-response answers with a message and no task.
func TestMessageResponse(t *testing.T) {
	for bname, protocol := range bindings() {
		t.Run(bname, func(t *testing.T) {
			c := client(t, serve(t, agent{}), protocol)
			res, err := c.SendMessage(ctxT(t), request(tckID("message-response"), nil))
			if err != nil {
				t.Fatal(err)
			}
			m, ok := res.(*a2a.Message)
			if !ok || m.Role != a2a.MessageRoleAgent || text(m) != textMessage || m.TaskID != "" {
				t.Fatalf("answer %T %+v, want the agent's message %q without a task", res, res, textMessage)
			}
		})
	}
}

// An input-required task completes on a tck-complete-task follow-up (the TCK's multi-turn and history tests),
// and a cancel ends one that waits: canceled; a completed task is not cancelable.
func TestFollowUpAndCancel(t *testing.T) {
	for bname, protocol := range bindings() {
		t.Run(bname, func(t *testing.T) {
			c := client(t, serve(t, agent{}), protocol)
			first := sendTask(t, c, request(tckID("input-required"), nil))
			if first.Status.State != a2a.TaskStateInputRequired {
				t.Fatalf("first turn: %s", first.Status.State)
			}
			done := sendTask(t, c, request(tckID("input-required")+"-1", first)) // a follow-up keyed input-required asks again
			if done.ID != first.ID || done.Status.State != a2a.TaskStateInputRequired {
				t.Fatalf("an input-required follow-up: %s %s", done.ID, done.Status.State)
			}
			done = sendTask(t, c, request(tckID("complete-task"), first))
			if done.ID != first.ID || done.Status.State != a2a.TaskStateCompleted || text(done.Status.Message) != textComplete {
				t.Fatalf("the completing follow-up: %s %s %q", done.ID, done.Status.State, text(done.Status.Message))
			}
			if len(done.History) < 3 {
				t.Errorf("history %d messages, want the three user messages and more", len(done.History))
			}
			if _, err := c.CancelTask(ctxT(t), &a2a.CancelTaskRequest{ID: done.ID}); !errors.Is(err, a2a.ErrTaskNotCancelable) {
				t.Errorf("cancel of a completed task: %v, want TaskNotCancelableError", err)
			}

			waiting := sendTask(t, c, request(tckID("input-required"), nil))
			canceled, err := c.CancelTask(ctxT(t), &a2a.CancelTaskRequest{ID: waiting.ID})
			if err != nil || canceled.Status.State != a2a.TaskStateCanceled {
				t.Fatalf("cancel of an input-required task: %v %+v", err, canceled)
			}
		})
	}
}

// A part whose media type the card does not list is ContentTypeNotSupportedError (spec §3.1.1; the TCK's
// CORE-SEND-003 sends application/x-unsupported-tck-type).
func TestUnsupportedMediaType(t *testing.T) {
	for bname, protocol := range bindings() {
		t.Run(bname, func(t *testing.T) {
			c := client(t, serve(t, agent{}), protocol)
			p := a2a.NewRawPart([]byte("tck"))
			p.MediaType = "application/x-unsupported-tck-type"
			m := a2a.NewMessage(a2a.MessageRoleUser, p)
			m.ID = tckID("send-003")
			if _, err := c.SendMessage(ctxT(t), &a2a.SendMessageRequest{Message: m}); !errors.Is(err, a2a.ErrUnsupportedContentType) {
				t.Fatalf("an unsupported media type: %v, want ContentTypeNotSupportedError", err)
			}
		})
	}
}

// stream collects a SendStreamingMessage's events as "status:<state>" and "artifact:<text>[+append][+last]".
func stream(t *testing.T, c *a2aclient.Client, id string) []string {
	t.Helper()
	var got []string
	for ev, err := range c.SendStreamingMessage(ctxT(t), request(id, nil)) {
		if err != nil {
			t.Fatalf("stream %s: %v", id, err)
		}
		switch e := ev.(type) {
		case *a2a.Task:
			got = append(got, "task:"+string(e.Status.State))
		case *a2a.TaskStatusUpdateEvent:
			got = append(got, "status:"+string(e.Status.State))
		case *a2a.TaskArtifactUpdateEvent:
			s := "artifact:"
			for _, p := range e.Artifact.Parts {
				if p.Text() != "" {
					s += p.Text()
				} else {
					s += p.Filename
				}
			}
			if e.Append {
				s += "+append"
			}
			if e.LastChunk {
				s += "+last"
			}
			got = append(got, s)
		default:
			got = append(got, "other")
		}
	}
	return got
}

// The streaming scenarios of scenarios/streaming.feature: working, the artifact, completed, in that order
// (STREAM-ORDER-001), after the submitted task.
func TestStreamingScenarios(t *testing.T) {
	const sub, working, done = "task:" + string(a2a.TaskStateSubmitted), "status:" + string(a2a.TaskStateWorking), "status:" + string(a2a.TaskStateCompleted)
	cases := map[string][]string{
		"stream-001":              {sub, working, "artifact:" + textStream1, done},
		"stream-002":              {sub, done},
		"stream-003":              {sub, working, "artifact:" + textStream3, done},
		"stream-ordering-001":     {sub, working, "artifact:" + textStreamOrdering, done},
		"stream-artifact-text":    {sub, working, "artifact:" + textStreamArtifact, done},
		"stream-artifact-file":    {sub, working, "artifact:" + fileName, done},
		"stream-artifact-chunked": {sub, working, "artifact:" + textChunk1, "artifact:" + textChunk2 + "+append+last", done},
	}
	for bname, protocol := range bindings() {
		t.Run(bname, func(t *testing.T) {
			c := client(t, serve(t, agent{long: 50 * time.Millisecond}), protocol)
			for name, want := range cases {
				got := stream(t, c, tckID(name))
				if strings.Join(got, " ") != strings.Join(want, " ") {
					t.Errorf("%s: %v, want %v", name, got, want)
				}
			}
			// The resubscription scenario (its id has no tck- prefix): working for agent.long, then completed.
			start := time.Now()
			got := stream(t, c, prefixResubscribe+"-0a1b2c3d")
			if strings.Join(got, " ") != strings.Join([]string{sub, working, done}, " ") || time.Since(start) < 50*time.Millisecond {
				t.Errorf("resubscribe: %v after %v, want submitted, working, completed after 50ms", got, time.Since(start))
			}
		})
	}
}

// Where one prefix starts another, the longer one is the scenario; an id without a known prefix is none.
func TestScenarioPrefixes(t *testing.T) {
	for id, want := range map[string]string{
		tckID("artifact-file-url"):       prefixArtifactURL,
		tckID("artifact-file"):           prefixArtifactFile,
		tckID("stream-artifact-chunked"): prefixStreamChunked,
		tckID("stream-artifact-file"):    prefixStreamFile,
		tckID("stream-ordering-001"):     prefixStreamOrdering,
		tckID("stream-001"):              prefixStream1,
		tckID("input-required") + "-2":   prefixInputRequired,
		"test-resubscribe-message-id-x":  prefixResubscribe,
		tckID("complete-task"):           prefixComplete,
		tckID("multi-004"):               "",
		"":                               "",
	} {
		if got := scenario(id); got != want {
			t.Errorf("scenario(%q) = %q, want %q", id, got, want)
		}
	}
}

// The card: both bindings under the root URL, streaming, no push (the gate's), no required extension, and the
// input modes the enforcer applies.
func TestCard(t *testing.T) {
	base := serve(t, agent{})
	res, err := http.Get(base + "/.well-known/agent-card.json")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var card a2a.AgentCard
	if err := json.NewDecoder(res.Body).Decode(&card); err != nil {
		t.Fatal(err)
	}
	if len(card.SupportedInterfaces) != 2 ||
		card.SupportedInterfaces[0].URL != base || card.SupportedInterfaces[0].ProtocolBinding != a2a.TransportProtocolJSONRPC ||
		card.SupportedInterfaces[1].URL != base+restPrefix || card.SupportedInterfaces[1].ProtocolBinding != a2a.TransportProtocolHTTPJSON {
		t.Errorf("interfaces %+v", card.SupportedInterfaces)
	}
	if !card.Capabilities.Streaming || card.Capabilities.PushNotifications || len(card.Capabilities.Extensions) != 0 {
		t.Errorf("capabilities %+v: want streaming, no push, no extension", card.Capabilities)
	}
	if strings.Join(card.DefaultInputModes, ",") != "text/plain,application/json" {
		t.Errorf("input modes %v", card.DefaultInputModes)
	}
}

// run: the first line on stdout is the root URL, which serves the card; a usage error is 2; ctx's end stops it, 0.
func TestRun(t *testing.T) {
	for _, args := range [][]string{{"extra"}, {"--port", "70000"}, {"--nope"}} {
		if code := run(context.Background(), args, io.Discard, io.Discard); code != 2 {
			t.Errorf("run %v: %d, want 2", args, code)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	pr, pw := io.Pipe()
	var wg sync.WaitGroup
	wg.Add(1)
	code := -1
	go func() {
		defer wg.Done()
		code = run(ctx, []string{"--host", "127.0.0.1", "--port", "0"}, pw, io.Discard)
		pw.Close()
	}()
	line, err := bufio.NewReader(pr).ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "http://127.0.0.1:") {
		t.Fatalf("first line %q (%v), want the root URL", line, err)
	}
	go func() { _, _ = io.Copy(io.Discard, pr) }()
	res, err := http.Get(strings.TrimSpace(line) + "/.well-known/agent-card.json")
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("card: %v %v", err, res)
	}
	res.Body.Close()
	cancel()
	wg.Wait()
	if code != 0 {
		t.Errorf("run ended %d after its context, want 0", code)
	}
}
