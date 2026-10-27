package lifecycle

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/api/temporalproto"
	"go.temporal.io/sdk/activity"

	"kontra.sh/kontra/internal/hooks"
	"kontra.sh/kontra/internal/store"
	"kontra.sh/kontra/internal/temporaltest"
)

func hooksPatch(md, mm map[string]any) hooks.Patch { return hooks.Patch{Metadata: md, MCPMeta: mm} }

// stuckMCP is an MCP dispatch whose upstream never answers a cancel (spec §11.6, D57): it begins its dispatch,
// records a first upstream task id when first is set, logs working, and then runs, heartbeating, until Temporal
// cancels it; abort (registry) and forward_cancel do nothing to it, and forward_cancel itself hangs.
type stuckMCP struct {
	log       *store.Log
	first     bool
	aborts    atomic.Int32
	forwarded atomic.Int32
	running   atomic.Int32
}

func (d *stuckMCP) Dispatch(ctx context.Context, in DispatchInput) (DispatchResult, error) {
	id := uuid.MustParse(in.TaskID)
	f, err := d.log.BeginDispatch(ctx, id, in.N, "it-replica")
	if err != nil {
		return DispatchResult{}, err
	}
	if d.first {
		if err := d.log.SetFirstUpstreamID(ctx, f, "up-task-1", ""); err != nil {
			return DispatchResult{}, err
		}
	}
	row, err := d.log.Load(ctx, id)
	if err != nil {
		return DispatchResult{}, err
	}
	w, err := store.MCPStatusWrite(row, store.StateWorking, "working on it", nil)
	if err != nil {
		return DispatchResult{}, err
	}
	w.Fence = f
	if _, err := d.log.Append(ctx, nil, w); err != nil {
		return DispatchResult{}, err
	}
	d.running.Add(1)
	defer d.running.Add(-1)
	for {
		select {
		case <-ctx.Done():
			return DispatchResult{Outcome: DispatchTerminal}, nil
		case <-time.After(500 * time.Millisecond):
			activity.RecordHeartbeat(ctx)
		}
	}
}

func (d *stuckMCP) Abort(context.Context, AttemptRef) error {
	d.aborts.Add(1)
	return nil
}

func (d *stuckMCP) ForwardCancel(ctx context.Context, in AttemptRef) (ForwardCancelResult, error) {
	d.forwarded.Add(1)
	select {
	case <-ctx.Done():
	case <-time.After(12 * time.Second):
	}
	return ForwardCancelResult{Outcome: CancelContinues}, nil
}

// mcpTask creates a gate-owned MCP task of an mcp service, submitted, with its work item, and starts its workflow
// with Input.Protocol mcp, as mcprelay.Tasks.Create does.
func (e *pgEnv) mcpTask(t *testing.T, lc *Client) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	svc := store.ServiceRef{TenantID: e.svc.TenantID, Tenant: e.svc.Tenant, Name: "research", Kind: "mcp",
		Timeout: time.Hour, Retries: 2, Retention: 7 * 24 * time.Hour}
	id := uuid.Must(uuid.NewV7())
	nt, err := store.NewMCPTask(id, svc, buyer, store.StateSubmitted, nil, "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.log.CreateTask(ctx, nil, nt); err != nil {
		t.Fatal(err)
	}
	wid := uuid.Must(uuid.NewV7())
	work := &store.MCPWork{Request: []byte(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"research","task":{}}}`),
		Tool: "research", RequestID: json.RawMessage(`7`), Version: "2025-11-25", Session: strings.Repeat("ab", 32)}
	if err := e.log.InsertWorkItem(ctx, nil, store.WorkItem{ID: wid, TaskID: id, Kind: store.WorkExecute,
		Payload: store.WorkPayload{Owner: buyer, MCP: work}}); err != nil {
		t.Fatal(err)
	}
	in := Input{TaskID: id.String(), TimeoutSeconds: 3600, Retries: 2, Hooks: HooksSnapshot{LifecycleQueue: QueueLifecycle}, Protocol: ProtocolMCP}
	if _, err := lc.StartWithMessage(ctx, in, wid.String()); err != nil {
		t.Fatal(err)
	}
	return id
}

// An MCP task's kontra.cancel (spec §11.6, D57; changeMCPCancel, D46) over a development server with the real
// lifecycle activities: with an upstream task whose forward_cancel never answers, and before the first upstream
// task id with a dispatch an abort does not stop, the cancel completes canceled within 10 s, the task is canceled
// with its calls row, the dispatch is stopped, and the history takes the mcp-cancel branch. With
// KONTRA_RECORD_HISTORIES=1 both histories are recorded for TestReplayRecordedHistories (never over a file).
func TestMCPCancelWithinTenSeconds(t *testing.T) {
	e := newPGEnv(t)
	e.acts.Push = nil
	e.exec(`insert into kontra.services (tenant_id, name, kind, backend, source, timeout_s, retries, tasks_mode)
		values ($1, 'research', 'mcp', '{"mcp": {"url": "http://127.0.0.1:1"}}', 'config', 3600, 2, 'durable')`, e.svc.TenantID)
	c := temporaltest.Client(t)
	lc := NewClient(c)
	d := &stuckMCP{log: e.log}
	w, err := NewWorkers(c, WorkerConfig{ReplicaID: "it-replica", DrainTimeout: time.Second}, Activities{Lifecycle: e.acts, Dispatch: d})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.Stop)
	ctx := context.Background()

	for _, tc := range []struct {
		name   string
		first  bool
		record string
	}{
		{"with an upstream task", true, "mcp_cancel_upstream_silent"},
		{"before the first upstream task id", false, "mcp_cancel_before_first_id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d.first = tc.first
			aborts, forwarded := d.aborts.Load(), d.forwarded.Load()
			id := e.mcpTask(t, lc)
			eventually(t, "the dispatch runs", func() bool { return e.state(id.String()) == store.StateWorking && d.running.Load() == 1 })
			began := time.Now()
			res, ok, err := lc.Cancel(ctx, a2a.TaskID(id.String()))
			took := time.Since(began)
			if err != nil || !ok || res.Outcome != CancelCanceled || took >= 10*time.Second {
				t.Fatalf("kontra.cancel: %+v %v %v after %v", res, ok, err, took)
			}
			if st := e.state(id.String()); st != store.StateCanceled {
				t.Fatalf("the task is %s", st)
			}
			if tc.first && d.forwarded.Load() != forwarded+1 {
				t.Errorf("forward_cancel ran %d times", d.forwarded.Load()-forwarded)
			}
			if !tc.first && d.aborts.Load() != aborts+1 {
				t.Errorf("abort ran %d times", d.aborts.Load()-aborts)
			}
			var calls int
			if err := e.pool.QueryRow(ctx, `select count(*) from kontra.calls where task_id = $1 and is_error`, id).Scan(&calls); err != nil || calls != 1 {
				t.Fatalf("calls rows of the cancelled task: %d %v", calls, err)
			}
			eventually(t, "the dispatch stopped", func() bool { return d.running.Load() == 0 })
			eventually(t, "the workflow closed", func() bool { return closedStatus(t, c, id.String()) == enums.WORKFLOW_EXECUTION_STATUS_COMPLETED })
			js, err := temporalproto.CustomJSONMarshalOptions{}.Marshal(history(t, c, id.String()))
			if err != nil {
				t.Fatal(err)
			}
			// A version marker's change id is a JSON string payload, base64 in the history's JSON.
			if !strings.Contains(string(js), base64.StdEncoding.EncodeToString([]byte(`"`+changeMCPCancel+`"`))) {
				t.Fatal("the history has no mcp-cancel marker")
			}
			recordNewHistory(t, c, id.String(), tc.record)
		})
	}
}

// The lifecycle activities on an MCP task (spec §13.2, §13.3; "hooks get MCP TaskRefs"): task_ref names protocol mcp
// and kind mcp with the operator's keys only (never the projection's mcpMeta); an approval's submit writes an MCP
// status row, which drops the approve URL from the statusMessage and keeps the approval's mcpMeta; apply_metadata
// keeps a patch's mcpMeta.
func TestMCPTaskActivities(t *testing.T) {
	e := newPGEnv(t)
	ctx := context.Background()
	e.exec(`insert into kontra.services (tenant_id, name, kind, backend, source, timeout_s, retries, tasks_mode)
		values ($1, 'research', 'mcp', '{"mcp": {"url": "http://127.0.0.1:1"}}', 'config', 3600, 2, 'durable')`, e.svc.TenantID)
	svc := store.ServiceRef{TenantID: e.svc.TenantID, Tenant: e.svc.Tenant, Name: "research", Kind: "mcp"}
	id := uuid.Must(uuid.NewV7())
	exp := time.Now().Add(time.Hour)
	nt, err := store.NewMCPTask(id, svc, buyer, store.StateQuoted, &exp, "https://pay.example/q/1",
		map[string]any{"kontra.escrow.hold": "h-1", "caller.key": "x"}, map[string]any{"sh.kontra/price": "1 USD"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.log.CreateTask(ctx, nil, nt); err != nil {
		t.Fatal(err)
	}
	ref, err := e.acts.TaskRef(ctx, id.String())
	if err != nil {
		t.Fatal(err)
	}
	if ref.Protocol != "mcp" || ref.Kind != "mcp" || ref.State != "quoted" || ref.ContextID != id.String() ||
		len(ref.Metadata) != 1 || ref.Metadata["kontra.escrow.hold"] != "h-1" {
		t.Fatalf("task_ref of an MCP task: %+v", ref)
	}
	r, err := e.acts.Submit(ctx, SubmitInput{TaskID: id.String(), TimeoutSeconds: 3600,
		Metadata: map[string]any{"kontra.escrow.approved": true}, MCPMeta: map[string]any{"sh.kontra/escrow": "held"}})
	if err != nil || r.Deadline.IsZero() {
		t.Fatalf("submit: %+v %v", r, err)
	}
	row, err := e.log.Load(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != store.StateSubmitted || store.MCPStatusText(row.Snapshot) != "" || store.MCPMeta(row.Snapshot)["sh.kontra/escrow"] != "held" ||
		store.MCPMeta(row.Snapshot)["sh.kontra/price"] != "1 USD" || row.Snapshot.Metadata["kontra.escrow.approved"] != true {
		t.Fatalf("after submit: %s %+v", row.State, row.Snapshot)
	}
	var payload string
	if err := e.pool.QueryRow(ctx, `select payload::text from kontra.task_events where task_id = $1 and seq = $2`, id, row.Version).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(payload, `"status":"submitted"`) || strings.Contains(payload, "TASK_STATE") {
		t.Fatalf("the submitted row is not an MCP status: %s", payload)
	}
	if _, err := e.acts.ApplyMetadata(ctx, ApplyMetadataInput{TaskID: id.String(), Source: "test",
		Patch: hooksPatch(map[string]any{"kontra.escrow.note": "n"}, map[string]any{"sh.kontra/escrow": "released"})}); err != nil {
		t.Fatal(err)
	}
	if row, _ = e.log.Load(ctx, id); store.MCPMeta(row.Snapshot)["sh.kontra/escrow"] != "released" || row.Snapshot.Metadata["kontra.escrow.note"] != "n" {
		t.Fatalf("after apply_metadata: %+v", row.Snapshot.Metadata)
	}
}
