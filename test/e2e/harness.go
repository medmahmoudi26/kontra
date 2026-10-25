// Package e2e holds the gate's end-to-end tests (spec/relay.md §16) and their harness.
//
// The harness runs, once per `go test` invocation and never in parallel: one Postgres container
// (postgres:17-alpine, migrated by `kontra-gate migrate`), one Temporal dev server (the temporal CLI
// 1.9.1, namespace kontra, with frontend.WorkerCommandsEnabled and system.enableCancelActivityWorkerCommand
// on, spec §14.2), the slowagent upstream in the test process, and two kontra-gate replicas as
// subprocesses built from ./cmd/kontra-gate, sharing one kontra.yaml whose service `slow` is a direct
// backend at the upstream with allow_private. An API key is minted with `kontra-gate enroll --api-key`.
// The buyer is a minimal SSE client that keeps every `id:` line, because a2a-go's client ignores them.
// Everything the harness starts it removes, including on failure.
package e2e

import (
	"context"
	"encoding/json"
	"os/exec"

	"kontra.sh/kontra/internal/unimpl"
	"kontra.sh/kontra/test/e2e/slowagent"
)

// Options configure a harness.
type Options struct {
	PostgresImage string // default postgres:17-alpine
	TemporalCLI   string // default: $KONTRA_TEMPORAL_CLI, else `temporal` on PATH
	Upstream      slowagent.Config
}

// Harness is one running test environment.
type Harness struct {
	PostgresURL string
	Temporal    string // host:port of the dev server
	Gates       [2]*Gate
	Upstream    *slowagent.Agent
	APIKey      string
}

// Gate is one kontra-gate replica subprocess.
type Gate struct {
	Name string
	URL  string // http://127.0.0.1:<port>
	cmd  *exec.Cmd
}

// Start brings the environment up.
func Start(ctx context.Context, o Options) (*Harness, error) {
	return nil, unimpl.Error("e2e.Start", "WP-09")
}

// Close stops and removes everything Start started.
func (h *Harness) Close() error {
	return unimpl.Error("e2e.Harness.Close", "WP-09")
}

// Kill sends SIGKILL to the replica (no drain), as replica_kill needs.
func (g *Gate) Kill() error {
	return unimpl.Error("e2e.Gate.Kill", "WP-09")
}

// ReplicaID reads the replica id the process registered in gate_replicas.
func (h *Harness) ReplicaID(ctx context.Context, g *Gate) (string, error) {
	return "", unimpl.Error("e2e.Harness.ReplicaID", "WP-09")
}

// AttemptReplica reads attempts.replica_id of the task's latest attempt: the replica holding the dispatch.
func (h *Harness) AttemptReplica(ctx context.Context, taskID string) (replicaID string, n int, err error) {
	return "", 0, unimpl.Error("e2e.Harness.AttemptReplica", "WP-09")
}

// CarriedSeqs reads the seqs of the task's carried rows (task, status, artifact) from task_events: what a
// complete stream must have delivered, each once, in order.
func (h *Harness) CarriedSeqs(ctx context.Context, taskID string) ([]int64, error) {
	return nil, unimpl.Error("e2e.Harness.CarriedSeqs", "WP-09")
}

// Event is one SSE event as the buyer received it.
type Event struct {
	ID    *int64          // nil for a leading snapshot
	Data  json.RawMessage // the StreamResponse (the JSON-RPC result on JSON-RPC)
	State string          // the task state the event carries, when it carries one
}

// Stream is one buyer SSE stream.
type Stream struct{}

// SendStreaming opens SendStreamingMessage on g (JSON-RPC) with the API key.
func (h *Harness) SendStreaming(ctx context.Context, g *Gate, text string) (*Stream, error) {
	return nil, unimpl.Error("e2e.Harness.SendStreaming", "WP-09")
}

// Subscribe opens SubscribeToTask on g, with Last-Event-ID when lastEventID is not nil.
func (h *Harness) Subscribe(ctx context.Context, g *Gate, taskID string, lastEventID *int64) (*Stream, error) {
	return nil, unimpl.Error("e2e.Harness.Subscribe", "WP-09")
}

// Next returns the next event; io.EOF when the gate closed the stream.
func (s *Stream) Next(ctx context.Context) (Event, error) {
	return Event{}, unimpl.Error("e2e.Stream.Next", "WP-09")
}

// TaskID is the gate task id of the stream (from its first event).
func (s *Stream) TaskID() string { return "" }

// Close drops the connection, as a buyer whose network went away.
func (s *Stream) Close() error {
	return unimpl.Error("e2e.Stream.Close", "WP-09")
}
