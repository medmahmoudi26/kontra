package mcprelay

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"kontra.sh/kontra/internal/unimpl"
)

// Call is one calls row (spec/relay.md §11.4): what lies outside sealed content, and no price.
type Call struct {
	TenantID   uuid.UUID
	Service    string
	Tool       string
	BuyerID    uuid.UUID
	Principal  string
	MCPSession *[32]byte  // the session's hash; a virtual one for a modern client (D160)
	TaskID     *uuid.UUID // a gate-owned task's
	BytesIn    int64
	BytesOut   int64
	Duration   time.Duration
	// IsError is a JSON-RPC error or a result whose top-level isError is true; nil when the outcome is not
	// observed (a passthrough task-augmented call, §11.5).
	IsError *bool
}

// Meter writes calls rows.
type Meter struct {
	pool *pgxpool.Pool
}

// NewMeter returns the meter.
func NewMeter(pool *pgxpool.Pool) *Meter { return &Meter{pool: pool} }

// Record writes one row when the response completes.
func (m *Meter) Record(ctx context.Context, c Call) error {
	return unimpl.Error("mcprelay.Meter.Record", "M3 WP-04")
}
