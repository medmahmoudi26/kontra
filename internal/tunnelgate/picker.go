package tunnelgate

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"kontra.sh/kontra/internal/lifecycle"
	"kontra.sh/kontra/internal/unimpl"
)

// Picker is prepare's session choice (relay.md §6.1), on any replica: the candidates are
//
//	SELECT t.session_id, t.replica_id FROM tunnels t
//	  JOIN tunnel_keys k   ON k.key_id = t.key_id AND k.revoked_at IS NULL AND k.revoked_reason IS DISTINCT FROM 'rotated'
//	  JOIN key_services ks ON ks.key_id = t.key_id AND ks.service = $service AND ks.revoked_at IS NULL
//	  JOIN gate_replicas r ON r.replica_id = t.replica_id AND r.last_seen_at > now() - interval '30 seconds' AND r.draining_at IS NULL
//	 WHERE t.tenant_id = $tenant AND $service = ANY (t.services)
//
// round-robin among them; for a new task in a known context the session of the context's previous task
// first while it is a candidate; a retry after a lost session avoids that session when another exists.
// No candidate is lifecycle.ErrNoSession.
type Picker struct {
	db *pgxpool.Pool
}

// NewPicker returns a picker over db.
func NewPicker(db *pgxpool.Pool) *Picker { return &Picker{db: db} }

var _ lifecycle.SessionPicker = (*Picker)(nil)

// PickSession implements lifecycle.SessionPicker.
func (p *Picker) PickSession(ctx context.Context, req lifecycle.SessionRequest) (lifecycle.PickedSession, error) {
	return lifecycle.PickedSession{}, unimpl.Error("tunnelgate.Picker.PickSession", "WP-05")
}
