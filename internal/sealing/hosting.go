// Package sealing is the nodes' half of the sealed extension (spec/ext-sealed.md §4–§7, D31, D61, D62, D162,
// D165): the hosting node unseals what the gate forwards to a sealed service and seals what the service
// answers, the hiring node seals what a local client sends to a sealed service and opens what comes back, and
// kontra seal is the escape hatch for scripting (§5.1).
//
// Both nodes read and write the A2A 1.0 shapes only, JSON-RPC and HTTP+JSON, unary and SSE (D165), and for MCP
// the 2025-11-25 tools/call, its result, and for a task the sealed call created its tasks/result, input requests,
// answers and statusMessage (D62); a sealed MCP service is never served to a 2026-07-28 request (D162). Neither
// node ever hands a key to a service or a client.
//
// Work package: M3 WP-08.
package sealing

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"filippo.io/age"

	"kontra.sh/kontra/internal/tunnel"
	"kontra.sh/kontra/internal/unimpl"
)

// Hosting-node bounds (spec/ext-sealed.md §5).
const (
	// DefaultMaxAge is --sealed-max-age: how old a request's iat may be; it must cover the operator's longest
	// quote expiry plus task.timeout.
	DefaultMaxAge = 25 * time.Hour
	// JobRetention is how long a job's state is kept after its last exchange (node-local clock).
	JobRetention = 7 * 24 * time.Hour
	// HostedDirName is the job state's directory under ~/.kontra: hosted/<gate host>/<service>/, 0600 files.
	HostedDirName = "hosted"
)

// HostingConfig configures the hosting side.
type HostingConfig struct {
	Dir        string                // ~/.kontra/hosted/<gate host>
	Identities func() []age.Identity // the identity file's keys, active first (a rotation's old key still opens)
	KeyIDs     func() []string       // their key ids, for the kids check (not_a_recipient)
	MaxAge     time.Duration         // --sealed-max-age
	Required   []string              // --sealed-required <service>: refuse an unsealed request (bad_header)
}

// Hosting is the hosting node's sealing, keyed per service and per job (spec §5 "Hosting node"): per job (the
// gate task id; for plain MCP calls, the session hash) the replyTo and rk of its first sealed request, the
// upstream task and context ids, the (messageId, part) pairs forwarded, and per gate task the MCP input requests
// it sealed and has not seen answered (InputRequest); per service every replyTo that opened a job, the upstream
// context id recorded per gate context, and for MCP every (session, messageId) forwarded. A job is opened, and
// its pairs recorded, only once the service has accepted the request.
//
// Bodies are read with exact keys (internal/mcpwire.Parse, internal/strictjson; D172): one with a repeated or
// case-variant key at a level the node reads, a service's reply included, is refused bad_header, since the gate
// is not trusted to have checked a request; bodies are sealed and opened by re-encoding what was parsed, never
// patched in place.
//
// MCP input requests (D175): an upstream elicitation/create or sampling/createMessage of a sealed task is sealed
// with messageId = the upstream server's own JSON-RPC id for it (as a string), and session and tool of the call
// that created the task. The gate rewrites the JSON-RPC id to k.<taskId>.<seq> and back, and never touches the
// envelope. The answer, a JSON-RPC response the gate forwards on an exchange of that task, passes rule 4b: its
// header's session, tool and messageId equal the preamble's mcp_session, the task's tool and the response's
// id, which names an InputRequest of the preamble's task sealed less than MaxAge ago (wrong_message otherwise)
// and not answered yet (replayed); replyTo and rk equal the job's (wrong_job). Rule 5's (session, messageId)
// set is for requests only: a server's ids are another space than the client's. In a job whose request was
// sealed, a response whose id is that of an InputRequest MUST itself be sealed and pass rule 4b: one without an
// exact {v, envelope} in result._meta["sh.kontra/sealed"] is refused bad_header and nothing of it is forwarded
// to the service, so a gate cannot answer the service's question in plaintext for the buyer (spec §5, "Answers
// must be sealed").
type Hosting struct {
	cfg HostingConfig
	log *slog.Logger
}

// NewHosting opens the job state directory.
func NewHosting(cfg HostingConfig, log *slog.Logger) (*Hosting, error) {
	return nil, unimpl.Error("sealing.NewHosting", "M3 WP-08")
}

// InputRequest is an MCP input request this node sealed for a gate task and has not seen answered.
type InputRequest struct {
	Task      string    // the preamble's task
	MessageID string    // the upstream server's JSON-RPC id, as a string
	SealedAt  time.Time // node clock; the answer must come within MaxAge
}

// Exchange is one forwarded exchange's sealing state, from Request to Response.
type Exchange struct{}

// Request checks and unseals one request the gate forwards to a sealed service (§5 rules 1–5) with the
// preamble's ids, replacing each sealed part with its plaintext part. A refusal is a *sealed.RejectError, which
// the proxy answers itself: 400, Kontra-Node: sealed_rejected, Kontra-Sealed-Reject: <reason> (§6).
func (h *Hosting) Request(ctx context.Context, pre tunnel.Preamble, req *http.Request) (*http.Request, *Exchange, error) {
	return nil, nil, unimpl.Error("sealing.Hosting.Request", "M3 WP-08")
}

// Response seals every part of the service's answer to the job's replyTo, with the reply header and auth,
// reusing the ciphertext of a part already sealed with the same header and body (so polling sees identical
// bytes); for an exchange whose request was not sealed it passes the answer unchanged. It also commits the job
// once the service has accepted the request.
func (h *Hosting) Response(ctx context.Context, x *Exchange, resp *http.Response) (*http.Response, error) {
	return nil, unimpl.Error("sealing.Hosting.Response", "M3 WP-08")
}

// Sweep drops jobs JobRetention after their last exchange.
func (h *Hosting) Sweep(now time.Time) error {
	return unimpl.Error("sealing.Hosting.Sweep", "M3 WP-08")
}
