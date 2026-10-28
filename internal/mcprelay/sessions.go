package mcprelay

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"kontra.sh/kontra/internal/mcpwire"
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

// IdleWrite is the most often last_seen_at is written for one session; while a request or a GET stream of a legacy
// session is open on a replica, that replica writes it at least every IdleWrite and a half (D231: Handler.hold). A
// variable for tests.
var IdleWrite = time.Minute

// EvictIdle is how old a session's last_seen_at must be before an initialize beyond max_sessions_per_principal may
// close it (D231): the stdio bridge's own bound on stopping an idle child (stdiobridge.EvictIdle). A session with a
// request or a stream in flight is never that old (IdleWrite). A variable for tests.
var EvictIdle = 5 * time.Minute

// ErrTooManySessions is a new session beyond max_sessions_per_principal when too few of the principal's sessions are
// idle for EvictIdle to make room: it is refused, and none is closed (D231).
var ErrTooManySessions = errors.New("mcprelay: the principal's sessions of the service are at max_sessions_per_principal and in use")

// Sessions is the mcp_sessions table.
type Sessions struct {
	pool      *pgxpool.Pool
	idleWrite time.Duration // IdleWrite when the store was made (Touch runs on goroutines of their own too)
}

// NewSessions returns the store.
func NewSessions(pool *pgxpool.Pool) *Sessions { return &Sessions{pool: pool, idleWrite: IdleWrite} }

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
	return insertSession(ctx, s.pool, clientID, row)
}

// execer is the pool or a transaction.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func insertSession(ctx context.Context, db execer, clientID string, row Session) error {
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
	_, err := db.Exec(ctx, `insert into kontra.mcp_sessions (client_sid_hash, tenant_id, service, buyer_id, principal,
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
		hash[:], now, s.idleWrite.Microseconds())
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

// Capacity is what max_sessions_per_principal's check of one new legacy session needs (§11.2, D213, D231).
type Capacity struct {
	Limit int        // max_sessions_per_principal, at least 1
	Busy  [][32]byte // the sessions with a request or a stream in flight on this replica: never closed
	Now   time.Time
}

// Evict makes room for one more open legacy session of a principal on a tunnel service within c.Limit (§11.2, D213,
// D231). The principal's open sessions pinned to a tunnel are counted (a direct backend's and a 2026-07-28 virtual
// session are not); when they leave no room, as many as needed of those whose last_seen_at is at least EvictIdle old
// and that are not in c.Busy are closed with reason idle, the oldest last_seen_at first, and returned so the caller
// can send each upstream a DELETE. When too few are, none is closed and the answer is ErrTooManySessions. The check
// and the close are one transaction under an advisory lock of the (tenant, service, principal), so concurrent
// initializes of one principal decide one after the other.
func (s *Sessions) Evict(ctx context.Context, tenant uuid.UUID, service, principal string, c Capacity) ([]Session, error) {
	return s.capped(ctx, tenant, service, principal, c, nil)
}

// OpenCapped is Open under max_sessions_per_principal: Evict's check, its closes and the insert of the new row in one
// transaction, so that concurrent initializes of one principal leave at most c.Limit open; the one that finds no room
// writes nothing and is ErrTooManySessions.
func (s *Sessions) OpenCapped(ctx context.Context, clientID string, row Session, c Capacity) ([]Session, error) {
	return s.capped(ctx, row.TenantID, row.Service, row.Principal, c, func(tx pgx.Tx) error {
		return insertSession(ctx, tx, clientID, row)
	})
}

func (s *Sessions) capped(ctx context.Context, tenant uuid.UUID, service, principal string, c Capacity, insert func(pgx.Tx) error) ([]Session, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("mcprelay: max_sessions_per_principal: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock($1)`, capLock(tenant, service, principal)); err != nil {
		return nil, fmt.Errorf("mcprelay: max_sessions_per_principal: %w", err)
	}
	rows, err := tx.Query(ctx, `select client_sid_hash, last_seen_at <= $5::timestamptz - $6::bigint * interval '1 microsecond'
		  from kontra.mcp_sessions
		 where tenant_id = $1 and service = $2 and principal = $3 and closed_at is null and protocol_version <> $4
		   and tunnel_session is not null
		 order by last_seen_at, created_at`, tenant, service, principal, mcpwire.V20260728, c.Now, EvictIdle.Microseconds())
	if err != nil {
		return nil, fmt.Errorf("mcprelay: max_sessions_per_principal: %w", err)
	}
	busy := make(map[[32]byte]bool, len(c.Busy))
	for _, h := range c.Busy {
		busy[h] = true
	}
	open := 0
	var idle [][]byte
	for rows.Next() {
		var (
			h      []byte
			isIdle bool
		)
		if err := rows.Scan(&h, &isIdle); err != nil {
			rows.Close()
			return nil, fmt.Errorf("mcprelay: max_sessions_per_principal: %w", err)
		}
		open++
		if isIdle && len(h) == 32 && !busy[[32]byte(h)] {
			idle = append(idle, h)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mcprelay: max_sessions_per_principal: %w", err)
	}
	need := open + 1 - max(c.Limit, 1)
	if need > len(idle) {
		return nil, ErrTooManySessions
	}
	var closed []Session
	if need > 0 {
		rows, err := tx.Query(ctx, `update kontra.mcp_sessions m set closed_at = now(), close_reason = 'idle'
			 where m.client_sid_hash = any($1) and m.closed_at is null
			returning `+sweptColumns, idle[:need])
		if err != nil {
			return nil, fmt.Errorf("mcprelay: evict sessions: %w", err)
		}
		for rows.Next() {
			x, err := scanSession(rows)
			if err != nil {
				rows.Close()
				return nil, fmt.Errorf("mcprelay: evict sessions: %w", err)
			}
			closed = append(closed, *x)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("mcprelay: evict sessions: %w", err)
		}
	}
	if insert != nil {
		if err := insert(tx); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("mcprelay: max_sessions_per_principal: %w", err)
	}
	return closed, nil
}

// capLock is the transaction-scoped advisory lock under which one principal's sessions of one service are counted
// against max_sessions_per_principal (D231), derived like push's per-buyer lock.
func capLock(tenant uuid.UUID, service, principal string) int64 {
	b := append([]byte("kontra.mcp_sessions\x00"), tenant[:]...)
	b = append(append(append(b, service...), 0), principal...)
	sum := sha256.Sum256(b)
	return int64(binary.BigEndian.Uint64(sum[:8]))
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

// Virtual opens, or returns the open, virtual session of a principal at a service (D160, D177): an mcp_sessions row
// with protocol_version 2026-07-28, a client_sid_hash over a random id the gate never sends, and no upstream, tunnel
// or replica ids (it pins nothing; any replica's idle sweep closes it). The caller runs Admission's session decision
// before forwarding a principal's first modern request, and calls Virtual once the upstream has answered it with a
// modern result, so a probe that ends in the legacy 400 opens nothing. One open row per (tenant, service,
// principal) is enforced by migration 0002's partial unique index (mcp_sessions_virtual: protocol_version
// 2026-07-28, closed_at null, nulls not distinct): the insert is on conflict do nothing, then the open row is read,
// so two first requests in flight at once, on one replica or two, share one session. row gives the tenant,
// service, buyer, principal, via and the client's capabilities; its other fields are ignored.
func (s *Sessions) Virtual(ctx context.Context, row Session) (*Session, error) {
	_, h, err := MintID()
	if err != nil {
		return nil, err
	}
	caps := row.ClientCaps
	if len(caps) == 0 || !json.Valid(caps) {
		caps = []byte("{}")
	}
	for range 3 {
		if _, err := s.pool.Exec(ctx, `insert into kontra.mcp_sessions (client_sid_hash, tenant_id, service, buyer_id, principal,
				via, protocol_version, client_capabilities)
			values ($1, $2, $3, $4, $5, $6, $7, $8::jsonb)
			on conflict (tenant_id, service, principal) where protocol_version = '2026-07-28' and closed_at is null do nothing`,
			h[:], row.TenantID, row.Service, row.BuyerID, row.Principal, row.Via, mcpwire.V20260728, string(caps)); err != nil {
			return nil, fmt.Errorf("mcprelay: open a virtual session: %w", err)
		}
		x, err := s.OpenVirtual(ctx, row.TenantID, row.Service, row.Principal, time.Time{})
		if !errors.Is(err, ErrNoSession) {
			return x, err
		}
		// The winner's row was closed (idle) between the conflict and the read: insert again.
	}
	return nil, fmt.Errorf("mcprelay: open a virtual session: %w", ErrNoSession)
}

// OpenVirtual returns the open virtual session of a principal at a service (D160), ErrNoSession when there is none,
// and writes its last_seen_at when now is at least IdleWrite past it (a zero now writes nothing).
func (s *Sessions) OpenVirtual(ctx context.Context, tenant uuid.UUID, service, principal string, now time.Time) (*Session, error) {
	x, err := scanSession(s.pool.QueryRow(ctx, `select `+sessionColumns+` from kontra.mcp_sessions
		 where tenant_id = $1 and service = $2 and principal = $3 and protocol_version = $4 and closed_at is null`,
		tenant, service, principal, mcpwire.V20260728))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, ErrNoSession
	case err != nil:
		return nil, fmt.Errorf("mcprelay: read a virtual session: %w", err)
	}
	if !now.IsZero() && now.Sub(x.LastSeenAt) >= IdleWrite {
		if err := s.Touch(ctx, x.Hash, now); err != nil {
			return nil, err
		}
	}
	return x, nil
}
