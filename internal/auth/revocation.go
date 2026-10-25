package auth

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/medmahmoudi26/kontra/internal/unimpl"
)

// RevocationCache is the replica's view of revoked API keys (and, from M2, node keys and key-service
// bindings) (spec §2.3). It is refreshed by NOTIFY kontra_revoke, re-read from Postgres (what was revoked
// since the last read) at least every 5 s and whenever the LISTEN connection reconnects; NOTIFY only
// shortens the delay.
type RevocationCache struct {
	db        *pgxpool.Pool
	interval  time.Duration
	lastCheck time.Time
}

// RefreshInterval is the longest a revocation takes to reach a replica without NOTIFY.
const RefreshInterval = 5 * time.Second

// NewRevocationCache returns an empty cache over db.
func NewRevocationCache(db *pgxpool.Pool) *RevocationCache {
	return &RevocationCache{db: db, interval: RefreshInterval}
}

// Run keeps the cache current until ctx ends.
func (c *RevocationCache) Run(ctx context.Context) error {
	return unimpl.Error("auth.RevocationCache.Run", "WP-03")
}

// Notify applies one kontra_revoke payload (pg.Listener.OnChannel).
func (c *RevocationCache) Notify(payload string) {
	panic(unimpl.Error("auth.RevocationCache.Notify", "WP-03"))
}

// Catchup re-reads revocations since the last read (pg.Listener.OnReconnect).
func (c *RevocationCache) Catchup() {
	panic(unimpl.Error("auth.RevocationCache.Catchup", "WP-03"))
}

// APIKeyRevoked reports whether the key with this id is revoked or expired as of the last refresh.
func (c *RevocationCache) APIKeyRevoked(id uuid.UUID) bool {
	panic(unimpl.Error("auth.RevocationCache.APIKeyRevoked", "WP-03"))
}
