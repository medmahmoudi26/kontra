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
	"crypto/ecdsa"
	"encoding/json"

	"kontra.sh/kontra/internal/unimpl"
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
func LoadKeySet(path string) (*KeySet, error) {
	return nil, unimpl.Error("jws.LoadKeySet", "M3 WP-01")
}

// EphemeralKeySet mints one P-256 key for a loopback gate without signing_key (D166). Its cards verify only
// against this process's JWKS.
func EphemeralKeySet() (*KeySet, error) {
	return nil, unimpl.Error("jws.EphemeralKeySet", "M3 WP-01")
}

// KeyID is the active key's kid, its RFC 7638 thumbprint (base64url SHA-256).
func (k *KeySet) KeyID() (string, error) {
	return "", unimpl.Error("jws.KeySet.KeyID", "M3 WP-01")
}

// JWKS returns the JSON of the public JWK Set: every key, active first, each with kty EC, crv P-256, x, y, kid,
// use sig and alg ES256.
func (k *KeySet) JWKS() ([]byte, error) {
	return nil, unimpl.Error("jws.KeySet.JWKS", "M3 WP-01")
}

// Sign signs the JCS of payload (a card without signatures) with the active key: protected header {alg ES256,
// kid, typ JOSE, jku, kontra.upstream: up} (up nil leaves it out).
func (k *KeySet) Sign(payload []byte, jku string, up *Upstream) (Signature, error) {
	return Signature{}, unimpl.Error("jws.KeySet.Sign", "M3 WP-01")
}

// KeySetPublic is a parsed public JWK Set: the gate's (fetched from jku) or a seller's (seller_keys).
type KeySetPublic struct {
	keys map[string]*ecdsa.PublicKey // by kid
}

// ParseJWKS parses a JWK Set, keeping the EC P-256 keys (and refusing none at all). Keys of other types are
// skipped: a seller's set may hold keys the gate does not verify with.
func ParseJWKS(b []byte) (*KeySetPublic, error) {
	return nil, unimpl.Error("jws.ParseJWKS", "M3 WP-01")
}

// Thumbprints are the RFC 7638 thumbprints of the set's keys, sorted (pins record a seller's, ext-sealed §7).
func (s *KeySetPublic) Thumbprints() ([]string, error) {
	return nil, unimpl.Error("jws.KeySetPublic.Thumbprints", "M3 WP-01")
}

// Verify checks sig over the JCS of payload against keys, by the protected header's kid, and returns that
// header. A header naming another algorithm, a kid the set does not hold, or a crit member is refused.
func Verify(payload []byte, sig Signature, keys *KeySetPublic) (Header, error) {
	return Header{}, unimpl.Error("jws.Verify", "M3 WP-01")
}
