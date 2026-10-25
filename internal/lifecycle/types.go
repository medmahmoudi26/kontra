package lifecycle

import (
	"time"

	"kontra.sh/kontra/internal/hooks"
)

// Input is the workflow's start input (spec §14.1): everything it branches on comes from here or from
// activity results, so a config reload never changes a running task.
type Input struct {
	TaskID         string        `json:"taskId"`
	TimeoutSeconds int64         `json:"timeoutSeconds"`
	Retries        int           `json:"retries"`
	Hooks          HooksSnapshot `json:"hooks"`
	// QuoteExpiresAt is set for a task created quoted (spec §13.2): the workflow waits for kontra.approval
	// until then. Nil for a task created submitted.
	QuoteExpiresAt *time.Time `json:"quoteExpiresAt,omitempty"`
}

// HooksSnapshot is the hook configuration in force when the task started (spec §13.3, D46).
type HooksSnapshot struct {
	LifecycleQueue string `json:"lifecycleQueue"` // QueueLifecycle when built in
	// OperatorPrefixes are operator_metadata_prefixes (spec §13.2): the kontra.approval and kontra.annotate
	// validators refuse metadata keys outside them (and kontra.keyRetainUntil) without reading config.
	OperatorPrefixes []string `json:"operatorPrefixes,omitempty"`
}

// hooksQueue is the queue the Lifecycle hooks are scheduled on.
func (h HooksSnapshot) hooksQueue() string {
	if h.LifecycleQueue == "" {
		return QueueLifecycle
	}
	return h.LifecycleQueue
}

// Update and signal payloads (spec §14.1).
type (
	// MessageUpdate is kontra.message; its UpdateID is the work item id. After is, for a follow-up, the seq
	// of the task's last event other than a metadata row when the work item was stored: the turn end the
	// caller answers when the task was waiting for input (spec §7). It tells the validator whether a
	// refusal is final or the workflow has not yet processed the turn end the caller read. Zero for the
	// first message.
	MessageUpdate struct {
		WorkItemID string `json:"workItemId"`
		After      int64  `json:"after,omitempty"`
	}
	// MessageResult is kontra.message's result.
	MessageResult struct {
		Turn int `json:"turn"`
	}
	// CancelUpdate is kontra.cancel.
	CancelUpdate struct{}
	// CancelResult is kontra.cancel's result.
	CancelResult struct {
		Outcome       CancelOutcome  `json:"outcome"`
		UpstreamError *UpstreamError `json:"upstreamError,omitempty"`
	}
	// ApprovalUpdate is kontra.approval (spec §13.2). M5 in practice; built in M1 for completeness.
	ApprovalUpdate struct {
		Accepted bool           `json:"accepted"`
		Metadata map[string]any `json:"metadata,omitempty"`
		MCPMeta  map[string]any `json:"mcpMeta,omitempty"`
	}
	// ApprovalResult is kontra.approval's result.
	ApprovalResult struct {
		State string `json:"state"`
	}
	// ReviewMessageUpdate is kontra.review_message; its UpdateID is the work item id.
	ReviewMessageUpdate struct {
		WorkItemID string         `json:"workItemId"`
		MessageID  string         `json:"messageId"`
		Metadata   map[string]any `json:"metadata,omitempty"` // caller_metadata keys only, ≤ 16 KiB (D59)
	}
	// ReviewMessageResult is kontra.review_message's result.
	ReviewMessageResult struct {
		Accepted bool   `json:"accepted"`
		Seq      *int64 `json:"seq,omitempty"`
	}
	// AnnotateUpdate is kontra.annotate.
	AnnotateUpdate struct {
		Metadata map[string]any `json:"metadata,omitempty"`
		MCPMeta  map[string]any `json:"mcpMeta,omitempty"`
	}
	// DispatchedSignal is kontra.dispatched, sent on an attempt's first upstream task id.
	DispatchedSignal struct {
		N int `json:"n"`
	}
	// PushSignal is kontra.push, sent after a push config Save that inserted a row (or set a failed
	// config again), or a Delete. A sender passes the config's id to Client.SignalPush; the signal in
	// history carries its ConfigRef.
	PushSignal struct {
		ConfigID string `json:"configId"`
		Deleted  bool   `json:"deleted"`
	}
	// InputResponseSignal is kontra.input_response (MCP, M3).
	InputResponseSignal struct {
		Seq int64 `json:"seq"`
	}
)

// CancelOutcome is kontra.cancel's outcome.
type CancelOutcome string

// Cancel outcomes.
const (
	CancelCanceled      CancelOutcome = "canceled"
	CancelNotCancelable CancelOutcome = "not_cancelable"
	CancelUnsupported   CancelOutcome = "unsupported"
)

// UpstreamError is an upstream's JSON-RPC error, relayed to the caller of a cancel (spec §8).
type UpstreamError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Activity inputs and results (spec §14.2).
type (
	// AttemptRef names attempt n of a task.
	AttemptRef struct {
		TaskID string `json:"taskId"`
		N      int    `json:"n"`
	}
	// PrepareInput is prepare's input: attempt N to create, and, when the dispatch of attempt N-1 was lost
	// (a heartbeat or schedule-to-start timeout, a lost context) so that no dispatch could end it, the
	// outcome prepare records on attempt N-1 if it is still open (spec §4: a failure that is retried ends
	// its attempt).
	PrepareInput struct {
		TaskID          string `json:"taskId"`
		N               int    `json:"n"`
		PreviousOutcome string `json:"previousOutcome,omitempty"`
	}
	// PrepareResult is prepare's result.
	PrepareResult struct {
		Queue     string    `json:"queue"`               // QueueDirect, or QueueTunnel(replica) in M2
		Backend   string    `json:"backend"`             // direct | tunnel
		SessionID string    `json:"sessionId,omitempty"` // tunnel only
		Deadline  time.Time `json:"deadline"`            // tasks.deadline, as stored
	}
	// DispatchInput is dispatch's input.
	DispatchInput struct {
		TaskID     string `json:"taskId"`
		N          int    `json:"n"`
		WorkItemID string `json:"workItemId"`
		Resume     bool   `json:"resume"` // re-attach to the recorded upstream task (D42)
	}
	// DispatchResult is dispatch's result: the outcome only; a2a-go's SendMessageResult is discarded.
	DispatchResult struct {
		Outcome DispatchOutcome `json:"outcome"`
		// Reason is the store.FailureReason of the outcome: for terminal, the failure the relay logged (empty
		// for a non-failed terminal state); for retryable, the reason write_terminal records when the retries
		// are exhausted (upstream_unreachable, or upstream_error for HTTP 408/429).
		Reason string `json:"reason,omitempty"`
		// RetryAfterSeconds is the upstream's Retry-After on a retryable HTTP 408/429 (spec §6.4): the next
		// attempt waits at least this long. Zero otherwise.
		RetryAfterSeconds int64 `json:"retryAfterSeconds,omitempty"`
	}
	// ForwardCancelResult is forward_cancel's result.
	ForwardCancelResult struct {
		Outcome       CancelOutcome  `json:"outcome"`
		UpstreamError *UpstreamError `json:"upstreamError,omitempty"`
		UpstreamState string         `json:"upstreamState,omitempty"`
	}
	// AttemptState is attempt_state's result, read from Postgres: the attempt (when N > 0) and the task's
	// state, so the cancel table and the retry table decide on what Postgres recorded.
	AttemptState struct {
		Backend           string     `json:"backend,omitempty"`
		ReplicaID         string     `json:"replicaId,omitempty"`
		FirstUpstreamIDAt *time.Time `json:"firstUpstreamIdAt,omitempty"`
		Ended             bool       `json:"ended,omitempty"`
		TaskState         string     `json:"taskState"`
	}
	// WriteTerminalInput is write_terminal's input. Text is the gate's own text (a reason's default when
	// empty), never service content.
	WriteTerminalInput struct {
		TaskID   string `json:"taskId"`
		State    string `json:"state"`
		Reason   string `json:"reason,omitempty"`
		Canceled string `json:"canceled,omitempty"`
		Text     string `json:"text"`
		// EndAttempt, when set, ends the task's latest attempt if it is still open, with this outcome; when
		// the task was waiting at input-required, the outcome is interrupted instead (D77).
		EndAttempt string `json:"endAttempt,omitempty"`
	}
	// TerminalResult is write_terminal's result: the terminal row's seq and the task's terminal state,
	// which is another one when the task had already ended (the write is then left undone).
	TerminalResult struct {
		Seq   int64  `json:"seq"`
		State string `json:"state"`
	}
	// ApplyMetadataInput is apply_metadata's input: the patch itself, ≤ 16 KiB (D59). Source names who
	// wrote it (a hook, kontra.annotate, kontra.approval) for the audit row of a refused patch.
	ApplyMetadataInput struct {
		TaskID string      `json:"taskId"`
		Patch  hooks.Patch `json:"patch"`
		Source string      `json:"source,omitempty"`
	}
	// SubmitInput is submit's input: the approval handler's submitted write (spec §13.2, §14.1). The
	// approval's metadata crosses history as an operator patch (D59).
	SubmitInput struct {
		TaskID         string         `json:"taskId"`
		Metadata       map[string]any `json:"metadata,omitempty"`
		TimeoutSeconds int64          `json:"timeoutSeconds"`
	}
	// SubmitResult is submit's result: tasks.deadline, fixed by the store at the transition (spec §5.2).
	SubmitResult struct {
		Deadline time.Time `json:"deadline"`
	}
	// ReviewAppendInput is append_review_message's input (spec §14.1, D66).
	ReviewAppendInput struct {
		TaskID     string `json:"taskId"`
		WorkItemID string `json:"workItemId"`
	}
	// PushInput is push_webhook's input. ConfigID is the config's ConfigRef, never its id.
	PushInput struct {
		TaskID   string    `json:"taskId"`
		ConfigID string    `json:"configId"`
		Deadline time.Time `json:"deadline"`
	}
	// PushResult is push_webhook's result.
	PushResult struct {
		DeliveredSeq int64  `json:"deliveredSeq"`
		Stopped      string `json:"stopped"` // PushStoppedTerminal | PushStoppedDeleted | PushStoppedFailed
	}
)

// DispatchOutcome is what the per-process registry recorded for the dispatch (spec §15).
type DispatchOutcome string

// Dispatch outcomes.
const (
	DispatchTerminal    DispatchOutcome = "terminal"     // the relay logged a terminal event
	DispatchInterrupted DispatchOutcome = "interrupted"  // input-required: the turn ended
	DispatchRetryable   DispatchOutcome = "retryable"    // failed before the first upstream task id
	DispatchTunnelLost  DispatchOutcome = "tunnel_lost"  // tunnel backends (M2)
	DispatchContextLost DispatchOutcome = "context_lost" // direct: re-attach (D42)
	DispatchAborted     DispatchOutcome = "aborted"      // abort() stopped it (spec §8)
)
