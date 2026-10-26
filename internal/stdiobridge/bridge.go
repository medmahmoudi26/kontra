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
// Work package: M3 WP-05.
package stdiobridge

import (
	"io"
	"log/slog"
	"net/http"
	"time"

	"kontra.sh/kontra/internal/mcproots"
	"kontra.sh/kontra/internal/unimpl"
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
)

// Config is one stdio service.
type Config struct {
	Service     string
	Command     []string // SplitCommand's result
	Env         []string // names from --env, passed through from the node's environment
	SessionIdle time.Duration
	MaxSessions int
	Stderr      io.Writer       // the node's log sink for the children's stderr
	Roots       *mcproots.Roots // --roots for this service; nil: none, roots/list is answered with an empty list
}

// SplitCommand splits a command line with POSIX shell word rules (quotes, backslashes; no expansion but a
// leading ~/), refusing an empty one or an unterminated quote.
func SplitCommand(line, home string) ([]string, error) {
	return nil, unimpl.Error("stdiobridge.SplitCommand", "M3 WP-05")
}

// Bridge serves one stdio service's exchanges.
type Bridge struct {
	cfg Config
	log *slog.Logger
}

// New checks cfg; children are spawned by initialize requests.
func New(cfg Config, log *slog.Logger) (*Bridge, error) {
	return nil, unimpl.Error("stdiobridge.New", "M3 WP-05")
}

// ServeHTTP serves one exchange from a tunnel stream: POST, GET or DELETE on the service's MCP path.
func (b *Bridge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	http.Error(w, unimpl.Error("stdiobridge.Bridge", "M3 WP-05").Error(), http.StatusNotImplemented)
}

// Close stops every child and its process group (stdin closed, SIGTERM after TerminateAfter, SIGKILL after
// KillAfter more).
func (b *Bridge) Close() error {
	return unimpl.Error("stdiobridge.Bridge.Close", "M3 WP-05")
}
