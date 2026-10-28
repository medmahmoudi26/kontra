package tunnel

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
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
	Anonymous  bool      `json:"anonymous,omitempty"`   // true only: the node asks for an anonymous session (§4, §6, DM4-18)
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
	for i, x := range Codes {
		if x == c {
			return CloseStatusBase + i, true
		}
	}
	return 0, false
}

// CodeOfStatus is the code a WebSocket close status 4000 + i names.
func CodeOfStatus(status int) (Code, bool) {
	i := status - CloseStatusBase
	if i < 0 || i >= len(Codes) {
		return "", false
	}
	return Codes[i], true
}

// Retryable reports whether the node redials after this code with backoff (§4, §13): every code but
// bad_origin, bad_proof, revoked, enroll_invalid, service_denied, kind_mismatch and auth_invalid, which
// need a change by the node's operator. A code this version does not know is retried, as §4 says of
// "the others".
func (c Code) Retryable() bool {
	switch c {
	case CodeBadOrigin, CodeBadProof, CodeRevoked, CodeEnrollInvalid, CodeServiceDenied, CodeKindMismatch,
		CodeAuthInvalid:
		return false
	}
	return true
}

// HandshakeError is a refusal the gate sends as M4 error, or a node receives.
type HandshakeError struct {
	Code    Code
	Message string
}

// Error is the refusal for a log or the operator's terminal: the code and the message are the gate's
// words, so they print through Printable (M2 review, security-5).
func (e *HandshakeError) Error() string {
	return fmt.Sprintf("tunnel: %s: %s", Printable(string(e.Code)), Printable(e.Message))
}

// Printable is s with every rune that is not graphic written as its Go escape (\x1b, \a, \n, \u202e), and
// every byte that is not UTF-8 as \xNN: text a gate chose (a refusal's message, a close reason), printed on
// the operator's terminal without the escape sequences it could carry (an OSC 52 clipboard write, a screen
// clear, a bidi override; M2 review, security-5). Graphic text, spaces and quotes included, is unchanged.
func Printable(s string) string {
	if utf8.ValidString(s) && !strings.ContainsFunc(s, func(r rune) bool { return !unicode.IsGraphic(r) }) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		r, w := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && w == 1:
			fmt.Fprintf(&b, `\x%02x`, s[i])
		case unicode.IsGraphic(r):
			b.WriteRune(r)
		default:
			q := strconv.QuoteRuneToGraphic(r)
			b.WriteString(q[1 : len(q)-1])
		}
		i += w
	}
	return b.String()
}

// Refuse returns a HandshakeError with a formatted message.
func Refuse(code Code, format string, args ...any) *HandshakeError {
	return &HandshakeError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// MaxNodeName is the most characters hello.name has (§4; tunnel_keys.node_name's CHECK).
const MaxNodeName = 64

// serviceNameRE is relay.md §2.1's service name (config.NameRE; services.name's CHECK).
var serviceNameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// Encode marshals a handshake message (Hello, Challenge, Proof, Welcome, ErrorMessage), refusing one
// over MaxHandshakeMessage. A Hello's Services encode as [] when empty. A Hello's Raw is set to the bytes.
//
// An empty t is set to the message's type and a zero v to Version, on the caller's message when it is a
// pointer; a t naming another type is an error. A Hello must be passed as *Hello, so that Raw, the bytes
// the transcript hashes (§4.1), is set. A Welcome's Services also encode as [] when empty.
func Encode(msg any) ([]byte, error) {
	var (
		b   []byte
		err error
	)
	switch m := msg.(type) {
	case *Hello:
		if m == nil {
			return nil, errors.New("tunnel: encode: nil hello")
		}
		if err := setTV(&m.T, &m.V, MsgHello); err != nil {
			return nil, err
		}
		c := *m
		if c.Services == nil {
			c.Services = []Service{}
		}
		if b, err = json.Marshal(&c); err != nil {
			return nil, fmt.Errorf("tunnel: encode hello: %w", err)
		}
		if err := checkSize(b); err != nil {
			return nil, err
		}
		m.Raw = b
		return b, nil
	case Hello:
		return nil, errors.New("tunnel: encode: pass a *Hello, so that its Raw is set")
	case *Challenge:
		if m == nil {
			return nil, errors.New("tunnel: encode: nil challenge")
		}
		if err := setTV(&m.T, &m.V, MsgChallenge); err != nil {
			return nil, err
		}
		b, err = json.Marshal(m)
	case Challenge:
		return Encode(&m)
	case *Proof:
		if m == nil {
			return nil, errors.New("tunnel: encode: nil proof")
		}
		if err := setTV(&m.T, &m.V, MsgProof); err != nil {
			return nil, err
		}
		b, err = json.Marshal(m)
	case Proof:
		return Encode(&m)
	case *Welcome:
		if m == nil {
			return nil, errors.New("tunnel: encode: nil welcome")
		}
		if err := setTV(&m.T, &m.V, MsgWelcome); err != nil {
			return nil, err
		}
		c := *m
		if c.Services == nil {
			c.Services = []WelcomeService{}
		}
		b, err = json.Marshal(&c)
	case Welcome:
		return Encode(&m)
	case *ErrorMessage:
		if m == nil {
			return nil, errors.New("tunnel: encode: nil error message")
		}
		if err := setTV(&m.T, &m.V, MsgError); err != nil {
			return nil, err
		}
		b, err = json.Marshal(m)
	case ErrorMessage:
		return Encode(&m)
	default:
		return nil, fmt.Errorf("tunnel: encode: %T is not a handshake message", msg)
	}
	if err != nil {
		return nil, fmt.Errorf("tunnel: encode: %w", err)
	}
	if err := checkSize(b); err != nil {
		return nil, err
	}
	return b, nil
}

// setTV sets an empty t to want and a zero v to Version, and refuses a t naming another message.
func setTV(t *string, v *int, want string) error {
	switch *t {
	case "":
		*t = want
	case want:
	default:
		return fmt.Errorf("tunnel: encode: t %q on a %s message", *t, want)
	}
	if *v == 0 {
		*v = Version
	}
	return nil
}

func checkSize(b []byte) error {
	if len(b) > MaxHandshakeMessage {
		return fmt.Errorf("tunnel: handshake message of %d bytes, over %d", len(b), MaxHandshakeMessage)
	}
	return nil
}

// envelope is what every handshake message has, read before the rest so that a message of another
// version is refused for its version and not for a field whose shape that version changed.
type envelope struct {
	T string `json:"t"`
	V int    `json:"v"`
}

// open checks a message's size, that it is one JSON object, its t and its v.
func open(b []byte, want string) error {
	if err := checkSize(b); err != nil {
		return err
	}
	var e envelope
	if err := json.Unmarshal(b, &e); err != nil {
		return fmt.Errorf("not a %s message: %v", want, err)
	}
	if e.T != want {
		return fmt.Errorf("t %q, want %q", e.T, want)
	}
	if e.V != Version {
		return errVersion{e.V}
	}
	return nil
}

type errVersion struct{ v int }

func (e errVersion) Error() string { return fmt.Sprintf("v %d, this side speaks %d", e.v, Version) }

// ParseHello decodes M1 and checks its shape (§4 table): t, v (unsupported_version), the origin's form,
// kid = KeyID(recipient), name 1–64 characters, service kinds and names (relay.md §2.1's name pattern),
// nonce of 32 bytes. Raw keeps b. The origin's equality with the gate's public_url is the caller's check
// (bad_origin).
//
// Every refusal is a *HandshakeError the gate sends as M4.
//
// SPEC-CONFLICT: §4's closed list of codes has none for a hello of the wrong shape, and the gate must
// close with one of them. Each refusal therefore takes the code of what it is about, and a shape error
// that is about no listed subject is unsupported_version ("not a v1 hello this gate reads"), which the
// node retries with backoff. `kontra connect` must therefore check --name (its default, the hostname, can
// be longer than 64 characters) and --serve before it dials, so that a node of this version never sends
// one. The owner may prefer a dedicated non-retryable code (a §4 amendment). The codes:
//   - unsupported_version: over MaxHandshakeMessage, not a JSON object, t not hello, v not Version, a field
//     of the wrong JSON type, a name outside 1–64 characters, a nonce that is not 32 base64url bytes, or a
//     NUL character in the name or the agent (Postgres text cannot hold it): not a v1 hello this gate reads;
//   - bad_origin: an origin that is not scheme://host[:port], lowercase, default port elided;
//   - bad_proof: a recipient that is not a canonical X25519 age1… recipient, or a kid that is not its key
//     id, for the active key or rotate_from's;
//   - service_denied: a declared service whose kind is not a2a or mcp, whose name does not match the
//     pattern, or whose name is declared twice.
func ParseHello(b []byte) (*Hello, error) {
	if err := open(b, MsgHello); err != nil {
		return nil, Refuse(CodeUnsupportedVersion, "hello: %s", clip(err.Error()))
	}
	var h Hello
	if err := json.Unmarshal(b, &h); err != nil {
		return nil, Refuse(CodeUnsupportedVersion, "hello: %s", clip(err.Error()))
	}
	if err := checkHello(&h); err != nil {
		return nil, err
	}
	h.Raw = append([]byte(nil), b...)
	return &h, nil
}

func checkHello(h *Hello) *HandshakeError {
	if u, err := url.Parse(h.Origin); err != nil {
		return Refuse(CodeBadOrigin, "hello: origin %q is not a URL", clip(h.Origin))
	} else if c, err := CanonicalOrigin(u); err != nil || c != h.Origin {
		return Refuse(CodeBadOrigin, "hello: origin %q is not scheme://host[:port], lowercase, default port elided", clip(h.Origin))
	}
	if err := checkKey("", h.KID, h.Recipient); err != nil {
		return err
	}
	if n := utf8.RuneCountInString(h.Name); n < 1 || n > MaxNodeName {
		return Refuse(CodeUnsupportedVersion, "hello: name of %d characters, want 1 to %d", n, MaxNodeName)
	}
	if strings.ContainsRune(h.Name, 0) || strings.ContainsRune(h.Agent, 0) {
		return Refuse(CodeUnsupportedVersion, "hello: a NUL character in name or agent")
	}
	if _, err := decodeSized("nonce", h.Nonce, NonceSize); err != nil {
		return Refuse(CodeUnsupportedVersion, "hello: %v", err)
	}
	seen := make(map[string]bool, len(h.Services))
	for _, s := range h.Services {
		if s.Kind != KindA2A && s.Kind != KindMCP {
			return Refuse(CodeServiceDenied, "hello: service %q: kind %q, want a2a or mcp", clip(s.Name), clip(s.Kind))
		}
		if !serviceNameRE.MatchString(s.Name) {
			return Refuse(CodeServiceDenied, "hello: service name %q does not match %s", clip(s.Name), serviceNameRE)
		}
		if seen[s.Name] {
			return Refuse(CodeServiceDenied, "hello: service %q declared twice", s.Name)
		}
		seen[s.Name] = true
	}
	if r := h.RotateFrom; r != nil {
		if err := checkKey("rotate_from ", r.KID, r.Recipient); err != nil {
			return err
		}
	}
	return nil
}

// clip shortens a value a refusal quotes back, so that the error message stays far below
// MaxHandshakeMessage whatever the hello carried.
func clip(s string) string {
	const max = 200
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// checkKey checks a kid and recipient pair (§2.2): bad_proof, since the proof is made for that key.
func checkKey(what, kid, recipient string) *HandshakeError {
	if _, err := ParseRecipient(recipient); err != nil {
		return Refuse(CodeBadProof, "hello: %s%s", what, clip(err.Error()))
	}
	if kid != KeyID(recipient) {
		return Refuse(CodeBadProof, "hello: %skid %q is not the key id of its recipient", what, clip(kid))
	}
	return nil
}

// ParseChallenge decodes M2: t, v, a session id in UUID form, a 32-byte nonce, cb tls-exporter or none,
// and enc (and enc_old when present) of EncSize bytes. A refusal is the node's own error.
func ParseChallenge(b []byte) (*Challenge, error) {
	if err := open(b, MsgChallenge); err != nil {
		return nil, fmt.Errorf("tunnel: challenge: %w", err)
	}
	var c Challenge
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("tunnel: challenge: %w", err)
	}
	if err := checkSession(c.Session); err != nil {
		return nil, fmt.Errorf("tunnel: challenge: %w", err)
	}
	if c.CB != CBTLSExporter && c.CB != CBNone {
		return nil, fmt.Errorf("tunnel: challenge: cb %q, want %s or %s", c.CB, CBTLSExporter, CBNone)
	}
	if _, err := decodeSized("nonce", c.Nonce, NonceSize); err != nil {
		return nil, fmt.Errorf("tunnel: challenge: %w", err)
	}
	if _, err := decodeSized("enc", c.Enc, EncSize); err != nil {
		return nil, fmt.Errorf("tunnel: challenge: %w", err)
	}
	if c.EncOld != "" {
		if _, err := decodeSized("enc_old", c.EncOld, EncSize); err != nil {
			return nil, fmt.Errorf("tunnel: challenge: %w", err)
		}
	}
	return &c, nil
}

// ParseProof decodes M3. A refusal is a *HandshakeError: unsupported_version for a message that is not a
// v1 proof, bad_proof for a proof or proof_old that is not ProofSize base64url bytes.
func ParseProof(b []byte) (*Proof, error) {
	if err := open(b, MsgProof); err != nil {
		return nil, Refuse(CodeUnsupportedVersion, "proof: %s", clip(err.Error()))
	}
	var p Proof
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, Refuse(CodeUnsupportedVersion, "proof: %s", clip(err.Error()))
	}
	if _, err := decodeSized("proof", p.Proof, ProofSize); err != nil {
		return nil, Refuse(CodeBadProof, "%v", err)
	}
	if p.ProofOld != "" {
		if _, err := decodeSized("proof_old", p.ProofOld, ProofSize); err != nil {
			return nil, Refuse(CodeBadProof, "%v", err)
		}
	}
	return &p, nil
}

// ParseOutcome decodes M4: a Welcome, or the refusal as a *HandshakeError. A welcome must carry a session
// id in UUID form and a positive ping_s; anything else that is not a v1 welcome or error is the node's own
// error.
func ParseOutcome(b []byte) (*Welcome, error) {
	if err := checkSize(b); err != nil {
		return nil, err
	}
	var e envelope
	if err := json.Unmarshal(b, &e); err != nil {
		return nil, fmt.Errorf("tunnel: outcome: %w", err)
	}
	switch e.T {
	case MsgWelcome:
		if err := open(b, MsgWelcome); err != nil {
			return nil, fmt.Errorf("tunnel: welcome: %w", err)
		}
		var w Welcome
		if err := json.Unmarshal(b, &w); err != nil {
			return nil, fmt.Errorf("tunnel: welcome: %w", err)
		}
		if err := checkSession(w.Session); err != nil {
			return nil, fmt.Errorf("tunnel: welcome: %w", err)
		}
		if w.PingS < 1 {
			return nil, fmt.Errorf("tunnel: welcome: ping_s %d", w.PingS)
		}
		if err := checkWelcome(&w); err != nil {
			return nil, fmt.Errorf("tunnel: welcome: %w", err)
		}
		return &w, nil
	case MsgError:
		if err := open(b, MsgError); err != nil {
			return nil, fmt.Errorf("tunnel: error message: %w", err)
		}
		var m ErrorMessage
		if err := json.Unmarshal(b, &m); err != nil {
			return nil, fmt.Errorf("tunnel: error message: %w", err)
		}
		if m.Code == "" {
			return nil, errors.New("tunnel: error message without a code")
		}
		return nil, &HandshakeError{Code: m.Code, Message: m.Message}
	default:
		return nil, fmt.Errorf("tunnel: outcome: t %q, want %s or %s", e.T, MsgWelcome, MsgError)
	}
}

// maxWelcomeURL bounds a welcome service's URL.
const maxWelcomeURL = 2048

// checkWelcome accepts the welcome's text of the shape the gate writes, which the node prints on the
// operator's terminal (M2 review, security-5): a tenant that is empty or a tenant name (relay.md §2.1), a
// replica id of graphic characters, and services of kind a2a or mcp, a service name, and an http(s) URL with
// a host, in printable ASCII ({public_url}/agents/[{tenant}/]{name}).
func checkWelcome(w *Welcome) error {
	if w.Tenant != "" && !serviceNameRE.MatchString(w.Tenant) {
		return fmt.Errorf("tenant %s is not a tenant name", strconv.QuoteToASCII(w.Tenant))
	}
	if Printable(w.Replica) != w.Replica || len(w.Replica) > 256 {
		return fmt.Errorf("replica %s", strconv.QuoteToASCII(w.Replica))
	}
	for _, sv := range w.Services {
		switch {
		case sv.Kind != KindA2A && sv.Kind != KindMCP:
			return fmt.Errorf("service kind %s", strconv.QuoteToASCII(sv.Kind))
		case !serviceNameRE.MatchString(sv.Name):
			return fmt.Errorf("service name %s", strconv.QuoteToASCII(sv.Name))
		}
		if err := checkWelcomeURL(sv.URL); err != nil {
			return fmt.Errorf("service %s: %w", sv.Name, err)
		}
	}
	return nil
}

// checkWelcomeURL accepts an absolute http(s) URL with a host, written in printable ASCII.
func checkWelcomeURL(s string) error {
	bad := len(s) == 0 || len(s) > maxWelcomeURL || strings.ContainsFunc(s, func(r rune) bool { return r <= ' ' || r > '~' })
	if !bad {
		u, err := url.Parse(s)
		bad = err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil
	}
	if bad {
		return fmt.Errorf("url %s is not an http(s) URL in printable ASCII", strconv.QuoteToASCII(s))
	}
	return nil
}

// checkSession accepts a session id in the canonical UUID form the gate writes (tunnels.session_id).
func checkSession(s string) error {
	u, err := uuid.Parse(s)
	if err != nil || u.String() != s {
		return fmt.Errorf("session %q is not a UUID", s)
	}
	return nil
}
