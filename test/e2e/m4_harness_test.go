package e2e

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"kontra.sh/kontra/internal/config"
	"kontra.sh/kontra/internal/edge"
	"kontra.sh/kontra/internal/hooks"
)

// The M4 harness's own pieces, without Postgres or Temporal (they run under -short).

// The Front's PROXY v2 headers, read by the gate's own edge (internal/edge: proxy_protocol, trusted 127.0.0.1/32):
// a connection the Front forwards after SetSource(addr) is served as coming from addr, an IPv4 /24 and an IPv6 /48
// alike; one forwarded with no source, and one made to the edge directly, are served as 127.0.0.1. Without this, the
// anonymous tests' network limits would count every node against one network.
func TestFrontProxyHeaderIsTheClientAddress(t *testing.T) {
	routes := edge.NewRouter()
	routes.Handle("GET /who", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a, _ := edge.ClientAddr(r.Context())
		_, _ = io.WriteString(w, a.String())
	}))
	srv, err := edge.New(edge.Config{Listen: "127.0.0.1:0", PublicURL: "http://127.0.0.1", Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		ClientIP: edge.ClientIP{From: config.ClientIPProxyProtocol, Trusted: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}}}, routes)
	if err != nil {
		t.Fatal(err)
	}
	addr, err := srv.Listen()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = srv.Serve(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	f, err := NewFront()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.Close)
	f.Route(&Gate{Name: "edge", Addr: addr.String()})
	who := func(base string) string {
		t.Helper()
		// A new connection each time: the Front writes the header once per connection.
		c := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: 10 * time.Second}
		resp, err := c.Get(base + "/who")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	if got := who(f.URL); got != "127.0.0.1" {
		t.Errorf("a connection with no source set: the edge saw %s, want 127.0.0.1", got)
	}
	for _, src := range []string{"198.51.100.7", "2001:db8:1:4::1", "203.0.113.200"} {
		f.SetSource(netip.MustParseAddr(src))
		if got := who(f.URL); got != src {
			t.Errorf("SetSource(%s): the edge saw %s", src, got)
		}
		if got := who("http://" + addr.String()); got != "127.0.0.1" {
			t.Errorf("a direct connection while the Front names %s: the edge saw %s, want 127.0.0.1", src, got)
		}
	}
	f.SetSource(netip.Addr{})
	if got := who(f.URL); got != "127.0.0.1" {
		t.Errorf("after SetSource(zero): the edge saw %s", got)
	}
}

// The Admission double answers as hooks.json says and as the gate reads it (hooks.ParseDecision, hooks.CheckQuote): a
// quote for a service it quotes, naming the pre-minted task id in Cloud's thread URL by default and in a team gate's
// own page when told; allow for any other; a raw decision when set; nothing once Down, and again after Up. It records
// the requests and their Authorization.
func TestAdmissionDouble(t *testing.T) {
	a, err := StartAdmission("paid")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	ask := func(service, task string) (hooks.Decision, error) {
		t.Helper()
		body := `{"operation":"task","taskId":"` + task + `","service":{"tenantId":"00000000-0000-0000-0000-000000000001","tenant":"default","name":"` +
			service + `","kind":"a2a"},"principal":{"kind":"key","id":"key:kk_e2e","buyerId":"00000000-0000-0000-0000-000000000002","via":"api_key"},"protocol":"a2a"}`
		req, _ := http.NewRequest(http.MethodPost, a.URL(), strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+a.Secret)
		resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
		if err != nil {
			return hooks.Decision{}, err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return hooks.ParseDecision(b)
	}
	const task = "01a18a00-0000-7000-8000-000000000001"
	d, err := ask("paid", task)
	if err != nil || d.Outcome != hooks.Quote || d.Quote == nil || d.Quote.ApproveURL != "https://app.kontra.sh/#/t/"+task {
		t.Fatalf("a quoted service: %+v %v", d, err)
	}
	if err := hooks.CheckQuote(d.Quote, time.Now()); err != nil {
		t.Errorf("the double's quote does not pass the gate's check: %v", err)
	}
	if d, err := ask("free", task); err != nil || d.Outcome != hooks.Allow {
		t.Errorf("a service it does not quote: %+v %v", d, err)
	}
	a.Set(func(id string) string { return "https://gate.team.example/approve/" + id }, time.Minute, "")
	if d, err := ask("paid", task); err != nil || d.Quote == nil || d.Quote.ApproveURL != "https://gate.team.example/approve/"+task {
		t.Errorf("a team gate's approve URL: %+v %v", d, err)
	}
	a.Set(nil, 0, `{"outcome":"deny","refusal":{"code":-32001,"message":"no"}}`)
	if d, err := ask("paid", task); err != nil || d.Outcome != hooks.Deny {
		t.Errorf("a raw decision: %+v %v", d, err)
	}
	reqs := a.Requests()
	if len(reqs) != 4 || reqs[0].Service != "paid" || reqs[0].TaskID != task || reqs[0].Auth != "Bearer "+a.Secret || reqs[1].Service != "free" {
		t.Errorf("the recorded requests: %+v", reqs)
	}
	a.Down()
	if _, err := ask("paid", task); err == nil {
		t.Error("the double answered while down")
	}
	if err := a.Up(); err != nil {
		t.Fatal(err)
	}
	a.Set(nil, 0, "")
	if d, err := ask("free", task); err != nil || d.Outcome != hooks.Allow {
		t.Errorf("after Up: %+v %v", d, err)
	}
}
