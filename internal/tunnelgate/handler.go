package tunnelgate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/hashicorp/yamux"

	"kontra.sh/kontra/internal/enroll"
	"kontra.sh/kontra/internal/tunnel"
)

// Admitter decides a proven hello; *enroll.Store implements it.
type Admitter interface {
	Admit(ctx context.Context, req enroll.Request) (*enroll.Decision, error)
}

// SessionSink registers the sessions the Handler admitted; *Registry implements it (WP-04). Tests and the
// e2e FakeGate use their own.
//
// Add is called once per session, after welcome was sent, once the session can carry streams: yamux runs,
// the control stream is open (the first stream the gate opened, s's control), the ping loop and the loop
// that accepts node-opened streams run. Until Add returns nil the session is no candidate for any stream
// but the control stream, so a sink may use it (a card fetch, relay.md §3) before it makes it one (the
// tunnels row). served is the welcome's services.
//
// An error from Add closes the session: the node has its welcome and redials as after any close. After
// Add returns nil the registration is the sink's: it removes the session when mux.CloseChan() closes. The
// Handler closes mux when the node leaves, when the ping loop finds it silent, and when a sink (or anyone)
// calls mux.Close.
type SessionSink interface {
	Add(ctx context.Context, s *Session, mux *yamux.Session, served []tunnel.WelcomeService) error
}

var _ SessionSink = (*Registry)(nil)

// HandlerConfig configures the upgrade handler.
type HandlerConfig struct {
	// PublicURL is the gate's public_url: hello.origin must equal its origin (bad_origin).
	PublicURL string
	// ChannelBinding is tunnel.channel_binding: tls-exporter (the gate terminates TLS; the exporter is read
	// from the upgrade request's TLS state) or none (§4).
	ChannelBinding string
	ReplicaID      string
	Admitter       Admitter
	// Registry registers admitted sessions (*Registry in the gate).
	Registry SessionSink
	// Draining reports the replica's drain: a draining replica refuses new tunnels (draining, relay.md §14.3).
	// Nil is never draining.
	Draining func() bool
	Logger   *slog.Logger

	// PingEvery is the ping loop's interval, tunnel.PingEvery when zero; welcome.ping_s carries it in
	// whole seconds (at least 1). Tests shorten it.
	PingEvery time.Duration
	// Yamux is the sessions' configuration, tunnel.YamuxConfig(Logger) when nil. Tests shorten its
	// durations; its keepalive must stay off (D33).
	Yamux *yamux.Config
}

// Handler serves GET /v1/tunnel (relay.md §2.1). Mounted on the edge router, it is behind the edge's
// rate and connection limits; it carries no credential in a header (§3), so the edge's per-key in-flight
// cap never applies to it.
type Handler struct {
	cfg    HandlerConfig
	origin string // the canonical origin of PublicURL
	log    *slog.Logger
	ping   time.Duration
}

// NewHandler returns the upgrade handler, refusing a configuration it cannot serve: a public_url without
// an http(s) origin, a channel binding other than tls-exporter or none, no replica id, no Admitter or no
// Registry, or a yamux configuration with keepalive on.
func NewHandler(cfg HandlerConfig) (*Handler, error) {
	u, err := url.Parse(cfg.PublicURL)
	if err != nil {
		return nil, fmt.Errorf("tunnelgate: public_url: %w", err)
	}
	origin, err := tunnel.CanonicalOrigin(u)
	if err != nil {
		return nil, fmt.Errorf("tunnelgate: public_url: %w", err)
	}
	switch {
	case cfg.ChannelBinding != tunnel.CBTLSExporter && cfg.ChannelBinding != tunnel.CBNone:
		return nil, fmt.Errorf("tunnelgate: channel binding %q: want %s or %s", cfg.ChannelBinding, tunnel.CBTLSExporter, tunnel.CBNone)
	case cfg.ReplicaID == "":
		return nil, errors.New("tunnelgate: no replica id")
	case cfg.Admitter == nil:
		return nil, errors.New("tunnelgate: no Admitter")
	case cfg.Registry == nil:
		return nil, errors.New("tunnelgate: no session registry")
	case cfg.Yamux != nil && cfg.Yamux.EnableKeepAlive:
		return nil, errors.New("tunnelgate: yamux keepalive must be off (D33)")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Draining == nil {
		cfg.Draining = func() bool { return false }
	}
	h := &Handler{cfg: cfg, origin: origin, log: cfg.Logger.With("component", "tunnel"), ping: cfg.PingEvery}
	if h.ping <= 0 {
		h.ping = tunnel.PingEvery
	}
	if h.cfg.Yamux == nil {
		h.cfg.Yamux = tunnel.YamuxConfig(cfg.Logger)
	}
	return h, nil
}

// ServeHTTP upgrades (400 without the subprotocol; binary messages only after the handshake; compression
// off), runs the handshake within tunnel.HandshakeTimeout with a 16 KiB read limit (§4): hello, the
// origin check, challenge (a UUIDv7 session id, a gate nonce, cb, enc and enc_old), proof, Admit, then
// welcome; or error with close status 4000 + i. After welcome: yamux.Server, the control stream as the
// first stream, the ping loop, the accept loop for node-opened streams (closed in M2), and Registry.Add.
// It returns when the session ends.
//
// Order. welcome goes before Registry.Add, which the M2 plan put the other way round: a registered
// session is a candidate for dispatch streams (relay.md §6.1), and no stream may be opened before welcome
// is written (a yamux frame ahead of welcome would break the node's handshake) nor before the control
// stream (§7); a tunnel service's card can also then be fetched through the session before its tunnels row
// makes it a candidate (M2 plan audit, gap 3). §11's "on welcome the gate inserts tunnels" holds.
//
// Drain. A draining replica answers draining (status 4010, retried) up to the proof; once Admit has run
// the handshake ends in welcome, since a refusal after an admission that consumed enrollment tokens would
// make the node resend them (M2 plan audit, gap 1). A session admitted while the drain starts is the
// registry's to tell {"t": "drain"}.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	log := h.log.With("remote", r.RemoteAddr)
	if !offersSubprotocol(r.Header) {
		http.Error(w, "the upgrade must offer Sec-WebSocket-Protocol: "+tunnel.Subprotocol, http.StatusBadRequest)
		return
	}
	var cbValue []byte
	if h.cfg.ChannelBinding == tunnel.CBTLSExporter {
		// tls-exporter is never downgraded to none for one connection (M2 plan audit): a connection
		// without an exporter is refused before the upgrade.
		//
		// SPEC-CONFLICT: §4 has no code for a connection that cannot be bound (TLS 1.2 without Extended
		// Master Secret, or tunnel.channel_binding tls-exporter on a listener without TLS), and its list of
		// codes is closed. The closest faithful behaviour is to refuse the upgrade itself, so that no
		// challenge (and no cb none) is ever sent: 400 when the client's TLS cannot export, 500 when the
		// gate's own configuration is wrong. The node sees a failed dial and retries with backoff. The
		// owner may prefer a handshake code for it (a §4 amendment).
		v, err := tunnel.ChannelBinding(r.TLS)
		if err != nil {
			status, msg := http.StatusBadRequest, "this TLS connection has no exporter for the channel binding (tls-exporter needs TLS 1.3, or TLS 1.2 with Extended Master Secret)"
			if r.TLS == nil {
				// tunnel.channel_binding is tls-exporter on a listener without TLS: the gate's own error.
				status, msg = http.StatusInternalServerError, "the gate binds tls-exporter on a connection without TLS"
			}
			log.Warn("tunnel refused before the upgrade: no channel binding", "err", err, "status", status)
			http.Error(w, msg, status)
			return
		}
		cbValue = v
	}
	upgraded := time.Now()
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols:    []string{tunnel.Subprotocol},
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		log.Debug("tunnel upgrade failed", "err", err) // Accept answered the request
		return
	}
	defer c.CloseNow()
	if c.Subprotocol() != tunnel.Subprotocol {
		_ = c.Close(websocket.StatusProtocolError, "kontra-tunnel.v1 not selected")
		return
	}
	c.SetReadLimit(tunnel.MaxHandshakeMessage)

	// The session outlives the request: the connection is hijacked, and the request's context is not
	// for use after Accept.
	hctx, cancel := context.WithDeadline(context.WithoutCancel(r.Context()), upgraded.Add(tunnel.HandshakeTimeout))
	defer cancel()
	a, err := h.handshake(hctx, c, cbValue, log)
	if err != nil {
		var herr *tunnel.HandshakeError
		if errors.As(err, &herr) {
			h.refuse(hctx, c, herr, log)
			return
		}
		switch {
		case errors.Is(err, errGate) && hctx.Err() != nil:
			log.Warn("tunnel admission ran out of the handshake's time", "after", time.Since(upgraded).Round(time.Millisecond), "err", err)
		case errors.Is(err, errGate):
			log.Error("tunnel handshake failed on the gate's side", "err", err)
		case hctx.Err() != nil:
			log.Info("tunnel handshake timed out", "after", time.Since(upgraded).Round(time.Millisecond), "err", err)
		default:
			log.Info("tunnel handshake ended by the connection", "err", err)
		}
		// SPEC-CONFLICT: §4's M4 is "welcome or error", and error's codes are a closed list with none for
		// the gate's own failure (Postgres unreachable during Admit, a timeout of the admission). The
		// closest faithful behaviour is to send neither and close with 1011 (internal error), not a
		// handshake status: the node retries it as one of "the others" (§4), and, since an admission may
		// have committed before the failure, redials with its active key and no tokens first (node
		// ErrNoOutcome; M2 plan audit, gap 1). The owner may prefer a retryable code for it.
		_ = c.Close(websocket.StatusInternalError, "")
		return
	}
	cancel()
	h.serve(c, a, log)
}

// errGate marks a handshake that failed on the gate's side, not the node's or the connection's.
var errGate = errors.New("tunnelgate")

// admitted is a handshake that ended in Admit's decision.
type admitted struct {
	session uuid.UUID
	hello   *tunnel.Hello
	dec     *enroll.Decision
}

// handshake runs M1 to Admit and writes welcome. A refusal is a *tunnel.HandshakeError for the caller to
// send; any other error closes the connection without one.
func (h *Handler) handshake(ctx context.Context, c *websocket.Conn, cbValue []byte, log *slog.Logger) (*admitted, error) {
	// M1.
	hb, err := readMessage(ctx, c)
	if err != nil {
		return nil, fmt.Errorf("read hello: %w", err)
	}
	if h.cfg.Draining() {
		return nil, tunnel.Refuse(tunnel.CodeDraining, "this replica is draining: dial again")
	}
	hello, err := tunnel.ParseHello(hb)
	if err != nil {
		return nil, err
	}
	if hello.Origin != h.origin {
		return nil, tunnel.Refuse(tunnel.CodeBadOrigin, "hello: origin %q is not this gate's (%s)", hello.Origin, h.origin)
	}

	// M2. Fresh per connection: the session id, the gate nonce and enc (§4.1, no replay).
	session, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("%w: session id: %w", errGate, err)
	}
	gn, gnEnc, err := tunnel.NewNonce()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errGate, err)
	}
	cb := h.cfg.ChannelBinding
	info, err := tunnel.Transcript{
		Origin: hello.Origin, CB: cb, CBValue: cbValue, Hello: hello.Raw, Session: session.String(), GateNonce: gn,
	}.Info()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errGate, err)
	}
	pub, err := tunnel.ParseRecipient(hello.Recipient)
	if err != nil { // ParseHello checked it
		return nil, tunnel.Refuse(tunnel.CodeBadProof, "hello: %v", err)
	}
	enc, want, err := tunnel.Encapsulate(pub, info, tunnel.LabelPoP)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errGate, err)
	}
	ch := tunnel.Challenge{Session: session.String(), Nonce: gnEnc, CB: cb, Enc: tunnel.EncodeBase64URL(enc)}
	var oldPub, wantOld []byte
	if r := hello.RotateFrom; r != nil {
		op, err := tunnel.ParseRecipient(r.Recipient)
		if err != nil {
			return nil, tunnel.Refuse(tunnel.CodeBadProof, "hello: rotate_from: %v", err)
		}
		encOld, w, err := tunnel.Encapsulate(op, info, tunnel.LabelPoPOld)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", errGate, err)
		}
		ch.EncOld, oldPub, wantOld = tunnel.EncodeBase64URL(encOld), op.Bytes(), w
	}
	if err := writeMessage(ctx, c, &ch); err != nil {
		return nil, fmt.Errorf("send challenge: %w", err)
	}

	// M3.
	pb, err := readMessage(ctx, c)
	if err != nil {
		return nil, fmt.Errorf("read proof: %w", err)
	}
	p, err := tunnel.ParseProof(pb)
	if err != nil {
		return nil, err
	}
	proof, _ := tunnel.DecodeBase64URL(p.Proof) // ParseProof checked both
	if !tunnel.Accept(proof, want) {
		return nil, tunnel.Refuse(tunnel.CodeBadProof, "the proof does not verify for this connection")
	}
	switch {
	case hello.RotateFrom != nil && p.ProofOld == "":
		return nil, tunnel.Refuse(tunnel.CodeBadProof, "rotate_from without proof_old")
	case hello.RotateFrom != nil:
		po, _ := tunnel.DecodeBase64URL(p.ProofOld)
		if !tunnel.Accept(po, wantOld) {
			return nil, tunnel.Refuse(tunnel.CodeBadProof, "proof_old does not verify for this connection")
		}
	case p.ProofOld != "":
		return nil, tunnel.Refuse(tunnel.CodeBadProof, "proof_old without rotate_from")
	}
	// The last point where the drain can refuse: Admit may consume tokens.
	if h.cfg.Draining() {
		return nil, tunnel.Refuse(tunnel.CodeDraining, "this replica is draining: dial again")
	}

	// Admission, inside the handshake's 10 s (§4).
	dec, err := h.cfg.Admitter.Admit(ctx, enroll.Request{
		Hello: hello, PubKey: pub.Bytes(), OldPubKey: oldPub, SessionID: session, ReplicaID: h.cfg.ReplicaID,
	})
	if err != nil {
		var herr *tunnel.HandshakeError
		if errors.As(err, &herr) {
			return nil, herr
		}
		return nil, fmt.Errorf("%w: admit: %w", errGate, err)
	}
	if dec == nil {
		return nil, fmt.Errorf("%w: admit: no decision", errGate)
	}

	// M4.
	wb, err := tunnel.Encode(&tunnel.Welcome{
		Session: session.String(), Replica: h.cfg.ReplicaID, PingS: pingSeconds(h.ping),
		Tenant: dec.Key.Tenant, Services: dec.Served,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: welcome: %w", errGate, err)
	}
	if err := c.Write(ctx, websocket.MessageText, wb); err != nil {
		log.Warn("tunnel welcome lost: the node redials", "session", session, "key_id", dec.Key.KeyID, "err", err)
		return nil, fmt.Errorf("send welcome: %w", err)
	}
	return &admitted{session: session, hello: hello, dec: dec}, nil
}

// refuse sends M4 error and closes with 4000 + the code's index (§4).
func (h *Handler) refuse(ctx context.Context, c *websocket.Conn, herr *tunnel.HandshakeError, log *slog.Logger) {
	status, ok := herr.Code.CloseStatus()
	if !ok {
		// Not one of §4's codes: a bug on this side. The node sees a close it retries.
		log.Error("tunnel refusal with an unknown code", "code", herr.Code, "message", herr.Message)
		_ = c.Close(websocket.StatusInternalError, "")
		return
	}
	log.Info("tunnel refused", "code", herr.Code, "message", herr.Message)
	if err := writeMessage(ctx, c, &tunnel.ErrorMessage{Code: herr.Code, Message: herr.Message}); err != nil {
		log.Debug("tunnel refusal not delivered", "err", err)
	}
	_ = c.Close(websocket.StatusCode(status), string(herr.Code))
}

// serve runs an admitted session until it ends (§7, §11, §13).
func (h *Handler) serve(c *websocket.Conn, a *admitted, log *slog.Logger) {
	dec := a.dec
	log = log.With("session", a.session, "key_id", dec.Key.KeyID, "tenant", dec.Key.Tenant, "node", a.hello.Name)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mux, err := tunnel.ServerSessionWith(ctx, c, h.cfg.Yamux)
	if err != nil {
		log.Error("tunnel session did not start", "err", err)
		_ = c.Close(websocket.StatusInternalError, "")
		return
	}
	defer mux.Close()

	// The control stream is the first stream the gate opens (§7, §13).
	control, err := openControl(mux)
	if err != nil {
		log.Warn("tunnel control stream did not open", "err", err)
		return
	}
	s := &Session{
		ID:            a.session,
		KeyID:         dec.Key.KeyID,
		TenantID:      dec.Key.TenantID,
		Tenant:        dec.Key.Tenant,
		HirePrincipal: dec.HirePrincipal,
		CLIVersion:    a.hello.Agent,
		NodeName:      a.hello.Name,
		mux:           mux,
		served:        make(map[string]string, len(dec.Served)),
		control:       control,
	}
	for _, sv := range dec.Served {
		s.served[sv.Name] = sv.Kind
	}

	go acceptNodeStreams(mux, log)
	live := make(chan error, 1)
	go func() { live <- tunnel.KeepAlive(ctx, mux, h.ping, tunnel.PingFailures) }()

	if err := h.cfg.Registry.Add(ctx, s, mux, dec.Served); err != nil {
		log.Error("tunnel session not registered: closing it", "err", err)
		return
	}
	log.Info("tunnel session open", "services", servedNames(dec.Served), "agent", a.hello.Agent)
	<-mux.CloseChan()
	// The ping loop ends with the session; when it is what closed it, it returns once the close is done.
	cancel()
	if err := <-live; err != nil {
		log.Warn("tunnel session closed: the node is silent", "err", err)
	} else {
		log.Info("tunnel session closed")
	}
}

// openControl opens the control stream and writes its preamble (§8.1: op control).
func openControl(mux *yamux.Session) (net.Conn, error) {
	st, err := mux.OpenStream()
	if err != nil {
		return nil, err
	}
	_ = st.SetWriteDeadline(time.Now().Add(tunnel.ConnectionWriteTimeout))
	if err := tunnel.WritePreamble(st, tunnel.Preamble{Op: tunnel.OpControl}); err != nil {
		_ = st.Close()
		return nil, err
	}
	_ = st.SetWriteDeadline(time.Time{})
	return st, nil
}

// acceptNodeStreams accepts node-opened streams promptly (§7: an un-accepted stream holds the node's
// open, and its data, until the backlog fills) and closes them: hiring (§9) and blob fetches (§8.3) are
// M3. It returns when the session ends.
func acceptNodeStreams(mux *yamux.Session, log *slog.Logger) {
	for {
		st, err := mux.AcceptStream()
		if err != nil {
			return
		}
		log.Debug("node-opened stream closed: hiring is not served before M3", "stream", st.StreamID())
		_ = st.Close()
	}
}

// WriteControl writes one control line on the session's control stream (§13), within
// tunnel.ConnectionWriteTimeout: a node that stopped reading its control stream cannot hold the writer.
// It is how a SessionSink tells the node drain, revoked, auth_revoked or enrolled.
func (s *Session) WriteControl(c tunnel.Control) error {
	s.controlMu.Lock()
	defer s.controlMu.Unlock()
	if s.control == nil {
		return ErrSessionGone
	}
	_ = s.control.SetWriteDeadline(time.Now().Add(tunnel.ConnectionWriteTimeout))
	defer s.control.SetWriteDeadline(time.Time{})
	return tunnel.WriteControl(s.control, c)
}

// Mux is the session's yamux.Server.
func (s *Session) Mux() *yamux.Session { return s.mux }

// offersSubprotocol reports whether the upgrade offers kontra-tunnel.v1 among its Sec-WebSocket-Protocol
// values (one header or several, comma-separated).
func offersSubprotocol(h http.Header) bool {
	for _, v := range h.Values("Sec-WebSocket-Protocol") {
		for _, p := range strings.Split(v, ",") {
			if strings.TrimSpace(p) == tunnel.Subprotocol {
				return true
			}
		}
	}
	return false
}

// readMessage reads one handshake message: text only (§4); the 16 KiB read limit is the connection's.
func readMessage(ctx context.Context, c *websocket.Conn) ([]byte, error) {
	typ, b, err := c.Read(ctx)
	if err != nil {
		return nil, err
	}
	if typ != websocket.MessageText {
		// Not a v1 handshake message this gate reads (ParseHello's rule for what fits no other code).
		return nil, tunnel.Refuse(tunnel.CodeUnsupportedVersion, "a binary message during the handshake")
	}
	return b, nil
}

// writeMessage encodes and sends one handshake message as text.
func writeMessage(ctx context.Context, c *websocket.Conn, msg any) error {
	b, err := tunnel.Encode(msg)
	if err != nil {
		return err
	}
	return c.Write(ctx, websocket.MessageText, b)
}

// pingSeconds is welcome.ping_s: the interval in whole seconds, rounded up, at least 1.
func pingSeconds(d time.Duration) int {
	return max(1, int(math.Ceil(d.Seconds())))
}

func servedNames(s []tunnel.WelcomeService) []string {
	names := make([]string, len(s))
	for i, x := range s {
		names[i] = x.Name
	}
	return names
}
