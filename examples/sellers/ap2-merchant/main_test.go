package main

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
)

// serve starts the agent with merchant m (a new key when nil) on an httptest server and returns its root URL.
func serve(t *testing.T, m *merchant) string {
	t.Helper()
	if m == nil {
		m = newTestMerchant(t)
	}
	var h http.Handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(w, r) }))
	t.Cleanup(srv.Close)
	h = newHandler(srv.URL, m, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return srv.URL
}

func newTestMerchant(t *testing.T) *merchant {
	t.Helper()
	key, err := loadKey("")
	if err != nil {
		t.Fatal(err)
	}
	m, err := newMerchant(key)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// headers adds a client's extra request headers (A2A-Extensions).
type headers struct {
	h    http.Header
	next http.RoundTripper
}

func (x headers) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	for k, vs := range x.h {
		for _, v := range vs {
			r.Header.Add(k, v)
		}
	}
	return x.next.RoundTrip(r)
}

// client talks to the agent at base over one binding, sending h with every request.
func client(t *testing.T, base string, protocol a2a.TransportProtocol, h http.Header) *a2aclient.Client {
	t.Helper()
	url, hc := base, &http.Client{Transport: headers{h, http.DefaultTransport}}
	opt := a2aclient.WithJSONRPCTransport(hc)
	if protocol == a2a.TransportProtocolHTTPJSON {
		url, opt = base+restPrefix, a2aclient.WithRESTTransport(hc)
	}
	c, err := a2aclient.NewFromEndpoints(context.Background(), []*a2a.AgentInterface{a2a.NewAgentInterface(url, protocol)}, opt)
	if err != nil {
		t.Fatalf("client for %s %s: %v", protocol, url, err)
	}
	t.Cleanup(func() { _ = c.Destroy() })
	return c
}

func bindings() map[string]a2a.TransportProtocol {
	return map[string]a2a.TransportProtocol{"JSONRPC": a2a.TransportProtocolJSONRPC, "HTTP+JSON": a2a.TransportProtocolHTTPJSON}
}

// activate is the A2A-Extensions header of a shopper that speaks AP2.
func activate() http.Header { return http.Header{"A2A-Extensions": {ExtAP2}} }

func ctxT(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func send(t *testing.T, c *a2aclient.Client, m *a2a.Message, task *a2a.Task) *a2a.Task {
	t.Helper()
	if task != nil {
		m.TaskID, m.ContextID = task.ID, task.ContextID
	}
	res, err := c.SendMessage(ctxT(t), &a2a.SendMessageRequest{Message: m})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	got, ok := res.(*a2a.Task)
	if !ok {
		t.Fatalf("SendMessage returned %T, want a task", res)
	}
	return got
}

// dataValue is the value under key in the task's status message's data parts, and its text.
func dataValue(t *testing.T, task *a2a.Task, key string) (any, string) {
	t.Helper()
	if task.Status.Message == nil {
		t.Fatalf("task %s (%s) has no status message", task.ID, task.Status.State)
	}
	var text string
	var v any
	for _, p := range task.Status.Message.Parts {
		text += p.Text()
		if obj, ok := p.Data().(map[string]any); ok && obj[key] != nil {
			v = obj[key]
		}
	}
	return v, text
}

// jwks fetches the merchant's public key from base.
func jwks(t *testing.T, base string) *ecdsa.PublicKey {
	t.Helper()
	resp, err := http.Get(base + jwksPath)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var set struct {
		Keys []struct{ Kty, Crv, X, Y, Kid, Alg string } `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&set); err != nil || len(set.Keys) != 1 {
		t.Fatalf("jwks: %+v %v", set, err)
	}
	k := set.Keys[0]
	x, _ := base64.RawURLEncoding.DecodeString(k.X)
	y, _ := base64.RawURLEncoding.DecodeString(k.Y)
	pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), append(append([]byte{4}, x...), y...))
	if err != nil || k.Kty != "EC" || k.Crv != "P-256" || k.Alg != "ES256" || k.Kid == "" {
		t.Fatalf("jwks key %+v: %v", k, err)
	}
	return pub
}

// verifyJWT checks jwt's ES256 signature with pub (as a shopper checks the merchant's) and returns its header and
// payload.
func verifyJWT(t *testing.T, jwt string, pub *ecdsa.PublicKey) (map[string]any, map[string]any) {
	t.Helper()
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("not a JWS: %q", jwt)
	}
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	if len(sig) != 64 || !ecdsa.Verify(pub, sha([]byte(parts[0]+"."+parts[1])), new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Fatalf("the JWS is not signed by the merchant's published key")
	}
	var h, p map[string]any
	if decodeJSON(parts[0], &h) != nil || decodeJSON(parts[1], &p) != nil {
		t.Fatalf("the JWS's header or payload is not JSON")
	}
	return h, p
}

// shopper builds closed Checkout Mandates as SD-JWTs (RFC 9901), the shape of docs/ap2/checkout_mandate.md's
// "Closed Checkout Mandate" example: the checkout_jwt claim selectively disclosed, signed with the shopper's own key
// (which the merchant does not check).
type shopper struct{ key *ecdsa.PrivateKey }

func newShopper(t *testing.T) shopper {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return shopper{k}
}

// mandate is a closed Checkout Mandate over checkoutJWT whose checkout_hash is hash ("" computes it); extra claims
// are added to the payload.
func (s shopper) mandate(t *testing.T, checkoutJWT, hash string, extra map[string]any) string {
	t.Helper()
	if hash == "" {
		hash = b64(sha([]byte(checkoutJWT)))
	}
	salt := make([]byte, 16)
	_, _ = rand.Read(salt)
	raw, _ := json.Marshal([]any{b64(salt), "checkout_jwt", checkoutJWT})
	disclosure := b64(raw)
	payload := map[string]any{"vct": vctCheckout, "checkout_hash": hash, "_sd": []any{b64(sha([]byte(disclosure)))}, "_sd_alg": "sha-256",
		"iat": time.Now().Unix()}
	for k, v := range extra {
		payload[k] = v
	}
	m := &merchant{key: s.key, kid: "shopper"}
	jwt, err := m.sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	return jwt + "~" + disclosure + "~"
}

func mandateMessage(token string) *a2a.Message {
	return a2a.NewMessage(a2a.MessageRoleUser, a2a.NewDataPart(map[string]any{KeyCheckoutMandate: token}))
}

func TestCard(t *testing.T) {
	base := serve(t, nil)
	resp, err := http.Get(base + "/.well-known/agent-card.json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var card a2a.AgentCard
	if err := json.Unmarshal(raw, &card); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("card: %d %v\n%s", resp.StatusCode, err, raw)
	}
	if len(card.SupportedInterfaces) != 2 || card.SupportedInterfaces[0].URL != base || card.SupportedInterfaces[1].URL != base+restPrefix {
		t.Fatalf("interfaces %+v", card.SupportedInterfaces)
	}
	var doc struct {
		Capabilities struct {
			Extensions []json.RawMessage `json:"extensions"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || len(doc.Capabilities.Extensions) != 1 {
		t.Fatalf("extensions %s %v", raw, err)
	}
	var e struct {
		URI         string         `json:"uri"`
		Description string         `json:"description"`
		Required    bool           `json:"required"`
		Params      map[string]any `json:"params"`
	}
	_ = json.Unmarshal(doc.Capabilities.Extensions[0], &e)
	if e.URI != ExtAP2 || !e.Required || e.Description == "" || len(e.Params) != 1 {
		t.Fatalf("the AP2 entry %s", doc.Capabilities.Extensions[0])
	}
	if roles, _ := e.Params["roles"].([]any); len(roles) != 1 || roles[0] != "merchant" {
		t.Fatalf("params.roles %v, want [merchant]", e.Params["roles"])
	}
}

// A message that does not activate AP2 is refused ExtensionSupportRequiredError, on both bindings; GetTask needs no
// activation.
func TestRequiresActivation(t *testing.T) {
	base := serve(t, nil)
	for name, protocol := range bindings() {
		t.Run(name, func(t *testing.T) {
			_, err := client(t, base, protocol, nil).SendMessage(ctxT(t), &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("buy"))})
			if !errors.Is(err, a2a.ErrExtensionSupportRequired) {
				t.Fatalf("SendMessage without AP2: %v, want ExtensionSupportRequiredError", err)
			}
			legacy := client(t, base, protocol, http.Header{"X-A2A-Extensions": {"https://other.example/ext, " + ExtAP2}})
			task := send(t, legacy, a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("buy")), nil)
			if task.Status.State != a2a.TaskStateInputRequired {
				t.Fatalf("with X-A2A-Extensions naming AP2 among others: %s", task.Status.State)
			}
			if _, err := client(t, base, protocol, nil).GetTask(ctxT(t), &a2a.GetTaskRequest{ID: task.ID}); err != nil {
				t.Fatalf("GetTask without AP2: %v", err)
			}
		})
	}
}

// The round trip: a signed checkout, a mandate bound to it, a Success receipt that completes the task. Both bindings.
func TestCheckoutThenMandate(t *testing.T) {
	base := serve(t, nil)
	pub := jwks(t, base)
	buyer := newShopper(t)
	for name, protocol := range bindings() {
		t.Run(name, func(t *testing.T) {
			c := client(t, base, protocol, activate())
			task := send(t, c, a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("I'd like to buy the example item")), nil)
			v, text := dataValue(t, task, KeyCheckout)
			co, _ := v.(map[string]any)
			if task.Status.State != a2a.TaskStateInputRequired || co == nil || text != askMandate {
				t.Fatalf("the first answer: %s %q %v", task.Status.State, text, v)
			}
			jwt, _ := co["checkout_jwt"].(string)
			h, p := verifyJWT(t, jwt, pub)
			if h["alg"] != "ES256" || h["kid"] == "" || co["checkout_hash"] != b64(sha([]byte(jwt))) || p["id"] != co["checkout_id"] ||
				p["currency"] != "USD" || p["status"] != "incomplete" {
				t.Fatalf("the checkout %v: header %v payload %v", co, h, p)
			}

			// A message without a mandate asks again; the task keeps waiting with the same checkout.
			again := send(t, c, a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("what now?")), task)
			if _, text := dataValue(t, again, KeyCheckoutReceipt); again.Status.State != a2a.TaskStateInputRequired || text != askAgain {
				t.Fatalf("a message without a mandate: %s %q", again.Status.State, text)
			}

			token := buyer.mandate(t, jwt, "", nil)
			done := send(t, c, mandateMessage(token), task)
			rv, text := dataValue(t, done, KeyCheckoutReceipt)
			rjwt, _ := rv.(string)
			if done.Status.State != a2a.TaskStateCompleted || done.ID != task.ID || !strings.Contains(text, co["checkout_id"].(string)) {
				t.Fatalf("the mandate's answer: %s %q", done.Status.State, text)
			}
			_, r := verifyJWT(t, rjwt, pub)
			ref := b64(sha([]byte(strings.Split(token, "~")[0])))
			if r["status"] != "Success" || r["iss"] != website || r["reference"] != ref || r["order_id"] == nil || r["iat"] == nil || r["error"] != nil {
				t.Fatalf("the receipt %v, want Success with reference %s", r, ref)
			}
		})
	}
}

// A refused mandate answers an Error receipt and the task keeps waiting; a good one then completes it.
func TestRefusedThenAccepted(t *testing.T) {
	base := serve(t, nil)
	pub := jwks(t, base)
	buyer := newShopper(t)
	c := client(t, base, a2a.TransportProtocolJSONRPC, activate())
	task := send(t, c, a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("buy")), nil)
	v, _ := dataValue(t, task, KeyCheckout)
	jwt := v.(map[string]any)["checkout_jwt"].(string)
	other := send(t, c, a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("buy")), nil)
	ov, _ := dataValue(t, other, KeyCheckout)

	bad := send(t, c, mandateMessage(buyer.mandate(t, ov.(map[string]any)["checkout_jwt"].(string), "", nil)), task)
	rv, text := dataValue(t, bad, KeyCheckoutReceipt)
	_, r := verifyJWT(t, rv.(string), pub)
	if bad.Status.State != a2a.TaskStateInputRequired || r["status"] != "Error" || r["error"] != "unknown_checkout" ||
		r["error_description"] == "" || r["reference"] == "" || r["order_id"] != nil || !strings.Contains(text, "refused") {
		t.Fatalf("another task's checkout: %s %q %v", bad.Status.State, text, r)
	}
	good := send(t, c, mandateMessage(buyer.mandate(t, jwt, "", nil)), task)
	if good.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("the right mandate after a refused one: %s", good.Status.State)
	}
}

// checkoutMandate's refusals, merchant-side.
func TestCheckoutMandate(t *testing.T) {
	m := newTestMerchant(t)
	buyer := newShopper(t)
	co, err := m.checkout()
	if err != nil {
		t.Fatal(err)
	}
	c := issued{jwt: co["checkout_jwt"].(string), id: co["checkout_id"].(string)}
	forger := newTestMerchant(t)
	forged, _ := forger.sign(map[string]any{"id": c.id, "exp": time.Now().Add(time.Hour).Unix()})
	good := buyer.mandate(t, c.jwt, "", nil)
	parts := strings.Split(good, "~")
	undisclosed := parts[0] + "~"
	sd512 := func() string {
		var p map[string]any
		_ = decodeJSON(strings.Split(parts[0], ".")[1], &p)
		p["_sd_alg"] = "sha-512"
		jwt, _ := (&merchant{key: buyer.key}).sign(p)
		return jwt + "~" + parts[1] + "~"
	}()
	for _, tc := range []struct {
		name, token, code string
	}{
		{"good", good, ""},
		{"good, with a key-binding JWT", good + "eyJhbGciOiJFUzI1NiJ9.e30.c2ln", ""},
		{"good, closing a delegation chain", "eyJhbGciOiJFUzI1NiJ9.e30.c2ln~~" + good, ""},
		{"the hash of another value", buyer.mandate(t, c.jwt, b64(sha([]byte("x"))), nil), "checkout_hash_mismatch"},
		{"another checkout of this merchant", buyer.mandate(t, mustCheckout(t, m), "", nil), "unknown_checkout"},
		{"a checkout signed by another key", buyer.mandate(t, forged, "", nil), "unknown_checkout"},
		{"checkout_jwt not disclosed", undisclosed, "invalid_mandate"},
		{"an expired mandate", buyer.mandate(t, c.jwt, "", map[string]any{"exp": time.Now().Add(-time.Minute).Unix()}), "mandate_expired"},
		{"another vct", strings.Replace(good, parts[0], mustResign(t, buyer, parts[0], "vct", "mandate.payment.1"), 1), "invalid_mandate"},
		{"sha-512", sd512, "invalid_mandate"},
		{"a disclosure twice", good + parts[1] + "~", "invalid_mandate"},
		{"not an SD-JWT", "hello", "invalid_mandate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, err := m.checkoutMandate(tc.token, c)
			if code != tc.code || (code == "") != (err == nil) {
				t.Fatalf("code %q err %v, want %q", code, err, tc.code)
			}
		})
	}
	t.Run("an expired checkout", func(t *testing.T) {
		later := *m
		later.now = func() time.Time { return time.Now().Add(2 * checkoutTTL) }
		if code, _ := later.checkoutMandate(good, c); code != "checkout_expired" {
			t.Fatalf("code %q, want checkout_expired", code)
		}
	})
}

func mustCheckout(t *testing.T, m *merchant) string {
	t.Helper()
	co, err := m.checkout()
	if err != nil {
		t.Fatal(err)
	}
	return co["checkout_jwt"].(string)
}

// mustResign is jwt with claim set to v, re-signed by s.
func mustResign(t *testing.T, s shopper, jwt, claim string, v any) string {
	t.Helper()
	var p map[string]any
	if err := decodeJSON(strings.Split(jwt, ".")[1], &p); err != nil {
		t.Fatal(err)
	}
	p[claim] = v
	out, err := (&merchant{key: s.key}).sign(p)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// AP2 v0.2.0's own "Closed Checkout Mandate" example (docs/ap2/checkout_mandate.md, "Encoded Token"; Apache 2.0),
// vendored as testdata/closed-checkout-mandate.sdjwt: its checkout_jwt sits in a disclosure inside a disclosed array
// element. The binding checks out; the checkout is not this merchant's.
func TestAP2Example(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "closed-checkout-mandate.sdjwt"))
	if err != nil {
		t.Fatal(err)
	}
	token := strings.TrimSpace(string(raw))
	claims, err := disclose(closedSegment(token))
	if err != nil {
		t.Fatal(err)
	}
	mandate, ok := findVCT(claims, vctCheckout)
	if !ok || mandate["checkout_hash"] != "NivWhuqfzcvZNapvIEJ2-3tsdQLkiuIcye2g46WVgX8" {
		t.Fatalf("the example's claims %v", claims)
	}
	jwt, _ := mandate["checkout_jwt"].(string)
	if b64(sha([]byte(jwt))) != mandate["checkout_hash"] {
		t.Fatal("the example's checkout_hash is not the SHA-256 of its checkout_jwt")
	}
	m := newTestMerchant(t)
	if code, _ := m.checkoutMandate(token, issued{jwt: "other"}); code != "unknown_checkout" {
		t.Fatalf("the example against another checkout: %q", code)
	}
	if code, err := m.checkoutMandate(token, issued{jwt: jwt}); code != "unknown_checkout" || !strings.Contains(err.Error(), "not a compact JWS") &&
		!strings.Contains(err.Error(), "not signed by this merchant") {
		t.Fatalf("the example's checkout, not signed by this merchant: %q %v", code, err)
	}
	r := m.complete(token, issued{jwt: jwt})
	if r.Status != "Error" || r.Reference != b64(sha([]byte(strings.Split(token, "~")[0]))) {
		t.Fatalf("the receipt %+v", r)
	}
}

func TestLoadKey(t *testing.T) {
	dir := t.TempDir()
	write := func(name, typ string, der []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	sec1, _ := x509.MarshalECPrivateKey(k)
	pkcs8, _ := x509.MarshalPKCS8PrivateKey(k)
	k384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	p384, _ := x509.MarshalPKCS8PrivateKey(k384)
	for _, f := range []string{write("sec1.pem", "EC PRIVATE KEY", sec1), write("pkcs8.pem", "PRIVATE KEY", pkcs8)} {
		got, err := loadKey(f)
		if err != nil || !got.Equal(k) {
			t.Errorf("%s: %v", f, err)
		}
	}
	for _, f := range []string{write("p384.pem", "PRIVATE KEY", p384), write("cert.pem", "CERTIFICATE", []byte{1}), filepath.Join(dir, "none")} {
		if _, err := loadKey(f); err == nil {
			t.Errorf("%s: loaded", f)
		}
	}
}

func TestRun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pr, pw := io.Pipe()
	code := make(chan int, 1)
	go func() { code <- run(ctx, []string{"--port", "0"}, pw, io.Discard); pw.Close() }()
	line, err := bufio.NewReader(pr).ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "http://127.0.0.1:") {
		t.Fatalf("first line %q %v", line, err)
	}
	go func() { _, _ = io.Copy(io.Discard, pr) }()
	for _, path := range []string{"/.well-known/agent-card.json", jwksPath} {
		resp, err := http.Get(strings.TrimSpace(line) + path)
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: %v %v", path, resp, err)
		}
		resp.Body.Close()
	}
	cancel()
	if c := <-code; c != 0 {
		t.Fatalf("exit %d", c)
	}
	for _, args := range [][]string{{"--port", "70000"}, {"extra"}, {"--nope"}, {"--key", filepath.Join(t.TempDir(), "none")}} {
		if c := run(context.Background(), args, io.Discard, io.Discard); c != 2 {
			t.Errorf("run %v: exit %d, want 2", args, c)
		}
	}
}
