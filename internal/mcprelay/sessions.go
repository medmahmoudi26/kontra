package mcprelay

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

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

// IdleWrite is the most often last_seen_at is written for one session.
const IdleWrite = time.Minute

// Sessions is the mcp_sessions table.
type Sessions struct {
	pool *pgxpool.Pool
}

// NewSessions returns the store.
func NewSessions(pool *pgxpool.Pool) *Sessions { return &Sessions{pool: pool} }

// Open inserts a legacy session after its upstream initialize succeeded and returns the client id to send
// (128 random bits, base64url, 22 characters).
func (s *Sessions) Open(ctx context.Context, row Session) (clientID string, err error) {
	return "", unimpl.Error("mcprelay.Sessions.Open", "M3 WP-04")
}

// Lookup returns the open session a client id names, for this service and this principal only: an unknown or
// closed id, one bound to another principal, or one presented on another service's path is ErrNoSession, the
// client's 404 (mcp_session_cross_node).
func (s *Sessions) Lookup(ctx context.Context, clientID string, tenant uuid.UUID, service, principal string) (*Session, error) {
	return nil, unimpl.Error("mcprelay.Sessions.Lookup", "M3 WP-04")
}

// Touch writes last_seen_at at most once per IdleWrite.
func (s *Sessions) Touch(ctx context.Context, hash [32]byte, now time.Time) error {
	return unimpl.Error("mcprelay.Sessions.Touch", "M3 WP-04")
}

// Close closes a session with reason; closing a closed session changes nothing.
func (s *Sessions) Close(ctx context.Context, hash [32]byte, reason CloseReason) error {
	return unimpl.Error("mcprelay.Sessions.Close", "M3 WP-04")
}

// CloseTunnel closes every open session pinned to a tunnel session that closed for a reason other than a
// revocation (tunnel_lost), or to a key or binding that was revoked (revoked): spec/tunnel.md §11–§12.
func (s *Sessions) CloseTunnel(ctx context.Context, tunnel uuid.UUID, service string, reason CloseReason) (int, error) {
	return 0, unimpl.Error("mcprelay.Sessions.CloseTunnel", "M3 WP-04")
}

// SweepIdle closes the sessions idle for longer than their service's session_idle_s, returning them so the
// caller can send each upstream a best-effort DELETE.
func (s *Sessions) SweepIdle(ctx context.Context, now time.Time) ([]Session, error) {
	return nil, unimpl.Error("mcprelay.Sessions.SweepIdle", "M3 WP-04")
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
