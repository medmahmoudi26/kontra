// Package lifecycle is the gate's Temporal side (spec/relay.md §14): the TaskLifecycle workflow, its
// Updates and signals, its activities and their queues, the per-replica workers, and the Temporal-backed
// a2a-go workqueue.Queue (spec §8.1, §15, D6).
//
// Postgres is the record and the log; Temporal is the clock and the durable dispatcher. Events never
// enter Temporal history: inputs, Updates, signals and activity results carry ids, states, small outcomes
// and operator patches of at most 16 KiB (D59). Retries are the workflow's, never Temporal's: an activity
// on a per-replica queue has MaximumAttempts 1, because Temporal retries on the original queue, which for
// a dead replica nobody polls (spec §6.4, §14.2).
//
// This package defines the activity interfaces; internal/relay (dispatch, abort, forward_cancel) and
// internal/push (push_webhook) implement them, so this package imports neither.
package lifecycle

import (
	"github.com/a2aproject/a2a-go/v2/a2a"
)

// WorkflowType is the one workflow type (spec §14.1).
const WorkflowType = "TaskLifecycle"

// Task queues (spec §14.2).
const (
	QueueLifecycle = "kontra-lifecycle"  // the workflow, lifecycle-queue activities, built-in hooks
	QueueDirect    = "kontra-direct"     // dispatch for direct backends, any replica
	QueueDirectCtl = "kontra-direct-ctl" // forward_cancel for a direct task with no dispatch running
	QueuePush      = "kontra-push"       // push_webhook, its own slots (D55)
)

// QueueTunnel is the queue only the replica holding a tunnel session polls (M2).
func QueueTunnel(replicaID string) string { return "kontra-tunnel-" + replicaID }

// QueueCtl is a replica's control queue for abort and forward_cancel, with its own worker slots so
// control is never queued behind dispatches (D44).
func QueueCtl(replicaID string) string { return "kontra-ctl-" + replicaID }

// WorkflowID is the workflow id of a task's lifecycle.
func WorkflowID(taskID a2a.TaskID) string { return "task-" + string(taskID) }

// Updates (refusable synchronously, D41) and signals (fire and forget), spec §14.1.
const (
	UpdateMessage       = "kontra.message"
	UpdateCancel        = "kontra.cancel"
	UpdateApproval      = "kontra.approval"
	UpdateReviewMessage = "kontra.review_message"
	UpdateAnnotate      = "kontra.annotate"

	SignalDispatched    = "kontra.dispatched"
	SignalPush          = "kontra.push"
	SignalInputResponse = "kontra.input_response"
	SignalClose         = "kontra.close"
)

// Activity type names (spec §14.2, §13.3). Hook names keep unique last dotted segments, which is what the
// Go replayer compares.
const (
	ActPrepare         = "prepare"
	ActDispatch        = "dispatch"
	ActAbort           = "abort"
	ActForwardCancel   = "forward_cancel"
	ActAttemptState    = "attempt_state"
	ActListPushConfigs = "list_push_configs"
	ActWriteTerminal   = "write_terminal"
	ActApplyMetadata   = "apply_metadata"
	ActPushWebhook     = "push_webhook"

	HookOnDispatched    = "kontra.lifecycle.OnDispatched"
	HookOnTerminal      = "kontra.lifecycle.OnTerminal"
	HookReviewWindow    = "kontra.lifecycle.ReviewWindow"
	HookOnReviewMessage = "kontra.lifecycle.OnReviewMessage"
	HookOnReviewClosed  = "kontra.lifecycle.OnReviewClosed"
)

// MaxTurns caps the turns of one task (each is about 11 history events; Temporal allows 2,000 Updates per
// run): the 1,001st fails upstream_error (spec §14.1). v4 needs no continue-as-new.
const MaxTurns = 1000
