// Package hooks holds the operator hook points and their payloads (spec/relay.md §13, D26;
// spec/schema/hooks.json, camelCase keys, durations in integer seconds).
//
// The open-source gate contains no money code (PRD §13). Each hook has a built-in implementation, which
// is what this repository ships (Admission allows whatever passed allow:; the Lifecycle activities are
// no-ops and ReviewWindow answers {open: false}), and a remote one an operator selects in kontra.yaml
// (M5): Admission over HTTPS, failing closed; Lifecycle as five activities by type name on the operator's
// Temporal queue.
package hooks

import (
	"context"
	"errors"
)

// Operation is what an Admission is asked about.
type Operation string

// Operations.
const (
	OpTask    Operation = "task"
	OpSession Operation = "session" // MCP initialize (M3)
	OpCall    Operation = "call"    // plain MCP tools/call (M3)
)

// Service is hooks.json#/$defs/service.
type Service struct {
	TenantID string `json:"tenantId"`
	Tenant   string `json:"tenant"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}

// Principal is hooks.json#/$defs/principal.
type Principal struct {
	Kind     string `json:"kind"`
	ID       string `json:"id"`
	BuyerID  string `json:"buyerId"`
	TenantID string `json:"tenantId,omitempty"`
	Via      string `json:"via"`
}

// PartInfo describes one part without its content.
type PartInfo struct {
	MediaType string `json:"mediaType,omitempty"`
	Size      int64  `json:"size"`
	Sealed    bool   `json:"sealed"`
}

// MessageInfo is a message without content (spec §13.1).
type MessageInfo struct {
	MessageID string         `json:"messageId"`
	Metadata  map[string]any `json:"metadata,omitempty"`
	Parts     []PartInfo     `json:"parts"`
}

// AdmitRequest is hooks.json#/$defs/admitRequest.
type AdmitRequest struct {
	Operation  Operation    `json:"operation"`
	TaskID     string       `json:"taskId,omitempty"` // pre-minted, for "task"
	Service    Service      `json:"service"`
	Principal  Principal    `json:"principal"`
	Protocol   string       `json:"protocol"`
	Extensions []string     `json:"extensions,omitempty"`
	ContextID  string       `json:"contextId,omitempty"`
	Message    *MessageInfo `json:"message,omitempty"`
	Tool       string       `json:"tool,omitempty"`
}

// Outcome is a Decision's outcome.
type Outcome string

// Decision outcomes.
const (
	Allow Outcome = "allow"
	Deny  Outcome = "deny"
	Quote Outcome = "quote" // creates the task quoted; never in open source's built-in Admission
)

// Refusal is a denial's error.
type Refusal struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// QuoteTerms is a quote decision's approval URL and expiry.
type QuoteTerms struct {
	ApproveURL string `json:"approveUrl"`
	ExpiresAt  string `json:"expiresAt"` // RFC 3339
}

// Decision is hooks.json#/$defs/decision.
type Decision struct {
	Outcome  Outcome        `json:"outcome"`
	Refusal  *Refusal       `json:"refusal,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
	MCPMeta  map[string]any `json:"mcpMeta,omitempty"`
	Quote    *QuoteTerms    `json:"quote,omitempty"`
}

// Admission decides whether a request may create a task, a session or a call, and on what terms. It is
// consulted after allow: and before any task row exists; it never moves money (D45).
type Admission interface {
	Admit(ctx context.Context, req AdmitRequest) (Decision, error)
}

// BuiltinAdmission is open source's Admission: allow every request that passed the allow: list.
type BuiltinAdmission struct{}

var _ Admission = BuiltinAdmission{}

// Admit implements Admission: allow. The allow: list, per-tool policy, Kontra's reserved keys and the edge
// limits were enforced by the gate before it was asked (spec §13).
func (BuiltinAdmission) Admit(ctx context.Context, req AdmitRequest) (Decision, error) {
	if req.Operation == "" {
		return Decision{}, errors.New("hooks: an admission request needs its operation")
	}
	return Decision{Outcome: Allow}, nil
}

// Admitted is what Admission decided for a new task, carried on the request context from the admission
// interceptor to the work queue's Write (spec §13.1, §15): the pre-minted task id the hook was shown, which
// Write must use as the task's id so an operator that recorded anything under it finds the task (D45),
// and the decision (its metadata and mcpMeta apply to the task, a quote creates it quoted).
type Admitted struct {
	TaskID   string
	Decision Decision
}

type admittedKey struct{}

// WithAdmitted returns ctx carrying a.
func WithAdmitted(ctx context.Context, a Admitted) context.Context {
	return context.WithValue(ctx, admittedKey{}, a)
}

// AdmittedFrom returns the admission of the request ctx belongs to, if Admission ran for it.
func AdmittedFrom(ctx context.Context) (Admitted, bool) {
	a, ok := ctx.Value(admittedKey{}).(Admitted)
	return a, ok
}

// TaskRef is hooks.json#/$defs/taskRef, built by the activity from Postgres, never carried in history.
//
// SPEC-CONFLICT: spec §14 and D59 say that caller content never crosses Temporal history ("the one
// caller-authored value that crosses history is a review-window message's caller_metadata keys"), while
// §13.3 and spec/schema/hooks.json put contextId in every TaskRef (the gate context id, the caller's verbatim
// when it chose one, up to 256 characters), and a hook's input is in history; so does a review message's
// messageId (§13.3's OnReviewMessage args, §14.1's kontra.review_message). Both are kept as the hook
// contract names them: replacing them (a hash) would change the contract the operator's hooks are written
// against. The proposal: D59 lists these two caller-chosen identifiers explicitly.
type TaskRef struct {
	TaskID    string         `json:"taskId"`
	TenantID  string         `json:"tenantId"`
	Service   string         `json:"service"`
	Kind      string         `json:"kind"`
	Protocol  string         `json:"protocol"`
	BuyerID   string         `json:"buyerId"`
	Principal string         `json:"principal"`
	Via       string         `json:"via"`
	ContextID string         `json:"contextId"`
	State     string         `json:"state"`
	Metadata  map[string]any `json:"metadata,omitempty"` // operator-written keys only, ≤ 16 KiB
}

// Patch is hooks.json#/$defs/patch: applied as one metadata event.
type Patch struct {
	Metadata        map[string]any `json:"metadata,omitempty"`
	MCPMeta         map[string]any `json:"mcpMeta,omitempty"`
	HoldOpenSeconds int64          `json:"holdOpenSeconds,omitempty"`
}

// ReviewWindow is hooks.json#/$defs/reviewWindow.
type ReviewWindow struct {
	Open    bool  `json:"open"`
	Seconds int64 `json:"seconds,omitempty"` // ≤ 604800
}

// DispatchedArgs, ReviewMessageArgs, ReviewMessageResult and ReviewClosedArgs are the other hook payloads.
type (
	DispatchedArgs struct {
		Attempt int `json:"attempt"`
	}
	ReviewMessageArgs struct {
		WorkItemID string         `json:"workItemId"`
		MessageID  string         `json:"messageId"`
		Metadata   map[string]any `json:"metadata"`
	}
	ReviewMessageResult struct {
		Accept  bool   `json:"accept"`
		Dispute bool   `json:"dispute"`
		Patch   *Patch `json:"patch,omitempty"`
	}
	ReviewClosedArgs struct {
		Disputed bool `json:"disputed"`
	}
)

// BuiltinLifecycle is open source's Lifecycle: no-op activities registered on kontra-lifecycle under
// the same type names a remote operator serves, so switching changes only the queue (spec §13.3).
// OnTerminal is therefore the last hook that fires in open source. The workflow schedules every hook with
// two arguments, (TaskRef, args), as a remote worker receives them; a built-in whose args are empty takes
// only the TaskRef (the Go SDK ignores a trailing argument a function does not declare).
type BuiltinLifecycle struct{}

// OnDispatched is a no-op; its result is null.
func (BuiltinLifecycle) OnDispatched(ctx context.Context, t TaskRef, args DispatchedArgs) error {
	return nil
}

// OnTerminal returns an empty patch.
func (BuiltinLifecycle) OnTerminal(ctx context.Context, t TaskRef) (Patch, error) {
	return Patch{}, nil
}

// ReviewWindow answers {open: false}: no review window opens in open source.
func (BuiltinLifecycle) ReviewWindow(ctx context.Context, t TaskRef) (ReviewWindow, error) {
	return ReviewWindow{Open: false}, nil
}

// OnReviewMessage refuses: no window opens in open source, so a message offered anyway is not accepted.
func (BuiltinLifecycle) OnReviewMessage(ctx context.Context, t TaskRef, args ReviewMessageArgs) (ReviewMessageResult, error) {
	return ReviewMessageResult{Accept: false}, nil
}

// OnReviewClosed returns an empty patch.
func (BuiltinLifecycle) OnReviewClosed(ctx context.Context, t TaskRef, args ReviewClosedArgs) (Patch, error) {
	return Patch{}, nil
}
