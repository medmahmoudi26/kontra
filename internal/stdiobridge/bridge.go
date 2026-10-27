// Package stdiobridge is kontra connect's bridge for --serve mcp:<name>=stdio:<command line>
// (spec/tunnel.md §10, spec/relay.md §11.10): the node owns the server process and the gate sees a Streamable
// HTTP target like any other. It is an in-process http.Handler on tunnel streams and listens on no port.
//
//   - The command line is one argument, split with POSIX shell word rules, a leading ~/ expanded, run without a
//     shell in a scrubbed environment (PATH, HOME, LANG and the variables --env names). Its stderr goes to the
//     node's log, capped at 1 MiB a minute; the node never stops reading its stdout.
//   - One child per MCP session (2025-11-25 has one initialize per process): an initialize without a session id
//     spawns a child, forwards the message and returns a node-minted Mcp-Session-Id.
//   - Routing: a response to the POST that carried its id; a message carrying a pending request's
//     progressToken to that request's POST stream; one carrying the related-task key to that task's open
//     tasks/result POST; everything else, other server-to-client requests included, to the session's GET
//     stream, queued while none is open (256 messages or 1 MiB, oldest dropped and logged) and flushed in order
//     when one opens, with event ids <child start>-<n> that a GET's Last-Event-ID replays after.
//   - Roots never leave the node (D173, internal/mcproots): the child's roots/list is answered by the bridge
//     from Config.Roots, the client's roots capability is replaced by the node's, and the client's
//     notifications/roots/list_changed is dropped (stdio_roots). Messages are read with exact keys (D172).
//   - Each child runs in its own process group (Setpgid). DELETE closes the child's stdin, sends the group
//     SIGTERM after TerminateAfter and SIGKILL KillAfter later, so an npx launcher leaves no node behind (D184);
//     a child idle for --session-idle (default 1 h, at least the gate's session_idle_s) is stopped the same way
//     and its session answers 404, as does a crashed child's, whose group is stopped too. At most
//     --max-sessions (8) children per service; an initialize beyond is 503 Kontra-Node: full.
//   - Legacy only in v4.0 (D161): a request without a session id that is not initialize, a 2026-07-28 request
//     among them, is 400 with the legacy body of spec/relay.md §11.12 rule 3, so a dual-era client falls back.
//
// The bridge speaks Streamable HTTP as a legacy server does: an initialize is answered with one JSON response and
// the session id; every other request with an event stream that carries what is routed to it and ends after its
// response; a notification or a response from the client with 202. The GET stream is one per session: a GET that
// opens while another is open takes its place (the older one ends), as a client that reconnects after a lost stream
// expects. A child's session belongs to the tunnel session that opened it (WithOwner): a request for it from
// another tunnel session is 404, and CloseOwner stops the children of a tunnel session that closed, since the gate
// ends their MCP sessions with it (relay.md §11.2: a pinned session dies with its tunnel).
//
// A durable legacy task on a stdio service costs one child for as long as it runs, because its dispatch opens its
// own session (D22): --max-sessions bounds concurrent durable tasks as well as clients' sessions.
//
// Work package: M3 WP-05.
package stdiobridge

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"kontra.sh/kontra/internal/mcproots"
	"kontra.sh/kontra/internal/mcpwire"
	"kontra.sh/kontra/internal/tunnel"
)

// Defaults and bounds (spec/tunnel.md §10).
const (
	DefaultMaxSessions = 8
	DefaultSessionIdle = time.Hour
	QueueMessages      = 256
	QueueBytes         = 1 << 20
	TerminateAfter     = 5 * time.Second // stdin closed, then SIGTERM to the process group
	KillAfter          = 5 * time.Second // then SIGKILL to the group (D184)
	StderrPerMinute    = 1 << 20

	// MaxMessage bounds one message: a POST body, a line of the child's stdout. A longer line is dropped and
	// logged (the gate refuses an event over 4 MiB anyway, upstream.MaxSSEEvent).
	MaxMessage = 8 << 20
	// maxStdinQueue bounds what waits for the child to read its stdin; a client message beyond is 503 full.
	maxStdinQueue = 16 << 20
	// maxStreamQueue bounds what waits on one POST stream for the gate to read it; beyond, the stream ends.
	maxStreamQueue = 16 << 20
)

// Config is one stdio service.
type Config struct {
	Service string
	Command []string // SplitCommand's result
	Env     []string // names from --env, passed through from the node's environment
	// LookupEnv reads the node's environment for PATH, HOME, LANG and Env; nil is os.LookupEnv.
	LookupEnv   func(string) (string, bool)
	SessionIdle time.Duration
	MaxSessions int
	Stderr      io.Writer       // the node's log sink for the children's stderr, line by line; nil: the bridge's logger
	Roots       *mcproots.Roots // --roots for this service; nil: none, roots/list is answered with an empty list
	// TerminateAfter and KillAfter replace the package's defaults when set (tests shorten them).
	TerminateAfter time.Duration
	KillAfter      time.Duration
}

// envNameRE is a variable name --env may pass.
var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// CheckEnvName refuses a --env value that is not a variable name (a NAME=value among them: the value always comes
// from kontra connect's own environment).
func CheckEnvName(name string) error {
	if !envNameRE.MatchString(name) {
		return fmt.Errorf("--env %q: want a variable name (its value comes from kontra connect's environment)", clip(name))
	}
	return nil
}

// Bridge serves one stdio service's exchanges.
type Bridge struct {
	cfg Config
	log *slog.Logger
	env []string // the children's environment

	mu       sync.Mutex
	sessions map[string]*child   // by session id, once its initialize was answered
	all      map[*child]struct{} // every child whose group is not stopped yet (counted against MaxSessions)
	reserved int                 // children being spawned
	closed   bool
}

// New checks cfg; children are spawned by initialize requests. It starts nothing.
func New(cfg Config, log *slog.Logger) (*Bridge, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if len(cfg.Command) == 0 || cfg.Command[0] == "" {
		return nil, errors.New("stdiobridge: no command")
	}
	if cfg.MaxSessions <= 0 {
		cfg.MaxSessions = DefaultMaxSessions
	}
	if cfg.SessionIdle <= 0 {
		cfg.SessionIdle = DefaultSessionIdle
	}
	if cfg.TerminateAfter <= 0 {
		cfg.TerminateAfter = TerminateAfter
	}
	if cfg.KillAfter <= 0 {
		cfg.KillAfter = KillAfter
	}
	lookup := cfg.LookupEnv
	if lookup == nil {
		lookup = os.LookupEnv
	}
	b := &Bridge{cfg: cfg, sessions: map[string]*child{}, all: map[*child]struct{}{},
		log: log.With("component", "stdio-bridge", "service", cfg.Service)}
	seen := map[string]bool{}
	for _, k := range append([]string{"PATH", "HOME", "LANG"}, cfg.Env...) {
		if err := CheckEnvName(k); err != nil {
			return nil, err
		}
		if seen[k] {
			continue
		}
		seen[k] = true
		if v, ok := lookup(k); ok {
			b.env = append(b.env, k+"="+v)
		}
	}
	if b.env == nil {
		b.env = []string{} // never nil: exec would pass the node's whole environment
	}
	return b, nil
}

// ownerKey is the context key of WithOwner.
type ownerKey struct{}

// WithOwner marks a request as one of owner's, a comparable value naming the tunnel session it came on: a child's
// session is reachable only from the owner whose initialize spawned it, and CloseOwner stops them.
func WithOwner(ctx context.Context, owner any) context.Context {
	return context.WithValue(ctx, ownerKey{}, owner)
}

func ownerOf(ctx context.Context) any { return ctx.Value(ownerKey{}) }

// ServeHTTP serves one exchange from a tunnel stream: POST, GET or DELETE on the service's MCP path.
func (b *Bridge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	owner := ownerOf(r.Context())
	sid := r.Header.Get(mcpwire.HeaderSessionID)
	switch r.Method {
	case http.MethodPost:
		b.post(w, r, owner, sid)
	case http.MethodGet, http.MethodDelete:
		if sid == "" {
			writeJSON(w, http.StatusBadRequest, mcpwire.LegacyNoSessionBody)
			return
		}
		c := b.lookup(sid, owner)
		if c == nil {
			sessionNotFound(w, nil)
			return
		}
		if r.Method == http.MethodGet {
			c.serveGET(w, r)
			return
		}
		c.log.Info("the client ended the session (DELETE): stopping the child")
		c.shutdown("deleted")
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusOK)
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// post serves a POST: an initialize opens a session; anything else needs one (D161).
func (b *Bridge) post(w http.ResponseWriter, r *http.Request, owner any, sid string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxMessage+1))
	if err != nil {
		writeRPCError(w, http.StatusBadRequest, nil, mcpwire.CodeInvalidRequest, "the request body could not be read")
		return
	}
	if len(body) > MaxMessage {
		writeRPCError(w, http.StatusRequestEntityTooLarge, nil, mcpwire.CodeInvalidRequest, "request too large")
		return
	}
	m, err := mcpwire.Parse(body, mcpwire.FromClient)
	if err != nil {
		b.log.Info("a client message is refused: it is not one JSON-RPC message with exact keys (D172)", "err", err)
		refuseParse(w, m, err)
		return
	}
	version := r.Header.Get(mcpwire.HeaderProtocolVersion)
	if m.Method == mcpwire.MethodInitialize {
		if len(m.ID) == 0 {
			writeRPCError(w, http.StatusBadRequest, nil, mcpwire.CodeInvalidRequest, "Invalid Request: initialize is a request")
			return
		}
		if sid != "" {
			writeRPCError(w, http.StatusBadRequest, m.ID, mcpwire.CodeInvalidRequest, "Invalid Request: Server already initialized")
			return
		}
		b.initialize(w, r, owner, body, m)
		return
	}
	switch version {
	case mcpwire.V20251125, mcpwire.V20250618, "":
	case mcpwire.V20260728:
		// The bridge is legacy only (D161): a modern request, with or without a session id, gets what a legacy
		// server answers, so that a dual-era client falls back to initialize.
		writeJSON(w, http.StatusBadRequest, mcpwire.LegacyNoSessionBody)
		return
	default:
		writeRPCError(w, http.StatusBadRequest, m.ID, mcpwire.CodeLegacyNoSession, "Bad Request: Unsupported protocol version")
		return
	}
	if sid == "" {
		writeJSON(w, http.StatusBadRequest, mcpwire.LegacyNoSessionBody)
		return
	}
	c := b.lookup(sid, owner)
	if c == nil {
		sessionNotFound(w, m.ID)
		return
	}
	c.post(w, r, body, m)
}

// initialize spawns a child for a new session, forwards the initialize (with the node's roots capability) and
// answers its response with a node-minted session id. A child that cannot start, or that ends or stays silent
// until the gate gives up, is 502 target_unreachable; one that answers with an error is stopped and its error
// relayed with no session.
func (b *Bridge) initialize(w http.ResponseWriter, r *http.Request, owner any, body []byte, m *mcpwire.Message) {
	if !b.reserve() {
		b.log.Warn("an initialize is refused: the service is at --max-sessions", "max_sessions", b.cfg.MaxSessions)
		nodeAnswer(w, http.StatusServiceUnavailable, tunnel.NodeFull, m.ID, "kontra connect: the stdio service is at its --max-sessions")
		return
	}
	id, err := newSessionID()
	if err != nil {
		b.mu.Lock()
		b.reserved--
		b.mu.Unlock()
		nodeAnswer(w, http.StatusBadGateway, tunnel.NodeTargetUnreachable, m.ID, "kontra connect: no session id")
		return
	}
	c, err := b.spawn(owner, id)
	if err != nil {
		b.log.Error("the stdio server did not start", "command", b.cfg.Command[0], "err", err)
		nodeAnswer(w, http.StatusBadGateway, tunnel.NodeTargetUnreachable, m.ID, "kontra connect: the stdio server did not start")
		return
	}
	d, err := c.roots.FromClient(mcpwire.EraLegacy, body, m)
	if err != nil || d.Action != mcproots.Forward {
		c.shutdown("its initialize could not be forwarded")
		writeRPCError(w, http.StatusBadRequest, m.ID, mcpwire.CodeInvalidRequest, "invalid request")
		return
	}
	st, err := c.register(m)
	if err != nil {
		c.shutdown("its initialize could not be forwarded")
		nodeAnswer(w, http.StatusBadGateway, tunnel.NodeTargetUnreachable, m.ID, "kontra connect: the stdio server is gone")
		return
	}
	defer c.unregister(m, st)
	if err := c.send(d.Body); err != nil {
		c.shutdown("its initialize could not be written")
		nodeAnswer(w, http.StatusBadGateway, tunnel.NodeTargetUnreachable, m.ID, "kontra connect: the stdio server is gone")
		return
	}
	resp, ok := st.await(r.Context())
	if !ok {
		if r.Context().Err() != nil {
			c.shutdown("the gate gave up on its initialize")
			return
		}
		c.shutdown("it did not answer its initialize")
		nodeAnswer(w, http.StatusBadGateway, tunnel.NodeTargetUnreachable, m.ID, "kontra connect: the stdio server did not answer the initialize")
		return
	}
	if _, isErr := mcpwire.Member(resp, "error"); isErr {
		c.shutdown("its initialize failed")
		writeJSON(w, http.StatusOK, resp)
		return
	}
	if !b.open(c) {
		c.shutdown("the node is stopping")
		nodeAnswer(w, http.StatusServiceUnavailable, tunnel.NodeFull, m.ID, "kontra connect is stopping")
		return
	}
	c.log.Info("stdio MCP session open: one child for it")
	w.Header().Set(mcpwire.HeaderSessionID, id)
	writeJSON(w, http.StatusOK, resp)
}

// reserve counts a child about to be spawned against MaxSessions; false when the service is full or closed.
func (b *Bridge) reserve() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || len(b.all)+b.reserved >= b.cfg.MaxSessions {
		return false
	}
	b.reserved++
	return true
}

// open makes c's session reachable by its id; false when the bridge is closing or c is gone.
func (b *Bridge) open(c *child) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return false
	}
	c.mu.Lock()
	gone := c.gone
	if !gone {
		c.lastUsed = time.Now()
		c.idle = time.AfterFunc(b.cfg.SessionIdle, c.checkIdle)
	}
	c.mu.Unlock()
	if gone {
		return false
	}
	b.sessions[c.id] = c
	return true
}

// lookup is the live session sid of owner, nil when there is none (unknown, ended, crashed, idle, another owner's).
func (b *Bridge) lookup(sid string, owner any) *child {
	b.mu.Lock()
	defer b.mu.Unlock()
	c := b.sessions[sid]
	if c == nil || c.owner != owner {
		return nil
	}
	return c
}

// forget makes c's session unreachable (it answers 404 from now on).
func (b *Bridge) forget(c *child) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.sessions[c.id] == c {
		delete(b.sessions, c.id)
	}
}

// release ends c's count against MaxSessions, once its process group is stopped.
func (b *Bridge) release(c *child) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.all, c)
}

// Sessions is the number of children counted against MaxSessions: live, starting and stopping.
func (b *Bridge) Sessions() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.all) + b.reserved
}

// CloseOwner stops the children whose sessions owner opened (its tunnel session closed); it does not wait.
func (b *Bridge) CloseOwner(owner any) {
	b.mu.Lock()
	var stop []*child
	for c := range b.all {
		if c.owner == owner {
			stop = append(stop, c)
		}
	}
	b.mu.Unlock()
	for _, c := range stop {
		c.shutdown("its tunnel session closed")
	}
}

// Close stops every child and its process group (stdin closed, SIGTERM after TerminateAfter, SIGKILL after
// KillAfter more), and returns once every group is gone or was sent SIGKILL. Later initializes are 503 full.
func (b *Bridge) Close() error {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
	for {
		// A child an initialize was spawning when Close began joins all later: each round stops what is there.
		b.mu.Lock()
		all := make([]*child, 0, len(b.all))
		for c := range b.all {
			all = append(all, c)
		}
		left := len(all) + b.reserved
		b.mu.Unlock()
		if left == 0 {
			return nil
		}
		for _, c := range all {
			c.shutdown("kontra connect is stopping")
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// newSessionID is a node-minted Mcp-Session-Id: 128 random bits, base64url (visible ASCII, as MCP requires).
func newSessionID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// --- answers the bridge writes itself ---

func writeJSON(w http.ResponseWriter, status int, body []byte) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", fmt.Sprint(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeRPCError(w http.ResponseWriter, status int, id json.RawMessage, code int, msg string) {
	b, err := mcpwire.ErrorBody(id, code, msg, nil)
	if err != nil {
		b, _ = mcpwire.ErrorBody(nil, code, msg, nil)
	}
	writeJSON(w, status, b)
}

// nodeAnswer is an answer with one of tunnel.md §8.2's Kontra-Node reasons (full, target_unreachable), so the gate
// never mistakes it for the server's own.
func nodeAnswer(w http.ResponseWriter, status int, reason tunnel.NodeReason, id json.RawMessage, msg string) {
	w.Header().Set(tunnel.HeaderNode, string(reason))
	writeRPCError(w, status, id, mcpwire.CodeInternal, msg)
}

// sessionNotFound is what a legacy server answers for a session it ended (MCP: 404, the client re-initializes).
func sessionNotFound(w http.ResponseWriter, id json.RawMessage) {
	writeRPCError(w, http.StatusNotFound, id, -32001, "Session not found")
}

// refuseParse answers a client message Parse refused: 400 with -32700 for bytes that are not JSON, -32600 for a
// batch and for a message that is ambiguous or invalid, with its id when it was read unambiguously (relay §11.1).
func refuseParse(w http.ResponseWriter, m *mcpwire.Message, err error) {
	var id json.RawMessage
	if m != nil {
		id = m.ID
	}
	switch {
	case errors.Is(err, mcpwire.ErrParse):
		writeRPCError(w, http.StatusBadRequest, nil, mcpwire.CodeParse, "Parse error")
	case errors.Is(err, mcpwire.ErrBatch):
		writeRPCError(w, http.StatusBadRequest, nil, mcpwire.CodeInvalidRequest, "batch not supported")
	default:
		writeRPCError(w, http.StatusBadRequest, id, mcpwire.CodeInvalidRequest, "invalid request")
	}
}

// clip shortens a value an error or a log line quotes.
func clip(s string) string {
	const max = 80
	if len(s) <= max {
		return s
	}
	return strings.ToValidUTF8(s[:max], "") + "…"
}
