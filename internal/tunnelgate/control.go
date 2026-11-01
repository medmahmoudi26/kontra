package tunnelgate

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/google/uuid"

	"kontra.sh/kontra/internal/anon"
	"kontra.sh/kontra/internal/config"
	"kontra.sh/kontra/internal/enroll"
	"kontra.sh/kontra/internal/obs"
	"kontra.sh/kontra/internal/tunnel"
)

// sessionPrefix starts NOTIFY kontra_tunnel's (pg.ChannelTunnel) payload for a superseded session, mcpPrefix
// its payload for a change in the nodes serving an MCP service; the enrollment's is enroll.EnrolledPayload. All
// are gate-internal: see Registry.
const (
	sessionPrefix = "session:"
	mcpPrefix     = "mcp:"
)

// SessionPayload is NOTIFY kontra_tunnel's payload for a superseded session, "session:<session_id>": the
// replica holding it closes it (its row is already deleted).
func SessionPayload(id uuid.UUID) string { return sessionPrefix + id.String() }

// MCPChangedPayload is NOTIFY kontra_tunnel's payload for a session serving an MCP service that became a candidate
// on replica, or was removed there: "mcp:<tenant_id>/<service>/<replica_id>". Every other replica runs its
// OnMCPChange callbacks for the service (the MCP relay drops the service's era, relay.md §11.12, D177: through the
// peer hop every replica uses the nodes another one holds); the sending replica ran them already.
func MCPChangedPayload(tenantID uuid.UUID, service, replica string) string {
	return mcpPrefix + tenantID.String() + "/" + service + "/" + replica
}

// parseMCPChanged reads an MCPChangedPayload; ok is false for anything else, a malformed one included.
func parseMCPChanged(payload string) (tenantID uuid.UUID, service, replica string, ok bool) {
	rest, ok := strings.CutPrefix(payload, mcpPrefix)
	if !ok {
		return uuid.Nil, "", "", false
	}
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) != 3 || !config.NameRE.MatchString(parts[1]) || parts[2] == "" {
		return uuid.Nil, "", "", false
	}
	tid, err := uuid.Parse(parts[0])
	if err != nil {
		return uuid.Nil, "", "", false
	}
	return tid, parts[1], parts[2], true
}

// Revoked applies one revocation (enroll.Revocations.Subscribe), §12. It runs on the goroutine that learned
// the revocation and does not wait on the network or the database: what does is done on goroutines of its
// own, an aborted stream's FIN included (exchange.abort).
//
//   - A key: its sessions take no new stream and their streams are aborted at once; then each is told
//     {"t": "revoked"} and closed (its dispatches fail tunnel_lost), and its open mcp_sessions rows are
//     closed revoked. The close keeps the connection until the node has read the line, within a bound
//     (Session.closeTold). A rotated key whose overlap ended (revoked_reason rotated, §12: "revoked_at is set when
//     the overlap ends") is not an operator's revocation: its session closes without the message, which
//     would end a node that has moved to its new key.
//   - A binding: the service leaves the session's served set at once and its streams on the session are
//     aborted (an aborted stream reads as a lost session: tunnel_lost, or a retry before the first upstream
//     id; M2 plan audit), then tunnels.services drops it, the open mcp_sessions rows of (session, service)
//     are closed revoked, and the node is told {"t": "revoked", "service"}. The gate never relies on the
//     node honouring it: Allows refuses every later stream for the service.
func (r *Registry) Revoked(rev enroll.Revocation) {
	for _, s := range r.all() {
		if s.KeyID != rev.KeyID {
			continue
		}
		if rev.Service == "" {
			r.revokeKey(s, rev.Reason)
		} else {
			r.revokeBinding(s, rev.Service)
		}
	}
}

func (r *Registry) revokeKey(s *Session, reason string) {
	why, tell := "its key was revoked (§12)", true
	if reason == enroll.ReasonRotated {
		why, tell = "its key's rotation overlap ended (§12)", false
	}
	// Either way a pinned MCP session closes revoked (relay.md §6.1: "the key or binding is revoked or the key is
	// past its rotation overlap"), and a stream refused from now on says so (ErrRevoked).
	s.markRevoked()
	if !s.shut(why) {
		return
	}
	log := r.log.With(obs.KeyTunnelSession, s.ID, "key_id", s.KeyID)
	log.Info("tunnel session closed", "reason", why)
	r.goAsync(func() {
		if tell {
			if err := s.WriteControl(tunnel.Control{T: tunnel.CtlRevoked}); err != nil {
				log.Debug("tunnel session: {t: revoked} not delivered", "error", err)
			}
		}
		if s.hostedMCP() {
			r.closeMCP(s.ID, "", mcpRevoked)
		}
		if tell {
			_ = s.closeTold(why, false) // the node reads the line before the connection ends
			return
		}
		_ = s.close(why, false)
	})
}

func (r *Registry) revokeBinding(s *Session, service string) {
	if s.anonymousEnded() {
		// An anonymous session's bindings are revoked at its end (anon.End): the session ends with {"t":
		// "revoked"}, not one {"t": "revoked", "service"} per binding (spec/tunnel.md §6.1).
		r.goAsync(s.Expire)
		return
	}
	why := fmt.Errorf("%w: the binding of key %s to %s was revoked (§12)", ErrRevoked, s.KeyID, service)
	if !s.dropService(service, why) {
		return
	}
	r.goAsync(func() {
		log := r.log.With(obs.KeyTunnelSession, s.ID, "key_id", s.KeyID, "service", service)
		log.Info("tunnel session: binding revoked; the service is no longer served on it")
		if r.cfg.DB != nil {
			ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
			if _, err := r.cfg.DB.Exec(ctx, `update kontra.tunnels set services = array_remove(services, $2) where session_id = $1`,
				s.ID, service); err != nil {
				// The candidate query also joins key_services, so the row's stale services selects nothing.
				log.Warn("tunnels.services not updated after a binding revocation", "error", err)
			}
			cancel()
		}
		r.closeMCP(s.ID, service, mcpRevoked)
		if err := s.WriteControl(tunnel.Control{T: tunnel.CtlRevoked, Service: service}); err != nil {
			log.Debug("tunnel session: {t: revoked, service} not delivered", "error", err)
		}
	})
}

// APIKeyRevoked applies an API key revocation (auth.RevocationCache.Subscribe: NOTIFY kontra_revoke
// 'apikey:<id>', or the re-read that found it), §12: every session of this replica hiring with that key gets 401
// on each later stream it opens, and is told {"t": "auth_revoked"} on its control stream. Hosting on the
// sessions is unaffected. It does not wait on the network (the message goes out on a goroutine of its own).
func (r *Registry) APIKeyRevoked(id uuid.UUID) {
	for _, s := range r.all() {
		if s.hire != nil && s.hire.KeyID != nil && *s.hire.KeyID == id {
			s.authRevoked(r.log)
		}
	}
}

// mcp_sessions.close_reason values the registry writes (relay.md §11.2).
const (
	mcpRevoked    = "revoked"
	mcpTunnelLost = "tunnel_lost"
)

// closeMCP closes the open mcp_sessions rows pinned to a session (and service, when not empty) with reason:
// revoked on a key or binding revocation (§12, before the node is told), tunnel_lost when the session closes for
// any other reason (relay.md §11.2: a pinned session dies with its tunnel). Closing a closed row changes nothing,
// so the revocation's reason stands when the session's own close follows it.
func (r *Registry) closeMCP(session uuid.UUID, service, reason string) {
	if r.cfg.DB == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()
	if _, err := r.cfg.DB.Exec(ctx, `update kontra.mcp_sessions set closed_at = now(), close_reason = $3
		where tunnel_session = $1 and closed_at is null and ($2 = '' or service = $2)`, session, service, reason); err != nil {
		r.log.Warn("mcp_sessions pinned to a tunnel session not closed", obs.KeyTunnelSession, session, "service", service,
			"reason", reason, "error", err)
	}
}

// Enrolled tells this replica's sessions hosting the binding's service that another key was enrolled for
// it: {"t": "enrolled", "service", "key"} (§5). The enrolled key's own sessions are not told.
func (r *Registry) Enrolled(b enroll.Binding) {
	for _, s := range r.all() {
		if s.TenantID != b.TenantID || s.KeyID == b.KeyID || !s.open() || !s.Serves(b.Service) {
			continue
		}
		r.goAsync(func() {
			if err := s.WriteControl(tunnel.Control{T: tunnel.CtlEnrolled, Service: b.Service, Key: b.KeyID}); err != nil {
				r.log.Debug("tunnel session: {t: enrolled} not delivered", obs.KeyTunnelSession, s.ID, "error", err)
			}
		})
	}
}

// ExpireTenant ends this replica's sessions of an anonymous tenant that ended (anon.ExpiredPayload: its expiry, or a
// conversion): each is told {"t": "revoked"} and closed (Session.Expire), its key not revoked (spec/tunnel.md §6.1).
// It does not wait on the network.
func (r *Registry) ExpireTenant(tenantID uuid.UUID) {
	for _, s := range r.all() {
		if s.TenantID == tenantID {
			s.ended.Store(true) // before any of the tenant's binding revocations is applied (revokeBinding)
			r.goAsync(s.Expire)
		}
	}
}

// Notify applies one gate-internal NOTIFY kontra_tunnel (pg.Listener.OnChannel): 'session:<id>', sent by
// the replica whose handshake superseded a session held here, closes it (its row is gone already);
// 'enrolled:<key_id>/<tenant_id>/<service>' (enroll.EnrolledPayload), sent by the admission that committed
// the binding, is told to this replica's sessions hosting the service (§5), on every replica, the admitting
// one included; 'mcp:<tenant_id>/<service>/<replica_id>' (MCPChangedPayload), sent by a replica where a node of
// the MCP service connected or disconnected, runs the OnMCPChange callbacks here, unless this replica sent it (it
// ran them when the change happened); 'expired:<tenant_id>' (anon.ExpiredPayload), sent by the transaction that ended
// an anonymous tenant, ends this replica's sessions of it (ExpireTenant). Anything else is ignored. It never waits on
// the database (pg.Listener runs every channel's callbacks on one goroutine).
//
// SPEC-CONFLICT (M2 plan, confirmed conflict on §11 and §5): see Registry; this channel is not tunnel wire
// protocol.
func (r *Registry) Notify(payload string) {
	switch {
	case strings.HasPrefix(payload, sessionPrefix):
		id, err := uuid.Parse(strings.TrimPrefix(payload, sessionPrefix))
		if err != nil {
			return
		}
		if s := r.lookup(id); s != nil {
			s.mu.Lock()
			s.rowGone = true // the superseding transaction deleted it
			s.mu.Unlock()
			r.goAsync(func() { _ = s.Close("superseded by a newer session of its key on another replica (§11)") })
		}
	case strings.HasPrefix(payload, mcpPrefix):
		if tenantID, service, from, ok := parseMCPChanged(payload); ok && from != r.cfg.ReplicaID {
			r.runMCP(tenantID, []string{service})
		}
	case strings.HasPrefix(payload, "expired:"):
		if tenantID, ok := anon.ParseExpiredPayload(payload); ok {
			r.ExpireTenant(tenantID)
		}
	default:
		if b, ok := enroll.ParseEnrolledPayload(payload); ok {
			r.Enrolled(b)
		}
	}
}

// Drain sends {"t": "drain"} on every session's control stream (§13, D35): the nodes dial new sessions at
// once and keep serving the streams on these, which close when CloseAll runs at the end of the drain. A
// session added afterwards is told as soon as it is registered. It returns when every message was written
// or ctx ends.
//
// The gate calls it once gate_replicas.draining_at has committed (M2 plan audit): a node dialing another
// replica then supersedes nothing here (Add keeps a draining replica's sessions), and the tasks on these
// sessions run to their end within drain_timeout.
func (r *Registry) Drain(ctx context.Context) {
	r.mu.Lock()
	r.drained = true
	all := make([]*Session, 0, len(r.sessions))
	for _, s := range r.sessions {
		all = append(all, s)
	}
	r.mu.Unlock()
	var wg sync.WaitGroup
	for _, s := range all {
		wg.Go(func() { r.tellDrain(s) })
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// tellDrain writes {"t": "drain"} on a session's control stream, once.
func (r *Registry) tellDrain(s *Session) {
	s.mu.Lock()
	told := s.toldDrain || s.closed
	s.toldDrain = true
	s.mu.Unlock()
	if told {
		return
	}
	if err := s.WriteControl(tunnel.Control{T: tunnel.CtlDrain}); err != nil {
		r.log.Debug("tunnel session: {t: drain} not delivered", obs.KeyTunnelSession, s.ID, "error", err)
	}
}

// goAsync runs fn on its own goroutine, counted for CloseAll.
func (r *Registry) goAsync(fn func()) {
	r.async.Go(fn)
}
