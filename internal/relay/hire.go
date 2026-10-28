package relay

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/google/uuid"

	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/lifecycle"
	"kontra.sh/kontra/internal/store"
)

// HireRequest is one hire of the gate's own /mcp (spec §12.2, §12.3): the principal, the A2A service, the A2A message
// the gate built from the tool's arguments (its messageId minted by the gate, role user, its parts, the caller's
// contextId when given), and the hire's resend key, SHA-256(ref ‖ 0x00 ‖ JCS(arguments)).
type HireRequest struct {
	Principal    auth.Principal
	Service      store.ServiceRef
	Message      *a2a.Message
	FirstRequest [32]byte
}

// HireOutcome is the task a hire created, or the one an earlier hire of the same key created that it answers
// (§12.3 Retries: Existing), as it is now.
type HireOutcome struct {
	Task     *store.TaskRow
	Existing bool
}

// HireRetryWindow is how long after a hire task left quoted a hire with the same resend key answers it (§12.3
// Retries: the client's retry after its URL elicitation completed).
const HireRetryWindow = 10 * time.Minute

// Hirer creates and follows up the tasks of /mcp's hire through the A2A service's own handler (spec §12.2): the task
// is an ordinary gate task (protocol a2a, dispatched as §6), created by the service's interceptors and work queue
// exactly as an A2A SendMessage would create it, so the identity, §2.5's key stripping, the sealed and part checks,
// an anonymous tenant's storage and task caps (spec/tunnel.md §6.2), Admission with operation task and the
// principal's via (§13.1), CheckQuote and the quoted state (§13.2), large parts (§10.4) and the TaskLifecycle start
// are the A2A route's own. What differs is carried to the work queue by lifecycle.WithHire: the task is marked hired,
// its first_request is the hire's key, and a retry is matched on that key (§12.3). The thread, SubscribeToTask and
// /mcp's MCP Tasks then read one log.
//
// Nothing here answers the MCP client: internal/gatemcp renders the outcome.
type Hirer struct {
	Handlers *Handlers
	Log      *store.Log
	// RetryWindow is §12.3's retry window; zero is HireRetryWindow.
	RetryWindow time.Duration
	// RequiredExtensions are the URIs a service's served card marks required: true, as the A2A route reads them for
	// §2.4's check (the gate wires a2ahttp.Server.Declared); nil: none.
	RequiredExtensions func(svc store.ServiceRef) []string
	// Starts is the service's work queue re-issuing a task's start (lifecycle.Queue.ReissueStart, as the A2A route's
	// resend does, §14.5); nil re-issues nothing.
	Starts func(svc store.ServiceRef) StartReissuer
}

// ExtensionsRequiredError refuses a hire or a reply, before anything is stored or sent, to a service whose served
// card marks extensions required: true (spec §2.4: a request missing one is refused before any task row exists). A
// tool call of /mcp has no A2A-Extensions to activate them with, so it always misses them (DM4-27). It is
// ExtensionSupportRequiredError (-32008).
type ExtensionsRequiredError struct {
	URIs []string // the required URIs, in the card's order
}

func (e *ExtensionsRequiredError) Error() string {
	s, them := "", "it"
	if len(e.URIs) != 1 {
		s, them = "s", "them"
	}
	return "the service requires the extension" + s + " " + strings.Join(e.URIs, ", ") + ": send " + them + " in A2A-Extensions"
}

func (e *ExtensionsRequiredError) Unwrap() error { return a2a.ErrExtensionSupportRequired }

// requireNone is §2.4's check of a request that activates no extension: refused when svc's served card requires any.
func (h *Hirer) requireNone(svc store.ServiceRef) error {
	if h.RequiredExtensions == nil {
		return nil
	}
	if req := h.RequiredExtensions(svc); len(req) > 0 {
		return &ExtensionsRequiredError{URIs: slices.Clone(req)}
	}
	return nil
}

func (h *Hirer) window() time.Duration {
	if h.RetryWindow > 0 {
		return h.RetryWindow
	}
	return HireRetryWindow
}

// HireOwner is the owner a principal's hire tasks are created and found with: the buyer, the principal and its via,
// as the identity interceptor puts them on the call context (CallerOwner).
func HireOwner(p auth.Principal) store.Owner {
	return store.Owner{BuyerID: p.BuyerID, Principal: p.ID, Via: string(p.Via), TenantID: p.TenantID}
}

// Hire answers a hire: refused with ExtensionsRequiredError when the service's served card requires an extension
// (§2.4, as the A2A route refuses a request that does not activate it, before anything else); the task of the same
// resend key that is quoted, or left quoted less than the retry window ago, as it is (nothing is sent, Admission is
// not asked again, and nothing changes its state: a retry never approves anything, §13.2), its start re-issued first
// while it is quoted (§14.5: a task whose start was lost gets its workflow, as an A2A resend does); otherwise a new
// task created through the service's handler. An error is the A2A error the interceptors or the work queue refused
// it with (a2a-go's sentinels: Admission's refusal, "admission unavailable", "rate limited", "anonymous storage
// limit", a sealed or part refusal, a lifecycle not accepting it in time), or the gate's own failure.
func (h *Hirer) Hire(ctx context.Context, req HireRequest) (HireOutcome, error) {
	if h.Handlers == nil || h.Log == nil {
		return HireOutcome{}, errors.New("relay: the hirer needs the handlers and the log")
	}
	if req.Message == nil || req.Message.ID == "" || len(req.Message.Parts) == 0 || req.Message.TaskID != "" {
		return HireOutcome{}, fmt.Errorf("relay: a hire needs a new message with its id and parts: %w", a2a.ErrInvalidParams)
	}
	if err := h.requireNone(req.Service); err != nil {
		return HireOutcome{}, err
	}
	owner := HireOwner(req.Principal)
	prev, err := h.Log.FindHireRetry(ctx, req.Service, owner, req.FirstRequest, h.window())
	if err != nil {
		return HireOutcome{}, err
	}
	if prev != nil {
		// A retry of a quoted task (its first hire may have had its row committed and its start lost, and answered
		// 503): re-issue the start, idempotent, so the person's approval has a workflow to reach (§14.5, DM4-23). A task
		// that left quoted had its workflow (kontra.approval, or the quote's expiry, came through it).
		if prev.State == store.StateQuoted && h.Starts != nil {
			if err := h.Starts(req.Service).ReissueStart(ctx, prev); err != nil {
				return HireOutcome{}, err
			}
		}
		return HireOutcome{Task: prev, Existing: true}, nil
	}
	sh, err := h.Handlers.For(ctx, req.Service)
	if err != nil {
		return HireOutcome{}, err
	}
	hctx := lifecycle.WithHire(ctx, lifecycle.Hire{Key: req.FirstRequest, RetryWindow: h.window()})
	start, err := send(hctx, sh, req.Principal, &a2a.SendMessageRequest{Message: req.Message})
	if err != nil {
		return HireOutcome{}, err
	}
	id, err := uuid.Parse(string(start.TaskID))
	if err != nil {
		return HireOutcome{}, fmt.Errorf("relay: the work queue answered task %q: %w", start.TaskID, err)
	}
	row, err := h.Log.Load(ctx, id)
	if err != nil {
		return HireOutcome{}, err
	}
	return HireOutcome{Task: row, Existing: !start.New}, nil
}

// Reply sends parts as the §7 follow-up of a hire task of the principal's buyer, through the service's handler: the
// followUp interceptor and the kontra.message Update decide (spec §7: refused unless the task is input-required; a
// quoted task is refused, its quote being accepted on the operator's page and never in the protocol, §13.2; one
// follow-up per turn end); a service whose served card now requires an extension refuses it as Hire does (§2.4). It
// returns the task's version read before the Update (the turn the follow-up started begins after it). task is the
// caller's row (store.Log.LoadHired).
func (h *Hirer) Reply(ctx context.Context, p auth.Principal, task *store.TaskRow, parts a2a.ContentParts) (int64, error) {
	if h.Handlers == nil || h.Log == nil {
		return 0, errors.New("relay: the hirer needs the handlers and the log")
	}
	if task == nil || task.Owner.BuyerID != p.BuyerID {
		return 0, a2a.NewError(a2a.ErrTaskNotFound, "task not found")
	}
	if task.State == store.StateQuoted {
		// Refused before anything is stored: the interceptor refuses it too (§7, §13.2).
		return 0, a2a.NewError(a2a.ErrUnsupportedOperation, "the task is quoted: its quote is accepted on the operator's page, not in the protocol")
	}
	svc, err := h.Log.LoadService(ctx, task.TenantID, task.Service)
	if err != nil {
		return 0, err
	}
	if err := h.requireNone(svc); err != nil {
		return 0, err
	}
	sh, err := h.Handlers.For(ctx, svc)
	if err != nil {
		return 0, err
	}
	mid, err := uuid.NewV7()
	if err != nil {
		return 0, err
	}
	msg := &a2a.Message{ID: mid.String(), Role: a2a.MessageRoleUser, TaskID: a2a.TaskID(task.ID.String()),
		ContextID: task.ContextID, Parts: parts}
	start, err := send(ctx, sh, p, &a2a.SendMessageRequest{Message: msg})
	if err != nil {
		return 0, err
	}
	return start.Version, nil
}

// send runs req through the service's handler as SendStreamingMessage on behalf of p (the identity interceptor reads
// p from ctx; the call context carries A2A-Version 1.0, as the transports' would), takes the first event and stops,
// which closes a2a-go's reader (sse.HandlerStarter's way). The work queue's Write fills the StreamStart only when it
// accepted the turn: a filled one is the answer whatever a2a-go reports after it.
func send(ctx context.Context, sh *ServiceHandler, p auth.Principal, req *a2a.SendMessageRequest) (lifecycle.StreamStart, error) {
	var ss lifecycle.StreamStart
	ctx = lifecycle.WithStreamStart(auth.WithPrincipal(ctx, p), &ss)
	ctx, cc := a2asrv.NewCallContext(ctx, a2asrv.NewServiceParams(map[string][]string{a2a.SvcParamVersion: {string(a2a.Version)}}))
	cc.ProtocolVersion = a2a.Version
	var ferr error
	for _, err := range sh.Handler.SendStreamingMessage(ctx, req) {
		ferr = err
		break
	}
	switch {
	case ss.TaskID != "":
		return ss, nil
	case ferr != nil:
		return ss, ferr
	}
	return ss, a2a.NewError(a2a.ErrInternalError, "the request produced no task")
}

// CodeOf is the JSON-RPC code of an error the relay answered (an a2a-go sentinel's, -32603 for any other), and its
// message for a caller.
func CodeOf(err error) (int, string) {
	var ae *a2a.Error
	msg := ""
	if errors.As(err, &ae) {
		msg = ae.Message
	}
	return codeForError(err), msg
}
