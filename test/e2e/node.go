package e2e

import (
	"context"
	"os/exec"

	"kontra.sh/kontra/internal/unimpl"
)

// M2 harness (WP-08). The replicas share one public_url, which a node's hello must name as its origin
// (spec/tunnel.md §4: bad_origin otherwise), so the public port is a Front: a TCP proxy that forwards each
// new connection to the replica the test chose (Front.Route), replica A by default. Tunnel services are
// services with backend tunnel, each served by a `kontra connect` subprocess (a Node) of the binary built
// once from ./cmd/kontra, with its own HOME under TempRoot (its identity key and credentials) and the
// slowagent (or the helloworld seller) as its --serve target. Every Node carries KONTRA_E2E_RUN, so
// scripts/test_e2e.sh can prove none outlived the run.

// Front is the public_url listener of the harness.
type Front struct {
	Addr string
}

// Route sends every new connection to g from now on; established ones are left alone.
func (f *Front) Route(g *Gate) {
	panic(unimpl.Error("e2e.Front.Route", "WP-08"))
}

// NodeOptions configure one `kontra connect`.
type NodeOptions struct {
	Name   string            // --name, and the directory under the run's
	Serves map[string]string // service name → target URL (--serve a2a:<name>=<url>)
	Enroll []string          // service tokens from Harness.EnrollToken
	Rotate bool
	Args   []string // extra flags
}

// Node is one `kontra connect` subprocess.
type Node struct {
	Name    string
	Home    string
	LogPath string
	KeyID   string // read from the identity file once created
	cmd     *exec.Cmd
	opts    NodeOptions
}

// EnrollToken mints a service token with `kontra-gate enroll -f kontra.yaml <service>`.
func (h *Harness) EnrollToken(ctx context.Context, service string) (string, error) {
	return "", unimpl.Error("e2e.Harness.EnrollToken", "WP-08")
}

// StartNode starts a node and waits for its welcome (its tunnels row).
func (h *Harness) StartNode(ctx context.Context, o NodeOptions) (*Node, error) {
	return nil, unimpl.Error("e2e.Harness.StartNode", "WP-08")
}

// Kill sends SIGKILL to the node and waits until it is gone (tunnel_kill).
func (n *Node) Kill() error {
	return unimpl.Error("e2e.Node.Kill", "WP-08")
}

// Restart starts the node again with the same HOME (the same key, no token) and waits for its welcome.
func (n *Node) Restart(ctx context.Context, h *Harness) error {
	return unimpl.Error("e2e.Node.Restart", "WP-08")
}

// Sessions reads the node key's tunnels rows: session id → replica id.
func (h *Harness) Sessions(ctx context.Context, keyID string) (map[string]string, error) {
	return nil, unimpl.Error("e2e.Harness.Sessions", "WP-08")
}
