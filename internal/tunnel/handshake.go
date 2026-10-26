package tunnel

import (
	"fmt"
	"time"

	"kontra.sh/kontra/internal/unimpl"
)

// Message types (§4): M1 hello (node → gate), M2 challenge (gate → node), M3 proof (node → gate), M4
// welcome or error (gate → node). Each is one WebSocket text message, a JSON object of at most
// MaxHandshakeMessage bytes; unknown fields are ignored.
const (
	MsgHello     = "hello"
	MsgChallenge = "challenge"
	MsgProof     = "proof"
	MsgWelcome   = "welcome"
	MsgError     = "error"
)

// Channel binding types (§4 M2 "cb"): tls-exporter when the gate terminates TLS for this connection (the
// default deployment), none behind a TLS-terminating front proxy (`tunnel.channel_binding: none`).
const (
	CBTLSExporter = "tls-exporter"
	CBNone        = "none"
	// ExporterLabel is RFC 9266's label; the exporter is 32 bytes with an empty context.
	ExporterLabel = "EXPORTER-Channel-Binding"
)

// Service is one hosted service a node declares in hello (§4): its kind and name, the tenant coming
// from the key. The service a session serves comes from key_services, never from a request; the
// declared list only narrows it (§5, relay.md §6.1).
type Service struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// KeyRef names a key by its id and canonical age1… recipient (§4 rotate_from).
type KeyRef struct {
	KID       string `json:"kid"`
	Recipient string `json:"recipient"`
}

// Hello is M1 (§4). The gate hashes the exact bytes the node sent into the transcript (§4.1), so a
// decoder keeps them (Raw) and never re-encodes a hello to verify it.
type Hello struct {
	T          string    `json:"t"`
	V          int       `json:"v"`
	Origin     string    `json:"origin"`                // the origin the node dialed: scheme://host[:port], lowercase, default port elided
	KID        string    `json:"kid"`                   // KeyID(Recipient)
	Recipient  string    `json:"recipient"`             // the active key, age1…
	Name       string    `json:"name"`                  // 1–64 characters; --name, default the hostname
	Nonce      string    `json:"nonce"`                 // base64url, 32 random bytes
	Services   []Service `json:"services"`              // may be empty (a node that only hires)
	Enroll     []string  `json:"enroll,omitempty"`      // one-time enrollment tokens (§5)
	Auth       string    `json:"auth,omitempty"`        // an API key: the hiring principal, and with an enroll: scope an authority to enroll
	RotateFrom *KeyRef   `json:"rotate_from,omitempty"` // the key this one replaces (§12)
	Public     []string  `json:"public,omitempty"`      // anonymous sessions only (§6, M4)
	Agent      string    `json:"agent"`                 // kontra/<version> (<os>/<arch>)

	Raw []byte `json:"-"` // the message exactly as received or sent
}

// Challenge is M2 (§4).
type Challenge struct {
	T       string `json:"t"`
	V       int    `json:"v"`
	Session string `json:"session"`           // UUIDv7, the tunnels.session_id to be
	Nonce   string `json:"nonce"`             // base64url, 32 bytes
	CB      string `json:"cb"`                // CBTLSExporter or CBNone
	Enc     string `json:"enc"`               // base64url HPKE encapsulated key to the hello's recipient
	EncOld  string `json:"enc_old,omitempty"` // to rotate_from's recipient, only when M1 carried it
}

// Proof is M3 (§4).
type Proof struct {
	T        string `json:"t"`
	V        int    `json:"v"`
	Proof    string `json:"proof"`               // base64url, 32 bytes
	ProofOld string `json:"proof_old,omitempty"` // with rotate_from
}

// WelcomeService is one service the session will receive requests for, with its gate URL (§4 M4).
type WelcomeService struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	URL  string `json:"url"`
}

// AnonymousInfo is welcome.anonymous (§6, M4).
type AnonymousInfo struct {
	ExpiresAt time.Time `json:"expires_at"`
}

// Welcome is M4 on success (§4): the declared services the key is enrolled for, after this handshake's
// enrollments; a revoked binding is left out.
type Welcome struct {
	T         string           `json:"t"`
	V         int              `json:"v"`
	Session   string           `json:"session"`
	Replica   string           `json:"replica"`
	PingS     int              `json:"ping_s"`
	Tenant    string           `json:"tenant"`
	Services  []WelcomeService `json:"services"`
	Anonymous *AnonymousInfo   `json:"anonymous,omitempty"`
}

// ErrorMessage is M4 on refusal (§4); the gate then closes with CloseStatus(Code).
type ErrorMessage struct {
	T       string `json:"t"`
	V       int    `json:"v"`
	Code    Code   `json:"code"`
	Message string `json:"message"`
}

// Code is a handshake error code (§4).
type Code string

// The codes, in the order of §4's list: the gate closes with WebSocket status 4000 + the code's index.
const (
	CodeUnsupportedVersion   Code = "unsupported_version"
	CodeBadOrigin            Code = "bad_origin"
	CodeBadProof             Code = "bad_proof"
	CodeUnknownKey           Code = "unknown_key"
	CodeRevoked              Code = "revoked"
	CodeEnrollInvalid        Code = "enroll_invalid"
	CodeServiceDenied        Code = "service_denied"
	CodeKindMismatch         Code = "kind_mismatch"
	CodeAnonymousUnavailable Code = "anonymous_unavailable"
	CodeRateLimited          Code = "rate_limited"
	CodeDraining             Code = "draining"
	CodeAuthInvalid          Code = "auth_invalid"
)

// Codes is §4's list in order; never reordered.
var Codes = []Code{
	CodeUnsupportedVersion, CodeBadOrigin, CodeBadProof, CodeUnknownKey, CodeRevoked, CodeEnrollInvalid,
	CodeServiceDenied, CodeKindMismatch, CodeAnonymousUnavailable, CodeRateLimited, CodeDraining, CodeAuthInvalid,
}

// CloseStatusBase is the WebSocket status of Codes[0].
const CloseStatusBase = 4000

// CloseStatus is 4000 + the code's index in Codes, and false for a code not in the list.
func (c Code) CloseStatus() (int, bool) {
	panic(unimpl.Error("tunnel.Code.CloseStatus", "WP-01"))
}

// CodeOfStatus is the code a WebSocket close status 4000 + i names.
func CodeOfStatus(status int) (Code, bool) {
	panic(unimpl.Error("tunnel.CodeOfStatus", "WP-01"))
}

// Retryable reports whether the node redials after this code with backoff (§4, §13): every code but
// bad_origin, bad_proof, revoked, enroll_invalid, service_denied, kind_mismatch and auth_invalid, which
// need a change by the node's operator.
func (c Code) Retryable() bool {
	panic(unimpl.Error("tunnel.Code.Retryable", "WP-01"))
}

// HandshakeError is a refusal the gate sends as M4 error, or a node receives.
type HandshakeError struct {
	Code    Code
	Message string
}

func (e *HandshakeError) Error() string { return fmt.Sprintf("tunnel: %s: %s", e.Code, e.Message) }

// Refuse returns a HandshakeError with a formatted message.
func Refuse(code Code, format string, args ...any) *HandshakeError {
	return &HandshakeError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Encode marshals a handshake message (Hello, Challenge, Proof, Welcome, ErrorMessage), refusing one
// over MaxHandshakeMessage. A Hello's Services encode as [] when empty. A Hello's Raw is set to the bytes.
func Encode(msg any) ([]byte, error) {
	return nil, unimpl.Error("tunnel.Encode", "WP-01")
}

// ParseHello decodes M1 and checks its shape (§4 table): t, v (unsupported_version), the origin's form,
// kid = KeyID(recipient), name 1–64 characters, service kinds and names (relay.md §2.1's name pattern),
// nonce of 32 bytes. Raw keeps b. The origin's equality with the gate's public_url is the caller's check
// (bad_origin).
func ParseHello(b []byte) (*Hello, error) {
	return nil, unimpl.Error("tunnel.ParseHello", "WP-01")
}

// ParseChallenge decodes M2.
func ParseChallenge(b []byte) (*Challenge, error) {
	return nil, unimpl.Error("tunnel.ParseChallenge", "WP-01")
}

// ParseProof decodes M3.
func ParseProof(b []byte) (*Proof, error) {
	return nil, unimpl.Error("tunnel.ParseProof", "WP-01")
}

// ParseOutcome decodes M4: a Welcome, or the refusal as a *HandshakeError.
func ParseOutcome(b []byte) (*Welcome, error) {
	return nil, unimpl.Error("tunnel.ParseOutcome", "WP-01")
}
