package card

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"kontra.sh/kontra/internal/jws"
	"kontra.sh/kontra/internal/ssrf"
	"kontra.sh/kontra/internal/strictjson"
	"kontra.sh/kontra/internal/upstream"
)

func mustSellerKeys(t *testing.T, raw string, client *http.Client) *SellerKeys {
	t.Helper()
	k, err := NewSellerKeys(json.RawMessage(raw), client)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestNewSellerKeysRefuses(t *testing.T) {
	seller, _ := jws.EphemeralKeySet()
	jwks := string(mustJWKS(t, seller))
	client := http.DefaultClient
	for _, raw := range []string{
		`{"keys": []}`,
		`{"url": "http://seller.example/jwks.json"}`,
		`{"url": "https://u:p@seller.example/jwks.json"}`,
		`{"url": "https://seller.example/jwks.json", "x": 1}`,
		`{"url": 7}`,
		`{"url": "https://a.example/jwks", "keys": []}`,
		`{"URL": "https://a.example/jwks", "url": "https://b.example/jwks"}`,
		`{"keys": [], "keys": []}`,
		`[]`,
		`{}`,
	} {
		if _, err := NewSellerKeys(json.RawMessage(raw), client); err == nil {
			t.Errorf("NewSellerKeys(%s) succeeded", raw)
		}
	}
	if _, err := NewSellerKeys(json.RawMessage(`{"url": "https://seller.example/jwks.json"}`), nil); err == nil {
		t.Error("a {url} set without the SSRF-safe client was accepted")
	}
	if _, err := NewSellerKeys(json.RawMessage(jwks), nil); err != nil {
		t.Errorf("an inline set: %v", err)
	}
}

// Verify accepts any one signature by a configured key over the JCS of the card without signatures, and names
// the keys that verified; a card without signatures, signed by another key, or altered after signing does not
// verify.
func TestSellerKeysVerify(t *testing.T) {
	seller, _ := jws.EphemeralKeySet()
	stranger, _ := jws.EphemeralKeySet()
	keys := mustSellerKeys(t, string(mustJWKS(t, seller)), nil)
	card := string(read(t, "v1-vendor.json"))
	ctx := context.Background()

	ok, tps, err := keys.Verify(ctx, []byte(sellerSigned(t, seller, card)))
	sellerPub, _ := jws.ParseJWKS(mustJWKS(t, seller))
	want, _ := sellerPub.Thumbprints()
	if err != nil || !ok || len(tps) != 1 || tps[0] != want[0] {
		t.Fatalf("a seller-signed card: %v %v %v (want thumbprint %v)", ok, tps, err, want)
	}
	// Two signatures, one by a stranger: any one valid signature is enough.
	both := sellerSigned(t, stranger, card)
	m, _ := strictjson.Members([]byte(both))
	sig, _ := seller.Sign([]byte(card), "", nil)
	sb, _ := json.Marshal(sig)
	both = strings.Replace(both, string(m["signatures"]), "["+string(m["signatures"][1:len(m["signatures"])-1])+","+string(sb)+"]", 1)
	if ok, _, err := keys.Verify(ctx, []byte(both)); err != nil || !ok {
		t.Errorf("a card with the seller's signature beside a stranger's: %v %v", ok, err)
	}
	for name, c := range map[string]string{
		"a stranger's signature": sellerSigned(t, stranger, card),
		"no signatures":          strings.Replace(card, `"signatures": [{"protected": "eyJhbGciOiJFUzI1NiJ9", "signature": "c2lnbmF0dXJl"}],`, "", 1),
		"altered after signing":  strings.Replace(sellerSigned(t, seller, card), "Booking Agent", "Booking Agent!", 1),
		"the test card's own":    card,
	} {
		if ok, _, err := keys.Verify(ctx, []byte(c)); err != nil || ok {
			t.Errorf("%s: verified %v, %v", name, ok, err)
		}
	}
	if _, _, err := keys.Verify(ctx, []byte(`{"a":1,"a":2}`)); err == nil {
		t.Error("a card with a repeated key was judged")
	}
}

// jwksHost serves a seller's key set over TLS and can be switched off.
type jwksHost struct {
	srv   *httptest.Server
	mu    sync.Mutex
	body  []byte
	down  bool
	fetch atomic.Int64
}

func newJWKSHost(t *testing.T, body []byte) *jwksHost {
	h := &jwksHost{body: body}
	h.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.fetch.Add(1)
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.down {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write(h.body)
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *jwksHost) setDown(down bool) { h.mu.Lock(); h.down = down; h.mu.Unlock() }

// seller_keys in the keeper (spec §3, D89): a card a configured key verifies is served with verified: true; one
// no key verifies makes the service unavailable at once (503 card routes, service_unavailable dispatch) and is
// not persisted; a failed refresh (the card's or the {url} key set's) keeps the last verified card until 30
// minutes after the last good refresh.
func TestKeeperWithSellerKeys(t *testing.T) {
	gate, _ := jws.EphemeralKeySet()
	seller, _ := jws.EphemeralKeySet()
	stranger, _ := jws.EphemeralKeySet()
	card := string(read(t, "a2a-cli-exec.json"))
	good, bad := sellerSigned(t, seller, card), sellerSigned(t, stranger, card)
	host := newJWKSHost(t, mustJWKS(t, seller))
	keys := mustSellerKeys(t, `{"url": "`+host.srv.URL+`/jwks.json"}`, host.srv.Client())

	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	var clockMu sync.Mutex
	clock := func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return now }
	advance := func(d time.Duration) { clockMu.Lock(); now = now.Add(d); clockMu.Unlock() }
	var persisted, touched atomic.Int64
	cs := newCardServer(t, good)
	in := Input{PublicURL: "https://gate.example", ServicePath: "/agents/hello"}
	opts := []KeeperOption{WithClock(clock), WithSigner(&JWSSigner{Keys: gate, JWKSURL: "https://gate.example/.well-known/jwks.json"}),
		WithSellerKeys(keys),
		WithPersist(func(context.Context, *Served, time.Time) error { persisted.Add(1); return nil }),
		WithTouch(func(context.Context, *Served) { touched.Add(1) })}
	k := NewKeeper(backend(t, cs), in, opts...)
	ctx := context.Background()
	if err := k.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	s, err := k.Current()
	if err != nil || !s.Verified || persisted.Load() != 1 {
		t.Fatalf("a verified card: %+v %v (persisted %d)", s, err, persisted.Load())
	}
	pub, _ := jws.ParseJWKS(mustJWKS(t, gate))
	h, err := jws.Verify(s.Body, signatures(t, s.Body)[0], pub)
	if err != nil || h.Upstream == nil || !h.Upstream.Verified {
		t.Fatalf("the gate's header does not say verified: %+v %v", h, err)
	}
	if _, _, err := k.Dispatch(); err != nil {
		t.Fatal(err)
	}
	// A refresh of the same card touches card_fetched_at.
	if err := k.Refresh(ctx); err != nil || touched.Load() != 1 {
		t.Fatalf("an unchanged verified card: %v, touched %d", err, touched.Load())
	}

	// The key host fails: a failed refresh keeps the last verified card until 30 minutes after the last success.
	host.setDown(true)
	advance(29 * time.Minute)
	if err := k.Refresh(ctx); err == nil {
		t.Fatal("a refresh whose key set could not be fetched succeeded")
	}
	if s, err := k.Current(); err != nil || s.Unavailable || !s.Verified {
		t.Errorf("29 minutes after the last success: %+v %v", s, err)
	}
	advance(2 * time.Minute)
	if _, err := k.Current(); !errors.Is(err, ErrUnavailable) {
		t.Errorf("31 minutes after the last success: %v", err)
	}
	host.setDown(false)
	if err := k.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Current(); err != nil {
		t.Fatalf("back after a good refresh: %v", err)
	}

	// The seller now publishes a card no configured key verifies: unavailable at once, not persisted or touched.
	cs.set(bad, 0)
	persistedBefore, touchedBefore := persisted.Load(), touched.Load()
	if err := k.Refresh(ctx); !errors.Is(err, ErrNotVerified) {
		t.Fatalf("an unverifiable card: %v", err)
	}
	if s, err := k.Current(); !errors.Is(err, ErrUnavailable) || !errors.Is(err, ErrNotVerified) || s == nil || !s.Unavailable {
		t.Errorf("an unverifiable card is served: %+v %v", s, err)
	}
	if _, _, err := k.Dispatch(); !errors.Is(err, ErrUnavailable) {
		t.Errorf("dispatch on an unverifiable card: %v", err)
	}
	if persisted.Load() != persistedBefore || touched.Load() != touchedBefore {
		t.Error("an unverifiable card was recorded")
	}
	// A card handler answers 503 with an empty body for it (both card routes).
	for _, up := range []bool{false, true} {
		rec := httptest.NewRecorder()
		Handler(func(*http.Request) (*Served, error) { return k.Current() }, up).ServeHTTP(rec, httptest.NewRequest("GET", "/agents/hello", nil))
		if rec.Code != http.StatusServiceUnavailable || rec.Body.Len() != 0 {
			t.Errorf("card route (upstream %v): %d %q", up, rec.Code, rec.Body.String())
		}
	}

	// A replica that starts with the stored (verified) card and a {url} key set it has not fetched: unavailable
	// until its first refresh fetches the keys, even while the seller itself is down.
	cs.set("", http.StatusBadGateway)
	keys2 := mustSellerKeys(t, `{"url": "`+host.srv.URL+`/jwks.json"}`, host.srv.Client())
	k2 := NewKeeper(backend(t, cs), in, WithClock(clock), WithSellerKeys(keys2))
	if err := k2.Seed([]byte(good), clock()); err != nil {
		t.Fatal(err)
	}
	if _, err := k2.Current(); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("a seeded card before its keys were fetched: %v", err)
	}
	if err := k2.Refresh(ctx); err == nil {
		t.Fatal("a refresh with the seller down succeeded")
	}
	if s, err := k2.Current(); err != nil || !s.Verified {
		t.Errorf("the seeded card once the keys were fetched: %+v %v", s, err)
	}
	// An inline set verifies a seeded card at once, and refuses an unverifiable one.
	inline := mustSellerKeys(t, string(mustJWKS(t, seller)), nil)
	k3 := NewKeeper(backend(t, cs), in, WithClock(clock), WithSellerKeys(inline))
	if err := k3.Seed([]byte(good), clock()); err != nil {
		t.Fatal(err)
	}
	if _, err := k3.Current(); err != nil {
		t.Errorf("an inline-verified seed: %v", err)
	}
	if err := k3.Seed([]byte(bad), clock()); err != nil {
		t.Fatal(err)
	}
	if _, err := k3.Current(); !errors.Is(err, ErrNotVerified) {
		t.Errorf("an unverifiable seed: %v", err)
	}
}

// staticResolver resolves every name to its addresses.
type staticResolver []netip.Addr

func (r staticResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return r, nil
}

// A {url} seller key set is fetched with the SSRF-safe client only (spec §3, §10.3): an https URL to a loopback
// or private address, or a name resolving to one, is refused by the dialer, and nothing can be verified.
func TestSellerKeysURLThroughTheSSRFDialer(t *testing.T) {
	for _, tc := range []struct {
		url    string
		policy ssrf.Policy
	}{
		{"https://127.0.0.1:1/jwks.json", ssrf.Policy{}},
		{"https://seller.example/jwks.json", ssrf.Policy{Resolver: staticResolver{netip.MustParseAddr("10.1.2.3")}}},
		{"https://seller.example/jwks.json", ssrf.Policy{Resolver: staticResolver{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("169.254.169.254")}}},
	} {
		keys := mustSellerKeys(t, `{"url": "`+tc.url+`"}`, ssrf.NewClient(tc.policy))
		if err := keys.Refresh(context.Background()); !errors.Is(err, ssrf.ErrAddress) {
			t.Errorf("%s: %v, want the SSRF dialer's refusal", tc.url, err)
		}
		if _, _, err := keys.Verify(context.Background(), []byte(`{"name":"x"}`)); !errors.Is(err, ErrNoSellerKeys) {
			t.Errorf("%s: Verify with no keys fetched: %v", tc.url, err)
		}
	}
}

func (h *jwksHost) setBody(b []byte) { h.mu.Lock(); h.body = b; h.mu.Unlock() }

// A {url} key fetch is bounded on its own (spec §6.5's rule for every upstream exchange; the card fetch's 30 s
// budget): a key host that accepts the request and never answers fails RefreshSellerKeys, the replicas-without-a-
// session path whose loop context never ends, instead of blocking it; and a slow key host does not eat into the
// card fetch's budget on the session path.
func TestSellerKeyFetchIsBounded(t *testing.T) {
	release := make(chan struct{})
	var delay atomic.Int64 // 0: hang until the test ends
	seller, _ := jws.EphemeralKeySet()
	body := mustJWKS(t, seller)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if d := time.Duration(delay.Load()); d > 0 {
			time.Sleep(d)
			_, _ = w.Write(body)
			return
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	keys := mustSellerKeys(t, `{"url": "`+srv.URL+`/jwks.json"}`, srv.Client())
	card := sellerSigned(t, seller, string(read(t, "a2a-cli-exec.json")))
	var fetchErr atomic.Value
	fetch := func(ctx context.Context, _ upstream.Backend) (*upstream.Fetched, error) {
		select {
		case <-time.After(250 * time.Millisecond): // a seller answering in a quarter of a second
		case <-ctx.Done():
			fetchErr.Store(ctx.Err())
			return nil, ctx.Err()
		}
		return &upstream.Fetched{Raw: []byte(card)}, nil
	}
	k := NewKeeper(nil, Input{PublicURL: "https://gate.example", ServicePath: "/agents/hello"},
		WithSellerKeys(keys), WithFetchTimeout(400*time.Millisecond), WithFetch(fetch))

	done := make(chan error, 1)
	go func() { done <- k.RefreshSellerKeys(context.Background()) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("a key host that never answers: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RefreshSellerKeys is still waiting on a key host that never answers")
	}

	// The key host answers in 300 ms, the seller in 250 ms: each within the 400 ms budget, not both in one.
	delay.Store(int64(300 * time.Millisecond))
	if err := k.Refresh(context.Background()); err != nil {
		t.Fatalf("a refresh whose key and card fetches each fit their budget: %v (card fetch: %v)", err, fetchErr.Load())
	}
	if s, err := k.Current(); err != nil || !s.Verified {
		t.Fatalf("the card: verified %v, %v", s != nil && s.Verified, err)
	}
}

// A card refused against a stale {url} key set is verified again when the keys are refreshed (spec §3): after a
// seller rotates its key and re-signs its card, a replica that holds no session (it serves the stored card the
// NOTIFY seeded) serves it as soon as its key refresh fetches the new key, and so does the session path when the
// card fetch fails but the keys arrive.
func TestRefusedCardReverifiedAfterKeyRefresh(t *testing.T) {
	old, _ := jws.EphemeralKeySet()
	rotated, _ := jws.EphemeralKeySet()
	card := sellerSigned(t, rotated, string(read(t, "a2a-cli-exec.json")))
	host := newJWKSHost(t, mustJWKS(t, old))
	in := Input{PublicURL: "https://gate.example", ServicePath: "/agents/hello"}
	ctx := context.Background()

	// No session: RefreshSellerKeys.
	k := NewKeeper(nil, in, WithSellerKeys(mustSellerKeys(t, `{"url": "`+host.srv.URL+`/jwks.json"}`, host.srv.Client())))
	if err := k.RefreshSellerKeys(ctx); err != nil {
		t.Fatal(err)
	}
	if err := k.Seed([]byte(card), time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Current(); !errors.Is(err, ErrNotVerified) {
		t.Fatalf("a card signed by a key the stale set lacks: %v", err)
	}
	host.setBody(mustJWKS(t, rotated))
	if err := k.RefreshSellerKeys(ctx); err != nil {
		t.Fatal(err)
	}
	if s, err := k.Current(); err != nil || !s.Verified {
		t.Fatalf("after the key refresh: verified %v, %v", s != nil && s.Verified, err)
	}

	// A session whose card fetch fails while the keys arrive.
	host.setBody(mustJWKS(t, old))
	cs := newCardServer(t, "")
	cs.set("", http.StatusBadGateway)
	k2 := NewKeeper(backend(t, cs), in, WithSellerKeys(mustSellerKeys(t, `{"url": "`+host.srv.URL+`/jwks.json"}`, host.srv.Client())))
	if err := k2.RefreshSellerKeys(ctx); err != nil {
		t.Fatal(err)
	}
	if err := k2.Seed([]byte(card), time.Now()); err != nil {
		t.Fatal(err)
	}
	host.setBody(mustJWKS(t, rotated))
	if err := k2.Refresh(ctx); err == nil {
		t.Fatal("a refresh whose card fetch failed succeeded")
	}
	if s, err := k2.Current(); err != nil || !s.Verified {
		t.Fatalf("after a refresh that fetched the keys only: verified %v, %v", s != nil && s.Verified, err)
	}
}
