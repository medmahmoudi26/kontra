package tunnel

import (
	"crypto/ecdh"
	"crypto/tls"

	"kontra.sh/kontra/internal/unimpl"
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

// Info is the transcript's info hash.
func (t Transcript) Info() []byte {
	panic(unimpl.Error("tunnel.Transcript.Info", "WP-01"))
}

// Encapsulate is the gate's half: HPKE SetupBaseS to recipient with info, and the export under label. It
// returns enc for the challenge and the proof the gate expects.
func Encapsulate(recipient *ecdh.PublicKey, info []byte, label string) (enc, want []byte, err error) {
	return nil, nil, unimpl.Error("tunnel.Encapsulate", "WP-01")
}

// Prove is the node's half: HPKE SetupBaseR with the identity's private key; a low-order enc is an error.
func Prove(enc []byte, identity *ecdh.PrivateKey, info []byte, label string) ([]byte, error) {
	return nil, unimpl.Error("tunnel.Prove", "WP-01")
}

// Accept compares a proof with the expected one in constant time.
func Accept(proof, want []byte) bool {
	panic(unimpl.Error("tunnel.Accept", "WP-01"))
}

// ChannelBinding is the RFC 9266 tls-exporter value of a TLS connection: ExportKeyingMaterial(
// ExporterLabel, nil, 32). The gate reads it from the upgrade request's TLS state, the node from the
// dial's response (§4.1); both are equal only on one TLS leg.
func ChannelBinding(cs *tls.ConnectionState) ([]byte, error) {
	return nil, unimpl.Error("tunnel.ChannelBinding", "WP-01")
}

// NewNonce returns NonceSize random bytes and their base64url form.
func NewNonce() (raw []byte, encoded string, err error) {
	return nil, "", unimpl.Error("tunnel.NewNonce", "WP-01")
}
