package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/mcprelay/mcptest"
	"kontra.sh/kontra/internal/mcpwire"
)

// The 2026-07-28 surface of /mcp/{svc} end to end (spec/relay.md §11.12, §16; D158–D162, D176–D180): two
// replicas, a node on B, the clients on A unless a row says otherwise.

// legacyNoSessionCode is rule 3's legacy answer's code (-32000, "Bad Request: No valid session ID provided").
const legacyNoSessionCode = -32000

// mcp_modern_task (§11.12, D159, D176, D178): a 2026-07-28 client's tools/call on a durable service whose upstream
// (the dual-era double) makes a task that asks one question: a CreateTaskResult with a gate id; tasks/get is
// input_required with one request keyed by a gate id, the same from either replica; tasks/update answers it, and a
// second answer to the key is ignored; tasks/get then shows completed with the result; tasks/result is 404 with
// -32601; another buyer's tasks/get is -32602. A request whose _meta protocolVersion differs from
// MCP-Protocol-Version is 400 with -32020 and reaches no upstream. A client that closes its response stream during
// the 2 s wait gets neither answer, and its task ends canceled within 10 s, the upstream seeing the cancel.
func TestMCPModernTask(t *testing.T) {
	h := start(t, Options{MCP: map[string]MCPService{"research": {}}, Peer: true})
	ctx := ctxFor(t, 10*time.Minute)
	up := mcptest.New()
	up.Tools = []json.RawMessage{dualEraTool("ask", ""), dualEraTool("slow", "")}
	modernTasks(up)
	dbl := h.Double(up)
	A, B := h.Gates[0], h.Gates[1]
	h.Front.Route(B)
	startNode(t, ctx, h, "host", nil, func(o *NodeOptions) { o.MCP = map[string]string{"research": dbl} })
	m := &ModernClient{Endpoint: A.URL + "/mcp/research", Key: h.APIKey, Tasks: true}
	if r, err := m.Request(ctx, 1, "server/discover", map[string]any{}); err != nil || r.Result() == nil {
		t.Fatalf("server/discover: %v %+v", err, r)
	}
	r, err := m.Request(ctx, 2, "tools/call", map[string]any{"name": "ask", "arguments": map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	res := r.Result()
	taskID, _ := res["taskId"].(string)
	if _, err := uuid.Parse(taskID); err != nil || res["resultType"] != "task" || res["pollIntervalMs"] != float64(1000) {
		t.Fatalf("not a CreateTaskResult with a gate task id: %d %s", r.Status, r.Text())
	}
	upTasks := up.ModernTasks()
	if len(upTasks) != 1 || upTasks[0].ID == taskID {
		t.Fatalf("the upstream's tasks %d (the gate's id must not be the upstream's)", len(upTasks))
	}
	get := func(g *Gate, c *ModernClient) *MCPResponse {
		t.Helper()
		r, err := c.At(g.URL+"/mcp/research").Request(ctx, 3, "tasks/get", map[string]any{"taskId": taskID})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	var key string
	eventually(t, 30*time.Second, "tasks/get input_required with one request", func() bool {
		res := get(A, m).Result()
		reqs, _ := res["inputRequests"].(map[string]any)
		for k := range reqs {
			key = k
		}
		return res["status"] == "input_required" && len(reqs) == 1
	})
	if !strings.HasPrefix(key, "k."+taskID+".") {
		t.Fatalf("the input request's key %q is not a gate id", key)
	}
	onA, onB := get(A, m).Result(), get(B, m).Result()
	if !reflect.DeepEqual(onA, onB) {
		t.Fatalf("tasks/get differs between the replicas:\n%v\n%v", onA, onB)
	}
	if strings.Contains(fmt.Sprint(onA), upTasks[0].ID) || strings.Contains(fmt.Sprint(onA), `"which"`) {
		t.Fatalf("the upstream's task id or key reached the client: %v", onA)
	}
	answer := func(id int, g *Gate, text string) {
		t.Helper()
		r, err := m.At(g.URL+"/mcp/research").Request(ctx, id, "tasks/update", map[string]any{"taskId": taskID,
			"inputResponses": map[string]any{key: map[string]any{"action": "accept", "content": map[string]any{"answer": text}}}})
		if err != nil || r.Result() == nil || r.Result()["resultType"] != "complete" {
			t.Fatalf("tasks/update: %v %+v", err, r)
		}
	}
	answer(4, A, "first")
	answer(5, B, "second")
	var done map[string]any
	eventually(t, 30*time.Second, "tasks/get completed", func() bool {
		done = get(B, m).Result()
		return done["status"] == "completed"
	})
	if b, _ := json.Marshal(done["result"]); !strings.Contains(string(b), "answered first") || strings.Contains(string(b), "second") {
		t.Fatalf("the completed task's result: %s", b)
	}
	if r, err := m.Request(ctx, 6, "tasks/result", map[string]any{"taskId": taskID}); err != nil || r.Status != http.StatusNotFound {
		t.Fatalf("tasks/result: %v %+v", err, r)
	} else if code, _, _, _ := r.Error(); code != -32601 {
		t.Fatalf("tasks/result: %s", r.Text())
	}
	other := &ModernClient{Endpoint: A.URL + "/mcp/research", Key: h.APIKeyB, Tasks: true}
	if r := get(A, other); func() bool { code, _, _, ok := r.Error(); return !ok || code != mcpwire.CodeInvalidParams }() {
		t.Fatalf("another buyer's tasks/get: %d %s", r.Status, r.Text())
	}

	// A _meta version that differs from the header: 400 -32020, nothing forwarded.
	seen := len(up.Requests())
	params := map[string]any{"name": "ask", "arguments": map[string]any{},
		"_meta": ModernMeta(mcpwire.V20251125, true)}
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 7, "method": "tools/call", "params": params})
	r, err = m.Raw(ctx, string(body), Headers("tools/call", params))
	if err != nil {
		t.Fatal(err)
	}
	if code, _, _, ok := r.Error(); r.Status != http.StatusBadRequest || !ok || code != -32020 {
		t.Fatalf("a _meta version other than the header: %d %s", r.Status, r.Text())
	}
	if n := len(up.Requests()); n != seen {
		t.Fatalf("the mismatched request reached the upstream (%d requests)", n-seen)
	}

	// Cancel on close (D178): the client leaves during the 2 s wait.
	before := map[string]bool{}
	for _, id := range texts(t, h, `select id::text from kontra.tasks where service = 'research'`) {
		before[id] = true
	}
	cctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	_, err = m.Request(cctx, 8, "tools/call", map[string]any{"name": "slow", "arguments": map[string]any{}})
	cancel()
	if err == nil {
		t.Fatal("an answer reached the client that left during the 2 s wait")
	}
	var leftTask string
	eventually(t, 5*time.Second, "the left client's task exists", func() bool {
		for _, id := range texts(t, h, `select id::text from kontra.tasks where service = 'research'`) {
			if !before[id] {
				leftTask = id
			}
		}
		return leftTask != ""
	})
	start := time.Now()
	eventually(t, 10*time.Second, "the left client's task canceled", func() bool {
		return scalar[string](t, h, `select state from kontra.tasks where id = $1`, leftTask) == "canceled"
	})
	t.Logf("canceled %v after the client left", time.Since(start).Round(100*time.Millisecond))
	if up.Count("tasks/cancel") == 0 && len(up.ModernTasks()) > 1 {
		t.Fatalf("the upstream task of the left client was neither cancelled nor avoided")
	}
}

// mcp_era_fallback (§11.12, D162, D177): a dual-era client's first modern request to a legacy-only service
// (server-everything@1.0.0), to a sealed service and to a stdio service (server-filesystem@1.0.0) gets the legacy
// 400 (-32000), and its call succeeds after initialize. The legacy-only upstream receives the gate's own
// server/discover and none of three modern requests a client sends within a minute; the sealed service's upstream
// is never probed. Against the dual-era double: a 404 it returns to one principal's modern request reaches that
// client, and another principal's next modern request is still forwarded and answered modern; the client stays
// modern and pins nothing; two concurrent first requests of a principal write one virtual session row. A client that
// succeeded modern on /mcp/dual then gets the legacy 400 from /mcp/legacy on the same origin: the residual of
// §11.12, logged, not asserted as a pass.
func TestMCPEraFallback(t *testing.T) {
	h := start(t, Options{MCP: map[string]MCPService{
		"legacy":    {ToolsDeny: EverythingDeny},
		"sealedsvc": {Sealed: "{required: false}"},
		"stdio":     {ToolsDeny: []string{"write_file", "edit_file", "create_directory", "move_file"}},
		"dual":      {},
	}, Peer: true})
	ctx := ctxFor(t, 10*time.Minute)
	rec := everythingBehindRecorder(t, h, "legacy")
	sealedUp := mcptest.New()
	sealedUp.Tools = []json.RawMessage{dualEraTool("echo", "")}
	dual := mcptest.New()
	dual.Modern = true
	dual.Tools = []json.RawMessage{dualEraTool("add", ""), dualEraTool("refuse-me", "")}
	dual.OnModern = func(q mcptest.Request) *mcptest.Reply {
		if q.Method == "tools/call" && strings.Contains(string(q.Params), `"refuse-me"`) {
			return &mcptest.Reply{Status: http.StatusNotFound, Raw: []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32001,"message":"Session not found"}}`)}
		}
		return nil
	}
	fsDir := tempDir(t, h, "fs")
	writeFile(t, fsDir, "a.txt", "a")
	h.Front.Route(h.Gates[1])
	startNode(t, ctx, h, "host", nil, func(o *NodeOptions) {
		o.MCP = map[string]string{"legacy": rec.URL, "sealedsvc": h.Double(sealedUp), "stdio": h.FilesystemCommand(t, fsDir), "dual": h.Double(dual)}
		o.Roots = []string{"stdio=" + fsDir}
	})
	A := h.Gates[0]

	// The legacy 400 first, then the legacy era works.
	for svc, call := range map[string][2]string{"legacy": {"echo", "message"}, "sealedsvc": {"echo", "text"}, "stdio": {"list_allowed_directories", ""}} {
		m := &ModernClient{Endpoint: A.URL + "/mcp/" + svc, Key: h.APIKey}
		args := map[string]any{}
		if call[1] != "" {
			args[call[1]] = "hi"
		}
		r, err := m.Request(ctx, 1, "tools/call", map[string]any{"name": call[0], "arguments": args})
		if err != nil {
			t.Fatal(err)
		}
		if code, _, _, ok := r.Error(); r.Status != http.StatusBadRequest || !ok || code != legacyNoSessionCode {
			t.Fatalf("%s: a modern request: %d %s, want the legacy 400", svc, r.Status, r.Text())
		}
		mc := NewMCPClient(A.URL+"/mcp/"+svc, h.APIKey)
		if _, err := mc.Initialize(ctx); err != nil {
			t.Fatal(err)
		}
		r, err = mc.Call(ctx, 2, call[0], args)
		if text, isErr := r.ToolText(); err != nil || r.Status != http.StatusOK || isErr || text == "" {
			t.Fatalf("%s: the call after initialize: %v %d %s", svc, err, r.Status, r.Text())
		}
	}
	// Three modern requests within a minute to the legacy-only service: none forwarded; the gate probed once.
	m := &ModernClient{Endpoint: A.URL + "/mcp/legacy", Key: h.APIKey}
	for i := 0; i < 3; i++ {
		r, err := m.Request(ctx, 10+i, "tools/call", map[string]any{"name": "echo", "arguments": map[string]any{"message": "again"}})
		if err != nil {
			t.Fatal(err)
		}
		if code, _, _, ok := r.Error(); r.Status != http.StatusBadRequest || !ok || code != legacyNoSessionCode {
			t.Fatalf("modern request %d to the legacy-only service: %d %s", i, r.Status, r.Text())
		}
	}
	var probes, modern int
	for _, q := range rec.Requests() {
		if q.Header.Get(mcpwire.HeaderProtocolVersion) != mcpwire.V20260728 {
			continue
		}
		modern++
		if q.Method == "server/discover" && q.Header.Get(mcpwire.HeaderSessionID) == "" {
			probes++
		}
	}
	if probes != 1 || modern != 1 {
		t.Fatalf("the legacy-only upstream received %d modern requests, %d of them the gate's server/discover: want the probe only", modern, probes)
	}
	if n := len(sealedUp.ModernRequests()); n != 0 {
		t.Fatalf("the sealed service's upstream received %d modern requests (a probe among them?)", n)
	}

	// The dual-era double: one principal's 404 teaches nothing.
	p1 := &ModernClient{Endpoint: A.URL + "/mcp/dual", Key: h.APIKey}
	r, err := p1.Request(ctx, 20, "tools/call", map[string]any{"name": "refuse-me", "arguments": map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != http.StatusNotFound || !strings.Contains(r.Text(), "Session not found") {
		t.Fatalf("the upstream's 404 to P1: %d %s", r.Status, r.Text())
	}
	p2 := &ModernClient{Endpoint: A.URL + "/mcp/dual", Key: h.APIKeyB}
	r, err = p2.Request(ctx, 21, "tools/call", map[string]any{"name": "add", "arguments": map[string]any{"text": "modern"}})
	if err != nil {
		t.Fatal(err)
	}
	if text, _ := r.ToolText(); r.Status != http.StatusOK || r.Result()["resultType"] != "complete" || text != "modern" {
		t.Fatalf("P2's modern request after P1's 404: %d %s", r.Status, r.Text())
	}
	rows := virtualRows(t, h, "dual", principalOf(t, h, h.APIKeyB))
	if len(rows) != 1 || rows[0].pinned {
		t.Fatalf("P2's virtual sessions: %+v, want one that pins nothing", rows)
	}
	// Two concurrent first requests of a third principal: one virtual row.
	key3, err := h.enroll(ctx, "p3")
	if err != nil {
		t.Fatal(err)
	}
	p3 := &ModernClient{Endpoint: A.URL + "/mcp/dual", Key: key3}
	var wg sync.WaitGroup
	statuses := make([]int, 2)
	for i := range statuses {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if r, err := p3.Request(ctx, 30+i, "tools/list", map[string]any{}); err == nil {
				statuses[i] = r.Status
			}
		}(i)
	}
	wg.Wait()
	if statuses[0] != http.StatusOK || statuses[1] != http.StatusOK {
		t.Fatalf("P3's concurrent first requests: %v", statuses)
	}
	if rows := virtualRows(t, h, "dual", principalOf(t, h, key3)); len(rows) != 1 || rows[0].pinned {
		t.Fatalf("P3's virtual sessions after two concurrent first requests: %+v, want one", rows)
	}
	// The residual of §11.12: a client that succeeded modern on /mcp/dual gets the legacy 400 from /mcp/legacy.
	r, err = p2.At(A.URL+"/mcp/legacy").Request(ctx, 40, "tools/list", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("the residual of §11.12 (D177), not asserted as a pass: after a modern success on /mcp/dual, a modern "+
		"request to /mcp/legacy on the same origin answers %d %s; a client that caches the era per origin and does "+
		"not probe again fails there", r.Status, strings.TrimSpace(r.Text()))
}

// TestMCPModernEverything2 runs the 2026-07-28 surface against server-everything@2.0.0 as well (D171), which needs
// Node 24 ($KONTRA_NODE24, or node on PATH): skipped, saying why, without it, or failed under KONTRA_REQUIRE_NODE24=1
// (CI sets it, M3 review robustness-6).
func TestMCPModernEverything2(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: skipped under -short")
	}
	Node24(t)
	h := start(t, Options{MCP: map[string]MCPService{"everything2": {ToolsDeny: EverythingDeny}}})
	ctx := ctxFor(t, 5*time.Minute)
	srv := h.StartEverything2(t, "everything2")
	startNode(t, ctx, h, "host", nil, func(o *NodeOptions) { o.MCP = map[string]string{"everything2": srv.URL} })
	m := &ModernClient{Endpoint: h.Gates[0].URL + "/mcp/everything2", Key: h.APIKey}
	r, err := m.Request(ctx, 1, "server/discover", map[string]any{})
	if err != nil || r.Result() == nil || r.Result()["cacheScope"] != "private" {
		t.Fatalf("server/discover: %v %+v", err, r)
	}
	r, err = m.Request(ctx, 2, "tools/call", map[string]any{"name": "echo", "arguments": map[string]any{"message": "modern"}})
	if text, _ := r.ToolText(); err != nil || r.Status != http.StatusOK || text != "Echo: modern" {
		t.Fatalf("a modern echo: %v %+v", err, r)
	}
}

// virtualRow is an open 2026-07-28 mcp_sessions row.
type virtualRow struct {
	pinned bool // an upstream session, a tunnel session or a replica named
}

// virtualRows reads the open virtual sessions of a principal on a service.
func virtualRows(t *testing.T, h *Harness, service, principal string) []virtualRow {
	t.Helper()
	rows, err := h.Pool.Query(context.Background(), `select upstream_sid is not null or tunnel_session is not null or replica_id is not null
		from kontra.mcp_sessions where service = $1 and principal = $2 and protocol_version = '2026-07-28' and closed_at is null`,
		service, principal)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []virtualRow
	for rows.Next() {
		var v virtualRow
		if err := rows.Scan(&v.pinned); err != nil {
			t.Fatal(err)
		}
		out = append(out, v)
	}
	return out
}

// principalOf is an API key's principal, key:<prefix>.
func principalOf(t *testing.T, h *Harness, key string) string {
	t.Helper()
	hash := auth.KeyHash(key)
	return "key:" + scalar[string](t, h, `select prefix from kontra.api_keys where hash = $1`, hash[:])
}
