package relay

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
	"go.temporal.io/sdk/client"

	"kontra.sh/kontra/internal/hooks"
	"kontra.sh/kontra/internal/lifecycle"
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

// TestHireRefusesARequiredExtension (spec §2.4, §13.4; DM4-27; M4 WP-04 review): a tool call of /mcp activates no
// extension, so a hire of a service whose served card marks one required: true is refused with
// ExtensionSupportRequiredError before anything else (no Admission, no task row, nothing to the seller), and so is a
// reply to a hire task of such a service: what the A2A route refuses a request without A2A-Extensions with.
func TestHireRefusesARequiredExtension(t *testing.T) {
	adm := &recordingAdmission{}
	r := newResendRig(t, adm)
	ctx := context.Background()
	p := principal(buyerA)
	const escrow = "https://x.example.com/ext/escrow"
	var required []string
	h := &Hirer{Handlers: r.handlers(r.lc, NewRegistry()), Log: r.log,
		RequiredExtensions: func(svc store.ServiceRef) []string {
			if svc != r.svc {
				t.Errorf("RequiredExtensions asked about %+v", svc)
			}
			return required
		}}

	// A task at input-required, hired while the card required nothing.
	out, err := h.Hire(ctx, HireRequest{Principal: p, Service: r.svc, Message: hireMessage("book"), FirstRequest: sha256.Sum256([]byte("a"))})
	if err != nil {
		t.Fatal(err)
	}
	task := r.waitState(a2a.TaskID(out.Task.ID.String()), store.StateInputRequired)

	required = []string{escrow}
	tasks := scalarCount(t, r, `select count(*) from kontra.tasks`)
	_, err = h.Hire(ctx, HireRequest{Principal: p, Service: r.svc, Message: hireMessage("book"), FirstRequest: sha256.Sum256([]byte("b"))})
	var xe *ExtensionsRequiredError
	if !errors.As(err, &xe) || !errors.Is(err, a2a.ErrExtensionSupportRequired) || len(xe.URIs) != 1 || xe.URIs[0] != escrow {
		t.Fatalf("a hire of a service requiring %s: %v", escrow, err)
	}
	if n := scalarCount(t, r, `select count(*) from kontra.tasks`); n != tasks {
		t.Errorf("the refused hire created %d task rows", n-tasks)
	}
	adm.mu.Lock()
	asked := len(adm.got)
	adm.mu.Unlock()
	if asked != 1 {
		t.Errorf("Admission was asked %d times, want once (the first hire only)", asked)
	}
	if _, err := h.Reply(ctx, p, task, a2a.ContentParts{a2a.NewTextPart("the blue one")}); !errors.As(err, &xe) {
		t.Fatalf("a reply to a task of a service requiring %s: %v", escrow, err)
	}
	time.Sleep(time.Second)
	if sent := r.agent.sentMessages(t); len(sent) != 1 {
		t.Errorf("the seller received %d messages, want the first hire's only", len(sent))
	}
	if row := r.waitState(a2a.TaskID(task.ID.String()), store.StateInputRequired); row.Version != task.Version {
		t.Errorf("the refused reply changed the task: version %d, was %d", row.Version, task.Version)
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
