package enroll

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"kontra.sh/kontra/internal/audit"
	"kontra.sh/kontra/internal/tunnel"
)

var tokenShape = regexp.MustCompile(`^ket_[0-9A-Za-z]{32}$`)

// A token is ket_ and 32 base62 characters; its stored form is sha256 of its bytes.
func TestTokenFormat(t *testing.T) {
	seen := map[string]bool{}
	for range 200 {
		tok, err := newToken(nil)
		if err != nil {
			t.Fatal(err)
		}
		if !tokenShape.MatchString(tok) || !ValidToken(tok) {
			t.Fatalf("token %q", tok)
		}
		if seen[tok] {
			t.Fatalf("token %q twice", tok)
		}
		seen[tok] = true
		if h := TokenHash(tok); h != sha256.Sum256([]byte(tok)) {
			t.Fatal("TokenHash is not sha256 of the token")
		}
	}
	// Every base62 character is reachable and nothing else: a reader of all-0..255 bytes covers the range.
	all := make([]byte, 64)
	for i := range all {
		all[i] = byte(i * 4)
	}
	tok, err := newToken(bytes.NewReader(append(all, all...)))
	if err != nil || !ValidToken(tok) {
		t.Fatalf("token from a fixed reader: %q %v", tok, err)
	}
	if _, err := newToken(bytes.NewReader(nil)); err == nil {
		t.Error("a reader with no randomness gave a token")
	}
	for _, bad := range []string{"", "ket_", "ket_" + strings.Repeat("a", 31), "ket_" + strings.Repeat("a", 33),
		"KET_" + strings.Repeat("a", 32), "ket_" + strings.Repeat("a", 31) + "-", " ket_" + strings.Repeat("a", 32)} {
		if ValidToken(bad) {
			t.Errorf("ValidToken(%q)", bad)
		}
	}
}

// A hello's tokens are checked for shape before any query, deduplicated, and consumed in sorted order.
func TestTokenHashes(t *testing.T) {
	a, b := "ket_"+strings.Repeat("a", 32), "ket_"+strings.Repeat("b", 32)
	hs, ok := tokenHashes([]string{b, a, b})
	if !ok || len(hs) != 2 {
		t.Fatalf("tokenHashes = %d, %v", len(hs), ok)
	}
	if bytes.Compare(hs[0][:], hs[1][:]) >= 0 {
		t.Error("hashes not sorted")
	}
	if _, ok := tokenHashes([]string{a, "ket_short"}); ok {
		t.Error("a malformed token was accepted")
	}
	if hs, ok := tokenHashes(nil); !ok || len(hs) != 0 {
		t.Error("no tokens")
	}
}

type testKey struct {
	priv      *ecdh.PrivateKey
	pub       []byte
	recipient string
	kid       string
}

func newTestKey(t testing.TB) *testKey {
	t.Helper()
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	r, err := tunnel.FormatRecipient(priv.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	return &testKey{priv: priv, pub: priv.PublicKey().Bytes(), recipient: r, kid: tunnel.KeyID(r)}
}

func (k *testKey) ref() *tunnel.KeyRef { return &tunnel.KeyRef{KID: k.kid, Recipient: k.recipient} }

// Admit refuses a request whose decoded keys are not the hello's: bad_proof, before any query.
func TestCheckRequest(t *testing.T) {
	a, b := newTestKey(t), newTestKey(t)
	h := &tunnel.Hello{KID: a.kid, Recipient: a.recipient, Name: "n"}
	if err := checkRequest(Request{Hello: h, PubKey: a.pub}); err != nil {
		t.Fatalf("a matching request: %v", err)
	}
	cases := map[string]Request{
		"another public key":          {Hello: h, PubKey: b.pub},
		"a short public key":          {Hello: h, PubKey: a.pub[:31]},
		"a kid of another recipient":  {Hello: &tunnel.Hello{KID: b.kid, Recipient: a.recipient}, PubKey: a.pub},
		"rotate_from without its key": {Hello: &tunnel.Hello{KID: a.kid, Recipient: a.recipient, RotateFrom: b.ref()}, PubKey: a.pub},
		"rotate_from with another key": {Hello: &tunnel.Hello{KID: a.kid, Recipient: a.recipient, RotateFrom: b.ref()}, PubKey: a.pub,
			OldPubKey: a.pub},
	}
	for name, req := range cases {
		var herr *tunnel.HandshakeError
		if err := checkRequest(req); !errors.As(err, &herr) || herr.Code != tunnel.CodeBadProof {
			t.Errorf("%s: %v, want bad_proof", name, err)
		}
	}
	if err := checkRequest(Request{Hello: h, PubKey: a.pub, OldPubKey: b.pub}); err == nil {
		t.Error("an old key without rotate_from was accepted")
	}
	if err := checkRequest(Request{}); err == nil {
		t.Error("no hello was accepted")
	}
}

// Welcome URLs follow relay.md §2.1: /agents/ for A2A and /mcp/ for MCP, with the tenant on a multi-tenant
// gate; a trailing slash on public_url is not doubled.
func TestServiceURL(t *testing.T) {
	team := NewStore(nil, Options{PublicURL: "https://gate.example/"})
	multi := NewStore(nil, Options{PublicURL: "https://gate.example", Multi: true})
	for _, c := range []struct {
		s          *Store
		kind, want string
	}{
		{team, tunnel.KindA2A, "https://gate.example/agents/hello"},
		{team, tunnel.KindMCP, "https://gate.example/mcp/hello"},
		{multi, tunnel.KindA2A, "https://gate.example/agents/acme/hello"},
		{multi, tunnel.KindMCP, "https://gate.example/mcp/acme/hello"},
	} {
		if got := c.s.serviceURL(c.kind, "acme", "hello"); got != c.want {
			t.Errorf("serviceURL(%s) = %q, want %q", c.kind, got, c.want)
		}
	}
}

// The per-service advisory locks are never the audit chain's (or any other "kontra"-prefixed) lock, and
// differ by tenant and by name.
func TestServiceLock(t *testing.T) {
	t1, t2 := uuid.New(), uuid.New()
	a, b, c := serviceLock(t1, "hello"), serviceLock(t1, "recon"), serviceLock(t2, "hello")
	if a == b || a == c {
		t.Error("service locks collide")
	}
	for _, l := range []int64{a, b, c} {
		if l>>32 != 0x6b747376 || l == audit.LockID {
			t.Errorf("lock %#x outside its space", l)
		}
	}
}

// The view emits each revocation once, whether it learns it from a NOTIFY or a re-read; a rotated key is
// usable until its overlap passes; a binding the view no longer holds is not revoked.
func TestRevocationsView(t *testing.T) {
	r := NewRevocations(nil)
	var got []Revocation
	r.Subscribe(func(rev Revocation) { got = append(got, rev) })
	now := time.Now()
	if !r.KeyUsable("k1_aaaaaaaaaaaaaaaaaaaaaaaaaa", now) || r.BindingRevoked("k1_aaaaaaaaaaaaaaaaaaaaaaaaaa", "hello") {
		t.Fatal("an unknown key is not usable")
	}
	at := now.Add(-time.Second)
	r.emit(r.markKey("k1_a", &at, ReasonOperator, nil))
	r.emit(r.markKey("k1_a", &at, ReasonOperator, nil))
	r.emit(r.markKey("k1_a", nil, "", nil))
	if len(got) != 1 || got[0] != (Revocation{KeyID: "k1_a", Reason: ReasonOperator}) {
		t.Fatalf("emitted %+v", got)
	}
	if r.KeyUsable("k1_a", now) {
		t.Error("a revoked key is usable")
	}
	// A NOTIFY whose row could not be read marks the key revoked with no reason.
	r.emit(r.markKey("k1_b", nil, "", nil))
	if len(got) != 2 || got[1] != (Revocation{KeyID: "k1_b"}) || r.KeyUsable("k1_b", now) {
		t.Fatalf("emitted %+v", got)
	}
	// A rotated key inside its overlap: usable, nothing emitted; past it, not usable; its revocation (the
	// sweep) is emitted once with reason rotated.
	until := now.Add(time.Hour)
	r.emit(r.markKey("k1_c", nil, ReasonRotated, &until))
	if len(got) != 2 || !r.KeyUsable("k1_c", now) || r.KeyUsable("k1_c", until) {
		t.Fatalf("overlap: emitted %+v", got)
	}
	r.emit(r.markKey("k1_c", &until, ReasonRotated, &until))
	if len(got) != 3 || got[2] != (Revocation{KeyID: "k1_c", Reason: ReasonRotated}) || r.KeyUsable("k1_c", now) {
		t.Fatalf("overlap ended: emitted %+v", got)
	}
	// Bindings.
	r.emit(r.markBinding(bindingRef{"k1_d", "hello"}, &at))
	r.emit(r.markBinding(bindingRef{"k1_d", "hello"}, nil))
	if len(got) != 4 || got[3] != (Revocation{KeyID: "k1_d", Service: "hello"}) || !r.BindingRevoked("k1_d", "hello") ||
		r.BindingRevoked("k1_d", "recon") || !r.KeyUsable("k1_d", now) {
		t.Fatalf("binding: emitted %+v", got)
	}
	// The same binding revoked again (enrolled in between, which sends no NOTIFY): another revoked_at is
	// another revocation, emitted once; the same revoked_at, or none, is not.
	again := at.Add(time.Second)
	r.emit(r.markBinding(bindingRef{"k1_d", "hello"}, &again))
	r.emit(r.markBinding(bindingRef{"k1_d", "hello"}, &again))
	r.emit(r.markBinding(bindingRef{"k1_d", "hello"}, nil))
	if len(got) != 5 || got[4] != (Revocation{KeyID: "k1_d", Service: "hello"}) {
		t.Fatalf("binding revoked again: emitted %+v", got)
	}
	// Held from a NOTIFY payload (no time yet): emitted once, when its row dates it.
	r.mu.Lock()
	r.holdBindingLocked(bindingRef{"k1_e", "hello"})
	r.mu.Unlock()
	if len(got) != 5 || !r.BindingRevoked("k1_e", "hello") {
		t.Fatalf("held binding: emitted %+v", got)
	}
	r.emit(r.markBinding(bindingRef{"k1_e", "hello"}, &at))
	r.emit(r.markBinding(bindingRef{"k1_e", "hello"}, &at))
	if len(got) != 6 || got[5] != (Revocation{KeyID: "k1_e", Service: "hello"}) {
		t.Fatalf("held binding dated: emitted %+v", got)
	}
	// A subscriber that panics does not stop the others.
	r.Subscribe(func(Revocation) { panic("boom") })
	var after int
	r.Subscribe(func(Revocation) { after++ })
	r.emit(r.markBinding(bindingRef{"k1_d", "recon"}, nil))
	if after != 1 {
		t.Error("a panicking subscriber stopped the next one")
	}
	// Fresh counts from the last successful read.
	if r.Fresh() {
		t.Error("fresh before any read")
	}
	r.lastOK = now
	r.now = func() time.Time { return now.Add(RevocationRefresh - time.Millisecond) }
	if !r.Fresh() {
		t.Error("not fresh within the interval")
	}
	r.now = func() time.Time { return now.Add(RevocationRefresh) }
	if r.Fresh() {
		t.Error("fresh past the interval")
	}
}

// Notify ignores payloads that are not the tunnel's (API keys are auth's) or that cannot be read.
func TestRevocationsNotifyIgnores(t *testing.T) {
	r := NewRevocations(nil)
	n := 0
	r.Subscribe(func(Revocation) { n++ })
	for _, p := range []string{"apikey:" + uuid.NewString(), "key:", "key:k1_short", "binding:k1_aaaaaaaaaaaaaaaaaaaaaaaaaa",
		"binding:k1_aaaaaaaaaaaaaaaaaaaaaaaaaa/Bad_Name", "nonsense", ""} {
		r.Notify(p) // a nil pool would panic if any of these reached a query
	}
	r.drainers.Wait()
	if n != 0 || r.draining || len(r.pending) != 0 || len(r.keys) != 0 || len(r.bindings) != 0 {
		t.Errorf("%d revocations, %d queued, %d keys and %d bindings held from unreadable payloads", n, len(r.pending),
			len(r.keys), len(r.bindings))
	}
	if KeyRevokePayload("k1_x") != "key:k1_x" || BindingRevokePayload("k1_x", "hello") != "binding:k1_x/hello" {
		t.Error("payload format")
	}
}
