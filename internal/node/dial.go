package node

import (
	"context"
	"crypto/ecdh"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"

	"kontra.sh/kontra/internal/nodekey"
	"kontra.sh/kontra/internal/tunnel"
)

// Bounds of the node's own waits. The gate closes a handshake that has not completed 10 s after the
// upgrade (§4); a gate that does not, or a path that went silent, must not hold the node longer.
const (
	// DialTimeout bounds TCP, TLS and the upgrade.
	DialTimeout = 15 * time.Second
	// HandshakeWait bounds the four messages, from hello to welcome or error.
	HandshakeWait = tunnel.HandshakeTimeout + 5*time.Second
)

// Errors of the node's half of the handshake.
var (
	// ErrSubprotocol is a gate that did not select kontra-tunnel.v1 (§3): the node closes the connection.
	ErrSubprotocol = errors.New("node: the gate did not select the " + tunnel.Subprotocol + " subprotocol")
	// ErrChannelBindingRequired is a challenge with cb none to a node started with
	// --require-channel-binding (§4.1): the node sends no proof. Its operator must change something (the
	// flag, or the gate's TLS), so it is not retried.
	ErrChannelBindingRequired = errors.New("node: the gate offers no channel binding (cb none) and --require-channel-binding is set")
	// ErrNoOutcome is a connection that ended, or a wait that ran out, after the proof was (or may have
	// been) sent and before welcome or error arrived. The gate may have admitted the hello: an enrollment
	// or a rotation may have committed and its tokens been consumed. The node then redials with its active
	// key and neither tokens nor rotate_from: a welcome means the change committed, unknown_key that it did
	// not (M2 plan audit, gap 1 and conflict C2). A consumed token is never sent again.
	ErrNoOutcome = errors.New("node: the connection ended after the proof, before welcome or error")
)

// Dialed is an upgraded connection before the handshake.
type Dialed struct {
	WS      *websocket.Conn
	Origin  string // tunnel.CanonicalOrigin of what was dialed: hello.origin
	CBValue []byte // the TLS exporter of this connection; nil over plain http (localhost)
}

// UserAgent is the User-Agent of the upgrade and hello's agent (§3, §4): kontra/<version> (<os>/<arch>).
func UserAgent(version string) string {
	if version == "" {
		version = "dev"
	}
	return fmt.Sprintf("kontra/%s (%s/%s)", version, runtime.GOOS, runtime.GOARCH)
}

// Dial opens GET {gate}/v1/tunnel with Upgrade: websocket, Sec-WebSocket-Protocol: kontra-tunnel.v1 and
// User-Agent: kontra/<version> (<os>/<arch>), no credential in any header (§3). It closes a connection on
// which the gate did not select the subprotocol, and sets the 16 KiB handshake read limit.
//
// It is Dialer{UserAgent: userAgent}.Dial: the system's roots and proxy settings, no redirects.
func Dial(ctx context.Context, gate *url.URL, userAgent string) (*Dialed, error) {
	return Dialer{UserAgent: userAgent}.Dial(ctx, gate)
}

// Dialer dials gates.
type Dialer struct {
	// UserAgent is sent on the upgrade; empty is UserAgent("dev").
	UserAgent string
	// HTTPClient performs the upgrade. Nil is a client with Go's default transport: the system's roots,
	// and HTTPS_PROXY/HTTP_PROXY for the gate (an HTTP CONNECT proxy carries the TLS end to end, so the
	// channel binding holds; a proxy that terminates TLS fails the proof). Whatever the client, a redirect
	// is never followed: the hello, which can carry credentials, goes only to the origin dialed (§2.4).
	HTTPClient *http.Client
}

// Dial is the package's Dial with d's client.
func (d Dialer) Dial(ctx context.Context, gate *url.URL) (*Dialed, error) {
	if gate == nil {
		return nil, errors.New("node: dial: no gate URL")
	}
	// The URL is checked again here, not only at flag parsing: net/http would send userinfo as an
	// Authorization header, and §3 keeps every credential out of headers.
	u, err := tunnel.CheckGateURL(gate.String())
	if err != nil {
		return nil, fmt.Errorf("node: dial: %w", err)
	}
	origin, err := tunnel.CanonicalOrigin(u)
	if err != nil {
		return nil, fmt.Errorf("node: dial: %w", err)
	}
	ua := d.UserAgent
	if ua == "" {
		ua = UserAgent("")
	}
	target := u.JoinPath(tunnel.Path)

	ctx, cancel := context.WithTimeout(ctx, DialTimeout)
	defer cancel()
	ws, resp, err := websocket.Dial(ctx, target.String(), &websocket.DialOptions{
		HTTPClient:      noRedirects(d.HTTPClient),
		HTTPHeader:      http.Header{"User-Agent": []string{ua}},
		Subprotocols:    []string{tunnel.Subprotocol},
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return nil, fmt.Errorf("node: dial %s: %w%s", origin, err, refusalBody(resp))
	}
	if ws.Subprotocol() != tunnel.Subprotocol {
		_ = ws.Close(websocket.StatusProtocolError, "kontra-tunnel.v1 not selected")
		return nil, fmt.Errorf("node: dial %s: %w", origin, ErrSubprotocol)
	}
	ws.SetReadLimit(tunnel.MaxHandshakeMessage)
	dialed := &Dialed{WS: ws, Origin: origin}
	if u.Scheme == "https" {
		// The exporter of this TLS connection (§4.1); the node reads it from the upgrade's response. A
		// connection that cannot export (TLS 1.2 without Extended Master Secret) has none: a gate that binds
		// tls-exporter then fails the handshake rather than either side falling back to none.
		if resp != nil {
			if cb, err := tunnel.ChannelBinding(resp.TLS); err == nil {
				dialed.CBValue = cb
			}
		}
	}
	return dialed, nil
}

// refusalBody is the start of the body of a refused upgrade (the gate says why: a missing subprotocol, no
// channel binding on this connection), for the dial's error; coder/websocket keeps up to 1 KiB of it.
func refusalBody(resp *http.Response) string {
	if resp == nil || resp.Body == nil {
		return ""
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	line, _, _ := strings.Cut(strings.TrimSpace(string(b)), "\n")
	if line == "" {
		return ""
	}
	if len(line) > 200 {
		line = line[:200] + "…"
	}
	return ": " + strconv.QuoteToGraphic(line)
}

// noRedirects is c (or a default client) that never follows a redirect.
func noRedirects(c *http.Client) *http.Client {
	var n http.Client
	if c != nil {
		n = *c
	}
	n.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &n
}

// Handshake is the node's half (§4): send hello (its exact bytes kept for the transcript), read the
// challenge, refuse cb none when requireCB, prove (and prove the old key for rotate_from), read welcome or
// error. A refusal is a *tunnel.HandshakeError.
//
// Handshake fills hello in place: origin (it must be d.Origin when set), kid and recipient (the
// identity's active key when empty; they must be it when set), and always a fresh nonce, since both
// nonces are fresh per connection (§4.1). hello.Raw is what was sent. rotate_from is the caller's (a
// pending rotation, nodekey.Identity.PendingRotation); its private key must be in the identity file.
//
// After any failure the connection is closed. The errors that matter to the caller's retry decision:
//   - *tunnel.HandshakeError: the gate's refusal, from its error message or, when that was lost, its
//     close status 4000 + i (tunnel.CloseStatusOf); Code.Retryable says whether to redial;
//   - ErrChannelBindingRequired: not retried;
//   - ErrNoOutcome: the outcome of an admission that may have committed is unknown (see its doc);
//   - anything else happened before the proof was sent: nothing was admitted.
//
// On success the connection is left open for tunnel.ClientSession.
func Handshake(ctx context.Context, d *Dialed, id *nodekey.Identity, hello *tunnel.Hello, requireCB bool) (*tunnel.Welcome, error) {
	if d == nil || d.WS == nil {
		return nil, errors.New("node: handshake: no connection")
	}
	w, err := handshake(ctx, d, id, hello, requireCB)
	if err != nil {
		_ = d.WS.CloseNow()
		return nil, err
	}
	return w, nil
}

func handshake(ctx context.Context, d *Dialed, id *nodekey.Identity, hello *tunnel.Hello, requireCB bool) (*tunnel.Welcome, error) {
	if id == nil || hello == nil {
		return nil, errors.New("node: handshake: no identity or no hello")
	}
	ctx, cancel := context.WithTimeout(ctx, HandshakeWait)
	defer cancel()

	// What is proven, before anything is sent.
	sk, err := id.PrivateKey()
	if err != nil {
		return nil, fmt.Errorf("node: handshake: %w", err)
	}
	if err := fillHello(d, id, hello); err != nil {
		return nil, err
	}
	var oldSK *ecdh.PrivateKey
	if hello.RotateFrom != nil {
		if oldSK, err = id.OldPrivateKey(hello.RotateFrom.Recipient); err != nil {
			return nil, fmt.Errorf("node: handshake: rotate_from: %w", err)
		}
		if tunnel.KeyID(hello.RotateFrom.Recipient) != hello.RotateFrom.KID {
			return nil, errors.New("node: handshake: rotate_from's kid is not its recipient's key id")
		}
	}
	b, err := tunnel.Encode(hello)
	if err != nil {
		return nil, fmt.Errorf("node: handshake: %w", err)
	}

	// M1, M2.
	if err := d.WS.Write(ctx, websocket.MessageText, b); err != nil {
		return nil, fmt.Errorf("node: handshake: send hello: %w", err)
	}
	m2, err := readMessage(ctx, d.WS)
	if err != nil {
		return nil, refusalOr(err, "node: handshake: read challenge")
	}
	if isError(m2) {
		_, err := tunnel.ParseOutcome(m2)
		return nil, err
	}
	ch, err := tunnel.ParseChallenge(m2)
	if err != nil {
		return nil, fmt.Errorf("node: handshake: %w", err)
	}
	var cbValue []byte
	switch ch.CB {
	case tunnel.CBNone:
		if requireCB {
			_ = d.WS.Close(websocket.StatusPolicyViolation, "channel binding required")
			return nil, ErrChannelBindingRequired
		}
	case tunnel.CBTLSExporter:
		if len(d.CBValue) == 0 {
			return nil, fmt.Errorf("node: handshake: the gate binds %s, and this connection has no TLS exporter", tunnel.CBTLSExporter)
		}
		cbValue = d.CBValue
	}
	switch {
	case hello.RotateFrom != nil && ch.EncOld == "":
		return nil, errors.New("node: handshake: a challenge without enc_old to a hello with rotate_from")
	case hello.RotateFrom == nil && ch.EncOld != "":
		return nil, errors.New("node: handshake: a challenge with enc_old to a hello without rotate_from")
	}
	nonce, _ := tunnel.DecodeBase64URL(ch.Nonce) // ParseChallenge checked it
	info, err := tunnel.Transcript{
		Origin: hello.Origin, CB: ch.CB, CBValue: cbValue, Hello: hello.Raw, Session: ch.Session, GateNonce: nonce,
	}.Info()
	if err != nil {
		return nil, fmt.Errorf("node: handshake: %w", err)
	}

	// M3.
	enc, _ := tunnel.DecodeBase64URL(ch.Enc)
	proof, err := tunnel.Prove(enc, sk, info, tunnel.LabelPoP)
	if err != nil {
		return nil, fmt.Errorf("node: handshake: %w", err)
	}
	p := tunnel.Proof{Proof: tunnel.EncodeBase64URL(proof)}
	if hello.RotateFrom != nil {
		encOld, _ := tunnel.DecodeBase64URL(ch.EncOld)
		po, err := tunnel.Prove(encOld, oldSK, info, tunnel.LabelPoPOld)
		if err != nil {
			return nil, fmt.Errorf("node: handshake: proof_old: %w", err)
		}
		p.ProofOld = tunnel.EncodeBase64URL(po)
	}
	pb, err := tunnel.Encode(&p)
	if err != nil {
		return nil, fmt.Errorf("node: handshake: %w", err)
	}
	if err := d.WS.Write(ctx, websocket.MessageText, pb); err != nil {
		// Some of it may have left: the outcome is unknown from here on.
		return nil, fmt.Errorf("%w: send proof: %w", ErrNoOutcome, err)
	}

	// M4.
	m4, err := readMessage(ctx, d.WS)
	if err != nil {
		if code, ok := tunnel.CloseStatusOf(err); ok {
			return nil, &tunnel.HandshakeError{Code: code, Message: closeReason(err)}
		}
		return nil, fmt.Errorf("%w: %w", ErrNoOutcome, err)
	}
	w, err := tunnel.ParseOutcome(m4)
	if err != nil {
		var herr *tunnel.HandshakeError
		if errors.As(err, &herr) {
			return nil, herr
		}
		// Not a v1 welcome or error: the gate answered something; whether it admitted is unknown.
		return nil, fmt.Errorf("%w: %w", ErrNoOutcome, err)
	}
	if w.Session != ch.Session {
		return nil, fmt.Errorf("%w: welcome for session %s, the challenge's was %s", ErrNoOutcome, w.Session, ch.Session)
	}
	return w, nil
}

// fillHello sets and checks the fields Handshake owns.
func fillHello(d *Dialed, id *nodekey.Identity, h *tunnel.Hello) error {
	switch h.Origin {
	case "":
		h.Origin = d.Origin
	case d.Origin:
	default:
		return fmt.Errorf("node: handshake: hello's origin %q is not the one dialed (%s)", h.Origin, d.Origin)
	}
	rcpt := id.Recipient()
	if h.Recipient == "" {
		h.Recipient = rcpt
	} else if h.Recipient != rcpt {
		return errors.New("node: handshake: hello's recipient is not the identity's active key")
	}
	if kid := tunnel.KeyID(rcpt); h.KID == "" {
		h.KID = kid
	} else if h.KID != kid {
		return errors.New("node: handshake: hello's kid is not the identity's active key id")
	}
	_, nonce, err := tunnel.NewNonce()
	if err != nil {
		return fmt.Errorf("node: handshake: %w", err)
	}
	h.Nonce = nonce
	h.Raw = nil
	return nil
}

// readMessage reads one handshake message; it must be text (§4).
func readMessage(ctx context.Context, ws *websocket.Conn) ([]byte, error) {
	typ, b, err := ws.Read(ctx)
	if err != nil {
		return nil, err
	}
	if typ != websocket.MessageText {
		return nil, errors.New("a binary message during the handshake")
	}
	return b, nil
}

// isError reports whether a handshake message is an M4 error (the gate refuses at M2's place when it
// refuses the hello itself).
func isError(b []byte) bool {
	var e struct {
		T string `json:"t"`
	}
	return json.Unmarshal(b, &e) == nil && e.T == tunnel.MsgError
}

// refusalOr is the gate's refusal when the read ended with a close status 4000 + i, else err wrapped.
func refusalOr(err error, what string) error {
	if code, ok := tunnel.CloseStatusOf(err); ok {
		return &tunnel.HandshakeError{Code: code, Message: closeReason(err)}
	}
	return fmt.Errorf("%s: %w", what, err)
}

// closeReason is the reason of a close error, for a refusal whose error message was lost.
func closeReason(err error) string {
	var ce websocket.CloseError
	if errors.As(err, &ce) && ce.Reason != "" {
		return ce.Reason
	}
	return "closed with status " + fmt.Sprint(int(websocket.CloseStatus(err)))
}
