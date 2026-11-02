// Command a2ui is a small A2A 1.0 agent, written with a2a-go v2.7.0 and no LLM, that takes a table booking through an
// A2UI v0.9.1 form (https://a2ui.org): a name, the number of guests and a date and time.
//
// Its card declares two extensions, neither required: A2UI v0.9.1 (https://a2ui.org/a2a-extension/a2ui/v0.9.1, with
// the basic catalog as its one catalog) and the input-schema extension (spec/ext-input-schema.md), which is a JSON
// entry of the card like any seller's own. A first message stops at input-required with the form, as A2UI messages in
// one data part, when the client renders A2UI; the form's Book button sends an A2UI action whose context holds the
// three fields, and the agent completes with a text that sums the booking up and deletes the form. Each task's form is
// its own surface, so one context holds several. A client that renders no A2UI is asked in text for the input
// schema's data part instead, and a first message that already carries that data part, complete, is booked at once.
//
// It is an example seller (examples/README.md): a plain A2A agent that imports nothing from Kontra and has no
// Kontra-specific endpoint. It serves its card at /.well-known/agent-card.json, JSON-RPC at its root and HTTP+JSON
// under /rest, all under one root URL, so that `kontra connect --serve a2a:a2ui=http://127.0.0.1:9995` can host it.
//
//	a2ui [--host 127.0.0.1] [--port 9995]
//
// The first line on standard output is the agent's root URL (with the port chosen when --port is 0). SIGINT or
// SIGTERM stops it. Tasks live in memory: a restart forgets them.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
)

// A2UI v0.9.1 over A2A (the A2UI repository's specification/v0_9_1/docs/a2ui_extension_specification.md).
const (
	// ExtA2UI is the A2UI extension's URI: a client that sends it in A2A-Extensions renders A2UI v0.9.1.
	ExtA2UI = "https://a2ui.org/a2a-extension/a2ui/v0.9.1"
	// MimeA2UI marks a data part as A2UI messages, in the part's metadata.mimeType.
	MimeA2UI = "application/a2ui+json"
	// mimeA2UILegacy is the name v0.8 and v0.9 used; it is read, never written.
	mimeA2UILegacy = "application/json+a2ui"
	// BasicCatalog is the id of the basic catalog of v0.9 and v0.9.1, the only catalog this agent builds with.
	BasicCatalog = "https://a2ui.org/specification/v0_9/catalogs/basic/catalog.json"
	// capsKey is the message metadata key under which a client lists what it renders, by A2UI version.
	capsKey = "a2uiClientCapabilities"
)

// The A2UI versions the agent writes in its messages' version: v0.9.1, or v0.9 to a client that says it reads only
// v0.9 (negotiate). v0.9.1's schemas accept both; v0.9's accept only "v0.9", and differ from v0.9.1's in nothing else.
const (
	v091 = "v0.9.1"
	v09  = "v0.9"
)

// ExtInputSchema is the input-schema extension's URI (spec/ext-input-schema.md): the card says what a first
// message's data part may hold.
const ExtInputSchema = "https://kontra.sh/ext/input-schema/v1"

// The form. A task's surface is surfacePrefix and the task's id: A2UI refuses a createSurface for an id a client
// still shows, and a context (A2UI's session) holds as many forms as it has tasks.
const (
	surfacePrefix = "booking-"
	actionBook    = "book"
	modelPath     = "/booking"
	maxGuests     = 6
	maxName       = 100 // runes
)

// restPrefix is where the HTTP+JSON binding is served; JSON-RPC is served at the root.
const restPrefix = "/rest"

// maxBody bounds a request body: the gate's default limits.body (spec/relay.md §2.2), as the booking example does.
const maxBody = 4 << 20

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// run is main without the process: it serves until ctx ends and returns the exit code (2 for a usage error).
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("a2ui", flag.ContinueOnError)
	fs.SetOutput(stderr)
	host := fs.String("host", "127.0.0.1", "address to listen on (0.0.0.0 in a container)")
	port := fs.Int("port", 9995, "port to listen on (0: any free port)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "a2ui: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if *port < 0 || *port > 65535 {
		fmt.Fprintf(stderr, "a2ui: --port %d: want 0 to 65535\n", *port)
		return 2
	}
	l, err := net.Listen("tcp", net.JoinHostPort(*host, strconv.Itoa(*port)))
	if err != nil {
		fmt.Fprintf(stderr, "a2ui: %v\n", err)
		return 1
	}
	base := rootURL(l.Addr().(*net.TCPAddr))
	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	srv := &http.Server{Handler: newHandler(base, log), ReadHeaderTimeout: 10 * time.Second}
	fmt.Fprintln(stdout, base)
	fmt.Fprintf(stderr, "a2ui: serving the card at %s%s, JSON-RPC at / and HTTP+JSON under %s\n", base, a2asrv.WellKnownAgentCardPath, restPrefix)

	done := make(chan error, 1)
	go func() { done <- srv.Serve(l) }()
	select {
	case err := <-done:
		fmt.Fprintf(stderr, "a2ui: %v\n", err)
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

// rootURL is the URL the card advertises. A wildcard listener is advertised on loopback: whoever reaches the agent
// from elsewhere (a gate, a sidecar) dials its own configured address, not the card's (D71).
func rootURL(a *net.TCPAddr) string {
	ip := a.IP
	if ip == nil || ip.IsUnspecified() {
		ip = net.IPv4(127, 0, 0, 1)
	}
	return "http://" + net.JoinHostPort(ip.String(), strconv.Itoa(a.Port))
}

// newHandler serves the agent whose root URL is base, with request bodies bounded by maxBody; a2a-go logs to log.
func newHandler(base string, log *slog.Logger) http.Handler {
	handler := a2asrv.NewHandler(agent{}, a2asrv.WithTaskStore(taskstore.NewInMemory(nil)), a2asrv.WithLogger(log))
	raw, err := cardJSON(base)
	if err != nil {
		panic(err) // the card is this file's own constant data
	}
	mux := http.NewServeMux()
	mux.Handle("GET "+a2asrv.WellKnownAgentCardPath, a2asrv.NewAgentCardHandler(rawCard(raw)))
	mux.Handle("POST /{$}", a2asrv.NewJSONRPCHandler(handler))
	mux.Handle(restPrefix+"/", http.StripPrefix(restPrefix, a2asrv.NewRESTHandler(handler)))
	return http.MaxBytesHandler(mux, maxBody)
}

// rawCard serves a card's JSON as it is (a2asrv.AgentCardJSONProducer).
type rawCard []byte

func (c rawCard) Card(context.Context) (*a2a.AgentCard, error) {
	var card a2a.AgentCard
	err := json.Unmarshal(c, &card)
	return &card, err
}

func (c rawCard) CardJSON(context.Context) ([]byte, error) { return c, nil }

// card is the agent's A2A 1.0 card.
func card(base string) *a2a.AgentCard {
	return &a2a.AgentCard{
		Name:        "Table Booking (A2UI)",
		Description: "Books a table through an A2UI v0.9.1 form: a name, the number of guests and a date and time, then a confirmation.",
		Version:     "1.0.0",
		SupportedInterfaces: []*a2a.AgentInterface{
			a2a.NewAgentInterface(base, a2a.TransportProtocolJSONRPC),
			a2a.NewAgentInterface(base+restPrefix, a2a.TransportProtocolHTTPJSON),
		},
		DefaultInputModes:  []string{"text/plain", "application/json", MimeA2UI},
		DefaultOutputModes: []string{"text/plain", MimeA2UI},
		Capabilities:       a2a.AgentCapabilities{Streaming: true, Extensions: extensions()},
		Skills: []a2a.AgentSkill{{
			ID:          "book",
			Name:        "Book a table",
			Description: "Shows a booking form (A2UI v0.9.1, basic catalog) and confirms the name, guests and date it is sent.",
			Tags:        []string{"booking", "a2ui", "input-required"},
			Examples:    []string{"a table, please"},
		}},
	}
}

// extensions are the card's two extension entries.
func extensions() []a2a.AgentExtension {
	return []a2a.AgentExtension{
		{
			URI:         ExtA2UI,
			Description: "Shows its booking form as A2UI v0.9.1 messages, built with the basic catalog.",
			Params:      map[string]any{"supportedCatalogIds": []any{BasicCatalog}},
		},
		{
			URI:         ExtInputSchema,
			Description: "The booking a first message's data part may hold.",
			Params:      map[string]any{"schema": inputSchema(), "mediaType": "application/json"},
		},
	}
}

// inputSchema is the input schema's JSON Schema: the booking, as the form's action context holds it, with the
// number of guests as a number.
func inputSchema() map[string]any {
	return map[string]any{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"type":    "object",
		"properties": map[string]any{
			"name":   map[string]any{"type": "string", "minLength": 1, "maxLength": maxName, "description": "The name the table is booked under."},
			"guests": map[string]any{"type": "integer", "minimum": 1, "maximum": maxGuests, "description": "How many guests."},
			"date":   map[string]any{"type": "string", "format": "date-time", "description": "When, as an RFC 3339 date-time."},
		},
		"required":             []any{"name", "guests", "date"},
		"additionalProperties": false,
	}
}

// cardJSON is the card as served. a2a-go's AgentExtension leaves out a false `required`; the A2UI and input-schema
// specifications write it out, so the served entries do too.
func cardJSON(base string) ([]byte, error) {
	raw, err := json.Marshal(card(base))
	if err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	var caps map[string]json.RawMessage
	if err := json.Unmarshal(m["capabilities"], &caps); err != nil {
		return nil, err
	}
	type entry struct {
		URI         string         `json:"uri"`
		Description string         `json:"description"`
		Required    bool           `json:"required"`
		Params      map[string]any `json:"params"`
	}
	var entries []entry
	for _, e := range extensions() {
		entries = append(entries, entry{URI: e.URI, Description: e.Description, Required: e.Required, Params: e.Params})
	}
	if caps["extensions"], err = json.Marshal(entries); err != nil {
		return nil, err
	}
	if m["capabilities"], err = json.Marshal(caps); err != nil {
		return nil, err
	}
	return json.Marshal(m)
}

// agent is the agent's a2asrv.AgentExecutor.
type agent struct{}

// Execute runs one turn: a booking that checks out (an A2UI action, or the input schema's data part) completes the
// task; anything else asks for it (input-required).
func (agent) Execute(_ context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if ec.StoredTask == nil && !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
			return
		}
		in := read(ec.Message)
		problems := in.booking.check()
		var question string
		switch {
		case in.found && len(problems) == 0:
			parts := append([]*a2a.Part{a2a.NewTextPart(in.booking.summary())}, closeForm(ec)...)
			yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, a2a.NewMessageForTask(a2a.MessageRoleAgent, ec, parts...)), nil)
			return
		case in.found:
			question = strings.Join(problems, " ") + " "
		case ec.StoredTask != nil:
			question = "That was not a booking. "
		}
		version, ui := negotiate(ec)
		yield(ask(ec, question, in.booking, version, ui), nil)
	}
}

// Cancel ends a task that is not over yet canceled, deleting its form.
func (agent) Cancel(_ context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if ec.StoredTask != nil && ec.StoredTask.Status.State.Terminal() {
			yield(nil, a2a.ErrTaskNotCancelable)
			return
		}
		parts := append([]*a2a.Part{a2a.NewTextPart("The booking was canceled.")}, closeForm(ec)...)
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCanceled, a2a.NewMessageForTask(a2a.MessageRoleAgent, ec, parts...)), nil)
	}
}

// What the agent asks: of a client that renders A2UI, before and after it has the form; of one that renders none.
const (
	askForm    = "Fill in the booking form: a name, the number of guests and a date and time."
	askCorrect = "Correct the booking form and press Book."
	askData    = `To book a table, reply with a data part {"name": "…", "guests": 1 to 6, "date": "2026-11-09T19:30:00Z"} ` +
		`(this agent's input schema), or ask again from a client that renders A2UI v0.9.1.`
)

// ask ends the turn at input-required with why (what was wrong, or nothing) and the question. A client that renders
// A2UI (ui) is sent the form in version, holding what b holds, unless the task has sent it the form already: a
// surface is created once, and the one the client has keeps what the person typed.
func ask(ec *a2asrv.ExecutorContext, why string, b booking, version string, ui bool) *a2a.TaskStatusUpdateEvent {
	parts := []*a2a.Part{a2a.NewTextPart(why + askData)}
	_, _, sent := shown(ec)
	switch {
	case ui && sent:
		parts = []*a2a.Part{a2a.NewTextPart(why + askCorrect)}
	case ui:
		parts = []*a2a.Part{a2a.NewTextPart(why + askForm), a2uiPart(surface(surfacePrefix+string(ec.TaskID), version, b))}
	}
	return a2a.NewStatusUpdateEvent(ec, a2a.TaskStateInputRequired, a2a.NewMessageForTask(a2a.MessageRoleAgent, ec, parts...))
}

// closeForm is what ends a task's form, for the message that ends the task: a deleteSurface, in the form's own
// version, when the task sent one; nothing otherwise. The client stops showing a form whose Book has nothing left to
// book.
func closeForm(ec *a2asrv.ExecutorContext) []*a2a.Part {
	id, version, ok := shown(ec)
	if !ok {
		return nil
	}
	return []*a2a.Part{a2uiPart([]any{map[string]any{"version": version, "deleteSurface": map[string]any{"surfaceId": id}}})}
}

// a2uiPart holds A2UI messages: a data part whose data is the list of messages, marked by metadata.mimeType (the
// A2UI extension's data encoding). mediaType, A2A 1.0's own field, says the same to a client that reads only it. The
// mark is v0.9.1's name in either version, as A2UI's own Node sample agent marks its v0.9 messages and A2UI's Lit
// sample client, which lists only v0.9, marks its actions.
func a2uiPart(msgs []any) *a2a.Part {
	return &a2a.Part{Content: a2a.Data{Value: msgs}, MediaType: MimeA2UI, Metadata: map[string]any{"mimeType": MimeA2UI}}
}

// shown is the form the agent has sent this task, if it has: the surface id and the A2UI version of the first
// createSurface in its messages' A2UI parts (the history, then the question the task waits on).
func shown(ec *a2asrv.ExecutorContext) (id, version string, ok bool) {
	if ec.StoredTask == nil {
		return "", "", false
	}
	for _, m := range append(slices.Clone(ec.StoredTask.History), ec.StoredTask.Status.Message) {
		if m == nil || m.Role != a2a.MessageRoleAgent {
			continue
		}
		for _, p := range m.Parts {
			if !isA2UI(p) {
				continue
			}
			msgs, _ := p.Data().([]any)
			for _, msg := range msgs {
				mm, _ := msg.(map[string]any)
				create, _ := mm["createSurface"].(map[string]any)
				if sid, _ := create["surfaceId"].(string); sid != "" {
					v, _ := mm["version"].(string)
					return sid, v, true
				}
			}
		}
	}
	return "", "", false
}

// --- the form, as A2UI v0.9.x messages (basic catalog) ---

// surface is the form on the surface id, in version: createSurface, updateComponents and updateDataModel, in that
// order.
func surface(id, version string, b booking) []any {
	bound := func(field string) map[string]any { return map[string]any{"path": modelPath + "/" + field} }
	required := func(field, message string) map[string]any {
		return map[string]any{
			"condition": map[string]any{"call": "required", "args": map[string]any{"value": bound(field)}},
			"message":   message,
		}
	}
	var options []any
	for n := 1; n <= maxGuests; n++ {
		label := strconv.Itoa(n) + " guests"
		if n == 1 {
			label = "1 guest"
		}
		options = append(options, map[string]any{"label": label, "value": strconv.Itoa(n)})
	}
	nameCheck, dateCheck := required("name", "A name is required."), required("date", "A date and time are required.")
	components := []any{
		map[string]any{"id": "root", "component": "Column", "children": []any{"title", "name", "guests", "date", "book"}},
		map[string]any{"id": "title", "component": "Text", "text": "Book a table", "variant": "h2"},
		map[string]any{"id": "name", "component": "TextField", "label": "Name for the booking", "value": bound("name"),
			"checks": []any{nameCheck}},
		map[string]any{"id": "guests", "component": "ChoicePicker", "label": "Guests", "variant": "mutuallyExclusive",
			"options": options, "value": bound("guests")},
		map[string]any{"id": "date", "component": "DateTimeInput", "label": "Date and time", "value": bound("date"),
			"enableDate": true, "enableTime": true, "checks": []any{dateCheck}},
		map[string]any{"id": "book_label", "component": "Text", "text": "Book"},
		map[string]any{"id": "book", "component": "Button", "child": "book_label", "variant": "primary",
			"checks": []any{nameCheck, dateCheck},
			"action": map[string]any{"event": map[string]any{"name": actionBook, "context": map[string]any{
				"name": bound("name"), "guests": bound("guests"), "date": bound("date")}}}},
	}
	return []any{
		map[string]any{"version": version, "createSurface": map[string]any{"surfaceId": id, "catalogId": BasicCatalog}},
		map[string]any{"version": version, "updateComponents": map[string]any{"surfaceId": id, "components": components}},
		dataModel(id, version, b),
	}
}

// dataModel sets the form's values to b's: the fields that checked out, and two guests when the number did not. The
// ChoicePicker's value is a list of strings.
func dataModel(id, version string, b booking) map[string]any {
	guests := 2
	if b.guests > 0 {
		guests = b.guests
	}
	value := map[string]any{"name": b.name, "guests": []any{strconv.Itoa(guests)}, "date": b.date}
	return map[string]any{"version": version, "updateDataModel": map[string]any{"surfaceId": id, "path": modelPath, "value": value}}
}

// --- reading a message ---

// incoming is what a message holds that the agent reads: a booking, from the form's action or from a data part of
// the input schema's shape.
type incoming struct {
	found   bool
	booking booking
}

// read takes the first booking a message holds: an A2UI action named book (v0.9.1's action, or v0.8's
// userAction), in a data part marked A2UI whose data is a list of messages or one message; else the first data
// part, not marked A2UI, whose data is an object with a name, guests or date (a client's other data parts, such as
// the A2UI samples' {"useStreaming": false}, are not bookings). Its fields are kept as far as they check out.
func read(m *a2a.Message) incoming {
	if m == nil {
		return incoming{}
	}
	for _, p := range m.Parts {
		if !isA2UI(p) {
			continue
		}
		msgs, ok := p.Data().([]any)
		if !ok {
			msgs = []any{p.Data()}
		}
		for _, msg := range msgs {
			mm, _ := msg.(map[string]any)
			act, _ := mm["action"].(map[string]any)
			if act == nil {
				act, _ = mm["userAction"].(map[string]any)
			}
			if act == nil || act["name"] != actionBook {
				continue
			}
			ctx, _ := act["context"].(map[string]any)
			return incoming{found: true, booking: parse(ctx)}
		}
	}
	for _, p := range m.Parts {
		obj, ok := p.Data().(map[string]any)
		if !ok || isA2UI(p) {
			continue
		}
		for _, field := range []string{"name", "guests", "date"} {
			if _, ok := obj[field]; ok {
				return incoming{found: true, booking: parse(obj)}
			}
		}
	}
	return incoming{}
}

// isA2UI reports whether p is a data part marked as A2UI messages, by metadata.mimeType or by mediaType, under
// either name.
func isA2UI(p *a2a.Part) bool {
	if p == nil {
		return false
	}
	if _, ok := p.Content.(a2a.Data); !ok {
		return false
	}
	mt, _ := p.Metadata["mimeType"].(string)
	for _, v := range []string{mt, p.MediaType} {
		if v == MimeA2UI || v == mimeA2UILegacy {
			return true
		}
	}
	return false
}

// negotiate reports whether the client renders this agent's form (ui), and in which A2UI version. Its
// a2uiClientCapabilities (message metadata, else the request's), when it sent them, decide whether: an entry for
// v0.9.1 or v0.9 that lists the basic catalog. Without them, a client that activated the extension (A2A-Extensions,
// its 0.3 name X-A2A-Extensions, or the message's extensions) renders the card's one catalog. The version is v0.9.1
// when the client said it reads v0.9.1, by a v0.9.1 entry that lists the basic catalog or by activating the v0.9.1
// URI, and v0.9 otherwise: a client that lists the catalog under v0.9 alone may be a v0.9 renderer, whose schemas
// refuse "v0.9.1", and a v0.9.1 renderer reads "v0.9" too.
func negotiate(ec *a2asrv.ExecutorContext) (version string, ui bool) {
	on := activated(ec)
	caps, sent := capabilities(ec)
	if !sent {
		return v091, on
	}
	lists := func(version string) bool {
		entry, _ := caps[version].(map[string]any)
		ids, _ := entry["supportedCatalogIds"].([]any)
		return slices.Contains(ids, any(BasicCatalog))
	}
	switch {
	case lists(v091), on && lists(v09):
		return v091, true
	case lists(v09):
		return v09, true
	}
	return "", false
}

// activated reports whether the client activated the A2UI v0.9.1 extension: in A2A-Extensions, its 0.3 name
// X-A2A-Extensions, or the message's extensions.
func activated(ec *a2asrv.ExecutorContext) bool {
	if ec.Message != nil && slices.Contains(ec.Message.Extensions, ExtA2UI) {
		return true
	}
	for _, header := range []string{a2a.SvcParamExtensions, "X-A2A-Extensions"} {
		values, _ := ec.ServiceParams.Get(header)
		for _, v := range values {
			for _, uri := range strings.Split(v, ",") {
				if strings.TrimSpace(uri) == ExtA2UI {
					return true
				}
			}
		}
	}
	return false
}

func capabilities(ec *a2asrv.ExecutorContext) (map[string]any, bool) {
	if ec.Message != nil {
		if caps, ok := ec.Message.Metadata[capsKey].(map[string]any); ok {
			return caps, true
		}
	}
	caps, ok := ec.Metadata[capsKey].(map[string]any)
	return caps, ok
}

// --- the booking ---

// booking is what the form or the data part held, each field kept only when it checks out (0 and "" otherwise).
type booking struct {
	name   string
	guests int
	date   string    // as sent
	when   time.Time // date, read
	timed  bool      // date had a time
	zone   bool      // date named its offset
	bad    []string  // what did not check out, in the order parse reads the fields
}

// Layouts a date may take: RFC 3339 (the input schema's date-time), a DateTimeInput's local date and time, with or
// without seconds, and a date alone.
var layouts = []struct {
	layout      string
	timed, zone bool
}{
	{time.RFC3339, true, true},
	{"2006-01-02T15:04:05", true, false},
	{"2006-01-02T15:04", true, false},
	{"2006-01-02", false, false},
}

// parse reads a booking from an object: name (a string), guests (a whole number 1 to 6, as a number or a string, or
// a list of one, as a ChoicePicker's value is) and date (one of layouts).
func parse(obj map[string]any) booking {
	var b booking
	if s, ok := obj["name"].(string); ok && strings.TrimSpace(s) != "" && utf8.RuneCountInString(strings.TrimSpace(s)) <= maxName {
		b.name = strings.TrimSpace(s)
	} else {
		b.bad = append(b.bad, fmt.Sprintf("The booking needs a name of at most %d characters.", maxName))
	}
	if n, ok := guests(obj["guests"]); ok {
		b.guests = n
	} else {
		b.bad = append(b.bad, fmt.Sprintf("The number of guests must be 1 to %d.", maxGuests))
	}
	s, _ := obj["date"].(string)
	s = strings.TrimSpace(s)
	dated := false
	for _, l := range layouts {
		if t, err := time.Parse(l.layout, s); err == nil {
			b.date, b.when, b.timed, b.zone, dated = s, t, l.timed, l.zone, true
			break
		}
	}
	if !dated {
		b.bad = append(b.bad, "The date must be a date and time, like 2026-11-09T19:30.")
	}
	return b
}

func guests(v any) (int, bool) {
	switch x := v.(type) {
	case []any:
		if len(x) == 1 {
			return guests(x[0])
		}
	case float64:
		if x == math.Trunc(x) && x >= 1 && x <= maxGuests {
			return int(x), true
		}
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(x)); err == nil && n >= 1 && n <= maxGuests {
			return n, true
		}
	}
	return 0, false
}

// check is what is wrong with the booking, nothing when it can be booked.
func (b booking) check() []string { return b.bad }

// summary is the confirmation of a booking that checked out.
func (b booking) summary() string {
	table := fmt.Sprintf("a table for %d", b.guests)
	when := b.when.Format("Monday, 2 January 2006")
	if b.timed {
		when += " at " + b.when.Format("15:04")
		if b.zone {
			when += b.when.Format(" -07:00")
		}
	}
	return fmt.Sprintf("Booked %s under %s on %s.", table, strconv.Quote(b.name), when)
}
