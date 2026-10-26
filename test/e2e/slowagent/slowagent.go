// Package slowagent is the e2e upstream: an A2A agent written with a2a-go v2.7.0 (local mode) that
// streams Chunks artifact chunks Interval apart and then completes.
//
// It keeps running its task when the gate's streaming connection drops (a2a-go runs executions detached
// from the request), serves GetTask and SubscribeToTask so a re-attaching dispatch can follow it (D42),
// and counts SendMessage and SendStreamingMessage calls so a test can prove the gate never sent the
// message twice. It records every request it receives (method, headers, body), so a test can prove the
// gate never forwarded a caller's Authorization, cookies or inline push config (spec §6.2, §10.1), and the
// context ids it was sent and assigned (spec §4, D53).
//
// A first message whose text starts with "ask" stops at input-required; the follow-up that answers it
// streams the chunks and completes (spec §7). CancelTask ends a task canceled at once, whatever it is
// doing (spec §8), and is recorded with the task it names and the Host it arrived with, so a test can tell
// a cancel forwarded through `kontra connect` (the target's Host, no X-Forwarded-*) from any other. With
// FirstEventDelay a task's first turn yields nothing for that long, so its upstream task id stays unknown
// to the gate meanwhile (the tunnel tests' before-the-first-upstream-id variants, spec §6.4). With TalksAfterCancel the agent is a seller that ignores its own cancel: the stream that
// carried the canceled status goes on with one more chunk and a completed status, which a2a-go itself
// never sends (its processor ends an execution at the canceled event and drops what it yields after).
//
// With Legacy025 the agent is the a2a-samples helloworld seller at protocol 0.2.5 (card_compat): it serves
// that seller's card at /.well-known/agent.json, speaks the 0.x JSON-RPC binding at its root, and answers
// every message with a "Hello World" Message, as that seller does.
package slowagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2acompat/a2av0"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
)

// Config shapes the agent.
type Config struct {
	Chunks    int           // artifact chunks before completed
	Interval  time.Duration // between chunks
	Streaming bool          // the card says capabilities.streaming
	Binding   string        // "JSONRPC" (default) or "HTTP+JSON"
	// Legacy025 makes the agent the a2a-samples helloworld seller at protocol 0.2.5 (card_compat); Chunks,
	// Interval and Binding are then ignored.
	Legacy025 bool
	// TalksAfterCancel (JSON-RPC binding): a SendStreamingMessage stream that ends with the canceled status
	// goes on with an artifact chunk and a completed status for the same task, written right after it on
	// the same response (cancel_forwarded: the gate must log neither, spec §8).
	TalksAfterCancel bool
	// FirstEventDelay holds a first turn's first event (the submitted task) back this long: until it
	// passes, the gate has no upstream task id (spec §6.4's before-the-first-id rows).
	FirstEventDelay time.Duration
}

// ChunkMarker starts the text of every chunk the agent streams, so a test can look for chunk text where it
// must never be (Temporal history).
const ChunkMarker = "slowagent-chunk"

// AskPrefix starts a first message that stops at input-required.
const AskPrefix = "ask"

// HelloWorld is what the Legacy025 seller answers.
const HelloWorld = "Hello World"

// Legacy025Card is the a2a-samples helloworld seller's card at 08dde55c070f (protocol 0.2.5), as that seller
// serves it, with its url left as %s.
const Legacy025Card = `{"capabilities":{"streaming":true},"defaultInputModes":["text"],"defaultOutputModes":["text"],"description":"Just a hello world agent","name":"Hello World Agent","protocolVersion":"0.2.5","skills":[{"description":"just returns hello world","examples":["hi","hello world"],"id":"hello_world","name":"Returns hello world","tags":["hello world"]}],"supportsAuthenticatedExtendedCard":true,"url":"%s","version":"1.0.0"}`

// Request is one HTTP request the agent received.
type Request struct {
	Method string      // the JSON-RPC method, or "METHOD /path" on HTTP+JSON and for card GETs
	Host   string      // the request's Host (net/http keeps it out of Header)
	Header http.Header // as received
	Body   []byte
	// The message's ids, when the request carries a message (a send). TaskID is also the task a JSON-RPC
	// CancelTask, GetTask or SubscribeToTask names (params.id).
	MessageID, TaskID, ContextID string
}

// Turn is one Execute call: a task's first turn or a follow-up.
type Turn struct {
	TaskID    a2a.TaskID
	ContextID string
	Text      string
	FollowUp  bool // the message named a task the agent already had
}

// Agent is one running upstream.
type Agent struct {
	cfg     Config
	store   *taskstore.InMemory
	handler a2asrv.RequestHandler
	sends   atomic.Int64
	cancels atomic.Int64
	late    atomic.Int64

	mu       sync.Mutex
	requests []Request
	turns    []Turn
	stops    map[a2a.TaskID]chan struct{}
	srv      *http.Server
	base     string
}

// New returns an agent; Start serves it.
func New(cfg Config) *Agent {
	if cfg.Binding == "" {
		cfg.Binding = string(a2a.TransportProtocolJSONRPC)
	}
	a := &Agent{cfg: cfg, store: taskstore.NewInMemory(nil), stops: map[a2a.TaskID]chan struct{}{}}
	a.handler = a2asrv.NewHandler(a, a2asrv.WithTaskStore(a.store))
	return a
}

// Start serves the agent on 127.0.0.1 and returns its base URL (the backend url a gate configures, D71).
// It stops when ctx ends or Close is called.
func (a *Agent) Start(ctx context.Context) (string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	a.base = "http://" + l.Addr().String()
	mux := http.NewServeMux()
	if a.cfg.Legacy025 {
		card := fmt.Sprintf(Legacy025Card, a.base+"/")
		mux.HandleFunc("GET /.well-known/agent.json", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, card)
		})
		mux.Handle("POST /{$}", a2av0.NewJSONRPCHandler(a.handler))
	} else {
		mux.HandleFunc("GET /.well-known/agent-card.json", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(a.Card())
		})
		if a.cfg.Binding == string(a2a.TransportProtocolHTTPJSON) {
			mux.Handle("/rest/", http.StripPrefix("/rest", a2asrv.NewRESTHandler(a.handler)))
		} else {
			mux.Handle("POST /rpc", a2asrv.NewJSONRPCHandler(a.handler))
		}
	}
	a.srv = &http.Server{Handler: a.record(mux), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = a.srv.Serve(l) }()
	go func() {
		<-ctx.Done()
		a.Close()
	}()
	return a.base, nil
}

// Close stops serving and ends every running task's execution.
func (a *Agent) Close() {
	a.mu.Lock()
	for id, c := range a.stops {
		closeOnce(c)
		delete(a.stops, id)
	}
	srv := a.srv
	a.mu.Unlock()
	if srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if srv.Shutdown(ctx) != nil {
			_ = srv.Close()
		}
	}
}

// URL is the agent's base URL, empty before Start.
func (a *Agent) URL() string { return a.base }

// Card is the agent's 1.0 card (not used with Legacy025).
func (a *Agent) Card() []byte {
	iface := map[string]string{"url": a.base + "/rpc", "protocolBinding": "JSONRPC", "protocolVersion": "1.0"}
	if a.cfg.Binding == string(a2a.TransportProtocolHTTPJSON) {
		iface = map[string]string{"url": a.base + "/rest", "protocolBinding": "HTTP+JSON", "protocolVersion": "1.0"}
	}
	b, _ := json.Marshal(map[string]any{
		"name": "slowagent", "description": "streams artifact chunks slowly", "version": "1.0.0",
		"supportedInterfaces": []any{iface},
		"capabilities":        map[string]any{"streaming": a.cfg.Streaming},
		"defaultInputModes":   []string{"text/plain"}, "defaultOutputModes": []string{"text/plain"},
		"skills": []any{map[string]any{"id": "slow", "name": "slow", "description": "streams chunks", "tags": []string{"e2e"}}},
	})
	return b
}

// Sends is the number of SendMessage and SendStreamingMessage calls the agent received (0.x: message/send
// and message/stream).
func (a *Agent) Sends() int64 {
	return a.sends.Load()
}

// Cancels is the number of times the agent's Cancel ran: a CancelTask that reached a task it holds.
func (a *Agent) Cancels() int64 {
	return a.cancels.Load()
}

// LateEvents is the number of events written after a canceled status (TalksAfterCancel).
func (a *Agent) LateEvents() int64 {
	return a.late.Load()
}

// Requests returns a copy of every request received so far, in arrival order.
func (a *Agent) Requests() []Request {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]Request(nil), a.requests...)
}

// Calls returns the requests received whose Method is method, in arrival order (CancelTask: the cancels
// that reached the agent, each with the task it named and the Host it arrived with).
func (a *Agent) Calls(method string) []Request {
	var out []Request
	for _, r := range a.Requests() {
		if r.Method == method {
			out = append(out, r)
		}
	}
	return out
}

// Count is the number of requests received whose Method is method.
func (a *Agent) Count(method string) int {
	n := 0
	for _, r := range a.Requests() {
		if r.Method == method {
			n++
		}
	}
	return n
}

// Turns returns every Execute call so far, in order.
func (a *Agent) Turns() []Turn {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]Turn(nil), a.turns...)
}

// Task is the agent's own view of one of its tasks.
func (a *Agent) Task(ctx context.Context, id a2a.TaskID) (*a2a.Task, error) {
	st, err := a.store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return st.Task, nil
}

// Executor is the agent's a2asrv.AgentExecutor.
func (a *Agent) Executor() a2asrv.AgentExecutor { return a }

// record counts sends and keeps every request, then serves it.
func (a *Agent) record(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
		if err != nil {
			http.Error(w, "read", http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		req := Request{Method: r.Method + " " + r.URL.Path, Host: r.Host, Header: r.Header.Clone(), Body: body}
		var env struct {
			Method string `json:"method"`
			Params struct {
				Message *msgIDs `json:"message"`
				ID      string  `json:"id"`
			} `json:"params"`
			Message *msgIDs `json:"message"`
		}
		if r.Method == http.MethodPost && json.Unmarshal(body, &env) == nil {
			m := env.Message
			if env.Method != "" {
				req.Method = env.Method
				m = env.Params.Message
				req.TaskID = env.Params.ID
			}
			if m != nil {
				req.MessageID, req.TaskID, req.ContextID = m.MessageID, m.TaskID, m.ContextID
			}
		}
		switch {
		case req.Method == "SendMessage", req.Method == "SendStreamingMessage",
			req.Method == "message/send", req.Method == "message/stream",
			strings.HasSuffix(r.URL.Path, "/message:send"), strings.HasSuffix(r.URL.Path, "/message:stream"):
			a.sends.Add(1)
		}
		a.mu.Lock()
		a.requests = append(a.requests, req)
		a.mu.Unlock()
		if a.cfg.TalksAfterCancel && req.Method == "SendStreamingMessage" {
			tw := &teeWriter{ResponseWriter: w}
			next.ServeHTTP(tw, r)
			a.talkAfterCancel(tw)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// teeWriter keeps what a2a-go wrote on a streaming response.
type teeWriter struct {
	http.ResponseWriter
	buf bytes.Buffer
}

func (t *teeWriter) Write(b []byte) (int, error) {
	t.buf.Write(b)
	return t.ResponseWriter.Write(b)
}

func (t *teeWriter) Flush() {
	if f, ok := t.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// talkAfterCancel writes, after a2a-go's stream ended with the canceled status, an artifact chunk and a
// completed status for the same task on the same response, framed as a2a-go frames its events.
func (a *Agent) talkAfterCancel(tw *teeWriter) {
	var last []byte
	for _, line := range bytes.Split(tw.buf.Bytes(), []byte("\n")) {
		if rest, ok := bytes.CutPrefix(line, []byte("data: ")); ok {
			last = rest
		}
	}
	var env struct {
		ID     json.RawMessage    `json:"id"`
		Result a2a.StreamResponse `json:"result"`
	}
	if last == nil || json.Unmarshal(last, &env) != nil {
		return
	}
	su, ok := env.Result.Event.(*a2a.TaskStatusUpdateEvent)
	if !ok || su.Status.State != a2a.TaskStateCanceled {
		return
	}
	info := a2a.TaskInfo{TaskID: su.TaskID, ContextID: su.ContextID}
	for i, ev := range []a2a.Event{
		a2a.NewArtifactEvent(info, a2a.NewTextPart(ChunkMarker+" after the cancel")),
		a2a.NewStatusUpdateEvent(info, a2a.TaskStateCompleted, nil),
	} {
		data, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": env.ID, "result": a2a.StreamResponse{Event: ev}})
		if err != nil {
			return
		}
		if _, err := fmt.Fprintf(tw.ResponseWriter, "id: late-%d\ndata: %s\n\n", i, data); err != nil {
			return
		}
		tw.Flush()
		a.late.Add(1)
	}
}

type msgIDs struct {
	MessageID string `json:"messageId"`
	TaskID    string `json:"taskId"`
	ContextID string `json:"contextId"`
}

func closeOnce(c chan struct{}) {
	select {
	case <-c:
	default:
		close(c)
	}
}

func (a *Agent) stopChan(id a2a.TaskID) chan struct{} {
	a.mu.Lock()
	defer a.mu.Unlock()
	c, ok := a.stops[id]
	if !ok {
		c = make(chan struct{})
		a.stops[id] = c
	}
	return c
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

// Execute implements a2asrv.AgentExecutor.
func (a *Agent) Execute(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	t := text(ec.Message)
	followUp := ec.StoredTask != nil
	a.mu.Lock()
	a.turns = append(a.turns, Turn{TaskID: ec.TaskID, ContextID: ec.ContextID, Text: t, FollowUp: followUp})
	a.mu.Unlock()
	if a.cfg.Legacy025 {
		return func(yield func(a2a.Event, error) bool) {
			yield(a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(HelloWorld)), nil)
		}
	}
	stop := a.stopChan(ec.TaskID)
	return func(yield func(a2a.Event, error) bool) {
		if !followUp && a.cfg.FirstEventDelay > 0 {
			select {
			case <-time.After(a.cfg.FirstEventDelay):
			case <-stop:
				return
			case <-ctx.Done():
				return
			}
		}
		if !followUp && !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
			return
		}
		if !yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateWorking, nil), nil) {
			return
		}
		if !followUp && strings.HasPrefix(t, AskPrefix) {
			yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateInputRequired,
				a2a.NewMessageForTask(a2a.MessageRoleAgent, ec, a2a.NewTextPart("which one?"))), nil)
			return
		}
		var artifact a2a.ArtifactID
		for i := 1; i <= a.cfg.Chunks; i++ {
			select {
			case <-time.After(a.cfg.Interval):
			case <-stop:
				return
			case <-ctx.Done():
				return
			}
			part := a2a.NewTextPart(fmt.Sprintf("%s %03d of %03d", ChunkMarker, i, a.cfg.Chunks))
			var ev *a2a.TaskArtifactUpdateEvent
			if i == 1 {
				ev = a2a.NewArtifactEvent(ec, part)
				artifact = ev.Artifact.ID
			} else {
				ev = a2a.NewArtifactUpdateEvent(ec, artifact, part)
			}
			ev.LastChunk = i == a.cfg.Chunks
			if !yield(ev, nil) {
				return
			}
		}
		select {
		case <-stop:
			return
		default:
		}
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, nil), nil)
	}
}

// Cancel implements a2asrv.AgentExecutor: the task ends canceled at once, and its execution stops.
func (a *Agent) Cancel(_ context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	a.cancels.Add(1)
	closeOnce(a.stopChan(ec.TaskID))
	return func(yield func(a2a.Event, error) bool) {
		if ec.StoredTask != nil && ec.StoredTask.Status.State.Terminal() {
			yield(nil, a2a.ErrTaskNotCancelable)
			return
		}
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCanceled, nil), nil)
	}
}
