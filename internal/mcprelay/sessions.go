package mcprelay

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"kontra.sh/kontra/internal/mcpwire"
	"kontra.sh/kontra/internal/unimpl"
)

// CloseReason is mcp_sessions.close_reason (spec/relay.md §11.2).
type CloseReason string

// Close reasons.
const (
	CloseDeleted        CloseReason = "deleted"
	CloseIdle           CloseReason = "idle"
	CloseTunnelLost     CloseReason = "tunnel_lost"
	CloseRevoked        CloseReason = "revoked"
	CloseUpstreamClosed CloseReason = "upstream_closed"
)

// ErrNoSession is a client id that names no open session of this service and principal: the client's 404
// (spec/relay.md §11.2, mcp_session_cross_node).
var ErrNoSession = errors.New("mcprelay: no such MCP session")

// Session is one mcp_sessions row. A legacy session pins a tunnel session (or a direct backend) and the
// upstream's Mcp-Session-Id; a modern client's virtual session (D160) pins nothing. ReplicaID is the replica
// holding the tunnel session, empty (NULL) for a direct backend and a virtual session, never the replica the
// client reached (D179): the reaper of a dead replica closes the sessions whose tunnels it held, and only those.
type Session struct {
	Hash            [32]byte // sha256 of the gate-minted client id; the client id itself is never stored
	TenantID        uuid.UUID
	Service         string
	BuyerID         uuid.UUID
	Principal       string
	Via             string
	UpstreamSID     string
	TunnelSession   *uuid.UUID
	ReplicaID       string
	ProtocolVersion string
	ClientCaps      []byte // the client's declared capabilities, JSON
	CreatedAt       time.Time
	LastSeenAt      time.Time
}

// HashHex is the session's hash as the tunnel preamble carries it (spec/tunnel.md §8.1: mcp_session).
func (s *Session) HashHex() string { return hex.EncodeToString(s.Hash[:]) }

// IdleWrite is the most often last_seen_at is written for one session.
const IdleWrite = time.Minute

// Sessions is the mcp_sessions table.
type Sessions struct {
	pool *pgxpool.Pool
}

// NewSessions returns the store.
func NewSessions(pool *pgxpool.Pool) *Sessions { return &Sessions{pool: pool} }

// MintID returns a new client id (128 random bits, base64url without padding: 22 characters, spec §11.2) and its
// SHA-256, which is all mcp_sessions keeps of it.
func MintID() (string, [32]byte, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", [32]byte{}, err
	}
	id := base64.RawURLEncoding.EncodeToString(b)
	return id, sha256.Sum256([]byte(id)), nil
}

// HashID is the SHA-256 of a client id.
func HashID(clientID string) [32]byte { return sha256.Sum256([]byte(clientID)) }

// Open inserts a legacy session after its upstream initialize succeeded, under the hash of the client id the
// caller minted (MintID) before forwarding the initialize, whose preamble already carried it (tunnel.md §8.1).
// row.Hash is ignored: the stored hash is the client id's.
func (s *Sessions) Open(ctx context.Context, clientID string, row Session) error {
	if len(clientID) != 22 {
		return fmt.Errorf("mcprelay: a client id has 22 characters, not %d", len(clientID))
	}
	h := HashID(clientID)
	caps := row.ClientCaps
	if len(caps) == 0 || !json.Valid(caps) {
		caps = []byte("{}")
	}
	var replica, upstreamSID *string
	if row.ReplicaID != "" {
		replica = &row.ReplicaID
	}
	if row.UpstreamSID != "" {
		upstreamSID = &row.UpstreamSID
	}
	_, err := s.pool.Exec(ctx, `insert into kontra.mcp_sessions (client_sid_hash, tenant_id, service, buyer_id, principal,
			via, upstream_sid, tunnel_session, replica_id, protocol_version, client_capabilities)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11::jsonb)`,
		h[:], row.TenantID, row.Service, row.BuyerID, row.Principal, row.Via, upstreamSID, row.TunnelSession, replica,
		row.ProtocolVersion, string(caps))
	if err != nil {
		return fmt.Errorf("mcprelay: open session: %w", err)
	}
	return nil
}

const sessionColumns = `client_sid_hash, tenant_id, service, buyer_id, principal, via, coalesce(upstream_sid, ''),
	tunnel_session, coalesce(replica_id, ''), protocol_version, client_capabilities::text, created_at, last_seen_at`

// sweptColumns is sessionColumns of the row an update of mcp_sessions m returns.
const sweptColumns = `m.client_sid_hash, m.tenant_id, m.service, m.buyer_id, m.principal, m.via, coalesce(m.upstream_sid, ''),
	m.tunnel_session, coalesce(m.replica_id, ''), m.protocol_version, m.client_capabilities::text, m.created_at, m.last_seen_at`

func scanSession(row pgx.Row) (*Session, error) {
	var (
		x       Session
		hash    []byte
		caps    string
		tenant  *uuid.UUID
		service *string
	)
	if err := row.Scan(&hash, &tenant, &service, &x.BuyerID, &x.Principal, &x.Via, &x.UpstreamSID, &x.TunnelSession,
		&x.ReplicaID, &x.ProtocolVersion, &caps, &x.CreatedAt, &x.LastSeenAt); err != nil {
		return nil, err
	}
	copy(x.Hash[:], hash)
	if tenant != nil {
		x.TenantID = *tenant
	}
	if service != nil {
		x.Service = *service
	}
	x.ClientCaps = []byte(caps)
	return &x, nil
}

// Lookup returns the open session a client id names, for this service and this principal only: an unknown or
// closed id, one bound to another principal, or one presented on another service's path is ErrNoSession, the
// client's 404 (mcp_session_cross_node). A virtual session (D160, 2026-07-28) is never named by a client id.
func (s *Sessions) Lookup(ctx context.Context, clientID string, tenant uuid.UUID, service, principal string) (*Session, error) {
	if clientID == "" || len(clientID) > 256 {
		return nil, ErrNoSession
	}
	h := HashID(clientID)
	x, err := scanSession(s.pool.QueryRow(ctx, `select `+sessionColumns+` from kontra.mcp_sessions
		 where client_sid_hash = $1 and closed_at is null`, h[:]))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, ErrNoSession
	case err != nil:
		return nil, fmt.Errorf("mcprelay: look up a session: %w", err)
	case x.TenantID != tenant || x.Service != service || x.Principal != principal || x.ProtocolVersion == mcpwire.V20260728:
		return nil, ErrNoSession
	}
	return x, nil
}

// Touch writes last_seen_at at most once per IdleWrite.
func (s *Sessions) Touch(ctx context.Context, hash [32]byte, now time.Time) error {
	_, err := s.pool.Exec(ctx, `update kontra.mcp_sessions set last_seen_at = $2::timestamptz
		 where client_sid_hash = $1 and closed_at is null and last_seen_at < $2::timestamptz - $3::bigint * interval '1 microsecond'`,
		hash[:], now, IdleWrite.Microseconds())
	if err != nil {
		return fmt.Errorf("mcprelay: touch a session: %w", err)
	}
	return nil
}

// Close closes a session with reason; closing a closed session changes nothing. It reports whether this call
// closed it.
func (s *Sessions) Close(ctx context.Context, hash [32]byte, reason CloseReason) (bool, error) {
	tag, err := s.pool.Exec(ctx, `update kontra.mcp_sessions set closed_at = now(), close_reason = $2
		 where client_sid_hash = $1 and closed_at is null`, hash[:], string(reason))
	if err != nil {
		return false, fmt.Errorf("mcprelay: close a session: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// CloseTunnel closes every open session pinned to a tunnel session that closed for a reason other than a
// revocation (tunnel_lost), or to a key or binding that was revoked (revoked): spec/tunnel.md §11–§12. With a
// service, only that service's sessions on the tunnel session (a binding revocation). It returns how many it
// closed. (The tunnel registry runs the same statement itself, tunnelgate's closeMCP, so that it needs no MCP
// package.)
func (s *Sessions) CloseTunnel(ctx context.Context, tunnel uuid.UUID, service string, reason CloseReason) (int, error) {
	tag, err := s.pool.Exec(ctx, `update kontra.mcp_sessions set closed_at = now(), close_reason = $3
		 where tunnel_session = $1 and closed_at is null and ($2 = '' or service = $2)`, tunnel, service, string(reason))
	if err != nil {
		return 0, fmt.Errorf("mcprelay: close the sessions of a tunnel session: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// SweepIdle closes the sessions idle for longer than their service's session_idle_s, returning them so the
// caller can send each upstream a best-effort DELETE. A replica sweeps the sessions whose tunnels it holds
// (replica_id) and those that pin no replica (direct backends, virtual sessions), which any replica reaches; one
// statement closes each once, whichever replica runs it first. A dead replica's sessions are the reaper's.
func (s *Sessions) SweepIdle(ctx context.Context, replicaID string, now time.Time) ([]Session, error) {
	rows, err := s.pool.Query(ctx, `update kontra.mcp_sessions m set closed_at = now(), close_reason = 'idle'
		  from kontra.services sv
		 where sv.tenant_id = m.tenant_id and sv.name = m.service and m.closed_at is null
		   and m.last_seen_at < $2::timestamptz - sv.session_idle_s * interval '1 second'
		   and (m.replica_id = $1 or m.replica_id is null)
		returning `+sweptColumns, replicaID, now)
	if err != nil {
		return nil, fmt.Errorf("mcprelay: sweep idle sessions: %w", err)
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		x, err := scanSession(rows)
		if err != nil {
			return nil, fmt.Errorf("mcprelay: sweep idle sessions: %w", err)
		}
		out = append(out, *x)
	}
	return out, rows.Err()
}

// PeerAddr is the peer listener address a replica advertises (gate_replicas.peer_addr), and whether the replica
// is live (seen within the last 30 s, spec/tunnel.md §11). A draining replica is live: it serves the streams of
// its sessions until its drain ends. "" when it advertises none.
func (s *Sessions) PeerAddr(ctx context.Context, replicaID string) (addr string, live bool, err error) {
	err = s.pool.QueryRow(ctx, `select coalesce(peer_addr, ''), last_seen_at > now() - interval '30 seconds'
		 from kontra.gate_replicas where replica_id = $1`, replicaID).Scan(&addr, &live)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("mcprelay: the peer address of replica %s: %w", replicaID, err)
	}
	return addr, live, nil
}

// Virtual returns the open virtual session of a principal at a service (D160, D177). The caller runs
// Admission's session decision before forwarding a principal's first modern request, and calls Virtual once the
// upstream has answered it with a modern result, so a probe that ends in the legacy 400 opens nothing. One open
// row per (tenant, service, principal) is enforced by migration 0002's partial unique index (protocol_version
// 2026-07-28, closed_at null): the insert is on conflict do nothing, then the row is read, so two first requests
// in flight at once share one session.
func (s *Sessions) Virtual(ctx context.Context, tenant uuid.UUID, service, principal string, open func(context.Context) error) (*Session, error) {
	return nil, unimpl.Error("mcprelay.Sessions.Virtual", "M3 WP-07")
}
