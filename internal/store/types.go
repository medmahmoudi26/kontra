package store

import (
	"encoding/json"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
)

// State is tasks.state: one vocabulary for both protocols (spec §5.1).
type State string

// Task states.
const (
	StateQuoted        State = "quoted"
	StateSubmitted     State = "submitted"
	StateWorking       State = "working"
	StateAuthRequired  State = "auth-required"
	StateInputRequired State = "input-required"
	StateCompleted     State = "completed"
	StateFailed        State = "failed"
	StateCanceled      State = "canceled"
	StateRejected      State = "rejected"
)

// Terminal reports whether s is completed, failed, canceled or rejected.
func (s State) Terminal() bool {
	switch s {
	case StateCompleted, StateFailed, StateCanceled, StateRejected:
		return true
	}
	return false
}

// EventKind is task_events.kind (spec §5.2).
type EventKind string

// Event kinds. The last three are MCP's (spec §11.5); M1 never writes them.
const (
	KindTask          EventKind = "task"
	KindStatus        EventKind = "status"
	KindArtifact      EventKind = "artifact"
	KindMessage       EventKind = "message"
	KindMetadata      EventKind = "metadata"
	KindInputRequest  EventKind = "input_request"
	KindInputResponse EventKind = "input_response"
	KindResult        EventKind = "result"
)

// Carried reports whether an A2A stream carries rows of this kind (spec §9): task, status and artifact.
func (k EventKind) Carried() bool {
	return k == KindTask || k == KindStatus || k == KindArtifact
}

// FailureReason is metadata["kontra.failure"] on a failed task: a closed set (spec §6.4).
type FailureReason string

// Failure reasons.
const (
	ReasonNoSession            FailureReason = "no_session"
	ReasonTunnelLost           FailureReason = "tunnel_lost"
	ReasonUpstreamUnreachable  FailureReason = "upstream_unreachable"
	ReasonUpstreamError        FailureReason = "upstream_error"
	ReasonServiceUnavailable   FailureReason = "service_unavailable"
	ReasonTimeout              FailureReason = "timeout"
	ReasonSealedRejected       FailureReason = "sealed_rejected"
	ReasonLifecycleUnavailable FailureReason = "lifecycle_unavailable"
)

// Protocol is tasks.protocol: what dispatch speaks upstream (spec §5.1).
type Protocol string

// Protocols.
const (
	ProtocolA2A Protocol = "a2a"
	ProtocolMCP Protocol = "mcp"
)

// ServiceRef names one service row (services primary key) and the policy its tasks are created with.
type ServiceRef struct {
	TenantID  uuid.UUID
	Tenant    string // slug, for paths and logs
	Name      string
	Kind      string // "a2a" or "mcp"
	Timeout   time.Duration
	Retries   int
	Retention time.Duration
}

// Owner is the authenticated caller a scoped read or write acts for (spec §2.3).
type Owner struct {
	BuyerID   uuid.UUID
	Principal string // e.g. "key:kk_ab12cd34"
	Via       string // api_key | oidc | mcp | node | anonymous
	TenantID  *uuid.UUID
}

// Event is one task_events row.
type Event struct {
	TaskID     uuid.UUID
	Seq        int64
	Kind       EventKind
	Payload    json.RawMessage // the A2A StreamResponse with gate ids; {"metadata": …} for metadata rows
	PayloadRef string          // bucket key for parts over 256 KiB (spec §10.4); never a URL
	CreatedAt  time.Time
}

// TaskRow is one tasks row, read without caller scope by internal callers.
type TaskRow struct {
	ID             uuid.UUID
	TenantID       uuid.UUID
	Service        string
	Protocol       Protocol
	Owner          Owner
	ContextID      string
	State          State
	Version        int64
	Snapshot       *a2a.Task
	StatusAt       time.Time
	QuoteExpiresAt *time.Time
	Deadline       *time.Time
	CreatedAt      time.Time
	SubmittedAt    *time.Time
	TerminalAt     *time.Time
	RetentionUntil *time.Time
}
