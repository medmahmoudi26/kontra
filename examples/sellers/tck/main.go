// Command tck is the seller the pinned A2A TCK runs against through the gate (spec/relay.md §0, §16 `tck_*`;
// D18, DM4-12): a plain A2A agent, written with a2a-go v2.7.0, that does what the TCK's Gherkin scenarios
// (a2a-tck d4d6fae, scenarios/core_operations.feature and scenarios/streaming.feature) ask of a system under
// test. The TCK signals the behaviour it wants in band, with the prefix of the message id: `tck-<scenario>-<8
// hex digits>` (a2a-tck tck/requirements/base.py, tck_id), and the gate passes message ids upstream unchanged
// (spec/relay.md §4). Everything else, the error answers, the history and the task ids, is the gate's.
//
// It imports nothing from Kontra and has no Kontra-specific endpoint (examples/README.md). It serves its card at
// /.well-known/agent-card.json, JSON-RPC at its root and HTTP+JSON under /rest, all under one root URL, so that
// a gate reaches it as a direct backend (`backend: { a2a: { url } }`) or `kontra connect --serve
// a2a:tck=<root URL>` hosts it through the tunnel.
//
//	tck [--host 127.0.0.1] [--port 9996]
//
// The first line on standard output is the agent's root URL (with the port chosen when --port is 0).
// SIGINT or SIGTERM stops it. Tasks live in memory: a restart forgets them.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/eventqueue"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
)

// restPrefix is where the HTTP+JSON binding is served; JSON-RPC is served at the root.
const restPrefix = "/rest"

// maxBody bounds a request body: the gate's default limits.body (spec/relay.md §2.2). a2a-go reads a whole
// body and sets no bound of its own.
const maxBody = 4 << 20

// longWait is how long the resubscription scenario stays working: twice the 2 s streaming timeout of the TCK's
// own generated seller (a2a-tck sut/a2a-python, "wait for 2x streaming timeout").
const longWait = 4 * time.Second

// inputModes are the media types the agent reads: what the TCK sends, text and JSON. a2a-go's card enforcer
// answers any other with ContentTypeNotSupportedError, as spec §3.1.1 asks of an agent (the TCK's CORE-SEND-003
// sends application/x-unsupported-tck-type). The gate does not enforce input modes; the upstream does (D14).
var inputModes = []string{"text/plain", "application/json"}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// run is main without the process: it serves until ctx ends and returns the exit code (2 for a usage error).
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("tck", flag.ContinueOnError)
	fs.SetOutput(stderr)
	host := fs.String("host", "127.0.0.1", "address to listen on (0.0.0.0 in a container)")
	port := fs.Int("port", 9996, "port to listen on (0: any free port)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "tck: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if *port < 0 || *port > 65535 {
		fmt.Fprintf(stderr, "tck: --port %d: want 0 to 65535\n", *port)
		return 2
	}

	l, err := net.Listen("tcp", net.JoinHostPort(*host, strconv.Itoa(*port)))
	if err != nil {
		fmt.Fprintf(stderr, "tck: %v\n", err)
		return 1
	}
	base := rootURL(l.Addr().(*net.TCPAddr))
	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	h, err := newHandler(agent{long: longWait}, base, log)
	if err != nil {
		_ = l.Close()
		fmt.Fprintf(stderr, "tck: %v\n", err)
		return 1
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	fmt.Fprintln(stdout, base)
	fmt.Fprintf(stderr, "tck: serving the card at %s%s, JSON-RPC at / and HTTP+JSON under %s\n",
		base, a2asrv.WellKnownAgentCardPath, restPrefix)

	done := make(chan error, 1)
	go func() { done <- srv.Serve(l) }()
	select {
	case err := <-done:
		fmt.Fprintf(stderr, "tck: %v\n", err)
		return 1
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		_ = srv.Close()
	}
	return 0
}

// rootURL is the URL the card advertises. A wildcard listener is advertised on loopback: whoever reaches the
// agent from elsewhere (a gate, a node) dials its own configured address, not the card's (D71).
func rootURL(a *net.TCPAddr) string {
	ip := a.IP
	if ip == nil || ip.IsUnspecified() {
		ip = net.IPv4(127, 0, 0, 1)
	}
	return "http://" + net.JoinHostPort(ip.String(), strconv.Itoa(a.Port))
}

// newHandler serves a, the agent whose root URL is base, with request bodies bounded by maxBody; a2a-go logs to
// log.
func newHandler(a agent, base string, log *slog.Logger) (http.Handler, error) {
	c := card(base)
	enforcer, err := a2asrv.NewAgentCardEnforcer(c)
	if err != nil {
		return nil, err
	}
	store := taskstore.NewInMemory(nil)
	handler := cancelRetry{
		RequestHandler: a2asrv.NewHandler(a, a2asrv.WithTaskStore(store), a2asrv.WithLogger(log),
			a2asrv.WithCallInterceptors(enforcer)),
		store: store,
	}
	mux := http.NewServeMux()
	mux.Handle("GET "+a2asrv.WellKnownAgentCardPath, a2asrv.NewStaticAgentCardHandler(c))
	mux.Handle("POST /{$}", a2asrv.NewJSONRPCHandler(handler))
	mux.Handle(restPrefix+"/", http.StripPrefix(restPrefix, a2asrv.NewRESTHandler(handler)))
	return http.MaxBytesHandler(mux, maxBody), nil
}

// card is the agent's A2A 1.0 card. The gate serves its own card made from it (spec/relay.md §3): its
// interfaces, streaming and push notifications are the gate's, the rest is this card's. Push notifications are
// the gate's (§10), so the agent declares none; it has no required extension, since the TCK never sends
// A2A-Extensions (a2a-tck#193).
func card(base string) *a2a.AgentCard {
	return &a2a.AgentCard{
		Name:        "TCK seller",
		Description: "Does what the A2A TCK's scenarios ask, keyed on the message id's prefix (a2a-tck d4d6fae).",
		Version:     "1.0.0",
		SupportedInterfaces: []*a2a.AgentInterface{
			a2a.NewAgentInterface(base, a2a.TransportProtocolJSONRPC),
			a2a.NewAgentInterface(base+restPrefix, a2a.TransportProtocolHTTPJSON),
		},
		DefaultInputModes:  inputModes,
		DefaultOutputModes: []string{"text/plain", "application/json"},
		Capabilities:       a2a.AgentCapabilities{Streaming: true},
		Skills: []a2a.AgentSkill{{
			ID:          "tck",
			Name:        "TCK scenarios",
			Description: "Completes, asks for input, rejects, streams and returns artifacts as the TCK's message id says.",
			Tags:        []string{"tck"},
		}},
	}
}

// Message id prefixes of the TCK's scenarios. Where one prefix starts another (tck-artifact-file and
// tck-artifact-file-url, tck-stream-… and tck-stream-artifact-…), the longer one is matched first (scenario).
const (
	prefixComplete       = "tck-complete-task"
	prefixArtifactText   = "tck-artifact-text"
	prefixArtifactFile   = "tck-artifact-file"
	prefixArtifactURL    = "tck-artifact-file-url"
	prefixArtifactData   = "tck-artifact-data"
	prefixMessage        = "tck-message-response"
	prefixInputRequired  = "tck-input-required"
	prefixReject         = "tck-reject-task"
	prefixStream1        = "tck-stream-001"
	prefixStream2        = "tck-stream-002"
	prefixStream3        = "tck-stream-003"
	prefixStreamOrdering = "tck-stream-ordering-001"
	prefixStreamText     = "tck-stream-artifact-text"
	prefixStreamFile     = "tck-stream-artifact-file"
	prefixStreamChunked  = "tck-stream-artifact-chunked"
	prefixResubscribe    = "test-resubscribe-message-id"
)

// Texts and values the scenarios name.
const (
	textComplete       = "Hello from TCK"
	textArtifact       = "Generated text content"
	textMessage        = "Direct message response"
	textRejected       = "rejected"
	textStream1        = "Stream hello from TCK"
	textStream3        = "Stream task lifecycle"
	textStreamOrdering = "Ordered output"
	textStreamArtifact = "Streamed text content"
	textChunk1         = "chunk-1 "
	textChunk2         = "chunk-2"
	fileName           = "output.txt"
	fileMediaType      = "text/plain"
	fileURL            = "https://example.com/output.txt"
	unhandled          = "Unhandled messageId prefix: "
)

// fileContent is the file artifact's content (the TCK's generated seller writes the same bytes).
var fileContent = []byte("tck")

// dataValue is the data artifact's value.
func dataValue() map[string]any { return map[string]any{"key": "value", "count": 42} }

// scenarios, longest prefix first where one prefix starts another, so the first match is the right one.
var scenarios = []string{
	prefixComplete,
	prefixArtifactText,
	prefixArtifactURL,
	prefixArtifactFile,
	prefixArtifactData,
	prefixMessage,
	prefixInputRequired,
	prefixReject,
	prefixStreamChunked,
	prefixStreamText,
	prefixStreamFile,
	prefixStreamOrdering,
	prefixStream1,
	prefixStream2,
	prefixStream3,
	prefixResubscribe,
}

// scenario is the scenario a message id names, "" for none.
func scenario(messageID string) string {
	for _, p := range scenarios {
		if strings.HasPrefix(messageID, p) {
			return p
		}
	}
	return ""
}

// agent is the TCK seller's a2asrv.AgentExecutor.
type agent struct {
	// long is how long the resubscription scenario stays working (longWait; a test shortens it).
	long time.Duration
}

// Execute runs one turn: what the scenario its message id names asks for. A turn on a task that does not exist
// yet starts it (submitted) first, except the one that answers with a message and no task. A follow-up on a
// task that waits for input is a turn like any other: the TCK completes an input-required task with a
// tck-complete-task message.
func (a agent) Execute(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		id := ""
		if ec.Message != nil {
			id = ec.Message.ID
		}
		which := scenario(id)
		if which == prefixMessage {
			yield(a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(textMessage)), nil)
			return
		}
		if ec.StoredTask == nil {
			if !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
				return
			}
		}
		// Each step yields one event and reports whether the turn goes on (the caller may have gone).
		status := func(s a2a.TaskState, m *a2a.Message) bool { return yield(a2a.NewStatusUpdateEvent(ec, s, m), nil) }
		artifact := func(p ...*a2a.Part) bool { return yield(a2a.NewArtifactEvent(ec, p...), nil) }
		say := func(text string) *a2a.Message {
			return a2a.NewMessageForTask(a2a.MessageRoleAgent, ec, a2a.NewTextPart(text))
		}
		// working, then the artifact, then completed: the streaming scenarios' order (STREAM-ORDER-001).
		stream := func(p *a2a.Part) {
			_ = status(a2a.TaskStateWorking, nil) && artifact(p) && status(a2a.TaskStateCompleted, nil)
		}

		switch which {
		case prefixComplete:
			status(a2a.TaskStateCompleted, say(textComplete))
		case prefixArtifactText:
			_ = artifact(a2a.NewTextPart(textArtifact)) && status(a2a.TaskStateCompleted, nil)
		case prefixArtifactURL:
			p := a2a.NewFileURLPart(a2a.URL(fileURL), fileMediaType)
			p.Filename = fileName
			_ = artifact(p) && status(a2a.TaskStateCompleted, nil)
		case prefixArtifactFile:
			_ = artifact(filePart()) && status(a2a.TaskStateCompleted, nil)
		case prefixArtifactData:
			_ = artifact(a2a.NewDataPart(dataValue())) && status(a2a.TaskStateCompleted, nil)
		case prefixInputRequired:
			status(a2a.TaskStateInputRequired, nil)
		case prefixReject:
			status(a2a.TaskStateRejected, say(textRejected))
		case prefixStream1:
			stream(a2a.NewTextPart(textStream1))
		case prefixStream2:
			status(a2a.TaskStateCompleted, nil)
		case prefixStream3:
			stream(a2a.NewTextPart(textStream3))
		case prefixStreamOrdering:
			stream(a2a.NewTextPart(textStreamOrdering))
		case prefixStreamText:
			stream(a2a.NewTextPart(textStreamArtifact))
		case prefixStreamFile:
			stream(filePart())
		case prefixStreamChunked:
			if !status(a2a.TaskStateWorking, nil) {
				return
			}
			first := a2a.NewArtifactEvent(ec, a2a.NewTextPart(textChunk1))
			if !yield(first, nil) {
				return
			}
			last := a2a.NewArtifactUpdateEvent(ec, first.Artifact.ID, a2a.NewTextPart(textChunk2))
			last.LastChunk = true
			_ = yield(last, nil) && status(a2a.TaskStateCompleted, nil)
		case prefixResubscribe:
			if !status(a2a.TaskStateWorking, nil) {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(a.long):
			}
			status(a2a.TaskStateCompleted, nil)
		default:
			status(a2a.TaskStateCompleted, say(unhandled+id))
		}
	}
}

// filePart is the file the file scenarios return: output.txt, text/plain, "tck".
func filePart() *a2a.Part {
	p := a2a.NewRawPart(fileContent)
	p.MediaType = fileMediaType
	p.Filename = fileName
	return p
}

// Cancel ends a task that is not over yet canceled.
func (a agent) Cancel(_ context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if ec.StoredTask != nil && ec.StoredTask.Status.State.Terminal() {
			yield(nil, a2a.ErrTaskNotCancelable)
			return
		}
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCanceled, nil), nil)
	}
}

// Bounds of cancelRetry: a2a-go cleans a turn's execution up within milliseconds of its last event.
const (
	cancelRetries   = 50
	cancelRetryWait = 20 * time.Millisecond
)

// cancelRetry makes a CancelTask that lost a race with the end of a turn again (as examples/sellers/booking does;
// the examples share no code). a2a-go v2.7.0 sends a cancel to the task's execution while one exists; when that
// execution has just ended the turn at input-required and is being cleaned up, the cancel resolves to the
// turn's result and fails TaskNotCancelableError, or "queue is closed", although the task waits and can be
// canceled (internal/taskexec/local_manager.go, handleCancelWithConcurrentRun, and its TODO). The TCK cancels an
// input-required task right after creating it (CORE-CANCEL-001). When the cancel failed that way and the stored
// task is still cancelable, it is made again once the execution is gone; when the stored task ended otherwise,
// the answer is TaskNotCancelableError.
type cancelRetry struct {
	a2asrv.RequestHandler
	store taskstore.Store
}

// CancelTask implements a2asrv.RequestHandler.
func (h cancelRetry) CancelTask(ctx context.Context, req *a2a.CancelTaskRequest) (*a2a.Task, error) {
	for try := 1; ; try++ {
		task, err := h.RequestHandler.CancelTask(ctx, req)
		if err == nil || req == nil || (!errors.Is(err, a2a.ErrTaskNotCancelable) && !errors.Is(err, eventqueue.ErrQueueClosed)) {
			return task, err
		}
		stored, gerr := h.store.Get(ctx, req.ID)
		if gerr != nil || stored == nil || stored.Task == nil {
			return task, err
		}
		if state := stored.Task.Status.State; state.Terminal() && state != a2a.TaskStateCanceled {
			if errors.Is(err, a2a.ErrTaskNotCancelable) {
				return task, err
			}
			return nil, fmt.Errorf("task %s is %s: %w", req.ID, state, a2a.ErrTaskNotCancelable)
		}
		if try == cancelRetries {
			return task, err
		}
		select {
		case <-ctx.Done():
			return task, err
		case <-time.After(cancelRetryWait):
		}
	}
}
