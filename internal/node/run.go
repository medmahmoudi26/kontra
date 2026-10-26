package node

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"time"

	"github.com/coder/websocket"

	"kontra.sh/kontra/internal/nodekey"
	"kontra.sh/kontra/internal/obs"
	"kontra.sh/kontra/internal/tunnel"
)

// ErrRevoked ends Run when the gate revokes the node's key ({"t": "revoked"} on a control stream, §12,
// §13): the node does not redial, since the next handshake would be refused revoked; its operator must
// enroll a new key.
var ErrRevoked = errors.New("node: the gate revoked this node's key (spec/tunnel.md §12); kontra connect does not redial")

// How often a running node looks at its key file: a rotation another `kontra connect` made (one process
// per gate, §2.1) is carried to this gate soon (rotate_from on a new session, make-before-break), and a
// rotated key is pruned once its 24 h overlap has passed (§12).
const (
	maintainEvery = time.Minute
	pruneEvery    = time.Hour
)

// next is what Run does after a connection attempt.
type next int

const (
	nextSession next = iota // a session is open
	nextRetry               // back off, then dial again
	nextAgain               // dial again at once: the probe said what the next hello must be
	nextFatal               // stop: the operator must change something (§4)
)

// Run connects and serves until ctx ends or the gate revokes the key: dial, handshake, the yamux client,
// the ping loop, the control stream, the accept loop serving each gate-opened stream through the Proxy;
// on a close it redials with backoff (§13); on a drain it dials a new session at once and lets the old one
// finish (make-before-break, D35). A handshake refused with a code that is not Retryable ends Run with that
// error, as the operator must change something first (§4).
//
// Sessions. One is current: its close starts the backoff and the redial. A drained one is retired: it
// keeps serving the streams the gate opens on it until the gate closes it, and is never redialed. So is a
// session whose key another process has rotated (the new key's session is dialed at once, with rotate_from)
// and one whose stream ids are nearly exhausted (§14: the node reconnects; yamux allows about 2³¹ opens per
// side): the node watches both the ids it accepts and its own (hiring, the key set and blob fetches open streams
// from the node since M3, session.open), and dials a new session before either side's run out, or as soon as an
// open of its own is refused ErrStreamsExhausted.
//
// It returns nil when ctx ends, ErrRevoked when the gate revokes the key, a *tunnel.HandshakeError for a
// refusal that is not retried, ErrChannelBindingRequired, or an error reading the key file.
func (n *Node) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer n.closeAll()
	tick := time.NewTicker(n.maintainEvery)
	defer tick.Stop()

	// Hiring (§9): the local endpoint is bound before the first dial, so that a --listen another process holds
	// stops kontra connect at once; its requests wait for a session.
	//
	// SPEC-CONFLICT (spec/tunnel.md §9; M3 WP-03 review): §9 has every node serve 127.0.0.1:7777 (--listen), and
	// none turns it off. So every kontra connect binds it, hosting-only ones included: a second node on the same
	// host stops at start unless it is given another --listen (this host's kontra-node-hello and
	// kontra-seller-hello units would collide on an upgrade), and every hosting node offers its hiring principal
	// (its --api-key, or node:<key_id>, which org:<tenant> admits) to every local process without a token. Built as
	// written; the owner decides whether a node that hosts serves the endpoint by default, or whether hiring can
	// be turned off (for example --listen off).
	ln, err := net.Listen("tcp", n.cfg.Listen)
	if err != nil {
		return fmt.Errorf("node: --listen %s: %w (another kontra connect may hold it: pass --listen 127.0.0.1:<port>)", n.cfg.Listen, err)
	}
	hdone := make(chan struct{})
	go func() {
		defer close(hdone)
		if err := n.hire.Serve(ctx, ln); err != nil {
			n.logger.Error("the local hiring endpoint stopped", "err", err)
		}
	}()
	defer func() { cancel(); <-hdone }()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	fmt.Fprintf(n.out, "kontra connect: hiring at http://localhost:%s (%s; local clients only, spec/tunnel.md §9)\n", port, ln.Addr())
	n.logger.Info("hiring: serving local clients", "listen", ln.Addr().String())

	var wait time.Duration
	for {
		if wait > 0 {
			t := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				t.Stop()
				return nil
			case <-n.revoked:
				t.Stop()
				return ErrRevoked
			case <-t.C:
			}
		}
		select {
		case <-n.revoked:
			return ErrRevoked
		default:
		}
		if ctx.Err() != nil {
			return nil
		}
		s, nx, err := n.connect(ctx)
		switch nx {
		case nextFatal:
			return err
		case nextAgain:
			wait = 0
			continue
		case nextRetry:
			if ctx.Err() != nil {
				return nil
			}
			wait = n.backoff.Next()
			n.logger.Info("redialing the gate after a wait", "wait", wait.Round(time.Millisecond), "err", err)
			continue
		}

		// Serve the current session until it closes, or a new one must be dialed while it goes on. Hiring
		// streams go on it (OpenHire) until then.
		n.setCurrent(s)
	watch:
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-n.revoked:
				return ErrRevoked
			case <-s.mux.CloseChan():
				n.clearCurrent(s)
				lived := time.Since(s.opened)
				n.backoff.SessionEnded(lived)
				if s.replaced.Load() {
					// The gate revoked the key of this session, which another `kontra connect` has rotated
					// since it was dialed (keyRevoked): the active key's session is dialed at once.
					s.log.Info("tunnel session of a rotated key closed: dialing a session of the active key at once",
						"lived", lived.Round(time.Second))
					wait = 0
					break watch
				}
				wait = n.backoff.Next()
				s.log.Info("tunnel session closed: redialing after a wait", "lived", lived.Round(time.Second),
					"wait", wait.Round(time.Millisecond))
				break watch
			case ev := <-s.events:
				switch ev {
				case evDrain:
					n.retire(s, "the gate is draining this replica: dialing a new session at once (make-before-break)")
				case evExhausted:
					n.retire(s, "the session's stream ids are nearly exhausted: dialing a new session")
				default:
					continue
				}
				wait = 0
				break watch
			case <-tick.C:
				if n.maintain(s) {
					n.retire(s, "the key was rotated: dialing a session of the new key with rotate_from")
					wait = 0
					break watch
				}
			}
		}
	}
}

// connect dials the gate and runs the handshake, keeping the credentials and the key file's rotation
// stamps in step with what the gate answered.
//
// SPEC-CONFLICT (M2 plan C2 and audit gap 1; spec/tunnel.md §5, §12 with §4): an enrollment token is single
// use and rotate_from must name a key not yet rotated, while §4 does not retry enroll_invalid. A hello that
// carried tokens or rotate_from and lost its outcome (the connection ended after the proof, ErrNoOutcome;
// or the process stopped meanwhile) may have committed, and sending it again would be refused
// enroll_invalid for good. The node's own fix within the spec text: the next handshake is a probe, the
// same hello with the active key and no enrollment authority: neither tokens nor rotate_from, nor the API
// key, whose enroll: scope (tunnel.enroll_with_api_key) would otherwise enroll the new key of a rotation
// that did not commit, a welcome the node would take for the rotation's (M2 WP-06 review). Its welcome
// means the change committed: the rotation is confirmed (nodekey.MarkRotated), and the tokens are recorded
// consumed if the welcome serves every declared service. unknown_key (a new or rotated key the gate does
// not know), or service_denied (a known key declaring the service the tokens were to bind), means it did
// not, and the full hello is sent again at once. So does a welcome that leaves a declared service out: a
// token that re-enrolls a revoked binding and did not commit leaves it out that way (§5: a revoked binding
// is left out, not refused). A consumed token is never sent again, not even when a restart passes the same
// --enroll. The probe's service_denied is taken as "not committed" although §4 does not retry
// service_denied: the full hello it is followed by differs, and its own refusal is final. Residual: the
// node cannot tell which services a token binds, so a probe welcomed without a declared service the
// tokens did not concern (a binding revoked earlier and still declared) is read as "not committed" even
// when the tokens did commit; the full hello then carries consumed tokens and is refused enroll_invalid,
// which ends Run and drops them, as for any refused token. The owner's alternative is a gate that accepts
// a replayed rotation or token as a reconnect (idempotent admission), or a welcome that says what the
// hello enrolled.
//
// SPEC-CONFLICT (spec/tunnel.md §12 with §2.1, §5 and §15; M2 WP-06 review): §12 has the node connect with
// rotate_from naming the old key, which must be a key of this gate and neither revoked (revoked) nor
// rotated (enroll_invalid), and §4 retries neither. But one key is enrolled separately at each gate the
// node dials (§2.1), and the key file records which gates confirmed a rotation, not which hold the old
// key. A node that rotated and then enrolls at a gate that never knew the old key (a first enrollment with
// --enroll, §5), or at a gate that revoked the old key (rotate after a leak, then revoke the old key, §12
// and §15), would send a rotate_from refused for good on every start, which only editing identity.key
// would undo. The node therefore sends rotate_from to a gate that has not settled the rotation, and when
// that gate refuses it enroll_invalid or revoked, sends the hello again at once without it, and never names
// that key in rotate_from at this gate again in this process: tokens or an enroll: scope in that hello
// enroll the new key, a gate that already knows the new key welcomes it, and a gate that knows neither key
// answers unknown_key, retried until the operator enrolls the key with --enroll. Any welcome of the active
// key settles the rotation at its gate (nodekey.MarkRotated), so rotate_from is never sent there again. A
// rotate_from refused because the same hello's tokens are bad costs one more handshake, whose
// enroll_invalid is final as before. Proposal for the owner: §12 says what a node does at a gate that does
// not hold the old key (send no rotate_from there; its other authorities enroll the new key), or the gate
// admits such a hello on its other authorities and ignores the rotate_from.
func (n *Node) connect(ctx context.Context) (*session, next, error) {
	id, err := nodekey.Load(n.cfg.KeyPath)
	if err != nil {
		return nil, nextFatal, fmt.Errorf("node: the identity key: %w", err)
	}
	kid := id.KeyID()
	n.mu.Lock()
	n.activeKey = id.Recipient()
	probe := n.unsure
	skip := n.skipRotation
	n.mu.Unlock()
	log := n.logger.With("key_id", kid)

	// A rotation this gate has not settled: the next hello names the old key in rotate_from, unless this gate
	// refused it; the tokens given to the old key are the new key's.
	var rotateOld string
	if old, ok := id.PendingRotation(n.origin); ok {
		rotateOld = old.Recipient().String()
		n.adoptTokens(tunnel.KeyID(rotateOld), kid, log)
	}
	cred := n.credential()
	h := &tunnel.Hello{Name: n.cfg.Name, Agent: UserAgent(n.cfg.Version), Services: n.declaredServices()}
	if !probe {
		h.Auth = cred.APIKey
		h.Enroll = cred.Key(kid).Enroll
		if rotateOld != "" && rotateOld != skip {
			h.RotateFrom = &tunnel.KeyRef{KID: tunnel.KeyID(rotateOld), Recipient: rotateOld}
		}
	}
	sent := slices.Clone(h.Enroll)
	if probe {
		log.Info("dialing with the active key alone: the outcome of an earlier hello that carried enrollment " +
			"tokens or rotate_from is unknown")
	}
	if len(sent) > 0 {
		// Before anything is sent: a stop after the proof leaves the probe for the next start.
		if err := n.updateCredential(func(c *Credential) { c.key(kid).Unconfirmed = hashes(sent) }); err != nil {
			return nil, nextFatal, err
		}
	}

	d, err := n.dialer.Dial(ctx, n.cfg.Gate)
	if err != nil {
		n.settled(kid, sent, probe)
		log.Warn("the gate could not be dialed", "err", err)
		return nil, nextRetry, err
	}
	w, err := Handshake(ctx, d, id, h, n.cfg.RequireChannelBinding)
	var herr *tunnel.HandshakeError
	switch {
	case err == nil:
		if err := n.welcomed(kid, h, w, probe, rotateOld, log); err != nil {
			log.Error("the credentials or key file could not be updated after the welcome", "err", err)
		}
		if probe {
			if cred := n.credential(); len(cred.Key(kid).Enroll) > 0 || cred.APIKey != "" {
				// The probe carried neither the tokens still waiting (given since the hello whose outcome was
				// lost, or that hello's own when this welcome did not confirm them) nor the API key, the
				// session's hiring principal: dial again at once with the full hello (this session is seconds
				// old).
				_ = d.WS.Close(websocket.StatusNormalClosure, "")
				log.Info("the probe was welcomed; dialing again with the full hello")
				return nil, nextAgain, nil
			}
		}
		s, err := n.startSession(ctx, d, w, id, h.Auth)
		if err != nil {
			log.Warn("the tunnel session did not start", "err", err)
			return nil, nextRetry, err
		}
		return s, nextSession, nil

	case errors.As(err, &herr):
		if probe && (herr.Code == tunnel.CodeUnknownKey || herr.Code == tunnel.CodeServiceDenied) {
			// Nothing committed: the tokens are still unused (the full hello marks them unconfirmed again).
			n.setUnsure(false)
			n.clearUnconfirmed(kid, log)
			log.Info("the earlier hello was not admitted: sending it again", "code", herr.Code)
			return nil, nextAgain, nil
		}
		n.settled(kid, sent, probe)
		if h.RotateFrom != nil && (herr.Code == tunnel.CodeEnrollInvalid || herr.Code == tunnel.CodeRevoked) {
			n.mu.Lock()
			n.skipRotation = rotateOld
			n.mu.Unlock()
			log.Warn("the gate refused rotate_from, so this gate cannot carry the rotation: sending the hello again "+
				"without it (its tokens or API key may enroll the new key)", "code", herr.Code, "message", herr.Message,
				"old_key_id", tunnel.KeyID(rotateOld))
			return nil, nextAgain, nil
		}
		n.refused(kid, herr, h, sent, log)
		if !herr.Code.Retryable() {
			log.Error("the gate refused the handshake; kontra connect does not retry this without a change by its operator",
				"code", herr.Code, "message", herr.Message)
			return nil, nextFatal, err
		}
		switch {
		case herr.Code == tunnel.CodeUnknownKey && len(h.Services) == 0:
			log.Error("the gate does not know this node's key: a node that only hires is admitted on an --api-key with "+
				"the hire scope (spec/tunnel.md §9)", "message", herr.Message)
		case herr.Code == tunnel.CodeUnknownKey:
			log.Error("the gate does not know this node's key: enroll it with --enroll <token> (kontra-gate enroll <service>)",
				"message", herr.Message)
		default:
			log.Warn("the gate refused the handshake; retrying", "code", herr.Code, "message", herr.Message)
		}
		return nil, nextRetry, err

	case errors.Is(err, ErrChannelBindingRequired):
		n.settled(kid, sent, probe)
		log.Error("the gate offers no channel binding (cb none) and --require-channel-binding is set")
		return nil, nextFatal, err

	case errors.Is(err, ErrNoOutcome):
		if !probe && (len(sent) > 0 || h.RotateFrom != nil) {
			n.setUnsure(true)
		}
		log.Warn("the connection ended after the proof, before the gate's answer", "err", err)
		return nil, nextRetry, err

	default:
		// Nothing was admitted: the failure came before the proof was sent.
		n.settled(kid, sent, probe)
		log.Warn("the handshake failed", "err", err)
		return nil, nextRetry, err
	}
}

// welcomed records what a welcome of the active key settles at this gate:
//
//   - a rotation the key file holds as pending here (rotateOld) is stamped (nodekey.MarkRotated): the gate
//     knows the active key, by this rotation or otherwise, so a rotate_from would now be refused (the new
//     key exists) and is never sent here again; Prune counts its 24 h from the latest stamp;
//   - the tokens: a full hello's are recorded consumed; a probe's welcome confirms the earlier hello's
//     (Unconfirmed) only if it serves every service the probe declared (connect's SPEC-CONFLICT), and
//     otherwise leaves them waiting, to be sent again.
func (n *Node) welcomed(kid string, h *tunnel.Hello, w *tunnel.Welcome, probe bool, rotateOld string, log *slog.Logger) error {
	n.setUnsure(false)
	cur := n.credential().Key(kid)
	var confirmed []string
	switch {
	case !probe:
		confirmed = hashes(h.Enroll)
	case len(cur.Unconfirmed) == 0:
	case servesAll(w, h.Services):
		confirmed = cur.Unconfirmed
	default:
		log.Info("the probe's welcome leaves out a declared service that the earlier hello's tokens may enroll: "+
			"they are sent again", "served", welcomeNames(w))
	}
	var err error
	if len(confirmed) > 0 || len(cur.Unconfirmed) > 0 {
		err = n.updateCredential(func(c *Credential) {
			for _, x := range confirmed {
				if !slices.Contains(c.Consumed, x) {
					c.Consumed = append(c.Consumed, x)
				}
			}
			k := c.key(kid)
			k.Enroll = slices.DeleteFunc(k.Enroll, func(t string) bool { return slices.Contains(confirmed, TokenHash(t)) })
			k.Unconfirmed = nil
		})
		if len(confirmed) > 0 {
			log.Info("the gate confirmed the enrollment: its tokens are not sent again", "enrollments", len(confirmed))
		}
	}
	if rotateOld != "" {
		if merr := nodekey.MarkRotated(n.cfg.KeyPath, rotateOld, n.origin, n.now()); merr != nil {
			// Not named in rotate_from here again by this process (the gate would refuse it); a restart probes.
			n.mu.Lock()
			n.skipRotation = rotateOld
			n.mu.Unlock()
			err = errors.Join(err, merr)
		} else if h.RotateFrom != nil {
			log.Info("the gate confirmed the key's rotation: the old key is kept, decrypt-only, for 24 h (§12)",
				"old_key_id", tunnel.KeyID(rotateOld))
		} else {
			log.Info("the gate knows the rotated key: the rotation is settled at this gate, and the old key is kept, "+
				"decrypt-only, for 24 h (§12)", "old_key_id", tunnel.KeyID(rotateOld))
		}
	}
	return err
}

// servesAll reports whether the welcome lists every declared service.
func servesAll(w *tunnel.Welcome, declared []tunnel.Service) bool {
	for _, d := range declared {
		if !slices.ContainsFunc(w.Services, func(s tunnel.WelcomeService) bool { return s.Name == d.Name }) {
			return false
		}
	}
	return true
}

func welcomeNames(w *tunnel.Welcome) []string {
	out := make([]string, 0, len(w.Services))
	for _, s := range w.Services {
		out = append(out, s.Name)
	}
	return out
}

// settled records that a hello's outcome is known and was not a welcome: its tokens are no longer
// unconfirmed. A probe carries no tokens, and leaves the earlier hello's in place.
func (n *Node) settled(kid string, sent []string, probe bool) {
	if probe || len(sent) == 0 {
		return
	}
	n.clearUnconfirmed(kid, n.logger)
}

// clearUnconfirmed empties the key's Unconfirmed.
func (n *Node) clearUnconfirmed(kid string, log *slog.Logger) {
	if len(n.credential().Key(kid).Unconfirmed) == 0 {
		return
	}
	if err := n.updateCredential(func(c *Credential) { c.key(kid).Unconfirmed = nil }); err != nil {
		log.Error("the credentials file could not be updated", "err", err)
	}
}

// adoptTokens moves the tokens waiting under the key the active one replaced to the active key: they were
// given to this node (its key file) before the rotation, by --enroll with --rotate or earlier.
func (n *Node) adoptTokens(from, to string, log *slog.Logger) {
	if len(n.credential().Key(from).Enroll) == 0 {
		return
	}
	if err := n.updateCredential(func(c *Credential) {
		old := c.Keys[from]
		if old == nil {
			return
		}
		k := c.key(to)
		for _, t := range old.Enroll {
			if !c.consumed(t) && !slices.Contains(k.Enroll, t) {
				k.Enroll = append(k.Enroll, t)
			}
		}
		old.Enroll = nil
	}); err != nil {
		log.Error("the credentials file could not be updated", "err", err)
	}
}

// refused drops from the credentials file what the gate refused as invalid, so that it is not sent again:
// the tokens of a hello refused enroll_invalid (unknown, used or expired; a hello with rotate_from never
// gets here, as connect sends it again without rotate_from first), and the API key of one refused
// auth_invalid. The operator gives new ones (§4: not retried without a change by its operator).
func (n *Node) refused(kid string, herr *tunnel.HandshakeError, h *tunnel.Hello, sent []string, log *slog.Logger) {
	var err error
	switch {
	case herr.Code == tunnel.CodeEnrollInvalid && len(sent) > 0 && h.RotateFrom == nil:
		err = n.updateCredential(func(c *Credential) {
			k := c.key(kid)
			k.Enroll = slices.DeleteFunc(k.Enroll, func(t string) bool { return slices.Contains(sent, t) })
		})
		log.Warn("the gate refused the enrollment tokens: they are removed from the credentials file", "enrollments", len(sent),
			"path", n.creds.Path())
	case herr.Code == tunnel.CodeAuthInvalid && h.Auth != "":
		err = n.updateCredential(func(c *Credential) {
			if c.APIKey == h.Auth {
				c.APIKey = ""
			}
		})
		log.Warn("the gate refused the API key: it is removed from the credentials file", "path", n.creds.Path())
	}
	if err != nil {
		n.logger.Error("the credentials file could not be updated", "err", err)
	}
}

func (n *Node) credential() Credential {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.cred.clone()
}

// updateCredential changes this origin's credential: the node's copy at once, and the file under its lock,
// where change applies to what the file holds then (Credentials.Update); the node's copy is then the
// file's. change is applied to both, so it must express a change (add, remove, set), never a copy.
func (n *Node) updateCredential(change func(*Credential)) error {
	n.credMu.Lock()
	defer n.credMu.Unlock()
	n.mu.Lock()
	c := n.cred.clone()
	change(&c)
	c.tidy()
	n.cred = c
	n.mu.Unlock()
	saved, err := n.creds.Update(n.origin, change)
	if err != nil {
		return err
	}
	n.mu.Lock()
	n.cred = saved
	n.mu.Unlock()
	return nil
}

func (n *Node) setUnsure(v bool) {
	n.mu.Lock()
	n.unsure = v
	n.mu.Unlock()
}

func hashes(tokens []string) []string {
	out := make([]string, len(tokens))
	for i, t := range tokens {
		out[i] = TokenHash(t)
	}
	return out
}

// startSession starts the yamux client over the welcomed connection (§7), and the session's accept loop,
// ping loop and control stream reader; it prints the welcome's service URLs. auth is the API key the hello
// carried.
func (n *Node) startSession(ctx context.Context, d *Dialed, w *tunnel.Welcome, id *nodekey.Identity, auth string) (*session, error) {
	sctx, cancel := context.WithCancel(ctx)
	log := n.logger.With(obs.KeyTunnelSession, w.Session, obs.KeyReplica, w.Replica, "key_id", id.KeyID())
	cfg := tunnel.YamuxConfig(log)
	if n.muxConfig != nil {
		cfg = n.muxConfig.Clone()
		cfg.LogOutput, cfg.Logger = nil, tunnel.YamuxLogger(log)
	}
	mux, err := tunnel.ClientSessionWith(sctx, d.WS, cfg)
	if err != nil {
		cancel()
		_ = d.WS.CloseNow()
		return nil, err
	}
	var served []string
	for _, ws := range w.Services {
		served = append(served, ws.Name)
	}
	// The gate's cap of node-opened streams in flight (§14, §6.2): the node keeps under it (session.open).
	limit := tunnel.GateMaxInflight
	if w.Anonymous != nil {
		limit = tunnel.GateMaxInflightAnonymous
	}
	s := &session{
		n:       n,
		id:      w.Session,
		replica: w.Replica,
		key:     id.Recipient(),
		mux:     mux,
		opened:  time.Now(),
		log:     log,
		events:  make(chan event, 4),
		ctx:     sctx,
		auth:    auth,
		slots:   make(chan struct{}, limit),
	}
	s.proxy = n.proxy.forSession(served, log)
	s.proxy.onControl = s.readControl
	// The kontra-blob: parts of what the gate forwards are fetched on this session's own blob streams (§8.3):
	// the gate serves a ref to the session one of whose attempts holds its task. They share the session's slots
	// with hiring (§14).
	s.proxy.setBlobOpener(func(ctx context.Context) (net.Conn, error) { return s.open(ctx, nil) })
	n.mu.Lock()
	n.sessions[s] = struct{}{}
	n.mu.Unlock()
	// A service dropped (revoked) while this session was being made is not served on it either.
	declared := n.declaredServices()
	for _, name := range served {
		if !slices.ContainsFunc(declared, func(x tunnel.Service) bool { return x.Name == name }) {
			s.proxy.Drop(name)
		}
	}

	ping := time.Duration(w.PingS) * time.Second
	if ping <= 0 || ping > tunnel.PingEvery {
		ping = tunnel.PingEvery // never less often than §11's 20 s, whatever the welcome says
	}
	go s.accept()
	go func() {
		if err := tunnel.KeepAlive(sctx, mux, ping, tunnel.PingFailures); err != nil {
			log.Warn("tunnel session closed: the gate is silent", "err", err)
		}
	}()
	go func() {
		<-mux.CloseChan()
		cancel()
		n.mu.Lock()
		delete(n.sessions, s)
		n.mu.Unlock()
	}()

	n.printWelcome(w, id)
	log.Info("tunnel session open", "tenant", w.Tenant, "services", s.proxy.Served())
	return s, nil
}

// printWelcome writes the welcome's services and their gate URLs (the CLI's stdout). The gate's text was
// checked by tunnel.ParseOutcome (a tenant and service names, http(s) URLs in printable ASCII), and prints
// through tunnel.Printable besides, so that nothing a gate writes reaches the terminal as an escape sequence
// (M2 review, security-5).
func (n *Node) printWelcome(w *tunnel.Welcome, id *nodekey.Identity) {
	p := tunnel.Printable
	fmt.Fprintf(n.out, "kontra connect: connected to %s as %s (tenant %s, session %s)\n", n.origin, id.KeyID(), p(w.Tenant), p(w.Session))
	if len(w.Services) == 0 {
		fmt.Fprintln(n.out, "  no service is served on this session")
	}
	for _, s := range w.Services {
		fmt.Fprintf(n.out, "  %s %s  %s\n", p(s.Kind), p(s.Name), p(s.URL))
	}
}

// retire keeps a session serving what the gate sends on it until the gate closes it, but never redials it.
// Run is then no longer watching it, so its life is recorded now (§13: a session that lived 60 s resets
// the backoff, whether it closes or is retired by a drain, a rotation or its stream ids; M2 review,
// tunnel-spec-3): a redial refused after the drain of a long session waits under BackoffMin, not on the
// ceiling left from failures before that session.
func (n *Node) retire(s *session, why string) {
	s.retired.Store(true)
	n.clearCurrent(s) // §13: no new hiring streams on a draining session
	n.backoff.SessionEnded(time.Since(s.opened))
	s.log.Info(why)
}

// closeAll closes every session (Run ends).
func (n *Node) closeAll() {
	n.mu.Lock()
	sessions := make([]*session, 0, len(n.sessions))
	for s := range n.sessions {
		sessions = append(sessions, s)
	}
	n.mu.Unlock()
	for _, s := range sessions {
		_ = s.mux.Close()
	}
	n.proxy.closeIdle()
}

// keyRevoked is {"t": "revoked"} on a session's control stream (§12): the node stops when it is the key
// the node now uses. A session of a rotated key, still serving its tasks in the 24 h overlap, ends alone:
// its key's overlap is over, and the new key's session goes on.
//
// The key the node uses is the key file's active key (activeRecipient), not the key of its last dial: another
// `kontra connect` may have rotated the shared file since, and this process sees it only at its next
// maintain tick, up to a minute later. A revocation of the old key in that window (rotate after a leak,
// then revoke the old key: D125, D128) closes the old key's session, which Run then replaces at once with a
// session of the new key (session.replaced), as maintain would have (M2 review, tunnel-spec-2).
func (n *Node) keyRevoked(s *session) {
	if s.key != n.activeRecipient() {
		s.log.Info("the gate revoked the session's rotated key: closing that session; the new key's goes on")
		s.replaced.Store(true)
		_ = s.mux.Close()
		return
	}
	s.log.Error("the gate revoked this node's key: kontra connect stops and does not redial (§12)")
	n.revokeOnce.Do(func() { close(n.revoked) })
}

// authRevoked is {"t": "auth_revoked"} on a session's control stream (§12, §13): the gate revoked the API key the
// session's hello carried, its hiring principal. The session's hiring requests are 401 from now on, and its
// hosting goes on. The key is dropped from this origin's credentials (when it is still the one stored) and so
// from every later hello: otherwise the next handshake (a network blip, a replica's restart, the make-before-break
// dial of a drain) would carry it and be refused auth_invalid, which §4 makes final, and kontra connect would stop
// hosting. Later sessions hire as node:<key_id> (§9); a key that only hires is refused until its operator gives a
// new --api-key (enroll.Admit).
func (n *Node) authRevoked(s *session) {
	s.log.Error("the gate revoked the session's hiring API key (auth_revoked): hiring requests on this session are refused 401 "+
		"and hosting goes on; the key is removed from the credentials file and no later hello carries it (spec/tunnel.md §12)",
		"path", n.creds.Path())
	if s.auth == "" {
		return
	}
	if err := n.updateCredential(func(c *Credential) {
		if c.APIKey == s.auth {
			c.APIKey = ""
		}
	}); err != nil {
		s.log.Error("the credentials file could not be updated", "err", err)
	}
}

// bindingRevoked is {"t": "revoked", "service"} on a session's control stream (§12): the gate revoked the
// binding of the session's key to service. When that key is the node's active key, the service is no longer
// served or declared (dropService). A binding is per key, and a rotation copied the old key's to the new one
// (D129), so a revocation that reaches a session of a rotated key, still serving out its 24 h overlap, is
// that key's alone: only that session stops serving the service, and the active key's sessions and the next
// hello keep it (M2 review, tunnel-spec-1).
func (n *Node) bindingRevoked(s *session, service string) {
	if s.key != n.activeRecipient() {
		s.proxy.Drop(service)
		s.log.Warn("the gate revoked the rotated key's binding to a service: that key's session no longer serves it; "+
			"the active key's sessions do", "service", service)
		return
	}
	n.dropService(service)
}

// activeRecipient is the recipient of the key file's active key, the key the next hello is made with; the
// key of the last dial when the file cannot be read.
func (n *Node) activeRecipient() string {
	id, err := nodekey.Load(n.cfg.KeyPath)
	if err == nil {
		return id.Recipient()
	}
	n.logger.Warn("the key file could not be read: taking the key of the last dial as the active key", "err", err)
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.activeKey
}

// maintain runs every maintainEvery: it prunes rotated keys hourly, and reports whether the key file's
// active key replaced the key cur was opened with and this gate has not confirmed that rotation (another
// `kontra connect` rotated the shared key file): a session of the new key must then be dialed, with
// rotate_from.
func (n *Node) maintain(cur *session) bool {
	now := n.now()
	if now.Sub(n.lastPrune) >= pruneEvery {
		n.prune(now)
	}
	id, err := nodekey.Load(n.cfg.KeyPath)
	if err != nil {
		n.logger.Warn("the key file could not be read", "err", err)
		return false
	}
	old, ok := id.PendingRotation(n.origin)
	return ok && cur != nil && !cur.retired.Load() && cur.key == old.Recipient().String()
}
