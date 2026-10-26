package tunnel

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"net"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"filippo.io/age"

	"kontra.sh/kontra/internal/bech32"
)

// vector is testdata/transcript-vector.json, computed by testdata/transcript-vector.py without the Go code.
type vector struct {
	NodeScalarHex string `json:"node_scalar_hex"`
	NodeSecretKey string `json:"node_secret_key"`
	Recipient     string `json:"recipient"`
	KID           string `json:"kid"`
	OldScalarHex  string `json:"old_scalar_hex"`
	OldRecipient  string `json:"old_recipient"`
	OldKID        string `json:"old_kid"`
	Origin        string `json:"origin"`
	Hello         string `json:"hello"`
	Session       string `json:"session"`
	GateNonceHex  string `json:"gate_nonce_hex"`
	EncHex        string `json:"enc_hex"`
	EncOldHex     string `json:"enc_old_hex"`
	Cases         []struct {
		CB          string `json:"cb"`
		CBValueHex  string `json:"cb_value_hex"`
		InfoHex     string `json:"info_hex"`
		ProofHex    string `json:"proof_hex"`
		ProofOldHex string `json:"proof_old_hex"`
	} `json:"cases"`
}

func loadVector(t *testing.T) vector {
	t.Helper()
	b, err := os.ReadFile("testdata/transcript-vector.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vector
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Cases) != 2 {
		t.Fatalf("vector has %d cases", len(v.Cases))
	}
	return v
}

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

var keyIDRE = regexp.MustCompile(`^k1_[a-z2-7]{26}$`) // tunnel_keys.key_id's CHECK (0001)

// §2.2: the key id of a fixed recipient equals the independently computed one, and has the shape of
// the 0001 CHECK.
func TestKeyIDVector(t *testing.T) {
	v := loadVector(t)
	for _, c := range []struct{ recipient, kid string }{{v.Recipient, v.KID}, {v.OldRecipient, v.OldKID}} {
		if got := KeyID(c.recipient); got != c.kid {
			t.Fatalf("KeyID(%s) = %s, want %s", c.recipient, got, c.kid)
		}
		if !keyIDRE.MatchString(c.kid) || len(c.kid) != 29 {
			t.Fatalf("%q does not match %s", c.kid, keyIDRE)
		}
	}
	for range 50 {
		id, err := age.GenerateX25519Identity()
		if err != nil {
			t.Fatal(err)
		}
		if k := KeyID(id.Recipient().String()); !keyIDRE.MatchString(k) {
			t.Fatalf("KeyID = %q", k)
		}
	}
}

// §4.1: the info hash and both proofs of a fixed transcript equal the independently computed ones, for
// cb tls-exporter and cb none, and the vector's keys are what age reads.
func TestTranscriptVector(t *testing.T) {
	v := loadVector(t)
	id, err := age.ParseX25519Identity(v.NodeSecretKey)
	if err != nil {
		t.Fatal(err)
	}
	if id.Recipient().String() != v.Recipient {
		t.Fatalf("age reads the secret key's recipient as %s, the vector says %s", id.Recipient(), v.Recipient)
	}
	sk, err := ecdh.X25519().NewPrivateKey(unhex(t, v.NodeScalarHex))
	if err != nil {
		t.Fatal(err)
	}
	oldSK, err := ecdh.X25519().NewPrivateKey(unhex(t, v.OldScalarHex))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		sk        *ecdh.PrivateKey
		recipient string
	}{{sk, v.Recipient}, {oldSK, v.OldRecipient}} {
		pub, err := ParseRecipient(c.recipient)
		if err != nil {
			t.Fatal(err)
		}
		if !pub.Equal(c.sk.PublicKey()) {
			t.Fatalf("ParseRecipient(%s) is not the scalar's public key", c.recipient)
		}
	}
	h, err := ParseHello([]byte(v.Hello))
	if err != nil {
		t.Fatalf("the vector's hello: %v", err)
	}
	if h.Origin != v.Origin || h.KID != v.KID || h.RotateFrom == nil || h.RotateFrom.KID != v.OldKID {
		t.Fatalf("the vector's hello parsed as %+v", h)
	}
	for _, c := range v.Cases {
		tr := Transcript{
			Origin:    v.Origin,
			CB:        c.CB,
			CBValue:   unhex(t, c.CBValueHex),
			Hello:     h.Raw,
			Session:   v.Session,
			GateNonce: unhex(t, v.GateNonceHex),
		}
		info := tr.Info()
		if got := hex.EncodeToString(info); got != c.InfoHex {
			t.Fatalf("cb %s: info %s, want %s", c.CB, got, c.InfoHex)
		}
		proof, err := Prove(unhex(t, v.EncHex), sk, info, LabelPoP)
		if err != nil {
			t.Fatal(err)
		}
		if got := hex.EncodeToString(proof); got != c.ProofHex {
			t.Fatalf("cb %s: proof %s, want %s", c.CB, got, c.ProofHex)
		}
		proofOld, err := Prove(unhex(t, v.EncOldHex), oldSK, info, LabelPoPOld)
		if err != nil {
			t.Fatal(err)
		}
		if got := hex.EncodeToString(proofOld); got != c.ProofOldHex {
			t.Fatalf("cb %s: proof_old %s, want %s", c.CB, got, c.ProofOldHex)
		}
	}
}

// RFC 9180 A.7 (DHKEM(X25519, HKDF-SHA256), HKDF-SHA256, Export-Only AEAD): Prove is that suite's
// receiver export, so the gate's and the node's halves cannot drift to another suite unnoticed.
func TestProveIsRFC9180ExportOnly(t *testing.T) {
	sk, err := ecdh.X25519().NewPrivateKey(unhex(t, "33d196c830a12f9ac65d6e565a590d80f04ee9b19c83c87f2c170d972a812848"))
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(sk.PublicKey().Bytes()); got != "194141ca6c3c3beb4792cd97ba0ea1faff09d98435012345766ee33aae2d7664" {
		t.Fatalf("pkRm %s", got)
	}
	enc := unhex(t, "e5e8f9bfff6c2f29791fc351d2c25ce1299aa5eaca78a757c0b4fb4bcd830918")
	info := unhex(t, "4f6465206f6e2061204772656369616e2055726e")
	for _, c := range []struct{ context, want string }{
		{"", "7a36221bd56d50fb51ee65edfd98d06a23c4dc87085aa5866cb7087244bd2a36"},
		{"\x00", "d5535b87099c6c3ce80dc112a2671c6ec8e811a2f284f948cec6dd1708ee33f0"},
		{"TestContext", "ffaabc85a776136ca0c378e5d084c9140ab552b78f039d2e8775f26efff4c70e"},
	} {
		got, err := Prove(enc, sk, info, c.context)
		if err != nil {
			t.Fatal(err)
		}
		if hex.EncodeToString(got) != c.want {
			t.Fatalf("export %q = %x, want %s", c.context, got, c.want)
		}
	}
}

func newKey(t *testing.T) *ecdh.PrivateKey {
	t.Helper()
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func baseTranscript() Transcript {
	return Transcript{
		Origin:    "https://gate.example",
		CB:        CBTLSExporter,
		CBValue:   bytes.Repeat([]byte{7}, 32),
		Hello:     []byte(`{"t":"hello","v":1}`),
		Session:   "0192f0b4-0000-7000-8000-000000000001",
		GateNonce: bytes.Repeat([]byte{9}, 32),
	}
}

// §4.1: Encapsulate and Prove agree; any one field of the transcript changed, or another private key,
// and the gate does not accept the proof; a low-order enc is an error; proof_old's label gives another
// value.
func TestEncapsulateProve(t *testing.T) {
	node := newKey(t)
	base := baseTranscript()
	enc, want, err := Encapsulate(node.PublicKey(), base.Info(), LabelPoP)
	if err != nil {
		t.Fatal(err)
	}
	if len(enc) != EncSize || len(want) != ProofSize {
		t.Fatalf("enc %d bytes, want %d bytes", len(enc), len(want))
	}
	proof, err := Prove(enc, node, base.Info(), LabelPoP)
	if err != nil {
		t.Fatal(err)
	}
	if !Accept(proof, want) {
		t.Fatal("the node's proof is refused")
	}

	changes := map[string]func(*Transcript){
		"origin":      func(tr *Transcript) { tr.Origin = "https://gate.example:8443" },
		"cb":          func(tr *Transcript) { tr.CB = CBNone },
		"cb_value":    func(tr *Transcript) { tr.CBValue = bytes.Repeat([]byte{8}, 32) },
		"no cb_value": func(tr *Transcript) { tr.CBValue = nil },
		"hello":       func(tr *Transcript) { tr.Hello = []byte(`{"t":"hello","v":1} `) },
		"session":     func(tr *Transcript) { tr.Session = "0192f0b4-0000-7000-8000-000000000002" },
		"gate nonce":  func(tr *Transcript) { tr.GateNonce = bytes.Repeat([]byte{10}, 32) },
	}
	for name, change := range changes {
		tr := base
		change(&tr)
		if bytes.Equal(tr.Info(), base.Info()) {
			t.Fatalf("%s: the info hash did not change", name)
		}
		p, err := Prove(enc, node, tr.Info(), LabelPoP)
		if err != nil {
			t.Fatal(err)
		}
		if Accept(p, want) {
			t.Fatalf("%s differs between gate and node, and the proof is accepted", name)
		}
	}

	other, err := Prove(enc, newKey(t), base.Info(), LabelPoP)
	if err != nil {
		t.Fatal(err)
	}
	if Accept(other, want) {
		t.Fatal("another key's proof is accepted")
	}

	old, err := Prove(enc, node, base.Info(), LabelPoPOld)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(old, proof) || Accept(old, want) {
		t.Fatal("the pop-old/v1 export equals the pop/v1 export")
	}

	// A second encapsulation to the same key is fresh: replaying the first proof does not verify.
	enc2, want2, err := Encapsulate(node.PublicKey(), base.Info(), LabelPoP)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(enc, enc2) || Accept(proof, want2) {
		t.Fatal("a second challenge to the same key accepts the first proof")
	}
}

// Low-order points (RFC 7748 §6.1's all-zero output) as enc are refused by the node, and as a recipient
// by the gate and by ParseRecipient.
func TestLowOrderPoints(t *testing.T) {
	node := newKey(t)
	info := baseTranscript().Info()
	lowOrder := [][]byte{
		make([]byte, 32),                       // 0
		append([]byte{1}, make([]byte, 31)...), // 1
		unhex(t, "e0eb7a7c3b41b8ae1656e3faf19fc46ada098deb9c32b1fd866205165f49b800"), // order 8
		unhex(t, "5f9c95bca3508c24b1d0b1559c83ef5b04445cc4581c8e86d8224eddd09f1157"), // order 8
		unhex(t, "ecffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f"), // p - 1
	}
	for _, enc := range lowOrder {
		if _, err := Prove(enc, node, info, LabelPoP); err == nil {
			t.Fatalf("Prove accepted the low-order enc %x", enc)
		}
		pub, err := ecdh.X25519().NewPublicKey(enc)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := Encapsulate(pub, info, LabelPoP); err == nil {
			t.Fatalf("Encapsulate to the low-order point %x", enc)
		}
		r, err := bech32.Encode("age", enc)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ParseRecipient(r); err == nil {
			t.Fatalf("ParseRecipient accepted the low-order point %x", enc)
		}
	}
	if _, err := Prove(make([]byte, 31), node, info, LabelPoP); err == nil {
		t.Fatal("Prove accepted a 31-byte enc")
	}
	p256, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Encapsulate(p256.PublicKey(), info, LabelPoP); err == nil {
		t.Fatal("Encapsulate to a P-256 key")
	}
	if _, err := Prove(make([]byte, 32), p256, info, LabelPoP); err == nil {
		t.Fatal("Prove with a P-256 key")
	}
}

// §4.1 "no relay to another gate" (C2): a gate the node dialed (origin https://a) relays gate B's challenge
// to the node. Whether it forwards the node's hello unmodified or rewrites its origin to B's, gate B
// computes the transcript with its own origin and the hello it received, and the node's proof (bound to
// https://a and the hello it sent) does not verify there.
func TestNoRelayToAnotherGate(t *testing.T) {
	node := newKey(t)
	helloA := []byte(`{"t":"hello","v":1,"origin":"https://a","nonce":"x"}`)
	helloB := []byte(`{"t":"hello","v":1,"origin":"https://b","nonce":"x"}`)
	sessionB := "0192f0b4-0000-7000-8000-0000000000bb"
	nonceB := bytes.Repeat([]byte{0xbb}, 32)

	for name, received := range map[string][]byte{"forwarded": helloA, "rewritten": helloB} {
		// Gate B: its own origin (it requires hello.origin to equal it), the hello as it received it.
		atB := Transcript{Origin: "https://b", CB: CBNone, Hello: received, Session: sessionB, GateNonce: nonceB}
		enc, want, err := Encapsulate(node.PublicKey(), atB.Info(), LabelPoP)
		if err != nil {
			t.Fatal(err)
		}
		// The node, connected to gate A, answers B's relayed challenge with what it dialed and sent.
		atNode := Transcript{Origin: "https://a", CB: CBNone, Hello: helloA, Session: sessionB, GateNonce: nonceB}
		proof, err := Prove(enc, node, atNode.Info(), LabelPoP)
		if err != nil {
			t.Fatal(err)
		}
		if Accept(proof, want) {
			t.Fatalf("%s hello: gate B accepts a proof the node made for gate A", name)
		}
	}
}

func TestAccept(t *testing.T) {
	a := bytes.Repeat([]byte{1}, ProofSize)
	b := bytes.Repeat([]byte{1}, ProofSize)
	if !Accept(a, b) {
		t.Fatal("equal proofs refused")
	}
	c := bytes.Clone(b)
	c[ProofSize-1] ^= 1
	for _, x := range []struct{ proof, want []byte }{
		{nil, nil}, {[]byte{}, []byte{}}, {a, c}, {a[:31], b[:31]}, {append(bytes.Clone(a), 1), append(bytes.Clone(b), 1)}, {a, nil}, {nil, b},
	} {
		if Accept(x.proof, x.want) {
			t.Fatalf("Accept(%x, %x)", x.proof, x.want)
		}
	}
}

func TestNewNonce(t *testing.T) {
	raw, enc, err := NewNonce()
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != NonceSize || strings.ContainsAny(enc, "=+/") {
		t.Fatalf("nonce %x as %q", raw, enc)
	}
	back, err := DecodeBase64URL(enc)
	if err != nil || !bytes.Equal(back, raw) {
		t.Fatalf("DecodeBase64URL(%q) = %x, %v", enc, back, err)
	}
	raw2, _, _ := NewNonce()
	if bytes.Equal(raw, raw2) {
		t.Fatal("two nonces are equal")
	}
	if _, err := DecodeBase64URL(enc[:10] + "\n" + enc[10:]); err == nil {
		t.Fatal("a line break inside base64url is accepted")
	}
	padded := base64.URLEncoding.EncodeToString([]byte{1, 2}) // "AQI="
	if b, err := DecodeBase64URL(padded); err != nil || !bytes.Equal(b, []byte{1, 2}) {
		t.Fatalf("padded base64url: %x, %v", b, err)
	}
}

// selfSigned is a TLS certificate for 127.0.0.1.
func selfSigned(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "gate.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"gate.test"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// tlsPair runs one TLS handshake over a pipe and returns both sides' connection states.
func tlsPair(t *testing.T, cert tls.Certificate, maxVersion uint16) (client, server tls.ConnectionState) {
	t.Helper()
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	srv := tls.Server(a, &tls.Config{Certificates: []tls.Certificate{cert}, MaxVersion: maxVersion})
	cli := tls.Client(b, &tls.Config{InsecureSkipVerify: true, MaxVersion: maxVersion})
	errc := make(chan error, 1)
	go func() { errc <- srv.Handshake() }()
	if err := cli.Handshake(); err != nil {
		t.Fatal(err)
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	return cli.ConnectionState(), srv.ConnectionState()
}

// RFC 9266: both ends of one TLS connection read the same 32-byte exporter, another connection reads
// another; no TLS state, an incomplete handshake or a state without an exporter is an error, not an empty
// value.
func TestChannelBinding(t *testing.T) {
	cert := selfSigned(t)
	for _, version := range []uint16{tls.VersionTLS13, tls.VersionTLS12} {
		c1, s1 := tlsPair(t, cert, version)
		c2, _ := tlsPair(t, cert, version)
		cb1, err := ChannelBinding(&c1)
		if err != nil {
			t.Fatal(err)
		}
		sb1, err := ChannelBinding(&s1)
		if err != nil {
			t.Fatal(err)
		}
		cb2, err := ChannelBinding(&c2)
		if err != nil {
			t.Fatal(err)
		}
		if len(cb1) != ChannelBindingSize || !bytes.Equal(cb1, sb1) {
			t.Fatalf("TLS %x: client %x, server %x", version, cb1, sb1)
		}
		if bytes.Equal(cb1, cb2) {
			t.Fatalf("TLS %x: two connections share an exporter", version)
		}
	}
	for name, cs := range map[string]*tls.ConnectionState{
		"nil":         nil,
		"incomplete":  {},
		"no exporter": {HandshakeComplete: true},
	} {
		if v, err := ChannelBinding(cs); err == nil || v != nil {
			t.Fatalf("%s: ChannelBinding = %x, %v", name, v, err)
		}
	}
}

// ParseRecipient takes only the canonical string of an X25519 key; FormatRecipient gives it back.
func TestParseRecipient(t *testing.T) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	r := id.Recipient().String()
	pub, err := ParseRecipient(r)
	if err != nil {
		t.Fatal(err)
	}
	back, err := FormatRecipient(pub)
	if err != nil || back != r {
		t.Fatalf("FormatRecipient(ParseRecipient(%s)) = %s, %v", r, back, err)
	}

	hy, err := age.GenerateHybridIdentity()
	if err != nil {
		t.Fatal(err)
	}
	_, raw, err := bech32.Decode(r)
	if err != nil {
		t.Fatal(err)
	}
	highBit := bytes.Clone(raw)
	highBit[31] |= 0x80                                                                        // the same point as raw for X25519, but another string and key id
	notReduced := unhex(t, "edffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f") // p itself
	refused := map[string]string{
		"upper case":   strings.ToUpper(r),
		"hybrid":       hy.Recipient().String(),
		"bad checksum": r[:len(r)-1] + map[bool]string{true: "q", false: "p"}[r[len(r)-1] != 'q'],
		"secret key":   id.String(),
		"empty":        "",
		"short":        mustBech32(t, "age", raw[:31]),
		"other hrp":    mustBech32(t, "agf", raw),
		"high bit":     mustBech32(t, "age", highBit),
		"not reduced":  mustBech32(t, "age", notReduced),
		"space":        " " + r,
	}
	for name, s := range refused {
		if _, err := ParseRecipient(s); err == nil {
			t.Fatalf("%s: ParseRecipient(%q) accepted", name, s)
		}
	}
	p256, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := FormatRecipient(p256.PublicKey()); err == nil {
		t.Fatal("FormatRecipient of a P-256 key")
	}
	if _, err := FormatRecipient(nil); err == nil {
		t.Fatal("FormatRecipient(nil)")
	}
}

func mustBech32(t *testing.T, hrp string, data []byte) string {
	t.Helper()
	s, err := bech32.Encode(hrp, data)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
