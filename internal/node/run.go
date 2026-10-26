package node

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
// side, and only the gate opens streams in M2, so the node watches the ids it accepts and dials a new
// session before the gate's run out).
//
// It returns nil when ctx ends, ErrRevoked when the gate revokes the key, a *tunnel.HandshakeError for a
// refusal that is not retried, ErrChannelBindingRequired, or an error reading the key file.
func (n *Node) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer n.closeAll()
	tick := time.NewTicker(n.maintainEvery)
	defer tick.Stop()

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

		// Serve the current session until it closes, or a new one must be dialed while it goes on.
	watch:
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-n.revoked:
				return ErrRevoked
			case <-s.mux.CloseChan():
				lived := time.Since(s.opened)
				n.backoff.SessionEnded(lived)
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
// same hello with the active key and neither tokens nor rotate_from. Its welcome means the change committed
// (the tokens are recorded consumed, the rotation confirmed with nodekey.MarkRotated); unknown_key (a new
// or rotated key the gate does not know), or service_denied (a known key declaring the service the tokens
// were to bind), means it did not, and the full hello is sent again at once. A consumed token is never
// sent again, not even when a restart passes the same --enroll. The probe's service_denied is taken as
// "not committed" although §4 does not retry service_denied: the full hello it is followed by differs, and
// its own refusal is final. The owner's alternative is a gate that accepts a replayed rotation or token as
// a reconnect (idempotent admission).
func (n *Node) connect(ctx context.Context) (*session, next, error) {
	id, err := nodekey.Load(n.cfg.KeyPath)
	if err != nil {
		return nil, nextFatal, fmt.Errorf("node: the identity key: %w", err)
	}
	n.mu.Lock()
	n.activeKey = id.Recipient()
	probe := n.unsure
	n.mu.Unlock()

	h := &tunnel.Hello{Name: n.cfg.Name, Agent: UserAgent(n.cfg.Version), Services: n.declaredServices(), Auth: n.credential().APIKey}
	var rotateOld string
	if !probe {
		h.Enroll = n.credential().Enroll
		if old, ok := id.PendingRotation(n.origin); ok {
			rotateOld = old.Recipient().String()
			h.RotateFrom = &tunnel.KeyRef{KID: tunnel.KeyID(rotateOld), Recipient: rotateOld}
		}
	}
	sent := slices.Clone(h.Enroll)
	log := n.logger.With("key_id", id.KeyID())
	if probe {
		log.Info("dialing with the active key alone: the outcome of an earlier hello that carried enrollment " +
			"tokens or rotate_from is unknown")
	}
	if len(sent) > 0 {
		// Before anything is sent: a stop after the proof leaves the probe for the next start.
		if err := n.updateCredential(func(c *Credential) { c.Unconfirmed = hashes(sent) }); err != nil {
			return nil, nextFatal, err
		}
	}

	d, err := n.dialer.Dial(ctx, n.cfg.Gate)
	if err != nil {
		n.settled(sent, probe)
		log.Warn("the gate could not be dialed", "err", err)
		return nil, nextRetry, err
	}
	w, err := Handshake(ctx, d, id, h, n.cfg.RequireChannelBinding)
	var herr *tunnel.HandshakeError
	switch {
	case err == nil:
		if err := n.welcomed(id, h, probe, rotateOld, log); err != nil {
			log.Error("the credentials file could not be updated after the welcome", "err", err)
		}
		if probe && len(n.credential().Enroll) > 0 {
			// Tokens given since the hello whose outcome was lost: they were not in the probe, so this session
			// does not serve what they enroll. Dial again at once with them (this session is seconds old).
			_ = d.WS.Close(websocket.StatusNormalClosure, "")
			log.Info("the earlier hello was admitted; dialing again with the enrollment tokens given since")
			return nil, nextAgain, nil
		}
		s, err := n.startSession(ctx, d, w, id)
		if err != nil {
			log.Warn("the tunnel session did not start", "err", err)
			return nil, nextRetry, err
		}
		return s, nextSession, nil

	case errors.As(err, &herr):
		if probe && (herr.Code == tunnel.CodeUnknownKey || herr.Code == tunnel.CodeServiceDenied) {
			// Nothing committed: the tokens are still unused (the full hello marks them unconfirmed again).
			n.setUnsure(false)
			if len(n.credential().Unconfirmed) > 0 {
				if err := n.updateCredential(func(c *Credential) { c.Unconfirmed = nil }); err != nil {
					log.Error("the credentials file could not be updated", "err", err)
				}
			}
			log.Info("the earlier hello was not admitted: sending it again", "code", herr.Code)
			return nil, nextAgain, nil
		}
		n.settled(sent, probe)
		n.refused(herr, h, sent, log)
		if !herr.Code.Retryable() {
			log.Error("the gate refused the handshake; kontra connect does not retry this without a change by its operator",
				"code", herr.Code, "message", herr.Message)
			return nil, nextFatal, err
		}
		if herr.Code == tunnel.CodeUnknownKey {
			log.Error("the gate does not know this node's key: enroll it with --enroll <token> (kontra-gate enroll <service>)",
				"message", herr.Message)
		} else {
			log.Warn("the gate refused the handshake; retrying", "code", herr.Code, "message", herr.Message)
		}
		return nil, nextRetry, err

	case errors.Is(err, ErrChannelBindingRequired):
		n.settled(sent, probe)
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
		n.settled(sent, probe)
		log.Warn("the handshake failed", "err", err)
		return nil, nextRetry, err
	}
}

// welcomed records what a welcome confirms: the hello's tokens consumed (a probe's: the earlier hello's,
// Unconfirmed), and the rotation committed at this gate (MarkRotated, from which Prune counts 24 h).
func (n *Node) welcomed(id *nodekey.Identity, h *tunnel.Hello, probe bool, rotateOld string, log *slog.Logger) error {
	n.setUnsure(false)
	var confirmed []string
	cur := n.credential()
	if probe {
		confirmed = cur.Unconfirmed
	} else {
		confirmed = hashes(h.Enroll)
	}
	var err error
	if len(confirmed) > 0 || len(cur.Unconfirmed) > 0 {
		err = n.updateCredential(func(c *Credential) {
			for _, x := range confirmed {
				if !slices.Contains(c.Consumed, x) {
					c.Consumed = append(c.Consumed, x)
				}
			}
			c.Enroll = slices.DeleteFunc(c.Enroll, func(t string) bool { return slices.Contains(confirmed, TokenHash(t)) })
			c.Unconfirmed = nil
		})
		if len(confirmed) > 0 {
			log.Info("the gate confirmed the enrollment: its tokens are not sent again", "enrollments", len(confirmed))
		}
	}
	if rotateOld == "" && probe {
		if old, ok := id.PendingRotation(n.origin); ok {
			rotateOld = old.Recipient().String()
		}
	}
	if rotateOld != "" {
		if merr := nodekey.MarkRotated(n.cfg.KeyPath, rotateOld, n.origin, n.now()); merr != nil {
			err = errors.Join(err, merr)
		} else {
			log.Info("the gate confirmed the key's rotation: the old key is kept, decrypt-only, for 24 h (§12)",
				"old_key_id", tunnel.KeyID(rotateOld))
		}
	}
	return err
}

// settled records that a hello's outcome is known and was not a welcome: its tokens are no longer
// unconfirmed. A probe carries no tokens, and leaves the earlier hello's in place.
func (n *Node) settled(sent []string, probe bool) {
	if probe || len(sent) == 0 {
		return
	}
	if err := n.updateCredential(func(c *Credential) { c.Unconfirmed = nil }); err != nil {
		n.logger.Error("the credentials file could not be updated", "err", err)
	}
}

// refused drops from the credentials file what the gate refused as invalid, so that it is not sent again:
// the tokens of a hello refused enroll_invalid (unknown, used or expired; a hello with rotate_from is not
// told apart from its rotation's refusal, so its tokens are kept), and the API key of one refused
// auth_invalid. The operator gives new ones (§4: not retried without a change by its operator).
func (n *Node) refused(herr *tunnel.HandshakeError, h *tunnel.Hello, sent []string, log *slog.Logger) {
	var err error
	switch {
	case herr.Code == tunnel.CodeEnrollInvalid && len(sent) > 0 && h.RotateFrom == nil:
		err = n.updateCredential(func(c *Credential) {
			c.Enroll = slices.DeleteFunc(c.Enroll, func(t string) bool { return slices.Contains(sent, t) })
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

// updateCredential changes this origin's credential and saves it.
func (n *Node) updateCredential(f func(*Credential)) error {
	n.mu.Lock()
	c := n.cred.clone()
	f(&c)
	n.cred = c
	n.mu.Unlock()
	return n.creds.Save(n.origin, c)
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
// ping loop and control stream reader; it prints the welcome's service URLs.
func (n *Node) startSession(ctx context.Context, d *Dialed, w *tunnel.Welcome, id *nodekey.Identity) (*session, error) {
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
	}
	s.proxy = n.proxy.forSession(served, log)
	s.proxy.onControl = s.readControl
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

// printWelcome writes the welcome's services and their gate URLs (the CLI's stdout).
func (n *Node) printWelcome(w *tunnel.Welcome, id *nodekey.Identity) {
	fmt.Fprintf(n.out, "kontra connect: connected to %s as %s (tenant %s, session %s)\n", n.origin, id.KeyID(), w.Tenant, w.Session)
	if len(w.Services) == 0 {
		fmt.Fprintln(n.out, "  no service is served on this session")
	}
	for _, s := range w.Services {
		fmt.Fprintf(n.out, "  %s %s  %s\n", s.Kind, s.Name, s.URL)
	}
}

// retire keeps a session serving what the gate sends on it until the gate closes it, but never redials it.
func (n *Node) retire(s *session, why string) {
	s.retired.Store(true)
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
func (n *Node) keyRevoked(s *session) {
	n.mu.Lock()
	active := n.activeKey
	n.mu.Unlock()
	if s.key != active {
		s.log.Info("the gate revoked the session's rotated key: closing that session; the new key's goes on")
		_ = s.mux.Close()
		return
	}
	s.log.Error("the gate revoked this node's key: kontra connect stops and does not redial (§12)")
	n.revokeOnce.Do(func() { close(n.revoked) })
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
