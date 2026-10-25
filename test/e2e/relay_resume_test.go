package e2e

import (
	"testing"
	"time"

	"github.com/medmahmoudi26/kontra/test/e2e/slowagent"
)

// TestRelayResume is relay_resume (spec/relay.md §9, §16; PRD §15): the buyer's SSE is cut at a random
// seq K; SubscribeToTask with Last-Event-ID: K on the other replica returns the snapshot as of K (no id
// line) equal to the fold of what the buyer had received, then every carried row after K exactly once,
// in seq order, ending after the terminal row. Also: K at or past the terminal seq of a finished task is
// UnsupportedOperationError, a K that is not a seq of this task is InvalidParams, and a seq of another
// buyer's task reads nothing (TaskNotFound).
func TestRelayResume(t *testing.T) {
	h := start(t, Options{Upstream: slowagent.Config{Chunks: 20, Interval: 200 * time.Millisecond, Streaming: true, Binding: "JSONRPC"}})
	_ = h
	t.Fatal("relay_resume: not implemented (WP-09)")
}
