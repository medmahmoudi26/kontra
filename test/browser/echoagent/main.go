// Command echoagent is the browser tests' agent (test/browser): a plain A2A 1.0 agent, a2a-go v2.7.0, that answers
// every message with one result, report.md, holding the message's text as Markdown, and completes; "message only" it
// answers with a bare Message and no task; "stream test" it streams a text result, log, in three appended chunks
// cut mid-word, and sends a result report twice, the second replacing the first; "form updates" it draws an A2UI
// surface while working, then sends five status messages that only update its data model. The tests send it
// Markdown that carries HTML (a script, an onerror handler) to check that the console shows agent content as text and
// runs none of it.
//
//	echoagent [--port 0] [--streaming]
//
// --streaming declares capabilities.streaming on its card, so the gate reaches it with SendStreamingMessage and logs
// each of its events as it comes (the tests' "live" service); without it the gate's SendMessage gets the final task.
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
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
)

type agent struct{}

func (agent) Execute(_ context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		// "message only": a stateless answer, a bare Message and no task (A2A allows either). The caller receives that
		// Message and the gate's record of it is no task (D362); the console shows Callee · Message.
		if ec.StoredTask == nil && len(ec.Message.Parts) > 0 {
			if t, ok := ec.Message.Parts[0].Content.(a2a.Text); ok && string(t) == "message only" {
				yield(a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("A plain answer.")), nil)
				return
			}
		}
		if ec.StoredTask == nil && !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
			return
		}
		if len(ec.Message.Parts) > 0 {
			if t, ok := ec.Message.Parts[0].Content.(a2a.Text); ok && string(t) == "stream test" {
				streamTest(ec, yield)
				return
			}
			if t, ok := ec.Message.Parts[0].Content.(a2a.Text); ok && string(t) == "form updates" {
				formUpdates(ec, yield)
				return
			}
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

// streamTest is "stream test": the console's Artifacts column draws log as one text "Read the code. Then curl it"
// (spec: Streamed Text Shown Continuously) and report as one entry holding "final" (spec: Artifacts Column).
func streamTest(ec *a2asrv.ExecutorContext, yield func(a2a.Event, error) bool) {
	if !yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateWorking, nil), nil) {
		return
	}
	first := a2a.NewArtifactEvent(ec, a2a.NewTextPart("Read the co"))
	first.Artifact.Name = "log"
	if !yield(first, nil) {
		return
	}
	for i, chunk := range []string{"de. Then", " curl it"} {
		time.Sleep(300 * time.Millisecond)
		up := a2a.NewArtifactUpdateEvent(ec, first.Artifact.ID, a2a.NewTextPart(chunk))
		up.Artifact.Name = "log"
		up.LastChunk = i == 1
		if !yield(up, nil) {
			return
		}
	}
	report := a2a.NewArtifactEvent(ec, a2a.NewTextPart("draft"))
	report.Artifact.Name = "report"
	if !yield(report, nil) {
		return
	}
	again := a2a.NewArtifactUpdateEvent(ec, report.Artifact.ID, a2a.NewTextPart("final"))
	again.Artifact.Name = "report"
	again.Append = false
	if !yield(again, nil) {
		return
	}
	yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, a2a.NewMessageForTask(a2a.MessageRoleAgent, ec, a2a.NewTextPart("Streamed."))), nil)
}

// a2uiPart holds A2UI v0.9.1 messages: a data part marked by metadata.mimeType, as examples/sellers/a2ui writes it.
func a2uiPart(msgs ...any) *a2a.Part {
	return &a2a.Part{Content: a2a.Data{Value: msgs}, MediaType: "application/a2ui+json",
		Metadata: map[string]any{"mimeType": "application/a2ui+json"}}
}

// formUpdates is "form updates": a surface showing a step, drawn once, then five messages that only set the step in
// its data model (spec: A2UI Rendering, "Agent updates its form while working"): the surface shows "step 5", and
// the transcript gains nothing for the five.
func formUpdates(ec *a2asrv.ExecutorContext, yield func(a2a.Event, error) bool) {
	id := "progress-" + string(ec.TaskID)
	step := func(n int) map[string]any {
		return map[string]any{"version": "v0.9.1", "updateDataModel": map[string]any{"surfaceId": id, "path": "/progress",
			"value": map[string]any{"step": fmt.Sprintf("step %d", n)}}}
	}
	created := a2uiPart(
		map[string]any{"version": "v0.9.1", "createSurface": map[string]any{"surfaceId": id,
			"catalogId": "https://a2ui.org/specification/v0_9/catalogs/basic/catalog.json"}},
		map[string]any{"version": "v0.9.1", "updateComponents": map[string]any{"surfaceId": id, "components": []any{
			map[string]any{"id": "root", "component": "Column", "children": []any{"label", "step"}},
			map[string]any{"id": "label", "component": "Text", "text": "Progress"},
			map[string]any{"id": "step", "component": "Text", "text": map[string]any{"path": "/progress/step"}},
		}}},
		step(0),
	)
	if !yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateWorking, a2a.NewMessageForTask(a2a.MessageRoleAgent, ec, created)), nil) {
		return
	}
	for n := 1; n <= 5; n++ {
		time.Sleep(150 * time.Millisecond)
		if !yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateWorking, a2a.NewMessageForTask(a2a.MessageRoleAgent, ec, a2uiPart(step(n)))), nil) {
			return
		}
	}
	yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, a2a.NewMessageForTask(a2a.MessageRoleAgent, ec, a2a.NewTextPart("Done."))), nil)
}

func (agent) Cancel(_ context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCanceled, nil), nil)
	}
}

func main() {
	port := flag.Int("port", 0, "port on 127.0.0.1 (0: any free port)")
	streaming := flag.Bool("streaming", false, "declare capabilities.streaming on the card")
	flag.Parse()
	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *port))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	base := "http://" + l.Addr().String()
	card := &a2a.AgentCard{
		// A long description that carries markup: the Cards page clamps it under its arrow, and shows it as text.
		Name: "Echo", Version: "1.0.0", Description: "Answers with the message's text as a Markdown result. " +
			"It keeps every line of what it was sent, headings, lists and code included, and adds nothing of its own, " +
			"so what the console draws is exactly what an agent's Markdown looks like. " +
			`<img src=x onerror="window.__pwned = 4"> stays text. Use it to check how a thread renders results.`,
		SupportedInterfaces: []*a2a.AgentInterface{a2a.NewAgentInterface(base, a2a.TransportProtocolJSONRPC)},
		DefaultInputModes:   []string{"text/plain"}, DefaultOutputModes: []string{"text/markdown"},
		Capabilities:        a2a.AgentCapabilities{Streaming: *streaming},
		Skills: []a2a.AgentSkill{{ID: "echo", Name: "Echo", Description: "Echoes Markdown.", Examples: []string{"**hello**"},
			Tags: []string{"markdown"}, InputModes: []string{"text/plain"}, OutputModes: []string{"text/markdown"}}},
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
