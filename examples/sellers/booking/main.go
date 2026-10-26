// Command booking is a small A2A agent, written with a2a-go v2.7.0, that books something for a date. Its first
// turn asks for the date and stops at input-required; a reply with a date confirms the booking and completes
// the task; a reply without one asks again. CancelTask ends a task that has not completed canceled.
//
// It is the examples' input-required seller (examples/README.md, spec/relay.md §7): a plain A2A agent that
// imports nothing from Kontra and has no Kontra-specific endpoint. It serves its card at
// /.well-known/agent-card.json, JSON-RPC at its root and HTTP+JSON under /rest, all under one root URL, so that
// `kontra connect --serve a2a:booking=http://127.0.0.1:9998` can host it (an M2 node hosts an agent at its root
// URL only).
//
//	booking [--host 127.0.0.1] [--port 9998] [--binding both|jsonrpc|http+json]
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
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/eventqueue"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
)

// Bindings the agent can serve (--binding).
const (
	bindingBoth     = "both"
	bindingJSONRPC  = "jsonrpc"
	bindingHTTPJSON = "http+json"
)

// restPrefix is where the HTTP+JSON binding is served; JSON-RPC is served at the root.
const restPrefix = "/rest"

// dateLayout is the only date the agent reads: an ISO 8601 calendar date, alone or as the date of a date-time.
const dateLayout = "2006-01-02"

var dateRE = regexp.MustCompile(`\d{4}-\d{2}-\d{2}`)

// maxBody bounds a request body: the gate's default limits.body (spec/relay.md §2.2), so that the agent takes
// whatever a gate passes on, and one request to an agent exposed directly (--host 0.0.0.0) cannot take the
// process's memory. a2a-go reads a whole body and sets no bound of its own.
const maxBody = 4 << 20

// maxQuote bounds what the agent quotes back of a message, in runes.
const maxQuote = 80

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// run is main without the process: it serves until ctx ends and returns the exit code (2 for a usage error).
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("booking", flag.ContinueOnError)
	fs.SetOutput(stderr)
	host := fs.String("host", "127.0.0.1", "address to listen on (0.0.0.0 in a container)")
	port := fs.Int("port", 9998, "port to listen on (0: any free port)")
	binding := fs.String("binding", bindingBoth, "bindings to serve and list in the card: both, jsonrpc or http+json")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "booking: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	switch *binding {
	case bindingBoth, bindingJSONRPC, bindingHTTPJSON:
	default:
		fmt.Fprintf(stderr, "booking: --binding %q: want both, jsonrpc or http+json\n", *binding)
		return 2
	}
	if *port < 0 || *port > 65535 {
		fmt.Fprintf(stderr, "booking: --port %d: want 0 to 65535\n", *port)
		return 2
	}

	l, err := net.Listen("tcp", net.JoinHostPort(*host, strconv.Itoa(*port)))
	if err != nil {
		fmt.Fprintf(stderr, "booking: %v\n", err)
		return 1
	}
	base := rootURL(l.Addr().(*net.TCPAddr))
	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	srv := &http.Server{Handler: newHandler(agent{}, base, *binding, log), ReadHeaderTimeout: 10 * time.Second}
	fmt.Fprintln(stdout, base)
	fmt.Fprintf(stderr, "booking: serving the card at %s%s, %s\n", base, a2asrv.WellKnownAgentCardPath, describe(*binding))

	done := make(chan error, 1)
	go func() { done <- srv.Serve(l) }()
	select {
	case err := <-done:
		fmt.Fprintf(stderr, "booking: %v\n", err)
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
// agent from elsewhere (a gate, a sidecar) dials its own configured address, not the card's (D71).
func rootURL(a *net.TCPAddr) string {
	ip := a.IP
	if ip == nil || ip.IsUnspecified() {
		ip = net.IPv4(127, 0, 0, 1)
	}
	return "http://" + net.JoinHostPort(ip.String(), strconv.Itoa(a.Port))
}

func describe(binding string) string {
	switch binding {
	case bindingJSONRPC:
		return "JSON-RPC at /"
	case bindingHTTPJSON:
		return "HTTP+JSON under " + restPrefix
	}
	return "JSON-RPC at / and HTTP+JSON under " + restPrefix
}

// newHandler serves a, the agent whose root URL is base, with request bodies bounded by maxBody; a2a-go logs to
// log.
func newHandler(a agent, base, binding string, log *slog.Logger) http.Handler {
	store := taskstore.NewInMemory(nil)
	handler := cancelRetry{
		RequestHandler: a2asrv.NewHandler(a, a2asrv.WithTaskStore(store), a2asrv.WithLogger(log)),
		store:          store,
	}
	mux := http.NewServeMux()
	mux.Handle("GET "+a2asrv.WellKnownAgentCardPath, a2asrv.NewStaticAgentCardHandler(card(base, binding)))
	if binding != bindingHTTPJSON {
		mux.Handle("POST /{$}", a2asrv.NewJSONRPCHandler(handler))
	}
	if binding != bindingJSONRPC {
		mux.Handle(restPrefix+"/", http.StripPrefix(restPrefix, a2asrv.NewRESTHandler(handler)))
	}
	return http.MaxBytesHandler(mux, maxBody)
}

// Bounds of cancelRetry: a2a-go cleans a turn's execution up within milliseconds of its last event.
const (
	cancelRetries   = 50
	cancelRetryWait = 20 * time.Millisecond
)

// cancelRetry makes a CancelTask that lost a race with the end of a turn again, and answers one that lost it to
// a turn that ended the task as a cancel of an ended task is answered.
//
// a2a-go v2.7.0 sends a cancel to the task's execution while one exists, and its own TODO says what goes wrong
// (internal/taskexec/local_manager.go, handleCancelWithConcurrentRun): when that execution has just ended the
// turn at input-required and is being cleaned up, the cancel resolves to that turn's result and fails
// TaskNotCancelableError ("task was in non-cancelable state: input-required"), or "queue is closed", although
// the task is waiting and cancelable. That is the moment a caller is most likely to cancel a booking it no
// longer wants. When the cancel failed that way and the stored task is still cancelable (not in a terminal
// state, or canceled already, which a2a-go answers with the task), the cancel is made again, as that TODO
// suggests, once the execution is gone.
//
// The same race with a reply that books, and so completes the task, can fail "queue is closed" too, which
// a2a-go would send as an internal error (-32603). The task is over then: the answer is
// TaskNotCancelableError, the answer to any cancel of a completed task.
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

// card is the agent's A2A 1.0 card.
func card(base, binding string) *a2a.AgentCard {
	var ifaces []*a2a.AgentInterface
	if binding != bindingHTTPJSON {
		ifaces = append(ifaces, a2a.NewAgentInterface(base, a2a.TransportProtocolJSONRPC))
	}
	if binding != bindingJSONRPC {
		ifaces = append(ifaces, a2a.NewAgentInterface(base+restPrefix, a2a.TransportProtocolHTTPJSON))
	}
	return &a2a.AgentCard{
		Name:                "Booking Agent",
		Description:         "Books what you ask for on a date: it asks for the date, then confirms the booking.",
		Version:             "1.0.0",
		SupportedInterfaces: ifaces,
		DefaultInputModes:   []string{"text/plain"},
		DefaultOutputModes:  []string{"text/plain"},
		Capabilities:        a2a.AgentCapabilities{Streaming: true},
		Skills: []a2a.AgentSkill{{
			ID:          "book",
			Name:        "Book for a date",
			Description: "Asks for the date of what you want booked (input-required), then confirms the booking.",
			Tags:        []string{"booking", "input-required"},
			Examples:    []string{"a table for two", "2026-11-02"},
		}},
	}
}

// agent is the booking agent's a2asrv.AgentExecutor.
type agent struct {
	// pause is nil in the agent. A test sets it to hold a turn or a cancel at a point (the pause* constants),
	// which makes a2a-go's races between the end of a turn and a CancelTask happen every time (main_test.go).
	pause func(at string)
}

// Points at which agent.pause is called.
const (
	pauseAsked   = "asked"   // a turn has sent its input-required event and has not returned yet
	pauseWorking = "working" // a booking turn has sent its working event
	pauseCancel  = "cancel"  // Cancel is about to send the canceled event
)

func (a agent) hold(at string) {
	if a.pause != nil {
		a.pause(at)
	}
}

// Execute runs one turn. A first message asks for a date; a reply carrying a valid YYYY-MM-DD date books it,
// and any other reply asks again.
func (a agent) Execute(_ context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if ec.StoredTask == nil {
			if !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
				return
			}
			if yield(ask(ec, fmt.Sprintf("For which date shall I book %s? Reply with the date as YYYY-MM-DD.", what(ec))), nil) {
				a.hold(pauseAsked)
			}
			return
		}
		reply := text(ec.Message)
		date, ok := parseDate(reply)
		if !ok {
			if yield(ask(ec, fmt.Sprintf("I could not read a date in %s. Reply with the date as YYYY-MM-DD, for example %s.",
				quote(reply), time.Now().AddDate(0, 0, 7).Format(dateLayout))), nil) {
				a.hold(pauseAsked)
			}
			return
		}
		if !yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateWorking, nil), nil) {
			return
		}
		a.hold(pauseWorking)
		booked := fmt.Sprintf("Booked %s on %s, %s.", what(ec), date.Weekday(), date.Format(dateLayout))
		artifact := a2a.NewArtifactEvent(ec, a2a.NewTextPart(booked))
		artifact.Artifact.Name = "booking"
		artifact.LastChunk = true
		if !yield(artifact, nil) {
			return
		}
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted,
			a2a.NewMessageForTask(a2a.MessageRoleAgent, ec, a2a.NewTextPart("Confirmed: "+booked))), nil)
	}
}

// Cancel ends a task that is not over yet canceled, whether it waits for its date or is being booked.
func (a agent) Cancel(_ context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if ec.StoredTask != nil && ec.StoredTask.Status.State.Terminal() {
			yield(nil, a2a.ErrTaskNotCancelable)
			return
		}
		a.hold(pauseCancel)
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCanceled,
			a2a.NewMessageForTask(a2a.MessageRoleAgent, ec, a2a.NewTextPart("The booking was canceled."))), nil)
	}
}

// ask ends the turn at input-required with question.
func ask(ec *a2asrv.ExecutorContext, question string) *a2a.TaskStatusUpdateEvent {
	return a2a.NewStatusUpdateEvent(ec, a2a.TaskStateInputRequired,
		a2a.NewMessageForTask(a2a.MessageRoleAgent, ec, a2a.NewTextPart(question)))
}

// what is what the task books: the text of its first user message, quoted (quote), or "it".
func what(ec *a2asrv.ExecutorContext) string {
	first := ec.Message
	if ec.StoredTask != nil {
		for _, m := range ec.StoredTask.History {
			if m != nil && m.Role == a2a.MessageRoleUser {
				first = m
				break
			}
		}
	}
	if t := strings.TrimSpace(text(first)); t != "" {
		return quote(t)
	}
	return "it"
}

// quote is s quoted, cut to its first maxQuote runes (with an ellipsis) when it is longer: the agent quotes a
// message back in its question and its booking, and a long message is not worth repeating.
func quote(s string) string {
	if utf8.RuneCountInString(s) > maxQuote {
		s = string([]rune(s)[:maxQuote]) + "…"
	}
	return strconv.Quote(s)
}

// parseDate finds the first YYYY-MM-DD in s that is a real calendar date and stands alone: not inside a longer
// word or number. A date followed by T and a time is the date of an ISO 8601 date-time (2026-11-02T19:00:00Z),
// and is read as that date; the time is not used.
func parseDate(s string) (time.Time, bool) {
	for _, m := range dateRE.FindAllStringIndex(s, -1) {
		start, end := m[0], m[1]
		if start > 0 && isWordByte(s[start-1]) {
			continue
		}
		if end < len(s) && isWordByte(s[end]) &&
			!((s[end] == 'T' || s[end] == 't') && end+1 < len(s) && s[end+1] >= '0' && s[end+1] <= '9') {
			continue
		}
		if d, err := time.Parse(dateLayout, s[start:end]); err == nil {
			return d, true
		}
	}
	return time.Time{}, false
}

// isWordByte reports whether b is an ASCII letter, digit or underscore (a word character of \b).
func isWordByte(b byte) bool {
	return b == '_' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

// text is the concatenated text of a message's text parts.
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
