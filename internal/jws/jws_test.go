package jws

import (
	"context"
	"crypto/ecdsa"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2acrypto"

	"kontra.sh/kontra/internal/jcs"
)

// The thumbprints of testdata/two-keys.pem's keys, computed outside this package (Python's cryptography: the
// public numbers as RFC 7638's {"crv","kty","x","y"} JSON, SHA-256, base64url). The keys are throwaway test keys.
const (
	kidA = "H6iLEQakd7DgMhgl7LZumiX9m-eS3Vn_HlALDWqavuI" // PKCS #8, first: active
	kidB = "TCob1fuytnFJAKfawLkodm9D-2LL-MXc2-f299tO18k" // SEC 1, after an EC PARAMETERS block
)

// RFC 9449 §6.1's DPoP key and its RFC 7638 thumbprint ("jkt"), a published P-256 vector.
const (
	rfc9449X   = "l8tFrhx-34tV3hRICRDY9zCkDlpBhF42UQUfWVAWBFs"
	rfc9449Y   = "9VE4jf_Ok_o64zbTTlcuNJajHmt6v9TDVrU0CdvGRDA"
	rfc9449JKT = "0ZcOCORZNYy-DWpqq30jZyJGHTN0d2HglBV3uiguA4I"
)

// card is an A2A 1.0 card with the things JCS changes: number forms, an escape, non-ASCII text, key order.
const card = `{
  "name": "Echo",
  "description": "echoes <things> & stuff   é 😀",
  "version": "1.0.0",
  "supportedInterfaces": [{"url": "https://gate.example/echo", "protocolBinding": "JSONRPC", "protocolVersion": "1.0"}],
  "capabilities": {"streaming": true, "extensions": []},
  "defaultInputModes": ["text/plain"],
  "defaultOutputModes": ["text/plain"],
  "skills": [{"id": "echo", "name": "Echo", "description": "x", "tags": ["t"]}],
  "price": 1.50,
  "big": 1E21
}`

// keyFile copies a testdata file to a fresh directory with mode.
func keyFile(t *testing.T, name string, mode os.FileMode) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return writeKey(t, b, mode)
}

func writeKey(t *testing.T, b []byte, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "kontra.key")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadKeySetTwoKeys(t *testing.T) {
	ks, err := LoadKeySet(keyFile(t, "two-keys.pem", 0o600))
	if err != nil {
		t.Fatal(err)
	}
	kid, err := ks.KeyID()
	if err != nil || kid != kidA {
		t.Fatalf("KeyID = %q, %v; want the first key's thumbprint %q", kid, err, kidA)
	}
	if got := ks.KeyIDs(); len(got) != 2 || got[0] != kidA || got[1] != kidB {
		t.Fatalf("KeyIDs = %v, want [%s %s]", got, kidA, kidB)
	}

	b, err := ks.JWKS()
	if err != nil {
		t.Fatal(err)
	}
	var set struct {
		Keys []map[string]string `json:"keys"`
	}
	if err := json.Unmarshal(b, &set); err != nil {
		t.Fatal(err)
	}
	if len(set.Keys) != 2 || set.Keys[0]["kid"] != kidA || set.Keys[1]["kid"] != kidB {
		t.Fatalf("JWKS keys not active first: %s", b)
	}
	for _, k := range set.Keys {
		if len(k) != 7 || k["kty"] != "EC" || k["crv"] != "P-256" || k["use"] != "sig" || k["alg"] != "ES256" ||
			len(k["x"]) != 43 || len(k["y"]) != 43 {
			t.Fatalf("JWK %v: want exactly kty EC, crv P-256, x, y, kid, use sig, alg ES256", k)
		}
		if _, hasD := k["d"]; hasD {
			t.Fatal("JWKS publishes a private key")
		}
	}
	// The published set verifies the RFC 7638 kids independently of the loader.
	pub, err := ParseJWKS(b)
	if err != nil {
		t.Fatal(err)
	}
	tps, err := pub.Thumbprints()
	if err != nil || len(tps) != 2 || tps[0] != kidA || tps[1] != kidB {
		t.Fatalf("Thumbprints = %v, %v", tps, err)
	}
}

func TestThumbprintRFC9449Vector(t *testing.T) {
	set, err := ParseJWKS([]byte(`{"keys":[{"kty":"EC","crv":"P-256","x":"` + rfc9449X + `","y":"` + rfc9449Y + `"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	tps, err := set.Thumbprints()
	if err != nil || len(tps) != 1 || tps[0] != rfc9449JKT {
		t.Fatalf("Thumbprints = %v, %v; want [%s]", tps, err, rfc9449JKT)
	}
	// A key without kid is found by its thumbprint.
	if _, ok := set.keys[rfc9449JKT]; !ok {
		t.Fatal("a JWK without kid is not indexed by its thumbprint")
	}
}

func TestLoadKeySetRefusals(t *testing.T) {
	two, err := os.ReadFile(filepath.Join("testdata", "two-keys.pem"))
	if err != nil {
		t.Fatal(err)
	}
	p384, err := os.ReadFile(filepath.Join("testdata", "p384.pem"))
	if err != nil {
		t.Fatal(err)
	}
	pubPEM := []byte("-----BEGIN PUBLIC KEY-----\nMFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE\n-----END PUBLIC KEY-----\n")
	for _, c := range []struct {
		name string
		data []byte
		mode os.FileMode
		want string
	}{
		{"others read", two, 0o604, "mode 0604"},
		{"group write", two, 0o620, "mode 0620"},
		{"world", two, 0o644, "mode 0644"},
		{"P-384", p384, 0o600, "not P-256"},
		{"P-256 then P-384", append(append([]byte{}, two...), p384...), 0o600, "not P-256"},
		{"empty", nil, 0o600, "no ECDSA P-256 private key"},
		{"public key block", pubPEM, 0o600, `"PUBLIC KEY"`},
		{"trailing data", append(append([]byte{}, two...), "junk"...), 0o600, "data after the last PEM block"},
		{"repeated key", append(append([]byte{}, two...), two...), 0o600, "repeats key"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := LoadKeySet(writeKey(t, c.data, c.mode))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("LoadKeySet: %v, want an error containing %q", err, c.want)
			}
			if strings.Contains(err.Error(), "PRIVATE KEY-----") || strings.Contains(err.Error(), "MIG") {
				t.Fatalf("the error quotes the key file: %v", err)
			}
		})
	}
	if _, err := LoadKeySet(t.TempDir()); err == nil {
		t.Fatal("a directory loads")
	}
}

func TestLoadKeySetAcceptedModes(t *testing.T) {
	for _, mode := range []os.FileMode{0o600, 0o400, 0o640, 0o440} {
		if _, err := LoadKeySet(keyFile(t, "two-keys.pem", mode)); err != nil {
			t.Fatalf("mode %04o: %v", mode, err)
		}
	}
	// A Kubernetes secret volume: kontra.key -> ..data/kontra.key, the target 0440.
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "..data"), 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join("testdata", "two-keys.pem"))
	target := filepath.Join(dir, "..data", "kontra.key")
	if err := os.WriteFile(target, b, 0o440); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..data", "kontra.key"), filepath.Join(dir, "kontra.key")); err != nil {
		t.Fatal(err)
	}
	ks, err := LoadKeySet(filepath.Join(dir, "kontra.key"))
	if err != nil {
		t.Fatalf("a mounted secret: %v", err)
	}
	if kid, _ := ks.KeyID(); kid != kidA {
		t.Fatalf("KeyID = %s", kid)
	}
}

func TestEphemeralKeySet(t *testing.T) {
	a, err := EphemeralKeySet()
	if err != nil {
		t.Fatal(err)
	}
	b, err := EphemeralKeySet()
	if err != nil {
		t.Fatal(err)
	}
	ka, _ := a.KeyID()
	kb, _ := b.KeyID()
	if ka == "" || ka == kb {
		t.Fatalf("ephemeral kids %q and %q", ka, kb)
	}
	sig, err := a.Sign([]byte(card), "http://127.0.0.1:8080"+JWKSPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	pa := publicOf(t, a)
	if _, err := Verify([]byte(card), sig, pa); err != nil {
		t.Fatalf("own JWKS: %v", err)
	}
	if _, err := Verify([]byte(card), sig, publicOf(t, b)); err == nil {
		t.Fatal("another process's key set verifies the card")
	}
}

func publicOf(t *testing.T, ks *KeySet) *KeySetPublic {
	t.Helper()
	b, err := ks.JWKS()
	if err != nil {
		t.Fatal(err)
	}
	p, err := ParseJWKS(b)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func loaded(t *testing.T) *KeySet {
	t.Helper()
	ks, err := LoadKeySet(keyFile(t, "two-keys.pem", 0o600))
	if err != nil {
		t.Fatal(err)
	}
	return ks
}

var upstream = &Upstream{
	SHA256:   "3f1e0c9d2b4a5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f9a0b1c2d3e4f",
	URL:      "https://gate.example/echo/.well-known/agent-card.upstream.json",
	Verified: true,
}

// served is the card as the gate serves it: with its one signature.
func served(t *testing.T, sig Signature) []byte {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(card), &m); err != nil {
		t.Fatal(err)
	}
	s, _ := json.Marshal([]Signature{sig})
	m["signatures"] = s
	b, _ := json.Marshal(m)
	return b
}

// TestSignVerifiesWithA2AGo: a2a-go v2.7.0's verifier, resolving the jku through its allowlisting JWKS resolver
// to a server serving JWKS(), accepts the gate's signature, and kontra.upstream is in the protected header and
// not in crit.
func TestSignVerifiesWithA2AGo(t *testing.T) {
	ks := loaded(t)
	jwks, err := ks.JWKS()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != JWKSPath {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", JWKSCacheControl)
		w.Write(jwks)
	}))
	defer srv.Close()
	if !strings.HasPrefix(srv.URL, "http://127.0.0.1:") {
		t.Fatalf("test server on %s, want loopback", srv.URL)
	}
	jku := srv.URL + JWKSPath

	sig, err := ks.Sign([]byte(card), jku, upstream)
	if err != nil {
		t.Fatal(err)
	}
	full := served(t, sig)

	v := a2acrypto.NewVerifier(a2acrypto.VerifierConfig{KeyResolver: a2acrypto.NewJWKSKeyResolver(srv.Client(), []string{jku})})
	if err := v.Verify(context.Background(), full, &a2a.AgentCardSignature{Protected: sig.Protected, Signature: sig.Signature}); err != nil {
		t.Fatalf("a2a-go's verifier refuses the gate's signature: %v", err)
	}
	// And a2a-go's card parser reads the served card with its signature.
	var parsed a2a.AgentCard
	if err := json.Unmarshal(full, &parsed); err != nil || len(parsed.Signatures) != 1 {
		t.Fatalf("a2a-go parses the served card: %v, %d signatures", err, len(parsed.Signatures))
	}

	hb, err := base64.RawURLEncoding.DecodeString(sig.Protected)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := jcs.Canonical(hb); !ok || err != nil {
		t.Fatalf("Sign's protected header is not JCS: %s", hb)
	}
	var hdr map[string]json.RawMessage
	if err := json.Unmarshal(hb, &hdr); err != nil {
		t.Fatal(err)
	}
	if _, ok := hdr["crit"]; ok {
		t.Fatalf("crit in the protected header: %s", hb)
	}
	for _, k := range []string{"alg", "kid", "typ", "jku", HeaderUpstream} {
		if _, ok := hdr[k]; !ok {
			t.Fatalf("%s missing from the protected header: %s", k, hb)
		}
	}
	if len(hdr) != 5 {
		t.Fatalf("protected header has other members: %s", hb)
	}

	h, err := Verify(full, sig, publicOf(t, ks))
	if err != nil {
		t.Fatal(err)
	}
	if h.Alg != Alg || h.Kid != kidA || h.Typ != Typ || h.JKU != jku || h.Upstream == nil || *h.Upstream != *upstream {
		t.Fatalf("Verify header = %+v", h)
	}
	// The signature is ES256's R ‖ S, 64 bytes.
	if raw, _ := base64.RawURLEncoding.DecodeString(sig.Signature); len(raw) != 64 {
		t.Fatalf("signature is %d bytes", len(raw))
	}
}

// TestVerifyAcceptsA2AGoSigner: a signature a2a-go's ES256 signer produced, whose protected header json.Marshal
// wrote (HTML-escaped, so not JCS), verifies.
func TestVerifyAcceptsA2AGoSigner(t *testing.T) {
	ks := loaded(t)
	jku := "https://gate.example/.well-known/jwks.json?a=1&b=<2>"
	signer, err := a2acrypto.NewSigner(a2acrypto.SignerConfig{PrivateKey: ks.keys[0], KeyID: kidA, JWKSURL: jku})
	if err != nil {
		t.Fatal(err)
	}
	asig, err := signer.Sign(context.Background(), json.RawMessage(card))
	if err != nil {
		t.Fatal(err)
	}
	hb, _ := base64.RawURLEncoding.DecodeString(asig.Protected)
	if ok, _ := jcs.Canonical(hb); ok {
		t.Fatalf("a2a-go's header is JCS (%s): the test needs one that is not", hb)
	}
	sig := Signature{Protected: asig.Protected, Signature: asig.Signature}
	h, err := Verify([]byte(card), sig, publicOf(t, ks))
	if err != nil {
		t.Fatalf("Verify refuses a2a-go's signature: %v", err)
	}
	if h.Kid != kidA || h.JKU != jku || h.Typ != "JOSE" || h.Upstream != nil {
		t.Fatalf("header = %+v", h)
	}
	// The served form (with signatures) verifies the same way.
	if _, err := Verify(served(t, sig), sig, publicOf(t, ks)); err != nil {
		t.Fatal(err)
	}
}

// signRaw signs protected (any bytes) over card with key, as a forger holding the key would.
func signRaw(t *testing.T, key *ecdsa.PrivateKey, protected []byte, c string) Signature {
	t.Helper()
	payload, err := CardPayload([]byte(c))
	if err != nil {
		t.Fatal(err)
	}
	p := base64.RawURLEncoding.EncodeToString(protected)
	d := sha256.Sum256([]byte(p + "." + base64.RawURLEncoding.EncodeToString(payload)))
	r, s, err := ecdsa.Sign(rand.Reader, key, d[:])
	if err != nil {
		t.Fatal(err)
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return Signature{Protected: p, Signature: base64.RawURLEncoding.EncodeToString(sig)}
}

func TestVerifyRefusals(t *testing.T) {
	ks := loaded(t)
	pub := publicOf(t, ks)
	key := ks.keys[0]
	good, err := ks.Sign([]byte(card), "https://gate.example"+JWKSPath, upstream)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Verify([]byte(card), good, pub); err != nil {
		t.Fatal(err)
	}
	hb, _ := base64.RawURLEncoding.DecodeString(good.Protected)

	// One byte of the card changed: "Echo" -> "Echp".
	tampered := strings.Replace(card, `"name": "Echo"`, `"name": "Echp"`, 1)

	reencode := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	withProtected := func(p string) Signature { return Signature{Protected: p, Signature: good.Signature} }
	otherKid := strings.Replace(string(hb), kidA, kidB, 1)
	unknownKid := strings.Replace(string(hb), kidA, rfc9449JKT, 1)
	hmacSig := func() Signature {
		p := reencode(`{"alg":"HS256","kid":"` + kidA + `"}`)
		payload, _ := CardPayload([]byte(card))
		m := hmac.New(sha256.New, []byte("secret"))
		m.Write([]byte(p + "." + base64.RawURLEncoding.EncodeToString(payload)))
		return Signature{Protected: p, Signature: base64.RawURLEncoding.EncodeToString(m.Sum(nil))}
	}()

	for _, c := range []struct {
		name string
		card string
		sig  Signature
		want string
	}{
		{"card byte", tampered, good, "bad signature"},
		{"header byte", card, withProtected(reencode(strings.Replace(string(hb), `"typ":"JOSE"`, `"typ":"JOSF"`, 1))), "bad signature"},
		{"upstream flag", card, withProtected(reencode(strings.Replace(string(hb), `"verified":true`, `"verified":false`, 1))), "bad signature"},
		{"kid of the other key", card, withProtected(reencode(otherKid)), "bad signature"},
		{"unknown kid", card, withProtected(reencode(unknownKid)), "no key for kid"},
		{"signature byte", card, Signature{Protected: good.Protected, Signature: flipLast(good.Signature)}, "bad signature"},
		{"short signature", card, Signature{Protected: good.Protected, Signature: good.Signature[:40]}, "64 bytes"},
		{"padded base64", card, Signature{Protected: good.Protected + "=", Signature: good.Signature}, "base64url"},
		{"alg none", card, Signature{Protected: reencode(`{"alg":"none","kid":"` + kidA + `"}`), Signature: ""}, `"none"`},
		{"alg HS256", card, hmacSig, `"HS256"`},
		{"no alg", card, signRaw(t, key, []byte(`{"kid":"`+kidA+`"}`), card), "no alg"},
		{"crit", card, signRaw(t, key, []byte(`{"alg":"ES256","crit":["kontra.upstream"],"kid":"`+kidA+`","kontra.upstream":{"sha256":"x","url":"y","verified":true}}`), card), "crit"},
		{"repeated alg", card, signRaw(t, key, []byte(`{"alg":"ES256","alg":"ES256","kid":"`+kidA+`"}`), card), "repeated"},
		{"case-variant alg", card, signRaw(t, key, []byte(`{"alg":"ES256","ALG":"none","kid":"`+kidA+`"}`), card), "repeated"},
		{"upstream extra member", card, signRaw(t, key, []byte(`{"alg":"ES256","kid":"`+kidA+`","kontra.upstream":{"sha256":"x","url":"y","verified":true,"z":1}}`), card), HeaderUpstream},
		{"upstream verified string", card, signRaw(t, key, []byte(`{"alg":"ES256","kid":"`+kidA+`","kontra.upstream":{"sha256":"x","url":"y","verified":"true"}}`), card), HeaderUpstream},
		{"upstream jwks not a string", card, signRaw(t, key, []byte(`{"alg":"ES256","kid":"`+kidA+`","kontra.upstream":{"jwks":1,"sha256":"x","url":"y","verified":true}}`), card), HeaderUpstream},
		{"upstream jwks empty", card, signRaw(t, key, []byte(`{"alg":"ES256","kid":"`+kidA+`","kontra.upstream":{"jwks":"","sha256":"x","url":"y","verified":true}}`), card), HeaderUpstream},
		{"upstream jwks and an extra member", card, signRaw(t, key, []byte(`{"alg":"ES256","kid":"`+kidA+`","kontra.upstream":{"jwks":"j","sha256":"x","url":"y","verified":true,"z":1}}`), card), HeaderUpstream},
		{"upstream case-variant jwks", card, signRaw(t, key, []byte(`{"alg":"ES256","kid":"`+kidA+`","kontra.upstream":{"JWKS":"j","sha256":"x","url":"y","verified":true}}`), card), HeaderUpstream},
		{"unprotected kid", card, Signature{Protected: good.Protected, Signature: good.Signature, Header: json.RawMessage(`{"kid":"x"}`)}, "unprotected header repeats kid"},
		{"unprotected crit", card, Signature{Protected: good.Protected, Signature: good.Signature, Header: json.RawMessage(`{"crit":["b64"]}`)}, "unprotected header repeats crit"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := Verify([]byte(c.card), c.sig, pub)
			if err == nil || !errors.Is(err, ErrVerification) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Verify: %v, want ErrVerification containing %q", err, c.want)
			}
		})
	}
	// kontra.upstream with jwks (D223) verifies, and is read back.
	withJWKS := signRaw(t, key, []byte(`{"alg":"ES256","kid":"`+kidA+`","kontra.upstream":{"jwks":"https://g/agents/s/.well-known/seller-jwks.json","sha256":"x","url":"y","verified":true}}`), card)
	if h, err := Verify([]byte(card), withJWKS, pub); err != nil || h.Upstream == nil || h.Upstream.JWKS != "https://g/agents/s/.well-known/seller-jwks.json" {
		t.Fatalf("kontra.upstream with jwks: %+v %v", h.Upstream, err)
	}
	// A validly signed header with other, non-critical members still verifies; its unprotected header may hold
	// other names.
	extra := signRaw(t, key, []byte(`{"alg":"ES256","kid":"`+kidA+`","x-other":1}`), card)
	extra.Header = json.RawMessage(`{"note":"n"}`)
	if _, err := Verify([]byte(card), extra, pub); err != nil {
		t.Fatalf("a non-critical extra member: %v", err)
	}
}

func flipLast(s string) string {
	b := []byte(s)
	if b[len(b)-2] == 'A' {
		b[len(b)-2] = 'B'
	} else {
		b[len(b)-2] = 'A'
	}
	return string(b)
}

func TestSignLeavesSignaturesOut(t *testing.T) {
	ks := loaded(t)
	a, err := CardPayload([]byte(card))
	if err != nil {
		t.Fatal(err)
	}
	b, err := CardPayload(served(t, Signature{Protected: "p", Signature: "s"}))
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) || strings.Contains(string(a), "signatures") {
		t.Fatalf("payload with signatures differs or keeps them:\n%s\n%s", a, b)
	}
	// JCS: sorted keys, 1.50 -> 1.5, 1E21 -> 1e+21, U+2028 raw, no white space.
	if !strings.HasPrefix(string(a), `{"big":1e+21,"capabilities":`) || !strings.Contains(string(a), `"price":1.5,`) ||
		!strings.Contains(string(a), "<things> & stuff   é 😀") {
		t.Fatalf("payload is not JCS: %s", a)
	}
	// A card is one object without a repeated key.
	if _, err := ks.Sign([]byte(`{"name":"a","name":"b"}`), "", nil); err == nil {
		t.Fatal("a card with a repeated key is signed")
	}
	if _, err := ks.Sign([]byte(`[1]`), "", nil); err == nil {
		t.Fatal("an array is signed as a card")
	}
	// Without jku and upstream the header is {alg, kid, typ}.
	sig, err := ks.Sign([]byte(card), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	hb, _ := base64.RawURLEncoding.DecodeString(sig.Protected)
	if string(hb) != `{"alg":"ES256","kid":"`+kidA+`","typ":"JOSE"}` {
		t.Fatalf("protected = %s", hb)
	}
}

func TestParseJWKS(t *testing.T) {
	p256 := `{"kty":"EC","crv":"P-256","x":"` + rfc9449X + `","y":"` + rfc9449Y + `","kid":"seller-1"}`
	for _, c := range []struct {
		name, set string
		want      string // "" is success
		kids      []string
	}{
		{"skips other types", `{"keys":[{"kty":"RSA","n":"AQAB","e":"AQAB","kid":"r"},{"kty":"OKP","crv":"Ed25519","x":"11qYAYKxCrfVS_7TyWQHOg7hcvPapiMlrwIaaPcHURo"},` + p256 + `]}`, "", []string{"seller-1"}},
		{"skips P-384 and enc keys", `{"keys":[{"kty":"EC","crv":"P-384","x":"a","y":"b"},{"kty":"EC","crv":"P-256","use":"enc","x":"a","y":"b"},{"kty":"EC","crv":"P-256","alg":"ES384","x":"a","y":"b"},` + p256 + `]}`, "", []string{"seller-1"}},
		{"none at all", `{"keys":[{"kty":"RSA","n":"AQAB","e":"AQAB"}]}`, "no EC P-256 signing key", nil},
		{"empty", `{"keys":[]}`, "no EC P-256 signing key", nil},
		{"no keys", `{}`, "no keys member", nil},
		{"not on the curve", `{"keys":[{"kty":"EC","crv":"P-256","x":"` + rfc9449X + `","y":"` + rfc9449X + `"}]}`, "not a P-256 point", nil},
		{"short x", `{"keys":[{"kty":"EC","crv":"P-256","x":"AQAB","y":"` + rfc9449Y + `"}]}`, "32 bytes", nil},
		{"repeated kid", `{"keys":[` + p256 + `,` + p256 + `]}`, "twice", nil},
		{"case-variant member", `{"keys":[{"kty":"EC","KTY":"RSA","crv":"P-256","x":"` + rfc9449X + `","y":"` + rfc9449Y + `"}]}`, "repeated", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			set, err := ParseJWKS([]byte(c.set))
			if c.want != "" {
				if err == nil || !strings.Contains(err.Error(), c.want) {
					t.Fatalf("ParseJWKS: %v, want %q", err, c.want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(set.keys) != len(c.kids) {
				t.Fatalf("kept %d keys, want %v", len(set.keys), c.kids)
			}
			for _, k := range c.kids {
				if _, ok := set.keys[k]; !ok {
					t.Fatalf("kid %s not kept", k)
				}
			}
		})
	}
}

// KeySetPublic.JWKS (D223): the kept keys only, public members only, sorted by kid, and parsed back to the same keys
// and thumbprints; a key the seller listed without kid is listed under its thumbprint.
func TestPublicJWKS(t *testing.T) {
	p256 := `{"kty":"EC","crv":"P-256","x":"` + rfc9449X + `","y":"` + rfc9449Y + `","kid":"seller-1","d":"private"}`
	pub := `{"kty":"EC","crv":"P-256","x":"` + rfc9449X + `","y":"` + rfc9449Y + `"}`
	set, err := ParseJWKS([]byte(`{"keys":[{"kty":"RSA","n":"AQAB","e":"AQAB","kid":"r"},` + p256 + `]}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := set.JWKS()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"d"`) || strings.Contains(string(b), "RSA") || !strings.Contains(string(b), `"kid":"seller-1"`) {
		t.Fatalf("the public set: %s", b)
	}
	back, err := ParseJWKS(b)
	if err != nil {
		t.Fatal(err)
	}
	t1, _ := set.Thumbprints()
	t2, _ := back.Thumbprints()
	if !slices.Equal(t1, t2) {
		t.Fatalf("thumbprints %v, parsed back %v", t1, t2)
	}
	noKid, _ := ParseJWKS([]byte(`{"keys":[` + pub + `]}`))
	b, _ = noKid.JWKS()
	if tp, _ := noKid.Thumbprints(); !strings.Contains(string(b), `"kid":"`+tp[0]+`"`) {
		t.Fatalf("a key without kid: %s", b)
	}
	if _, err := (*KeySetPublic)(nil).JWKS(); err == nil {
		t.Fatal("an empty set rendered")
	}
}

// TestLoadKeySetRefusesSkippedBlocks: pem.Decode skips a block it cannot decode and any text before a block. A
// rotation that prepends a mangled new key (D166) must fail, not leave the old key active without a word.
func TestLoadKeySetRefusesSkippedBlocks(t *testing.T) {
	two, err := os.ReadFile(filepath.Join("testdata", "two-keys.pem"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(two)
	i := strings.Index(s, "-----BEGIN EC PARAMETERS-----")
	first, rest := s[:i], s[i:] // the PKCS #8 key (kidA), then the EC PARAMETERS and SEC 1 key (kidB)
	if ks, err := LoadKeySet(writeKey(t, []byte(rest), 0o600)); err != nil || ks.kids[0] != kidB {
		t.Fatalf("the second key alone does not load: %v", err)
	}
	// One base64 character of the new key changed to '!'.
	j := strings.Index(first, "MIGHAgEA") + 20
	mangled := first[:j] + "!" + first[j+1:]
	noEnd := strings.Replace(first, "-----END PRIVATE KEY-----\n", "", 1)
	for _, c := range []struct{ name, data string }{
		{"mangled first key", mangled + rest},
		{"undecodable block between keys", first + "-----BEGIN PRIVATE KEY-----\n!!!notbase64!!!\n-----END PRIVATE KEY-----\n" + rest},
		{"first key without END", noEnd + rest},
		{"last key without END", first + strings.Replace(rest, "-----END EC PRIVATE KEY-----", "", 1)},
		{"text before the first block", "new key below\n" + s},
		{"text between blocks", first + "old key below\n" + rest},
		{"END without BEGIN", first + "-----END PRIVATE KEY-----\n" + rest},
		{"BEGIN inside a line", "x -----BEGIN PRIVATE KEY-----\n" + s},
	} {
		t.Run(c.name, func(t *testing.T) {
			ks, err := LoadKeySet(writeKey(t, []byte(c.data), 0o600))
			if err == nil {
				t.Fatalf("LoadKeySet loaded %v (active %s); want a refusal", ks.KeyIDs(), ks.kids[0])
			}
			if strings.Contains(err.Error(), "PRIVATE KEY-----") || strings.Contains(err.Error(), "MIG") ||
				strings.Contains(err.Error(), "below") {
				t.Fatalf("the error quotes the key file: %v", err)
			}
		})
	}
	// White space before, between and after blocks is fine, and so are CRLF line ends.
	for _, data := range []string{"\n\n" + first + "\n  \n" + rest + "\n\n", strings.ReplaceAll(s, "\n", "\r\n")} {
		ks, err := LoadKeySet(writeKey(t, []byte(data), 0o600))
		if err != nil || len(ks.kids) != 2 || ks.kids[0] != kidA {
			t.Fatalf("LoadKeySet(%q) = %v", data[:20], err)
		}
	}
}

// Signing is deterministic (RFC 6979): the same card, header and key sign to the same bytes, which still verify,
// so replicas sharing a key serve one ETag (M3 WP-02).
func TestSignIsDeterministic(t *testing.T) {
	ks, err := EphemeralKeySet()
	if err != nil {
		t.Fatal(err)
	}
	card := []byte(`{"name":"hello","signatures":[{"protected":"x","signature":"y"}]}`)
	up := &Upstream{SHA256: "00", URL: "https://gate.example/agents/hello/.well-known/agent-card.upstream.json", Verified: true}
	a, err := ks.Sign(card, "https://gate.example/.well-known/jwks.json", up)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := ks.Sign([]byte(`{"name": "hello"}`), "https://gate.example/.well-known/jwks.json", up)
	if a.Protected != b.Protected || a.Signature != b.Signature {
		t.Fatalf("two signatures of one card differ: %+v / %+v", a, b)
	}
	jwks, _ := ks.JWKS()
	pub, _ := ParseJWKS(jwks)
	if _, err := Verify(card, a, pub); err != nil {
		t.Fatal(err)
	}
	c, _ := ks.Sign([]byte(`{"name":"other"}`), "https://gate.example/.well-known/jwks.json", up)
	if c.Signature == a.Signature {
		t.Error("two cards signed to the same signature")
	}
}
