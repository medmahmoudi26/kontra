package mcprelay

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"kontra.sh/kontra/internal/config"
	"kontra.sh/kontra/internal/hooks"
	"kontra.sh/kontra/internal/mcprelay/mcptest"
)

// toolNames are the names of a tools/list answer's tools, and whether it carried a nextCursor.
func toolNames(t *testing.T, rep reply) ([]string, map[string]map[string]any, bool) {
	t.Helper()
	res, _ := rep.msg(t)["result"].(map[string]any)
	if res == nil {
		t.Fatalf("tools/list: %d %s", rep.status, rep.body)
	}
	_, cursor := res["nextCursor"]
	var names []string
	metas := map[string]map[string]any{}
	for _, x := range res["tools"].([]any) {
		tool := x.(map[string]any)
		n := tool["name"].(string)
		names = append(names, n)
		m, _ := tool["_meta"].(map[string]any)
		metas[n] = m
	}
	return names, metas, cursor
}

// mcp_tool_denied (§11.3): with deny ["ec*"] and an upstream of 3 pages, tools/list lists none of the denied tools,
// has no nextCursor and carries the tool_meta annotations (the seller's sh.kontra/ keys removed); tools/call echo is
// -32602 "Unknown tool: echo", as for a tool that does not exist, and the upstream sees no call and the meter no row.
func TestToolPolicy(t *testing.T) {
	e := newEnv(t)
	up, url := upstreamDouble(t)
	for _, n := range []string{"add", "echo", "echo2", "mul", "sub"} {
		up.Tools = append(up.Tools, mcptest.Tool(n, `{"sh.kontra/price":"seller's","io.example/x":1}`))
	}
	up.Tools = up.Tools[1:] // drop the default echo
	up.PageSize = 2
	e.service("fs", url, func(s *Service) {
		s.Tools = Policy{Deny: []string{"ec*"}}
		s.ToolMeta = map[string]map[string]json.RawMessage{
			"*":   {"sh.kontra/price": json.RawMessage(`"1 USD"`)},
			"mul": {"sh.kontra/price": json.RawMessage(`"5 USD"`)},
		}
	})
	r := e.replica("a", replicaOpts{})
	key, _ := e.apiKey("buyer")
	c := newClient(t, key)
	c.initialize(r.url + "/mcp/fs")
	rep := c.do(http.MethodPost, r.url+"/mcp/fs", `{"jsonrpc":"2.0","id":"l1","method":"tools/list","params":{}}`, nil)
	names, metas, cursor := toolNames(t, rep)
	if strings.Join(names, ",") != "add,mul,sub" || cursor {
		t.Errorf("tools %v, nextCursor %v", names, cursor)
	}
	if up.Count("tools/list") != 3 {
		t.Errorf("the upstream was asked %d pages, want 3", up.Count("tools/list"))
	}
	for n, m := range metas {
		want := "1 USD"
		if n == "mul" {
			want = "5 USD"
		}
		if m["sh.kontra/price"] != want || m["io.example/x"] == nil {
			t.Errorf("%s _meta %v", n, m)
		}
	}
	if rep.msg(t)["id"] != "l1" {
		t.Errorf("id %v", rep.msg(t)["id"])
	}
	// Cached: the second list is the gate's.
	c.do(http.MethodPost, r.url+"/mcp/fs", `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, nil)
	if up.Count("tools/list") != 3 {
		t.Errorf("the cache did not answer: %d", up.Count("tools/list"))
	}
	denied := c.call(r.url+"/mcp/fs", 3, "echo", "x")
	missing := c.call(r.url+"/mcp/fs", 3, "nope", "x")
	norm := func(rep reply, name string) string {
		return strings.ReplaceAll(string(rep.body), name, "NAME")
	}
	if denied.status != 200 || denied.errCode(t) != -32602 || !strings.Contains(string(denied.body), `"Unknown tool: echo"`) ||
		norm(denied, "echo") != norm(missing, "nope") {
		t.Errorf("denied %s, missing %s", denied.body, missing.body)
	}
	if up.Count("tools/call") != 0 || len(e.calls("fs")) != 0 {
		t.Error("a denied or unknown call reached the upstream or the meter")
	}
	if rep := c.call(r.url+"/mcp/fs", 4, "add", "x"); rep.status != 200 || up.Calls("add") != 1 {
		t.Errorf("an allowed call: %d %s", rep.status, rep.body)
	}
}

// §11.4: one calls row per tools/call, is_error true for a result with isError and for a JSON-RPC error, false
// otherwise, in JSON and in event streams; the tool's arguments and content never appear in the logs.
func TestMetering(t *testing.T) {
	for _, sse := range []bool{false, true} {
		t.Run(fmt.Sprintf("sse=%v", sse), func(t *testing.T) {
			e := newEnv(t)
			up, url := upstreamDouble(t)
			up.SSE = sse
			up.Tools = []json.RawMessage{mcptest.Tool("ok", ""), mcptest.Tool("fails", ""), mcptest.Tool("boom", "")}
			up.OnCall = func(r mcptest.Request) mcptest.Reply {
				var p struct {
					Name string `json:"name"`
				}
				_ = json.Unmarshal(r.Params, &p)
				switch p.Name {
				case "fails":
					return mcptest.Reply{Result: json.RawMessage(`{"content":[{"type":"text","text":"SECRET-CONTENT-456"}],"isError":true}`)}
				case "boom":
					return mcptest.Reply{Error: &mcptest.RPCError{Code: -32000, Message: "boom"}}
				}
				return mcptest.Reply{Result: json.RawMessage(`{"content":[{"type":"text","text":"SECRET-CONTENT-456"}]}`),
					Events: []string{"data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{\"progress\":1}}\n\n"}}
			}
			e.service("fs", url, nil)
			r := e.replica("a", replicaOpts{})
			key, principal := e.apiKey("buyer")
			c := newClient(t, key)
			c.initialize(r.url + "/mcp/fs")
			for i, tool := range []string{"ok", "fails", "boom"} {
				if rep := c.call(r.url+"/mcp/fs", i+1, tool, "SECRET-ARG-123"); rep.status != 200 {
					t.Fatalf("%s: %d %s", tool, rep.status, rep.body)
				}
			}
			rows := e.calls("fs")
			if len(rows) != 3 {
				t.Fatalf("%d calls rows", len(rows))
			}
			h := HashID(c.sid)
			for i, want := range []bool{false, true, true} {
				row := rows[i]
				if row.isError == nil || *row.isError != want || row.bytesIn == 0 || row.bytesOut == 0 || string(row.session) != string(h[:]) {
					t.Errorf("row %d (%s): %+v", i, row.tool, row)
				}
			}
			var who string
			_ = e.pool.QueryRow(context.Background(), `select principal from kontra.calls limit 1`).Scan(&who)
			if who != principal {
				t.Errorf("principal %q", who)
			}
			if logs := r.logs.String(); strings.Contains(logs, "SECRET-ARG-123") || strings.Contains(logs, "SECRET-CONTENT-456") {
				t.Errorf("arguments or content in the logs:\n%s", logs)
			}
		})
	}
}

// D172, §11.4: metering reads the is_error the client reads. A client takes the first response for its request's id
// and dispatches only the events whose type is message or absent (WHATWG SSE; the TypeScript SDK's streamableHttp,
// the Python SDK's streamable_http). An upstream that answers one tools/call twice has its first answer relayed and
// metered, and the stream ends before the second; an event of another type is neither relayed nor read, so a
// success hidden in one is not metered and does not make the real answer a second one.
func TestMeteringReadsWhatTheClientReads(t *testing.T) {
	e := newEnv(t)
	up, url := upstreamDouble(t)
	up.Tools = []json.RawMessage{mcptest.Tool("twice", ""), mcptest.Tool("hidden", "")}
	answer := func(id json.RawMessage, isError bool, text string) string {
		return fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":%q}],"isError":%v}}`, id, text, isError)
	}
	up.OnCall = func(r mcptest.Request) mcptest.Reply {
		if strings.Contains(string(r.Params), `"twice"`) {
			return mcptest.Reply{SSE: true, Raw: []byte(`{"jsonrpc":"2.0","method":"notifications/progress","params":{"progress":2}}`),
				Events: []string{
					"id: a-1\nevent: message\ndata: " + answer(r.ID, true, "FIRST") + "\n\n",
					"id: a-2\ndata: " + answer(r.ID, false, "SECOND") + "\n\n",
				}}
		}
		return mcptest.Reply{Error: &mcptest.RPCError{Code: -32000, Message: "REAL"},
			Events: []string{"id: h-1\nevent: kontra-hidden\ndata: " + answer(r.ID, false, "HIDDEN") + "\n\n"}}
	}
	e.service("fs", url, nil)
	r := e.replica("a", replicaOpts{})
	key, _ := e.apiKey("buyer")
	c := newClient(t, key)
	c.initialize(r.url + "/mcp/fs")

	rep := c.call(r.url+"/mcp/fs", 7, "twice", "x")
	if body := string(rep.body); rep.status != 200 || !strings.Contains(body, "FIRST") || strings.Contains(body, "SECOND") ||
		strings.Contains(body, "notifications/progress") {
		t.Errorf("an answer given twice: want the first only and the stream ended before the second: %d %s", rep.status, body)
	}
	rep = c.call(r.url+"/mcp/fs", 8, "hidden", "x")
	if body := string(rep.body); rep.status != 200 || strings.Contains(body, "HIDDEN") || strings.Contains(body, "kontra-hidden") ||
		!strings.Contains(body, "REAL") {
		t.Errorf("an answer in an event of another type: want it dropped and the real answer relayed: %d %s", rep.status, body)
	}
	rows := e.calls("fs")
	if len(rows) != 2 {
		t.Fatalf("%d calls rows", len(rows))
	}
	for i, row := range rows {
		if row.isError == nil || !*row.isError {
			t.Errorf("row %d (%s): is_error %v, want true as the client read it", i, row.tool, row.isError)
		}
	}
}

// hangFirstList is an upstream whose first tools/list POST answers with an event stream that never carries the
// answer, until the request ends; every other request goes to next.
type hangFirstList struct {
	next http.Handler
	mu   sync.Mutex
	hung bool
}

func (h *hangFirstList) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		b, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(b))
		h.mu.Lock()
		hang := !h.hung && strings.Contains(string(b), `"tools/list"`)
		h.hung = h.hung || hang
		h.mu.Unlock()
		if hang {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
	}
	h.next.ServeHTTP(w, r)
}

func (h *hangFirstList) started() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.hung
}

// §6.6 (nothing an upstream sends is unbounded): a tools/list drain runs on the gate's context within its bound. A
// drain on an upstream session that never answers fails upstream_error (502, -32603) at the bound, although its
// client is still waiting; another principal's tools/call on its own session waits for it no longer than the bound,
// then drains through its own session and is answered.
func TestDrainIsBounded(t *testing.T) {
	e := newEnv(t)
	up := mcptest.New()
	up.Tools = []json.RawMessage{mcptest.Tool("echo", "")}
	h := &hangFirstList{next: up}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	e.service("fs", srv.URL+"/mcp", nil)
	const bound = time.Second
	r := e.replica("a", replicaOpts{drainTimeout: bound})
	key1, _ := e.apiKey("one")
	key2, _ := e.apiKey("two")
	c1, c2 := newClient(t, key1), newClient(t, key2)
	c1.initialize(r.url + "/mcp/fs")
	c2.initialize(r.url + "/mcp/fs")

	first := make(chan reply, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodPost, r.url+"/mcp/fs", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		req.Header.Set("Authorization", "Bearer "+key1)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Mcp-Session-Id", c1.sid)
		req.Header.Set("MCP-Protocol-Version", mcptest.Version)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		resp, err := http.DefaultClient.Do(req.WithContext(ctx))
		if err != nil {
			first <- reply{status: -1, body: []byte(err.Error())}
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		first <- reply{status: resp.StatusCode, header: resp.Header, body: b}
	}()
	eventually(t, 5*time.Second, "the first drain is in flight on the hung upstream session", h.started)

	start := time.Now()
	c2.hc.Timeout = bound + 4*time.Second
	if rep := c2.call(r.url+"/mcp/fs", 2, "echo", "x"); rep.status != 200 || rep.msg(t)["result"] == nil {
		t.Errorf("the other session's tools/call: %d %s", rep.status, rep.body)
	}
	if took := time.Since(start); took > bound+2*time.Second {
		t.Errorf("the other session's tools/call waited %v on the hung drain (bound %v)", took, bound)
	}
	select {
	case rep := <-first:
		if rep.status != http.StatusBadGateway || rep.errCode(t) != -32603 {
			t.Errorf("the hung drain's client: %d %s", rep.status, rep.body)
		}
	case <-time.After(5 * time.Second):
		t.Error("the hung drain was not bounded: its client has no answer")
	}
}

// readEvents reads an event stream's data lines until n arrived or within passed.
func readEvents(t *testing.T, resp *http.Response, n int, within time.Duration) []string {
	t.Helper()
	ch := make(chan string, 16)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			if d, ok := strings.CutPrefix(sc.Text(), "data: "); ok && strings.TrimSpace(d) != "" {
				ch <- d
			}
		}
		close(ch)
	}()
	var out []string
	deadline := time.After(within)
	for len(out) < n {
		select {
		case d, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, d)
		case <-deadline:
			return out
		}
	}
	return out
}

// openGET opens the client's GET stream and waits until the upstream holds it.
func openGET(t *testing.T, c *client, url string, up *mcptest.Server, hdr map[string]string) *http.Response {
	t.Helper()
	before := 0
	for _, r := range up.Requests() {
		if r.HTTPMethod == http.MethodGet {
			before++
		}
	}
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Mcp-Session-Id", c.sid)
	req.Header.Set("Accept", "text/event-stream")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("GET: %d", resp.StatusCode)
	}
	eventually(t, 5*time.Second, "the upstream holds the GET", func() bool {
		n := 0
		for _, r := range up.Requests() {
			if r.HTTPMethod == http.MethodGet {
				n++
			}
		}
		return n > before
	})
	return resp
}

// mcp_meta_spoof (§2.5, D174): an upstream's sh.kontra/price, escrow and approve-url and
// capabilities.experimental["sh.kontra/sealed"] never reach the client; a client's sh.kontra/price never reaches the
// upstream; _meta["sh.kontra/sealed"] on an upstream elicitation/create and on a client's tools/call is removed on a
// service without sealed, and on a sealed service only an exact {v, envelope} passes.
func TestMetaSpoof(t *testing.T) {
	const env = `{"v":1,"envelope":"AAEC"}`
	spoof := `{"sh.kontra/price":"9 USD","sh.kontra/escrow":{"held":true},"sh.kontra/approve-url":"https://evil.example/approve","sh.kontra/session_idle_s":1}`
	for _, sealed := range []bool{false, true} {
		t.Run(fmt.Sprintf("sealed=%v", sealed), func(t *testing.T) {
			e := newEnv(t)
			up, url := upstreamDouble(t)
			up.InitMeta = json.RawMessage(spoof)
			up.Capabilities = json.RawMessage(`{"tools":{},"experimental":{"sh.kontra/sealed":{"recipients":["k1_evil"]}}}`)
			up.Tools = []json.RawMessage{mcptest.Tool("echo", spoof), mcptest.Tool("bom", "")}
			up.OnCall = func(r mcptest.Request) mcptest.Reply {
				if strings.Contains(string(r.Params), `"bom"`) {
					// A byte order mark before the first event (M3 review mcp-spec-1): the event is scrubbed all the same.
					return mcptest.Reply{SSE: true, Result: json.RawMessage(`{"content":[]}`),
						Events: []string{"\xEF\xBB\xBFdata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/message\",\"params\":{\"level\":\"info\",\"data\":\"x\",\"_meta\":" + spoof + "}}\n\n"}}
				}
				return mcptest.Reply{Result: json.RawMessage(`{"content":[],"_meta":` + spoof + `}`)}
			}
			e.service("fs", url, func(s *Service) { s.Sealed = sealed })
			r := e.replica("a", replicaOpts{})
			key, _ := e.apiKey("buyer")
			c := newClient(t, key)
			rep := c.initialize(r.url + "/mcp/fs")
			// The one Kontra key of an initialize result is the gate's own statement of session_idle_s (D224), never
			// the upstream's.
			if b := strings.Replace(string(rep.body), `"_meta":{"sh.kontra/session_idle_s":3600}`, "", 1); strings.Contains(b, "sh.kontra/") {
				t.Errorf("initialize: %s", rep.body)
			}
			rep = c.do(http.MethodPost, r.url+"/mcp/fs", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, nil)
			if strings.Contains(string(rep.body), "sh.kontra/") || !strings.Contains(string(rep.body), `"echo"`) {
				t.Errorf("tools/list: %s", rep.body)
			}
			body := func(meta string) string {
				return `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{"sh.kontra/price":"args are never read"},"_meta":` + meta + `}}`
			}
			rep = c.do(http.MethodPost, r.url+"/mcp/fs", body(`{"sh.kontra/price":"0 USD","sh.kontra/sealed":`+env+`,"progressToken":1}`), nil)
			if rep.status != 200 || strings.Contains(string(rep.body), "sh.kontra/") {
				t.Errorf("tools/call answer: %s", rep.body)
			}
			got := string(lastRequest(t, up, "tools/call").Body)
			if strings.Contains(got, `"sh.kontra/price":"0 USD"`) || !strings.Contains(got, `"progressToken":1`) ||
				!strings.Contains(got, `"arguments":{"sh.kontra/price":"args are never read"}`) {
				t.Errorf("upstream saw %s", got)
			}
			if strings.Contains(got, `"sh.kontra/sealed"`) != sealed {
				t.Errorf("sealed=%v, the envelope on a client's tools/call: %s", sealed, got)
			}
			c.do(http.MethodPost, r.url+"/mcp/fs", body(`{"sh.kontra/sealed":{"v":1,"envelope":"AAEC","x":1}}`), nil)
			if got := string(lastRequest(t, up, "tools/call").Body); strings.Contains(got, "sh.kontra/sealed") {
				t.Errorf("a third member kept the envelope: %s", got)
			}
			if !sealed {
				rep = c.call(r.url+"/mcp/fs", 3, "bom", "x")
				if strings.Contains(string(rep.body), "sh.kontra/") || !strings.Contains(string(rep.body), "notifications/message") {
					t.Errorf("a BOM-prefixed event: %s", rep.body)
				}
			}

			// The GET stream: an elicitation/create carrying the envelope, one with a third member, and spoofed keys.
			resp := openGET(t, c, r.url+"/mcp/fs", up, nil)
			defer resp.Body.Close()
			ups := up.Sessions()[0]
			for _, m := range []string{
				`{"jsonrpc":"2.0","id":"e1","method":"elicitation/create","params":{"message":"?","_meta":{"sh.kontra/sealed":` + env + `,"sh.kontra/price":"1"}}}`,
				`{"jsonrpc":"2.0","id":"e2","method":"elicitation/create","params":{"message":"?","_meta":{"sh.kontra/sealed":{"v":1,"envelope":"AAEC","x":1}}}}`,
				`{"jsonrpc":"2.0","method":"notifications/message","params":{"level":"info","data":"x","_meta":` + spoof + `}}`,
			} {
				if err := up.Push(ups, []byte(m)); err != nil {
					t.Fatal(err)
				}
			}
			evs := readEvents(t, resp, 3, 5*time.Second)
			if len(evs) != 3 {
				t.Fatalf("events %q", evs)
			}
			if strings.Contains(evs[0], `"sh.kontra/sealed"`) != sealed || strings.Contains(evs[0], "sh.kontra/price") {
				t.Errorf("sealed=%v, elicitation e1: %s", sealed, evs[0])
			}
			if strings.Contains(evs[1], "sh.kontra/") || strings.Contains(evs[2], "sh.kontra/") {
				t.Errorf("e2 or the notification: %q", evs[1:])
			}
			// The client's answer: the envelope kept on a sealed service only.
			c.do(http.MethodPost, r.url+"/mcp/fs", `{"jsonrpc":"2.0","id":"e1","result":{"action":"accept","_meta":{"sh.kontra/sealed":`+env+`,"sh.kontra/price":"x"}}}`, nil)
			var answer string
			for _, rq := range up.Requests() {
				if strings.Contains(string(rq.Body), `"id":"e1"`) {
					answer = string(rq.Body)
				}
			}
			if strings.Contains(answer, `"sh.kontra/sealed"`) != sealed || strings.Contains(answer, "sh.kontra/price") || answer == "" {
				t.Errorf("sealed=%v, the client's answer upstream: %s", sealed, answer)
			}
		})
	}
}

// mcp_case_smuggling (§11.1, D172) at package level: each probe is 400 with -32600 and the upstream, the Admission
// fake and the meter see nothing; an upstream result {"isError": true, "ISERROR": false} is not relayed (502 with
// -32603) and its calls row has is_error true; in an event stream the stream ends before that event, a stream whose
// first event follows a byte order mark included (M3 review mcp-spec-1). An answer whose id is another spelling of
// the request's ("10" for 10) is the answer, as the TypeScript SDK takes it, and the stream ends before a second one
// (mcp-spec-2); an isError that is not a boolean is not relayed and is metered an error (mcp-spec-3).
func TestCaseSmuggling(t *testing.T) {
	e := newEnv(t)
	up, url := upstreamDouble(t)
	up.Tools = []json.RawMessage{mcptest.Tool("echo", ""), mcptest.Tool("add", ""), mcptest.Tool("smuggle", ""), mcptest.Tool("smuggle-sse", ""),
		mcptest.Tool("bom-sse", ""), mcptest.Tool("spelling", ""), mcptest.Tool("string-iserror", "")}
	up.OnCall = func(r mcptest.Request) mcptest.Reply {
		switch {
		case strings.Contains(string(r.Params), "bom-sse"):
			return mcptest.Reply{SSE: true, Raw: []byte(`{"jsonrpc":"2.0","id":10,"result":{"content":[],"isError":false}}`),
				Events: []string{"\xEF\xBB\xBFdata: {\"jsonrpc\":\"2.0\",\"id\":10,\"result\":{\"content\":[],\"isError\":true,\"ISERROR\":false," +
					"\"_meta\":{\"sh.kontra/approve-url\":\"https://evil.example/approve\"}}}\n\n"}}
		case strings.Contains(string(r.Params), "spelling"):
			return mcptest.Reply{SSE: true, Raw: []byte(`{"jsonrpc":"2.0","id":11,"result":{"content":[],"isError":false}}`),
				Events: []string{"data: {\"jsonrpc\":\"2.0\",\"id\":\"11\",\"result\":{\"content\":[],\"isError\":true}}\n\n"}}
		case strings.Contains(string(r.Params), "string-iserror"):
			return mcptest.Reply{Raw: []byte(`{"jsonrpc":"2.0","id":12,"result":{"content":[],"isError":"true"}}`)}
		}
		if strings.Contains(string(r.Params), "smuggle-sse") {
			return mcptest.Reply{SSE: true, Raw: []byte(`{"jsonrpc":"2.0","id":9,"result":{"content":[],"isError":true,"ISERROR":false}}`),
				Events: []string{"data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{\"progress\":1}}\n\n"}}
		}
		if strings.Contains(string(r.Params), "smuggle") {
			return mcptest.Reply{Raw: []byte(`{"jsonrpc":"2.0","id":8,"result":{"content":[],"isError":true,"ISERROR":false}}`)}
		}
		return mcptest.Reply{Result: json.RawMessage(`{"content":[]}`)}
	}
	e.service("fs", url, func(s *Service) { s.Tools = Policy{Deny: []string{"echo"}} })
	adm := &fakeAdmission{}
	r := e.replica("a", replicaOpts{admission: adm, ops: []hooks.Operation{hooks.OpCall}})
	key, _ := e.apiKey("buyer")
	c := newClient(t, key)
	c.initialize(r.url + "/mcp/fs")
	seen := len(up.Requests())
	for _, probe := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","METHOD":"ping","params":{"name":"add"}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","NAME":"add"}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"add","name":"echo"}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"add","_meta":{"progressToken":1,"PROGRESSTOKEN":2}}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"add","arguments":{"a":1,"A":2}}}`,
		`{"jsonrpc":"2.0","id":1,"METHOD":"tools/call","params":{"name":"add"}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"NAME":"echo"}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"add","tasK":{}}}`,
		`{"jsonrpc":"2.0","ID":1,"method":"tools/call","params":{"name":"add"}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"add","_META":{}}}`,
	} {
		rep := c.do(http.MethodPost, r.url+"/mcp/fs", probe, nil)
		if rep.status != 400 || rep.errCode(t) != -32600 {
			t.Errorf("%s: %d %s", probe, rep.status, rep.body)
		}
	}
	if len(up.Requests()) != seen || adm.n() != 0 || len(e.calls("fs")) != 0 {
		t.Errorf("a probe reached the upstream (%d), Admission (%d) or the meter (%d)", len(up.Requests())-seen, adm.n(), len(e.calls("fs")))
	}
	// The upstream's ambiguous isError.
	rep := c.call(r.url+"/mcp/fs", 8, "smuggle", "x")
	if rep.status != 502 || rep.errCode(t) != -32603 || strings.Contains(string(rep.body), "ISERROR") {
		t.Errorf("ambiguous upstream result: %d %s", rep.status, rep.body)
	}
	rep = c.call(r.url+"/mcp/fs", 9, "smuggle-sse", "x")
	if strings.Contains(string(rep.body), "ISERROR") || !strings.Contains(string(rep.body), "notifications/progress") {
		t.Errorf("event stream: the refused event was relayed, or the one before it was not: %s", rep.body)
	}
	// A byte order mark before the first event: the event is read, refused, and the stream ends before it.
	rep = c.call(r.url+"/mcp/fs", 10, "bom-sse", "x")
	if strings.Contains(string(rep.body), "ISERROR") || strings.Contains(string(rep.body), "sh.kontra/") || strings.Contains(string(rep.body), `"isError":false`) {
		t.Errorf("a BOM-prefixed event: %s", rep.body)
	}
	// "11" answers request 11 (the client takes it): metered as it says, and the stream ends before 11.
	rep = c.call(r.url+"/mcp/fs", 11, "spelling", "x")
	if !strings.Contains(string(rep.body), `"id":"11"`) || strings.Contains(string(rep.body), `"isError":false`) {
		t.Errorf("another spelling of the id: %s", rep.body)
	}
	// An isError that is not a boolean.
	rep = c.call(r.url+"/mcp/fs", 12, "string-iserror", "x")
	if rep.status != 502 || rep.errCode(t) != -32603 {
		t.Errorf("a string isError: %d %s", rep.status, rep.body)
	}
	rows := e.calls("fs")
	if len(rows) != 5 {
		t.Fatalf("calls rows %+v", rows)
	}
	for i, row := range rows {
		if row.isError == nil || !*row.isError {
			t.Errorf("calls row %d is_error %v, want true", i, row.isError)
		}
	}
}

// §11.1: an Origin not in mcp.allowed_origins is 403; an upstream 401 becomes 502 with -32603, and neither the
// upstream's WWW-Authenticate nor its cookies reach the client.
func TestOriginAndUpstreamRefusals(t *testing.T) {
	e := newEnv(t)
	up, url := upstreamDouble(t)
	e.service("fs", url, nil)
	r := e.replica("a", replicaOpts{origins: []string{"https://app.example"}})
	key, _ := e.apiKey("buyer")
	c := newClient(t, key)
	init := `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{}}}`
	if rep := c.do(http.MethodPost, r.url+"/mcp/fs", init, map[string]string{"Origin": "https://evil.example"}); rep.status != 403 {
		t.Errorf("a foreign Origin: %d", rep.status)
	}
	if up.Count("initialize") != 0 {
		t.Error("a refused Origin reached the upstream")
	}
	if rep := c.do(http.MethodPost, r.url+"/mcp/fs", init, map[string]string{"Origin": "https://app.example"}); rep.status != 200 {
		t.Errorf("an allowed Origin: %d %s", rep.status, rep.body)
	}
	c.initialize(r.url + "/mcp/fs")
	c.do(http.MethodPost, r.url+"/mcp/fs", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, nil) // the cache knows echo
	up.Lock()
	up.Status, up.WWWAuthenticate = http.StatusUnauthorized, `Bearer resource_metadata="https://evil.example/.well-known/oauth-protected-resource"`
	up.Unlock()
	rep := c.call(r.url+"/mcp/fs", 1, "echo", "x")
	if rep.status != 502 || rep.errCode(t) != -32603 || rep.header.Get("WWW-Authenticate") != "" || rep.header.Get("Set-Cookie") != "" ||
		!strings.Contains(string(rep.body), "upstream refused") {
		t.Errorf("upstream 401: %d %v %s", rep.status, rep.header, rep.body)
	}
	if rows := e.calls("fs"); len(rows) != 1 || !*rows[0].isError {
		t.Errorf("calls %+v", rows)
	}
	// A tools/list the gate drains itself is refused the same way.
	up.Lock()
	up.Tools = append(up.Tools, mcptest.Tool("other", ""))
	up.Unlock()
	r.h.deps.Tools.Invalidate(ToolsKey{TenantID: e.team, Service: "fs"})
	rep = c.do(http.MethodPost, r.url+"/mcp/fs", `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, nil)
	if rep.status != 502 || rep.header.Get("WWW-Authenticate") != "" || !strings.Contains(string(rep.body), "upstream refused") {
		t.Errorf("tools/list: %d %v %s", rep.status, rep.header, rep.body)
	}
}

// §13.1 with a hooks.Admission fake (C3): a quote on call is a deny with -32603 "priced tasks need tasks: durable"; a
// deny carries the refusal; a failing hook is 503 "admission unavailable"; a quote on session refuses the
// initialize. The upstream sees none of them.
func TestAdmission(t *testing.T) {
	e := newEnv(t)
	up, url := upstreamDouble(t)
	e.service("fs", url, nil)
	adm := &fakeAdmission{}
	r := e.replica("a", replicaOpts{admission: adm, ops: []hooks.Operation{hooks.OpSession, hooks.OpCall}})
	key, principal := e.apiKey("buyer")
	c := newClient(t, key)
	c.initialize(r.url + "/mcp/fs")
	if adm.n() != 1 || adm.asked[0].Operation != hooks.OpSession || adm.asked[0].Principal.ID != principal || adm.asked[0].Service.Name != "fs" {
		t.Errorf("session admission %+v", adm.asked)
	}
	quote := hooks.Decision{Outcome: hooks.Quote, Quote: &hooks.QuoteTerms{ApproveURL: "https://cp.example/approve", ExpiresAt: "2030-01-01T00:00:00Z"}}
	adm.set(quote, nil)
	rep := c.call(r.url+"/mcp/fs", 1, "echo", "x")
	if rep.errCode(t) != -32603 || !strings.Contains(string(rep.body), "priced tasks need tasks: durable") {
		t.Errorf("quote on call: %s", rep.body)
	}
	if last := adm.asked[len(adm.asked)-1]; last.Operation != hooks.OpCall || last.Tool != "echo" {
		t.Errorf("call admission %+v", last)
	}
	adm.set(hooks.Decision{Outcome: hooks.Deny, Refusal: &hooks.Refusal{Code: -32001, Message: "over budget"}}, nil)
	if rep := c.call(r.url+"/mcp/fs", 2, "echo", "x"); rep.errCode(t) != -32001 || !strings.Contains(string(rep.body), "over budget") {
		t.Errorf("deny: %s", rep.body)
	}
	adm.set(hooks.Decision{}, errors.New("hook down"))
	if rep := c.call(r.url+"/mcp/fs", 3, "echo", "x"); rep.status != 503 || !strings.Contains(string(rep.body), "admission unavailable") {
		t.Errorf("failing hook: %d %s", rep.status, rep.body)
	}
	adm.set(quote, nil)
	o := newClient(t, key)
	rep = o.do(http.MethodPost, r.url+"/mcp/fs", `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{}}}`, nil)
	if rep.header.Get("Mcp-Session-Id") != "" || !strings.Contains(string(rep.body), "priced tasks need tasks: durable") {
		t.Errorf("quote on session: %v %s", rep.header, rep.body)
	}
	if up.Count("tools/call") != 0 || up.Count("initialize") != 1 || len(e.calls("fs")) != 0 {
		t.Errorf("refused requests reached the upstream: %d calls, %d initializes", up.Count("tools/call"), up.Count("initialize"))
	}
}

// §11.1: an upstream that negotiates another version is -32602 naming the supported ones, gets a best-effort
// DELETE, and opens no session.
func TestVersionNegotiation(t *testing.T) {
	e := newEnv(t)
	up, url := upstreamDouble(t)
	up.Negotiate = "2024-11-05"
	e.service("fs", url, nil)
	r := e.replica("a", replicaOpts{})
	key, _ := e.apiKey("buyer")
	c := newClient(t, key)
	rep := c.do(http.MethodPost, r.url+"/mcp/fs", `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{}}}`, nil)
	if rep.errCode(t) != -32602 || rep.header.Get("Mcp-Session-Id") != "" || !strings.Contains(string(rep.body), "2025-11-25") {
		t.Errorf("%d %v %s", rep.status, rep.header, rep.body)
	}
	eventually(t, 5*time.Second, "the upstream session is deleted", func() bool { return len(up.Sessions()) == 0 })
	var n int
	_ = e.pool.QueryRow(context.Background(), `select count(*) from kontra.mcp_sessions`).Scan(&n)
	if n != 0 {
		t.Errorf("%d session rows", n)
	}
	// An initialize the upstream refuses, with a JSON-RPC error or a bare status, opens no session.
	up.Lock()
	up.InitError = &mcptest.RPCError{Code: -32603, Message: "not today"}
	up.Unlock()
	rep = c.do(http.MethodPost, r.url+"/mcp/fs", `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{}}}`, nil)
	if rep.errCode(t) != -32603 || rep.header.Get("Mcp-Session-Id") != "" {
		t.Errorf("a refused initialize: %v %s", rep.header, rep.body)
	}
	up.Lock()
	up.InitError, up.Status = nil, http.StatusMethodNotAllowed
	up.Unlock()
	rep = c.do(http.MethodPost, r.url+"/mcp/fs", `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{}}}`, nil)
	if rep.status != 405 || rep.header.Get("Mcp-Session-Id") != "" {
		t.Errorf("a 405 initialize: %d %v", rep.status, rep.header)
	}
	_ = e.pool.QueryRow(context.Background(), `select count(*) from kontra.mcp_sessions`).Scan(&n)
	if n != 0 {
		t.Errorf("%d session rows after refused initializes", n)
	}
	// 2025-06-18 is spoken.
	up.Lock()
	up.Status = 0
	up.Negotiate = "2025-06-18"
	up.Unlock()
	c.initialize(r.url + "/mcp/fs")
	if v := e.row(c.sid).version; v != "2025-06-18" {
		t.Errorf("version %s", v)
	}
}

// §11.5's table: durable advertises no tasks until gate-owned tasks are built (M3 WP-06), as off does, and removes
// every tool's execution; passthrough shows the upstream's, relays tasks/*, and writes a task-augmented call's row
// at its CreateTaskResult with is_error NULL; a 2025-06-18 session gets no tasks.
func TestTasksModes(t *testing.T) {
	e := newEnv(t)
	up, url := upstreamDouble(t)
	up.Capabilities = json.RawMessage(`{"tools":{},"tasks":{"list":{},"cancel":{},"requests":{"tools":{"call":{}}}}}`)
	up.OnCall = func(r mcptest.Request) mcptest.Reply {
		if strings.Contains(string(r.Params), `"task"`) {
			return mcptest.Reply{Result: json.RawMessage(`{"task":{"taskId":"up-1","status":"working","createdAt":"2026-01-01T00:00:00Z","ttl":1000}}`)}
		}
		return mcptest.Reply{Result: json.RawMessage(`{"content":[]}`)}
	}
	up.OnRequest = func(r mcptest.Request) mcptest.Reply {
		return mcptest.Reply{Result: json.RawMessage(`{"taskId":"up-1","status":"working"}`)}
	}
	e.service("dur", url, nil)
	e.service("pass", url, func(s *Service) { s.Tasks = config.TasksPassthrough })
	e.service("off", url, func(s *Service) { s.Tasks = config.TasksOff })
	r := e.replica("a", replicaOpts{})
	key, _ := e.apiKey("buyer")
	taskCall := `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"echo","arguments":{},"task":{"ttl":1000}}}`
	for _, tc := range []struct {
		svc   string
		tasks bool
	}{{"dur", false}, {"off", false}, {"pass", true}} {
		t.Run(tc.svc, func(t *testing.T) {
			c := newClient(t, key)
			rep := c.initialize(r.url + "/mcp/" + tc.svc)
			if strings.Contains(string(rep.body), `"tasks"`) != tc.tasks {
				t.Errorf("initialize: %s", rep.body)
			}
			rep = c.do(http.MethodPost, r.url+"/mcp/"+tc.svc, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, nil)
			if strings.Contains(string(rep.body), `"execution"`) != tc.tasks {
				t.Errorf("tools/list: %s", rep.body)
			}
			calls := up.Count("tools/call")
			rep = c.do(http.MethodPost, r.url+"/mcp/"+tc.svc, taskCall, nil)
			get := c.do(http.MethodPost, r.url+"/mcp/"+tc.svc, `{"jsonrpc":"2.0","id":6,"method":"tasks/get","params":{"taskId":"up-1"}}`, nil)
			if !tc.tasks {
				if rep.errCode(t) != -32601 || get.errCode(t) != -32601 || up.Count("tools/call") != calls {
					t.Errorf("a task-augmented call or tasks/get was not -32601: %s %s", rep.body, get.body)
				}
				return
			}
			if !strings.Contains(string(rep.body), `"up-1"`) || !strings.Contains(string(get.body), `"up-1"`) {
				t.Errorf("passthrough: %s %s", rep.body, get.body)
			}
		})
	}
	rows := e.calls("pass")
	if len(rows) != 1 || rows[0].isError != nil || rows[0].task != nil {
		t.Errorf("passthrough calls rows %+v", rows)
	}
	// 2025-06-18 on passthrough: no tasks.
	up.Lock()
	up.Negotiate = "2025-06-18"
	up.Unlock()
	c := newClient(t, key)
	if rep := c.initialize(r.url + "/mcp/pass"); strings.Contains(string(rep.body), `"tasks"`) {
		t.Errorf("2025-06-18: %s", rep.body)
	}
}

// §11.2: a session idle past session_idle_s is closed idle and its upstream session gets a best-effort DELETE;
// last_seen_at is written at most once a minute.
func TestIdleSweep(t *testing.T) {
	e := newEnv(t)
	up, url := upstreamDouble(t)
	e.service("fs", url, nil)
	r := e.replica("a", replicaOpts{})
	key, _ := e.apiKey("buyer")
	c := newClient(t, key)
	// The initialize result states the service's session_idle_s (D224): a hiring node keeps a sealed session's key
	// that long.
	if rep := c.initialize(r.url + "/mcp/fs"); !strings.Contains(string(rep.body), `"_meta":{"sh.kontra/session_idle_s":3600}`) {
		t.Errorf("the initialize result does not state session_idle_s: %s", rep.body)
	}
	h := HashID(c.sid)
	var seen1, seen2 time.Time
	_ = e.pool.QueryRow(context.Background(), `select last_seen_at from kontra.mcp_sessions where client_sid_hash = $1`, h[:]).Scan(&seen1)
	c.call(r.url+"/mcp/fs", 1, "echo", "x")
	_ = e.pool.QueryRow(context.Background(), `select last_seen_at from kontra.mcp_sessions where client_sid_hash = $1`, h[:]).Scan(&seen2)
	if !seen1.Equal(seen2) {
		t.Error("last_seen_at written twice within a minute")
	}
	if n, err := r.h.SweepIdle(context.Background()); err != nil || n != 0 {
		t.Fatalf("a fresh session swept: %d %v", n, err)
	}
	if _, err := e.pool.Exec(context.Background(), `update kontra.mcp_sessions set last_seen_at = now() - interval '2 hours'`); err != nil {
		t.Fatal(err)
	}
	c.call(r.url+"/mcp/fs", 2, "echo", "x") // touched: no longer idle
	if n, _ := r.h.SweepIdle(context.Background()); n != 0 {
		t.Fatal("a session touched just now was swept")
	}
	if _, err := e.pool.Exec(context.Background(), `update kontra.mcp_sessions set last_seen_at = now() - interval '2 hours'`); err != nil {
		t.Fatal(err)
	}
	if n, err := r.h.SweepIdle(context.Background()); err != nil || n != 1 {
		t.Fatalf("swept %d %v", n, err)
	}
	if row := e.row(c.sid); !row.closed || *row.reason != string(CloseIdle) {
		t.Errorf("row %+v", row)
	}
	eventually(t, 5*time.Second, "the upstream DELETE", func() bool { return len(up.Sessions()) == 0 })
	if rep := c.call(r.url+"/mcp/fs", 3, "echo", "x"); rep.status != 404 {
		t.Errorf("an idle session: %d", rep.status)
	}
}

// §11.3: a forwarded notifications/tools/list_changed drops the cached list; the next tools/list drains again.
func TestListChangedInvalidates(t *testing.T) {
	e := newEnv(t)
	up, url := upstreamDouble(t)
	e.service("fs", url, nil)
	r := e.replica("a", replicaOpts{})
	key, _ := e.apiKey("buyer")
	c := newClient(t, key)
	c.initialize(r.url + "/mcp/fs")
	list := func() []string {
		names, _, _ := toolNames(t, c.do(http.MethodPost, r.url+"/mcp/fs", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, nil))
		return names
	}
	if got := list(); len(got) != 1 {
		t.Fatalf("%v", got)
	}
	up.Lock()
	up.Tools = append(up.Tools, mcptest.Tool("new", ""))
	up.Unlock()
	if got := list(); len(got) != 1 {
		t.Fatalf("the cache did not answer: %v", got)
	}
	resp := openGET(t, c, r.url+"/mcp/fs", up, nil)
	defer resp.Body.Close()
	_ = up.Push(up.Sessions()[0], []byte(`{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}`))
	if evs := readEvents(t, resp, 1, 5*time.Second); len(evs) != 1 || !strings.Contains(evs[0], "list_changed") {
		t.Fatalf("the notification was not forwarded: %q", evs)
	}
	if got := list(); len(got) != 2 {
		t.Errorf("after list_changed: %v", got)
	}
}

var _ = httptest.NewServer
