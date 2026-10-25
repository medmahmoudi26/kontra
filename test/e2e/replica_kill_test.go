package e2e

import (
	"testing"
	"time"

	"github.com/medmahmoudi26/kontra/test/e2e/slowagent"
)

// TestReplicaKillDirect is replica_kill, direct-backend variant (spec/relay.md §6.4, §14.2, §16; D42), the
// M1 gate: two replicas; a streaming task on the slow upstream; the buyer follows it on the replica that
// holds the dispatch (attempts.replica_id); that replica is SIGKILLed mid-stream after the first upstream
// task id. The dispatch's heartbeat times out (within 60 s), the workflow reads attempt_state and
// re-attaches on the surviving replica with a new dispatch of the same attempt, which writes its replica
// id, GetTasks the upstream, logs only the difference and follows it to completed. The buyer resumes with
// Last-Event-ID against the surviving replica. Expected: the task completes; attempt n is still 1 and the
// upstream received exactly one send; the buyer's carried rows across all its streams equal the log's
// carried rows, with no gap and no duplicate.
func TestReplicaKillDirect(t *testing.T) {
	h := start(t, Options{Upstream: slowagent.Config{Chunks: 60, Interval: time.Second, Streaming: true, Binding: "JSONRPC"}})
	_ = h
	t.Fatal("replica_kill (direct): not implemented (WP-09)")
}
