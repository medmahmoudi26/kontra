// Command echoagent is the browser tests' agent (test/browser): a plain A2A 1.0 agent, a2a-go v2.7.0, that answers
// every message with one result, report.md, holding the message's text as Markdown, and completes. The tests send it
// Markdown that carries HTML (a script, an onerror handler) to check that the console shows agent content as text and
// runs none of it.
//
//	echoagent [--port 0]
//
// The first line on standard output is its root URL.
package main

import (
	"context"
	"flag"
	"fmt"
	"iter"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
)

type agent struct{}

func (agent) Execute(_ context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if ec.StoredTask == nil && !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
			return
		}
		var text []string
		for _, p := range ec.Message.Parts {
			if t, ok := p.Content.(a2a.Text); ok {
				text = append(text, string(t))
			}
		}
		md := a2a.NewTextPart(strings.Join(text, "\n"))
		md.MediaType = "text/markdown"
		art := a2a.NewArtifactEvent(ec, md)
		art.Artifact.Name = "report.md"
		if !yield(art, nil) {
			return
		}
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, a2a.NewMessageForTask(a2a.MessageRoleAgent, ec, a2a.NewTextPart("Echoed."))), nil)
	}
}

func (agent) Cancel(_ context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCanceled, nil), nil)
	}
}

func main() {
	port := flag.Int("port", 0, "port on 127.0.0.1 (0: any free port)")
	flag.Parse()
	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *port))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	base := "http://" + l.Addr().String()
	card := &a2a.AgentCard{
		Name: "Echo", Description: "Answers with the message's text as a Markdown result.", Version: "1.0.0",
		SupportedInterfaces: []*a2a.AgentInterface{a2a.NewAgentInterface(base, a2a.TransportProtocolJSONRPC)},
		DefaultInputModes:   []string{"text/plain"}, DefaultOutputModes: []string{"text/markdown"},
		Skills: []a2a.AgentSkill{{ID: "echo", Name: "Echo", Description: "Echoes Markdown.", Examples: []string{"**hello**"}}},
	}
	h := a2asrv.NewHandler(agent{}, a2asrv.WithTaskStore(taskstore.NewInMemory(nil)))
	mux := http.NewServeMux()
	mux.Handle("GET "+a2asrv.WellKnownAgentCardPath, a2asrv.NewStaticAgentCardHandler(card))
	mux.Handle("POST /{$}", a2asrv.NewJSONRPCHandler(h))
	fmt.Println(base)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	srv := &http.Server{Handler: mux}
	go func() { <-ctx.Done(); _ = srv.Close() }()
	_ = srv.Serve(l)
}
