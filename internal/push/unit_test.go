package push

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	a2apush "github.com/a2aproject/a2a-go/v2/a2asrv/push"

	"github.com/medmahmoudi26/kontra/internal/ssrf"
)

// countingListener accepts on 127.0.0.1 and counts connections, to show that a refused URL is never
// dialed.
func countingListener(t *testing.T) (port string, conns *atomic.Int64) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	conns = &atomic.Int64{}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			conns.Add(1)
			c.Close()
		}
	}()
	t.Cleanup(func() { l.Close() })
	_, port, _ = net.SplitHostPort(l.Addr().String())
	return port, conns
}

// TestCheckURLRefusesSSRFTargets is ssrf_webhook's set-time half (spec §10.1, §10.3, D60): each address of
// the named test, and a name resolving to a private address, is InvalidParams when set, and nothing is
// dialed. A redirecting URL on a public host is accepted (its redirect is only seen at delivery).
func TestCheckURLRefusesSSRFTargets(t *testing.T) {
	port, conns := countingListener(t)
	res := fakeResolver{
		"private.example":   {"10.1.2.3"},
		"loopback.example":  {"127.0.0.1"},
		"mixed.example":     {"93.184.215.14", "192.168.1.1"},
		"redirect.example":  {"93.184.215.14"},
		"localhost":         {"127.0.0.1", "::1"},
		"metadata.example":  {"169.254.169.254"},
		"nat64.example":     {"64:ff9b::a00:1"},
		"public.example":    {"93.184.215.14"},
		"public6.example":   {"2606:2800:21f:cb07:6820:80da:af6b:8b2c"},
		"loopbackp.example": {"127.0.0.1"},
	}
	p := ssrf.Policy{Resolver: res}
	ctx := context.Background()
	for _, u := range []string{
		"https://169.254.169.254/latest/meta-data",
		"https://10.0.0.1/hook",
		"https://localhost/hook",
		"https://localhost:" + port + "/hook",
		"https://[::ffff:10.0.0.1]/hook",
		"https://[64:ff9b::a00:1]/hook",
		"https://private.example/hook",
		"https://loopback.example:" + port + "/hook",
		"https://mixed.example/hook",
		"https://metadata.example/hook",
		"https://nat64.example/hook",
		"https://100.64.0.1/hook",
		"https://[fd00::1]/hook",
		"http://public.example/hook",          // plain http without allow_private (§10.3)
		"https://user:pw@public.example/hook", // userinfo
		"ftp://public.example/hook",           // scheme
		"https://nowhere.example/hook",        // does not resolve
		"/relative/hook",                      // not absolute
		"",                                    // empty
	} {
		err := CheckURL(ctx, u, p)
		if !errors.Is(err, a2a.ErrInvalidParams) {
			t.Errorf("CheckURL(%q) = %v, want InvalidParams", u, err)
		}
		if err != nil && strings.Contains(err.Error(), "/hook") {
			t.Errorf("CheckURL(%q): the refusal quotes the URL: %v", u, err)
		}
	}
	for _, u := range []string{"https://redirect.example/302", "https://public.example:8443/hook?x=1", "https://public6.example/hook", "https://93.184.215.14/hook"} {
		if err := CheckURL(ctx, u, p); err != nil {
			t.Errorf("CheckURL(%q) = %v, want accepted", u, err)
		}
	}
	// push.allow_private skips the address check and permits http:// (D67), never userinfo.
	pp := ssrf.Policy{AllowPrivate: true, Resolver: res}
	for _, u := range []string{"http://127.0.0.1:" + port + "/hook", "http://localhost/hook", "https://10.0.0.1/hook"} {
		if err := CheckURL(ctx, u, pp); err != nil {
			t.Errorf("allow_private CheckURL(%q) = %v", u, err)
		}
	}
	if err := CheckURL(ctx, "http://u:p@127.0.0.1/hook", pp); !errors.Is(err, a2a.ErrInvalidParams) {
		t.Errorf("allow_private with userinfo: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if n := conns.Load(); n != 0 {
		t.Fatalf("the set-time check opened %d connections", n)
	}
}

// TestCheckURLOffline is SaveInline's check inside the work queue's transaction: every rule but DNS; an
// IP literal is still refused.
func TestCheckURLOffline(t *testing.T) {
	for _, tc := range []struct {
		url  string
		ok   bool
		pol  ssrf.Policy
		name string
	}{
		{"https://10.0.0.1/hook", false, ssrf.Policy{}, "private literal"},
		{"https://[::ffff:10.0.0.1]/hook", false, ssrf.Policy{}, "mapped literal"},
		{"https://[64:ff9b::a00:1]/hook", false, ssrf.Policy{}, "NAT64 literal"},
		{"http://example.com/hook", false, ssrf.Policy{}, "plain http"},
		{"https://u:p@example.com/hook", false, ssrf.Policy{}, "userinfo"},
		{"https://example.com/hook", true, ssrf.Policy{}, "a name: checked by the interceptor and at dial time"},
		{"http://127.0.0.1:9/hook", true, ssrf.Policy{AllowPrivate: true}, "allow_private"},
	} {
		err := checkURLOffline(tc.url, tc.pol)
		if (err == nil) != tc.ok {
			t.Errorf("%s: checkURLOffline(%q) = %v", tc.name, tc.url, err)
		}
		if err != nil && !errors.Is(err, a2a.ErrInvalidParams) {
			t.Errorf("%s: %v is not InvalidParams", tc.name, err)
		}
	}
}

func TestValidate(t *testing.T) {
	long := strings.Repeat("x", MaxIDChars+1)
	big := strings.Repeat("y", MaxConfigBytes)
	for _, tc := range []struct {
		name string
		cfg  *a2a.PushConfig
		ok   bool
	}{
		{"nil", nil, false},
		{"no url", &a2a.PushConfig{}, false},
		{"plain", &a2a.PushConfig{URL: "https://x/h"}, true},
		{"full", &a2a.PushConfig{ID: "cfg-1", URL: "https://x/h", Token: "t", Auth: &a2a.PushAuthInfo{Scheme: "Bearer", Credentials: "abc=="}}, true},
		{"id too long", &a2a.PushConfig{ID: long, URL: "https://x/h"}, false},
		{"id with a control char", &a2a.PushConfig{ID: "a\nb", URL: "https://x/h"}, false},
		{"token with CRLF", &a2a.PushConfig{URL: "https://x/h", Token: "a\r\nX-Evil: 1"}, false},
		{"token with NUL", &a2a.PushConfig{URL: "https://x/h", Token: "a\x00b"}, false},
		{"token with outer space", &a2a.PushConfig{URL: "https://x/h", Token: " a"}, false},
		{"scheme not a token", &a2a.PushConfig{URL: "https://x/h", Auth: &a2a.PushAuthInfo{Scheme: "Bear er", Credentials: "c"}}, false},
		{"credentials with LF", &a2a.PushConfig{URL: "https://x/h", Auth: &a2a.PushAuthInfo{Scheme: "Basic", Credentials: "c\nd"}}, false},
		{"too large", &a2a.PushConfig{URL: "https://x/h", Token: big}, false},
	} {
		err := validate(tc.cfg)
		if (err == nil) != tc.ok {
			t.Errorf("%s: validate = %v", tc.name, err)
		}
		if err != nil && !errors.Is(err, a2a.ErrInvalidParams) {
			t.Errorf("%s: %v is not InvalidParams", tc.name, err)
		}
		if err != nil && containsAny(err.Error(), "X-Evil", big[:20]) != "" {
			t.Errorf("%s: the refusal quotes the value: %v", tc.name, err)
		}
	}
}

func TestConfigNotFoundError(t *testing.T) {
	if !errors.Is(errConfigNotFound, a2a.ErrTaskNotFound) || !errors.Is(errConfigNotFound, a2apush.ErrPushConfigNotFound) {
		t.Fatalf("%v: want TaskNotFound (A2A §3.1.8) and a2a-go's ErrPushConfigNotFound", errConfigNotFound)
	}
	if errors.Is(errConfigNotFound, a2a.ErrInvalidParams) {
		t.Fatal("must map to one JSON-RPC code")
	}
}

// TestHostLimiter: deliveries to one host are spaced by the interval (10/N a second, D72); other hosts are
// independent; a host idle for a while starts at once.
func TestHostLimiter(t *testing.T) {
	l := newHostLimiter()
	t0 := time.Unix(1_000_000, 0)
	iv := 100 * time.Millisecond
	var slots []time.Duration
	for range 5 {
		slots = append(slots, l.reserve("a.example", t0, iv).Sub(t0))
	}
	for i, s := range slots {
		if s != time.Duration(i)*iv {
			t.Fatalf("slots %v, want every %s", slots, iv)
		}
	}
	if s := l.reserve("b.example", t0, iv); !s.Equal(t0) {
		t.Errorf("another host waits %s", s.Sub(t0))
	}
	later := t0.Add(time.Minute)
	if s := l.reserve("a.example", later, iv); !s.Equal(later) {
		t.Errorf("an idle host waits %s", s.Sub(later))
	}
	// 10 a second per host across the gate: with N = 2 replicas each spaces deliveries 200 ms apart.
	if got := time.Second * time.Duration(2) / RatePerHost; got != 200*time.Millisecond {
		t.Errorf("interval for N=2: %s", got)
	}
	// The map is pruned of stale hosts as it grows.
	for i := range limiterPrune + 10 {
		l.reserve(strings.Repeat("h", 1+i%7)+string(rune('a'+i%26))+time.Duration(i).String(), t0, iv)
	}
	l.reserve("trigger", later.Add(time.Hour), iv)
	if len(l.next) > limiterPrune {
		t.Errorf("%d hosts kept after a prune", len(l.next))
	}
}

func TestDestination(t *testing.T) {
	for in, want := range map[string]string{
		"https://Hooks.Example.COM:8443/a?b": "hooks.example.com",
		"http://[::1]:80/x":                  "::1",
		"https://10.0.0.1/":                  "10.0.0.1",
	} {
		if got := destination(in); got != want {
			t.Errorf("destination(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestPostIsOneRequestWithTheSpecHeaders: one POST, Content-Type application/a2a+json, the authentication
// as Authorization, the token in both token headers, the body {"task": …} (spec §10.2).
func TestPostIsOneRequestWithTheSpecHeaders(t *testing.T) {
	r := newReceiver(t, nil)
	d := NewDeliverer(nil, private, fast())
	snap := &a2a.Task{ID: "t1", ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateWorking}}
	body, _ := json.Marshal(a2a.StreamResponse{Event: snap})
	status, err := d.post(context.Background(), pushConfig(r.url("/hook")), body)
	if err != nil || status != http.StatusOK {
		t.Fatalf("post: %d %v", status, err)
	}
	got := r.requests()
	if len(got) != 1 {
		t.Fatalf("%d requests", len(got))
	}
	g := got[0]
	if g.method != http.MethodPost || g.path != "/hook" {
		t.Errorf("%s %s", g.method, g.path)
	}
	for k, want := range map[string]string{
		"Content-Type":             "application/a2a+json",
		"Authorization":            "Bearer " + secretCreds,
		"A2A-Notification-Token":   secretToken,
		"X-A2A-Notification-Token": secretToken,
	} {
		if v := g.header.Get(k); v != want {
			t.Errorf("%s = %q, want %q", k, v, want)
		}
	}
	var sr a2a.StreamResponse
	if err := json.Unmarshal([]byte(g.body), &sr); err != nil {
		t.Fatal(err)
	}
	if task, ok := sr.Event.(*a2a.Task); !ok || task.ID != "t1" || !strings.HasPrefix(g.body, `{"task":`) {
		t.Errorf("body %s: want {\"task\": …}", g.body)
	}

	// No credentials, no Authorization; credentials without a scheme are sent as they are.
	if _, err := d.post(context.Background(), &a2a.PushConfig{URL: r.url("/bare")}, body); err != nil {
		t.Fatal(err)
	}
	if _, err := d.post(context.Background(), &a2a.PushConfig{URL: r.url("/raw"), Auth: &a2a.PushAuthInfo{Credentials: "opaque"}}, body); err != nil {
		t.Fatal(err)
	}
	got = r.requests()
	if a := got[1].header.Get("Authorization"); a != "" || got[1].header.Get("A2A-Notification-Token") != "" {
		t.Errorf("a config without credentials or token sent %q", a)
	}
	if a := got[2].header.Get("Authorization"); a != "opaque" {
		t.Errorf("credentials without a scheme: %q", a)
	}
}

// TestPostNeverFollowsARedirect: a 3xx is returned as the delivery's status (a failed delivery), and the
// redirect target receives no connection (spec §10.2, D60).
func TestPostNeverFollowsARedirect(t *testing.T) {
	target := newReceiver(t, nil)
	redir := newReceiver(t, func(int) int { return http.StatusFound })
	redir.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, target.url("/target"), http.StatusTemporaryRedirect)
	})
	d := NewDeliverer(nil, private, fast())
	status, err := d.post(context.Background(), pushConfig(redir.url("/redirect")), []byte(`{"task":{}}`))
	if err != nil || status != http.StatusTemporaryRedirect {
		t.Fatalf("post: %d %v", status, err)
	}
	if n := target.conns.Load(); n != 0 {
		t.Fatalf("the redirect target received %d connections", n)
	}
}

// TestPostTimeout: a receiver that does not answer within the timeout is a failed delivery whose error
// names the timeout, not the URL.
func TestPostTimeout(t *testing.T) {
	r := newReceiver(t, nil)
	r.hold = 2 * time.Second
	o := fast()
	o.Timeout = 150 * time.Millisecond
	d := NewDeliverer(nil, private, o)
	start := time.Now()
	_, err := d.post(context.Background(), pushConfig(r.url("/"+secretPath)), []byte(`{}`))
	if err == nil || time.Since(start) > time.Second {
		t.Fatalf("post to a hung receiver: %v after %s", err, time.Since(start))
	}
	if strings.Contains(err.Error(), secretPath) || strings.Contains(err.Error(), "127.0.0.1:") && strings.Contains(err.Error(), "/") {
		t.Errorf("the error names the URL: %v", err)
	}
	if DeliveryTimeout != 10*time.Second {
		t.Errorf("DeliveryTimeout %s, want 10 s (spec §10.2)", DeliveryTimeout)
	}
}

// TestPostErrorsNameNoURL: http.Client quotes the URL in every error; the delivery's error must not.
func TestPostErrorsNameNoURL(t *testing.T) {
	port, _ := countingListener(t)
	d := NewDeliverer(nil, ssrf.Policy{Resolver: fakeResolver{"private.example": {"10.0.0.1"}}}, fast())
	for _, u := range []string{
		"https://private.example/" + secretPath + "?k=" + secretToken, // refused at dial time (SSRF)
		"http://127.0.0.1:" + port + "/" + secretPath,                 // plain http without allow_private
		"https://127.0.0.1:1/" + secretPath + "?k=" + secretToken,     // refused address
	} {
		_, err := d.post(context.Background(), &a2a.PushConfig{URL: u}, []byte(`{}`))
		if err == nil {
			t.Fatalf("post %s succeeded", u)
		}
		if s := containsAny(err.Error(), secretPath, secretToken); s != "" {
			t.Errorf("post error carries %q: %v", s, err)
		}
	}
	dp := NewDeliverer(nil, private, fast())
	_, err := dp.post(context.Background(), &a2a.PushConfig{URL: "http://127.0.0.1:1/" + secretPath + "?k=" + secretToken}, []byte(`{}`))
	if err == nil {
		t.Fatal("post to a closed port succeeded")
	}
	if s := containsAny(err.Error(), secretPath, secretToken); s != "" {
		t.Errorf("connection refused error carries %q: %v", s, err)
	}
}

// TestBadInputIsNotRetried: an input naming no gate task or no config is a non-retryable error.
func TestBadInputIsNotRetried(t *testing.T) {
	d := NewDeliverer(nil, private, fast())
	for _, in := range []struct{ task, cfg string }{{"not-a-uuid", "c"}, {"0192f0b4-0000-7000-8000-000000000001", ""}} {
		_, err := d.PushWebhook(context.Background(), pushInput(in.task, in.cfg))
		var ae interface{ NonRetryable() bool }
		if !errors.As(err, &ae) || !ae.NonRetryable() {
			t.Errorf("PushWebhook(%q, %q) = %v, want a non-retryable error", in.task, in.cfg, err)
		}
	}
}
