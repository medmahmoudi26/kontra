package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"github.com/google/uuid"
)

func TestGateRequestIDs(t *testing.T) {
	id := uuid.MustParse("0192f0b4-0000-7000-8000-0000000000aa")
	s := GateRequestID(id, 12)
	if s != "k.0192f0b4-0000-7000-8000-0000000000aa.12" {
		t.Fatal(s)
	}
	if got, seq, ok := ParseGateRequestID(s); !ok || got != id || seq != 12 {
		t.Fatalf("%v %d %v", got, seq, ok)
	}
	for _, bad := range []string{"k.0192F0B4-0000-7000-8000-0000000000AA.1", "k." + id.String() + ".0", "k." + id.String() + ".01",
		"k." + id.String() + ".-1", "k." + id.String(), "x." + id.String() + ".1", "k.{" + id.String() + "}.1", ""} {
		if _, _, ok := ParseGateRequestID(bad); ok {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestMCPStatusVocabulary(t *testing.T) {
	for s, want := range map[State]string{StateQuoted: "working", StateSubmitted: "working", StateWorking: "working",
		StateAuthRequired: "working", StateInputRequired: "input_required", StateCompleted: "completed", StateFailed: "failed",
		StateRejected: "failed", StateCanceled: "cancelled"} {
		if got := s.MCPStatus(); got != want {
			t.Errorf("%s: %s, want %s", s, got, want)
		}
	}
	for status, want := range map[string]State{"working": StateWorking, "input_required": StateInputRequired,
		"completed": StateCompleted, "failed": StateFailed, "cancelled": StateCanceled} {
		if got, ok := StateFromMCP(status); !ok || got != want {
			t.Errorf("%s: %s", status, got)
		}
	}
	if _, ok := StateFromMCP("canceled"); ok {
		t.Error("an MCP status MCP does not define was mapped")
	}
}

// mcpEnv adds an mcp service and an MCP task with its work item.
func (e *env) mcpTask(state State, approveURL string) (uuid.UUID, ServiceRef) {
	e.t.Helper()
	e.exec(`insert into kontra.services (tenant_id, name, kind, backend, source, timeout_s, retention_s, tasks_mode)
		values ($1, 'research', 'mcp', '{"mcp": {"url": "http://127.0.0.1:1"}}', 'config', 3600, 604800, 'durable')
		on conflict do nothing`, e.tenant)
	svc := ServiceRef{TenantID: e.tenant, Tenant: "default", Name: "research", Kind: "mcp"}
	id := uuid.Must(uuid.NewV7())
	var exp *time.Time
	if state == StateQuoted {
		x := time.Now().Add(time.Hour)
		exp = &x
	}
	nt, err := NewMCPTask(id, svc, buyerA, state, exp, approveURL, map[string]any{"kontra.escrow.id": "e-1"},
		map[string]any{"sh.kontra/price": "1 USD"})
	if err != nil {
		e.t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := e.log.CreateTask(ctx, nil, nt); err != nil {
		e.t.Fatal(err)
	}
	work := &MCPWork{Request: []byte(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"research","arguments":{"q":"<x>"}}}`),
		Tool: "research", RequestID: json.RawMessage(`7`), Version: "2025-11-25", Session: strings.Repeat("0f", 32)}
	if err := e.log.InsertWorkItem(ctx, nil, WorkItem{ID: uuid.Must(uuid.NewV7()), TaskID: id, Kind: WorkExecute,
		Payload: WorkPayload{Owner: buyerA, MCP: work}}); err != nil {
		e.t.Fatal(err)
	}
	return id, svc
}

// An MCP task's projection and log (spec §11.9): every writer's projection is the fold of its rows (Folder.MCP);
// an input response is logged once per input request (a second, a foreign buyer's, one after the end are not);
// the terminal event writes the task's one calls row in its statement; a write after it is refused.
func TestMCPTaskLog(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id, svc := e.mcpTask(StateQuoted, "https://pay.example/t/1")
	row, err := e.log.Load(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.Protocol != ProtocolMCP || row.ContextID != id.String() || MCPStatusText(row.Snapshot) != "https://pay.example/t/1" ||
		MCPMeta(row.Snapshot)["sh.kontra/approve-url"] != "https://pay.example/t/1" || MCPMeta(row.Snapshot)["sh.kontra/price"] != "1 USD" {
		t.Fatalf("the created task: %+v %+v", row, row.Snapshot)
	}
	write := func(build func(row *TaskRow) (Write, error)) int64 {
		t.Helper()
		row, err := e.log.Load(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		w, err := build(row)
		if err != nil {
			t.Fatal(err)
		}
		a, err := e.log.Append(ctx, nil, w)
		if err != nil {
			t.Fatal(err)
		}
		return a.Seq
	}
	write(func(row *TaskRow) (Write, error) { return MCPStatusWrite(row, StateSubmitted, "", nil) })
	write(func(row *TaskRow) (Write, error) { return MCPStatusWrite(row, StateWorking, "Gathering sources", nil) })
	if _, err := e.log.ApplyPatch(ctx, id, map[string]any{"kontra.escrow.held": true}, map[string]any{"sh.kontra/escrow": "held"}); err != nil {
		t.Fatal(err)
	}
	write(func(row *TaskRow) (Write, error) { return MCPStatusWrite(row, StateInputRequired, "", nil) })
	reqSeq, err := e.log.AppendInputRequest(ctx, nil, id, json.RawMessage(`41`), func(gateID string) (json.RawMessage, error) {
		return json.RawMessage(`{"jsonrpc":"2.0","id":"` + gateID + `","method":"elicitation/create","params":{"message":"which?"}}`), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	reqs, err := e.log.PendingInputRequests(ctx, id)
	if err != nil || len(reqs) != 1 || reqs[0].Seq != reqSeq || reqs[0].GateID != GateRequestID(id, reqSeq) ||
		string(reqs[0].UpstreamID) != "41" || reqs[0].Method != "elicitation/create" {
		t.Fatalf("pending input requests: %+v %v", reqs, err)
	}
	scope := Scope{TenantID: e.tenant, Service: svc.Name, BuyerID: buyerA.BuyerID}
	answer := json.RawMessage(`{"jsonrpc":"2.0","id":"` + GateRequestID(id, reqSeq) + `","result":{"action":"accept"}}`)
	if _, ok, err := e.log.AppendInputResponse(ctx, Scope{TenantID: e.tenant, Service: svc.Name, BuyerID: buyerB.BuyerID}, id, reqSeq, answer); ok || !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Fatalf("a foreign buyer's answer: %v %v", ok, err)
	}
	if _, ok, err := e.log.AppendInputResponse(ctx, scope, id, reqSeq-1, answer); err != nil || ok {
		t.Fatalf("an answer to a seq that is not an input request: %v %v", ok, err)
	}
	respSeq, ok, err := e.log.AppendInputResponse(ctx, scope, id, reqSeq, answer)
	if err != nil || !ok {
		t.Fatalf("the answer: %v %v", ok, err)
	}
	if _, ok, err := e.log.AppendInputResponse(ctx, scope, id, reqSeq, answer); err != nil || ok {
		t.Fatalf("a second answer was logged: %v %v", ok, err)
	}
	resps, err := e.log.InputResponses(ctx, id, 0)
	if err != nil || len(resps) != 1 || resps[0].Seq != respSeq || resps[0].RequestSeq != reqSeq || string(resps[0].Response) != string(answer) {
		t.Fatalf("input responses: %+v %v", resps, err)
	}
	if reqs, _ := e.log.PendingInputRequests(ctx, id); len(reqs) != 0 {
		t.Fatalf("an answered input request is pending: %+v", reqs)
	}
	write(func(row *TaskRow) (Write, error) { return MCPStatusWrite(row, StateWorking, "Continuing", nil) })
	result := json.RawMessage(`{"content":[{"type":"text","text":"done"}],"isError":false}`)
	write(func(row *TaskRow) (Write, error) {
		w, err := MCPResultWrite(row, StateCompleted, "result", result, nil)
		if err != nil {
			return Write{}, err
		}
		w.Call, err = e.log.MCPCall(ctx, id, false, int64(len(result)))
		return w, err
	})
	if _, err := e.log.ApplyPatch(ctx, id, map[string]any{"kontra.escrow.released": true}, nil); err != nil {
		t.Fatal(err)
	}

	// The fold of every row is the projection.
	row, err = e.log.Load(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	evs, err := e.log.Events(ctx, id, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	f := Folder{MCP: true}
	for _, ev := range evs {
		if err := f.Apply(ev); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := json.Marshal(f.Task())
	want, _ := json.Marshal(row.Snapshot)
	if string(got) != string(want) {
		t.Fatalf("the fold differs from the projection:\n fold %s\n proj %s", got, want)
	}
	if row.State != StateCompleted || MCPMeta(row.Snapshot)["sh.kontra/escrow"] != "held" || row.Snapshot.Metadata["kontra.escrow.released"] != true {
		t.Fatalf("the projection: %s %+v", row.State, row.Snapshot.Metadata)
	}
	o, ok, err := e.log.MCPTerminal(ctx, id)
	if err != nil || !ok || o.Kind != KindResult || string(o.Result) != string(result) {
		t.Fatalf("the terminal row: %+v %v %v", o, ok, err)
	}
	// One calls row, written by the terminal event's statement.
	var n int
	var tool string
	var isErr bool
	var sess []byte
	if err := e.pool.QueryRow(ctx, `select count(*) over (), tool, is_error, mcp_session from kontra.calls where task_id = $1`, id).Scan(&n, &tool, &isErr, &sess); err != nil || n != 1 || tool != "research" || isErr || len(sess) != 32 {
		t.Fatalf("calls row: %d %s %v %x %v", n, tool, isErr, sess, err)
	}
	// After the end: no status, no input request, no answer, and WriteTerminal leaves it (and writes no second
	// calls row).
	if _, err := e.log.Append(ctx, nil, func() Write {
		w, _ := MCPStatusWrite(row, StateWorking, "late", nil)
		return w
	}()); !errors.Is(err, taskstore.ErrConcurrentModification) {
		t.Fatalf("a status after the end: %v", err)
	}
	if _, err := e.log.AppendInputRequest(ctx, nil, id, json.RawMessage(`42`), func(g string) (json.RawMessage, error) {
		return json.RawMessage(`{}`), nil
	}); !errors.Is(err, taskstore.ErrConcurrentModification) {
		t.Fatalf("an input request after the end: %v", err)
	}
	if _, err := e.log.WriteTerminal(ctx, TerminalWrite{TaskID: id, State: StateFailed, Reason: ReasonTimeout}); err != nil {
		t.Fatal(err)
	}
	if err := e.pool.QueryRow(ctx, `select count(*) from kontra.calls where task_id = $1`, id).Scan(&n); err != nil || n != 1 {
		t.Fatalf("%d calls rows", n)
	}
}

// A gate-written end of an MCP task (write_terminal, the orphan sweep) is an MCP status row with kontra.failure or
// kontra.canceled and its calls row, is_error true (spec §11.8 cases 2 and 3); the fold still equals the projection.
func TestMCPTerminalWrites(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	failed, _ := e.mcpTask(StateSubmitted, "")
	if _, err := e.log.WriteTerminal(ctx, TerminalWrite{TaskID: failed, State: StateFailed, Reason: ReasonTimeout}); err != nil {
		t.Fatal(err)
	}
	orphan, _ := e.mcpTask(StateSubmitted, "")
	if ended, err := e.log.FailOrphan(ctx, orphan); err != nil || !ended {
		t.Fatalf("fail orphan: %v %v", ended, err)
	}
	canceled, _ := e.mcpTask(StateQuoted, "https://pay.example/q")
	if _, err := e.log.WriteTerminal(ctx, TerminalWrite{TaskID: canceled, State: StateCanceled, Canceled: CanceledQuoteExpired}); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[uuid.UUID]string{failed: "timeout", orphan: "lifecycle_unavailable", canceled: "quote_expired"} {
		row, err := e.log.Load(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		key := MetaFailure
		if row.State == StateCanceled {
			key = MetaCanceled
		}
		if row.Snapshot.Metadata[key] != want {
			t.Errorf("%s: %+v", want, row.Snapshot.Metadata)
		}
		var payload string
		if err := e.pool.QueryRow(ctx, `select payload::text from kontra.task_events where task_id = $1 and seq = $2`, id, row.Version).Scan(&payload); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(payload, `"status":"`+string(row.State)+`"`) || strings.Contains(payload, "TASK_STATE") {
			t.Errorf("%s: the terminal row is not an MCP status: %s", want, payload)
		}
		var n int
		if err := e.pool.QueryRow(ctx, `select count(*) from kontra.calls where task_id = $1 and is_error`, id).Scan(&n); err != nil || n != 1 {
			t.Errorf("%s: %d calls rows with is_error", want, n)
		}
		evs, _ := e.log.Events(ctx, id, 0, 1000)
		f := Folder{MCP: true}
		for _, ev := range evs {
			if err := f.Apply(ev); err != nil {
				t.Fatal(err)
			}
		}
		got, _ := json.Marshal(f.Task())
		proj, _ := json.Marshal(row.Snapshot)
		if string(got) != string(proj) {
			t.Errorf("%s: the fold differs from the projection", want)
		}
	}
}

// ListMCP lists the buyer's own MCP tasks of the service, newest first, with gate-minted cursors.
func TestListMCP(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	var ids []uuid.UUID
	for range MCPPageSize + 3 {
		id, _ := e.mcpTask(StateSubmitted, "")
		ids = append(ids, id)
	}
	scope := Scope{TenantID: e.tenant, Service: "research", BuyerID: buyerA.BuyerID}
	page, next, err := e.log.ListMCP(ctx, scope, "")
	if err != nil || len(page) != MCPPageSize || next == "" || page[0].ID != ids[len(ids)-1] {
		t.Fatalf("first page: %d %q %v", len(page), next, err)
	}
	rest, last, err := e.log.ListMCP(ctx, scope, next)
	if err != nil || len(rest) != 3 || last != "" || rest[2].ID != ids[0] {
		t.Fatalf("second page: %d %q %v", len(rest), last, err)
	}
	if other, _, err := e.log.ListMCP(ctx, Scope{TenantID: e.tenant, Service: "research", BuyerID: buyerB.BuyerID}, ""); err != nil || len(other) != 0 {
		t.Fatalf("another buyer's list: %d %v", len(other), err)
	}
	if _, _, err := e.log.ListMCP(ctx, scope, "not-a-cursor"); !errors.Is(err, ErrBadCursor) {
		t.Fatalf("a bad cursor: %v", err)
	}
}
