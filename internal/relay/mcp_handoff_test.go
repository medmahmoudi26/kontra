package relay

import (
	"context"
	"errors"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"

	"kontra.sh/kontra/internal/lifecycle"
	"kontra.sh/kontra/internal/store"
)

// fakeMCP records what the relay hands it.
type fakeMCP struct {
	dispatched []lifecycle.DispatchInput
	canceled   []lifecycle.AttemptRef
}

func (f *fakeMCP) Dispatch(_ context.Context, in lifecycle.DispatchInput) (lifecycle.DispatchResult, error) {
	f.dispatched = append(f.dispatched, in)
	return lifecycle.DispatchResult{Outcome: lifecycle.DispatchTerminal, Reason: "x"}, nil
}

func (f *fakeMCP) ForwardCancel(_ context.Context, in lifecycle.AttemptRef) (lifecycle.ForwardCancelResult, error) {
	f.canceled = append(f.canceled, in)
	return lifecycle.ForwardCancelResult{Outcome: lifecycle.CancelCanceled}, nil
}

// The dispatcher hands the dispatch and forward_cancel of a task whose protocol is mcp to its MCP dispatcher (spec
// §11.5; M3 WP-06), never to a2a-go; without one, they are errors and nothing is written.
func TestMCPTasksAreHandedOver(t *testing.T) {
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	e.exec(`insert into kontra.services (tenant_id, name, kind, backend, source) values ($1, 'research', 'mcp', '{"mcp": {"url": "http://127.0.0.1:1"}}', 'config')`, e.svc.TenantID)
	id := uuid.Must(uuid.NewV7())
	nt, err := store.NewMCPTask(id, store.ServiceRef{TenantID: e.svc.TenantID, Name: "research"}, store.Owner{BuyerID: uuid.New(), Principal: "key:k", Via: store.ViaAPIKey},
		store.StateSubmitted, nil, "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.log.CreateTask(ctx, nil, nt); err != nil {
		t.Fatal(err)
	}
	if err := e.log.BeginAttempt(ctx, store.Attempt{TaskID: id, N: 1, Backend: store.BackendDirect}); err != nil {
		t.Fatal(err)
	}
	f, err := e.log.BeginDispatch(ctx, id, 1, replica)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.log.SetFirstUpstreamID(ctx, f, "up-1", ""); err != nil {
		t.Fatal(err)
	}
	in := lifecycle.DispatchInput{TaskID: id.String(), N: 1, WorkItemID: uuid.NewString()}
	if _, err := e.disp.Dispatch(ctx, in); !errors.Is(err, errNoMCP) {
		t.Fatalf("an MCP task without the MCP dispatcher: %v", err)
	}
	if _, err := e.disp.ForwardCancel(ctx, lifecycle.AttemptRef{TaskID: id.String(), N: 1}); !errors.Is(err, errNoMCP) {
		t.Fatalf("an MCP task's forward_cancel without the MCP dispatcher: %v", err)
	}
	m := &fakeMCP{}
	e.disp.MCP = m
	res, err := e.disp.Dispatch(ctx, in)
	if err != nil || res.Outcome != lifecycle.DispatchTerminal || res.Reason != "x" || len(m.dispatched) != 1 || m.dispatched[0] != in {
		t.Fatalf("dispatch: %+v %v %+v", res, err, m.dispatched)
	}
	fc, err := e.disp.ForwardCancel(ctx, lifecycle.AttemptRef{TaskID: id.String(), N: 1})
	if err != nil || fc.Outcome != lifecycle.CancelCanceled || len(m.canceled) != 1 {
		t.Fatalf("forward_cancel: %+v %v", fc, err)
	}
	if n := len(e.events(a2a.TaskID(id.String()))); n != 1 {
		t.Fatalf("%d events: the relay wrote to the MCP task", n)
	}
}
