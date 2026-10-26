package enroll

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"kontra.sh/kontra/internal/unimpl"
)

// Revocation is one node-key or binding revocation a replica learned of (§12).
type Revocation struct {
	KeyID   string
	Service string // empty: the whole key
	Reason  string // owner, operator, rotated (overlap ended), expired
}

// Revocations is the replica's view of revoked node keys and key–service bindings (relay.md §2.3, the
// node half of the revocation cache auth.RevocationCache keeps for API keys): refreshed by NOTIFY
// kontra_revoke 'key:<key_id>' and 'binding:<key_id>/<service>', re-read from Postgres (what was revoked
// since the last read, overlapping it) at least every 5 s and whenever the LISTEN connection reconnects.
// NOTIFY only shortens the delay. The tunnel registry subscribes to act on each revocation (close the
// key's sessions, or drop the service from the served set), and asks it before every stream (key_scope).
type Revocations struct {
	db *pgxpool.Pool
}

// NewRevocations returns an empty view over db.
func NewRevocations(db *pgxpool.Pool) *Revocations { return &Revocations{db: db} }

// Run keeps the view current until ctx ends.
func (r *Revocations) Run(ctx context.Context) error {
	return unimpl.Error("enroll.Revocations.Run", "WP-02")
}

// Notify applies one kontra_revoke payload (pg.Listener.OnChannel); API key payloads are auth's.
func (r *Revocations) Notify(payload string) {
	panic(unimpl.Error("enroll.Revocations.Notify", "WP-02"))
}

// Catchup re-reads revocations since the last read (pg.Listener.OnReconnect).
func (r *Revocations) Catchup() {
	panic(unimpl.Error("enroll.Revocations.Catchup", "WP-02"))
}

// Subscribe registers fn for every revocation learned, by NOTIFY or re-read, once each.
func (r *Revocations) Subscribe(fn func(Revocation)) {
	panic(unimpl.Error("enroll.Revocations.Subscribe", "WP-02"))
}

// KeyUsable reports whether a key may carry a new stream at now: not revoked and, when rotated, still
// inside its overlap (relay.md §6.1).
func (r *Revocations) KeyUsable(keyID string, now time.Time) bool {
	panic(unimpl.Error("enroll.Revocations.KeyUsable", "WP-02"))
}

// BindingRevoked reports whether the key's binding to service was revoked.
func (r *Revocations) BindingRevoked(keyID, service string) bool {
	panic(unimpl.Error("enroll.Revocations.BindingRevoked", "WP-02"))
}

// Fresh reports whether the last successful re-read began less than 5 s ago; while it is not, the registry
// reads the key's row before opening a stream.
func (r *Revocations) Fresh() bool {
	panic(unimpl.Error("enroll.Revocations.Fresh", "WP-02"))
}
