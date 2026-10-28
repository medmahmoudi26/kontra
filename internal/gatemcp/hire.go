package gatemcp

import (
	"context"
	"encoding/json"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"

	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/hooks"
	"kontra.sh/kontra/internal/store"
)

// PlainWait is how long a plain hire and every reply wait for the task to end, ask a question or be quoted
// before answering {taskId, state} (§12.2, DM4-5): below the 60 s request timeout of the MCP SDKs.
const PlainWait = 25 * time.Second

// RetryWindow is how long after a task left quoted a hire with the same resend key answers that task (§12.3
// Retries), and how long a session's GET streams still get its notifications/elicitation/complete (DM4-6).
const RetryWindow = 10 * time.Minute

// HireRequest is one admitted-or-to-be-admitted hire: the A2A message the gate builds (its messageId minted by the
// gate, its parts from the arguments), the service, and the resend key of §12.3.
type HireRequest struct {
	Principal    auth.Principal
	Service      store.ServiceRef
	Message      *a2a.Message
	ContextID    string
	FirstRequest [32]byte // FirstRequest(ref, arguments)
}

// HireOutcome is the task a hire created or found again (§12.3 Retries), and the Admission decision that made it.
type HireOutcome struct {
	TaskID   uuid.UUID
	State    store.State
	Existing bool           // a retry answered with an existing task
	Decision hooks.Decision // Outcome quote: ApproveURL and ExpiresAt are the operator's
	// ElicitationID is set once a -32042 was sent for the task (tasks.elicitation_id), so that a retry to a session
	// that declared elicitation.url gets the same one.
	ElicitationID string
}

// Hirer creates and follows up hire tasks. The gate implements it over the A2A service's handler, work queue and
// TaskLifecycle (spec §6, §7, §15): the task is an ordinary A2A task (protocol a2a) marked tasks.hired, whose
// first_message_id is the gate-minted messageId and whose first_request is HireRequest.FirstRequest (§12.3).
type Hirer interface {
	// Hire runs Admission (operation task, §13.1) and creates the task submitted or quoted; or answers the
	// existing task a retry names: one of the same (tenant, service, buyer, principal, first_request) that is
	// quoted, or left quoted less than RetryWindow ago.
	Hire(ctx context.Context, req HireRequest) (HireOutcome, error)
	// Reply sends text as the §7 follow-up of a hire task of p's buyer in input-required; refused as §7 says
	// otherwise (the caller renders the refusal as isError).
	Reply(ctx context.Context, p auth.Principal, taskID uuid.UUID, message json.RawMessage) (store.State, error)
}
