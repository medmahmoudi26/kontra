package gate

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"kontra.sh/kontra/internal/gatemcp"
	"kontra.sh/kontra/internal/mcprelay/mcptest"
)

// hireSeller is an a2a-go v2.7.0 agent for /mcp's hire (spec §12): "hi" completes with Hello World; "kinds" answers
// the TCK seller's artifact kinds (examples/sellers/tck: a text artifact, a data artifact, the output.txt file as raw
// bytes and as a URL) with a data part in its final message too; "ask" asks a text-only question and completes with
// the answer; "slow" works until the gate cancels it. It records each method, each turn's text and each A2A-Extensions
// header it receives.
type hireSeller struct {
	mu      sync.Mutex
	methods []string
	turns   []string
	exts    []string
	srv     *httptest.Server
}

func newHireSeller(t *testing.T) *hireSeller {
	t.Helper()
	return newHireSellerCaps(t, `{"streaming":true}`)
}

// newHireSellerCaps is a hireSeller whose card's capabilities are caps (a JSON object), e.g. one that requires an
// extension of its own.
func newHireSellerCaps(t *testing.T, caps string) *hireSeller {
	t.Helper()
	s := &hireSeller{}
	rpc := a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(s))
	mux := http.NewServeMux()
	var card string
	mux.HandleFunc("/base/.well-known/agent-card.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, card)
	})
	mux.HandleFunc("/base/rpc", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		for _, m := range []string{"SendStreamingMessage", "SendMessage", "CancelTask", "GetTask", "SubscribeToTask"} {
			if bytes.Contains(body, []byte(`"method":"`+m+`"`)) {
				s.mu.Lock()
				s.methods = append(s.methods, m)
				s.mu.Unlock()
				break
			}
		}
		if v := r.Header.Values("A2A-Extensions"); len(v) > 0 {
			s.mu.Lock()
			s.exts = append(s.exts, v...)
			s.mu.Unlock()
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		rpc.ServeHTTP(w, r)
	})
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	card = `{"name":"kinds","description":"answers every kind of part","version":"1.0.0",
"supportedInterfaces":[{"url":"` + s.srv.URL + `/base/rpc","protocolBinding":"JSONRPC","protocolVersion":"1.0"}],
"capabilities":` + caps + `,"defaultInputModes":["text/plain"],"defaultOutputModes":["text/plain"],
"skills":[{"id":"kinds","name":"kinds","description":"kinds","tags":["k"]}]}`
	return s
}

// activations are the A2A-Extensions header values the seller received.
func (s *hireSeller) activations() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.exts...)
}

func (s *hireSeller) count(method string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, m := range s.methods {
		if m == method {
			n++
		}
	}
	return n
}

func (s *hireSeller) turnTexts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.turns...)
}

func (s *hireSeller) Execute(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		text := ec.Message.Parts[0].Text()
		s.mu.Lock()
		s.turns = append(s.turns, text)
		s.mu.Unlock()
		if ec.StoredTask == nil && !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
			return
		}
		say := func(parts ...*a2a.Part) *a2a.Message {
			return a2a.NewMessageForTask(a2a.MessageRoleAgent, ec, parts...)
		}
		switch {
		case ec.StoredTask != nil && ec.StoredTask.Status.State == a2a.TaskStateInputRequired:
			yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, say(a2a.NewTextPart("booked for "+text))), nil)
		case text == "hi":
			yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, say(a2a.NewTextPart("Hello World"))), nil)
		case text == "kinds":
			file := a2a.NewRawPart([]byte("tck"))
			file.MediaType, file.Filename = "text/plain", "output.txt"
			link := a2a.NewFileURLPart("https://example.com/output.txt", "text/plain")
			link.Filename = "output.txt"
			for _, p := range []*a2a.Part{a2a.NewTextPart("tck artifact"), a2a.NewDataPart(map[string]any{"key": "value"}), file, link} {
				if !yield(a2a.NewArtifactEvent(ec, p), nil) {
					return
				}
			}
			yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, say(a2a.NewTextPart("done"), a2a.NewDataPart(map[string]any{"total": 4.0}))), nil)
		case text == "ask":
			yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateInputRequired, say(a2a.NewTextPart("Which date?"))), nil)
		default: // slow: works until canceled
			if !yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateWorking, nil), nil) {
				return
			}
			<-ctx.Done()
		}
	}
}

func (s *hireSeller) Cancel(_ context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCanceled, nil), nil)
	}
}

// mcpClient is an MCP 2025-11-25 client of the gate's own /mcp on one replica, with one session.
type mcpClient struct {
	t       *testing.T
	url     string
	key     string
	sid     string
	version string
	accept  string
	n       int
	seen    []json.RawMessage // every message the client was answered with
}

func newMCPClient(t *testing.T, base, key string) *mcpClient {
	return &mcpClient{t: t, url: base + gatemcp.Path, key: key, version: "2025-11-25", accept: "application/json, text/event-stream"}
}

// on is the client's session on another replica's URL (any replica serves any request of a session).
func (c *mcpClient) on(base string) *mcpClient {
	cp := *c
	cp.url = base + gatemcp.Path
	return &cp
}

// post sends one message; it returns the status, the headers, and the messages of the answer: the JSON body, or each
// SSE event's data.
func (c *mcpClient) post(body string) (int, http.Header, []json.RawMessage) {
	c.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, c.url, strings.NewReader(body))
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", c.accept)
	if c.sid != "" {
		req.Header.Set("Mcp-Session-Id", c.sid)
		req.Header.Set("MCP-Protocol-Version", c.version)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var msgs []json.RawMessage
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		msgs = sseData(b)
	} else if len(bytes.TrimSpace(b)) > 0 {
		msgs = []json.RawMessage{b}
	}
	c.seen = append(c.seen, msgs...)
	return resp.StatusCode, resp.Header, msgs
}

// sseData is the data of each event of an event stream.
func sseData(b []byte) []json.RawMessage {
	var out []json.RawMessage
	var data []string
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if len(data) > 0 {
				out = append(out, json.RawMessage(strings.Join(data, "\n")))
			}
			data = nil
		case strings.HasPrefix(line, "data: "):
			data = append(data, strings.TrimPrefix(line, "data: "))
		}
	}
	return out
}

func (c *mcpClient) initialize(caps string) {
	c.t.Helper()
	status, hdr, msgs := c.post(`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"` + c.version +
		`","capabilities":` + caps + `,"clientInfo":{"name":"test","version":"1"}}}`)
	if status != http.StatusOK || hdr.Get("Mcp-Session-Id") == "" || len(msgs) != 1 {
		c.t.Fatalf("initialize: %d %v %s", status, hdr, msgs)
	}
	c.sid = hdr.Get("Mcp-Session-Id")
	if s, _, _ := c.post(`{"jsonrpc":"2.0","method":"notifications/initialized"}`); s != http.StatusAccepted {
		c.t.Fatalf("notifications/initialized: %d", s)
	}
}

// rpc sends a request and returns the HTTP status and the JSON-RPC response (the last message of the answer).
func (c *mcpClient) rpc(method, params string) (int, rpcAnswer) {
	c.t.Helper()
	c.n++
	status, _, msgs := c.post(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q,"params":%s}`, c.n, method, params))
	var a rpcAnswer
	if len(msgs) > 0 {
		if err := json.Unmarshal(msgs[len(msgs)-1], &a); err != nil {
			c.t.Fatalf("%s: %s: %v", method, msgs[len(msgs)-1], err)
		}
		a.raw = msgs[len(msgs)-1]
		a.all = msgs
	}
	return status, a
}

// call calls a tool and returns the HTTP status and the answer.
func (c *mcpClient) call(tool, args string) (int, rpcAnswer) {
	c.t.Helper()
	return c.rpc("tools/call", `{"name":"`+tool+`","arguments":`+args+`}`)
}

type rpcAnswer struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	} `json:"error"`
	raw json.RawMessage
	all []json.RawMessage
}

// toolResult is a CallToolResult as the tests read it.
type toolResult struct {
	Content []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		URI      string `json:"uri"`
		Resource *struct {
			Blob string `json:"blob"`
		} `json:"resource"`
	} `json:"content"`
	Structured struct {
		TaskID string `json:"taskId"`
		State  string `json:"state"`
		URL    string `json:"url"`
		Text   string `json:"text"`
		Data   []any  `json:"data"`
	} `json:"structuredContent"`
	IsError bool           `json:"isError"`
	Meta    map[string]any `json:"_meta"`
	Task    *struct {
		TaskID        string `json:"taskId"`
		Status        string `json:"status"`
		StatusMessage string `json:"statusMessage"`
		PollInterval  int    `json:"pollInterval"`
	} `json:"task"`
}

func (a rpcAnswer) tool(t *testing.T) toolResult {
	t.Helper()
	if a.Error != nil || a.Result == nil {
		t.Fatalf("not a result: %s", a.raw)
	}
	var r toolResult
	if err := json.Unmarshal(a.Result, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r toolResult) text() string {
	var b strings.Builder
	for _, c := range r.Content {
		b.WriteString(c.Text)
	}
	return b.String()
}

func (e *gateEnv) hireServices(kinds *hireSeller, extra string) string {
	return `services:
  hello:
    kind: a2a
    backend: { a2a: { url: "` + e.seller.srv.URL + `/base", allow_private: true } }
    allow: ["org:default"]
    task: { timeout: 10m, retries: 1 }
  kinds:
    kind: a2a
    backend: { a2a: { url: "` + kinds.srv.URL + `/base", allow_private: true } }
    allow: ["org:default"]
    task: { timeout: 10m, retries: 1 }
` + extra
}

// The gate's own /mcp, legacy era, through two whole replicas on one database (spec §12, M4 WP-04): no credential is
// 401 (D270); a session opened on A is served by B; tools/list; a plain hire completes inline, a slow one answers the
// hire result of a working task after the plain wait; a task-augmented hire answers CreateTaskResult and tasks/result
// renders the TCK seller's artifact kinds with their data parts in structuredContent.data; a question is the reply-tool
// text, reply answers it, and a form-declaring session gets the question as an elicitation/create on tasks/result whose
// accept is the follow-up once; tasks/list is the buyer's hire tasks across services, newest first, and another buyer's
// tasks/get is -32602; tasks/cancel is cancelled within 10 s and the seller saw CancelTask; an MCP service and a ref
// the principal may not hire are tool errors; /mcp writes no calls rows (D289); hire tasks are marked hired with the
// hire's resend key.
func TestGateMCPHire(t *testing.T) {
	e := newGateEnv(t)
	ctx := context.Background()
	kinds := newHireSeller(t)
	up := mcptest.New()
	up.Tools = []json.RawMessage{mcptest.Tool("echo", "")}
	direct := httptest.NewServer(up)
	t.Cleanup(direct.Close)
	services := e.hireServices(kinds, `  web:
    kind: mcp
    backend: { mcp: { url: "`+direct.URL+`/mcp", allow_private: true } }
    allow: ["org:default"]
  closed:
    kind: a2a
    backend: { a2a: { url: "`+kinds.srv.URL+`/base", allow_private: true } }
    allow: ["key:kk_nobody_*"]
`)
	timing := fastTiming
	timing.PlainWait = 4 * time.Second
	a := e.start("a", e.replicaConfig(freePort(t), time.Minute, services), timing)
	b := e.start("b", e.replicaConfig(freePort(t), time.Minute, services), timing)
	key, other := e.mintKey("buyer"), e.mintKey("other")

	// D270: no anonymous principal.
	anonymous := newMCPClient(t, a.url, "")
	if status, hdr, _ := anonymous.post(`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{}}`); status != http.StatusUnauthorized ||
		!strings.HasPrefix(hdr.Get("WWW-Authenticate"), "Bearer") {
		t.Fatalf("/mcp without a credential: %d %v", status, hdr)
	}

	c := newMCPClient(t, a.url, key)
	c.initialize(`{}`)
	cb := c.on(b.url)
	if n := scalar[int64](t, e.pool, `select count(*) from kontra.mcp_sessions where service is null and tenant_id is not null and protocol_version = '2025-11-25'`); n != 1 {
		t.Fatalf("mcp_sessions rows of /mcp: %d", n)
	}
	_, tl := cb.rpc("tools/list", `{}`)
	if tl.Error != nil || !strings.Contains(string(tl.Result), `"name":"hire"`) || !strings.Contains(string(tl.Result), `"taskSupport":"optional"`) {
		t.Fatalf("tools/list on replica b: %s", tl.raw)
	}

	// A plain hire that completes inline, on b; the task is an ordinary A2A task, marked hired, keyed by §12.3's hash.
	args := `{"service":"hello","message":"hi"}`
	_, ans := cb.call("hire", args)
	r := ans.tool(t)
	if r.IsError || r.text() != "Hello World" || r.Structured.State != "TASK_STATE_COMPLETED" || r.Structured.TaskID == "" || len(ans.all) != 1 {
		t.Fatalf("a plain hire: %s", ans.raw)
	}
	firstReq, _ := gatemcp.FirstRequest(gatemcp.Ref{Name: "hello"}, json.RawMessage(args))
	if !scalar[bool](t, e.pool, `select hired and protocol = 'a2a' and first_request = $2 and first_message_id is not null
		from kontra.tasks where id = $1`, r.Structured.TaskID, firstReq[:]) {
		t.Error("the hire task is not an A2A task marked hired with the hire's resend key")
	}
	// The same hire again is a new task: only an idempotencyKey answers an earlier one (§12.3, D338, D351).
	if _, again := c.call("hire", args); again.tool(t).Structured.TaskID == r.Structured.TaskID {
		t.Error("a second unkeyed hire answered the first task")
	}

	// A slow seller: the hire result of a working task after the plain wait, its text naming the A2A URL that follows it;
	// on JSON.
	c.accept = "application/json"
	began := time.Now()
	_, ans = c.call("hire", `{"service":"kinds","message":"slow"}`)
	c.accept = "application/json, text/event-stream"
	slow := ans.tool(t)
	if took := time.Since(began); took < 3*time.Second || took > 15*time.Second || slow.IsError ||
		slow.Structured.State != "TASK_STATE_WORKING" || !strings.Contains(slow.text(), a.url+"/agents/kinds") {
		t.Fatalf("a slow hire after %v: %s", took, ans.raw)
	}
	if slow.Structured.Text != slow.text() || !strings.Contains(slow.Structured.Text, "with GetTask at "+a.url+"/agents/kinds") ||
		slow.Structured.URL != "" {
		t.Errorf("a slow hire's structuredContent is not the hire result of a task still running (D324): %s", ans.raw)
	}

	// tasks/cancel (§11.6, D57) on b: cancelled within 10 s, and the seller received CancelTask.
	began = time.Now()
	_, ans = cb.rpc("tasks/cancel", `{"taskId":"`+slow.Structured.TaskID+`"}`)
	if ans.Error != nil || !strings.Contains(string(ans.Result), `"status":"cancelled"`) || time.Since(began) > 10*time.Second {
		t.Fatalf("tasks/cancel after %v: %s", time.Since(began), ans.raw)
	}
	eventually(t, 10*time.Second, "the seller received CancelTask", func() bool { return kinds.count("CancelTask") >= 1 })
	if st := scalar[string](t, e.pool, `select state from kontra.tasks where id = $1`, slow.Structured.TaskID); st != "canceled" {
		t.Fatalf("after tasks/cancel the task is %s", st)
	}
	_, ans = c.rpc("tasks/cancel", `{"taskId":"`+slow.Structured.TaskID+`"}`)
	if ans.Error == nil || ans.Error.Code != -32602 {
		t.Errorf("tasks/cancel of a cancelled task: %s", ans.raw)
	}

	// A task-augmented hire: CreateTaskResult; tasks/result on b renders the TCK seller's artifact kinds.
	_, ans = c.rpc("tools/call", `{"name":"hire","arguments":{"service":"kinds","message":"kinds"},"task":{"ttl":60000}}`)
	ct := ans.tool(t)
	if ct.Task == nil || ct.Task.TaskID == "" || ct.Task.Status != "working" || ct.Task.PollInterval != 1000 {
		t.Fatalf("a task-augmented hire: %s", ans.raw)
	}
	_, ans = cb.rpc("tasks/result", `{"taskId":"`+ct.Task.TaskID+`"}`)
	res := ans.tool(t)
	var types []string
	for _, b := range res.Content {
		types = append(types, b.Type)
	}
	if res.IsError || fmt.Sprint(types) != "[text text text text resource resource_link]" || res.Content[0].Text != "done" ||
		res.Content[1].Text != `{"total":4}` || res.Content[2].Text != "tck artifact" || res.Content[3].Text != `{"key":"value"}` ||
		res.Content[4].Resource == nil || res.Content[4].Resource.Blob != "dGNr" || res.Content[5].URI != "https://example.com/output.txt" {
		t.Fatalf("tasks/result: %s", ans.raw)
	}
	if res.Structured.TaskID != ct.Task.TaskID || res.Structured.State != "TASK_STATE_COMPLETED" ||
		fmt.Sprint(res.Structured.Data) != "[map[total:4] map[key:value]]" {
		t.Errorf("tasks/result's structuredContent: %+v", res.Structured)
	}
	if rel, _ := res.Meta["io.modelcontextprotocol/related-task"].(map[string]any); rel["taskId"] != ct.Task.TaskID {
		t.Errorf("tasks/result's related-task _meta: %v", res.Meta)
	}
	_, ans = cb.rpc("tasks/get", `{"taskId":"`+ct.Task.TaskID+`"}`)
	if !strings.Contains(string(ans.Result), `"status":"completed"`) {
		t.Errorf("tasks/get: %s", ans.raw)
	}

	// A question (§12.4): this session declared no form elicitation, so a plain hire answers the reply-tool text;
	// reply answers it and the task completes with the answer.
	_, ans = c.call("hire", `{"service":"kinds","message":"ask"}`)
	q := ans.tool(t)
	if q.IsError || q.Structured.State != "TASK_STATE_INPUT_REQUIRED" || q.Meta["sh.kontra/input-required"] != true ||
		q.text() != "Which date? (answer with the reply tool, taskId "+q.Structured.TaskID+")" {
		t.Fatalf("a hire that asks: %s", ans.raw)
	}
	_, ans = cb.rpc("tasks/get", `{"taskId":"`+q.Structured.TaskID+`"}`)
	if !strings.Contains(string(ans.Result), `"status":"working"`) || !strings.Contains(string(ans.Result), `"sh.kontra/input-required":true`) {
		t.Errorf("tasks/get of a question to a session without form elicitation: %s", ans.raw)
	}
	_, ans = cb.call("reply", `{"taskId":"`+q.Structured.TaskID+`","message":"tomorrow"}`)
	if rr := ans.tool(t); rr.IsError || rr.text() != "booked for tomorrow" || rr.Structured.State != "TASK_STATE_COMPLETED" {
		t.Fatalf("reply: %s", ans.raw)
	}
	_, ans = c.call("reply", `{"taskId":"`+q.Structured.TaskID+`","message":"again"}`)
	if rr := ans.tool(t); !rr.IsError {
		t.Errorf("a reply to a completed task: %s", ans.raw)
	}

	// A session that declared form elicitation: tasks/result delivers the question as elicitation/create k.<task>.<seq>;
	// its accept is the follow-up, once; the task completes.
	f := newMCPClient(t, b.url, key)
	f.initialize(`{"elicitation":{"form":{}}}`)
	_, ans = f.rpc("tools/call", `{"name":"hire","arguments":{"service":"kinds","message":"ask"},"task":{}}`)
	fq := ans.tool(t)
	eventually(t, 20*time.Second, "the question is asked", func() bool {
		return scalar[string](t, e.pool, `select state from kontra.tasks where id = $1`, fq.Task.TaskID) == "input-required"
	})
	_, ans = f.rpc("tasks/get", `{"taskId":"`+fq.Task.TaskID+`"}`)
	if !strings.Contains(string(ans.Result), `"status":"input_required"`) || !strings.Contains(string(ans.Result), `"statusMessage":"Which date?"`) {
		t.Errorf("tasks/get of a question to a form-declaring session: %s", ans.raw)
	}
	seq := scalar[int64](t, e.pool, `select max(seq) from kontra.task_events where task_id = $1 and kind = 'status'`, fq.Task.TaskID)
	gateID := fmt.Sprintf("k.%s.%d", fq.Task.TaskID, seq)
	streamed := make(chan []json.RawMessage, 1)
	go func() {
		rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(rctx, http.MethodPost, f.url, strings.NewReader(`{"jsonrpc":"2.0","id":"r1","method":"tasks/result","params":{"taskId":"`+fq.Task.TaskID+`"}}`))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Mcp-Session-Id", f.sid)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			streamed <- nil
			return
		}
		defer resp.Body.Close()
		var msgs []json.RawMessage
		br := bufio.NewReader(resp.Body)
		var data []string
		for {
			line, err := br.ReadString('\n')
			line = strings.TrimRight(line, "\r\n")
			if strings.HasPrefix(line, "data: ") {
				data = append(data, strings.TrimPrefix(line, "data: "))
			}
			if line == "" && len(data) > 0 {
				msgs = append(msgs, json.RawMessage(strings.Join(data, "\n")))
				data = nil
				if len(msgs) == 1 {
					// Answer the elicitation as the client does, on its session.
					_, _, _ = f.post(`{"jsonrpc":"2.0","id":"` + gateID + `","result":{"action":"accept","content":{"reply":"friday"}}}`)
					_, _, _ = f.post(`{"jsonrpc":"2.0","id":"` + gateID + `","result":{"action":"accept","content":{"reply":"saturday"}}}`)
				}
			}
			if err != nil {
				break
			}
		}
		streamed <- msgs
	}()
	msgs := <-streamed
	if len(msgs) != 2 || !strings.Contains(string(msgs[0]), `"method":"elicitation/create"`) || !strings.Contains(string(msgs[0]), `"id":"`+gateID+`"`) ||
		!strings.Contains(string(msgs[0]), `"message":"Which date?"`) || !strings.Contains(string(msgs[0]), `"mode":"form"`) ||
		!strings.Contains(string(msgs[1]), "booked for friday") {
		t.Fatalf("the tasks/result stream of a question: %s", msgs)
	}
	time.Sleep(time.Second)
	if turns := kinds.turnTexts(); strings.Count(strings.Join(turns, ","), "friday") != 1 || strings.Contains(strings.Join(turns, ","), "saturday") {
		t.Errorf("the seller's turns %v: the accepted answer once, the second never", turns)
	}

	// tasks/list: this buyer's hire tasks across services, newest first; another buyer sees none of them.
	_, ans = c.rpc("tasks/list", `{}`)
	var list struct {
		Tasks []struct {
			TaskID string `json:"taskId"`
		} `json:"tasks"`
	}
	_ = json.Unmarshal(ans.Result, &list)
	var listed []string
	for _, x := range list.Tasks {
		listed = append(listed, x.TaskID)
	}
	if len(listed) != 6 || listed[0] != fq.Task.TaskID {
		t.Errorf("tasks/list: %v (want 6, %s first)", listed, fq.Task.TaskID)
	}
	o := newMCPClient(t, a.url, other)
	o.initialize(`{}`)
	if _, ans = o.rpc("tasks/get", `{"taskId":"`+ct.Task.TaskID+`"}`); ans.Error == nil || ans.Error.Code != -32602 {
		t.Errorf("another buyer's tasks/get: %s", ans.raw)
	}
	if _, ans = o.rpc("tasks/list", `{}`); string(ans.Result) != `{"tasks":[]}` {
		t.Errorf("another buyer's tasks/list: %s", ans.raw)
	}
	if _, ans = o.call("reply", `{"taskId":"`+q.Structured.TaskID+`","message":"x"}`); !ans.tool(t).IsError || ans.tool(t).text() != "unknown task" {
		t.Errorf("another buyer's reply: %s", ans.raw)
	}

	// An MCP service, a ref nobody here may hire, a malformed one: tool errors, no task.
	before := scalar[int64](t, e.pool, `select count(*) from kontra.tasks`)
	_, ans = c.call("hire", `{"service":"web","message":"hi"}`)
	if mr := ans.tool(t); !mr.IsError || !strings.Contains(mr.text(), a.url+"/mcp/web") {
		t.Errorf("hire of an MCP service: %s", ans.raw)
	}
	for _, ref := range []string{"closed", "nope"} {
		if _, ans = c.call("hire", `{"service":"`+ref+`","message":"hi"}`); ans.tool(t).text() != "unknown service" || !ans.tool(t).IsError {
			t.Errorf("hire of %s: %s", ref, ans.raw)
		}
	}
	if _, ans = c.call("hire", `{"service":"hello","message":"hi","extra":1}`); !ans.tool(t).IsError {
		t.Errorf("hire with an unknown argument: %s", ans.raw)
	}
	if _, ans = c.call("nope", `{}`); ans.Error == nil || ans.Error.Code != -32602 {
		t.Errorf("an unknown tool: %s", ans.raw)
	}
	if after := scalar[int64](t, e.pool, `select count(*) from kontra.tasks`); after != before {
		t.Errorf("refused hires created %d tasks", after-before)
	}

	// search and describe through the replica; D289: /mcp's tools write no calls rows.
	// search returns A2A agents only: the MCP service web is never listed (D350).
	_, ans = c.call("search", `{}`)
	if sr := ans.tool(t); !strings.Contains(sr.text(), `"ref":"hello"`) || strings.Contains(sr.text(), `"ref":"closed"`) ||
		strings.Contains(sr.text(), `"ref":"web"`) {
		t.Errorf("search: %s", ans.raw)
	}
	_, ans = c.call("describe", `{"service":"web"}`)
	if !strings.Contains(string(ans.Result), `"toolsCached":false`) || !strings.Contains(string(ans.Result), a.url+"/mcp/web") {
		t.Errorf("describe of a cold MCP service: %s", ans.raw)
	}
	eventually(t, 20*time.Second, "describe's background drain fills the tools cache", func() bool {
		_, ans := c.call("describe", `{"service":"web"}`)
		return strings.Contains(string(ans.Result), `"toolsCached":true`) && strings.Contains(string(ans.Result), `"name":"echo"`)
	})
	if n := scalar[int64](t, e.pool, `select count(*) from kontra.calls`); n != 0 {
		t.Errorf("/mcp wrote %d calls rows", n)
	}
	if n := scalar[int64](t, e.pool, `select count(*) from kontra.tasks where not hired`); n != 0 {
		t.Errorf("%d tasks hire created are not marked hired", n)
	}

	// DELETE closes the session; a request on it is then 404.
	req, _ := http.NewRequest(http.MethodDelete, c.url, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Mcp-Session-Id", c.sid)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE: %v %v", resp, err)
	}
	if status, _ := cb.rpc("tools/list", `{}`); status != http.StatusNotFound {
		t.Errorf("a request on a deleted session: %d", status)
	}
}

// hookServer is an operator's Admission server on loopback (spec §13.1) whose answer the test sets: allow, or a raw
// decision.
type hookServer struct {
	mu     sync.Mutex
	raw    string
	asked  []string
	bodies []json.RawMessage // each AdmitRequest as received
	srv    *httptest.Server
}

func newHookServer(t *testing.T) *hookServer {
	h := &hookServer{}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			TaskID string `json:"taskId"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &req)
		h.mu.Lock()
		defer h.mu.Unlock()
		h.asked = append(h.asked, req.TaskID)
		h.bodies = append(h.bodies, b)
		w.Header().Set("Content-Type", "application/json")
		if h.raw != "" {
			_, _ = io.WriteString(w, h.raw)
			return
		}
		_, _ = io.WriteString(w, `{"outcome":"allow"}`)
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *hookServer) set(fn func(*hookServer)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	fn(h)
}

func (h *hookServer) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.asked)
}
