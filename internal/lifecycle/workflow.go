package lifecycle

import (
	"go.temporal.io/sdk/workflow"

	"github.com/medmahmoudi26/kontra/internal/unimpl"
)

// TaskLifecycle is the per-task workflow, id task-<task_id>, on kontra-lifecycle (spec §14.1).
//
// Sequence: (quoted: wait for kontra.approval or the quote's expiry) → attempt loop → terminal →
// OnTerminal → (review window; never open in open source). The task.timeout is a workflow timer at the
// deadline the store fixed at submission, never Temporal's execution timeout. Each turn is one dispatch;
// interrupted (input-required) waits for the next kontra.message. On a dispatch failure the workflow
// reads attempt_state (Postgres, not heartbeat details) and applies the retry table of spec §6.4: retry
// with a new attempt before the first upstream task id; after it, a direct backend is re-attached with a
// new dispatch of the same attempt (D42) and a tunnel backend fails tunnel_lost. Patches and annotations
// that arrive while a dispatch, abort or forward_cancel runs are held and applied when it returns.
// Every behaviour change goes behind workflow.GetVersion and is replay-tested (spec §14.4, D46).
func TaskLifecycle(ctx workflow.Context, in Input) error {
	return unimpl.Error("lifecycle.TaskLifecycle", "WP-05")
}
