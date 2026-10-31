package mcprelay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"kontra.sh/kontra/internal/anon"
	"kontra.sh/kontra/internal/mcprelay/mcptest"
	"kontra.sh/kontra/internal/pg"
)

// fakeCalls is an anonymous tenant's calls and streams caps (anon.Caps.Call and Stream) as the relay sees them: at
// most limit requests and streamLimit streams in flight, each counted apart (D308); full refuses.
type fakeCalls struct {
	mu          sync.Mutex
	limit       int
	streamLimit int
	inFlight    int
	taken       int
	released    int
	streams     slots
	asked       []string
}

// slots counts one kind of slot.
type slots struct{ inFlight, taken, released int }

func newFakeCalls(calls, streams int) *fakeCalls {
	return &fakeCalls{limit: calls, streamLimit: streams}
}

func (f *fakeCalls) Call(_ context.Context, _ uuid.UUID, slug string) (func(), error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, slug)
	if f.inFlight >= f.limit {
		return nil, fmt.Errorf("%w (calls %d)", anon.ErrTooManyCalls, f.limit)
	}
	f.inFlight++
	f.taken++
	var once sync.Once
	return func() {
		once.Do(func() {
			f.mu.Lock()
			f.inFlight--
			f.released++
			f.mu.Unlock()
		})
	}, nil
}

func (f *fakeCalls) Stream(_ context.Context, _ uuid.UUID, slug string) (func(), error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, slug)
	if f.streams.inFlight >= f.streamLimit {
		return nil, fmt.Errorf("%w (streams %d)", anon.ErrTooManyStreams, f.streamLimit)
	}
	f.streams.inFlight++
	f.streams.taken++
	var once sync.Once
	return func() {
		once.Do(func() {
			f.mu.Lock()
			f.streams.inFlight--
			f.streams.released++
			f.mu.Unlock()
		})
	}, nil
}

// set sets both limits.
func (f *fakeCalls) set(limit int) {
	f.mu.Lock()
	f.limit, f.streamLimit = limit, limit
	f.mu.Unlock()
}

func (f *fakeCalls) setCalls(limit int) {
	f.mu.Lock()
	f.limit = limit
	f.mu.Unlock()
}

func (f *fakeCalls) counts() (inFlight, taken, released int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inFlight, f.taken, f.released
}

func (f *fakeCalls) streamCounts() slots {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.streams
}

// spec/tunnel.md §6.2 (D287): a plain tools/call to an anonymous tenant's service takes one of its calls slots for
// as long as its answer is relayed; with none free it is 429 with -32600 "rate limited" and Retry-After, before
// anything is forwarded or metered, in both eras. tools/list, answered from the gate's cache, takes no slot.
func TestPlainCallsAreCapped(t *testing.T) {
	e := newEnv(t)
	up, url := dualEra(t, mcptest.Tool("echo", ""))
	e.service("svc", url, nil)
	calls := newFakeCalls(100, 100)
	r := e.replica("a", replicaOpts{withDeps: func(_ *replica, deps *Deps, _ *pg.Listener, _ *slog.Logger) { deps.Calls = calls }})
	key, _ := e.apiKey("buyer")
	c := newClient(t, key)
	c.initialize(r.url + "/mcp/svc")
	modern := newModern(t, key, r.url+"/mcp/svc", false)
	_, taken0, released0 := calls.counts() // the initialize's slot (D304)

	refused := func(what string, rep reply) {
		t.Helper()
		code, _ := rpcError(t, rep)
		msg, _ := rep.msg(t)["error"].(map[string]any)["message"].(string)
		if rep.status != http.StatusTooManyRequests || code != -32600 || msg != "rate limited" || rep.header.Get("Retry-After") != "5" {
			t.Fatalf("%s: %d %v %s", what, rep.status, rep.header, rep.body)
		}
	}
	// No slot free (calls in flight elsewhere, on any replica).
	calls.set(0)
	before := up.Count("tools/call")
	refused("legacy, full", c.call(r.url+"/mcp/svc", 2, "echo", "x"))
	refused("modern, full", modern.req(3, "tools/call", map[string]any{"name": "echo", "arguments": map[string]any{"text": "x"}}))
	if up.Count("tools/call") != before {
		t.Fatal("a refused call reached the upstream")
	}
	if n := scalarInt(t, e, `select count(*) from kontra.calls`); n != 0 {
		t.Fatalf("%d calls rows for refused calls", n)
	}
	// A free slot: the call goes through and gives its slot back once answered; tools/list takes none.
	calls.set(1)
	if rep := c.call(r.url+"/mcp/svc", 4, "echo", "y"); rep.status != http.StatusOK {
		t.Fatalf("legacy with a slot: %d %s", rep.status, rep.body)
	}
	if rep := modern.req(5, "tools/call", map[string]any{"name": "echo", "arguments": map[string]any{"text": "y"}}); rep.status != http.StatusOK {
		t.Fatalf("modern with a slot: %d %s", rep.status, rep.body)
	}
	if rep := c.do(http.MethodPost, r.url+"/mcp/svc", `{"jsonrpc":"2.0","id":6,"method":"tools/list"}`, nil); rep.status != http.StatusOK {
		t.Fatalf("tools/list: %d", rep.status)
	}
	if inFlight, taken, released := calls.counts(); inFlight != 0 || taken-taken0 != 2 || released-released0 != 2 {
		t.Fatalf("slots: %d in flight, %d taken, %d released", inFlight, taken-taken0, released-released0)
	}
	calls.mu.Lock()
	asked := calls.asked
	calls.mu.Unlock()
	for _, s := range asked {
		if s != "default" {
			t.Errorf("the cap was asked for tenant %q", s)
		}
	}
	if n := scalarInt(t, e, `select count(*) from kontra.calls`); n != 2 {
		t.Fatalf("%d calls rows, want 2", n)
	}
}

// How a backUpstream answers a POST without an id or without a method: a notification, a client's response, an
// initialize without an id.
const (
	backStock   int32 = iota // as the stock server does (mcptest)
	backEndless              // an event stream of notifications/message events, 4 KiB every 20 ms, that never ends
	backJSON                 // a 1 MiB notifications/message as a 200 application/json body
	backSilent               // an event stream's headers, then nothing until the exchange is ended
)

// backUpstream is dualEra's upstream behind a handler that answers a notification or a client's response as mode
// says, as a seller's own server may; ended receives the body of each such exchange held open (backEndless,
// backSilent) once the upstream saw it end (the gate closed it). idlessInits counts the initializes without an id it
// received, whatever mode is.
type backUpstream struct {
	*mcptest.Server
	url         string
	mode        atomic.Int32
	ended       chan string
	idlessInits atomic.Int32
}

func streamingBack(t *testing.T, tools ...json.RawMessage) *backUpstream {
	t.Helper()
	up := &backUpstream{Server: mcptest.New(), ended: make(chan string, 16)}
	up.Modern = true
	up.Tools = tools
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			up.ServeHTTP(w, r)
			return
		}
		b, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(b))
		var m map[string]json.RawMessage
		_ = json.Unmarshal(b, &m)
		_, hasID := m["id"]
		method, hasMethod := m["method"]
		if !hasID && string(method) == `"initialize"` {
			up.idlessInits.Add(1)
		}
		mode := up.mode.Load()
		if (hasID && hasMethod) || mode == backStock {
			up.ServeHTTP(w, r)
			return
		}
		switch mode {
		case backJSON:
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","method":"notifications/message","params":{"level":"info","data":"%s"}}`, strings.Repeat("A", 1<<20))
			return
		case backSilent:
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case backEndless:
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			for i := 0; ; i++ {
				if _, err := fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/message\","+
					"\"params\":{\"level\":\"info\",\"data\":\"%d %s\"}}\n\n", i, strings.Repeat("A", 4096)); err != nil {
					break
				}
				w.(http.Flusher).Flush()
				select {
				case <-r.Context().Done():
				case <-time.After(20 * time.Millisecond):
					continue
				}
				break
			}
		}
		up.ended <- string(b)
	}))
	t.Cleanup(srv.Close)
	up.url = srv.URL + "/mcp"
	return up
}

// standaloneGET opens a legacy session's standalone GET stream, as the TypeScript SDK's client does once
// notifications/initialized is accepted, and returns its response and the function that closes it (run at the end of
// the test too).
func standaloneGET(t *testing.T, key, url, sid string) (*http.Response, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Mcp-Session-Id", sid)
	req.Header.Set("MCP-Protocol-Version", mcptest.Version)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("GET stream: %v", err)
	}
	closeGET := func() { cancel(); resp.Body.Close() }
	t.Cleanup(closeGET) // a test that fails with the stream open still lets the replica's server close
	return resp, closeGET
}

// spec/tunnel.md §6.2 (D304, D307, D308): not only tools/call. Every other request the relay forwards to an anonymous
// tenant's service takes one of its calls slots while its answer is relayed (an initialize, resources/read,
// prompts/get, completion/complete, a method the gate does not know; on a modern request too), and a GET stream holds
// one of its streams slots, counted apart, for as long as it is open; with none free each is 429 with -32600 "rate
// limited" and Retry-After, and the upstream sees nothing. A notification and a client response take none, and are
// answered 202 with no body whatever the upstream answers them, an endless event stream included: the gate closes it
// unread, so it never relays a stream no slot bounds. An initialize without an id is no request: 400, never forwarded
// (D312).
func TestEveryRelayedRequestIsCapped(t *testing.T) {
	e := newEnv(t)
	up := streamingBack(t, mcptest.Tool("echo", ""))
	e.service("svc", up.url, nil)
	calls := newFakeCalls(100, 100)
	r := e.replica("a", replicaOpts{withDeps: func(_ *replica, deps *Deps, _ *pg.Listener, _ *slog.Logger) { deps.Calls = calls }})
	key, _ := e.apiKey("buyer")
	c := newClient(t, key)
	svcURL := r.url + "/mcp/svc"
	c.initialize(svcURL)
	if inFlight, taken, released := calls.counts(); inFlight != 0 || taken != 1 || released != 1 {
		t.Fatalf("the initialize's slot: %d in flight, %d taken, %d released", inFlight, taken, released)
	}
	modern := newModern(t, key, svcURL, false)
	refused := func(what string, rep reply) {
		t.Helper()
		code, _ := rpcError(t, rep)
		msg, _ := rep.msg(t)["error"].(map[string]any)["message"].(string)
		if rep.status != http.StatusTooManyRequests || code != -32600 || msg != "rate limited" || rep.header.Get("Retry-After") != "5" {
			t.Fatalf("%s: %d %v %s", what, rep.status, rep.header, rep.body)
		}
	}
	methods := []string{"resources/read", "prompts/get", "completion/complete", "x/anything"}
	upstream := func() int {
		n := 0
		for _, m := range append([]string{"initialize"}, methods...) {
			n += up.Count(m)
		}
		return n
	}

	// No slot free.
	calls.set(0)
	before := upstream()
	params := map[string]map[string]any{"resources/read": {"uri": "file:///x"}, "prompts/get": {"name": "p"},
		"completion/complete": {"ref": map[string]any{"type": "ref/prompt", "name": "p"}, "argument": map[string]any{"name": "a", "value": "v"}},
		"x/anything":          {}}
	for i, m := range methods {
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 10 + i, "method": m, "params": params[m]})
		refused("legacy "+m, c.do(http.MethodPost, svcURL, string(b), nil))
		refused("modern "+m, modern.req(20+i, m, params[m]))
	}
	refused("legacy GET stream", c.do(http.MethodGet, svcURL, "", map[string]string{"Accept": "text/event-stream"}))
	other := newClient(t, key)
	refused("initialize", other.do(http.MethodPost, svcURL, `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25",`+
		`"capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`, map[string]string{"MCP-Protocol-Version": ""}))
	if n := upstream() - before; n != 0 {
		t.Fatalf("%d refused requests reached the upstream", n)
	}
	// A modern subscriptions/listen stream takes a stream's slot, not a call's (D308): refused with no stream slot free
	// while calls are, and served, counted as a stream, with one.
	calls.setCalls(100)
	listen := map[string]any{"notifications": map[string]any{"toolsListChanged": true}}
	refused("modern subscriptions/listen with no stream slot", modern.req(40, "subscriptions/listen", listen))
	if up.Count("subscriptions/listen") != 0 {
		t.Fatal("a refused subscriptions/listen reached the upstream")
	}
	calls.set(1)
	streamsBefore := calls.streamCounts()
	_, callsBefore, _ := calls.counts()
	if rep := modern.req(41, "subscriptions/listen", listen); rep.status != http.StatusOK || up.Count("subscriptions/listen") != 1 {
		t.Fatalf("modern subscriptions/listen with a stream slot: %d %s", rep.status, rep.body)
	}
	if _, callsAfter, _ := calls.counts(); calls.streamCounts().taken != streamsBefore.taken+1 || callsAfter != callsBefore {
		t.Fatalf("subscriptions/listen took %+v → %+v stream slots and %d → %d call slots", streamsBefore, calls.streamCounts(), callsBefore, callsAfter)
	}
	calls.set(0)
	// A notification and a client response take no slot, and are answered 202 with no body: from a stock upstream, and
	// from one that answers them with an endless event stream, whose exchange the gate ends at once (D307).
	quick := &client{t: t, key: key, sid: c.sid, hc: &http.Client{Timeout: 5 * time.Second}}
	for _, mode := range []int32{backStock, backEndless} {
		up.mode.Store(mode)
		streams := mode == backEndless
		for _, b := range []string{`{"jsonrpc":"2.0","method":"notifications/progress","params":{"progressToken":1,"progress":1}}`,
			`{"jsonrpc":"2.0","id":"upstream-1","result":{}}`} {
			rep := quick.do(http.MethodPost, svcURL, b, nil)
			if rep.status != http.StatusAccepted || len(rep.body) != 0 || rep.header.Get("Content-Type") != "" {
				t.Fatalf("%s (upstream streams back: %v): %d %q %d bytes", b, streams, rep.status, rep.header.Get("Content-Type"), len(rep.body))
			}
			if streams {
				select {
				case <-up.ended:
				case <-time.After(10 * time.Second):
					t.Fatalf("%s: the upstream's stream was not ended", b)
				}
			}
		}
	}
	// An initialize without an id is no request, and is refused before anything is forwarded (D312, amending D307):
	// 400 with -32600 "invalid request" and id null, without a session and on one, with no slot free, whatever the
	// upstream would answer it (the stock server, a JSON body, a silent event stream, an endless one). Before, the gate
	// forwarded it with no slot and no 202: it relayed the upstream's JSON answer, and held a silent stream, or one of
	// events not typed message, for as long as the upstream kept it open.
	idless := `{"jsonrpc":"2.0","method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},` +
		`"clientInfo":{"name":"test","version":"1"}}}`
	for _, mode := range []int32{backStock, backJSON, backSilent, backEndless} {
		up.mode.Store(mode)
		for _, sid := range []string{"", c.sid} {
			cl := &client{t: t, key: key, sid: sid, hc: &http.Client{Timeout: 5 * time.Second}}
			rep := cl.do(http.MethodPost, svcURL, idless, nil)
			if rep.status != http.StatusBadRequest || rep.header.Get("Content-Type") != "application/json" ||
				string(rep.body) != `{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"invalid request"}}` {
				t.Fatalf("an initialize without an id (upstream mode %d, session %q): %d %q %.200s", mode, sid, rep.status,
					rep.header.Get("Content-Type"), rep.body)
			}
		}
	}
	if n := up.idlessInits.Load(); n != 0 {
		t.Fatalf("%d initializes without an id reached the upstream", n)
	}
	up.mode.Store(backStock)
	if inFlight, _, _ := calls.counts(); inFlight != 0 || calls.streamCounts().inFlight != 0 {
		t.Fatalf("slots in flight after the notifications: %d calls, %+v streams", inFlight, calls.streamCounts())
	}

	// One slot of each: a request goes through (the upstream's own -32601) and gives its slot back. An open GET stream
	// holds a stream's slot, not a call's (D308): a request meanwhile is served, a second stream is refused, and the
	// slot is freed when the stream closes.
	calls.set(1)
	if rep := c.do(http.MethodPost, svcURL, `{"jsonrpc":"2.0","id":30,"method":"resources/read","params":{"uri":"file:///x"}}`, nil); rep.status != http.StatusOK ||
		up.Count("resources/read") != 1 {
		t.Fatalf("resources/read with a slot: %d %s", rep.status, rep.body)
	}
	if inFlight, _, _ := calls.counts(); inFlight != 0 {
		t.Fatalf("%d slots in flight after the answer", inFlight)
	}
	resp, closeGET := standaloneGET(t, key, svcURL, c.sid)
	if resp.StatusCode != http.StatusOK {
		closeGET()
		t.Fatalf("GET stream: %d", resp.StatusCode)
	}
	eventually(t, 5*time.Second, "the GET stream holds a stream's slot", func() bool { return calls.streamCounts().inFlight == 1 })
	if rep := c.do(http.MethodPost, svcURL, `{"jsonrpc":"2.0","id":31,"method":"prompts/get","params":{"name":"p"}}`, nil); rep.status != http.StatusOK {
		t.Fatalf("prompts/get while the stream is open: %d %s", rep.status, rep.body)
	}
	refused("a second GET stream while the first is open", c.do(http.MethodGet, svcURL, "", map[string]string{"Accept": "text/event-stream"}))
	closeGET()
	eventually(t, 5*time.Second, "the closed GET stream frees its slot", func() bool { return calls.streamCounts().inFlight == 0 })
	resp, closeGET = standaloneGET(t, key, svcURL, c.sid)
	closeGET()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a GET stream after the first closed: %d", resp.StatusCode)
	}
	eventually(t, 5*time.Second, "every slot released", func() bool {
		inFlight, taken, released := calls.counts()
		st := calls.streamCounts()
		return inFlight == 0 && taken == released && st.inFlight == 0 && st.taken == st.released && st.taken == 3 // the listen and two GETs
	})
}

// spec/tunnel.md §6.2 (review round 2, D308): the TypeScript SDK's client opens its standalone GET stream as soon as
// notifications/initialized is accepted, and the stock server keeps it open while the client is idle. With §6.2's
// defaults, eight such clients hold eight streams, and every request of theirs, and a ninth client's initialize, is
// still served: streams take the streams slots, never the calls requests need. A ninth stream is refused.
func TestIdleStreamsLeaveCallsTheirSlots(t *testing.T) {
	e := newEnv(t)
	_, url := dualEra(t, mcptest.Tool("echo", ""))
	e.service("svc", url, nil)
	calls := newFakeCalls(anon.Defaults.Calls, anon.Defaults.Streams)
	r := e.replica("a", replicaOpts{withDeps: func(_ *replica, deps *Deps, _ *pg.Listener, _ *slog.Logger) { deps.Calls = calls }})
	key, _ := e.apiKey("buyer")
	svcURL := r.url + "/mcp/svc"
	var clients []*client
	for i := range anon.Defaults.Streams {
		c := newClient(t, key)
		c.initialize(svcURL)
		if rep := c.do(http.MethodPost, svcURL, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, nil); rep.status != http.StatusAccepted {
			t.Fatalf("client %d: notifications/initialized: %d %s", i, rep.status, rep.body)
		}
		resp, _ := standaloneGET(t, key, svcURL, c.sid)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("client %d: GET stream: %d", i, resp.StatusCode)
		}
		clients = append(clients, c)
	}
	eventually(t, 5*time.Second, "8 GET streams open", func() bool {
		inFlight, _, _ := calls.counts()
		return inFlight+calls.streamCounts().inFlight == anon.Defaults.Streams
	})
	for i, c := range clients {
		if rep := c.call(svcURL, 100+i, "echo", "hi"); rep.status != http.StatusOK || rep.msg(t)["result"] == nil {
			t.Fatalf("client %d: tools/call with 8 idle streams open: %d %s", i, rep.status, rep.body)
		}
	}
	if st := calls.streamCounts(); st.inFlight != anon.Defaults.Streams {
		t.Fatalf("the 8 GET streams hold %+v stream slots", st)
	}
	ninth := newClient(t, key)
	ninth.initialize(svcURL)
	resp, closeGET := standaloneGET(t, key, svcURL, ninth.sid)
	defer closeGET()
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") != "5" {
		t.Fatalf("a ninth GET stream: %d", resp.StatusCode)
	}
	if rep := ninth.call(svcURL, 200, "echo", "hi"); rep.status != http.StatusOK {
		t.Fatalf("the ninth client's tools/call: %d %s", rep.status, rep.body)
	}
	if inFlight, taken, released := calls.counts(); inFlight != 0 || taken != released || taken != 2*len(clients)+2 {
		t.Fatalf("calls slots: %d in flight, %d taken, %d released", inFlight, taken, released)
	}
}

// anonRefused writes §6.2's answers: a runtime cap 429 with -32600 and Retry-After, a caller's call over the storage
// limit -32602 "anonymous storage limit"; anything else is not its.
func TestAnonRefused(t *testing.T) {
	for _, c := range []struct {
		err    error
		status int
		code   int
		msg    string
	}{
		{fmt.Errorf("x: %w", anon.ErrTooManyTasks), 429, -32600, "rate limited"},
		{anon.ErrTooManyCalls, 429, -32600, "rate limited"},
		{fmt.Errorf("%w (1 stored)", anon.ErrStorageFull), 200, -32602, "anonymous storage limit"},
	} {
		w := httptest.NewRecorder()
		if !anonRefused(w, []byte(`7`), c.err) {
			t.Fatalf("%v not answered", c.err)
		}
		rep := reply{status: w.Code, header: w.Header(), body: w.Body.Bytes()}
		code, _ := rpcError(t, rep)
		msg := rep.msg(t)["error"].(map[string]any)["message"]
		if rep.status != c.status || code != c.code || msg != c.msg || (c.status == 429) != (rep.header.Get("Retry-After") == "5") ||
			rep.msg(t)["id"] != float64(7) {
			t.Errorf("%v: %d %v %s", c.err, rep.status, rep.header, rep.body)
		}
	}
	if anonRefused(httptest.NewRecorder(), nil, errors.New("other")) {
		t.Error("another error answered")
	}
}

func scalarInt(t *testing.T, e *env, sql string) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), sql).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
