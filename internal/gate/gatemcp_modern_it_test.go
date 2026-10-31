package gate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"go.temporal.io/api/enums/v1"

	"kontra.sh/kontra/internal/gatemcp"
	"kontra.sh/kontra/internal/lifecycle"
	"kontra.sh/kontra/internal/temporaltest"
)

// modernClient is an MCP 2026-07-28 client of the gate's own /mcp on one replica (spec §12.5): no session; every
// request carries MCP-Protocol-Version, Mcp-Method, Mcp-Name when the method has one, and the version and the
// client's capabilities in params._meta.
type modernClient struct {
	t    *testing.T
	url  string
	key  string
	caps string // the per-request clientCapabilities, JSON
	n    int
}

const (
	capsTasks     = `{"extensions":{"io.modelcontextprotocol/tasks":{}}}`
	capsTasksForm = `{"extensions":{"io.modelcontextprotocol/tasks":{}},"elicitation":{"form":{},"url":{}}}`
)

func newModernClient(t *testing.T, base, key, caps string) *modernClient {
	return &modernClient{t: t, url: base + gatemcp.Path, key: key, caps: caps}
}

// on is the same client against another replica.
func (c *modernClient) on(base string) *modernClient {
	cp := *c
	cp.url = base + gatemcp.Path
	return &cp
}

// with is the same client declaring other capabilities.
func (c *modernClient) with(caps string) *modernClient {
	cp := *c
	cp.caps = caps
	return &cp
}

// body is a request's JSON: params (a JSON object's members, without braces) and the _meta 2026-07-28 requires.
func (c *modernClient) body(method, params string) string {
	c.n++
	meta := `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":` + c.caps + `}`
	if params != "" {
		params += ","
	}
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q,"params":{%s%s}}`, c.n, method, params, meta)
}

// post sends body with the modern headers (Mcp-Name = name when not empty; hdr overrides, "" removes) on ctx and
// returns the status and the answer's last message.
func (c *modernClient) post(ctx context.Context, method, name, body string, hdr map[string]string) (int, rpcAnswer, error) {
	c.t.Helper()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.url, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2026-07-28")
	req.Header.Set("Mcp-Method", method)
	if name != "" {
		req.Header.Set("Mcp-Name", name)
	}
	for k, v := range hdr {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, rpcAnswer{}, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, rpcAnswer{}, err
	}
	if resp.Header.Get("Mcp-Session-Id") != "" {
		c.t.Errorf("a modern answer named a session: %s", resp.Header.Get("Mcp-Session-Id"))
	}
	msgs := []json.RawMessage{b}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		msgs = sseData(b)
	}
	var a rpcAnswer
	if len(msgs) > 0 && len(strings.TrimSpace(string(msgs[len(msgs)-1]))) > 0 {
		if err := json.Unmarshal(msgs[len(msgs)-1], &a); err != nil {
			c.t.Fatalf("%s: %s: %v", method, msgs[len(msgs)-1], err)
		}
		a.raw, a.all = msgs[len(msgs)-1], msgs
	}
	return resp.StatusCode, a, nil
}

// rpc sends method with params (members, without braces); name is its Mcp-Name.
func (c *modernClient) rpc(method, name, params string) (int, rpcAnswer) {
	c.t.Helper()
	status, a, err := c.post(context.Background(), method, name, c.body(method, params), nil)
	if err != nil {
		c.t.Fatalf("%s: %v", method, err)
	}
	return status, a
}

// call calls a tool.
func (c *modernClient) call(tool, args string) (int, rpcAnswer) {
	c.t.Helper()
	return c.rpc("tools/call", tool, `"name":"`+tool+`","arguments":`+args)
}

// modernTask is a CreateTaskResult or a tasks/get result as the tests read it.
type modernTask struct {
	ResultType     string                     `json:"resultType"`
	TaskID         string                     `json:"taskId"`
	Status         string                     `json:"status"`
	StatusMessage  string                     `json:"statusMessage"`
	PollIntervalMs int                        `json:"pollIntervalMs"`
	TTLMs          *int64                     `json:"ttlMs"`
	InputRequests  map[string]json.RawMessage `json:"inputRequests"`
	Result         json.RawMessage            `json:"result"`
	Meta           map[string]any             `json:"_meta"`
}

func (a rpcAnswer) task(t *testing.T) modernTask {
	t.Helper()
	if a.Error != nil || a.Result == nil {
		t.Fatalf("not a result: %s", a.raw)
	}
	var m modernTask
	if err := json.Unmarshal(a.Result, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// resultType is a result's resultType ("" when it has none).
func (a rpcAnswer) resultType() string {
	var r struct {
		ResultType string `json:"resultType"`
	}
	_ = json.Unmarshal(a.Result, &r)
	return r.ResultType
}

func (e *gateEnv) modernServices(kinds, paid *hireSeller, hookURL string) string {
	return e.hireServices(kinds, `  paid:
    kind: a2a
    backend: { a2a: { url: "`+paid.srv.URL+`/base", allow_private: true } }
    allow: ["org:default"]
    task: { timeout: 10m, retries: 1 }
hooks:
  admission: { url: "`+hookURL+`/admit", secret_env: KONTRA_TEST_ADMISSION_SECRET, timeout: 3s, operations: [task] }
`)
}

// The gate's own /mcp in the 2026-07-28 era, through two whole replicas on one database (spec §12.5, §11.12; D24,
// D159, D176, D269, D274; M4 WP-05):
//
//   - server/discover lists 2026-07-28, 2025-11-25 and 2025-06-18 and the tasks extension; the modern tools/list has
//     no execution; both are byte-identical from either replica;
//   - a hire declaring the tasks extension to a fast seller is answered in line (resultType complete); to a slow one
//     with a CreateTaskResult (resultType task, pollIntervalMs 1000), whose tasks/get is the same from either replica,
//     and whose tasks/cancel is acknowledged at once and cancels it within 10 s (the seller receives CancelTask);
//   - a quoted hire is working with sh.kontra/approve-url and never -32042, even for a client that declares URL
//     elicitation; tasks/update with the quoted event's gate key and with any other key is acknowledged, forwards
//     nothing and leaves it quoted (D38, D45); a plain retry answers the URL;
//   - the booking seller's question is input_required with exactly one inputRequests entry, k.<task>.<seq> of the event
//     that asked; a wrong key is ignored; tasks/update answers it once and a second answer is ignored; the task
//     completes with the first answer;
//   - tasks/result and tasks/list are 404 with -32601; a _meta protocolVersion that differs from the header is 400 with
//     -32020; tasks/get without the extension is 400 with -32021; a legacy request without a session is 400 with
//     -32000 (rule 3's body); a hire without the extension is a plain call;
//   - no modern request writes an mcp_sessions row (no virtual session), and /mcp writes no calls rows.
func TestGateMCPModern(t *testing.T) {
	e := newGateEnv(t)
	kinds, paid := newHireSeller(t), newHireSeller(t)
	hook := newHookServer(t)
	hook.set(func(h *hookServer) { h.raw = `{"outcome":"allow"}` })
	t.Setenv("KONTRA_TEST_ADMISSION_SECRET", "Hk4nW8qZ2xV6bM1tR9yL3pD7sF0gJ5cA")
	services := e.modernServices(kinds, paid, hook.srv.URL)
	timing := fastTiming
	timing.PlainWait = 4 * time.Second
	a := e.start("a", e.replicaConfig(freePort(t), time.Minute, services), timing)
	b := e.start("b", e.replicaConfig(freePort(t), time.Minute, services), timing)
	key := e.mintKey("buyer")
	ca := newModernClient(t, a.url, key, capsTasks)
	cb := ca.on(b.url)

	// server/discover and tools/list, the same from either replica.
	for _, method := range []string{"server/discover", "tools/list"} {
		sa, ansA := ca.rpc(method, "", "")
		sb, ansB := cb.rpc(method, "", "")
		if sa != http.StatusOK || sb != http.StatusOK || ansA.Error != nil || string(ansA.Result) != string(ansB.Result) {
			t.Fatalf("%s: %d %s / %d %s", method, sa, ansA.raw, sb, ansB.raw)
		}
	}
	_, ans := ca.rpc("server/discover", "", "")
	var disc struct {
		SupportedVersions []string        `json:"supportedVersions"`
		Capabilities      json.RawMessage `json:"capabilities"`
		CacheScope        string          `json:"cacheScope"`
	}
	_ = json.Unmarshal(ans.Result, &disc)
	if !reflect.DeepEqual(disc.SupportedVersions, []string{"2026-07-28", "2025-11-25", "2025-06-18"}) || disc.CacheScope != "private" ||
		string(disc.Capabilities) != `{"extensions":{"io.modelcontextprotocol/tasks":{}},"tools":{}}` {
		t.Errorf("server/discover: %s", ans.raw)
	}
	_, ans = cb.rpc("tools/list", "", "")
	if strings.Contains(string(ans.Result), "execution") || !strings.Contains(string(ans.Result), `"name":"hire"`) ||
		!strings.Contains(string(ans.Result), `"cacheScope":"private"`) || ans.resultType() != "complete" {
		t.Errorf("the modern tools/list: %s", ans.raw)
	}

	// A fast hire with the extension: in line.
	_, ans = ca.call("hire", `{"service":"kinds","message":"hi"}`)
	if r := ans.tool(t); r.IsError || r.text() != "Hello World" || r.Structured.State != "TASK_STATE_COMPLETED" || ans.resultType() != "complete" {
		t.Fatalf("a fast modern hire: %s", ans.raw)
	}

	// A slow one: CreateTaskResult; tasks/get the same on both replicas; tasks/cancel at once, canceled within 10 s.
	began := time.Now()
	_, ans = cb.call("hire", `{"service":"kinds","message":"slow"}`)
	slow := ans.task(t)
	if took := time.Since(began); slow.ResultType != "task" || slow.Status != "working" || slow.PollIntervalMs != 1000 || slow.TaskID == "" ||
		slow.TTLMs == nil || took < 2*time.Second || took > 8*time.Second {
		t.Fatalf("a slow modern hire after %v: %s", took, ans.raw)
	}
	_, getA := ca.rpc("tasks/get", slow.TaskID, `"taskId":"`+slow.TaskID+`"`)
	_, getB := cb.rpc("tasks/get", slow.TaskID, `"taskId":"`+slow.TaskID+`"`)
	if g := getA.task(t); g.ResultType != "complete" || g.Status != "working" || string(getA.Result) != string(getB.Result) {
		t.Errorf("tasks/get of the slow task: %s / %s", getA.raw, getB.raw)
	}
	began = time.Now()
	_, ans = ca.rpc("tasks/cancel", slow.TaskID, `"taskId":"`+slow.TaskID+`"`)
	if ans.Error != nil || string(ans.Result) != `{"resultType":"complete"}` || time.Since(began) > 5*time.Second {
		t.Fatalf("tasks/cancel after %v: %s", time.Since(began), ans.raw)
	}
	eventually(t, 10*time.Second, "the cancelled task is canceled", func() bool {
		return scalar[string](t, e.pool, `select state from kontra.tasks where id = $1`, slow.TaskID) == "canceled"
	})
	eventually(t, 10*time.Second, "the seller received CancelTask", func() bool { return kinds.count("CancelTask") >= 1 })
	_, ans = cb.rpc("tasks/get", slow.TaskID, `"taskId":"`+slow.TaskID+`"`)
	if g := ans.task(t); g.Status != "cancelled" || g.Meta["sh.kontra/canceled"] != "client" {
		t.Errorf("tasks/get of the cancelled task: %s", ans.raw)
	}

	// A quoted hire: working with the approve URL, never -32042 (even declaring URL elicitation).
	hook.set(func(h *hookServer) { h.raw = "" })
	cq := ca.with(capsTasksForm)
	_, ans = cq.call("hire", `{"service":"paid","message":"hi"}`)
	if ans.Error != nil {
		t.Fatalf("a quoted modern hire answered an error: %s", ans.raw)
	}
	q := ans.task(t)
	approve := "https://app.kontra.sh/#/t/" + q.TaskID
	if q.ResultType != "task" || q.Status != "working" || q.StatusMessage != approve || q.Meta["sh.kontra/approve-url"] != approve {
		t.Fatalf("a quoted modern hire: %s", ans.raw)
	}
	hook.set(func(h *hookServer) { h.raw = `{"outcome":"allow"}` })
	quoted := func(what string) {
		t.Helper()
		if st := scalar[string](t, e.pool, `select state from kontra.tasks where id = $1`, q.TaskID); st != "quoted" {
			t.Fatalf("after %s the quoted task is %s", what, st)
		}
		if n := paid.count("SendMessage") + paid.count("SendStreamingMessage"); n != 0 {
			t.Fatalf("after %s the seller received %d messages", what, n)
		}
	}
	quoted("the hire")
	qseq := scalar[int64](t, e.pool, `select max(seq) from kontra.task_events where task_id = $1`, q.TaskID)
	for _, responses := range []string{
		fmt.Sprintf(`{"k.%s.%d":{"action":"accept","content":{"reply":"I accept"}}}`, q.TaskID, qseq),
		fmt.Sprintf(`{"k.%s.%d":{"action":"accept"}}`, q.TaskID, qseq),
		fmt.Sprintf(`{"k.%s.1":{"action":"accept","content":{"reply":"yes"}},"approve":{"action":"accept"}}`, q.TaskID),
	} {
		for _, c := range []*modernClient{cq, cq.on(b.url)} {
			status, ans := c.rpc("tasks/update", q.TaskID, `"taskId":"`+q.TaskID+`","inputResponses":`+responses)
			if status != http.StatusOK || ans.Error != nil || string(ans.Result) != `{"resultType":"complete"}` {
				t.Errorf("tasks/update %s on the quoted task: %d %s", responses, status, ans.raw)
			}
		}
	}
	time.Sleep(time.Second)
	quoted("tasks/update on it")
	_, ans = cq.rpc("tasks/get", q.TaskID, `"taskId":"`+q.TaskID+`"`)
	if g := ans.task(t); g.Status != "working" || g.StatusMessage != approve || len(g.InputRequests) != 0 {
		t.Errorf("tasks/get of the quoted task: %s", ans.raw)
	}
	// A retry without the extension (a plain call): the same task's URL, at once, as text.
	_, ans = ca.with(`{}`).call("hire", `{"service":"paid","message":"hi"}`)
	if r := ans.tool(t); r.IsError || r.Structured.TaskID != q.TaskID || r.Meta["sh.kontra/approve-url"] != approve ||
		!strings.Contains(r.text(), approve) || ans.resultType() != "complete" {
		t.Errorf("a plain retry of the quoted hire: %s", ans.raw)
	}
	quoted("the retry")
	if n := hook.count(); n != 3 { // the fast, the slow and the quoted hire; the retry asks nothing (§12.3)
		t.Errorf("Admission asked %d times, want 3", n)
	}

	// The booking seller's question, to a client declaring form elicitation.
	cf := ca.with(capsTasksForm)
	_, ans = cf.call("hire", `{"service":"kinds","message":"ask"}`)
	ask := ans.task(t)
	eventually(t, 20*time.Second, "the question is asked", func() bool {
		return scalar[string](t, e.pool, `select state from kontra.tasks where id = $1`, ask.TaskID) == "input-required"
	})
	seq := scalar[int64](t, e.pool, `select max(seq) from kontra.task_events where task_id = $1 and kind in ('task', 'status')`, ask.TaskID)
	gateKey := fmt.Sprintf("k.%s.%d", ask.TaskID, seq)
	_, getA = cf.rpc("tasks/get", ask.TaskID, `"taskId":"`+ask.TaskID+`"`)
	_, getB = cf.on(b.url).rpc("tasks/get", ask.TaskID, `"taskId":"`+ask.TaskID+`"`)
	g := getA.task(t)
	if g.Status != "input_required" || g.StatusMessage != "Which date?" || len(g.InputRequests) != 1 || string(getA.Result) != string(getB.Result) {
		t.Fatalf("tasks/get of the question: %s / %s", getA.raw, getB.raw)
	}
	if ir := string(g.InputRequests[gateKey]); !strings.Contains(ir, `"method":"elicitation/create"`) || !strings.Contains(ir, `"mode":"form"`) ||
		!strings.Contains(ir, `"message":"Which date?"`) {
		t.Fatalf("the input request under %s: %s", gateKey, getA.raw)
	}
	// Without form elicitation the same question is working with the reply-tool text.
	_, ans = ca.rpc("tasks/get", ask.TaskID, `"taskId":"`+ask.TaskID+`"`)
	if g := ans.task(t); g.Status != "working" || len(g.InputRequests) != 0 || g.Meta["sh.kontra/input-required"] != true {
		t.Errorf("tasks/get of the question without form elicitation: %s", ans.raw)
	}
	// A wrong key: ignored.
	_, ans = cf.rpc("tasks/update", ask.TaskID, fmt.Sprintf(`"taskId":"%s","inputResponses":{"k.%s.%d":{"action":"accept","content":{"reply":"monday"}}}`,
		ask.TaskID, ask.TaskID, seq-1))
	if string(ans.Result) != `{"resultType":"complete"}` {
		t.Errorf("tasks/update with an earlier key: %s", ans.raw)
	}
	time.Sleep(500 * time.Millisecond)
	if st := scalar[string](t, e.pool, `select state from kontra.tasks where id = $1`, ask.TaskID); st != "input-required" {
		t.Fatalf("after an answer to an earlier key the task is %s", st)
	}
	// The answer, then a second one: the first is the follow-up, once.
	for i, reply := range []string{"friday", "saturday"} {
		c := cf
		if i == 1 {
			c = cf.on(b.url)
		}
		_, ans = c.rpc("tasks/update", ask.TaskID, `"taskId":"`+ask.TaskID+`","inputResponses":{"`+gateKey+`":{"action":"accept","content":{"reply":"`+reply+`"}}}`)
		if ans.Error != nil || string(ans.Result) != `{"resultType":"complete"}` {
			t.Fatalf("tasks/update %s: %s", reply, ans.raw)
		}
	}
	eventually(t, 20*time.Second, "the question's task completes", func() bool {
		return scalar[string](t, e.pool, `select state from kontra.tasks where id = $1`, ask.TaskID) == "completed"
	})
	_, ans = cf.on(b.url).rpc("tasks/get", ask.TaskID, `"taskId":"`+ask.TaskID+`"`)
	if g := ans.task(t); g.Status != "completed" || !strings.Contains(string(g.Result), "booked for friday") ||
		!strings.Contains(string(g.Result), `"resultType":"complete"`) {
		t.Errorf("tasks/get of the answered task: %s", ans.raw)
	}
	time.Sleep(time.Second)
	if turns := strings.Join(kinds.turnTexts(), ","); strings.Count(turns, "friday") != 1 || strings.Contains(turns, "saturday") || strings.Contains(turns, "monday") {
		t.Errorf("the seller's turns %s: friday once, nothing else", turns)
	}

	// What the extension removes, header checks, and the legacy rule 3.
	for _, method := range []string{"tasks/result", "tasks/list"} {
		name := ""
		params := ""
		if method == "tasks/result" {
			name, params = ask.TaskID, `"taskId":"`+ask.TaskID+`"`
		}
		if status, ans := ca.rpc(method, name, params); status != http.StatusNotFound || ans.Error == nil || ans.Error.Code != -32601 {
			t.Errorf("%s: %d %s", method, status, ans.raw)
		}
	}
	status, mm, _ := ca.post(context.Background(), "tools/list", "", `{"jsonrpc":"2.0","id":"m","method":"tools/list","params":{"_meta":{`+
		`"io.modelcontextprotocol/protocolVersion":"2025-11-25","io.modelcontextprotocol/clientCapabilities":{}}}}`, nil)
	if status != http.StatusBadRequest || mm.Error == nil || mm.Error.Code != -32020 {
		t.Errorf("a mismatched protocolVersion: %d %s", status, mm.raw)
	}
	if status, ans := ca.with(`{}`).rpc("tasks/get", ask.TaskID, `"taskId":"`+ask.TaskID+`"`); status != http.StatusBadRequest ||
		ans.Error == nil || ans.Error.Code != -32021 {
		t.Errorf("tasks/get without the extension: %d %s", status, ans.raw)
	}
	for _, v := range []string{"", "2025-11-25"} {
		req, _ := http.NewRequest(http.MethodPost, a.url+gatemcp.Path, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if v != "" {
			req.Header.Set("MCP-Protocol-Version", v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest ||
			string(body) != `{"jsonrpc":"2.0","id":null,"error":{"code":-32000,"message":"Bad Request: No valid session ID provided"}}` {
			t.Errorf("a legacy request (version %q) without a session: %d %s", v, resp.StatusCode, body)
		}
	}

	// A hire without the extension is a plain call: in line, and a slow one after the plain wait.
	plain := ca.with(`{}`)
	_, ans = plain.call("hire", `{"service":"kinds","message":"hi"}`)
	if r := ans.tool(t); r.IsError || r.text() != "Hello World" || ans.resultType() != "complete" || strings.Contains(string(ans.Result), `"resultType":"task"`) {
		t.Errorf("a plain modern hire: %s", ans.raw)
	}
	began = time.Now()
	_, ans = plain.call("hire", `{"service":"kinds","message":"slow"}`)
	slowPlain := ans.tool(t)
	if time.Since(began) < 3*time.Second || slowPlain.Structured.State != "TASK_STATE_WORKING" || !strings.Contains(slowPlain.text(), a.url+"/agents/kinds") {
		t.Errorf("a slow plain modern hire after %v: %s", time.Since(began), ans.raw)
	}
	// Its client knows its id from the answer: tasks/cancel (declaring the extension) ends it.
	_, ans = ca.rpc("tasks/cancel", slowPlain.Structured.TaskID, `"taskId":"`+slowPlain.Structured.TaskID+`"`)
	if string(ans.Result) != `{"resultType":"complete"}` {
		t.Errorf("tasks/cancel of the plain hire's task: %s", ans.raw)
	}
	eventually(t, 10*time.Second, "the plain hire's task is canceled", func() bool {
		return scalar[string](t, e.pool, `select state from kontra.tasks where id = $1`, slowPlain.Structured.TaskID) == "canceled"
	})
	// A cancel of an ended task is acknowledged and changes nothing.
	_, ans = cb.rpc("tasks/cancel", ask.TaskID, `"taskId":"`+ask.TaskID+`"`)
	if string(ans.Result) != `{"resultType":"complete"}` || scalar[string](t, e.pool, `select state from kontra.tasks where id = $1`, ask.TaskID) != "completed" {
		t.Errorf("tasks/cancel of a completed task: %s", ans.raw)
	}

	// No virtual session, no /mcp session, no calls rows; every task hire created is marked hired.
	if n := scalar[int64](t, e.pool, `select count(*) from kontra.mcp_sessions`); n != 0 {
		t.Errorf("modern /mcp requests wrote %d mcp_sessions rows", n)
	}
	if n := scalar[int64](t, e.pool, `select count(*) from kontra.calls`); n != 0 {
		t.Errorf("/mcp wrote %d calls rows", n)
	}
	if n := scalar[int64](t, e.pool, `select count(*) from kontra.tasks where not hired`); n != 0 {
		t.Errorf("%d tasks hire created are not marked hired", n)
	}
}

// A modern client that closes its response stream before its hire is answered has cancelled the request (2026-07-28
// Streamable HTTP, "Cancellation"; §11.12 step 4, D178): it gets no answer, and the task it created ends canceled
// within 10 s, the seller receiving CancelTask, whether it declared the tasks extension (the 2 s inline wait) or not
// (the plain wait). A quoted task is left quoted (D295: it runs nothing until a person approves it), and the client's
// re-issued hire answers it with its approve URL.
func TestGateMCPModernClientLeaves(t *testing.T) {
	e := newGateEnv(t)
	kinds, paid := newHireSeller(t), newHireSeller(t)
	hook := newHookServer(t)
	hook.set(func(h *hookServer) { h.raw = `{"outcome":"allow"}` })
	t.Setenv("KONTRA_TEST_ADMISSION_SECRET", "Hk4nW8qZ2xV6bM1tR9yL3pD7sF0gJ5cA")
	// The hook answers after delay: a client can leave while Admission decides.
	var delay atomic.Int64
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(time.Duration(delay.Load()))
		hook.srv.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(slow.Close)
	services := e.modernServices(kinds, paid, slow.URL)
	timing := fastTiming
	timing.PlainWait = 10 * time.Second
	a := e.start("a", e.replicaConfig(freePort(t), time.Minute, services), timing)
	key := e.mintKey("buyer")

	leave := func(c *modernClient, args string, after time.Duration) string {
		t.Helper()
		before := scalar[int64](t, e.pool, `select count(*) from kontra.tasks`)
		ctx, cancel := context.WithTimeout(context.Background(), after)
		defer cancel()
		_, ans, err := c.post(ctx, "tools/call", "hire", c.body("tools/call", `"name":"hire","arguments":`+args), nil)
		if err == nil {
			t.Fatalf("the client that left got an answer: %s", ans.raw)
		}
		eventually(t, 10*time.Second, "the hire's task exists", func() bool {
			return scalar[int64](t, e.pool, `select count(*) from kontra.tasks`) == before+1
		})
		return scalar[string](t, e.pool, `select id::text from kontra.tasks order by created_at desc limit 1`)
	}

	for i, c := range []*modernClient{newModernClient(t, a.url, key, capsTasks), newModernClient(t, a.url, key, `{}`)} {
		id := leave(c, `{"service":"kinds","message":"slow","contextId":"leave-`+fmt.Sprint(i)+`"}`, 700*time.Millisecond)
		began := time.Now()
		eventually(t, 10*time.Second, "the task of the client that left is canceled", func() bool {
			return scalar[string](t, e.pool, `select state from kontra.tasks where id = $1`, id) == "canceled"
		})
		t.Logf("canceled %v after the client left", time.Since(began))
		eventually(t, 10*time.Second, "the seller received CancelTask", func() bool { return kinds.count("CancelTask") >= i+1 })
	}

	// A quoted one, whose client left while Admission decided, is left quoted, and the re-issued hire answers it.
	hook.set(func(h *hookServer) { h.raw = "" })
	delay.Store(int64(time.Second))
	c := newModernClient(t, a.url, key, capsTasks)
	args := `{"service":"paid","message":"hi","contextId":"quoted"}`
	id := leave(c, args, 300*time.Millisecond)
	eventually(t, 10*time.Second, "the task is quoted", func() bool {
		return scalar[string](t, e.pool, `select state from kontra.tasks where id = $1`, id) == "quoted"
	})
	time.Sleep(3 * time.Second)
	delay.Store(0)
	if st := scalar[string](t, e.pool, `select state from kontra.tasks where id = $1`, id); st != "quoted" {
		t.Fatalf("the quoted task of a client that left is %s", st)
	}
	_, ans := c.call("hire", args)
	if q := ans.task(t); q.TaskID != id || q.Status != "working" || q.Meta["sh.kontra/approve-url"] != "https://app.kontra.sh/#/t/"+id {
		t.Errorf("the re-issued hire: %s", ans.raw)
	}
	if hook.count() != 3 {
		t.Errorf("Admission asked %d times, want 3 (the re-issue is a retry)", hook.count())
	}
}

// A drain that cuts a modern hire's wait (M4 WP-05 review; §12.5, D178): the replica's listener stops after its
// workers drained, closing the stream of a modern plain hire whose task still runs; its client has left, so the task is
// sent kontra.cancel, detached, and the drain awaits that cancel (gatemcp.HireTasks.Wait) before the replica closes its
// Temporal client: when Run returns, the workflow has accepted it (another replica's kontra-lifecycle worker did; this
// one's have stopped), so it is in the task's history and nothing of it hangs on the closed client. The task ends
// canceled and the seller receives CancelTask.
func TestGateMCPModernDrainCancelsTheTaskOfAHireItCut(t *testing.T) {
	e := newGateEnv(t)
	kinds := newHireSeller(t)
	services := e.hireServices(kinds, "")
	timing := fastTiming
	timing.PlainWait = time.Minute
	a := e.start("a", e.replicaConfig(freePort(t), 2*time.Second, services), timing)
	key := e.mintKey("buyer")
	c := newModernClient(t, a.url, key, `{}`)
	answered := make(chan string, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_, ans, err := c.post(ctx, "tools/call", "hire", c.body("tools/call", `"name":"hire","arguments":{"service":"kinds","message":"slow"}`), nil)
		if err != nil {
			answered <- ""
			return
		}
		answered <- string(ans.raw)
	}()
	var id string
	eventually(t, 20*time.Second, "the hire's task runs on a", func() bool {
		if scalar[int64](t, e.pool, `select count(*) from kontra.tasks where hired`) != 1 {
			return false
		}
		id = scalar[string](t, e.pool, `select id::text from kontra.tasks where hired`)
		return scalar[string](t, e.pool, `select state from kontra.tasks where id = $1`, id) == "working" &&
			attemptReplica(t, e.pool, a2a.TaskID(id)) == a.r.ID()
	})
	e.start("b", e.replicaConfig(freePort(t), time.Minute, services), timing) // its workers take the cancel
	a.drain()
	if err := a.wait(t, 60*time.Second); err != nil {
		t.Fatalf("Run of the drained replica: %v", err)
	}
	if !cancelAccepted(t, id) {
		t.Fatal("Run of the drained replica returned before the workflow accepted the cancel it sent")
	}
	select {
	case raw := <-answered:
		if raw != "" {
			t.Fatalf("the client whose stream the drain closed got an answer: %s", raw)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the hire's stream is still open after the drain")
	}
	eventually(t, 15*time.Second, "the task of the hire the drain cut is canceled", func() bool {
		return scalar[string](t, e.pool, `select state from kontra.tasks where id = $1`, id) == "canceled"
	})
	eventually(t, 15*time.Second, "the seller received CancelTask", func() bool { return kinds.count("CancelTask") >= 1 })
}

// cancelAccepted reports whether the task's workflow history holds an accepted kontra.cancel Update.
func cancelAccepted(t *testing.T, taskID string) bool {
	t.Helper()
	it := temporaltest.Client(t).GetWorkflowHistory(context.Background(), lifecycle.WorkflowID(a2a.TaskID(taskID)), "", false,
		enums.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	for it.HasNext() {
		ev, err := it.Next()
		if err != nil {
			t.Fatal(err)
		}
		if ev.GetEventType() == enums.EVENT_TYPE_WORKFLOW_EXECUTION_UPDATE_ACCEPTED &&
			ev.GetWorkflowExecutionUpdateAcceptedEventAttributes().GetAcceptedRequest().GetInput().GetName() == lifecycle.UpdateCancel {
			return true
		}
	}
	return false
}
