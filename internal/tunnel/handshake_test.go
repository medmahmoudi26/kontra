package tunnel

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode"

	"filippo.io/age"
)

// specList reads the backquoted words of the sentence of spec/tunnel.md §4 that starts with prefix and
// ends with end.
func specList(t *testing.T, prefix, end string) []string {
	t.Helper()
	b, err := os.ReadFile("../../spec/tunnel.md")
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(strings.Fields(string(b)), " ")
	_, rest, ok := strings.Cut(text, prefix)
	if !ok {
		t.Fatalf("spec/tunnel.md has no %q", prefix)
	}
	sentence, _, ok := strings.Cut(rest, end)
	if !ok {
		t.Fatalf("spec/tunnel.md: no %q after %q", end, prefix)
	}
	sentence = regexp.MustCompile(`\([^)]*\)`).ReplaceAllString(sentence, "") // asides name other things
	var words []string
	for _, m := range regexp.MustCompile("`([a-z_]+)`").FindAllStringSubmatch(sentence, -1) {
		words = append(words, m[1])
	}
	return words
}

// §4: Codes is the spec's list in its order, so the close status of each is 4000 + its index there.
func TestCodesFollowSpec(t *testing.T) {
	want := specList(t, "`code` is one of", ". The gate then closes")
	if len(want) != 12 {
		t.Fatalf("spec lists %d codes: %v", len(want), want)
	}
	var got []string
	for _, c := range Codes {
		got = append(got, string(c))
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Codes = %v\nspec  = %v", got, want)
	}
	for i, c := range Codes {
		st, ok := c.CloseStatus()
		if !ok || st != 4000+i {
			t.Fatalf("%s.CloseStatus() = %d, %v", c, st, ok)
		}
		back, ok := CodeOfStatus(st)
		if !ok || back != c {
			t.Fatalf("CodeOfStatus(%d) = %s, %v", st, back, ok)
		}
	}
	for c, st := range map[Code]int{CodeBadProof: 4002, CodeAuthInvalid: 4011, CodeUnsupportedVersion: 4000, CodeDraining: 4010} {
		if got, _ := c.CloseStatus(); got != st {
			t.Fatalf("%s closes %d, want %d", c, got, st)
		}
	}
	for _, st := range []int{0, 1000, 1008, 3999, 4012, 4999} {
		if c, ok := CodeOfStatus(st); ok {
			t.Fatalf("CodeOfStatus(%d) = %s", st, c)
		}
	}
	if st, ok := Code("nope").CloseStatus(); ok {
		t.Fatalf("an unknown code closes %d", st)
	}
}

// §4: the node does not retry exactly the seven codes the spec names; it retries the others.
func TestRetryableFollowsSpec(t *testing.T) {
	notRetried := specList(t, "The node does not retry", "without a change by its operator")
	if len(notRetried) != 7 {
		t.Fatalf("spec names %d codes: %v", len(notRetried), notRetried)
	}
	set := map[Code]bool{}
	for _, c := range notRetried {
		set[Code(c)] = true
	}
	var n int
	for _, c := range Codes {
		if c.Retryable() == set[c] {
			t.Fatalf("%s.Retryable() = %v", c, c.Retryable())
		}
		if !c.Retryable() {
			n++
		}
	}
	if n != 7 {
		t.Fatalf("%d codes are not retryable", n)
	}
	if !Code("from_a_later_version").Retryable() {
		t.Fatal("an unknown code is not retried")
	}
}

type testKey struct {
	id        *age.X25519Identity
	recipient string
	kid       string
}

func newTestKey(t *testing.T) testKey {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	r := id.Recipient().String()
	return testKey{id: id, recipient: r, kid: KeyID(r)}
}

func validHello(t *testing.T) *Hello {
	t.Helper()
	k, old := newTestKey(t), newTestKey(t)
	_, nonce, err := NewNonce()
	if err != nil {
		t.Fatal(err)
	}
	return &Hello{
		Origin:     "https://gate.team.internal",
		KID:        k.kid,
		Recipient:  k.recipient,
		Name:       "build-box-3",
		Nonce:      nonce,
		Services:   []Service{{Kind: KindA2A, Name: "hello"}, {Kind: KindMCP, Name: "fs"}},
		Enroll:     []string{"ket_0123456789abcdefghijABCDEFGHIJ01"},
		Auth:       "kk_acme_0123456789abcdefghijABCDEFGHIJ",
		RotateFrom: &KeyRef{KID: old.kid, Recipient: old.recipient},
		Agent:      "kontra/4.0.0 (linux/amd64)",
	}
}

func validChallenge(t *testing.T) *Challenge {
	_, nonce, _ := NewNonce()
	return &Challenge{
		Session: "0192f0b4-0000-7000-8000-000000000001",
		Nonce:   nonce,
		CB:      CBTLSExporter,
		Enc:     EncodeBase64URL(bytes.Repeat([]byte{5}, EncSize)),
		EncOld:  EncodeBase64URL(bytes.Repeat([]byte{6}, EncSize)),
	}
}

// §4: each message encodes and parses back to itself; a Hello's Raw is the bytes encoded, and the
// bytes parsed.
func TestHandshakeRoundTrip(t *testing.T) {
	h := validHello(t)
	b, err := Encode(h)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(h.Raw, b) || h.T != MsgHello || h.V != Version {
		t.Fatalf("Encode left Raw %q, t %q, v %d", h.Raw, h.T, h.V)
	}
	got, err := ParseHello(b)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, h) {
		t.Fatalf("hello round trip:\n%+v\n%+v", got, h)
	}

	c := validChallenge(t)
	b, err = Encode(c)
	if err != nil {
		t.Fatal(err)
	}
	gc, err := ParseChallenge(b)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gc, c) || gc.T != MsgChallenge || gc.V != Version {
		t.Fatalf("challenge round trip: %+v", gc)
	}

	p := &Proof{Proof: EncodeBase64URL(bytes.Repeat([]byte{1}, 32)), ProofOld: EncodeBase64URL(bytes.Repeat([]byte{2}, 32))}
	b, err = Encode(*p) // a value works too
	if err != nil {
		t.Fatal(err)
	}
	gp, err := ParseProof(b)
	if err != nil {
		t.Fatal(err)
	}
	if gp.Proof != p.Proof || gp.ProofOld != p.ProofOld || gp.T != MsgProof || gp.V != Version {
		t.Fatalf("proof round trip: %+v", gp)
	}

	exp := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	w := &Welcome{
		Session: "0192f0b4-0000-7000-8000-000000000001", Replica: "gate-a-1234abcd", PingS: 20, Tenant: "acme",
		Services:  []WelcomeService{{Kind: KindA2A, Name: "hello", URL: "https://gate.team.internal/agents/hello"}},
		Anonymous: &AnonymousInfo{ExpiresAt: exp},
	}
	b, err = Encode(w)
	if err != nil {
		t.Fatal(err)
	}
	gw, err := ParseOutcome(b)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gw, w) {
		t.Fatalf("welcome round trip:\n%+v\n%+v", gw, w)
	}

	b, err = Encode(&ErrorMessage{Code: CodeBadProof, Message: "the proof does not verify"})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte(`"t":"error","v":1,"code":"bad_proof"`)) {
		t.Fatalf("error message %s", b)
	}
	_, err = ParseOutcome(b)
	var he *HandshakeError
	if !errors.As(err, &he) || he.Code != CodeBadProof || he.Message != "the proof does not verify" {
		t.Fatalf("ParseOutcome(error) = %v", err)
	}
}

// Raw is byte-exact: whitespace, key order, escapes and unknown fields are kept as received, since
// the gate hashes M1 as sent (§4.1).
func TestHelloRawIsExact(t *testing.T) {
	h := validHello(t)
	odd := []byte("{ \"agent\" : \"kontra/4.0.0 (linux/amd64)\",\n\t\"v\":1, \"t\":\"hello\", \"future\": {\"x\": [1, 2]}," +
		"\"origin\":\"https://gate.team.internal\",\"kid\":\"" + h.KID + "\",\"recipient\":\"" + h.Recipient +
		"\",\"name\":\"build\\u002dbox\",\"nonce\":\"" + h.Nonce + "\",\"services\":[] }  ")
	got, err := ParseHello(odd)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Raw, odd) {
		t.Fatalf("Raw = %q", got.Raw)
	}
	if got.Name != "build-box" || len(got.Services) != 0 {
		t.Fatalf("parsed %+v", got)
	}
	odd[0] = ' ' // Raw is a copy, not the caller's buffer
	if got.Raw[0] != '{' {
		t.Fatal("Raw aliases the input")
	}
}

// §4: unknown fields are ignored in every message.
func TestUnknownFieldsIgnored(t *testing.T) {
	addField := func(b []byte) []byte {
		return append([]byte(`{"zz_future":{"nested":[true,null]},`), b[1:]...)
	}
	h := validHello(t)
	b, _ := Encode(h)
	if _, err := ParseHello(addField(b)); err != nil {
		t.Fatalf("hello: %v", err)
	}
	b, _ = Encode(validChallenge(t))
	if _, err := ParseChallenge(addField(b)); err != nil {
		t.Fatalf("challenge: %v", err)
	}
	b, _ = Encode(&Proof{Proof: EncodeBase64URL(make([]byte, 32))})
	if _, err := ParseProof(addField(b)); err != nil {
		t.Fatalf("proof: %v", err)
	}
	b, _ = Encode(&Welcome{Session: "0192f0b4-0000-7000-8000-000000000001", PingS: 20})
	if _, err := ParseOutcome(addField(b)); err != nil {
		t.Fatalf("welcome: %v", err)
	}
	b, _ = Encode(&ErrorMessage{Code: CodeDraining})
	var he *HandshakeError
	if _, err := ParseOutcome(addField(b)); !errors.As(err, &he) || he.Code != CodeDraining {
		t.Fatalf("error: %v", err)
	}
}

// §4: 16 KiB is the limit of every message, both ways.
func TestHandshakeSizeLimit(t *testing.T) {
	h := validHello(t)
	b, err := Encode(h)
	if err != nil {
		t.Fatal(err)
	}
	pad := func(b []byte, n int) []byte { // b grown to n bytes by trailing whitespace
		return append(bytes.Clone(b), bytes.Repeat([]byte{' '}, n-len(b))...)
	}
	if _, err := ParseHello(pad(b, MaxHandshakeMessage)); err != nil {
		t.Fatalf("a 16 KiB hello: %v", err)
	}
	_, err = ParseHello(pad(b, MaxHandshakeMessage+1))
	var he *HandshakeError
	if !errors.As(err, &he) || he.Code != CodeUnsupportedVersion {
		t.Fatalf("a 16 KiB + 1 hello: %v", err)
	}
	c, _ := Encode(validChallenge(t))
	if _, err := ParseChallenge(pad(c, MaxHandshakeMessage+1)); err == nil {
		t.Fatal("a 16 KiB + 1 challenge")
	}
	p, _ := Encode(&Proof{Proof: EncodeBase64URL(make([]byte, 32))})
	if _, err := ParseProof(pad(p, MaxHandshakeMessage+1)); err == nil {
		t.Fatal("a 16 KiB + 1 proof")
	}
	w, _ := Encode(&Welcome{Session: "0192f0b4-0000-7000-8000-000000000001", PingS: 20})
	if _, err := ParseOutcome(pad(w, MaxHandshakeMessage+1)); err == nil {
		t.Fatal("a 16 KiB + 1 welcome")
	}
	if _, err := ParseOutcome(pad(w, MaxHandshakeMessage)); err != nil {
		t.Fatalf("a 16 KiB welcome: %v", err)
	}

	big := validHello(t)
	big.Agent = strings.Repeat("a", MaxHandshakeMessage)
	if _, err := Encode(big); err == nil || big.Raw != nil {
		t.Fatalf("Encode of a hello over 16 KiB: %v, Raw %d bytes", err, len(big.Raw))
	}
	if _, err := Encode(&ErrorMessage{Code: CodeBadProof, Message: strings.Repeat("m", MaxHandshakeMessage)}); err == nil {
		t.Fatal("Encode of an error message over 16 KiB")
	}
}

func TestEncode(t *testing.T) {
	h := validHello(t)
	h.Services = nil
	b, err := Encode(h)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte(`"services":[]`)) {
		t.Fatalf("empty services encode as %s", b)
	}
	if h.Services != nil {
		t.Fatal("Encode changed the caller's Services")
	}
	w, err := Encode(&Welcome{Session: "0192f0b4-0000-7000-8000-000000000001", PingS: 20})
	if err != nil || !bytes.Contains(w, []byte(`"services":[]`)) {
		t.Fatalf("welcome %s, %v", w, err)
	}
	if _, err := Encode(*validHello(t)); err == nil {
		t.Fatal("a Hello value, whose Raw cannot be set, is encoded")
	}
	if _, err := Encode(&Proof{T: MsgWelcome}); err == nil {
		t.Fatal("a proof labelled welcome is encoded")
	}
	if _, err := Encode("hello"); err == nil {
		t.Fatal("a string is encoded")
	}
	v2 := validHello(t)
	v2.V = 2
	b, err = Encode(v2)
	if err != nil || !bytes.Contains(b, []byte(`"v":2`)) {
		t.Fatalf("an explicit v is kept: %s, %v", b, err)
	}
}

// ParseHello's refusals carry the code of what they are about (§4).
func TestParseHelloRefusals(t *testing.T) {
	type mut func(m map[string]any)
	other := newTestKey(t)
	cases := []struct {
		name string
		raw  string // used as is when set
		mut  mut
		code Code
	}{
		{"not json", "{", nil, CodeUnsupportedVersion},
		{"array", `[1]`, nil, CodeUnsupportedVersion},
		{"t", "", func(m map[string]any) { m["t"] = "proof" }, CodeUnsupportedVersion},
		{"v 2", "", func(m map[string]any) { m["v"] = 2 }, CodeUnsupportedVersion},
		{"v 2 with fields of another shape", "", func(m map[string]any) { m["v"] = 2; m["services"] = "all" }, CodeUnsupportedVersion},
		{"v missing", "", func(m map[string]any) { delete(m, "v") }, CodeUnsupportedVersion},
		{"v string", "", func(m map[string]any) { m["v"] = "1" }, CodeUnsupportedVersion},
		{"services not a list", "", func(m map[string]any) { m["services"] = "all" }, CodeUnsupportedVersion},
		{"origin with path", "", func(m map[string]any) { m["origin"] = "https://gate.team.internal/" }, CodeBadOrigin},
		{"origin upper case", "", func(m map[string]any) { m["origin"] = "https://Gate.team.internal" }, CodeBadOrigin},
		{"origin default port", "", func(m map[string]any) { m["origin"] = "https://gate.team.internal:443" }, CodeBadOrigin},
		{"origin ws", "", func(m map[string]any) { m["origin"] = "wss://gate.team.internal" }, CodeBadOrigin},
		{"origin empty", "", func(m map[string]any) { m["origin"] = "" }, CodeBadOrigin},
		{"kid of another key", "", func(m map[string]any) { m["kid"] = other.kid }, CodeBadProof},
		{"recipient upper case", "", func(m map[string]any) { m["recipient"] = strings.ToUpper(m["recipient"].(string)) }, CodeBadProof},
		{"recipient missing", "", func(m map[string]any) { delete(m, "recipient") }, CodeBadProof},
		{"rotate_from kid", "", func(m map[string]any) {
			m["rotate_from"] = map[string]any{"kid": other.kid, "recipient": m["recipient"]}
		}, CodeBadProof},
		{"name empty", "", func(m map[string]any) { m["name"] = "" }, CodeUnsupportedVersion},
		{"name 65", "", func(m map[string]any) { m["name"] = strings.Repeat("é", 65) }, CodeUnsupportedVersion},
		{"name NUL", "", func(m map[string]any) { m["name"] = "a\x00b" }, CodeUnsupportedVersion},
		{"agent NUL", "", func(m map[string]any) { m["agent"] = "kontra\x00" }, CodeUnsupportedVersion},
		{"nonce 31 bytes", "", func(m map[string]any) { m["nonce"] = EncodeBase64URL(make([]byte, 31)) }, CodeUnsupportedVersion},
		{"nonce std base64", "", func(m map[string]any) { m["nonce"] = strings.Repeat("+", 43) }, CodeUnsupportedVersion},
		{"service kind", "", func(m map[string]any) { m["services"] = []any{map[string]any{"kind": "grpc", "name": "x"}} }, CodeServiceDenied},
		{"service name", "", func(m map[string]any) { m["services"] = []any{map[string]any{"kind": "a2a", "name": "Hello"}} }, CodeServiceDenied},
		{"service name tenant/name", "", func(m map[string]any) { m["services"] = []any{map[string]any{"kind": "a2a", "name": "acme/hello"}} }, CodeServiceDenied},
		{"service twice", "", func(m map[string]any) {
			m["services"] = []any{map[string]any{"kind": "a2a", "name": "x"}, map[string]any{"kind": "mcp", "name": "x"}}
		}, CodeServiceDenied},
	}
	for _, c := range cases {
		b := []byte(c.raw)
		if c.raw == "" {
			h := validHello(t)
			enc, err := Encode(h)
			if err != nil {
				t.Fatal(err)
			}
			var m map[string]any
			if err := json.Unmarshal(enc, &m); err != nil {
				t.Fatal(err)
			}
			c.mut(m)
			if b, err = json.Marshal(m); err != nil {
				t.Fatal(err)
			}
		}
		h, err := ParseHello(b)
		var he *HandshakeError
		if !errors.As(err, &he) || he.Code != c.code {
			t.Fatalf("%s: ParseHello = %+v, %v; want %s", c.name, h, err, c.code)
		}
		if len(he.Message) > 400 {
			t.Fatalf("%s: a refusal message of %d bytes", c.name, len(he.Message))
		}
	}

	// A hello may declare no service (a node that only hires), with services null or [].
	h := validHello(t)
	h.Services = nil
	h.RotateFrom = nil
	h.Enroll = nil
	b, _ := Encode(h)
	if _, err := ParseHello(bytes.Replace(b, []byte(`"services":[]`), []byte(`"services":null`), 1)); err != nil {
		t.Fatal(err)
	}
	// A refusal quoting a huge value stays small enough to send back.
	m := map[string]any{}
	json.Unmarshal(b, &m)
	m["origin"] = "https://" + strings.Repeat("X", 15000)
	big, _ := json.Marshal(m)
	_, err := ParseHello(big)
	var he *HandshakeError
	if !errors.As(err, &he) || he.Code != CodeBadOrigin {
		t.Fatal(err)
	}
	if _, err := Encode(&ErrorMessage{Code: he.Code, Message: he.Message}); err != nil {
		t.Fatalf("the refusal cannot be sent: %v", err)
	}
}

func TestParseChallengeRefusals(t *testing.T) {
	cases := map[string]func(c *Challenge){
		"v 2":            func(c *Challenge) { c.V = 2 },
		"t":              func(c *Challenge) { c.T = MsgProof },
		"session":        func(c *Challenge) { c.Session = "not-a-uuid" },
		"session braces": func(c *Challenge) { c.Session = "{" + c.Session + "}" },
		"cb":             func(c *Challenge) { c.CB = "tls-unique" },
		"nonce":          func(c *Challenge) { c.Nonce = EncodeBase64URL(make([]byte, 16)) },
		"enc missing":    func(c *Challenge) { c.Enc = "" },
		"enc 33":         func(c *Challenge) { c.Enc = EncodeBase64URL(make([]byte, 33)) },
		"enc_old 31":     func(c *Challenge) { c.EncOld = EncodeBase64URL(make([]byte, 31)) },
	}
	for name, mut := range cases {
		c := validChallenge(t)
		c.T, c.V = MsgChallenge, Version
		mut(c)
		b, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := ParseChallenge(b); err == nil {
			t.Fatalf("%s: ParseChallenge = %+v", name, got)
		}
	}
	c := validChallenge(t)
	c.EncOld = ""
	c.CB = CBNone
	b, _ := Encode(c)
	if _, err := ParseChallenge(b); err != nil {
		t.Fatalf("a challenge with cb none and no enc_old: %v", err)
	}
}

func TestParseProofRefusals(t *testing.T) {
	ok := EncodeBase64URL(make([]byte, 32))
	cases := []struct {
		raw  string
		code Code
	}{
		{`{"t":"proof","v":2,"proof":"` + ok + `"}`, CodeUnsupportedVersion},
		{`{"t":"hello","v":1,"proof":"` + ok + `"}`, CodeUnsupportedVersion},
		{`{"t":"proof","v":1,"proof":5}`, CodeUnsupportedVersion},
		{`{"t":"proof","v":1}`, CodeBadProof},
		{`{"t":"proof","v":1,"proof":"` + EncodeBase64URL(make([]byte, 31)) + `"}`, CodeBadProof},
		{`{"t":"proof","v":1,"proof":"` + ok + `","proof_old":"!!"}`, CodeBadProof},
	}
	for _, c := range cases {
		_, err := ParseProof([]byte(c.raw))
		var he *HandshakeError
		if !errors.As(err, &he) || he.Code != c.code {
			t.Fatalf("ParseProof(%s) = %v; want %s", c.raw, err, c.code)
		}
	}
}

func TestParseOutcomeRefusals(t *testing.T) {
	for _, raw := range []string{
		`{"t":"challenge","v":1}`,
		`{"t":"welcome","v":2,"session":"0192f0b4-0000-7000-8000-000000000001","ping_s":20}`,
		`{"t":"welcome","v":1,"session":"x","ping_s":20}`,
		`{"t":"welcome","v":1,"session":"0192f0b4-0000-7000-8000-000000000001","ping_s":0}`,
		`{"t":"error","v":1,"message":"no code"}`,
		`{"t":"error","v":2,"code":"bad_proof"}`,
		`nope`,
	} {
		w, err := ParseOutcome([]byte(raw))
		var he *HandshakeError
		if err == nil || errors.As(err, &he) {
			t.Fatalf("ParseOutcome(%s) = %+v, %v; want the node's own error", raw, w, err)
		}
	}
	// A code this version does not know is still a refusal, and retried (§4 "the others").
	_, err := ParseOutcome([]byte(`{"t":"error","v":1,"code":"overloaded","message":"later"}`))
	var he *HandshakeError
	if !errors.As(err, &he) || he.Code != "overloaded" || !he.Code.Retryable() {
		t.Fatalf("unknown code: %v", err)
	}
}

// What a gate writes and the node prints on the operator's terminal carries no escape sequence (M2 review,
// security-5): a welcome whose tenant, service kind, name or URL is not of the shape the gate writes is the
// node's own error, never printed, and a refusal's code and message print with every control character, and
// every invisible formatting character such as a bidi override, escaped.
func TestOutcomeCarriesNoTerminalEscapes(t *testing.T) {
	welcome := func(tenant string, svc WelcomeService) []byte {
		t.Helper()
		b, err := Encode(&Welcome{Session: "0192f0b4-0000-7000-8000-000000000001", Replica: "gate-a", PingS: 20, Tenant: tenant,
			Services: []WelcomeService{svc}})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	good := WelcomeService{Kind: KindA2A, Name: "hello", URL: "https://gate.team.internal/agents/acme/hello"}
	if _, err := ParseOutcome(welcome("acme", good)); err != nil {
		t.Fatalf("a welcome of the gate's shape: %v", err)
	}
	if _, err := ParseOutcome(welcome("", good)); err != nil {
		t.Fatalf("a welcome without a tenant: %v", err)
	}
	osc52 := "\x1b]52;c;ZWNobyBvd25lZA==\x07"
	for name, b := range map[string][]byte{
		"an escape in the tenant":      welcome("acme"+osc52, good),
		"an uppercase tenant":          welcome("Acme", good),
		"an escape in the URL":         welcome("acme", WelcomeService{Kind: KindA2A, Name: "hello", URL: "https://gate.example/agents/hello" + osc52}),
		"a bidi override in the URL":   welcome("acme", WelcomeService{Kind: KindA2A, Name: "hello", URL: "https://gate.example/\u202ehello"}),
		"a URL that is not http(s)":    welcome("acme", WelcomeService{Kind: KindA2A, Name: "hello", URL: "javascript:alert(1)"}),
		"a URL without a host":         welcome("acme", WelcomeService{Kind: KindA2A, Name: "hello", URL: "https:///agents/hello"}),
		"an escape in the name":        welcome("acme", WelcomeService{Kind: KindA2A, Name: "hello" + osc52, URL: good.URL}),
		"a kind that is not a2a / mcp": welcome("acme", WelcomeService{Kind: "a2a\x1b[2J", Name: "hello", URL: good.URL}),
	} {
		w, err := ParseOutcome(b)
		var he *HandshakeError
		if err == nil || errors.As(err, &he) {
			t.Errorf("%s: ParseOutcome(%s) = %+v, %v; want the node's own error", name, b, w, err)
		}
	}

	_, err := ParseOutcome([]byte(`{"t":"error","v":1,"code":"bad_origin\u001b[31m","message":"go to \u001b]8;;https://evil.example\u0007here\u001b]8;;\u0007 \u202eexe.\nkontra connect: connected"}`))
	var he *HandshakeError
	if !errors.As(err, &he) {
		t.Fatalf("a refusal: %v", err)
	}
	msg := err.Error()
	if strings.ContainsFunc(msg, func(r rune) bool { return !unicode.IsGraphic(r) }) {
		t.Fatalf("the refusal prints as %q: a character that is not graphic", msg)
	}
	if !strings.Contains(msg, `\x1b]8;;https://evil.example\a`) || !strings.Contains(msg, `\u202e`) || !strings.Contains(msg, `\n`) {
		t.Fatalf("the refusal prints as %q, want its escapes visible", msg)
	}
	if got := Printable("a plain refusal: origin \"https://gate.example\" is not this gate's"); got != "a plain refusal: origin \"https://gate.example\" is not this gate's" {
		t.Fatalf("Printable changed a printable message: %q", got)
	}
	if got := Printable("bad \xff byte"); got != `bad \xff byte` {
		t.Fatalf("Printable of an invalid byte: %q", got)
	}
}

func TestRefuse(t *testing.T) {
	err := Refuse(CodeRevoked, "key %s revoked", "k1_x")
	if err.Code != CodeRevoked || err.Message != "key k1_x revoked" || err.Error() != "tunnel: revoked: key k1_x revoked" {
		t.Fatalf("%+v %q", err, err.Error())
	}
}

// FuzzParseHello: no panic; an accepted hello keeps the input as Raw, and re-encodes to a hello that
// parses to the same message and encodes to the same bytes. An absent, null or empty list (services may
// be empty, §4; enroll and public are optional) is one value: Encode writes services as [] and leaves
// out an empty enroll or public, so the comparison treats nil and empty alike.
func FuzzParseHello(f *testing.F) {
	k, _ := age.GenerateX25519Identity()
	r := k.Recipient().String()
	_, nonce, _ := NewNonce()
	h := &Hello{Origin: "https://gate.example", KID: KeyID(r), Recipient: r, Name: "n", Nonce: nonce,
		Services: []Service{{Kind: KindA2A, Name: "hello"}}, Agent: "kontra/4.0.0 (linux/amd64)"}
	b, _ := Encode(h)
	f.Add(b)
	f.Add([]byte(`{"t":"hello","v":1}`))
	f.Add([]byte(`{"t":"hello","v":2,"services":"x"}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		h, err := ParseHello(b)
		if err != nil {
			var he *HandshakeError
			if !errors.As(err, &he) {
				t.Fatalf("ParseHello refused with %T, not a *HandshakeError", err)
			}
			if _, ok := he.Code.CloseStatus(); !ok {
				t.Fatalf("refusal code %q is not in §4's list", he.Code)
			}
			return
		}
		if !bytes.Equal(h.Raw, b) {
			t.Fatal("Raw differs from the input")
		}
		if KeyID(h.Recipient) != h.KID {
			t.Fatal("accepted a kid that is not its recipient's")
		}
		h.Raw = nil
		again, err := Encode(h)
		if err != nil {
			return // a hello at the size limit can grow when re-encoded
		}
		h2, err := ParseHello(again)
		if err != nil {
			t.Fatalf("re-encoded hello refused: %v", err)
		}
		if !bytes.Equal(h2.Raw, again) {
			t.Fatal("Raw of the re-encoded hello differs from its bytes")
		}
		h2.Raw = nil
		third, err := Encode(h2)
		if err != nil || !bytes.Equal(third, again) {
			t.Fatalf("a re-parsed hello encodes to other bytes: %s\n%s, %v", again, third, err)
		}
		h.Raw, h2.Raw = nil, nil
		if !reflect.DeepEqual(normalHello(h), normalHello(h2)) {
			t.Fatalf("%+v\n%+v", h, h2)
		}
	})
}

// normalHello is a copy of h with each empty list nil.
func normalHello(h *Hello) Hello {
	c := *h
	if len(c.Services) == 0 {
		c.Services = nil
	}
	if len(c.Enroll) == 0 {
		c.Enroll = nil
	}
	if len(c.Public) == 0 {
		c.Public = nil
	}
	return c
}

// FuzzParseOutcome: no panic, and a refusal is never mistaken for a welcome.
func FuzzParseOutcome(f *testing.F) {
	f.Add([]byte(`{"t":"welcome","v":1,"session":"0192f0b4-0000-7000-8000-000000000001","ping_s":20,"services":[]}`))
	f.Add([]byte(`{"t":"error","v":1,"code":"bad_proof","message":"m"}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		w, err := ParseOutcome(b)
		if (w == nil) == (err == nil) {
			t.Fatalf("ParseOutcome = %v, %v", w, err)
		}
		if w != nil && (w.PingS < 1 || w.Session == "") {
			t.Fatalf("accepted %+v", w)
		}
	})
}
