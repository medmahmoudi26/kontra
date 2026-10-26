package tunnel

import (
	"crypto/ecdh"
	"crypto/hpke"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"strings"
)

// Proof of possession (§4.1, D30): the gate encapsulates to the node's X25519 key with HPKE (RFC 9180, Go
// 1.26 crypto/hpke: DHKEM(X25519, HKDF-SHA256), HKDF-SHA256, export-only) over the transcript's info; the
// node decapsulates and returns the exported secret. An X25519 key cannot sign; nothing here opens a
// sealed part, because the export is domain-separated from age's own key derivation.
const (
	// TranscriptLabel starts the info hash.
	TranscriptLabel = "kontra.sh/tunnel/v1"
	// LabelPoP is the export label of the proof; LabelPoPOld that of proof_old (§12 rotation).
	LabelPoP    = "kontra.sh/tunnel/pop/v1"
	LabelPoPOld = "kontra.sh/tunnel/pop-old/v1"
	// ProofSize is the exported length; NonceSize the random bytes of either nonce.
	ProofSize = 32
	NonceSize = 32
	// EncSize is the length of an HPKE encapsulated key of DHKEM(X25519): an X25519 public key.
	EncSize = 32
	// ChannelBindingSize is the length of the RFC 9266 tls-exporter value.
	ChannelBindingSize = 32
)

// Transcript is what the proof binds (§4.1):
//
//	info = SHA-256("kontra.sh/tunnel/v1" || 0x00 || lp(origin) || lp(cb) || lp(cb_value)
//	               || lp(SHA-256(M1 bytes as sent)) || lp(session) || lp(gate nonce))
//
// with lp(x) = uint16_be(len(x)) || x. The origin is the one the node dialed (the gate uses M1's, which
// it has checked against its public_url); cb_value is the 32-byte TLS exporter, empty when cb is none.
type Transcript struct {
	Origin    string
	CB        string
	CBValue   []byte
	Hello     []byte // M1 exactly as sent
	Session   string
	GateNonce []byte
}

// Validate checks the transcript is one §4.1 defines: cb is tls-exporter with a ChannelBindingSize
// cb_value, or none with an empty one; the gate nonce is NonceSize bytes; origin, hello and session are
// present; and every field fits lp's 16-bit length.
//
// A cb and a cb_value that do not match are refused, never hashed: a missing exporter (Dialed.CBValue is
// nil over plain http) is reported as missing on the side that lacks it, rather than surfacing as a
// bad_proof the node does not retry, and a side never falls back to an unbound transcript for one
// connection.
func (t Transcript) Validate() error {
	switch t.CB {
	case CBTLSExporter:
		if len(t.CBValue) != ChannelBindingSize {
			return fmt.Errorf("tunnel: transcript: cb %s with a cb_value of %d bytes, want %d", t.CB, len(t.CBValue), ChannelBindingSize)
		}
	case CBNone:
		if len(t.CBValue) != 0 {
			return fmt.Errorf("tunnel: transcript: cb %s with a cb_value of %d bytes, want none", t.CB, len(t.CBValue))
		}
	default:
		return fmt.Errorf("tunnel: transcript: cb %q, want %s or %s", t.CB, CBTLSExporter, CBNone)
	}
	if len(t.GateNonce) != NonceSize {
		return fmt.Errorf("tunnel: transcript: gate nonce of %d bytes, want %d", len(t.GateNonce), NonceSize)
	}
	for _, f := range []struct {
		name string
		n    int
	}{{"origin", len(t.Origin)}, {"hello", len(t.Hello)}, {"session", len(t.Session)}} {
		if f.n == 0 {
			return fmt.Errorf("tunnel: transcript: no %s", f.name)
		}
		if f.n > 0xffff {
			return fmt.Errorf("tunnel: transcript: %s of %d bytes, over lp's 65535", f.name, f.n)
		}
	}
	return nil
}

// Info is the transcript's info hash, after Validate: a transcript Validate refuses has no info. Both
// sides call it, the gate before Encapsulate and the node before Prove.
func (t Transcript) Info() ([]byte, error) {
	if err := t.Validate(); err != nil {
		return nil, err
	}
	h := sha256.New()
	h.Write([]byte(TranscriptLabel))
	h.Write([]byte{0})
	lp(h, []byte(t.Origin))
	lp(h, []byte(t.CB))
	lp(h, t.CBValue)
	hello := sha256.Sum256(t.Hello)
	lp(h, hello[:])
	lp(h, []byte(t.Session))
	lp(h, t.GateNonce)
	return h.Sum(nil), nil
}

// lp writes uint16_be(len(x)) || x. Validate has bounded every field Info frames.
func lp(h hash.Hash, x []byte) {
	var n [2]byte
	binary.BigEndian.PutUint16(n[:], uint16(len(x)))
	h.Write(n[:])
	h.Write(x)
}

// Encapsulate is the gate's half: HPKE SetupBaseS to recipient with info, and the export under label. It
// returns enc for the challenge and the proof the gate expects. A recipient that is not an X25519 key, or
// is a low-order point, is an error.
func Encapsulate(recipient *ecdh.PublicKey, info []byte, label string) (enc, want []byte, err error) {
	if recipient == nil || recipient.Curve() != ecdh.X25519() {
		return nil, nil, errors.New("tunnel: encapsulate: not an X25519 public key")
	}
	pk, err := hpke.NewDHKEMPublicKey(recipient)
	if err != nil {
		return nil, nil, fmt.Errorf("tunnel: encapsulate: %w", err)
	}
	enc, s, err := hpke.NewSender(pk, hpke.HKDFSHA256(), hpke.ExportOnly(), info)
	if err != nil {
		return nil, nil, fmt.Errorf("tunnel: encapsulate: %w", err)
	}
	want, err = s.Export(label, ProofSize)
	if err != nil {
		return nil, nil, fmt.Errorf("tunnel: encapsulate: %w", err)
	}
	return enc, want, nil
}

// Prove is the node's half: HPKE SetupBaseR with the identity's private key; a low-order enc is an error.
func Prove(enc []byte, identity *ecdh.PrivateKey, info []byte, label string) ([]byte, error) {
	if identity == nil || identity.Curve() != ecdh.X25519() {
		return nil, errors.New("tunnel: prove: not an X25519 private key")
	}
	if len(enc) != EncSize {
		return nil, fmt.Errorf("tunnel: prove: enc of %d bytes, want %d", len(enc), EncSize)
	}
	sk, err := hpke.NewDHKEMPrivateKey(identity)
	if err != nil {
		return nil, fmt.Errorf("tunnel: prove: %w", err)
	}
	r, err := hpke.NewRecipient(enc, sk, hpke.HKDFSHA256(), hpke.ExportOnly(), info)
	if err != nil {
		return nil, fmt.Errorf("tunnel: prove: %w", err)
	}
	proof, err := r.Export(label, ProofSize)
	if err != nil {
		return nil, fmt.Errorf("tunnel: prove: %w", err)
	}
	return proof, nil
}

// Accept compares a proof with the expected one in constant time. Both must be ProofSize bytes: an empty
// expectation never accepts anything.
func Accept(proof, want []byte) bool {
	if len(want) != ProofSize || len(proof) != ProofSize {
		return false
	}
	return subtle.ConstantTimeCompare(proof, want) == 1
}

// ChannelBinding is the RFC 9266 tls-exporter value of a TLS connection: ExportKeyingMaterial(
// ExporterLabel, nil, 32). The gate reads it from the upgrade request's TLS state, the node from the
// dial's response (§4.1); both are equal only on one TLS leg.
//
// It is an error, never an empty value, when there is no completed TLS handshake or the connection
// cannot export (TLS 1.2 without Extended Master Secret, renegotiation enabled): a gate configured for
// tls-exporter must then refuse the connection rather than fall back to none.
func ChannelBinding(cs *tls.ConnectionState) (cb []byte, err error) {
	if cs == nil || !cs.HandshakeComplete {
		return nil, errors.New("tunnel: channel binding: no completed TLS handshake")
	}
	defer func() {
		// A ConnectionState not obtained from a tls.Conn has no exporter and panics when asked.
		if r := recover(); r != nil {
			cb, err = nil, errors.New("tunnel: channel binding: the connection state has no exporter")
		}
	}()
	v, err := cs.ExportKeyingMaterial(ExporterLabel, nil, ChannelBindingSize)
	if err != nil {
		return nil, fmt.Errorf("tunnel: channel binding: %w", err)
	}
	if len(v) != ChannelBindingSize {
		return nil, fmt.Errorf("tunnel: channel binding: exporter of %d bytes", len(v))
	}
	return v, nil
}

// NewNonce returns NonceSize random bytes and their base64url form.
func NewNonce() (raw []byte, encoded string, err error) {
	raw = make([]byte, NonceSize)
	if _, err := rand.Read(raw); err != nil {
		return nil, "", fmt.Errorf("tunnel: nonce: %w", err)
	}
	return raw, EncodeBase64URL(raw), nil
}

// EncodeBase64URL is the base64url form, without padding, of nonces, enc, enc_old, proof and proof_old.
func EncodeBase64URL(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// DecodeBase64URL decodes a base64url field strictly; padding is tolerated, nothing else (encoding/base64
// would skip line breaks).
func DecodeBase64URL(s string) ([]byte, error) {
	if strings.ContainsAny(s, "\r\n") {
		return nil, errors.New("base64url: line break")
	}
	enc := base64.RawURLEncoding.Strict()
	if strings.HasSuffix(s, "=") {
		enc = base64.URLEncoding.Strict()
	}
	return enc.DecodeString(s)
}

// decodeSized decodes a base64url field of exactly n bytes.
func decodeSized(field, s string, n int) ([]byte, error) {
	b, err := DecodeBase64URL(s)
	if err != nil {
		return nil, fmt.Errorf("%s: not base64url", field)
	}
	if len(b) != n {
		return nil, fmt.Errorf("%s: %d bytes, want %d", field, len(b), n)
	}
	return b, nil
}
