package a2ahttp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/google/uuid"

	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/card"
	"kontra.sh/kontra/internal/edge"
	"kontra.sh/kontra/internal/ids"
	"kontra.sh/kontra/internal/lifecycle"
	"kontra.sh/kontra/internal/relay"
	"kontra.sh/kontra/internal/sse"
	"kontra.sh/kontra/internal/store"
)

var tenantID = uuid.MustParse("0192f0b4-0000-7000-8000-0000000000ee")

// call is one a2a-go handler call as the stub saw it.
type call struct {
	method     string
	principal  auth.Principal
	versions   []string
	extensions []string
}

// stub is the service's a2a-go handler: it records each call and answers a fixed task, or err.
type stub struct {
	mu    sync.Mutex
	calls []call
	err   error
}

func (s *stub) record(ctx context.Context, method string) {
	c := call{method: method}
	c.principal, _ = auth.FromContext(ctx)
	if cc, ok := a2asrv.CallContextFrom(ctx); ok {
		c.versions, _ = cc.ServiceParams().Get(a2a.SvcParamVersion)
		c.extensions, _ = cc.ServiceParams().Get(a2a.SvcParamExtensions)
	}
	s.mu.Lock()
	s.calls = append(s.calls, c)
	s.mu.Unlock()
}

func (s *stub) last() (call, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.calls) == 0 {
		return call{}, 0
	}
	return s.calls[len(s.calls)-1], len(s.calls)
}

func task() *a2a.Task {
	return &a2a.Task{ID: "0192f0b4-0000-7000-8000-000000000001", ContextID: "c", Status: a2a.TaskStatus{State: a2a.TaskStateWorking}}
}

func (s *stub) GetTask(ctx context.Context, _ *a2a.GetTaskRequest) (*a2a.Task, error) {
	s.record(ctx, "GetTask")
	return task(), s.err
}
func (s *stub) ListTasks(ctx context.Context, _ *a2a.ListTasksRequest) (*a2a.ListTasksResponse, error) {
	s.record(ctx, "ListTasks")
	return &a2a.ListTasksResponse{Tasks: []*a2a.Task{}}, s.err
}
func (s *stub) CancelTask(ctx context.Context, _ *a2a.CancelTaskRequest) (*a2a.Task, error) {
	s.record(ctx, "CancelTask")
	return task(), s.err
}
func (s *stub) SendMessage(ctx context.Context, _ *a2a.SendMessageRequest) (a2a.SendMessageResult, error) {
	s.record(ctx, "SendMessage")
	if s.err != nil {
		return nil, s.err
	}
	return task(), nil
}
func (s *stub) SubscribeToTask(ctx context.Context, _ *a2a.SubscribeToTaskRequest) iter.Seq2[a2a.Event, error] {
	s.record(ctx, "SubscribeToTask(a2a-go)")
	return func(yield func(a2a.Event, error) bool) { yield(task(), nil) }
}
func (s *stub) SendStreamingMessage(ctx context.Context, _ *a2a.SendMessageRequest) iter.Seq2[a2a.Event, error] {
	s.record(ctx, "SendStreamingMessage")
	return func(yield func(a2a.Event, error) bool) {
		if s.err != nil {
			yield(nil, s.err)
			return
		}
		yield(task(), nil)
	}
}
func (s *stub) GetTaskPushConfig(ctx context.Context, _ *a2a.GetTaskPushConfigRequest) (*a2a.PushConfig, error) {
	s.record(ctx, "GetTaskPushConfig")
	return &a2a.PushConfig{ID: "p", URL: "https://x"}, s.err
}
func (s *stub) ListTaskPushConfigs(ctx context.Context, _ *a2a.ListTaskPushConfigRequest) (*a2a.ListTaskPushConfigResponse, error) {
	s.record(ctx, "ListTaskPushConfigs")
	return &a2a.ListTaskPushConfigResponse{}, s.err
}
func (s *stub) CreateTaskPushConfig(ctx context.Context, _ *a2a.PushConfig) (*a2a.PushConfig, error) {
	s.record(ctx, "CreateTaskPushConfig")
	return &a2a.PushConfig{ID: "p", URL: "https://x"}, s.err
}
func (s *stub) DeleteTaskPushConfig(ctx context.Context, _ *a2a.DeleteTaskPushConfigRequest) error {
	s.record(ctx, "DeleteTaskPushConfig")
	return s.err
}
func (s *stub) GetExtendedAgentCard(ctx context.Context, _ *a2a.GetExtendedAgentCardRequest) (*a2a.AgentCard, error) {
	s.record(ctx, "GetExtendedAgentCard")
	return nil, a2a.ErrUnsupportedOperation
}

type handlers struct{ h *stub }

func (f handlers) For(_ context.Context, svc store.ServiceRef) (*relay.ServiceHandler, error) {
	return &relay.ServiceHandler{Service: svc, Handler: f.h}, nil
}

// streams records what the SSE writer was asked; SendStreaming runs the starter.
type streams struct {
	mu     sync.Mutex
	method string
	stream sse.Stream
	header http.Header
	start  sse.Start
	err    error
}

func (f *streams) SendStreaming(w http.ResponseWriter, r *http.Request, s sse.Stream, start sse.Starter) {
	st, err := start(r.Context())
	f.mu.Lock()
	f.method, f.stream, f.header, f.start, f.err = "SendStreaming", s, r.Header.Clone(), st, err
	f.mu.Unlock()
	if err != nil {
		sse.WriteError(w, s.Binding, s.RequestID, 0, err)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = w.Write([]byte("data: stream\n\n"))
}

func (f *streams) Subscribe(w http.ResponseWriter, r *http.Request, s sse.Stream) {
	f.mu.Lock()
	f.method, f.stream, f.header = "Subscribe", s, r.Header.Clone()
	f.mu.Unlock()
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = w.Write([]byte("data: stream\n\n"))
}

func (f *streams) take() (string, sse.Stream, http.Header) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, s, h := f.method, f.stream, f.header
	f.method = ""
	return m, s, h
}

// authn maps bearer tokens to principals; "bad" is a refused credential.
type authn map[string]auth.Principal

func (a authn) Authenticate(r *http.Request, anonymousBuyer uuid.UUID) (auth.Principal, bool, error) {
	tok, present, err := auth.BearerToken(r.Header)
	if !present {
		return auth.Anonymous(anonymousBuyer), false, nil
	}
	if err != nil {
		return auth.Principal{}, false, auth.ErrUnauthenticated
	}
	p, ok := a[tok]
	if !ok {
		return auth.Principal{}, false, auth.ErrUnauthenticated
	}
	return p, true, nil
}

type registry map[string]*Service

func (r registry) Lookup(_ context.Context, tenant, name string) (*Service, error) {
	return r[tenant+"/"+name], nil
}

var (
	buyerA = uuid.MustParse("0192f0b4-0000-7000-8000-00000000000a")
	alice  = auth.Principal{Kind: auth.KindKey, ID: "key:kk_alice_00000001", BuyerID: buyerA, Via: auth.ViaAPIKey,
		Scopes: []auth.Scope{auth.ScopeHire}, TenantID: &tenantID, TenantSlug: "default"}
	bob = auth.Principal{Kind: auth.KindKey, ID: "key:kk_bob_00000001", BuyerID: uuid.MustParse("0192f0b4-0000-7000-8000-00000000000b"),
		Via: auth.ViaAPIKey, Scopes: []auth.Scope{auth.ScopeHire}}
	enroller = auth.Principal{Kind: auth.KindKey, ID: "key:kk_alice_00000002", BuyerID: buyerA, Via: auth.ViaAPIKey,
		Scopes: []auth.Scope{auth.ScopeEnrollAll}, TenantID: &tenantID, TenantSlug: "default"}
)

const (
	extOptional = "https://example.com/ext/optional/v1"
	extRequired = "https://example.com/ext/required/v1"
)

func cardOf(t *testing.T, required bool) *card.Served {
	t.Helper()
	exts := []map[string]any{{"uri": extOptional}, {"uri": "not-an-object-entry", "required": "yes"}}
	if required {
		exts = append(exts, map[string]any{"uri": extRequired, "required": true})
	}
	body, _ := json.Marshal(map[string]any{"name": "hello", "capabilities": map[string]any{"extensions": exts}})
	return &card.Served{Body: body, Upstream: []byte(`{"name":"hello","url":"http://seller"}`), ETag: card.ETag(body), LastModified: time.Now()}
}

func allow(t *testing.T, entries ...string) auth.AllowList {
	t.Helper()
	a, err := auth.ParseAllowList(entries)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

type fixture struct {
	t   *testing.T
	srv *Server
	h   *stub
	st  *streams
	reg registry
}

func newFixture(t *testing.T, multi bool) *fixture {
	f := &fixture{t: t, h: &stub{}, st: &streams{}, reg: registry{}}
	ref := func(name, kind string) store.ServiceRef {
		return store.ServiceRef{TenantID: tenantID, Tenant: "default", Name: name, Kind: kind, Timeout: time.Hour}
	}
	c := cardOf(t, false)
	cards := func() (*card.Served, error) { return c, nil }
	tn := "default"
	f.reg[tn+"/hello"] = &Service{Ref: ref("hello", "a2a"), Allow: allow(t, "org:default"), Cards: cards}
	f.reg[tn+"/strict"] = &Service{Ref: ref("strict", "a2a"), Allow: allow(t, "org:default"),
		Cards: func() (*card.Served, error) { return cardOf(t, true), nil }}
	f.reg[tn+"/open"] = &Service{Ref: ref("open", "a2a"), Allow: allow(t, "anonymous"), Cards: cards}
	f.reg[tn+"/listed"] = &Service{Ref: ref("listed", "a2a"), Allow: allow(t, "org:default"), Listed: true, Cards: cards}
	f.reg[tn+"/off"] = &Service{Ref: ref("off", "a2a"), Allow: allow(t, "org:default"), Disabled: true, Cards: cards}
	f.reg[tn+"/tools"] = &Service{Ref: ref("tools", "mcp"), Allow: allow(t, "org:default"), Cards: cards}
	f.srv = New(Deps{Services: f.reg, Handlers: handlers{f.h}, SSE: f.st, Multi: multi,
		Auth: authn{"alice": alice, "bob": bob, "enroller": enroller}})
	return f
}

type req struct {
	method, path, body string
	header             http.Header
}

func (f *fixture) do(r req) *httptest.ResponseRecorder {
	f.t.Helper()
	hr := httptest.NewRequest(r.method, r.path, strings.NewReader(r.body))
	for k, v := range r.header {
		hr.Header[k] = v
	}
	w := httptest.NewRecorder()
	f.srv.ServeHTTP(w, hr)
	return w
}

func hdr(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(kv); i += 2 {
		h.Add(kv[i], kv[i+1])
	}
	return h
}

// v1 is a request header set of an authenticated 1.0 caller.
func v1(token string, kv ...string) http.Header {
	h := hdr(kv...)
	h.Set("A2A-Version", "1.0")
	if token != "" {
		h.Set("Authorization", "Bearer "+token)
	}
	return h
}

func rpc(method, params string, id any) string {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": json.RawMessage(params)})
	return string(b)
}

const msg = `{"message":{"messageId":"m1","role":"ROLE_USER","parts":[{"text":"hi"}]}}`

type rpcResp struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *sse.RPCError   `json:"error"`
}

func decodeRPC(t *testing.T, w *httptest.ResponseRecorder) rpcResp {
	t.Helper()
	var r rpcResp
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatalf("%d %s: %v", w.Code, w.Body, err)
	}
	return r
}

// JSON-RPC on POST {svc} and POST {svc}/: the nine non-streaming methods reach a2a-go's JSON-RPC handler
// with the caller's principal; SendStreamingMessage and SubscribeToTask go to the gate's SSE writer.
func TestJSONRPCRoutes(t *testing.T) {
	f := newFixture(t, false)
	tid := "0192f0b4-0000-7000-8000-000000000001"
	for i, c := range []struct{ method, params, want string }{
		{"SendMessage", msg, "SendMessage"},
		{"GetTask", `{"id":"` + tid + `"}`, "GetTask"},
		{"ListTasks", `{}`, "ListTasks"},
		{"CancelTask", `{"id":"` + tid + `"}`, "CancelTask"},
		{"CreateTaskPushNotificationConfig", `{"taskId":"` + tid + `","url":"https://x"}`, "CreateTaskPushConfig"},
		{"GetTaskPushNotificationConfig", `{"taskId":"` + tid + `","id":"p"}`, "GetTaskPushConfig"},
		{"ListTaskPushNotificationConfigs", `{"taskId":"` + tid + `"}`, "ListTaskPushConfigs"},
		{"DeleteTaskPushNotificationConfig", `{"taskId":"` + tid + `","id":"p"}`, "DeleteTaskPushConfig"},
		{"GetExtendedAgentCard", `{}`, "GetExtendedAgentCard"},
	} {
		for _, path := range []string{"/agents/hello", "/agents/hello/"} {
			w := f.do(req{"POST", path, rpc(c.method, c.params, i), v1("alice")})
			got, _ := f.h.last()
			if w.Code != 200 || got.method != c.want || got.principal.ID != alice.ID {
				t.Errorf("%s %s: %d %s; handler saw %+v", path, c.method, w.Code, w.Body, got)
			}
			if r := decodeRPC(t, w); string(r.ID) != fmt.Sprint(i) {
				t.Errorf("%s %s: id %s", path, c.method, r.ID)
			}
		}
	}
	// The streaming methods are the SSE writer's, with the request id kept as sent.
	w := f.do(req{"POST", "/agents/hello/", rpc("SubscribeToTask", `{"id":"`+tid+`"}`, "sub-1"), v1("alice", "Last-Event-ID", "12")})
	m, s, _ := f.st.take()
	if w.Code != 200 || m != "Subscribe" || s.Binding != sse.JSONRPC || string(s.RequestID) != `"sub-1"` ||
		string(s.TaskID) != tid || s.LastEventID == nil || *s.LastEventID != 12 || s.Owner.BuyerID != buyerA ||
		s.Owner.Principal != alice.ID || s.Service.Name != "hello" {
		t.Errorf("SubscribeToTask: %d %s %+v", w.Code, m, s)
	}
	w = f.do(req{"POST", "/agents/hello", rpc("SendStreamingMessage", msg, 3), v1("alice")})
	m, s, _ = f.st.take()
	if w.Code != 200 || m != "SendStreaming" || s.Binding != sse.JSONRPC || string(s.RequestID) != "3" {
		t.Errorf("SendStreamingMessage: %d %s %+v", w.Code, m, s)
	}
	// The starter ran a2a-go's SendStreamingMessage with the caller's principal and a 1.0 call context; the
	// stub fills no StreamStart, so its snapshot is an interceptor's own answer.
	if got, _ := f.h.last(); got.method != "SendStreamingMessage" || got.principal.ID != alice.ID || fmt.Sprint(got.versions) != "[1.0]" {
		t.Errorf("starter: %+v", got)
	}
	if f.st.start.Answer == nil || f.st.err != nil {
		t.Errorf("start %+v %v", f.st.start, f.st.err)
	}
	// A bad Last-Event-ID is InvalidParams before the writer.
	w = f.do(req{"POST", "/agents/hello", rpc("SubscribeToTask", `{"id":"`+tid+`"}`, 4), v1("alice", "Last-Event-ID", "x")})
	if r := decodeRPC(t, w); r.Error == nil || r.Error.Code != -32602 {
		t.Errorf("bad Last-Event-ID: %s", w.Body)
	}
	if m, _, _ := f.st.take(); m != "" {
		t.Errorf("the writer ran: %s", m)
	}
	// Malformed streaming requests are answered as a2a-go would.
	w = f.do(req{"POST", "/agents/hello", `{"jsonrpc":"2.0","id":5,"method":"SendStreamingMessage","params":{"message":7}}`, v1("alice")})
	if r := decodeRPC(t, w); r.Error == nil || r.Error.Code != -32602 || string(r.ID) != "5" {
		t.Errorf("params of the wrong type: %s", w.Body)
	}
	w = f.do(req{"POST", "/agents/hello", `{"jsonrpc":"1.0","id":6,"method":"SubscribeToTask","params":{}}`, v1("alice")})
	if r := decodeRPC(t, w); r.Error == nil || r.Error.Code != -32600 {
		t.Errorf("jsonrpc 1.0: %s", w.Body)
	}
	w = f.do(req{"POST", "/agents/hello", `not json`, v1("alice")})
	if r := decodeRPC(t, w); r.Error == nil || r.Error.Code != -32700 {
		t.Errorf("not JSON: %s", w.Body)
	}
}

// HTTP+JSON: message:stream and tasks/{id}:subscribe (GET and POST) go to the SSE writer; the other nine
// operations reach a2a-go's REST handler under the service path.
func TestRESTRoutes(t *testing.T) {
	f := newFixture(t, false)
	tid := "0192f0b4-0000-7000-8000-000000000001"
	base := "/agents/hello"
	for _, c := range []struct{ method, path, body, want string }{
		{"POST", "/message:send", msg, "SendMessage"},
		{"GET", "/tasks", "", "ListTasks"},
		{"GET", "/tasks/" + tid, "", "GetTask"},
		{"POST", "/tasks/" + tid + ":cancel", "", "CancelTask"},
		{"POST", "/tasks/" + tid + "/pushNotificationConfigs", `{"url":"https://x"}`, "CreateTaskPushConfig"},
		{"GET", "/tasks/" + tid + "/pushNotificationConfigs", "", "ListTaskPushConfigs"},
		{"GET", "/tasks/" + tid + "/pushNotificationConfigs/p", "", "GetTaskPushConfig"},
		{"DELETE", "/tasks/" + tid + "/pushNotificationConfigs/p", "", "DeleteTaskPushConfig"},
		{"GET", "/extendedAgentCard", "", "GetExtendedAgentCard"},
	} {
		w := f.do(req{c.method, base + c.path, c.body, v1("alice")})
		got, _ := f.h.last()
		if got.method != c.want || got.principal.ID != alice.ID {
			t.Errorf("%s %s: %d %s; handler saw %+v", c.method, c.path, w.Code, w.Body, got)
		}
	}
	for _, m := range []string{"GET", "POST"} {
		w := f.do(req{m, base + "/tasks/" + tid + ":subscribe", "", v1("alice", "Last-Event-ID", "3")})
		method, s, _ := f.st.take()
		if w.Code != 200 || method != "Subscribe" || s.Binding != sse.HTTPJSON || string(s.TaskID) != tid || *s.LastEventID != 3 {
			t.Errorf("%s :subscribe: %d %s %+v", m, w.Code, method, s)
		}
	}
	w := f.do(req{"POST", base + "/message:stream", msg, v1("alice")})
	if method, s, _ := f.st.take(); w.Code != 200 || method != "SendStreaming" || s.Binding != sse.HTTPJSON {
		t.Errorf("message:stream: %d %s", w.Code, method)
	}
	w = f.do(req{"POST", base + "/message:stream", msg + " trailing", v1("alice")})
	if w.Code != 400 || !strings.Contains(w.Body.String(), "INVALID_ARGUMENT") {
		t.Errorf("message:stream with trailing data: %d %s", w.Code, w.Body)
	}
	// Paths that are no A2A route are the closed router's empty 404.
	for _, p := range []string{"/agents/hello/tasks/" + tid + ":nope", "/agents/hello/foo", "/agents/hello/tasks/", "/agents/", "/agents/hello/.well-known/x"} {
		if w := f.do(req{"POST", p, "", v1("alice")}); w.Code != 404 || w.Body.Len() != 0 {
			t.Errorf("POST %s: %d %q", p, w.Code, w.Body)
		}
	}
	if w := f.do(req{"PUT", base, "", v1("alice")}); w.Code != 404 || w.Body.Len() != 0 {
		t.Errorf("PUT {svc}: %d", w.Code)
	}
}

// GET {svc} returns the card (D10), as does the well-known path; the upstream card is verbatim; both
// carry the caching headers, are not version-checked, and the anonymous/listed exception holds.
func TestCards(t *testing.T) {
	f := newFixture(t, false)
	c := cardOf(t, false)
	for _, p := range []string{"/agents/hello", "/agents/hello/.well-known/agent-card.json"} {
		w := f.do(req{"GET", p, "", hdr("Authorization", "Bearer alice")}) // no A2A-Version: cards are not checked
		if w.Code != 200 || w.Body.String() != string(c.Body) || w.Header().Get("Cache-Control") != "max-age=60" ||
			w.Header().Get("ETag") != c.ETag || w.Header().Get("Last-Modified") == "" {
			t.Errorf("GET %s: %d %s %v", p, w.Code, w.Body, w.Header())
		}
	}
	w := f.do(req{"GET", "/agents/hello/.well-known/agent-card.upstream.json", "", hdr("Authorization", "Bearer alice")})
	if w.Code != 200 || w.Body.String() != string(c.Upstream) {
		t.Errorf("upstream card: %d %s", w.Code, w.Body)
	}
	// Public cards: anonymous and listed services show their cards to anyone; their A2A routes still
	// follow allow:.
	for _, name := range []string{"open", "listed"} {
		for _, h := range []http.Header{nil, hdr("Authorization", "Bearer bob")} {
			if w := f.do(req{"GET", "/agents/" + name, "", h}); w.Code != 200 {
				t.Errorf("%s card for %v: %d", name, h, w.Code)
			}
		}
	}
	if w := f.do(req{"POST", "/agents/listed", rpc("GetTask", `{"id":"x"}`, 1), v1("bob")}); w.Code != 404 {
		t.Errorf("a listed service's A2A route for an unadmitted caller: %d", w.Code)
	}
	// The anonymous principal reaches an anonymous service with its fixed buyer (spec §2.3).
	w = f.do(req{"POST", "/agents/open", rpc("SubscribeToTask", `{"id":"x"}`, 1), v1("")})
	_, s, _ := f.st.take()
	want, _ := ids.AnonymousBuyer(uuid.Nil)
	if w.Code != 200 || s.Owner.BuyerID != want || s.Owner.Via != string(auth.ViaAnonymous) || s.Owner.Principal != auth.AnonymousID {
		t.Errorf("anonymous: %d %+v", w.Code, s.Owner)
	}
}

// The unknown-service answer: a path naming no service, a disabled one, one of the other kind, one whose
// allow: does not admit the caller, an invalid name, and a JSON-RPC request with a tenant all get the same
// 404, with a body that fits the route: empty on cards, JSON-RPC -32601 on JSON-RPC, google.rpc.Status
// NOT_FOUND on HTTP+JSON (spec §2.1, D50).
func TestUnknownServiceIsOneAnswer(t *testing.T) {
	f := newFixture(t, false)
	routes := []struct {
		method, rest, body string
		shape              string
	}{
		{"GET", "", "", "empty"},
		{"GET", "/.well-known/agent-card.json", "", "empty"},
		{"GET", "/.well-known/agent-card.upstream.json", "", "empty"},
		{"POST", "", rpc("GetTask", `{"id":"x"}`, 9), "jsonrpc"},
		{"POST", "/", rpc("SendStreamingMessage", msg, 9), "jsonrpc"},
		{"POST", "/message:send", msg, "rest"},
		{"POST", "/message:stream", msg, "rest"},
		{"GET", "/tasks", "", "rest"},
		{"GET", "/tasks/x", "", "rest"},
		{"POST", "/tasks/x:subscribe", "", "rest"},
		{"GET", "/tasks/x:subscribe", "", "rest"},
		{"POST", "/tasks/x:cancel", "", "rest"},
		{"GET", "/tasks/x/pushNotificationConfigs", "", "rest"},
		{"DELETE", "/tasks/x/pushNotificationConfigs/p", "", "rest"},
		{"GET", "/extendedAgentCard", "", "rest"},
	}
	type answer struct {
		code int
		ct   string
		body string
	}
	for _, r := range routes {
		var first *answer
		for _, c := range []struct {
			name, svc, token string
		}{
			{"no such service", "nope", "alice"},
			{"disabled", "off", "alice"},
			{"other kind", "tools", "alice"},
			{"not admitted", "hello", "bob"},
			{"anonymous caller", "hello", ""},
			{"no hire scope", "hello", "enroller"},
			{"invalid name", "Not_A_Name", "alice"},
		} {
			w := f.do(req{r.method, "/agents/" + c.svc + r.rest, r.body, v1(c.token)})
			got := answer{w.Code, w.Header().Get("Content-Type"), w.Body.String()}
			if got.code != 404 {
				t.Errorf("%s %s (%s): %d %s", r.method, r.rest, c.name, w.Code, w.Body)
				continue
			}
			switch r.shape {
			case "empty":
				if got.body != "" {
					t.Errorf("%s %s (%s): body %q", r.method, r.rest, c.name, got.body)
				}
			case "jsonrpc":
				resp := decodeRPC(t, w)
				if resp.Error == nil || resp.Error.Code != -32601 || resp.Error.Message != "unknown service" || string(resp.ID) != "9" {
					t.Errorf("%s %s (%s): %s", r.method, r.rest, c.name, w.Body)
				}
			case "rest":
				var b struct {
					Error struct {
						Code   int    `json:"code"`
						Status string `json:"status"`
					} `json:"error"`
				}
				if json.Unmarshal(w.Body.Bytes(), &b) != nil || b.Error.Code != 404 || b.Error.Status != "NOT_FOUND" {
					t.Errorf("%s %s (%s): %s", r.method, r.rest, c.name, w.Body)
				}
			}
			if first == nil {
				first = &got
			} else if *first != got {
				t.Errorf("%s %s (%s): %+v differs from %+v", r.method, r.rest, c.name, got, *first)
			}
		}
	}
	// A JSON-RPC request with a non-empty tenant is the unknown-service answer too, for every method.
	for _, m := range []string{"GetTask", "SendStreamingMessage", "SubscribeToTask"} {
		w := f.do(req{"POST", "/agents/hello", rpc(m, `{"id":"x","tenant":"acme","message":{"messageId":"m","role":"ROLE_USER","parts":[{"text":"x"}]}}`, 9), v1("alice")})
		resp := decodeRPC(t, w)
		if w.Code != 404 || resp.Error == nil || resp.Error.Code != -32601 {
			t.Errorf("%s with a tenant: %d %s", m, w.Code, w.Body)
		}
	}
	if n := len(f.h.calls); n != 0 {
		t.Errorf("the service handler ran %d times for unknown services", n)
	}
	if m, _, _ := f.st.take(); m != "" {
		t.Errorf("the SSE writer ran: %s", m)
	}
}

// A bad credential is 401 on every route, whether or not the service exists (the answer never says).
func TestBadCredential(t *testing.T) {
	f := newFixture(t, false)
	for _, svc := range []string{"hello", "nope"} {
		for _, r := range []req{
			{"GET", "/agents/" + svc, "", hdr("Authorization", "Bearer bad")},
			{"POST", "/agents/" + svc, rpc("GetTask", `{"id":"x"}`, 2), v1("bad")},
			{"GET", "/agents/" + svc + "/tasks", "", v1("bad")},
			{"GET", "/agents/" + svc + "/tasks", "", hdr("A2A-Version", "1.0", "Authorization", "Basic abc")},
		} {
			w := f.do(r)
			if w.Code != 401 || w.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Errorf("%s %s: %d", r.method, r.path, w.Code)
			}
		}
	}
	w := f.do(req{"POST", "/agents/hello", rpc("GetTask", `{"id":"x"}`, 2), v1("bad")})
	if r := decodeRPC(t, w); r.Error == nil || r.Error.Code != -31401 || string(r.ID) != "2" {
		t.Errorf("JSON-RPC 401 body: %s", w.Body)
	}
}

// A2A-Version: absent is -32009 (HTTP 400 on HTTP+JSON) naming the header; 1.0.1 is accepted as 1.0; the
// query parameter is honoured; a2a-go sees exactly one "1.0".
func TestVersions(t *testing.T) {
	f := newFixture(t, false)
	get := func(path string, h http.Header) *httptest.ResponseRecorder {
		h.Set("Authorization", "Bearer alice")
		return f.do(req{"GET", path, "", h})
	}
	// Absent: refused before the handler, naming the header.
	n0 := len(f.h.calls)
	w := get("/agents/hello/tasks", hdr())
	if w.Code != 400 || !strings.Contains(w.Body.String(), "VERSION_NOT_SUPPORTED") || !strings.Contains(w.Body.String(), "A2A-Version: 1.0") {
		t.Errorf("REST without A2A-Version: %d %s", w.Code, w.Body)
	}
	w = f.do(req{"POST", "/agents/hello", rpc("GetTask", `{"id":"x"}`, "v"), hdr("Authorization", "Bearer alice")})
	if r := decodeRPC(t, w); w.Code != 200 || r.Error == nil || r.Error.Code != -32009 || !strings.Contains(r.Error.Message, "A2A-Version") || string(r.ID) != `"v"` {
		t.Errorf("JSON-RPC without A2A-Version: %d %s", w.Code, w.Body)
	}
	w = f.do(req{"POST", "/agents/hello", rpc("SendStreamingMessage", msg, "v"), hdr("Authorization", "Bearer alice", "A2A-Version", "0.3")})
	if r := decodeRPC(t, w); r.Error == nil || r.Error.Code != -32009 || w.Header().Get("Content-Type") == "text/event-stream" {
		t.Errorf("a stream at 0.3: %d %s", w.Code, w.Body)
	}
	if len(f.h.calls) != n0 {
		t.Error("a refused version reached the handler")
	}
	for _, c := range []struct {
		h     http.Header
		query string
		ok    bool
	}{
		{hdr("A2A-Version", "1.0"), "", true},
		{hdr("A2A-Version", "1.0.1"), "", true},
		{hdr("A2A-Version", " 1.0.0 "), "", true},
		{hdr("A2A-Version", "1.0", "A2A-Version", "1.0.1"), "", true},
		{hdr(), "?A2A-Version=1.0.1", true},
		{hdr("A2A-Version", ""), "?A2A-Version=1.0", true},
		{hdr("A2A-Version", "0.3"), "?A2A-Version=1.0", false}, // the header wins
		{hdr("A2A-Version", "1.1"), "", false},
		{hdr("A2A-Version", "1.0.x"), "", false},
		{hdr("A2A-Version", "1.0", "A2A-Version", "0.3"), "", false},
		{hdr(), "?A2A-Version=", false},
	} {
		w := get("/agents/hello/tasks"+c.query, c.h)
		got, _ := f.h.last()
		if c.ok && (w.Code != 200 || fmt.Sprint(got.versions) != "[1.0]") {
			t.Errorf("%v %s: %d %s, a2a-go saw %v", c.h, c.query, w.Code, w.Body, got.versions)
		}
		if !c.ok && w.Code != 400 {
			t.Errorf("%v %s: %d, want 400", c.h, c.query, w.Code)
		}
	}
}

// A2A-Extensions: a comma list, repeated headers and the X- alias are merged and deduplicated; URIs the
// card does not declare are dropped; activated URIs are echoed in one header; a missing required URI is
// -32008 before the handler (so before any task row).
func TestExtensions(t *testing.T) {
	f := newFixture(t, false)
	h := v1("alice")
	h.Add("A2A-Extensions", extOptional+", https://undeclared.example/x")
	h.Add("A2A-Extensions", " "+extOptional+" ,")
	h.Add("X-A2A-Extensions", "https://undeclared.example/y,"+extOptional)
	w := f.do(req{"POST", "/agents/hello/message:send", msg, h})
	got, _ := f.h.last()
	if w.Code != 200 || fmt.Sprint(got.extensions) != "["+extOptional+"]" || w.Header().Get("A2A-Extensions") != extOptional {
		t.Errorf("hello: %d, a2a-go saw %v, echoed %v", w.Code, got.extensions, w.Header().Values("A2A-Extensions"))
	}
	// No extension requested: none echoed.
	w = f.do(req{"POST", "/agents/hello/message:send", msg, v1("alice")})
	if len(w.Header().Values("A2A-Extensions")) != 0 {
		t.Errorf("echoed %v", w.Header().Values("A2A-Extensions"))
	}
	// A required extension missing: -32008 on both bindings, before the handler; present (through the
	// alias), accepted and both echoed in one header.
	_, n0 := f.h.last()
	w = f.do(req{"POST", "/agents/strict/message:send", msg, v1("alice", "A2A-Extensions", extOptional)})
	if w.Code != 400 || !strings.Contains(w.Body.String(), "EXTENSION_SUPPORT_REQUIRED") || !strings.Contains(w.Body.String(), extRequired) {
		t.Errorf("REST without the required extension: %d %s", w.Code, w.Body)
	}
	w = f.do(req{"POST", "/agents/strict", rpc("SendStreamingMessage", msg, 4), v1("alice")})
	if r := decodeRPC(t, w); r.Error == nil || r.Error.Code != -32008 || string(r.ID) != "4" {
		t.Errorf("JSON-RPC stream without the required extension: %s", w.Body)
	}
	if _, n := f.h.last(); n != n0 {
		t.Error("a request missing a required extension reached the handler")
	}
	if m, _, _ := f.st.take(); m != "" {
		t.Error("a request missing a required extension reached the SSE writer")
	}
	w = f.do(req{"POST", "/agents/strict/message:send", msg, v1("alice", "X-A2A-Extensions", extRequired+","+extOptional)})
	got, _ = f.h.last()
	if w.Code != 200 || w.Header().Get("A2A-Extensions") != extRequired+", "+extOptional || len(w.Header().Values("A2A-Extensions")) != 1 ||
		fmt.Sprint(got.extensions) != "["+extRequired+" "+extOptional+"]" {
		t.Errorf("strict with its extension: %d %v, a2a-go saw %v", w.Code, w.Header().Values("A2A-Extensions"), got.extensions)
	}
	// The streams see the normalized header too.
	f.do(req{"POST", "/agents/strict", rpc("SubscribeToTask", `{"id":"x"}`, 5), v1("alice", "A2A-Extensions", extRequired+", nope")})
	if _, _, sh := f.st.take(); fmt.Sprint(sh.Values("A2A-Extensions")) != "["+extRequired+"]" {
		t.Errorf("the SSE writer saw %v", sh.Values("A2A-Extensions"))
	}
}

// An Update not accepted in time: -32603 on both bindings, HTTP 503 on HTTP+JSON (spec §7, §14.1); any
// other internal error keeps a2a-go's 500.
func TestUnavailableUpdate(t *testing.T) {
	f := newFixture(t, false)
	f.h.err = fmt.Errorf("failed to create work item: %w", fmt.Errorf("%w: %w", a2a.NewError(a2a.ErrInternalError, "the task's lifecycle is unavailable"), lifecycle.ErrUnavailable))
	w := f.do(req{"POST", "/agents/hello/message:send", msg, v1("alice")})
	if w.Code != 503 || !strings.Contains(w.Body.String(), `"UNAVAILABLE"`) || !strings.Contains(w.Body.String(), "INTERNAL_ERROR") {
		t.Errorf("REST: %d %s", w.Code, w.Body)
	}
	w = f.do(req{"POST", "/agents/hello/message:stream", msg, v1("alice")})
	if w.Code != 503 {
		t.Errorf("REST stream: %d %s", w.Code, w.Body)
	}
	w = f.do(req{"POST", "/agents/hello", rpc("SendMessage", msg, 1), v1("alice")})
	if r := decodeRPC(t, w); w.Code != 200 || r.Error == nil || r.Error.Code != -32603 {
		t.Errorf("JSON-RPC: %d %s", w.Code, w.Body)
	}
	f.h.err = a2a.ErrInternalError
	if w := f.do(req{"POST", "/agents/hello/message:send", msg, v1("alice")}); w.Code != 500 {
		t.Errorf("other internal error: %d", w.Code)
	}
}

// Register mounts the routes on the edge's closed router: every A2A route reaches the server, a JSON-RPC
// route's edge refusals are JSON-RPC errors, and anything else under /agents/ is the router's empty 404.
// Multi-tenant paths carry the tenant.
func TestRegister(t *testing.T) {
	for _, multi := range []bool{false, true} {
		f := newFixture(t, multi)
		rt := edge.NewRouter()
		f.srv.Register(rt)
		svc := "/agents/hello"
		if multi {
			svc = "/agents/default/hello"
		}
		tid := "0192f0b4-0000-7000-8000-000000000001"
		for _, r := range []req{
			{"GET", svc, "", v1("alice")},
			{"HEAD", svc + "/.well-known/agent-card.json", "", v1("alice")},
			{"GET", svc + "/.well-known/agent-card.upstream.json", "", v1("alice")},
			{"POST", svc, rpc("GetTask", `{"id":"x"}`, 1), v1("alice")},
			{"POST", svc + "/", rpc("GetTask", `{"id":"x"}`, 1), v1("alice")},
			{"POST", svc + "/message:send", msg, v1("alice")},
			{"POST", svc + "/message:stream", msg, v1("alice")},
			{"GET", svc + "/tasks", "", v1("alice")},
			{"GET", svc + "/tasks/" + tid, "", v1("alice")},
			{"GET", svc + "/tasks/" + tid + ":subscribe", "", v1("alice")},
			{"POST", svc + "/tasks/" + tid + ":subscribe", "", v1("alice")},
			{"POST", svc + "/tasks/" + tid + ":cancel", "", v1("alice")},
			{"POST", svc + "/tasks/" + tid + "/pushNotificationConfigs", `{"url":"https://x"}`, v1("alice")},
			{"GET", svc + "/tasks/" + tid + "/pushNotificationConfigs", "", v1("alice")},
			{"GET", svc + "/tasks/" + tid + "/pushNotificationConfigs/p", "", v1("alice")},
			{"DELETE", svc + "/tasks/" + tid + "/pushNotificationConfigs/p", "", v1("alice")},
			{"GET", svc + "/extendedAgentCard", "", v1("alice")},
		} {
			hr := httptest.NewRequest(r.method, r.path, strings.NewReader(r.body))
			hr.Header = r.header
			w := httptest.NewRecorder()
			rt.ServeHTTP(w, hr)
			if w.Code == 404 && w.Body.Len() == 0 {
				t.Errorf("multi=%v %s %s did not reach the server", multi, r.method, r.path)
			}
		}
		for _, p := range []string{svc + "/foo", svc + "/tasks/", "/agents/", svc + "/tasks/" + tid + "/x"} {
			w := httptest.NewRecorder()
			rt.ServeHTTP(w, httptest.NewRequest("GET", p, nil))
			if w.Code != 404 || w.Body.Len() != 0 {
				t.Errorf("multi=%v GET %s: %d %q", multi, p, w.Code, w.Body)
			}
		}
		// Behind the edge's middleware, a JSON-RPC route's 413 is a JSON-RPC error.
		h := edge.Middleware(edge.MiddlewareConfig{Limits: edge.Limits{Body: 64}}, rt)
		w := httptest.NewRecorder()
		big := rpc("GetTask", `{"id":"`+strings.Repeat("x", 100)+`"}`, 1)
		h.ServeHTTP(w, httptest.NewRequest("POST", svc+"/", strings.NewReader(big)))
		if w.Code != 413 || !bytes.Contains(w.Body.Bytes(), []byte(`"code":-32600`)) {
			t.Errorf("multi=%v 413: %d %s", multi, w.Code, w.Body)
		}
	}
	// On a multi-tenant gate a single-tenant path names no service.
	f := newFixture(t, true)
	if w := f.do(req{"GET", "/agents/hello", "", v1("alice")}); w.Code != 404 {
		t.Errorf("single-tenant path on a multi-tenant gate: %d", w.Code)
	}
	if w := f.do(req{"GET", "/agents/other/hello", "", v1("alice")}); w.Code != 404 {
		t.Errorf("another tenant's path: %d", w.Code)
	}
}

// The middleware on its own: Versions writes HTTP+JSON errors outside the server's routing.
func TestMiddlewareStandalone(t *testing.T) {
	var seen http.Header
	h := Versions(Extensions(func(*http.Request) *ExtensionSet { return NewExtensionSet([]string{extOptional}, nil) },
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen = r.Header.Clone() })))
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/?A2A-Version=1.0.7", nil)
	r.Header.Set("X-A2A-Extensions", extOptional)
	h.ServeHTTP(w, r)
	if w.Code != 200 || seen.Get("A2A-Version") != "1.0" || seen.Get("A2A-Extensions") != extOptional || seen.Get("X-A2A-Extensions") != "" {
		t.Errorf("%d %v", w.Code, seen)
	}
	if r.Header.Get("A2A-Version") != "" || r.Header.Get("X-A2A-Extensions") == "" {
		t.Error("the middleware changed the caller's request")
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 400 {
		t.Errorf("no version: %d", w.Code)
	}
}

// A2A-Extensions costs the gate time linear in the header: a 1 MiB header of distinct URIs (the most
// net/http accepts) against a card declaring thousands of extensions is answered at once, never with a
// scan of the request's URIs or the card's per URI (security: one request burned minutes of CPU). Only the
// first MaxRequestedExtensions distinct URIs count; a declared URI past them is ignored like an undeclared
// one, and a required URI the request misses is still refused (-32008), naming a bounded few.
func TestExtensionsHeaderIsLinear(t *testing.T) {
	var all, required []string
	for i := range 20000 {
		u := fmt.Sprintf("urn:x:%d", i)
		all = append(all, u)
		if i%2 == 1 {
			required = append(required, u)
		}
	}
	set := NewExtensionSet(all, required)
	header := distinctURIs((http.DefaultMaxHeaderBytes - 4096) / 4)
	var seen http.Header
	for name, tc := range map[string]struct {
		set    *ExtensionSet
		header string
		want   int
	}{
		"1 MiB of undeclared URIs, nothing required": {NewExtensionSet(all, nil), header, 200},
		"1 MiB of undeclared URIs, 10000 required":   {set, header, 400},
		"the first required URI after 1 MiB":         {set, header + "urn:x:1", 400},
	} {
		h := Extensions(func(*http.Request) *ExtensionSet { return tc.set },
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen = r.Header.Clone() }))
		r := httptest.NewRequest("POST", "/", nil)
		r.Header.Set("A2A-Extensions", tc.header)
		r.Header.Set("X-A2A-Extensions", tc.header)
		w := httptest.NewRecorder()
		start := time.Now()
		h.ServeHTTP(w, r)
		if d := time.Since(start); d > 2*time.Second {
			t.Errorf("%s: %v for one request", name, d)
		}
		if w.Code != tc.want {
			t.Errorf("%s: %d, want %d", name, w.Code, tc.want)
		}
		if w.Code == 400 && (!strings.Contains(w.Body.String(), "requires the extensions urn:x:1, ") || w.Body.Len() > 4096) {
			t.Errorf("%s: %d bytes: %.300s", name, w.Body.Len(), w.Body.String())
		}
	}
	// A short list still activates what the card declares, in the request's order, deduplicated.
	r := httptest.NewRequest("POST", "/", nil)
	r.Header.Add("A2A-Extensions", "urn:x:3, urn:x:2,urn:x:3")
	r.Header.Add("X-A2A-Extensions", "urn:nope, urn:x:2")
	w := httptest.NewRecorder()
	Extensions(func(*http.Request) *ExtensionSet { return NewExtensionSet(all, []string{"urn:x:2"}) },
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen = r.Header.Clone() })).ServeHTTP(w, r)
	if w.Code != 200 || !slices.Equal(seen.Values("A2A-Extensions"), []string{"urn:x:3", "urn:x:2"}) || w.Header().Get("A2A-Extensions") != "urn:x:3, urn:x:2" {
		t.Errorf("%d %v %q", w.Code, seen.Values("A2A-Extensions"), w.Header().Get("A2A-Extensions"))
	}
	if got := splitExtensions([]string{header}); len(got) != MaxRequestedExtensions {
		t.Errorf("%d URIs considered, want %d", len(got), MaxRequestedExtensions)
	}
}

// distinctURIs is a header value of n distinct 3-byte URIs, comma-separated (4 bytes each).
func distinctURIs(n int) string {
	const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz_-"
	var b strings.Builder
	for i := range n {
		b.WriteByte(alphabet[i%64])
		b.WriteByte(alphabet[i/64%64])
		b.WriteByte(alphabet[i/4096%64])
		b.WriteByte(',')
	}
	return b.String()
}

// Through net/http, as the reviewer measured it: one request with a 256 KiB header of distinct 3-byte
// URIs took 14 s of CPU before the fix.
func TestExtensionsHeaderOverHTTP(t *testing.T) {
	var b strings.Builder
	b.WriteString(distinctURIs(65536))
	srv := httptest.NewServer(Extensions(func(*http.Request) *ExtensionSet { return NewExtensionSet([]string{"urn:a"}, nil) },
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })))
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL, nil)
	req.Header.Set("A2A-Extensions", b.String())
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if d := time.Since(start); d > 2*time.Second || resp.StatusCode != 204 {
		t.Errorf("%d after %v", resp.StatusCode, d)
	}
}

// a2a-go's answers that are not streams (GetTask, ListTasks, a blocking SendMessage) get a write deadline
// from their first byte: a caller that stops reading frees the handler and the answer it holds after it.
// A handler that waits long before it answers (a blocking SendMessage) is not cut by it.
func TestBoundWritesEndsAStalledAnswer(t *testing.T) {
	done := make(chan struct{})
	big := bytes.Repeat([]byte("x"), 64<<20)
	srv := httptest.NewServer(boundWrites(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(big)
	}), 300*time.Millisecond))
	defer srv.Close()
	c, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.(*net.TCPConn).SetReadBuffer(4096)
	if _, err := io.WriteString(c, "GET / HTTP/1.1\r\nHost: gate\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("an answer to a caller that stopped reading never ended")
	}

	slow := httptest.NewServer(boundWrites(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(time.Second) // the task works; the deadline is not armed yet
		_, _ = io.WriteString(w, "answer")
	}), 300*time.Millisecond))
	defer slow.Close()
	resp, err := http.Get(slow.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "answer" {
		t.Errorf("a late answer: %q", body)
	}
	// The next request on the same connection starts without the first one's deadline.
	time.Sleep(400 * time.Millisecond)
	resp, err = http.Get(slow.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "answer" {
		t.Errorf("the second answer on a kept-alive connection: %q", body)
	}
}
