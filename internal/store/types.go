package store

import (
	"encoding/json"
	"fmt"
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

// Valid reports whether s is one of the nine states of tasks.state.
func (s State) Valid() bool {
	switch s {
	case StateQuoted, StateSubmitted, StateWorking, StateAuthRequired, StateInputRequired,
		StateCompleted, StateFailed, StateCanceled, StateRejected:
		return true
	}
	return false
}

// A2A is the A2A wire state of s (spec §5.1): quoted shows as input-required, with the approve URL in the
// task's metadata.
func (s State) A2A() a2a.TaskState {
	switch s {
	case StateQuoted, StateInputRequired:
		return a2a.TaskStateInputRequired
	case StateSubmitted:
		return a2a.TaskStateSubmitted
	case StateWorking:
		return a2a.TaskStateWorking
	case StateAuthRequired:
		return a2a.TaskStateAuthRequired
	case StateCompleted:
		return a2a.TaskStateCompleted
	case StateFailed:
		return a2a.TaskStateFailed
	case StateCanceled:
		return a2a.TaskStateCanceled
	case StateRejected:
		return a2a.TaskStateRejected
	}
	return a2a.TaskStateUnspecified
}

// StateFromA2A maps an A2A task state to tasks.state. input-required maps to input-required: only the
// writers that create or end a quote write quoted, explicitly (spec §13.2). The unspecified state, or a
// value a2a-go does not define, is an error wrapping a2a.ErrInvalidParams.
func StateFromA2A(ts a2a.TaskState) (State, error) {
	switch ts {
	case a2a.TaskStateSubmitted:
		return StateSubmitted, nil
	case a2a.TaskStateWorking:
		return StateWorking, nil
	case a2a.TaskStateAuthRequired:
		return StateAuthRequired, nil
	case a2a.TaskStateInputRequired:
		return StateInputRequired, nil
	case a2a.TaskStateCompleted:
		return StateCompleted, nil
	case a2a.TaskStateFailed:
		return StateFailed, nil
	case a2a.TaskStateCanceled:
		return StateCanceled, nil
	case a2a.TaskStateRejected:
		return StateRejected, nil
	}
	return "", fmt.Errorf("store: task state %q: %w", ts, a2a.ErrInvalidParams)
}

// EventKind is task_events.kind (spec §5.2).
type EventKind string

// Event kinds. The last three are MCP's (spec §11.5); M1 never writes them.
//
// Every row's payload is an A2A StreamResponse with gate ids ({"task": …}, {"statusUpdate": …},
// {"artifactUpdate": …}, {"message": …}), except metadata rows, whose payload is {"metadata": <patch>}: the
// patch merged at the top level of the task's metadata (spec §5.2, §9).
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

// Valid reports whether r is one of the closed set of spec §6.4.
func (r FailureReason) Valid() bool {
	switch r {
	case ReasonNoSession, ReasonTunnelLost, ReasonUpstreamUnreachable, ReasonUpstreamError,
		ReasonServiceUnavailable, ReasonTimeout, ReasonSealedRejected, ReasonLifecycleUnavailable:
		return true
	}
	return false
}

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
	// Sealed is services.sealed when the service is sealed: "optional" or "required"; "" when it is off
	// (ext-sealed.md §1, §5). The gate's interceptors refuse an unsealed part to a required service and mark one to an
	// optional service. Off is written "" everywhere, so that a ServiceRef built without it equals one read back.
	Sealed string
}

// SealedRef is services.sealed as ServiceRef.Sealed holds it: "" for off.
func SealedRef(sealed string) string {
	if sealed == "off" {
		return ""
	}
	return sealed
}

// Owner is the authenticated caller a scoped read or write acts for (spec §2.3).
type Owner struct {
	BuyerID   uuid.UUID  `json:"buyerId"`
	Principal string     `json:"principal"` // e.g. "key:kk_ab12cd34"
	Via       string     `json:"via"`       // api_key | oidc | mcp | node | anonymous
	TenantID  *uuid.UUID `json:"tenantId,omitempty"`
}

// Via values (tasks.via, spec §2.3).
const (
	ViaAPIKey    = "api_key"
	ViaOIDC      = "oidc"
	ViaMCP       = "mcp"
	ViaNode      = "node"
	ViaAnonymous = "anonymous"
)

func validVia(v string) bool {
	switch v {
	case ViaAPIKey, ViaOIDC, ViaMCP, ViaNode, ViaAnonymous:
		return true
	}
	return false
}

// Scope is the caller scope of a caller-facing read or write: the service's tenant and name from the
// route, and the principal's buyer id (PRD §12: never from the request body).
type Scope struct {
	TenantID uuid.UUID
	Service  string
	BuyerID  uuid.UUID
}

// ScopeOf returns the scope of o's reads and writes on svc.
func ScopeOf(svc ServiceRef, o Owner) Scope {
	return Scope{TenantID: svc.TenantID, Service: svc.Name, BuyerID: o.BuyerID}
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

// TaskRow is one tasks row, read without caller scope by internal callers. Owner.TenantID is never set:
// tasks records the buyer and the principal, not the principal's tenant.
type TaskRow struct {
	ID             uuid.UUID
	TenantID       uuid.UUID
	Service        string
	Protocol       Protocol
	Owner          Owner
	ContextID      string
	FirstMessageID string
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
