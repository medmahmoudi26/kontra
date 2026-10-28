package gate

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2acrypto"

	"kontra.sh/kontra/internal/jcs"
	"kontra.sh/kontra/internal/jws"
	"kontra.sh/kontra/internal/strictjson"
)

// writeSigningKey writes a PEM file of one ECDSA P-256 private key (PKCS #8, mode 0600) and returns its path.
func writeSigningKey(t *testing.T) string {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "kontra.key")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// sellerSign returns cardJSON (an object without signatures) with one signature by keys appended.
func sellerSign(t *testing.T, keys *jws.KeySet, cardJSON string) string {
	t.Helper()
	sig, err := keys.Sign([]byte(cardJSON), "https://seller.example/.well-known/jwks.json", nil)
	if err != nil {
		t.Fatal(err)
	}
	sb, _ := json.Marshal(sig)
	trimmed := strings.TrimRight(strings.TrimSpace(cardJSON), "}")
	return trimmed + `,"signatures":[` + string(sb) + `]}`
}

func jwksOf(t *testing.T, keys *jws.KeySet) string {
	t.Helper()
	b, err := keys.JWKS()
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// get fetches path with the key (none when empty) and returns the status, headers and body.
func get(t *testing.T, base, path, key string) (int, http.Header, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, base+path, nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, b
}

// card_passthrough (spec §3, §16; D13, D166): a seller card carrying the seller's signature, served by the gate,
// has exactly one signature, the gate's, which a2a-go's verifier accepts once it resolves the jku to the gate's
// JWKS. Its protected header binds the upstream card (kontra.upstream.sha256 is the hex SHA-256 of its JCS, url
// names the upstream route) and says the seller's signature verified against the configured seller_keys; the
// upstream route serves the seller's card byte for byte, signature included; the seller's fields are
// byte-identical under JCS; services.card_hash holds the hash; the card routes keep their caching headers (the
// TCK's). A service whose seller_keys verify no signature is unavailable: its card routes answer 503 with an
// empty body and a new task fails service_unavailable at its first attempt. seller_keys {url} on a private
// address is refused by the SSRF dialer and leaves its service unavailable.
func TestCardPassthrough(t *testing.T) {
	e := newGateEnv(t)
	seller, _ := jws.EphemeralKeySet()
	stranger, _ := jws.EphemeralKeySet()
	upstreamCard := sellerSign(t, seller, e.seller.cardJSON)
	e.seller.cardJSON = upstreamCard
	keyPath := writeSigningKey(t)
	port := freePort(t)
	base := e.seller.srv.URL + "/base"
	services := fmt.Sprintf(`services:
  hello:
    kind: a2a
    backend: { a2a: { url: %q, allow_private: true } }
    allow: ["org:default"]
    seller_keys: %s
  forged:
    kind: a2a
    backend: { a2a: { url: %q, allow_private: true } }
    allow: ["org:default"]
    seller_keys: %s
    task: { retries: 2 }
  remote:
    kind: a2a
    backend: { a2a: { url: %q, allow_private: true } }
    allow: ["org:default"]
    seller_keys: { url: "https://127.0.0.1:%d/jwks.json" }
`, base, jwksOf(t, seller), base, jwksOf(t, stranger), base, freePort(t))
	path := e.replicaConfig(port, 10*time.Second, "signing_key: file:"+keyPath+"\n"+services)
	run := e.start("a", path, fastTiming)
	key := e.mintKey("cards")
	publicURL := fmt.Sprintf("http://127.0.0.1:%d", port)

	var status int
	var hdr http.Header
	var served []byte
	eventually(t, 15*time.Second, "the signed card", func() bool {
		status, hdr, served = get(t, run.url, "/agents/hello/.well-known/agent-card.json", key)
		return status == http.StatusOK
	})
	if hdr.Get("Cache-Control") != "max-age=60" || hdr.Get("ETag") == "" || hdr.Get("Last-Modified") == "" {
		t.Errorf("card headers %v", hdr)
	}
	m, err := strictjson.Members(served)
	if err != nil {
		t.Fatal(err)
	}
	var sigs []a2a.AgentCardSignature
	if err := json.Unmarshal(m["signatures"], &sigs); err != nil || len(sigs) != 1 {
		t.Fatalf("signatures %s: want exactly one", m["signatures"])
	}
	// a2a-go's verifier, resolving the jku to the gate's JWKS.
	jku := publicURL + jws.JWKSPath
	v := a2acrypto.NewVerifier(a2acrypto.VerifierConfig{KeyResolver: a2acrypto.NewJWKSKeyResolver(http.DefaultClient, []string{jku})})
	if err := v.Verify(context.Background(), served, &sigs[0]); err != nil {
		t.Fatalf("a2a-go's verifier refuses the gate's signature: %v", err)
	}
	hb, _ := base64.RawURLEncoding.DecodeString(sigs[0].Protected)
	var header struct {
		JKU      string       `json:"jku"`
		Upstream jws.Upstream `json:"kontra.upstream"`
	}
	if err := json.Unmarshal(hb, &header); err != nil {
		t.Fatal(err)
	}
	canon, _ := jcs.Transform([]byte(upstreamCard))
	sum := sha256.Sum256(canon)
	if header.JKU != jku || header.Upstream.SHA256 != hex.EncodeToString(sum[:]) || !header.Upstream.Verified ||
		header.Upstream.URL != publicURL+"/agents/hello/.well-known/agent-card.upstream.json" ||
		header.Upstream.JWKS != publicURL+"/agents/hello/.well-known/seller-jwks.json" {
		t.Fatalf("protected header %s", hb)
	}
	// The upstream route serves the seller's card byte for byte, signature included.
	if status, _, up := get(t, run.url, "/agents/hello/.well-known/agent-card.upstream.json", key); status != 200 || string(up) != upstreamCard {
		t.Fatalf("upstream route: %d %s", status, up)
	}
	// Seller fields are byte-identical under JCS.
	strip := func(b []byte) string {
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
		return string(out)
	}
	if strip(served) != strip([]byte(upstreamCard)) {
		t.Errorf("seller fields differ under JCS:\n%s\n%s", strip(served), strip([]byte(upstreamCard)))
	}
	eventually(t, 10*time.Second, "services.card_hash", func() bool {
		var h []byte
		err := e.pool.QueryRow(context.Background(), `select card_hash from kontra.services where name = 'hello'`).Scan(&h)
		return err == nil && bytes.Equal(h, sum[:])
	})
	// The JWKS lists the file's key.
	status, hdr, jwks := get(t, run.url, jws.JWKSPath, "")
	keys, err := jws.LoadKeySet(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	kid, _ := keys.KeyID()
	if status != 200 || hdr.Get("Cache-Control") != "max-age=300" || !strings.Contains(string(jwks), kid) {
		t.Fatalf("JWKS: %d %v %s", status, hdr, jwks)
	}

	// The forged service: its keys verify no signature.
	for _, route := range []string{"/agents/forged", "/agents/forged/.well-known/agent-card.json", "/agents/forged/.well-known/agent-card.upstream.json",
		"/agents/forged/.well-known/seller-jwks.json"} {
		if status, _, body := get(t, run.url, route, key); status != http.StatusServiceUnavailable || len(body) != 0 {
			t.Errorf("%s: %d %q, want 503 with an empty body", route, status, body)
		}
	}
	task := rpcSend(t, run.url+"/agents/forged", key, "hi")
	if task.Status.State != a2a.TaskStateFailed || task.Metadata["kontra.failure"] != "service_unavailable" {
		t.Fatalf("a task on the forged service: %s %v", task.Status.State, task.Metadata)
	}
	var attempts int
	if err := e.pool.QueryRow(context.Background(), `select count(*) from kontra.attempts where task_id = $1`, string(task.ID)).Scan(&attempts); err != nil || attempts != 1 {
		t.Errorf("the task failed after %d attempts (%v), want its first", attempts, err)
	}
	if n := e.seller.count("SendMessage") + e.seller.count("SendStreamingMessage"); n != 0 {
		t.Errorf("the forged service's seller was sent %d messages", n)
	}
	// The {url} set on a private address: the SSRF dialer refuses it, and the service is unavailable.
	if status, _, _ := get(t, run.url, "/agents/remote/.well-known/agent-card.json", key); status != http.StatusServiceUnavailable {
		t.Errorf("a service whose key set cannot be fetched: %d", status)
	}
}

// rpcSend sends one blocking JSON-RPC SendMessage to svc and returns the task.
func rpcSend(t *testing.T, svc, key, text string) *a2a.Task {
	t.Helper()
	params, _ := json.Marshal(a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text))})
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "SendMessage", "params": json.RawMessage(params)})
	req, _ := http.NewRequest(http.MethodPost, svc, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("A2A-Version", "1.0")
	req.Header.Set("Content-Type", "application/json")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.Error != nil {
		t.Fatalf("SendMessage: %d %s %v", resp.StatusCode, out.Error, err)
	}
	var sr a2a.StreamResponse
	if err := json.Unmarshal(out.Result, &sr); err != nil {
		t.Fatalf("result %s: %v", out.Result, err)
	}
	task, ok := sr.Event.(*a2a.Task)
	if !ok {
		t.Fatalf("result %s", out.Result)
	}
	return task
}

// A loopback gate without signing_key (D166) starts, signs with a key minted at start, logs it, and lists it in
// its JWKS: its served card verifies against that key.
func TestEphemeralSigningKeyOnLoopback(t *testing.T) {
	e := newGateEnv(t)
	port := freePort(t)
	run := e.start("a", e.replicaConfig(port, 10*time.Second, ""), fastTiming)
	key := e.mintKey("cards")
	status, _, jwks := get(t, run.url, jws.JWKSPath, "")
	pub, err := jws.ParseJWKS(jwks)
	if status != 200 || err != nil {
		t.Fatalf("JWKS: %d %s %v", status, jwks, err)
	}
	if tps, _ := pub.Thumbprints(); len(tps) != 1 {
		t.Fatalf("the JWKS lists %d keys, want the one minted at start", len(tps))
	}
	var served []byte
	eventually(t, 15*time.Second, "the card", func() bool {
		status, _, served = get(t, run.url, "/agents/hello", key)
		return status == http.StatusOK
	})
	m, _ := strictjson.Members(served)
	var sigs []jws.Signature
	if err := json.Unmarshal(m["signatures"], &sigs); err != nil || len(sigs) != 1 {
		t.Fatalf("signatures %s", m["signatures"])
	}
	h, err := jws.Verify(served, sigs[0], pub)
	if err != nil || h.Upstream == nil || h.Upstream.Verified {
		t.Fatalf("the card does not verify against the ephemeral key: %+v %v", h, err)
	}
	if h.JKU != fmt.Sprintf("http://127.0.0.1:%d%s", port, jws.JWKSPath) {
		t.Errorf("jku %q", h.JKU)
	}
}
