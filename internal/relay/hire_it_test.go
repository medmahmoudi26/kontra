package relay

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
	"go.temporal.io/sdk/client"

	"kontra.sh/kontra/internal/hooks"
	"kontra.sh/kontra/internal/lifecycle"
	"kontra.sh/kontra/internal/sealed"
	"kontra.sh/kontra/internal/store"
)

// hireMessage is a hire's A2A message: a new gate-minted messageId each time, as gatemcp builds it.
func hireMessage(text string) *a2a.Message {
	return a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text))
}

// waitState waits up to 20 s for the task to stand at want.
func (r *resendRig) waitState(id a2a.TaskID, want store.State) *store.TaskRow {
	r.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		row, err := r.log.Load(context.Background(), uuid.MustParse(string(id)))
		if err != nil {
			r.t.Fatal(err)
		}
		if row.State == want {
			return row
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("task %s stands at %s, want %s", id, row.State, want)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestHireRetryReissuesALostStart (spec §12.3 Retries, §14.5; M4 WP-04 review): a hire whose task was created quoted
// and whose start was lost after its row committed (its caller got an error and retries) is answered by the retry
// with that task, before Admission, as §12.3 says, and the retry re-issues the start as an A2A resend does: the task
// gets its workflow, which waits for kontra.approval, and the operator's approval then runs it. Before the fix the
// retry answered the quoted task without a workflow, which the approval could not reach.
func TestHireRetryReissuesALostStart(t *testing.T) {
	adm := &recordingAdmission{decision: hooks.Decision{Outcome: hooks.Quote, Quote: &hooks.QuoteTerms{
		ApproveURL: "https://approve.example.com/t/x", ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	}}}
	r := newResendRig(t, adm)
	ctx := context.Background()
	p := principal(buyerA)
	key := sha256.Sum256([]byte("SHA-256(ref ‖ 0x00 ‖ JCS(arguments))"))

	lost := &Hirer{Handlers: r.handlers(lifecycle.NewClient(lostStarts{r.c}), NewRegistry()), Log: r.log}
	if _, err := lost.Hire(ctx, HireRequest{Principal: p, Service: r.svc, Message: hireMessage("book"), FirstRequest: key}); err == nil {
		t.Fatal("the hire whose start was lost was answered")
	}
	prev, err := r.log.FindHireRetry(ctx, r.svc, HireOwner(p), key, HireRetryWindow)
	if err != nil || prev == nil || prev.State != store.StateQuoted {
		t.Fatalf("the hire task whose start was lost: %+v, %v; want a quoted row", prev, err)
	}
	id := a2a.TaskID(prev.ID.String())
	if ok, err := r.lc.WorkflowExists(ctx, id); err != nil || ok {
		t.Fatalf("the lost start's workflow exists: %v, %v", ok, err)
	}

	// The retry, through a Hirer wired as the gate wires it (Starts: the service's work queue).
	h := &Hirer{Handlers: r.handlers(r.lc, NewRegistry()), Log: r.log,
		Starts: func(svc store.ServiceRef) StartReissuer { return lifecycle.NewQueue(svc, r.log, r.lc, r.queues) }}
	out, err := h.Hire(ctx, HireRequest{Principal: p, Service: r.svc, Message: hireMessage("book"), FirstRequest: key})
	if err != nil || !out.Existing || out.Task.ID != prev.ID || out.Task.State != store.StateQuoted {
		t.Fatalf("the retry: %+v, %v; want the quoted task", out, err)
	}
	if ok, err := r.lc.WorkflowExists(ctx, id); err != nil || !ok {
		t.Fatalf("the retry did not re-issue the lost start: workflow exists %v, %v", ok, err)
	}
	adm.mu.Lock()
	asked := len(adm.got)
	adm.mu.Unlock()
	if asked != 1 {
		t.Errorf("Admission was asked %d times, want once: a retry never asks again", asked)
	}
	if sent := r.agent.sentMessages(t); len(sent) != 0 {
		t.Fatalf("the seller received %d messages before any approval, want 0", len(sent))
	}

	// The re-issued workflow takes the operator's approval and runs the task.
	uctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	uh, err := r.c.UpdateWorkflow(uctx, client.UpdateWorkflowOptions{
		WorkflowID: lifecycle.WorkflowID(id), UpdateName: lifecycle.UpdateApproval,
		Args: []any{lifecycle.ApprovalUpdate{Accepted: true}}, WaitForStage: client.WorkflowUpdateStageCompleted,
	})
	if err != nil {
		t.Fatalf("kontra.approval: %v", err)
	}
	var res lifecycle.ApprovalResult
	if err := uh.Get(uctx, &res); err != nil || res.State != "submitted" {
		t.Fatalf("kontra.approval answered %+v, %v; want submitted", res, err)
	}
	r.waitState(id, store.StateInputRequired)
	if sent := r.agent.sentMessages(t); len(sent) != 1 {
		t.Fatalf("the seller received %d messages, want the approved task's once", len(sent))
	}
}

// TestHireActivatesTheOperatorsExtensions (spec §2.4, §12.2, §13.4; D294, D300): a tool call of /mcp has no
// A2A-Extensions. A required URI that the operator's card_extensions entries assert (Cloud's escrow entry, ext-escrow
// §1) is activated for the buyer: the hire runs Admission, which is told of the URI in extensions, and its task runs;
// the seller never receives the activation, even though its own card declares the URI (an A2A caller's activation
// would reach it). A reply to that task goes through. A required URI of the seller's own, and the sealed extension's
// whoever asserts it, refuse a hire with ExtensionSupportRequiredError before anything else (no Admission, no task
// row, nothing to the seller), and a reply to a hire task of such a service the same way: what the A2A route refuses
// a request without A2A-Extensions with.
func TestHireActivatesTheOperatorsExtensions(t *testing.T) {
	adm := &recordingAdmission{}
	r := newResendRig(t, adm)
	ctx := context.Background()
	p := principal(buyerA)
	const escrow, own = "https://operator.example.com/ext/escrow", "https://seller.example.com/ext/own"
	r.backends.mu.Lock()
	r.backends.info.Extensions = []string{escrow, own}
	r.backends.mu.Unlock()
	var mu sync.Mutex
	var ce CardExtensions
	card := func(c CardExtensions) {
		mu.Lock()
		ce = c
		mu.Unlock()
	}
	h := &Hirer{Handlers: r.handlers(r.lc, NewRegistry()), Log: r.log,
		Extensions: func(svc store.ServiceRef) CardExtensions {
			if svc != r.svc {
				t.Errorf("Extensions asked about %+v", svc)
			}
			mu.Lock()
			defer mu.Unlock()
			return ce
		}}
	asked := func() []hooks.AdmitRequest {
		adm.mu.Lock()
		defer adm.mu.Unlock()
		return append([]hooks.AdmitRequest(nil), adm.got...)
	}
	activations := func() []string {
		var out []string
		for _, rec := range r.agent.recorded() {
			out = append(out, rec.header.Values("A2A-Extensions")...)
		}
		return out
	}

	// The operator's required entry: activated for the buyer, told to Admission, never sent to the seller.
	card(CardExtensions{Required: []string{escrow}, Operator: []string{escrow, "https://operator.example.com/ext/other"}})
	out, err := h.Hire(ctx, HireRequest{Principal: p, Service: r.svc, Message: hireMessage("book"), FirstRequest: sha256.Sum256([]byte("a"))})
	if err != nil {
		t.Fatalf("a hire of a service the operator's entry requires %s of: %v", escrow, err)
	}
	task := r.waitState(a2a.TaskID(out.Task.ID.String()), store.StateInputRequired)
	if got := asked(); len(got) != 1 || len(got[0].Extensions) != 1 || got[0].Extensions[0] != escrow || got[0].TaskID != out.Task.ID.String() {
		t.Fatalf("Admission was asked %+v, want once with extensions [%s]", got, escrow)
	}
	if _, err := h.Reply(ctx, p, task, a2a.ContentParts{a2a.NewTextPart("the blue one")}); err != nil {
		t.Fatalf("a reply to a task of a service the operator's entry requires an extension of: %v", err)
	}
	r.waitState(a2a.TaskID(task.ID.String()), store.StateCompleted)
	if sent := r.agent.sentMessages(t); len(sent) != 2 {
		t.Fatalf("the seller received %d messages, want the hire's and the reply's", len(sent))
	}
	if a := activations(); len(a) != 0 {
		t.Errorf("the seller received A2A-Extensions %q: an activation the gate made for the buyer", a)
	}

	// A task at input-required, for the refused reply below.
	out, err = h.Hire(ctx, HireRequest{Principal: p, Service: r.svc, Message: hireMessage("book"), FirstRequest: sha256.Sum256([]byte("b"))})
	if err != nil {
		t.Fatal(err)
	}
	waiting := r.waitState(a2a.TaskID(out.Task.ID.String()), store.StateInputRequired)

	// The seller's own required URI, and the sealed extension's even as the operator's entry: refused before anything.
	tasks, admitted, sent := scalarCount(t, r, `select count(*) from kontra.tasks`), len(asked()), len(r.agent.sentMessages(t))
	for _, c := range []struct {
		card CardExtensions
		want string
	}{
		{CardExtensions{Required: []string{own, escrow}, Operator: []string{escrow}}, own},
		{CardExtensions{Required: []string{sealed.URI}, Operator: []string{sealed.URI}}, sealed.URI},
	} {
		card(c.card)
		_, err = h.Hire(ctx, HireRequest{Principal: p, Service: r.svc, Message: hireMessage("book"), FirstRequest: sha256.Sum256([]byte("c"))})
		var xe *ExtensionsRequiredError
		if !errors.As(err, &xe) || !errors.Is(err, a2a.ErrExtensionSupportRequired) || len(xe.URIs) != 1 || xe.URIs[0] != c.want {
			t.Errorf("a hire of a service requiring %+v: %v, want ExtensionsRequiredError [%s]", c.card, err, c.want)
		}
		if _, err := h.Reply(ctx, p, waiting, a2a.ContentParts{a2a.NewTextPart("the red one")}); !errors.As(err, &xe) || xe.URIs[0] != c.want {
			t.Errorf("a reply to a task of a service requiring %+v: %v", c.card, err)
		}
	}
	if n := scalarCount(t, r, `select count(*) from kontra.tasks`); n != tasks {
		t.Errorf("the refused hires created %d task rows", n-tasks)
	}
	if n := len(asked()); n != admitted {
		t.Errorf("the refused hires asked Admission %d times", n-admitted)
	}
	time.Sleep(time.Second)
	if n := len(r.agent.sentMessages(t)); n != sent {
		t.Errorf("the refused hires and replies sent the seller %d messages", n-sent)
	}
	if row := r.waitState(a2a.TaskID(waiting.ID.String()), store.StateInputRequired); row.Version != waiting.Version {
		t.Errorf("the refused replies changed the task: version %d, was %d", row.Version, waiting.Version)
	}
}

func scalarCount(t *testing.T, r *resendRig, sql string) int64 {
	t.Helper()
	var n int64
	if err := r.log.DB().QueryRow(context.Background(), sql).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
