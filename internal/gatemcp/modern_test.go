package gatemcp

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"

	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/lifecycle"
	"kontra.sh/kontra/internal/mcpwire"
	"kontra.sh/kontra/internal/store"
)

// server/discover on /mcp (§12.5, D269): the three versions, newest first; tools and the tasks extension; the
// gate's serverInfo in _meta; ttlMs and cacheScope private (2026-07-28 CacheableResult); resultType complete.
func TestDiscoverResult(t *testing.T) {
	b, err := DiscoverResult()
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		ResultType        string                     `json:"resultType"`
		SupportedVersions []string                   `json:"supportedVersions"`
		Capabilities      map[string]json.RawMessage `json:"capabilities"`
		Meta              map[string]map[string]any  `json:"_meta"`
		Instructions      string                     `json:"instructions"`
		TTLMs             *int64                     `json:"ttlMs"`
		CacheScope        string                     `json:"cacheScope"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.ResultType != "complete" || !reflect.DeepEqual(got.SupportedVersions, []string{"2026-07-28", "2025-11-25", "2025-06-18"}) ||
		got.CacheScope != "private" || got.TTLMs == nil || *got.TTLMs != 600000 || got.Instructions == "" {
		t.Errorf("server/discover: %s", b)
	}
	if string(got.Capabilities["tools"]) != `{}` || string(got.Capabilities["extensions"]) != `{"io.modelcontextprotocol/tasks":{}}` ||
		len(got.Capabilities) != 2 {
		t.Errorf("capabilities: %s", b)
	}
	if info := got.Meta["io.modelcontextprotocol/serverInfo"]; info["name"] != "kontra-gate" || info["version"] == nil {
		t.Errorf("serverInfo: %s", b)
	}
}

// The modern tools/list (§12.5): the same four tools with the same input schemas, no execution on any (2026-07-28
// removed it: the server decides), resultType complete, ttlMs and cacheScope private, one page.
func TestModernToolsList(t *testing.T) {
	b, err := ToolsList(mcpwire.EraModern, mcpwire.V20260728)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Tools      []map[string]json.RawMessage `json:"tools"`
		ResultType string                       `json:"resultType"`
		TTLMs      int64                        `json:"ttlMs"`
		CacheScope string                       `json:"cacheScope"`
		NextCursor *string                      `json:"nextCursor"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.ResultType != "complete" || got.TTLMs != 600000 || got.CacheScope != "private" || got.NextCursor != nil || len(got.Tools) != 4 {
		t.Fatalf("modern tools/list: %s", b)
	}
	legacy, _ := ToolsList(mcpwire.EraLegacy, mcpwire.V20250618)
	var old struct {
		Tools []map[string]json.RawMessage `json:"tools"`
	}
	_ = json.Unmarshal(legacy, &old)
	for i, tool := range got.Tools {
		if _, ok := tool["execution"]; ok {
			t.Errorf("%s has execution in the modern list", tool["name"])
		}
		if !bytes.Equal(tool["inputSchema"], old.Tools[i]["inputSchema"]) || !bytes.Equal(tool["name"], old.Tools[i]["name"]) ||
			!bytes.Equal(tool["outputSchema"], old.Tools[i]["outputSchema"]) {
			t.Errorf("tool %d differs from the legacy list: %s", i, tool["name"])
		}
	}
	// The legacy lists keep no modern members.
	for _, v := range []string{mcpwire.V20251125, mcpwire.V20250618} {
		l, _ := ToolsList(mcpwire.EraLegacy, v)
		if bytes.Contains(l, []byte(`"resultType"`)) || bytes.Contains(l, []byte(`"cacheScope"`)) {
			t.Errorf("the %s list carries modern members: %s", v, l)
		}
	}
}

// A hire task as a modern reader sees it (§12.4, §12.5, D274, §11.12's table): quoted is working with the approve
// URL (D21); a text-only question is input_required for a reader whose request declared form elicitation, else
// working with the reply-tool text; failed and rejected are completed (their result a CallToolResult with isError, the
// tasks extension's "failed" being for JSON-RPC errors only) with sh.kontra/failure; canceled is cancelled.
func TestModernView(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	svc := store.ServiceRef{Timeout: time.Hour, Retention: 24 * time.Hour}
	expires := now.Add(10 * time.Minute)
	row := &store.TaskRow{ID: uuid.Must(uuid.NewV7()), State: store.StateQuoted, CreatedAt: now, StatusAt: now.Add(time.Second),
		QuoteExpiresAt: &expires,
		Snapshot:       taskWith(a2a.TaskStateInputRequired, nil, map[string]any{lifecycle.MetaApproveURL: "https://app.kontra.sh/#/t/x"})}
	form := Reader{Era: mcpwire.EraModern, FormElicitation: true}

	b, err := modernCreateTaskResult(row, svc, form)
	if err != nil {
		t.Fatal(err)
	}
	var ct map[string]any
	_ = json.Unmarshal(b, &ct)
	want := map[string]any{"resultType": "task", "taskId": row.ID.String(), "status": "working",
		"statusMessage": "https://app.kontra.sh/#/t/x", "createdAt": "2026-10-28T12:00:00.000Z",
		"lastUpdatedAt": "2026-10-28T12:00:01.000Z", "ttlMs": float64((10*time.Minute + time.Hour + 24*time.Hour).Milliseconds()),
		"pollIntervalMs": float64(1000), "_meta": map[string]any{MetaApproveURL: "https://app.kontra.sh/#/t/x"}}
	if !reflect.DeepEqual(ct, want) {
		t.Errorf("a quoted task's CreateTaskResult\n%v\nwant\n%v", ct, want)
	}

	for _, tc := range []struct {
		name   string
		state  store.State
		task   *a2a.Task
		rd     Reader
		status string
		meta   map[string]any
	}{
		{"working", store.StateWorking, taskWith(a2a.TaskStateWorking, nil, nil), form, "working", nil},
		{"question, form", store.StateInputRequired, taskWith(a2a.TaskStateInputRequired, agentText("Which date?"), nil), form, "input_required", nil},
		{"question, no form", store.StateInputRequired, taskWith(a2a.TaskStateInputRequired, agentText("Which date?"), nil),
			Reader{Era: mcpwire.EraModern}, "working", map[string]any{MetaInputRequired: true}},
		{"completed", store.StateCompleted, taskWith(a2a.TaskStateCompleted, agentText("done"), nil), form, "completed", nil},
		{"failed", store.StateFailed, taskWith(a2a.TaskStateFailed, nil, map[string]any{store.MetaFailure: "timeout"}), form, "completed",
			map[string]any{MetaFailure: "timeout"}},
		{"rejected", store.StateRejected, taskWith(a2a.TaskStateRejected, agentText("no"), nil), form, "completed",
			map[string]any{MetaFailure: FailureRejected}},
		{"canceled", store.StateCanceled, taskWith(a2a.TaskStateCanceled, nil, map[string]any{store.MetaCanceled: "client"}), form, "cancelled",
			map[string]any{MetaCanceled: "client"}},
	} {
		row.State, row.Snapshot = tc.state, tc.task
		v := modernView(row, tc.rd)
		if v.status != tc.status || !reflect.DeepEqual(v.meta, tc.meta) {
			t.Errorf("%s: %+v, want %s with %v", tc.name, v, tc.status, tc.meta)
		}
		if tc.status == "working" && tc.meta != nil && !strings.HasSuffix(v.statusMessage, "(answer with the reply tool, taskId "+string(tc.task.ID)+")") {
			t.Errorf("%s: statusMessage %q", tc.name, v.statusMessage)
		}
		// The legacy reader of the same task: failed and rejected stay failed (§11.8, §12.2), the rest as above.
		if lv := view(row, Reader{FormElicitation: tc.rd.FormElicitation}); (tc.state == store.StateFailed || tc.state == store.StateRejected) != (lv.status != v.status) {
			t.Errorf("%s: the legacy view %s, the modern %s", tc.name, lv.status, v.status)
		}
	}
}

// withResultType sets resultType on an object that has none, and never replaces one.
func TestWithResultType(t *testing.T) {
	for in, want := range map[string]string{
		`{"content":[]}`:                     `{"content":[],"resultType":"complete"}`,
		`{"resultType":"task","taskId":"x"}`: `{"resultType":"task","taskId":"x"}`,
		`{}`:                                 `{"resultType":"complete"}`,
		`[1]`:                                `[1]`,
	} {
		if got := withResultType(json.RawMessage(in), resultComplete); string(got) != want {
			t.Errorf("withResultType(%s) = %s, want %s", in, got, want)
		}
	}
}

// modernRequest builds a modern request to /mcp: the MCP-Protocol-Version, Mcp-Method and (for methods with one)
// Mcp-Name headers equal to the body's, which hdr may override (an empty value removes a header).
func modernRequest(t *testing.T, method string, params map[string]any, hdr map[string]string) (*request, *httptest.ResponseRecorder) {
	t.Helper()
	if params == nil {
		params = map[string]any{}
	}
	if _, ok := params["_meta"]; !ok {
		params["_meta"] = map[string]any{mcpwire.MetaProtocolVersion: mcpwire.V20260728, mcpwire.MetaClientCapabilities: map[string]any{}}
	}
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 7, "method": method, "params": params})
	return modernRaw(t, method, body, params, hdr)
}

func modernRaw(t *testing.T, method string, body []byte, params map[string]any, hdr map[string]string) (*request, *httptest.ResponseRecorder) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, Path, bytes.NewReader(body))
	r.Header.Set(mcpwire.HeaderProtocolVersion, mcpwire.V20260728)
	r.Header.Set(mcpwire.HeaderMethod, method)
	if src, ok := mcpwire.NameSource(method); ok {
		if v, ok := params[src].(string); ok {
			r.Header.Set(mcpwire.HeaderName, mcpwire.EncodeHeaderValue(v))
		}
	}
	for k, v := range hdr {
		if v == "" {
			r.Header.Del(k)
		} else {
			r.Header.Set(k, v)
		}
	}
	m, err := mcpwire.Parse(body, mcpwire.FromClient)
	if err != nil {
		t.Fatalf("%s: %v", body, err)
	}
	if era, ref := mcpwire.Classify(r, m); ref != nil || era != mcpwire.EraModern {
		t.Fatalf("%s is not a modern request: %v %v", body, era, ref)
	}
	w := httptest.NewRecorder()
	tid := uuid.Must(uuid.NewV7())
	p := auth.Principal{Kind: auth.KindKey, ID: "key:kk_test", TenantID: &tid, BuyerID: uuid.Must(uuid.NewV7()), Via: auth.ViaAPIKey,
		Scopes: []auth.Scope{auth.ScopeHire}}
	return &request{w: w, r: r, p: p, body: body, msg: m, modern: true}, w
}

type answer struct {
	status int
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	} `json:"error"`
	ID   json.RawMessage `json:"id"`
	body string
}

func answerOf(t *testing.T, w *httptest.ResponseRecorder) answer {
	t.Helper()
	b, _ := io.ReadAll(w.Result().Body)
	a := answer{status: w.Code, body: string(b)}
	if len(bytes.TrimSpace(b)) > 0 {
		if err := json.Unmarshal(b, &a); err != nil {
			t.Fatalf("%d %s: %v", w.Code, b, err)
		}
	}
	return a
}

// The modern surface's answers that need nothing but the request (§12.5, §11.12; D176, D295): CheckModern's
// refusals (a missing _meta field -32602, a version, Mcp-Method or Mcp-Name that differs from the body -32020, a
// response 400 -32600, a notification 202), server/discover and tools/list, tasks/result, tasks/list, ping and an
// unknown method 404 -32601, tasks/* without the tasks extension 400 -32021, subscriptions/listen asking for task
// notifications without the extension -32021 and any other 404 -32601. Nothing a modern request sends opens a
// session or names one: no Mcp-Session-Id is ever written.
func TestModernRequests(t *testing.T) {
	h := &Handler{deps: Deps{PublicURL: "https://gate.example"}, log: slog.New(slog.DiscardHandler)}
	ext := map[string]any{mcpwire.MetaProtocolVersion: mcpwire.V20260728,
		mcpwire.MetaClientCapabilities: map[string]any{"extensions": map[string]any{mcpwire.ExtTasks: map[string]any{}}}}
	task := uuid.Must(uuid.NewV7()).String()
	for _, tc := range []struct {
		name   string
		method string
		params map[string]any
		hdr    map[string]string
		status int
		code   int
	}{
		{"mismatched protocolVersion", "tools/list", map[string]any{"_meta": map[string]any{mcpwire.MetaProtocolVersion: "2025-11-25",
			mcpwire.MetaClientCapabilities: map[string]any{}}}, nil, 400, -32020},
		{"no protocolVersion", "tools/list", map[string]any{"_meta": map[string]any{mcpwire.MetaClientCapabilities: map[string]any{}}}, nil, 400, -32602},
		{"no clientCapabilities", "tools/list", map[string]any{"_meta": map[string]any{mcpwire.MetaProtocolVersion: "2026-07-28"}}, nil, 400, -32602},
		{"Mcp-Method differs", "tools/list", nil, map[string]string{mcpwire.HeaderMethod: "tools/call"}, 400, -32020},
		{"Mcp-Name differs", "tools/call", map[string]any{"name": "hire", "arguments": map[string]any{}}, map[string]string{mcpwire.HeaderName: "search"}, 400, -32020},
		{"Mcp-Name missing", "tasks/get", map[string]any{"taskId": task, "_meta": ext}, map[string]string{mcpwire.HeaderName: ""}, 400, -32020},
		{"tasks/result", "tasks/result", map[string]any{"taskId": task, "_meta": ext}, nil, 404, -32601},
		{"tasks/list", "tasks/list", map[string]any{"_meta": ext}, nil, 404, -32601},
		{"ping", "ping", nil, nil, 404, -32601},
		{"an unknown method", "resources/list", nil, nil, 404, -32601},
		{"tasks/get without the extension", "tasks/get", map[string]any{"taskId": task}, nil, 400, -32021},
		{"tasks/update without the extension", "tasks/update", map[string]any{"taskId": task, "inputResponses": map[string]any{}}, nil, 400, -32021},
		{"tasks/cancel without the extension", "tasks/cancel", map[string]any{"taskId": task}, nil, 400, -32021},
		{"listen for tasks without the extension", "subscriptions/listen", map[string]any{"notifications": map[string]any{"taskIds": []string{task}}}, nil, 400, -32021},
		{"listen for tasks with it", "subscriptions/listen", map[string]any{"notifications": map[string]any{"taskIds": []string{task}}, "_meta": ext}, nil, 404, -32601},
		{"listen for tools", "subscriptions/listen", map[string]any{"notifications": map[string]any{"toolsListChanged": true}}, nil, 404, -32601},
		{"an unknown tool", "tools/call", map[string]any{"name": "rm", "arguments": map[string]any{}}, nil, 200, -32602},
	} {
		rq, w := modernRequest(t, tc.method, tc.params, tc.hdr)
		h.modern(rq)
		a := answerOf(t, w)
		if a.status != tc.status || a.Error == nil || a.Error.Code != tc.code || string(a.ID) != "7" {
			t.Errorf("%s: %d %s, want %d with %d", tc.name, a.status, a.body, tc.status, tc.code)
		}
		if tc.code == -32021 && !strings.Contains(string(a.Error.Data), `"requiredCapabilities":{"extensions":{"io.modelcontextprotocol/tasks":{}}}`) {
			t.Errorf("%s: -32021's data %s", tc.name, a.Error.Data)
		}
		if w.Header().Get(mcpwire.HeaderSessionID) != "" {
			t.Errorf("%s: a modern answer named a session", tc.name)
		}
	}

	// A notification is 202, a response 400 -32600.
	rq, w := modernRaw(t, "notifications/cancelled", []byte(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{}}`), nil, nil)
	h.modern(rq)
	if a := answerOf(t, w); a.status != http.StatusAccepted || a.body != "" {
		t.Errorf("a notification: %d %s", a.status, a.body)
	}
	rq, w = modernRaw(t, "", []byte(`{"jsonrpc":"2.0","id":"k.x.1","result":{"action":"accept"}}`), nil, nil)
	h.modern(rq)
	if a := answerOf(t, w); a.status != http.StatusBadRequest || a.Error == nil || a.Error.Code != -32600 {
		t.Errorf("a response: %d %s", a.status, a.body)
	}

	// server/discover and tools/list, answered by the gate itself, with an Mcp-Session-Id header that is ignored.
	for _, method := range []string{"server/discover", "tools/list"} {
		rq, w := modernRequest(t, method, nil, map[string]string{mcpwire.HeaderSessionID: "a-legacy-session"})
		h.modern(rq)
		a := answerOf(t, w)
		want, _ := DiscoverResult()
		if method == "tools/list" {
			want, _ = ToolsList(mcpwire.EraModern, mcpwire.V20260728)
		}
		if a.status != http.StatusOK || a.Error != nil || !bytes.Equal(a.Result, want) || w.Header().Get(mcpwire.HeaderSessionID) != "" {
			t.Errorf("%s: %d %s", method, a.status, a.body)
		}
	}
}
