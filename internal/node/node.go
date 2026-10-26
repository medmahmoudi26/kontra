// Package node is `kontra connect` (spec/tunnel.md; note 2 §1–§2): one outbound WebSocket to a gate the
// node chose, over which the gate sends HTTP requests to services on the node's localhost (hosting).
//
// M2 builds hosting A2A services only:
//
//	kontra connect [--gate URL] --serve a2a:<name>=<url> [--serve …] [--key PATH] [--enroll TOKEN|@file|-]…
//	               [--api-key KEY|@file|-] [--name NAME] [--rotate] [--require-channel-binding]
//	               [--max-inflight 64] [--log json]
//
// The hiring side (127.0.0.1:7777, --listen, --local-token, --allow-origin, §9), blob streams (§8.3),
// sealing and per-job keys (§2.3) are M3, with `kontra proxy`'s deliverables (PRD §14 M3 row); MCP hosting,
// `mcp:<name>=<url>` and `mcp:<name>=stdio:<cmd>` (§10), is M3; anonymous sessions and `--public` (§6),
// and the console on :7778, are M4.
//
// The node is a fixed-target reverse proxy, not a SOCKS proxy (§8.2, sidecar_not_a_proxy): a stream's
// preamble names a hosted service, and its one request goes to that service's --serve target and nowhere
// else. It holds no task state: after any close but `revoked` it redials with the same key, backing off
// from 1 s to 60 s with full jitter, reset after a session that lived 60 s (§13).
//
// Work packages: WP-03 (dial.go: the dial and the node's half of the handshake), WP-06 (the rest).
package node

import (
	"context"
	"log/slog"
	"net/url"

	"kontra.sh/kontra/internal/unimpl"
)

// Config is a parsed `kontra connect` command line.
type Config struct {
	Gate                  *url.URL // --gate, default tunnel.DefaultGate; https:// except localhost (tunnel.CheckGateURL)
	Serves                []Serve  // --serve, repeatable
	KeyPath               string   // --key, $KONTRA_KEY, ~/.kontra/identity.key
	Enroll                []string // --enroll tokens, each a value, @file or - (stdin)
	APIKey                string   // --api-key: a value, @file or -
	Name                  string   // --name, default the hostname; 1–64 characters
	Rotate                bool     // --rotate (§12)
	RequireChannelBinding bool     // --require-channel-binding: refuse cb none (§4.1)
	MaxInflight           int      // --max-inflight, default tunnel.NodeMaxInflight
	LogJSON               bool     // --log json; human-readable on stderr otherwise
	CredentialsPath       string   // ~/.kontra/credentials (§2.4)
	Version               string   // for hello.agent and the User-Agent
}

// ParseArgs parses the arguments after `connect`; getenv reads KONTRA_KEY.
func ParseArgs(args []string, getenv func(string) string) (Config, error) {
	return Config{}, unimpl.Error("node.ParseArgs", "WP-06")
}

// Node is one running `kontra connect`.
type Node struct {
	cfg    Config
	logger *slog.Logger
}

// New loads (or creates) the identity key, refuses a group- or other-readable key file, resolves the
// credentials for the gate's origin, and checks the --serve targets.
func New(cfg Config, logger *slog.Logger) (*Node, error) {
	return nil, unimpl.Error("node.New", "WP-06")
}

// Run connects and serves until ctx ends or the gate revokes the key: dial, handshake, the yamux client,
// the ping loop, the control stream, the accept loop serving each gate-opened stream through the Proxy;
// on a close it redials with backoff (§13); on a drain it dials a new session at once and lets the old one
// finish (make-before-break, D35). A handshake refused with a code that is not Retryable ends Run with that
// error, as the operator must change something first (§4).
func (n *Node) Run(ctx context.Context) error {
	return unimpl.Error("node.Node.Run", "WP-06")
}
