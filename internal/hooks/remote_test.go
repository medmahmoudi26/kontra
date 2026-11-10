package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strconv"
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
		Operations: []Operation{OpTask, OpSession, OpCall}, Rules: KeyRules{OperatorPrefixes: []string{"kontra.op."}}}
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

// allow and deny with the operator's refusal are read as the hook answered them; the request is the
// AdmitRequest in hooks.json's camelCase, POSTed with the bearer secret.
func TestRemoteAdmissionDecisions(t *testing.T) {
	h := newHook(t)
	r := h.remote(t, nil)
	ctx := context.Background()

	h.answer(200, `{"outcome":"allow","metadata":{"kontra.op.note":"ok"},"mcpMeta":{"sh.kontra/note":"1 EUR"}}`)
	d, err := r.Admit(ctx, taskRequest())
	if err != nil || d.Outcome != Allow || d.Metadata["kontra.op.note"] != "ok" || d.MCPMeta["sh.kontra/note"] != "1 EUR" {
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
}

// Every way the hook can fail is ErrAdmissionUnavailable (the callers' -32603 "admission unavailable", 503): a
// timeout, a non-2xx, a 3xx (not followed), an unparsable or invalid answer, a metadata key outside the operator
// prefixes, an mcpMeta key outside sh.kontra/, a map over 16 KiB, an answer over 64 KiB, the retired outcome quote (D351).
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
		{"quote (retired, D351)", 200, `{"outcome":"quote","quote":{"approveUrl":"https://app.kontra.sh/#/t/1","expiresAt":"` + future() + `"}}`},
		{"deny without refusal", 200, `{"outcome":"deny"}`},
		{"refusal without code", 200, `{"outcome":"deny","refusal":{"message":"no"}}`},
		{"refusal code not an integer", 200, `{"outcome":"deny","refusal":{"code":1.5,"message":"no"}}`},
		// json.Unmarshal leaves an int untouched for null, which read as code 0 and acted on the operator's refusal.
		{"refusal code null", 200, `{"outcome":"deny","refusal":{"code":null,"message":"over budget"}}`},
		{"refusal code a string", 200, `{"outcome":"deny","refusal":{"code":"403","message":"no"}}`},
		{"refusal code beyond 2^53", 200, `{"outcome":"deny","refusal":{"code":1e300,"message":"no"}}`},
		{"refusal message null", 200, `{"outcome":"deny","refusal":{"code":1,"message":null}}`},
		{"refusal with an extra member", 200, `{"outcome":"deny","refusal":{"code":1,"message":"no","http":403}}`},
		{"metadata not an object", 200, `{"outcome":"allow","metadata":[1]}`},
		{"metadata null", 200, `{"outcome":"allow","metadata":null}`},
		{"metadata key outside the prefixes", 200, `{"outcome":"allow","metadata":{"kontra.other":"https://evil.example"}}`},
		{"metadata repeated key", 200, `{"outcome":"allow","metadata":{"kontra.op.a":1,"kontra.op.a":2}}`},
		{"mcpMeta key outside sh.kontra/", 200, `{"outcome":"allow","mcpMeta":{"price":"1"}}`},
		{"metadata over 16 KiB", 200, `{"outcome":"allow","metadata":{"kontra.op.blob":"` + big + `"}}`},
		{"mcpMeta over 16 KiB", 200, `{"outcome":"allow","mcpMeta":{"sh.kontra/blob":"` + big + `"}}`},
		{"answer over 64 KiB", 200, `{"outcome":"allow","metadata":{"kontra.op.a":"` + strings.Repeat("y", 65<<10) + `"}}`},
		// What time.Parse(time.RFC3339) takes and RFC 3339 (format date-time) does not.
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
		// Past the MCP relay's 30 s bound on a shared session decision (MaxAdmissionTimeout).
		{URL: "https://cp.example.com/admit", Secret: testSecret, Timeout: 30*time.Second + time.Millisecond},
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
	if r, err := NewRemoteAdmission(RemoteAdmissionConfig{URL: "https://cp.example.com/admit", Secret: testSecret, Timeout: MaxAdmissionTimeout}); err != nil || r.timeout != 30*time.Second {
		t.Errorf("a 30 s timeout: %v, %v", r, err)
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

// ParseDecision drops what an outcome does not carry, and refuses the retired member quote (D351).
func TestParseDecision(t *testing.T) {
	d, err := ParseDecision([]byte(`{"outcome":"allow","refusal":{"code":1,"message":"x"}}`))
	if err != nil || d.Outcome != Allow || d.Refusal != nil {
		t.Errorf("allow with a refusal: %+v, %v", d, err)
	}
	d, err = ParseDecision([]byte(` {"outcome": "deny", "refusal": {"code": -32001, "message": "no"}} `))
	if err != nil || d.Outcome != Deny || d.Refusal.Code != -32001 {
		t.Errorf("deny: %+v, %v", d, err)
	}
	for _, raw := range []string{`{"outcome":"quote","quote":{"approveUrl":"https://a.example","expiresAt":"2030-01-01T00:00:00Z"}}`,
		`{"outcome":"allow","quote":{"approveUrl":"https://a.example","expiresAt":"x"}}`} {
		if d, err := ParseDecision([]byte(raw)); !errors.Is(err, ErrInvalidDecision) {
			t.Errorf("%s: %+v, %v; want ErrInvalidDecision", raw, d, err)
		}
	}
	// hooks.json's code is a JSON Schema 2020-12 integer: a number with no fractional part, however it is written.
	for raw, want := range map[string]int{"-32001.0": -32001, "1e2": 100, "2.50E+1": 25, "10e-1": 1, "-0": 0, "0.0e9": 0,
		" 403 ": 403, "9007199254740993.0": 9007199254740993, "-9223372036854775808": math.MinInt64} {
		d, err := ParseDecision([]byte(`{"outcome":"deny","refusal":{"code":` + raw + `,"message":"no"}}`))
		if err != nil || d.Refusal == nil || d.Refusal.Code != want {
			t.Errorf("refusal code %s: %+v, %v; want %d", raw, d, err, want)
		}
	}
	for _, raw := range []string{"null", "1.5", "1e-1", "1.05e1", "1.0000000000000000001", `"1"`, "true", "{}", "[1]", "1e300",
		"9223372036854775808", "1e99999999999999999999"} {
		if d, err := ParseDecision([]byte(`{"outcome":"deny","refusal":{"code":` + raw + `,"message":"no"}}`)); !errors.Is(err, ErrInvalidDecision) {
			t.Errorf("refusal code %s: %+v, %v; want ErrInvalidDecision", raw, d, err)
		}
	}
}

// An error that does not quote the secret keeps its detail; one that might is withheld.
func TestUnavailableWithholdsOnlyWhatMayQuoteTheSecret(t *testing.T) {
	r := &RemoteAdmission{secret: testSecret}
	if err := r.unavailable("the answer", errors.New(`outcome "maybe" is not allow or deny`)); !strings.Contains(err.Error(), `"maybe"`) {
		t.Errorf("an ordinary cause lost its detail: %v", err)
	}
	if err := r.unavailable("the answer", errors.New(`unknown member "`+testSecret[5:30]+`…"`)); strings.Contains(err.Error(), testSecret[5:13]) ||
		!errors.Is(err, ErrAdmissionUnavailable) {
		t.Errorf("a cause quoting part of the secret: %v", err)
	}

	// A secret holding characters an error escapes (" and \), which a hook echoes into a member name, a metadata or an
	// mcpMeta key: the errors quote those Go-escaped (strictjson.Quote, %q), so its raw windows never match, and the
	// escaped forms are withheld too. An escape every 4 bytes leaves no 8 raw bytes of it in an escaped quote.
	const escSecret = `Zq8"f2K\L0p"W4n\7Rt"M3v\B9c"Y6h\`
	h := newHook(t)
	er := h.remote(t, func(c *RemoteAdmissionConfig) { c.Secret = escSecret })
	key, err := json.Marshal(escSecret)
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"a member":         `{"outcome":"allow",` + string(key) + `:1}`,
		"a refusal member": `{"outcome":"deny","refusal":{"code":1,"message":"no",` + string(key) + `:1}}`,
		"a quote member":   `{"outcome":"quote","quote":{"approveUrl":"https://a.example/","expiresAt":"` + future() + `",` + string(key) + `:1}}`,
		"a metadata key":   `{"outcome":"allow","metadata":{` + string(key) + `:1}}`,
		"an mcpMeta key":   `{"outcome":"allow","mcpMeta":{` + string(key) + `:1}}`,
	} {
		h.answer(200, body)
		_, err := er.Admit(context.Background(), taskRequest())
		if !errors.Is(err, ErrAdmissionUnavailable) {
			t.Errorf("%s quoting the secret: %v, want ErrAdmissionUnavailable", name, err)
			continue
		}
		if showsSecret(err.Error(), escSecret) {
			t.Errorf("%s quoting the secret: the error shows it: %v", name, err)
		}
	}
	if err := er.unavailable("the answer", fmt.Errorf("member %+q", "é"+escSecret)); showsSecret(err.Error(), escSecret) {
		t.Errorf("an ASCII-quoted cause shows the secret: %v", err)
	}

	// Nor does any rendering of the hook itself: String, GoString (%#v) and LogValue (slog) leave the secret out.
	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Info("admission", "hook", er)
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("admission", "hook", er)
	for _, s := range []string{fmt.Sprintf("%v|%+v|%s", er, er, er), fmt.Sprintf("%#v", er), buf.String()} {
		if showsSecret(s, escSecret) || !strings.Contains(s, "redacted") {
			t.Errorf("a rendering of the hook: %s", s)
		}
	}
}

// showsSecret reports whether s holds 8 consecutive bytes of secret as written or as Go quotes it.
func showsSecret(s, secret string) bool {
	q := strconv.Quote(secret)
	for _, form := range []string{secret, q[1 : len(q)-1]} {
		for i := 0; i+8 <= len(form); i++ {
			if strings.Contains(s, form[i:i+8]) {
				return true
			}
		}
	}
	return false
}

// integer agrees with exact rational arithmetic on every JSON number built from these mantissas and exponents: an
// integer within int's range is read as itself, anything else is refused.
func TestIntegerMatchesExactArithmetic(t *testing.T) {
	mantissas := []string{"0", "-0", "1", "-1", "7", "10", "120", "403", "-32001", "1.0", "1.5", "2.50", "0.25", "0.0", "-0.10",
		"32001.000", "9223372036854775807", "9223372036854775808", "-9223372036854775808", "922337203685477580.7", "0.000001"}
	exponents := []string{"", "e0", "E1", "e+2", "e-1", "E-2", "e-6", "e18", "e19", "e-20", "e400"}
	lo, hi := new(big.Rat).SetInt64(math.MinInt64), new(big.Rat).SetInt64(math.MaxInt64)
	for _, m := range mantissas {
		for _, e := range exponents {
			s := m + e
			if !json.Valid([]byte(s)) {
				t.Fatalf("%s is not a JSON number", s)
			}
			want, ok := new(big.Rat).SetString(s)
			if !ok {
				t.Fatalf("big.Rat cannot read %s", s)
			}
			isInt := want.IsInt() && want.Cmp(lo) >= 0 && want.Cmp(hi) <= 0
			got, gotInt := integer(json.RawMessage(s))
			if gotInt != isInt || isInt && int64(got) != want.Num().Int64() {
				t.Errorf("integer(%s) = %d, %v; want %s, %v", s, got, gotInt, want.RatString(), isInt)
			}
		}
	}
}
