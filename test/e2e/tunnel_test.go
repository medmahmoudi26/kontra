package e2e

import "testing"

// The M2 gate (PRD §14 M2 row, spec/tunnel.md §16, spec/relay.md §16). WP-08 builds each test; until then
// they are skipped, so `make test-e2e` never reports a pass that tested nothing.
func m2(t *testing.T, test string) {
	t.Helper()
	t.Skipf("%s: the M2 tunnel test is not built yet (WP-08)", test)
}

// TestTunnelKill is tunnel_kill (tunnel.md §11, §13; relay.md §6.4): SIGKILL the node mid-task; the task
// fails tunnel_lost once an upstream task exists (and is retried on the new session before it); the node
// re-registers within 60 s; the next task succeeds.
func TestTunnelKill(t *testing.T) { m2(t, "tunnel_kill") }

// TestInputRequiredSticky is input_required_sticky through the tunnel (relay.md §7): the reply reaches the
// same upstream task on the same session, never another node's; with that session gone the reply fails
// the task tunnel_lost.
func TestInputRequiredSticky(t *testing.T) { m2(t, "input_required_sticky") }

// TestCancelForwardedTunnel is cancel_forwarded through the tunnel (relay.md §8), a working task and an
// input-required one: the upstream receives CancelTask inside the tunnel; the task is canceled; nothing
// is logged after canceled.
func TestCancelForwardedTunnel(t *testing.T) { m2(t, "cancel_forwarded (tunnel)") }

// TestKeyScope is key_scope (tunnel.md §5, §8.1; relay.md §6.1): a session enrolled for one service never
// receives a stream for another; a revoked binding receives none from the next request on, with the node
// ignoring the control message.
func TestKeyScope(t *testing.T) { m2(t, "key_scope") }

// TestSidecarNotAProxy is sidecar_not_a_proxy (tunnel.md §8.2–§8.3): a foreign Host, an absolute URL, a ..
// or encoded path outside the base, CONNECT, Upgrade and a url part pointing at a private address reach
// only the configured target or are refused, and the node dials nothing else.
func TestSidecarNotAProxy(t *testing.T) { m2(t, "sidecar_not_a_proxy") }

// TestReplicaKillTunnel is replica_kill's tunnel variant (relay.md §16, tunnel.md §11): kill the replica
// holding the tunnel; the dispatch times out within 60 s; the task is retried (no upstream task yet) or
// fails tunnel_lost; the tunnels row is gone.
func TestReplicaKillTunnel(t *testing.T) { m2(t, "replica_kill (tunnel)") }

// TestHelloworldTunnel runs the a2a-samples helloworld seller (examples/sellers/helloworld, unmodified)
// behind `kontra connect` and gets its answer through the gate (PRD §14 M2).
func TestHelloworldTunnel(t *testing.T) { m2(t, "helloworld through the tunnel") }

// TestTunnelDrain is the make-before-break drain (tunnel.md §13, relay.md §14.3, D35): SIGTERM the replica
// holding a session mid-task; the node dials a new session at once; the task finishes on the old one
// within drain_timeout; the next task runs on the new one.
func TestTunnelDrain(t *testing.T) { m2(t, "tunnel drain") }
