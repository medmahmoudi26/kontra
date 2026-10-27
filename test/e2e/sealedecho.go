package e2e

import (
	"context"
	"io"
	"iter"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

// EchoSeller is the sealed tests' A2A seller (sealedecho in the M3 plan): an a2a-go v2.7.0 agent behind a hosting
// node that repeats its input in an artifact and in its status message. A first message whose first text part
// starts with "ask" stops at input-required ("which one of <text>?"), and its follow-up completes ("done: <text>");
// any other first message completes at once. Every text part comes back as "echo: <text>" in the artifact, and every
// raw part as the same bytes with its mediaType and filename. Its card offers A2A 1.0 JSON-RPC at /rpc, which sealing
// needs (D165).
type EchoSeller struct {
	URL string

	srv *http.Server
	l   net.Listener

	mu    sync.Mutex
	turns [][]*a2a.Part
	sends int
}

// AskPrefix starts a first message that asks one follow-up.
const echoAskPrefix = "ask"

const echoCard = `{"name":"sealedecho","description":"repeats its input","version":"1.0.0",
"supportedInterfaces":[{"url":"http://127.0.0.1:9/rpc","protocolBinding":"JSONRPC","protocolVersion":"1.0"}],
"capabilities":{"streaming":true},"defaultInputModes":["text/plain"],"defaultOutputModes":["text/plain"],
"skills":[{"id":"echo","name":"echo","description":"repeats its input","tags":["e2e"]}]}`

// StartEchoSeller serves the seller on 127.0.0.1 until the harness closes.
func (h *Harness) StartEchoSeller() (*EchoSeller, error) {
	s := &EchoSeller{}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s.l = l
	s.URL = "http://" + l.Addr().String()
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/agent-card.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, echoCard)
	})
	mux.Handle("/rpc", a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(s)))
	s.srv = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			s.mu.Lock()
			s.sends++
			s.mu.Unlock()
		}
		mux.ServeHTTP(w, r)
	})}
	go func() { _ = s.srv.Serve(l) }()
	h.OnClose(func() error {
		_ = s.srv.Close()
		return nil
	})
	return s, nil
}

// Turns are the parts of every message the seller executed, in order.
func (s *EchoSeller) Turns() [][]*a2a.Part {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]*a2a.Part(nil), s.turns...)
}

// Requests is the number of POSTs the seller received (every JSON-RPC request that reached it).
func (s *EchoSeller) Requests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sends
}

// Execute runs one turn.
func (s *EchoSeller) Execute(_ context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		s.mu.Lock()
		s.turns = append(s.turns, ec.Message.Parts)
		s.mu.Unlock()
		text := ""
		var echo []*a2a.Part
		for _, p := range ec.Message.Parts {
			switch {
			case p.Raw() != nil:
				echo = append(echo, &a2a.Part{Content: a2a.Raw(p.Raw()), MediaType: p.MediaType, Filename: p.Filename})
				if text == "" && strings.HasPrefix(p.MediaType, "text/") {
					text = string(p.Raw()) // a file kontra seal sealed (a raw part) is read as its text
				}
			default:
				if text == "" {
					text = p.Text()
				}
				echo = append(echo, a2a.NewTextPart("echo: "+p.Text()))
			}
		}
		if ec.StoredTask == nil {
			if !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
				return
			}
		}
		if !yield(a2a.NewArtifactEvent(ec, echo...), nil) {
			return
		}
		if ec.StoredTask == nil && strings.HasPrefix(text, echoAskPrefix) {
			yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateInputRequired,
				a2a.NewMessageForTask(a2a.MessageRoleAgent, ec, a2a.NewTextPart("which one of "+text+"?"))), nil)
			return
		}
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted,
			a2a.NewMessageForTask(a2a.MessageRoleAgent, ec, a2a.NewTextPart("done: "+text))), nil)
	}
}

// Cancel cancels.
func (s *EchoSeller) Cancel(_ context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCanceled, nil), nil)
	}
}
