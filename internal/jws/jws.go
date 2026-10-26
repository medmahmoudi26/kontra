// Package jws is the gate's card signature and the keys behind it (spec/relay.md §3, D13, D166): an ES256 JWS
// whose payload is the RFC 8785 JCS of a card without its `signatures` member, detached as A2A 1.0 carries it
// (`AgentCardSignature{protected, signature, header?}`), and the JWK Set the gate publishes at
// /.well-known/jwks.json.
//
// The same verification serves three readers: kontra connect and kontra seal verify the gate's signature with
// a `jku` equal to the origin they dialed (spec/tunnel.md §9, spec/ext-sealed.md §5.1), and the gate verifies a
// seller's signatures on the upstream card against the keys the operator configured (spec/relay.md §3,
// services.<name>.seller_keys; internal/card SellerKeys). The gate signs with its own code because a2a-go's
// signer cannot add the `kontra.upstream` header parameter (D13); card_passthrough checks the result against
// a2a-go's verifier.
//
// signing_key (D166) is file:<path> of a PEM file of ECDSA P-256 private keys: the first signs, every key is
// published, kid is the RFC 7638 thumbprint. A gate whose public_url is loopback and that has no signing_key
// signs with EphemeralKeySet's key (one replica, development); any other gate serving A2A services refuses to
// start without one.
//
// Work package: M3 WP-01 (this package), M3 WP-02 (the gate's use of it).
package jws

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"slices"
	"strings"

	"kontra.sh/kontra/internal/jcs"
	"kontra.sh/kontra/internal/strictjson"
)

// Fixed values of the card signature and its key set.
const (
	Alg = "ES256" // the only algorithm the gate signs with or accepts from itself
	Typ = "JOSE"
	// JWKSPath is where the gate serves its public keys (spec/relay.md §2.1).
	JWKSPath = "/.well-known/jwks.json"
	// JWKSCacheControl lets clients cache the key set across a rotation's overlap (D166).
	JWKSCacheControl = "max-age=300"
	// HeaderUpstream is the protected header parameter that binds the upstream card (spec/relay.md §3). It is
	// never listed in crit, so stock verifiers accept the card.
	HeaderUpstream = "kontra.upstream"
)

// Limits on what this package reads.
const (
	maxKeyFile   = 1 << 20 // a PEM file of signing keys
	maxProtected = 16 << 10
	coordSize    = 32 // P-256 coordinate and scalar length
)

// ErrVerification is wrapped by every Verify refusal.
var ErrVerification = errors.New("jws: signature not verified")

// Signature is one A2A 1.0 AgentCardSignature: the base64url protected header, the base64url signature over
// protected "." base64url(JCS(card without signatures)), and an optional unprotected header.
type Signature struct {
	Protected string          `json:"protected"`
	Signature string          `json:"signature"`
	Header    json.RawMessage `json:"header,omitempty"`
}

// Upstream is the kontra.upstream header parameter: the hex SHA-256 over the JCS of the upstream card, the URL
// it is served at ({public_url}{svc}/.well-known/agent-card.upstream.json), and whether the gate verified a
// seller signature on it against configured keys.
type Upstream struct {
	SHA256   string `json:"sha256"`
	URL      string `json:"url"`
	Verified bool   `json:"verified"`
}

// Header is a decoded protected header.
type Header struct {
	Alg      string    `json:"alg"`
	Kid      string    `json:"kid"`
	Typ      string    `json:"typ,omitempty"`
	JKU      string    `json:"jku,omitempty"`
	Upstream *Upstream `json:"kontra.upstream,omitempty"`
}

// KeySet holds the gate's signing keys: Active signs, every key is published (a rotation's overlap, D166).
type KeySet struct {
	keys []*ecdsa.PrivateKey
	kids []string
}

// LoadKeySet reads signing_key's PEM file: one or more ECDSA P-256 private keys (PKCS #8 or SEC 1), the first
// active. It refuses a file others may read or write or the group may write (a Kubernetes secret mounted with
// fsGroup is group-readable), a key of another curve, and a file with no key.
//
// A symbolic link (a mounted secret's ..data link) is followed and its target checked. An "EC PARAMETERS" block
// (openssl ecparam -genkey writes one) is skipped; any other PEM block, a key listed twice, a block that cannot be
// decoded (bad base64, no END line, text after the END marker on its line), and anything but white space before,
// between or after the blocks are refused. encoding/pem alone skips an undecodable block and any text before a
// block, so a rotation that prepended a mangled new key would keep the old key active without a word (D166).
func LoadKeySet(path string) (*KeySet, error) {
	// Refuse a FIFO or a device before opening it (an open could block), and check what was opened again.
	if fi, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("jws: signing key: %w", err)
	} else if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("jws: signing key %s is not a regular file", path)
	}
	fh, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("jws: signing key: %w", err)
	}
	defer fh.Close()
	fi, err := fh.Stat()
	if err != nil {
		return nil, fmt.Errorf("jws: signing key: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("jws: signing key %s is not a regular file", path)
	}
	if perm := fi.Mode().Perm(); perm&0o027 != 0 {
		return nil, fmt.Errorf("jws: signing key %s has mode %04o: it must not be readable or writable by others or "+
			"writable by its group (chmod 600, or 440 for a secret mounted with fsGroup)", path, perm)
	}
	data, err := io.ReadAll(io.LimitReader(fh, maxKeyFile+1))
	if err != nil {
		return nil, fmt.Errorf("jws: signing key: %w", err)
	}
	if len(data) > maxKeyFile {
		return nil, fmt.Errorf("jws: signing key %s is over %d bytes", path, maxKeyFile)
	}
	ks, err := parseKeys(data)
	if err != nil {
		// Never quote the file: it holds private keys.
		return nil, fmt.Errorf("jws: signing key %s: %w", path, err)
	}
	return ks, nil
}

// pemBegin starts every PEM block's first line.
var pemBegin = []byte("-----BEGIN ")

func parseKeys(data []byte) (*KeySet, error) {
	ks := &KeySet{}
	rest := data
	for n := 1; ; n++ {
		rest = bytes.TrimLeft(rest, " \t\r\n")
		if len(rest) == 0 {
			break
		}
		if !bytes.HasPrefix(rest, pemBegin) {
			if n == 1 {
				return nil, errors.New("text before the first PEM block")
			}
			if !bytes.Contains(rest, pemBegin) {
				return nil, errors.New("data after the last PEM block")
			}
			return nil, fmt.Errorf("text between PEM blocks %d and %d", n-1, n)
		}
		block, next := pem.Decode(rest)
		// pem.Decode skips a block it cannot decode and returns the next one: the block must be the one rest starts
		// with, so what it consumed holds exactly one BEGIN line.
		if block == nil || bytes.Count(rest[:len(rest)-len(next)], pemBegin) != 1 {
			return nil, fmt.Errorf("PEM block %d cannot be decoded (bad base64, or no END line)", n)
		}
		rest = next
		var key any
		var err error
		switch block.Type {
		case "EC PARAMETERS":
			continue
		case "PRIVATE KEY":
			key, err = x509.ParsePKCS8PrivateKey(block.Bytes)
		case "EC PRIVATE KEY":
			key, err = x509.ParseECPrivateKey(block.Bytes)
		default:
			return nil, fmt.Errorf("PEM block %d is %q, not an ECDSA P-256 private key (PRIVATE KEY or EC PRIVATE KEY)", n, block.Type)
		}
		if err != nil {
			return nil, fmt.Errorf("PEM block %d: %w", n, err)
		}
		ec, ok := key.(*ecdsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("PEM block %d is a %T, not an ECDSA P-256 key", n, key)
		}
		if ec.Curve != elliptic.P256() {
			return nil, fmt.Errorf("PEM block %d is an ECDSA key on %s, not P-256", n, ec.Curve.Params().Name)
		}
		kid, err := thumbprint(&ec.PublicKey)
		if err != nil {
			return nil, fmt.Errorf("PEM block %d: %w", n, err)
		}
		if slices.Contains(ks.kids, kid) {
			return nil, fmt.Errorf("PEM block %d repeats key %s", n, kid)
		}
		ks.keys = append(ks.keys, ec)
		ks.kids = append(ks.kids, kid)
	}
	if len(ks.keys) == 0 {
		return nil, errors.New("no ECDSA P-256 private key in the file")
	}
	return ks, nil
}

// EphemeralKeySet mints one P-256 key for a loopback gate without signing_key (D166). Its cards verify only
// against this process's JWKS.
func EphemeralKeySet() (*KeySet, error) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("jws: generate: %w", err)
	}
	kid, err := thumbprint(&k.PublicKey)
	if err != nil {
		return nil, err
	}
	return &KeySet{keys: []*ecdsa.PrivateKey{k}, kids: []string{kid}}, nil
}

// KeyID is the active key's kid, its RFC 7638 thumbprint (base64url SHA-256).
func (k *KeySet) KeyID() (string, error) {
	if k == nil || len(k.kids) == 0 {
		return "", errors.New("jws: empty key set")
	}
	return k.kids[0], nil
}

// KeyIDs are every key's kid, the active one first.
func (k *KeySet) KeyIDs() []string {
	if k == nil {
		return nil
	}
	return slices.Clone(k.kids)
}

// jwk is a public EC JWK as this package writes it (RFC 7517, RFC 7518 §6.2.1).
type jwk struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
}

// JWKS returns the JSON of the public JWK Set: every key, active first, each with kty EC, crv P-256, x, y, kid,
// use sig and alg ES256.
func (k *KeySet) JWKS() ([]byte, error) {
	if k == nil || len(k.keys) == 0 {
		return nil, errors.New("jws: empty key set")
	}
	set := struct {
		Keys []jwk `json:"keys"`
	}{}
	for i, key := range k.keys {
		x, y, err := coords(&key.PublicKey)
		if err != nil {
			return nil, err
		}
		set.Keys = append(set.Keys, jwk{Kty: "EC", Crv: "P-256", X: x, Y: y, Kid: k.kids[i], Use: "sig", Alg: Alg})
	}
	return json.Marshal(set)
}

// Sign signs the JCS of payload (a card without signatures) with the active key: protected header {alg ES256,
// kid, typ JOSE, jku, kontra.upstream: up} (up nil leaves it out).
//
// The protected header is itself the JCS of those members, jku is left out when empty, and kontra.upstream is
// never named in crit. A top-level signatures member of payload is left out of what is signed, as A2A 1.0 §8.4
// and a2a-go's verifier do. The signature is ES256's R ‖ S, 64 bytes, base64url without padding.
func (k *KeySet) Sign(payload []byte, jku string, up *Upstream) (Signature, error) {
	if k == nil || len(k.keys) == 0 {
		return Signature{}, errors.New("jws: empty key set")
	}
	canon, err := CardPayload(payload)
	if err != nil {
		return Signature{}, err
	}
	hdr := Header{Alg: Alg, Kid: k.kids[0], Typ: Typ, JKU: jku, Upstream: up}
	hb, err := json.Marshal(hdr)
	if err != nil {
		return Signature{}, fmt.Errorf("jws: header: %w", err)
	}
	if hb, err = jcs.Transform(hb); err != nil {
		return Signature{}, fmt.Errorf("jws: header: %w", err)
	}
	protected := base64.RawURLEncoding.EncodeToString(hb)
	digest := sha256.Sum256([]byte(protected + "." + base64.RawURLEncoding.EncodeToString(canon)))
	r, s, err := ecdsa.Sign(rand.Reader, k.keys[0], digest[:])
	if err != nil {
		return Signature{}, fmt.Errorf("jws: sign: %w", err)
	}
	sig := make([]byte, 2*coordSize)
	r.FillBytes(sig[:coordSize])
	s.FillBytes(sig[coordSize:])
	return Signature{Protected: protected, Signature: base64.RawURLEncoding.EncodeToString(sig)}, nil
}

// CardPayload is what a card signature covers: the JCS of the card without its top-level signatures member
// (A2A 1.0 §8.4.2). The card must be one JSON object with no repeated key.
func CardPayload(card []byte) ([]byte, error) {
	m, err := strictjson.Members(card)
	if err != nil {
		return nil, fmt.Errorf("jws: card: %w", err)
	}
	if _, ok := m["signatures"]; ok {
		delete(m, "signatures")
		if card, err = json.Marshal(m); err != nil {
			return nil, fmt.Errorf("jws: card: %w", err)
		}
	}
	out, err := jcs.Transform(card)
	if err != nil {
		return nil, fmt.Errorf("jws: card: %w", err)
	}
	return out, nil
}

// KeySetPublic is a parsed public JWK Set: the gate's (fetched from jku) or a seller's (seller_keys).
type KeySetPublic struct {
	keys map[string]*ecdsa.PublicKey // by kid
}

// ParseJWKS parses a JWK Set, keeping the EC P-256 keys (and refusing none at all). Keys of other types are
// skipped: a seller's set may hold keys the gate does not verify with.
//
// Members are read with exact keys. A key is kept when kty is "EC" and crv "P-256", its use (when present) is
// "sig" and its alg (when present) "ES256"; x and y must then be 32 bytes each, base64url, of a point on the
// curve, or the set is refused. A key without kid is found by its thumbprint. Two kept keys with one kid refuse
// the set.
func ParseJWKS(b []byte) (*KeySetPublic, error) {
	top, err := strictjson.Members(b)
	if err != nil {
		return nil, fmt.Errorf("jws: JWKS: %w", err)
	}
	rawKeys, ok := top["keys"]
	if !ok {
		return nil, errors.New("jws: JWKS has no keys member")
	}
	var list []json.RawMessage
	if err := json.Unmarshal(rawKeys, &list); err != nil {
		return nil, fmt.Errorf("jws: JWKS keys is not an array: %w", err)
	}
	set := &KeySetPublic{keys: map[string]*ecdsa.PublicKey{}}
	for i, raw := range list {
		m, err := strictjson.Members(raw)
		if err != nil {
			return nil, fmt.Errorf("jws: JWKS key %d: %w", i, err)
		}
		str := func(name string) (string, bool, error) {
			v, ok := m[name]
			if !ok {
				return "", false, nil
			}
			var s string
			if err := json.Unmarshal(v, &s); err != nil || !isString(v) {
				return "", true, fmt.Errorf("jws: JWKS key %d: %s is not a string", i, name)
			}
			return s, true, nil
		}
		kty, _, err := str("kty")
		if err != nil {
			return nil, err
		}
		crv, _, err := str("crv")
		if err != nil {
			return nil, err
		}
		use, hasUse, err := str("use")
		if err != nil {
			return nil, err
		}
		alg, hasAlg, err := str("alg")
		if err != nil {
			return nil, err
		}
		if kty != "EC" || crv != "P-256" || (hasUse && use != "sig") || (hasAlg && alg != Alg) {
			continue
		}
		x, _, err := str("x")
		if err != nil {
			return nil, err
		}
		y, _, err := str("y")
		if err != nil {
			return nil, err
		}
		pub, err := publicKey(x, y)
		if err != nil {
			return nil, fmt.Errorf("jws: JWKS key %d: %w", i, err)
		}
		kid, hasKid, err := str("kid")
		if err != nil {
			return nil, err
		}
		if !hasKid || kid == "" {
			if kid, err = thumbprint(pub); err != nil {
				return nil, err
			}
		}
		if _, dup := set.keys[kid]; dup {
			return nil, fmt.Errorf("jws: JWKS lists kid %q twice", kid)
		}
		set.keys[kid] = pub
	}
	if len(set.keys) == 0 {
		return nil, errors.New("jws: JWKS has no EC P-256 signing key")
	}
	return set, nil
}

// Thumbprints are the RFC 7638 thumbprints of the set's keys, sorted (pins record a seller's, ext-sealed §7).
func (s *KeySetPublic) Thumbprints() ([]string, error) {
	if s == nil {
		return nil, errors.New("jws: empty key set")
	}
	var out []string
	for _, k := range s.keys {
		t, err := thumbprint(k)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	slices.Sort(out)
	return out, nil
}

// Verify checks sig over the JCS of payload against keys, by the protected header's kid, and returns that
// header. A header naming another algorithm, a kid the set does not hold, or a crit member is refused.
//
// The protected header's bytes need not be JCS: a2a-go v2.7.0's signer writes them with json.Marshal
// (a2acrypto/sign.go). They are decoded with exact keys, a repeated or case-variant member is refused, and the
// signature is checked over the protected string as received. typ, jku and kontra.upstream are returned as read
// (kontra.upstream with exactly its three members); other members are ignored, since none is critical. An
// unprotected header may not repeat a protected member or carry crit (RFC 7515 §4, §7.2.1). Payload is a card:
// its top-level signatures member is left out (CardPayload). Every refusal wraps ErrVerification.
func Verify(payload []byte, sig Signature, keys *KeySetPublic) (Header, error) {
	fail := func(format string, a ...any) (Header, error) {
		return Header{}, fmt.Errorf("%w: %s", ErrVerification, fmt.Sprintf(format, a...))
	}
	if keys == nil || len(keys.keys) == 0 {
		return fail("no keys")
	}
	if len(sig.Protected) > maxProtected {
		return fail("protected header over %d bytes", maxProtected)
	}
	hb, err := b64url(sig.Protected)
	if err != nil {
		return fail("protected header is not base64url: %v", err)
	}
	m, err := strictjson.Members(hb)
	if err != nil {
		return fail("protected header: %v", err)
	}
	if _, ok := m["crit"]; ok {
		return fail("crit is not supported")
	}
	var h Header
	for _, f := range []struct {
		name string
		dst  *string
		req  bool
	}{{"alg", &h.Alg, true}, {"kid", &h.Kid, true}, {"typ", &h.Typ, false}, {"jku", &h.JKU, false}} {
		v, ok := m[f.name]
		if !ok {
			if f.req {
				return fail("protected header has no %s", f.name)
			}
			continue
		}
		if !isString(v) || json.Unmarshal(v, f.dst) != nil {
			return fail("protected header %s is not a string", f.name)
		}
	}
	if h.Alg != Alg {
		return fail("algorithm %q is not %s", h.Alg, Alg)
	}
	if v, ok := m[HeaderUpstream]; ok {
		up, err := parseUpstream(v)
		if err != nil {
			return fail("%s: %v", HeaderUpstream, err)
		}
		h.Upstream = up
	}
	if len(bytes.TrimSpace(sig.Header)) != 0 && !bytes.Equal(bytes.TrimSpace(sig.Header), []byte("null")) {
		um, err := strictjson.Members(sig.Header)
		if err != nil {
			return fail("unprotected header: %v", err)
		}
		for name := range um {
			if _, dup := m[name]; dup || name == "crit" {
				return fail("unprotected header repeats %s", name)
			}
		}
	}
	pub, ok := keys.keys[h.Kid]
	if !ok {
		return fail("no key for kid %q", h.Kid)
	}
	canon, err := CardPayload(payload)
	if err != nil {
		return fail("%v", err)
	}
	raw, err := b64url(sig.Signature)
	if err != nil {
		return fail("signature is not base64url: %v", err)
	}
	if len(raw) != 2*coordSize {
		return fail("an ES256 signature is %d bytes, not %d", 2*coordSize, len(raw))
	}
	digest := sha256.Sum256([]byte(sig.Protected + "." + base64.RawURLEncoding.EncodeToString(canon)))
	r := new(big.Int).SetBytes(raw[:coordSize])
	s := new(big.Int).SetBytes(raw[coordSize:])
	if !ecdsa.Verify(pub, digest[:], r, s) {
		return fail("bad signature")
	}
	return h, nil
}

func parseUpstream(v json.RawMessage) (*Upstream, error) {
	m, err := strictjson.Members(v)
	if err != nil {
		return nil, err
	}
	if len(m) != 3 {
		return nil, errors.New("want exactly sha256, url and verified")
	}
	up := &Upstream{}
	for name, dst := range map[string]*string{"sha256": &up.SHA256, "url": &up.URL} {
		raw, ok := m[name]
		if !ok || !isString(raw) || json.Unmarshal(raw, dst) != nil {
			return nil, fmt.Errorf("%s is not a string", name)
		}
	}
	raw, ok := m["verified"]
	if !ok {
		return nil, errors.New("no verified")
	}
	switch string(bytes.TrimSpace(raw)) {
	case "true":
		up.Verified = true
	case "false":
	default:
		return nil, errors.New("verified is not a boolean")
	}
	return up, nil
}

// thumbprint is the RFC 7638 thumbprint of an EC P-256 key: base64url(SHA-256(`{"crv":"P-256","kty":"EC",
// "x":…,"y":…}`)), the members in lexicographic order with no white space.
func thumbprint(pub *ecdsa.PublicKey) (string, error) {
	x, y, err := coords(pub)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(`{"crv":"P-256","kty":"EC","x":"` + x + `","y":"` + y + `"}`))
	return base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

// coords are the key's x and y, 32 bytes each, base64url without padding.
func coords(pub *ecdsa.PublicKey) (x, y string, err error) {
	if pub == nil || pub.Curve != elliptic.P256() {
		return "", "", errors.New("jws: not a P-256 key")
	}
	b, err := pub.Bytes() // 0x04 ‖ X ‖ Y
	if err != nil {
		return "", "", fmt.Errorf("jws: %w", err)
	}
	if len(b) != 1+2*coordSize {
		return "", "", errors.New("jws: unexpected P-256 point encoding")
	}
	return base64.RawURLEncoding.EncodeToString(b[1 : 1+coordSize]), base64.RawURLEncoding.EncodeToString(b[1+coordSize:]), nil
}

// publicKey builds a P-256 key from base64url coordinates, refusing a point that is not on the curve.
func publicKey(x, y string) (*ecdsa.PublicKey, error) {
	xb, err := b64url(x)
	if err != nil {
		return nil, fmt.Errorf("x is not base64url: %w", err)
	}
	yb, err := b64url(y)
	if err != nil {
		return nil, fmt.Errorf("y is not base64url: %w", err)
	}
	if len(xb) != coordSize || len(yb) != coordSize {
		return nil, fmt.Errorf("x and y must be %d bytes each", coordSize)
	}
	pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), append(append([]byte{4}, xb...), yb...))
	if err != nil {
		return nil, fmt.Errorf("not a P-256 point: %w", err)
	}
	return pub, nil
}

// b64url decodes base64url without padding, refusing padding, white space and the standard alphabet's + and /.
func b64url(s string) ([]byte, error) {
	if strings.ContainsAny(s, "=+/\r\n \t") {
		return nil, errors.New("not unpadded base64url")
	}
	return base64.RawURLEncoding.Strict().DecodeString(s)
}

func isString(v json.RawMessage) bool {
	v = bytes.TrimSpace(v)
	return len(v) > 0 && v[0] == '"'
}
