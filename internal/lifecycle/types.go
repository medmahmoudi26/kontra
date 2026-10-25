package lifecycle

import (
	"time"

	"github.com/medmahmoudi26/kontra/internal/hooks"
)

// Input is the workflow's start input (spec §14.1): everything it branches on comes from here or from
// activity results, so a config reload never changes a running task.
type Input struct {
	TaskID         string        `json:"taskId"`
	TimeoutSeconds int64         `json:"timeoutSeconds"`
	Retries        int           `json:"retries"`
	Hooks          HooksSnapshot `json:"hooks"`
}

// HooksSnapshot is the hook configuration in force when the task started (spec §13.3, D46).
type HooksSnapshot struct {
	LifecycleQueue string `json:"lifecycleQueue"` // QueueLifecycle when built in
}

// Update and signal payloads (spec §14.1).
type (
	// MessageUpdate is kontra.message; its UpdateID is the work item id.
	MessageUpdate struct {
		WorkItemID string `json:"workItemId"`
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
	// PushSignal is kontra.push, sent after a push config Save that inserted a row, or a Delete.
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
		Reason  string          `json:"reason,omitempty"` // store.FailureReason when Outcome is failed
	}
	// ForwardCancelResult is forward_cancel's result.
	ForwardCancelResult struct {
		Outcome       CancelOutcome  `json:"outcome"`
		UpstreamError *UpstreamError `json:"upstreamError,omitempty"`
		UpstreamState string         `json:"upstreamState,omitempty"`
	}
	// AttemptState is attempt_state's result, read from Postgres.
	AttemptState struct {
		Backend           string     `json:"backend"`
		ReplicaID         string     `json:"replicaId,omitempty"`
		FirstUpstreamIDAt *time.Time `json:"firstUpstreamIdAt,omitempty"`
	}
	// WriteTerminalInput is write_terminal's input.
	WriteTerminalInput struct {
		TaskID   string `json:"taskId"`
		State    string `json:"state"`
		Reason   string `json:"reason,omitempty"`
		Canceled string `json:"canceled,omitempty"`
		Text     string `json:"text"`
	}
	// ApplyMetadataInput is apply_metadata's input: the patch itself, ≤ 16 KiB (D59).
	ApplyMetadataInput struct {
		TaskID string      `json:"taskId"`
		Patch  hooks.Patch `json:"patch"`
	}
	// PushInput is push_webhook's input.
	PushInput struct {
		TaskID   string    `json:"taskId"`
		ConfigID string    `json:"configId"`
		Deadline time.Time `json:"deadline"`
	}
	// PushResult is push_webhook's result.
	PushResult struct {
		DeliveredSeq int64  `json:"deliveredSeq"`
		Stopped      string `json:"stopped"` // terminal | deleted | failed
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
