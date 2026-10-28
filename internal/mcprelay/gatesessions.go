package mcprelay

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// OpenGate inserts a legacy session of the gate's own /mcp (spec/relay.md §12, DM4-2): an mcp_sessions row whose
// service is NULL and whose tenant_id is the principal's (NULL when it has none), pinning nothing (no upstream, no
// tunnel session, no replica), so any replica serves any request of it. Lookup finds it with service "" and the
// principal's tenant (uuid.Nil for none). row gives the buyer, principal, via, version and the client's
// capabilities; its other fields are ignored. (M4 WP-04: a file of its own, so M3's sessions.go is unchanged.)
func (s *Sessions) OpenGate(ctx context.Context, clientID string, row Session) error {
	if len(clientID) != 22 {
		return fmt.Errorf("mcprelay: a client id has 22 characters, not %d", len(clientID))
	}
	h := HashID(clientID)
	caps := row.ClientCaps
	if len(caps) == 0 || !json.Valid(caps) {
		caps = []byte("{}")
	}
	var tenant *uuid.UUID
	if row.TenantID != uuid.Nil {
		t := row.TenantID
		tenant = &t
	}
	_, err := s.pool.Exec(ctx, `insert into kontra.mcp_sessions (client_sid_hash, tenant_id, service, buyer_id, principal,
			via, protocol_version, client_capabilities)
		values ($1, $2, null, $3, $4, $5, $6, $7::jsonb)`,
		h[:], tenant, row.BuyerID, row.Principal, row.Via, row.ProtocolVersion, string(caps))
	if err != nil {
		return fmt.Errorf("mcprelay: open a session of /mcp: %w", err)
	}
	return nil
}

// SweepGateIdle closes the sessions of the gate's own /mcp idle for longer than idle (close_reason idle; §11.2's
// default session_idle_s, /mcp being no service with one of its own). They pin nothing, so any replica closes them,
// each once. It returns how many it closed.
func (s *Sessions) SweepGateIdle(ctx context.Context, idle time.Duration, now time.Time) (int, error) {
	tag, err := s.pool.Exec(ctx, `update kontra.mcp_sessions set closed_at = now(), close_reason = 'idle'
		 where service is null and closed_at is null and last_seen_at < $1::timestamptz - $2::bigint * interval '1 microsecond'`,
		now, idle.Microseconds())
	if err != nil {
		return 0, fmt.Errorf("mcprelay: sweep the idle sessions of /mcp: %w", err)
	}
	return int(tag.RowsAffected()), nil
}
