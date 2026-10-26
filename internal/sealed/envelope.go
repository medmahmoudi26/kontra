// Package sealed is envelope v1 of the sealed extension (spec/ext-sealed.md §3): the header, its canonical
// form, age encryption to the recipients, opening, the reply HMAC under the job's reply key, the refusal
// reasons a hosting node answers with (§6), and the shapes a sealed part takes in A2A 1.0 and in MCP (parts.go).
//
// It is a leaf: it knows bytes, keys and JSON shapes, never a task, a session or a store. The rules each party
// applies with it (the hiring node's checks, the hosting node's durable job state, the gate's refusals) live in
// internal/sealing and internal/relay. The gate imports only the constants and ParsePart: it never decrypts,
// re-encrypts or alters a sealed part, and holds no identity that opens one.
//
// The ciphertext is a binary age v1 file (filippo.io/age v1.3.2, X25519 stanzas, spec/tunnel.md §0) whose
// plaintext is JCS(header) ‖ 0x00 ‖ body. age authenticates chunk by chunk, so Open reads the whole file and
// authenticates its last chunk before returning anything (a blob part is opened the same way after its stream
// ends, spec/tunnel.md §8.3).
//
// Work package: M3 WP-01.
package sealed

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"filippo.io/age"

	"kontra.sh/kontra/internal/jcs"
	"kontra.sh/kontra/internal/strictjson"
	"kontra.sh/kontra/internal/tunnel"
)

// Identifiers of the extension (spec/ext-sealed.md §1, §3; D27).
const (
	URI       = "https://kontra.sh/ext/sealed/v1"
	MediaType = "application/vnd.kontra.sealed+age;v=1"
	Version   = 1
	// MetaKey is the MCP `_meta` key of a sealed call or answer, and the capability key: under
	// capabilities.experimental on 2025-11-25 (sealed MCP is 2025-11-25 only, D162).
	MetaKey = "sh.kontra/sealed"
	// MetadataKey and MetadataReplyTo are the A2A metadata keys callers may write (spec/relay.md §2.5).
	MetadataKey     = "kontra.sealed"
	MetadataReplyTo = "kontra.sealed.replyTo"
	// ResourceURI is the embedded resource that carries a sealed MCP result.
	ResourceURI = "kontra:sealed"
	// RKSize is the reply key's length in bytes (base64url in the header).
	RKSize = 32
	// MaxClockSkew is how far in the future a request's iat may be (§5 hosting rule 1).
	MaxClockSkew = 300 // seconds
)

// maxCiphertext bounds what Open reads: the largest object a blob stream carries (spec/tunnel.md §14).
const maxCiphertext = 64 << 20

// Dir is a header's direction.
type Dir string

// Directions.
const (
	DirRequest Dir = "req"
	DirReply   Dir = "res"
)

// Kind is which field of the original part the body carries (A2A 1.0 Part: text, raw, url or data; MCP is
// always data).
type Kind string

// Kinds.
const (
	KindText Kind = "text"
	KindRaw  Kind = "raw"
	KindURL  Kind = "url"
	KindData Kind = "data"
)

// Header is the envelope header (spec/ext-sealed.md §3's table). Its JCS form is what is sealed and compared:
// optional members are absent, not empty, and part and parts are always present. A2A requests carry iat,
// contextId, messageId, replyTo and rk (and taskId on a follow-up); replies carry taskId, auth and the reply's
// ids; MCP carries session (the hex SHA-256 of the gate Mcp-Session-Id), tool and messageId: the client's
// JSON-RPC id as a string for a tools/call and its result, and for an input request (an upstream
// elicitation/create or sampling/createMessage, sealed like a reply) and the hiring node's answer to it (sealed
// like a request), the upstream server's JSON-RPC id of that request as a string (D175).
type Header struct {
	V          int    `json:"v"`
	Dir        Dir    `json:"dir"`
	IAT        int64  `json:"iat,omitempty"`
	TaskID     string `json:"taskId,omitempty"`
	ContextID  string `json:"contextId,omitempty"`
	Session    string `json:"session,omitempty"`
	Tool       string `json:"tool,omitempty"`
	MessageID  string `json:"messageId,omitempty"`
	ArtifactID string `json:"artifactId,omitempty"`
	// Chunk is, for an artifact update, the 0-based count of updates for its artifactId the hosting node
	// sealed; 0 for a whole artifact; absent on messages.
	Chunk     *int     `json:"chunk,omitempty"`
	Part      int      `json:"part"`
	Parts     int      `json:"parts"`
	Kind      Kind     `json:"kind"`
	MediaType string   `json:"mediaType"`
	Kids      []string `json:"kids"`
	ReplyTo   string   `json:"replyTo,omitempty"`
	RK        string   `json:"rk,omitempty"`
	Auth      string   `json:"auth,omitempty"`
}

// headerMembers are the members Header knows, by their exact names.
var headerMembers = map[string]bool{
	"v": true, "dir": true, "iat": true, "taskId": true, "contextId": true, "session": true, "tool": true,
	"messageId": true, "artifactId": true, "chunk": true, "part": true, "parts": true, "kind": true,
	"mediaType": true, "kids": true, "replyTo": true, "rk": true, "auth": true,
}

// Canonical returns the header's JCS form, ASCII only (spec §3). It refuses a header no party may send: an
// unknown version or direction, a missing kids, or a member that is not ASCII.
//
// It also refuses what the §3 table rules out whatever the party: an unknown kind; part outside 0..parts-1;
// a negative iat or chunk; kids empty, unsorted or repeated (they are "sorted" key ids, one per recipient);
// iat, replyTo or rk on a reply and auth on a request; an rk that is not RKSize bytes of base64url. Which ids a
// header must carry for its exchange is each party's rule (internal/sealing).
func (h Header) Canonical() ([]byte, error) {
	if err := h.validate(); err != nil {
		return nil, err
	}
	b, err := json.Marshal(h)
	if err != nil {
		return nil, fmt.Errorf("sealed: header: %w", err)
	}
	out, err := jcs.Transform(b)
	if err != nil {
		return nil, fmt.Errorf("sealed: header: %w", err)
	}
	return out, nil
}

func (h Header) validate() error {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("sealed: header: %s", fmt.Sprintf(format, a...))
	}
	if h.V != Version {
		return bad("version %d is not %d", h.V, Version)
	}
	switch h.Dir {
	case DirRequest:
		if h.Auth != "" {
			return bad("a request carries no auth")
		}
	case DirReply:
		if h.IAT != 0 || h.ReplyTo != "" || h.RK != "" {
			return bad("a reply carries no iat, replyTo or rk")
		}
	default:
		return bad("direction %s is neither req nor res", strictjson.Quote(string(h.Dir)))
	}
	switch h.Kind {
	case KindText, KindRaw, KindURL, KindData:
	default:
		return bad("kind %s is not text, raw, url or data", strictjson.Quote(string(h.Kind)))
	}
	if h.IAT < 0 {
		return bad("iat is negative")
	}
	if h.Chunk != nil && *h.Chunk < 0 {
		return bad("chunk is negative")
	}
	if h.Parts < 1 || h.Part < 0 || h.Part >= h.Parts {
		return bad("part %d is not in 0..parts-1 (parts %d)", h.Part, h.Parts)
	}
	if len(h.Kids) == 0 {
		return bad("kids is missing")
	}
	for i, k := range h.Kids {
		if k == "" {
			return bad("an empty key id in kids")
		}
		if i > 0 && h.Kids[i-1] >= k {
			return bad("kids are not sorted and unique")
		}
	}
	if h.RK != "" {
		if _, err := DecodeRK(h.RK); err != nil {
			return bad("%v", err)
		}
	}
	for _, s := range append([]string{string(h.Dir), h.TaskID, h.ContextID, h.Session, h.Tool, h.MessageID,
		h.ArtifactID, string(h.Kind), h.MediaType, h.ReplyTo, h.RK, h.Auth}, h.Kids...) {
		for i := 0; i < len(s); i++ {
			if s[i] >= 0x80 {
				return bad("a member is not ASCII")
			}
		}
	}
	return nil
}

// ParseHeader decodes a header and refuses it (RejectBadHeader) unless re-serializing it gives the same bytes:
// a header that is not canonical, has a duplicate key or a member Header does not know is refused (spec §3).
//
// Every refusal is a *RejectError with RejectBadHeader: bytes that are not ASCII, not one JSON object, a repeated
// or case-variant member, a member Header does not know (exact names only), a member of the wrong type, anything
// Canonical refuses, and any difference between b and Canonical's bytes (white space, member order, number form,
// an escape JCS would not write, an absent-not-empty member written empty).
func ParseHeader(b []byte) (Header, error) {
	for _, c := range b {
		if c >= 0x80 {
			return Header{}, reject(RejectBadHeader, "header is not ASCII")
		}
	}
	m, err := strictjson.Members(b)
	if err != nil {
		return Header{}, reject(RejectBadHeader, "header: "+err.Error())
	}
	for k := range m {
		if !headerMembers[k] {
			return Header{}, reject(RejectBadHeader, "header member "+strictjson.Quote(k)+" is unknown")
		}
	}
	// Every key is now exactly one of Header's names, once, so encoding/json's case-insensitive matching has
	// nothing to choose between.
	var h Header
	if err := json.Unmarshal(b, &h); err != nil {
		return Header{}, reject(RejectBadHeader, "header: "+err.Error())
	}
	c, err := h.Canonical()
	if err != nil {
		return Header{}, reject(RejectBadHeader, err.Error())
	}
	if !bytes.Equal(c, b) {
		return Header{}, reject(RejectBadHeader, "header is not canonical")
	}
	return h, nil
}

// Seal encrypts JCS(h) ‖ 0x00 ‖ body to every recipient, as one binary (not armored) age file.
//
// Recipients must be X25519 (age1…) ones, at least one, and h.Kids must be exactly their key ids
// (spec/tunnel.md §2.2), sorted: a request's kids are its recipients', a reply's the per-job key's it is sealed to.
func Seal(recipients []age.Recipient, h Header, body []byte) ([]byte, error) {
	if len(recipients) == 0 {
		return nil, errors.New("sealed: no recipient")
	}
	var kids []string
	for _, r := range recipients {
		x, ok := r.(*age.X25519Recipient)
		if !ok {
			return nil, fmt.Errorf("sealed: recipient %T is not an X25519 recipient (envelope v1)", r)
		}
		kid := tunnel.KeyID(x.String())
		if !slices.Contains(kids, kid) {
			kids = append(kids, kid)
		}
	}
	slices.Sort(kids)
	if !slices.Equal(kids, h.Kids) {
		return nil, fmt.Errorf("sealed: header kids %v are not the recipients' key ids %v", h.Kids, kids)
	}
	hdr, err := h.Canonical()
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	w, err := age.Encrypt(&out, recipients...)
	if err != nil {
		return nil, fmt.Errorf("sealed: %w", err)
	}
	for _, b := range [][]byte{hdr, {0}, body} {
		if _, err := w.Write(b); err != nil {
			return nil, fmt.Errorf("sealed: %w", err)
		}
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("sealed: %w", err)
	}
	return out.Bytes(), nil
}

// Open decrypts a whole age file with any of identities, splits header and body at the first 0x00 and parses
// the header. A file none of identities opens is RejectUndecryptable; a header ParseHeader refuses is
// RejectBadHeader. Open checks nothing about ids: that is each party's rule (internal/sealing).
//
// age authenticates chunk by chunk; Open reads to the end of the file, so its last chunk (and nothing after it)
// is authenticated before anything is returned. A truncated, extended or altered file, an armored one and one
// over 64 MiB are RejectUndecryptable too; a plaintext without 0x00 is RejectBadHeader.
func Open(identities []age.Identity, ciphertext []byte) (Header, []byte, error) {
	if len(identities) == 0 {
		return Header{}, nil, reject(RejectUndecryptable, "no identity")
	}
	if len(ciphertext) > maxCiphertext {
		return Header{}, nil, reject(RejectUndecryptable, "ciphertext over 64 MiB")
	}
	r, err := age.Decrypt(bytes.NewReader(ciphertext), identities...)
	if err != nil {
		return Header{}, nil, reject(RejectUndecryptable, err.Error())
	}
	plain, err := io.ReadAll(r)
	if err != nil {
		return Header{}, nil, reject(RejectUndecryptable, err.Error())
	}
	i := bytes.IndexByte(plain, 0)
	if i < 0 {
		return Header{}, nil, reject(RejectBadHeader, "no 0x00 after the header")
	}
	h, err := ParseHeader(plain[:i])
	if err != nil {
		return Header{}, nil, err
	}
	return h, plain[i+1:], nil
}

// Auth is a reply's auth member: base64url(HMAC-SHA256(rk, JCS(h without auth) ‖ 0x00 ‖ body)).
//
// base64url is without padding, as everywhere in the header. h.Auth is ignored (cleared before the JCS).
func Auth(rk []byte, h Header, body []byte) (string, error) {
	mac, err := authMAC(rk, h, body)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(mac), nil
}

func authMAC(rk []byte, h Header, body []byte) ([]byte, error) {
	if len(rk) != RKSize {
		return nil, fmt.Errorf("sealed: reply key is %d bytes, not %d", len(rk), RKSize)
	}
	h.Auth = ""
	c, err := h.Canonical()
	if err != nil {
		return nil, err
	}
	m := hmac.New(sha256.New, rk)
	m.Write(c)
	m.Write([]byte{0})
	m.Write(body)
	return m.Sum(nil), nil
}

// VerifyAuth checks a reply's auth in constant time; a missing or wrong one is RejectBadHeader.
func VerifyAuth(rk []byte, h Header, body []byte) error {
	if h.Auth == "" {
		return reject(RejectBadHeader, "no auth")
	}
	got, err := base64.RawURLEncoding.Strict().DecodeString(h.Auth)
	if err != nil || strings.ContainsAny(h.Auth, "=\r\n") {
		return reject(RejectBadHeader, "auth is not base64url")
	}
	want, err := authMAC(rk, h, body)
	if err != nil {
		return reject(RejectBadHeader, err.Error())
	}
	if !hmac.Equal(got, want) {
		return reject(RejectBadHeader, "wrong auth")
	}
	return nil
}

// NewRK returns a fresh reply key.
func NewRK() ([]byte, error) {
	rk := make([]byte, RKSize)
	if _, err := rand.Read(rk); err != nil {
		return nil, fmt.Errorf("sealed: %w", err)
	}
	return rk, nil
}

// EncodeRK is the header's rk member for a reply key: base64url without padding.
func EncodeRK(rk []byte) string { return base64.RawURLEncoding.EncodeToString(rk) }

// DecodeRK reads a header's rk member, refusing anything but RKSize bytes of unpadded base64url.
func DecodeRK(s string) ([]byte, error) {
	if strings.ContainsAny(s, "=\r\n") {
		return nil, errors.New("sealed: rk is not unpadded base64url")
	}
	rk, err := base64.RawURLEncoding.Strict().DecodeString(s)
	if err != nil {
		return nil, errors.New("sealed: rk is not unpadded base64url")
	}
	if len(rk) != RKSize {
		return nil, fmt.Errorf("sealed: rk is %d bytes, not %d", len(rk), RKSize)
	}
	return rk, nil
}

// Reject is a hosting node's refusal reason (spec/ext-sealed.md §6), sent as Kontra-Sealed-Reject with
// Kontra-Node: sealed_rejected and a -32602 body. The hiring node uses the same reasons for what it drops.
type Reject string

// The reasons of §6.
const (
	RejectUndecryptable Reject = "undecryptable"
	RejectBadHeader     Reject = "bad_header"
	RejectWrongTask     Reject = "wrong_task"
	RejectWrongContext  Reject = "wrong_context"
	RejectWrongMessage  Reject = "wrong_message"
	RejectWrongJob      Reject = "wrong_job"
	RejectReplayed      Reject = "replayed"
	RejectNotRecipient  Reject = "not_a_recipient"
)

// Reasons are §6's reasons, in its order.
var Reasons = []Reject{RejectUndecryptable, RejectBadHeader, RejectWrongTask, RejectWrongContext,
	RejectWrongMessage, RejectWrongJob, RejectReplayed, RejectNotRecipient}

// RejectError is a refusal with its reason. Detail is for the node's log, never sent: at most maxDetail bytes,
// with no line break or other control character, since it may quote the sender's bytes.
type RejectError struct {
	Reason Reject
	Detail string
}

func (e *RejectError) Error() string { return "sealed part rejected: " + string(e.Reason) }

// maxDetail bounds a RejectError's Detail.
const maxDetail = 256

func reject(r Reject, detail string) *RejectError {
	return &RejectError{Reason: r, Detail: boundDetail(detail)}
}

// boundDetail keeps a log detail to one bounded line: what a decoder's or age's error copied of the input (a number
// literal, a stanza) is cut to maxDetail bytes plus "…", and its control characters are escaped.
func boundDetail(s string) string {
	cut := func(s string) string {
		if len(s) <= maxDetail {
			return s
		}
		n := maxDetail
		for n > 0 && !utf8.RuneStart(s[n]) {
			n--
		}
		return s[:n] + "…"
	}
	s = cut(s)
	if strings.IndexFunc(s, unicode.IsControl) >= 0 {
		q := strconv.Quote(s)
		s = cut(q[1 : len(q)-1])
	}
	return s
}

// ReasonOf is err's refusal reason, ok false when err is not a *RejectError.
func ReasonOf(err error) (Reject, bool) {
	var re *RejectError
	if errors.As(err, &re) {
		return re.Reason, true
	}
	return "", false
}
