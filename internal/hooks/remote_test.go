package hooks

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
)

const testSecret = "Zq8f2KxL0pW4n7Rt-M3vB9cY6hJ1dE5sQ0"

// hookDouble is an Admission server: it records what it was sent and answers with a status and a body.
type hookDouble struct {
	mu     sync.Mutex
	status int
	body   string
	delay  time.Duration
	reqs   []recorded
	srv    *httptest.Server
}

type recorded struct {
	method, path, auth, contentType string
	body                            []byte
}

func newHook(t *testing.T) *hookDouble {
	t.Helper()
	h := &hookDouble{status: 200, body: `{"outcome":"allow"}`}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.reqs = append(h.reqs, recorded{r.Method, r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Content-Type"), b})
		status, body, delay := h.status, h.body, h.delay
		h.mu.Unlock()
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *hookDouble) answer(status int, body string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.status, h.body = status, body
}

func (h *hookDouble) requests() []recorded {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]recorded(nil), h.reqs...)
}

func (h *hookDouble) remote(t *testing.T, mod func(*RemoteAdmissionConfig)) *RemoteAdmission {
	t.Helper()
	cfg := RemoteAdmissionConfig{URL: h.srv.URL + "/admit", Secret: testSecret, Timeout: 2 * time.Second,
		Operations: []Operation{OpTask, OpSession, OpCall}, Rules: KeyRules{OperatorPrefixes: []string{"kontra.escrow."}}}
	if mod != nil {
		mod(&cfg)
	}
	r, err := NewRemoteAdmission(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func taskRequest() AdmitRequest {
	return AdmitRequest{
		Operation: OpTask, TaskID: "0199a3b2-7c4e-7d10-8f00-2b9a6e1c0d11",
		Service:   Service{TenantID: "0199a3b2-0000-7000-8000-000000000001", Tenant: "default", Name: "hello", Kind: "a2a"},
		Principal: Principal{Kind: "key", ID: "key:kk_test", BuyerID: "0199a3b2-0000-7000-8000-0000000000bb", Via: "api_key"},
		Protocol:  "a2a", ContextID: "ctx-1",
		Message: &MessageInfo{MessageID: "m-1", Parts: []PartInfo{{MediaType: "text/plain", Size: 2}}},
	}
}

func future() string { return time.Now().Add(time.Hour).UTC().Format(time.RFC3339) }

// allow, deny with the operator's refusal, and quote are read as the hook answered them; the request is the
// AdmitRequest in hooks.json's camelCase, POSTed with the bearer secret.
func TestRemoteAdmissionDecisions(t *testing.T) {
	h := newHook(t)
	r := h.remote(t, nil)
	ctx := context.Background()

	h.answer(200, `{"outcome":"allow","metadata":{"kontra.escrow.note":"ok"},"mcpMeta":{"sh.kontra/price":"1 EUR"}}`)
	d, err := r.Admit(ctx, taskRequest())
	if err != nil || d.Outcome != Allow || d.Metadata["kontra.escrow.note"] != "ok" || d.MCPMeta["sh.kontra/price"] != "1 EUR" {
		t.Fatalf("allow: %+v, %v", d, err)
	}
	reqs := h.requests()
	if len(reqs) != 1 {
		t.Fatalf("%d requests", len(reqs))
	}
	got := reqs[0]
	if got.method != http.MethodPost || got.path != "/admit" || got.auth != "Bearer "+testSecret || got.contentType != "application/json" {
		t.Errorf("request %s %s, Authorization %q, Content-Type %q", got.method, got.path, got.auth, got.contentType)
	}
	var sent map[string]any
	if err := json.Unmarshal(got.body, &sent); err != nil {
		t.Fatal(err)
	}
	svc, _ := sent["service"].(map[string]any)
	pr, _ := sent["principal"].(map[string]any)
	if sent["operation"] != "task" || sent["taskId"] != "0199a3b2-7c4e-7d10-8f00-2b9a6e1c0d11" || sent["protocol"] != "a2a" ||
		svc["tenantId"] != "0199a3b2-0000-7000-8000-000000000001" || pr["buyerId"] != "0199a3b2-0000-7000-8000-0000000000bb" ||
		pr["via"] != "api_key" || sent["contextId"] != "ctx-1" {
		t.Errorf("the hook was sent %s", got.body)
	}

	h.answer(200, `{"outcome":"deny","refusal":{"code":-32602,"message":"over budget"}}`)
	d, err = r.Admit(ctx, taskRequest())
	if err != nil || d.Outcome != Deny || d.Refusal == nil || d.Refusal.Code != -32602 || d.Refusal.Message != "over budget" {
		t.Errorf("deny: %+v, %v", d, err)
	}

	approve := "https://app.kontra.sh/#/t/0199a3b2-7c4e-7d10-8f00-2b9a6e1c0d11"
	exp := future()
	h.answer(200, fmt.Sprintf(`{"outcome":"quote","metadata":{"kontra.escrow.quote":{"amount":"15.00"}},"quote":{"approveUrl":%q,"expiresAt":%q}}`, approve, exp))
	d, err = r.Admit(ctx, taskRequest())
	if err != nil || d.Outcome != Quote || d.Quote == nil || d.Quote.ApproveURL != approve || d.Quote.ExpiresAt != exp {
		t.Errorf("quote: %+v, %v", d, err)
	}
	// A team gate's own approve page is passed through as well (D188).
	team := "https://gate.team.example/approve/0199a3b2-7c4e-7d10-8f00-2b9a6e1c0d11?x=1"
	h.answer(200, fmt.Sprintf(`{"outcome":"quote","quote":{"approveUrl":%q,"expiresAt":%q}}`, team, exp))
	if d, err = r.Admit(ctx, taskRequest()); err != nil || d.Quote == nil || d.Quote.ApproveURL != team {
		t.Errorf("a team gate's quote: %+v, %v", d, err)
	}
}

// Every way the hook can fail is ErrAdmissionUnavailable (the callers' -32603 "admission unavailable", 503): a
// timeout, a non-2xx, a 3xx (not followed), an unparsable or invalid answer, a metadata key outside the operator
// prefixes, an mcpMeta key outside sh.kontra/, a map over 16 KiB, an answer over 64 KiB, a malformed or expired quote.
// None of the errors carries the secret.
func TestRemoteAdmissionFailsClosed(t *testing.T) {
	h := newHook(t)
	var targetHits atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		io.WriteString(w, `{"outcome":"allow"}`)
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()

	big := strings.Repeat("x", 17<<10)
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"500", 500, `{"outcome":"allow"}`},
		{"404", 404, `{"outcome":"allow"}`},
		{"401", 401, ``},
		{"204", 204, ``},
		{"not json", 200, `allow`},
		{"empty", 200, ``},
		{"an array", 200, `[{"outcome":"allow"}]`},
		{"two values", 200, `{"outcome":"allow"}{"outcome":"allow"}`},
		{"unknown outcome", 200, `{"outcome":"maybe"}`},
		{"no outcome", 200, `{"metadata":{}}`},
		{"outcome not a string", 200, `{"outcome":true}`},
		{"case-variant member", 200, `{"Outcome":"allow"}`},
		{"repeated member", 200, `{"outcome":"deny","outcome":"allow"}`},
		{"case-variant pair", 200, `{"outcome":"allow","OUTCOME":"allow"}`},
		{"unknown member", 200, `{"outcome":"allow","approve":true}`},
		{"deny without refusal", 200, `{"outcome":"deny"}`},
		{"refusal without code", 200, `{"outcome":"deny","refusal":{"message":"no"}}`},
		{"refusal code not an integer", 200, `{"outcome":"deny","refusal":{"code":1.5,"message":"no"}}`},
		{"refusal with an extra member", 200, `{"outcome":"deny","refusal":{"code":1,"message":"no","http":403}}`},
		{"quote without terms", 200, `{"outcome":"quote"}`},
		{"quote terms not an object", 200, `{"outcome":"quote","quote":"https://x.example"}`},
		{"quote without expiresAt", 200, `{"outcome":"quote","quote":{"approveUrl":"https://app.kontra.sh/#/t/1"}}`},
		{"quote with an extra member", 200, `{"outcome":"quote","quote":{"approveUrl":"https://app.kontra.sh/#/t/1","expiresAt":"` + future() + `","amount":1}}`},
		{"metadata not an object", 200, `{"outcome":"allow","metadata":[1]}`},
		{"metadata null", 200, `{"outcome":"allow","metadata":null}`},
		{"metadata key outside the prefixes", 200, `{"outcome":"allow","metadata":{"kontra.approve.url":"https://evil.example"}}`},
		{"metadata repeated key", 200, `{"outcome":"allow","metadata":{"kontra.escrow.a":1,"kontra.escrow.a":2}}`},
		{"mcpMeta key outside sh.kontra/", 200, `{"outcome":"allow","mcpMeta":{"price":"1"}}`},
		{"metadata over 16 KiB", 200, `{"outcome":"allow","metadata":{"kontra.escrow.blob":"` + big + `"}}`},
		{"mcpMeta over 16 KiB", 200, `{"outcome":"allow","mcpMeta":{"sh.kontra/blob":"` + big + `"}}`},
		{"answer over 64 KiB", 200, `{"outcome":"allow","metadata":{"kontra.escrow.a":"` + strings.Repeat("y", 65<<10) + `"}}`},
		{"approveUrl http", 200, `{"outcome":"quote","quote":{"approveUrl":"http://app.kontra.sh/#/t/1","expiresAt":"` + future() + `"}}`},
		{"approveUrl upper-case scheme", 200, `{"outcome":"quote","quote":{"approveUrl":"HTTPS://app.kontra.sh/#/t/1","expiresAt":"` + future() + `"}}`},
		{"approveUrl relative", 200, `{"outcome":"quote","quote":{"approveUrl":"/#/t/1","expiresAt":"` + future() + `"}}`},
		{"approveUrl without a host", 200, `{"outcome":"quote","quote":{"approveUrl":"https:///t/1","expiresAt":"` + future() + `"}}`},
		{"approveUrl with userinfo", 200, `{"outcome":"quote","quote":{"approveUrl":"https://u:p@app.kontra.sh/#/t/1","expiresAt":"` + future() + `"}}`},
		{"approveUrl with a space", 200, `{"outcome":"quote","quote":{"approveUrl":"https://app.kontra.sh/t 1","expiresAt":"` + future() + `"}}`},
		{"approveUrl javascript", 200, `{"outcome":"quote","quote":{"approveUrl":"javascript:alert(1)","expiresAt":"` + future() + `"}}`},
		{"expiresAt unparsable", 200, `{"outcome":"quote","quote":{"approveUrl":"https://app.kontra.sh/#/t/1","expiresAt":"soon"}}`},
		{"expiresAt without a zone", 200, `{"outcome":"quote","quote":{"approveUrl":"https://app.kontra.sh/#/t/1","expiresAt":"2030-01-01T00:00:00"}}`},
		{"expiresAt past", 200, `{"outcome":"quote","quote":{"approveUrl":"https://app.kontra.sh/#/t/1","expiresAt":"2020-01-01T00:00:00Z"}}`},
		{"expiresAt empty", 200, `{"outcome":"quote","quote":{"approveUrl":"https://app.kontra.sh/#/t/1","expiresAt":""}}`},
		{"a key quoting the secret", 200, `{"outcome":"allow","` + testSecret + `":1}`},
		{"a metadata key quoting the secret", 200, `{"outcome":"allow","metadata":{"` + testSecret[3:] + `":1}}`},
	}
	r := h.remote(t, nil)
	for _, c := range cases {
		h.answer(c.status, c.body)
		d, err := r.Admit(context.Background(), taskRequest())
		if !errors.Is(err, ErrAdmissionUnavailable) {
			t.Errorf("%s: %+v, %v; want ErrAdmissionUnavailable", c.name, d, err)
			continue
		}
		if strings.Contains(err.Error(), testSecret[:8]) || strings.Contains(err.Error(), testSecret[10:18]) {
			t.Errorf("%s: the error quotes the secret: %v", c.name, err)
		}
	}

	// A 3xx is a failure, and its target receives nothing.
	rr, err := NewRemoteAdmission(RemoteAdmissionConfig{URL: redirect.URL + "/admit", Secret: testSecret})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rr.Admit(context.Background(), taskRequest()); !errors.Is(err, ErrAdmissionUnavailable) || !strings.Contains(err.Error(), "307") {
		t.Errorf("a redirecting hook: %v", err)
	}
	if targetHits.Load() != 0 {
		t.Errorf("the redirect was followed (%d requests)", targetHits.Load())
	}
	// A client of the caller's own that would follow redirects follows none either.
	rr, err = NewRemoteAdmission(RemoteAdmissionConfig{URL: redirect.URL + "/admit", Secret: testSecret, Client: &http.Client{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rr.Admit(context.Background(), taskRequest()); !errors.Is(err, ErrAdmissionUnavailable) || targetHits.Load() != 0 {
		t.Errorf("a caller's client: %v, target hits %d", err, targetHits.Load())
	}

	// A hook that does not answer within the timeout.
	h.answer(200, `{"outcome":"allow"}`)
	h.mu.Lock()
	h.delay = 3 * time.Second
	h.mu.Unlock()
	slow := h.remote(t, func(c *RemoteAdmissionConfig) { c.Timeout = 150 * time.Millisecond })
	start := time.Now()
	if _, err := slow.Admit(context.Background(), taskRequest()); !errors.Is(err, ErrAdmissionUnavailable) || time.Since(start) > 2*time.Second {
		t.Errorf("a slow hook: %v after %v", err, time.Since(start))
	}
	h.mu.Lock()
	h.delay = 0
	h.mu.Unlock()

	// A hook that is down.
	down := httptest.NewServer(http.NotFoundHandler())
	downURL := down.URL
	down.Close()
	rd, err := NewRemoteAdmission(RemoteAdmissionConfig{URL: downURL + "/admit", Secret: testSecret})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rd.Admit(context.Background(), taskRequest()); !errors.Is(err, ErrAdmissionUnavailable) || strings.Contains(err.Error(), testSecret) {
		t.Errorf("a hook that is down: %v", err)
	}
}

// A quote is valid only for operation task (spec §13.1): for call and session it is the deny "priced tasks need
// tasks: durable", -32603.
func TestRemoteAdmissionQuoteOutsideTask(t *testing.T) {
	h := newHook(t)
	r := h.remote(t, nil)
	h.answer(200, `{"outcome":"quote","quote":{"approveUrl":"https://app.kontra.sh/#/t/1","expiresAt":"`+future()+`"}}`)
	for _, op := range []Operation{OpCall, OpSession} {
		req := taskRequest()
		req.Operation, req.TaskID, req.Protocol, req.Tool = op, "", "mcp", "echo"
		d, err := r.Admit(context.Background(), req)
		if err != nil || d.Outcome != Deny || d.Refusal == nil || d.Refusal.Code != -32603 || d.Refusal.Message != "priced tasks need tasks: durable" || d.Quote != nil {
			t.Errorf("a quote for %s: %+v, %v", op, d, err)
		}
	}
}

// An operation the hook is not asked about is answered as the built-in answers it, with no request.
func TestRemoteAdmissionOperations(t *testing.T) {
	h := newHook(t)
	h.answer(200, `{"outcome":"deny","refusal":{"code":-32001,"message":"no"}}`)
	r := h.remote(t, func(c *RemoteAdmissionConfig) { c.Operations = nil })
	if !r.Consults(OpTask) || r.Consults(OpCall) || r.Consults(OpSession) {
		t.Errorf("default operations %v, want [task]", r.Operations())
	}
	req := taskRequest()
	req.Operation, req.Tool = OpCall, "echo"
	if d, err := r.Admit(context.Background(), req); err != nil || d.Outcome != Allow {
		t.Errorf("a call the hook is not asked about: %+v, %v", d, err)
	}
	if n := len(h.requests()); n != 0 {
		t.Errorf("the hook received %d requests for an operation it is not asked about", n)
	}
	if d, err := r.Admit(context.Background(), taskRequest()); err != nil || d.Outcome != Deny {
		t.Errorf("a task: %+v, %v", d, err)
	}
	if _, err := r.Admit(context.Background(), AdmitRequest{}); err == nil {
		t.Error("a request without an operation was admitted")
	}
}

// NewRemoteAdmission refuses http:// off loopback, userinfo, an empty secret or one with white space, and an unknown
// operation; it accepts https:// anywhere and http:// on loopback. No error and no rendering of the configuration
// carries the secret.
func TestNewRemoteAdmission(t *testing.T) {
	for _, bad := range []RemoteAdmissionConfig{
		{URL: "http://cp.example.com/admit", Secret: testSecret},
		{URL: "http://10.0.0.7/admit", Secret: testSecret},
		{URL: "ftp://cp.example.com/admit", Secret: testSecret},
		{URL: "https://admin:" + testSecret + "@cp.example.com/admit", Secret: testSecret},
		{URL: "https://cp.example.com/admit#frag", Secret: testSecret},
		{URL: "/admit", Secret: testSecret},
		{URL: "https://cp.example.com/admit", Secret: ""},
		{URL: "https://cp.example.com/admit", Secret: testSecret + "\n"},
		{URL: "https://cp.example.com/admit", Secret: testSecret, Operations: []Operation{"settle"}},
		{URL: "https://cp.example.com/admit", Secret: testSecret, Timeout: -time.Second},
	} {
		_, err := NewRemoteAdmission(bad)
		if err == nil {
			t.Errorf("NewRemoteAdmission(%v) accepted", bad)
			continue
		}
		if strings.Contains(err.Error(), testSecret[:8]) {
			t.Errorf("the error quotes the secret: %v", err)
		}
	}
	for _, ok := range []string{"https://cp.example.com/admit", "https://10.0.0.7:8443/admit", "http://127.0.0.1:7000/admit",
		"http://localhost:7000/admit", "http://[::1]:7000/admit"} {
		r, err := NewRemoteAdmission(RemoteAdmissionConfig{URL: ok, Secret: testSecret})
		if err != nil {
			t.Errorf("NewRemoteAdmission(%s): %v", ok, err)
			continue
		}
		if r.timeout != 5*time.Second {
			t.Errorf("default timeout %v", r.timeout)
		}
		if s := r.String(); strings.Contains(s, testSecret) {
			t.Errorf("String() shows the secret: %s", s)
		}
	}
	cfg := RemoteAdmissionConfig{URL: "https://cp.example.com/admit", Secret: testSecret, Operations: []Operation{OpTask}}
	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Info("admission", "cfg", cfg)
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("admission", "cfg", cfg)
	for _, s := range []string{fmt.Sprint(cfg), fmt.Sprintf("%v %+v %#v %s", cfg, cfg, cfg, cfg), buf.String()} {
		if strings.Contains(s, testSecret) || !strings.Contains(s, "redacted") {
			t.Errorf("a rendering of the configuration: %s", s)
		}
	}
}

// ParseDecision drops what an outcome does not carry, and CheckQuote takes the quotes D188's thread URL and a team
// gate's page are.
func TestParseDecisionAndCheckQuote(t *testing.T) {
	d, err := ParseDecision([]byte(`{"outcome":"allow","refusal":{"code":1,"message":"x"},"quote":{"approveUrl":"https://a.example","expiresAt":"x"}}`))
	if err != nil || d.Outcome != Allow || d.Refusal != nil || d.Quote != nil {
		t.Errorf("allow with extras: %+v, %v", d, err)
	}
	d, err = ParseDecision([]byte(` {"outcome": "deny", "refusal": {"code": -32001, "message": "no"}, "quote": {"approveUrl": "https://a.example", "expiresAt": "x"}} `))
	if err != nil || d.Outcome != Deny || d.Quote != nil || d.Refusal.Code != -32001 {
		t.Errorf("deny with a quote: %+v, %v", d, err)
	}
	b, err := json.Marshal(map[string]any{"outcome": "quote", "quote": map[string]any{"approveUrl": "https://app.kontra.sh/#/t/x", "expiresAt": "2030-01-01T00:00:00+02:00"}})
	if err != nil {
		t.Fatal(err)
	}
	if d, err = ParseDecision(b); err != nil || d.Quote == nil {
		t.Fatalf("quote: %+v, %v", d, err)
	}
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	for _, u := range []string{"https://app.kontra.sh/#/t/0199a3b2-7c4e-7d10-8f00-2b9a6e1c0d11", "https://gate.team.example:8443/approve/1?a=b",
		"https://10.0.0.7/approve"} {
		if err := CheckQuote(&QuoteTerms{ApproveURL: u, ExpiresAt: "2026-10-28T12:00:01Z"}, now); err != nil {
			t.Errorf("CheckQuote(%s): %v", u, err)
		}
	}
	for _, q := range []*QuoteTerms{nil, {ApproveURL: "https://a.example", ExpiresAt: "2026-10-28T12:00:00Z"},
		{ApproveURL: "https://a.example", ExpiresAt: "2026-10-28T11:00:00Z"}, {ApproveURL: "https://a.example:99999/", ExpiresAt: "2027-01-01T00:00:00Z"},
		{ApproveURL: "https://ä.example/", ExpiresAt: "2027-01-01T00:00:00Z"}} {
		if err := CheckQuote(q, now); !errors.Is(err, ErrBadQuote) {
			t.Errorf("CheckQuote(%+v) = %v, want ErrBadQuote", q, err)
		}
	}
}

// An error that does not quote the secret keeps its detail; one that might is withheld.
func TestUnavailableWithholdsOnlyWhatMayQuoteTheSecret(t *testing.T) {
	r := &RemoteAdmission{secret: testSecret}
	if err := r.unavailable("the answer", errors.New(`outcome "maybe" is not allow, deny or quote`)); !strings.Contains(err.Error(), `"maybe"`) {
		t.Errorf("an ordinary cause lost its detail: %v", err)
	}
	if err := r.unavailable("the answer", errors.New(`unknown member "`+testSecret[5:30]+`…"`)); strings.Contains(err.Error(), testSecret[5:13]) ||
		!errors.Is(err, ErrAdmissionUnavailable) {
		t.Errorf("a cause quoting part of the secret: %v", err)
	}
}
