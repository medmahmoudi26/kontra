package card

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/medmahmoudi26/kontra/internal/upstream"
)

var update = flag.Bool("update", false, "rewrite the golden served cards in testdata")

const escrowURI = "https://kontra.sh/ext/escrow/v1"

func read(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func input(upstreamCard []byte) Input {
	return Input{
		Upstream:    upstreamCard,
		PublicURL:   "https://gate.example/",
		ServicePath: "/agents/booking",
		CardExtensions: []json.RawMessage{
			json.RawMessage(`{"uri":"https://kontra.sh/ext/escrow/v1","required":true,"params":{"currency":"EUR"}}`),
		},
	}
}

func members(t *testing.T, b []byte) object {
	t.Helper()
	o, err := parseObject(b)
	if err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	return o
}

// gateOwned are the top-level members §3 lets the gate write.
var gateOwned = map[string]bool{
	"supportedInterfaces": true, "securitySchemes": true, "securityRequirements": true, "capabilities": true, "signatures": true,
}

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, got, "", "  "); err != nil {
			t.Fatal(err)
		}
		// The golden file is the exact served bytes; the indented copy is for review.
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from the golden served card:\n got %s\nwant %s", name, got, want)
	}
}

// A 1.0 card keeps every seller field byte for byte and in its place, and gets exactly the gate's fields
// of §3 (acceptance: card golden tests).
func TestRewriteV1KeepsSellerFields(t *testing.T) {
	up := read(t, "v1-vendor.json")
	served, err := Rewrite(input(up))
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "v1-vendor.served.json", served)
	upO, srvO := members(t, up), members(t, served)

	// Every seller member is there with the same bytes, in the same relative order.
	var upKeys, srvKeys []string
	for _, m := range upO {
		if gateOwned[m.key] {
			continue
		}
		upKeys = append(upKeys, m.key)
		got, ok := srvO.get(m.key)
		if !ok || !bytes.Equal(got, m.val) {
			t.Errorf("seller member %q: got %s, want %s", m.key, got, m.val)
		}
	}
	for _, m := range srvO {
		if !gateOwned[m.key] {
			srvKeys = append(srvKeys, m.key)
		}
	}
	if !reflect.DeepEqual(upKeys, srvKeys) {
		t.Errorf("seller members %v, served %v", upKeys, srvKeys)
	}
	if srvO.has("signatures") {
		t.Error("M1 serves no signatures")
	}

	var card a2a.AgentCard
	if err := json.Unmarshal(served, &card); err != nil {
		t.Fatalf("the served card is not an A2A 1.0 card: %v", err)
	}
	want := []*a2a.AgentInterface{
		{URL: "https://gate.example/agents/booking", ProtocolBinding: a2a.TransportProtocolJSONRPC, ProtocolVersion: "1.0"},
		{URL: "https://gate.example/agents/booking", ProtocolBinding: a2a.TransportProtocolHTTPJSON, ProtocolVersion: "1.0"},
	}
	if !reflect.DeepEqual(card.SupportedInterfaces, want) {
		t.Errorf("interfaces %+v", card.SupportedInterfaces)
	}
	c := card.Capabilities
	if !c.Streaming || !c.PushNotifications || c.ExtendedAgentCard {
		t.Errorf("capabilities %+v", c)
	}
	var uris []string
	for _, e := range c.Extensions {
		uris = append(uris, e.URI)
	}
	// The seller's input-schema and own entries stay; its escrow entry is replaced by the operator's.
	if strings.Join(uris, " ") != "https://kontra.sh/ext/input-schema/v1 https://example.com/ext/seller "+escrowURI {
		t.Errorf("extensions %v", uris)
	}
	for _, e := range c.Extensions {
		if e.URI == escrowURI && (!e.Required || e.Params["currency"] != "EUR" || e.Description != "") {
			t.Errorf("the escrow entry is not the operator's: %+v", e)
		}
	}
	caps := members(t, mustGet(t, srvO, "capabilities"))
	if got, _ := caps.get("x-cap"); string(got) != `"seller"` {
		t.Errorf("an unknown capability member was lost: %s", got)
	}
	schemes := string(mustGet(t, srvO, "securitySchemes"))
	if schemes != `{"bearer":{"httpAuthSecurityScheme":{"scheme":"Bearer"}}}` {
		t.Errorf("securitySchemes %s", schemes)
	}
	if reqs := string(mustGet(t, srvO, "securityRequirements")); reqs != `[{"schemes":{"bearer":{"list":[]}}}]` {
		t.Errorf("securityRequirements %s", reqs)
	}
	// The gate never names an upstream URL.
	if strings.Contains(string(served), "10.1.2.3") {
		t.Error("an upstream URL reached the served card")
	}
	// The same input gives the same bytes (replicas converge on one ETag).
	again, _ := Rewrite(input(up))
	if !bytes.Equal(again, served) || ETag(again) != ETag(served) {
		t.Error("the rewrite is not deterministic")
	}
}

func mustGet(t *testing.T, o object, k string) json.RawMessage {
	t.Helper()
	v, ok := o.get(k)
	if !ok {
		t.Fatalf("no %q", k)
	}
	return v
}

func TestRewriteAnonymousAndOIDC(t *testing.T) {
	in := input(read(t, "v1-vendor.json"))
	in.OnlyAnonymous = true
	served, err := Rewrite(in)
	if err != nil {
		t.Fatal(err)
	}
	o := members(t, served)
	if o.has("securitySchemes") || o.has("securityRequirements") || o.has("security") {
		t.Errorf("allow: [anonymous] serves security fields: %s", served)
	}
	in.OnlyAnonymous, in.OIDC, in.OIDCIssuer = false, true, "https://idp.example/"
	served, err = Rewrite(in)
	if err != nil {
		t.Fatal(err)
	}
	var card a2a.AgentCard
	if err := json.Unmarshal(served, &card); err != nil {
		t.Fatal(err)
	}
	oidc, ok := card.SecuritySchemes["oidc"].(a2a.OpenIDConnectSecurityScheme)
	if !ok || oidc.OpenIDConnectURL != "https://idp.example/.well-known/openid-configuration" || len(card.SecurityRequirements) != 2 {
		t.Errorf("OIDC: %+v %+v", card.SecuritySchemes, card.SecurityRequirements)
	}
}

// 0.3 and 0.2.5 cards are served as 1.0 (card_compat): renamed fields, unknown ones kept.
func TestCompatCards(t *testing.T) {
	for _, tc := range []struct{ in, golden string }{
		{"v03.json", "v03.served.json"},
		{"v025-helloworld.json", "v025-helloworld.served.json"},
	} {
		up := read(t, tc.in)
		norm, err := Normalize(up)
		if err != nil {
			t.Fatal(err)
		}
		served, err := Rewrite(input(up))
		if err != nil {
			t.Fatal(err)
		}
		golden(t, tc.golden, served)
		o := members(t, served)
		for _, gone := range []string{"url", "preferredTransport", "additionalInterfaces", "protocolVersion", "supportsAuthenticatedExtendedCard", "security"} {
			if o.has(gone) {
				t.Errorf("%s: 0.x member %q served", tc.in, gone)
			}
		}
		var card a2a.AgentCard
		if err := json.Unmarshal(served, &card); err != nil || len(card.SupportedInterfaces) != 2 || card.Name == "" {
			t.Errorf("%s: not a 1.0 card: %+v %v", tc.in, card, err)
		}
		// Normalize alone yields the 1.0 shape with the seller's interfaces, which dispatch reads.
		var n struct {
			SupportedInterfaces []a2a.AgentInterface `json:"supportedInterfaces"`
			Capabilities        map[string]any       `json:"capabilities"`
		}
		if err := json.Unmarshal(norm, &n); err != nil || len(n.SupportedInterfaces) == 0 {
			t.Errorf("%s: normalized %s %v", tc.in, norm, err)
		}
		if n.Capabilities["extendedAgentCard"] != true {
			t.Errorf("%s: supportsAuthenticatedExtendedCard did not move into capabilities: %v", tc.in, n.Capabilities)
		}
	}
	norm, _ := Normalize(read(t, "v03.json"))
	o := members(t, norm)
	if got := string(mustGet(t, o, "x-vendor")); got != `"keep-me"` {
		t.Errorf("x-vendor %s", got)
	}
	if got := string(mustGet(t, o, "supportedInterfaces")); got != `[{"url":"http://agent.internal:10000/","protocolBinding":"JSONRPC","protocolVersion":"0.3.0"},{"url":"http://agent.internal:10000/rest","protocolBinding":"HTTP+JSON","protocolVersion":"0.3.0"}]` {
		t.Errorf("interfaces %s", got)
	}
	if got := string(mustGet(t, o, "securityRequirements")); got != `[{"schemes":{"bearer":{"list":[]}}},{"schemes":{"key":{"list":["read"]}}}]` {
		t.Errorf("security %s", got)
	}
	if got := string(mustGet(t, o, "securitySchemes")); got != `{"bearer":{"httpAuthSecurityScheme":{"bearerFormat":"JWT","scheme":"bearer"}},"key":{"apiKeySecurityScheme":{"location":"header","name":"X-Key"}}}` {
		t.Errorf("schemes %s", got)
	}
	if got := string(mustGet(t, o, "skills")); !strings.Contains(got, `"securityRequirements":[{"schemes":{"bearer":{"list":["fx"]}}}]`) || strings.Contains(got, `"security"`) {
		t.Errorf("skills %s", got)
	}
	// A 1.0 card normalizes to itself, byte for byte.
	v1 := read(t, "v1-vendor.json")
	if n, err := Normalize(v1); err != nil || !bytes.Equal(n, v1) {
		t.Errorf("a 1.0 card changed in Normalize: %v", err)
	}
	if _, err := Normalize([]byte(`["not an object"]`)); !errors.Is(err, ErrBadCard) {
		t.Errorf("not an object: %v", err)
	}
}

// The a2a-cli v0.3.x exec card (HTTP+JSON only, streaming true, push false) is served with both
// bindings, streaming and push notifications.
func TestA2ACLIExecCard(t *testing.T) {
	served, err := Rewrite(Input{Upstream: read(t, "a2a-cli-exec.json"), PublicURL: "http://localhost:8080", ServicePath: "/agents/hello"})
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "a2a-cli-exec.served.json", served)
	var card a2a.AgentCard
	if err := json.Unmarshal(served, &card); err != nil {
		t.Fatal(err)
	}
	var bindings []string
	for _, i := range card.SupportedInterfaces {
		bindings = append(bindings, string(i.ProtocolBinding)+"@"+i.URL)
	}
	if strings.Join(bindings, " ") != "JSONRPC@http://localhost:8080/agents/hello HTTP+JSON@http://localhost:8080/agents/hello" {
		t.Errorf("interfaces %v", bindings)
	}
	if !card.Capabilities.Streaming || !card.Capabilities.PushNotifications || card.Capabilities.ExtendedAgentCard {
		t.Errorf("capabilities %+v", card.Capabilities)
	}
	if bytes.Contains(served, []byte(`"signatures"`)) {
		t.Error("signatures served in M1")
	}
}

// cardServer serves a card at a base path and can be switched between answers.
type cardServer struct {
	srv   *httptest.Server
	mu    sync.Mutex
	card  string // served at <base>/.well-known/agent-card.json, or agent.json when legacy
	fail  int    // HTTP status to answer instead, 0 for the card
	gets  atomic.Int64
	agent bool // serve at agent.json only
}

func newCardServer(t *testing.T, card string) *cardServer {
	cs := &cardServer{card: card}
	cs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cs.gets.Add(1)
		cs.mu.Lock()
		defer cs.mu.Unlock()
		if cs.fail != 0 {
			w.WriteHeader(cs.fail)
			return
		}
		want := "/svc/.well-known/agent-card.json"
		if cs.agent {
			want = "/svc/.well-known/agent.json"
		}
		if r.URL.Path != want {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(cs.card))
	}))
	t.Cleanup(cs.srv.Close)
	return cs
}

func (cs *cardServer) set(card string, fail int) {
	cs.mu.Lock()
	cs.card, cs.fail = card, fail
	cs.mu.Unlock()
}

func backend(t *testing.T, cs *cardServer) upstream.Backend {
	t.Helper()
	b, err := upstream.NewDirect(cs.srv.URL+"/svc", true)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The card lifecycle: the first fetch is retried until the seller is up, a failed fetch keeps the last
// good card, 30 minutes without a good fetch make the service unavailable, a card over 256 KiB is never
// installed (service_unavailable).
func TestKeeperLifecycle(t *testing.T) {
	cs := newCardServer(t, string(read(t, "a2a-cli-exec.json")))
	cs.set(string(read(t, "a2a-cli-exec.json")), http.StatusServiceUnavailable)
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	var clockMu sync.Mutex
	clock := func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return now }
	advance := func(d time.Duration) { clockMu.Lock(); now = now.Add(d); clockMu.Unlock() }
	var persisted atomic.Int64
	k := NewKeeper(backend(t, cs), Input{PublicURL: "http://localhost:8080", ServicePath: "/agents/hello"},
		WithClock(clock), WithPersist(func(context.Context, *Served, time.Time) error { persisted.Add(1); return nil }))

	if _, err := k.Current(); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("before any fetch: %v", err)
	}
	if _, _, err := k.Dispatch(); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("dispatch before any fetch: %v", err)
	}
	// Run retries the failing first fetch with backoff; the seller comes up after the first attempt.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go k.Run(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for cs.gets.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cs.set(string(read(t, "a2a-cli-exec.json")), 0)
	for time.Now().Before(deadline) {
		if _, err := k.Current(); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	s, err := k.Current()
	if err != nil || s.Unavailable || !bytes.Contains(s.Body, []byte(`"Exec Agent"`)) || s.ETag != ETag(s.Body) {
		t.Fatalf("after the seller came up: %+v %v", s, err)
	}
	b, info, err := k.Dispatch()
	if err != nil || b == nil || info.Binding != a2a.TransportProtocolHTTPJSON || !info.Streaming {
		t.Fatalf("dispatch info %+v %v", info, err)
	}
	if persisted.Load() != 1 {
		t.Errorf("persisted %d times", persisted.Load())
	}
	first := s

	// A refresh of the same bytes changes nothing (no persist, same Last-Modified and ETag).
	advance(10 * time.Minute)
	if err := k.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	s, _ = k.Current()
	if persisted.Load() != 1 || !s.LastModified.Equal(first.LastModified) || s.ETag != first.ETag {
		t.Errorf("an unchanged card: persisted %d, %v / %v", persisted.Load(), s.LastModified, first.LastModified)
	}

	// The seller fails: the last good card stays until 30 minutes after the last good fetch.
	cs.set("", http.StatusBadGateway)
	advance(29 * time.Minute)
	if err := k.Refresh(context.Background()); err == nil {
		t.Fatal("a failing fetch succeeded")
	}
	if s, err := k.Current(); err != nil || s.Unavailable || s.ETag != first.ETag {
		t.Errorf("the last good card was not kept: %+v %v", s, err)
	}
	advance(2 * time.Minute)
	if s, err := k.Current(); !errors.Is(err, ErrUnavailable) || s == nil || !s.Unavailable {
		t.Errorf("31 minutes without a good fetch: %+v %v", s, err)
	}
	if _, _, err := k.Dispatch(); !errors.Is(err, ErrUnavailable) {
		t.Errorf("dispatch on an unavailable service: %v", err)
	}

	// A card over 256 KiB is not installed: the service stays unavailable.
	cs.set(`{"supportedInterfaces":[{"url":"http://x/","protocolBinding":"JSONRPC","protocolVersion":"1.0"}],"pad":"`+strings.Repeat("a", upstream.MaxCard)+`"}`, 0)
	if err := k.Refresh(context.Background()); !errors.Is(err, upstream.ErrLimit) {
		t.Errorf("a card over 256 KiB: %v", err)
	}
	if _, err := k.Current(); !errors.Is(err, ErrUnavailable) {
		t.Errorf("after an oversized card: %v", err)
	}
	// A changed card brings the service back, with a new Last-Modified and ETag, and is persisted.
	cs.set(strings.Replace(string(read(t, "a2a-cli-exec.json")), "Exec Agent", "Exec Agent 2", 1), 0)
	if err := k.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	s, err = k.Current()
	if err != nil || s.ETag == first.ETag || !s.LastModified.After(first.LastModified) || persisted.Load() != 2 {
		t.Errorf("a changed card: %+v %v persisted %d", s, err, persisted.Load())
	}
}

// A 0.2.5 seller has no agent-card.json: the keeper falls back to agent.json and serves 1.0 (card_compat).
func TestKeeperLegacyCard(t *testing.T) {
	cs := newCardServer(t, string(read(t, "v025-helloworld.json")))
	cs.agent = true
	k := NewKeeper(backend(t, cs), Input{PublicURL: "http://localhost:8080", ServicePath: "/agents/hello"})
	if err := k.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	s, err := k.Current()
	if err != nil || !bytes.Equal(s.Upstream, read(t, "v025-helloworld.json")) {
		t.Fatalf("%+v %v", s, err)
	}
	var card a2a.AgentCard
	if err := json.Unmarshal(s.Body, &card); err != nil || len(card.SupportedInterfaces) != 2 || card.SupportedInterfaces[0].ProtocolVersion != "1.0" {
		t.Errorf("served %s", s.Body)
	}
	_, info, err := k.Dispatch()
	if err != nil || info.ProtocolVersion != "0.3" || info.Binding != a2a.TransportProtocolJSONRPC || info.Path != "/" {
		t.Errorf("dispatch speaks to it as %+v %v", info, err)
	}
	// Seed installs a stored card at start.
	k2 := NewKeeper(backend(t, cs), Input{PublicURL: "http://localhost:8080", ServicePath: "/agents/hello"})
	if err := k2.Seed(s.Upstream, time.Now()); err != nil {
		t.Fatal(err)
	}
	if s2, err := k2.Current(); err != nil || s2.ETag != s.ETag {
		t.Errorf("seeded %+v %v", s2, err)
	}
}

// A seller that accepts the card request and never answers does not hold the keeper: each fetch has a
// deadline of its own, a fetch that runs out of it is a failed fetch, and Run's backoff fetches again, so
// the card arrives once the seller answers (spec §3).
func TestKeeperFetchTimeout(t *testing.T) {
	card := read(t, "a2a-cli-exec.json")
	var hang atomic.Bool
	hang.Store(true)
	var gets atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gets.Add(1)
		if hang.Load() {
			<-r.Context().Done() // accepted, never answered
			return
		}
		w.Write(card)
	}))
	t.Cleanup(srv.Close)
	b, err := upstream.NewDirect(srv.URL, true)
	if err != nil {
		t.Fatal(err)
	}
	k := NewKeeper(b, Input{PublicURL: "http://localhost:8080", ServicePath: "/agents/hello"}, WithFetchTimeout(200*time.Millisecond))
	start := time.Now()
	if err := k.Refresh(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a fetch the seller never answers: %v", err)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("the fetch returned after %v", took)
	}
	if _, err := k.Current(); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("no card yet: %v", err)
	}

	// Run: the first fetch hangs and times out; the seller recovers; the backoff fetches the card.
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { defer close(stopped); k.Run(ctx) }()
	defer func() { cancel(); <-stopped }()
	before := gets.Load()
	deadline := time.Now().Add(5 * time.Second)
	for gets.Load() == before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	hang.Store(false)
	for time.Now().Before(deadline) {
		if _, err := k.Current(); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the keeper never fetched the card after the seller recovered (%d requests)", gets.Load())
}
