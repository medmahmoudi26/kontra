package card

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2acrypto"

	"kontra.sh/kontra/internal/jcs"
	"kontra.sh/kontra/internal/jws"
	"kontra.sh/kontra/internal/strictjson"
)

// sellerSigned returns card with a signatures member holding one signature by the seller's key.
func sellerSigned(t *testing.T, seller *jws.KeySet, card string) string {
	t.Helper()
	sig, err := seller.Sign([]byte(card), "https://seller.example/.well-known/jwks.json", nil)
	if err != nil {
		t.Fatal(err)
	}
	sb, _ := json.Marshal(sig)
	b, err := signed([]byte(card), sb)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// signatures returns the served card's signatures.
func signatures(t *testing.T, body []byte) []jws.Signature {
	t.Helper()
	m, err := strictjson.Members(body)
	if err != nil {
		t.Fatal(err)
	}
	var sigs []jws.Signature
	if err := json.Unmarshal(m["signatures"], &sigs); err != nil {
		t.Fatalf("signatures %s: %v", m["signatures"], err)
	}
	return sigs
}

// protectedHeader decodes a signature's protected header.
func protectedHeader(t *testing.T, sig jws.Signature) map[string]json.RawMessage {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(sig.Protected)
	if err != nil {
		t.Fatal(err)
	}
	m, err := strictjson.Members(b)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// jwksServer serves the gate's JWKS through JWKSHandler.
func jwksServer(t *testing.T, keys *jws.KeySet) *httptest.Server {
	t.Helper()
	h, err := JWKSHandler(keys)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle(jws.JWKSPath, h)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// The served card is signed by the gate (spec §3, D13, D166): exactly one signature, which a2a-go's own verifier
// accepts once it resolves the jku to the gate's JWKS; its protected header binds the upstream card (the hex
// SHA-256 of its JCS, the URL it is served at) and says whether a seller signature verified. Seller fields are
// byte-identical under JCS, and signing is deterministic: the same card, the same bytes, the same ETag.
func TestKeeperSignsTheServedCard(t *testing.T) {
	gate, err := jws.EphemeralKeySet()
	if err != nil {
		t.Fatal(err)
	}
	seller, _ := jws.EphemeralKeySet()
	srv := jwksServer(t, gate)
	jku := srv.URL + jws.JWKSPath
	upstreamCard := sellerSigned(t, seller, string(read(t, "v1-vendor.json")))
	cs := newCardServer(t, upstreamCard)
	in := Input{PublicURL: "https://gate.example", ServicePath: "/agents/hello"}
	k := NewKeeper(backend(t, cs), in, WithSigner(&JWSSigner{Keys: gate, JWKSURL: jku}))
	if err := k.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	s, err := k.Current()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(s.Upstream, []byte(upstreamCard)) {
		t.Fatal("the upstream card is not kept byte for byte, seller signature included")
	}
	sigs := signatures(t, s.Body)
	if len(sigs) != 1 {
		t.Fatalf("%d signatures on the served card, want exactly the gate's", len(sigs))
	}
	// a2a-go's verifier, with the jku on its allowlist.
	v := a2acrypto.NewVerifier(a2acrypto.VerifierConfig{KeyResolver: a2acrypto.NewJWKSKeyResolver(srv.Client(), []string{jku})})
	if err := v.Verify(context.Background(), s.Body, &a2a.AgentCardSignature{Protected: sigs[0].Protected, Signature: sigs[0].Signature}); err != nil {
		t.Fatalf("a2a-go's verifier refuses the gate's signature: %v", err)
	}
	// And the gate's own.
	pub, err := jws.ParseJWKS(mustJWKS(t, gate))
	if err != nil {
		t.Fatal(err)
	}
	h, err := jws.Verify(s.Body, sigs[0], pub)
	if err != nil {
		t.Fatal(err)
	}
	canon, _ := jcs.Transform([]byte(upstreamCard))
	sum := sha256.Sum256(canon)
	if h.Upstream == nil || h.Upstream.SHA256 != hex.EncodeToString(sum[:]) || h.Upstream.URL != "https://gate.example/agents/hello/.well-known/agent-card.upstream.json" ||
		h.Upstream.Verified || h.JKU != jku || h.Alg != "ES256" || h.Typ != "JOSE" {
		t.Fatalf("protected header %+v %+v", h, h.Upstream)
	}
	if s.Hash != sum || s.Verified {
		t.Errorf("Served.Hash %x Verified %v", s.Hash, s.Verified)
	}
	if hdr := protectedHeader(t, sigs[0]); hdr["crit"] != nil {
		t.Error("kontra.upstream must never be critical")
	}
	// Seller fields are byte-identical under JCS: the served card without the gate's fields equals the upstream
	// card without them (v1-vendor.json is a 1.0 card).
	strip := func(b []byte) []byte {
		m, err := strictjson.Members(b)
		if err != nil {
			t.Fatal(err)
		}
		for _, k := range []string{"supportedInterfaces", "securitySchemes", "securityRequirements", "capabilities", "signatures"} {
			delete(m, k)
		}
		raw, _ := json.Marshal(m)
		out, err := jcs.Transform(raw)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if !bytes.Equal(strip(s.Body), strip([]byte(upstreamCard))) {
		t.Errorf("seller fields differ under JCS:\nserved   %s\nupstream %s", strip(s.Body), strip([]byte(upstreamCard)))
	}
	// Deterministic: a refresh of the same bytes serves the same body and ETag, and so does another keeper (another
	// replica) with the same key.
	if err := k.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	again, _ := k.Current()
	k2 := NewKeeper(backend(t, cs), in, WithSigner(&JWSSigner{Keys: gate, JWKSURL: jku}))
	if err := k2.Seed([]byte(upstreamCard), again.LastModified); err != nil {
		t.Fatal(err)
	}
	other, _ := k2.Current()
	if !bytes.Equal(again.Body, s.Body) || again.ETag != s.ETag || other.ETag != s.ETag {
		t.Errorf("signing is not deterministic: ETags %s %s %s", s.ETag, again.ETag, other.ETag)
	}
}

func mustJWKS(t *testing.T, k *jws.KeySet) []byte {
	t.Helper()
	b, err := k.JWKS()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// UpstreamHash is the SHA-256 of the upstream card's JCS, whatever its formatting.
func TestUpstreamHash(t *testing.T) {
	a, err := UpstreamHash([]byte(`{"b": 1, "a": "x"}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := UpstreamHash([]byte("{\"a\":\"x\",\n\"b\":1.0}"))
	if a != b || a != sha256.Sum256([]byte(`{"a":"x","b":1}`)) {
		t.Errorf("UpstreamHash %x / %x", a, b)
	}
	if _, err := UpstreamHash([]byte(`{"a":1,"a":2}`)); err == nil {
		t.Error("a card with a repeated key hashed")
	}
}

// GET /.well-known/jwks.json (spec §2.1, D166): every key of the set, active first, Cache-Control max-age=300;
// a conditional GET revalidates; other methods are the closed router's empty 404.
func TestJWKSHandler(t *testing.T) {
	keys, _ := jws.EphemeralKeySet()
	srv := jwksServer(t, keys)
	resp, err := http.Get(srv.URL + jws.JWKSPath)
	if err != nil {
		t.Fatal(err)
	}
	var set struct {
		Keys []struct {
			Kid, Kty, Crv, Alg, Use string
		} `json:"keys"`
	}
	err = json.NewDecoder(resp.Body).Decode(&set)
	resp.Body.Close()
	kid, _ := keys.KeyID()
	if err != nil || resp.StatusCode != 200 || resp.Header.Get("Cache-Control") != "max-age=300" || len(set.Keys) != 1 ||
		set.Keys[0].Kid != kid || set.Keys[0].Alg != "ES256" || set.Keys[0].Use != "sig" {
		t.Fatalf("JWKS: %d %v %+v %v", resp.StatusCode, resp.Header, set, err)
	}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+jws.JWKSPath, nil)
	req.Header.Set("If-None-Match", resp.Header.Get("ETag"))
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusNotModified {
		t.Errorf("conditional GET: %v %v", resp, err)
	}
	if resp, err := http.Post(srv.URL+jws.JWKSPath, "application/json", nil); err != nil || resp.StatusCode != http.StatusNotFound {
		t.Errorf("POST: %v %v", resp, err)
	}
}
