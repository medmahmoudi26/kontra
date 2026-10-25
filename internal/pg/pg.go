// Package pg owns the gate's Postgres connections: the pool, and the one dedicated LISTEN connection per
// replica whose notifications wake in-process waiters (spec/relay.md §5.2, §2.3, §13.5).
//
// Every connection sets TCP_USER_TIMEOUT and keepalives client-side and asks the server for the same
// (pg-store brief §5.3), so a silent failover ends in about 20 s. The LISTEN connection is never a pooled
// connection (a LISTEN on a pooled connection outlives its release) and never runs a transaction.
package pg

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/medmahmoudi26/kontra/internal/unimpl"
)

// Channel is a NOTIFY channel the gate listens on.
type Channel string

// The gate's channels. Payloads are hints; the tables are the truth.
const (
	// ChannelTaskEvents carries "<task_id>:<seq>" after every committed task_events row (spec §5.2).
	ChannelTaskEvents Channel = "kontra_task_events"
	// ChannelServices carries "<tenant>/<name>" after a services change (spec §13.5).
	ChannelServices Channel = "kontra_services"
	// ChannelRevoke carries "apikey:<id>", "key:<key_id>" or "binding:<key_id>/<service>" (spec §2.3,
	// spec/tunnel.md §12).
	ChannelRevoke Channel = "kontra_revoke"
)

// NewPool returns the gate's connection pool for a postgres:// URL, with the connection bounds above.
func NewPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	return nil, unimpl.Error("pg.NewPool", "WP-01")
}

// Listener holds the replica's LISTEN connection and fans notifications out without blocking.
type Listener struct {
	cfg *pgx.ConnConfig
}

// NewListener returns a listener for the database cfg names. Run must be called for it to deliver.
func NewListener(cfg *pgx.ConnConfig) *Listener {
	return &Listener{cfg: cfg}
}

// Run connects, LISTENs on every Channel and delivers until ctx ends. It reconnects with a jittered
// 100 ms to 5 s backoff, and after every (re)connect wakes every waiter and runs every OnReconnect
// callback, because notifications sent during the gap are lost. A wait without notifications times out
// after about 25 s, runs `select 1`, and wakes every waiter for a safety re-read.
func (l *Listener) Run(ctx context.Context) error {
	return unimpl.Error("pg.Listener.Run", "WP-01")
}

// WaitTask returns a channel that receives when a kontra_task_events notification for taskID arrives or
// the listener reconnects, and a func that releases the registration. Waiters re-read task_events: the
// notification only says "look again".
func (l *Listener) WaitTask(ctx context.Context, taskID uuid.UUID) (<-chan struct{}, func()) {
	panic(unimpl.Error("pg.Listener.WaitTask", "WP-01"))
}

// OnChannel registers fn for every notification on ch (services and revocations).
func (l *Listener) OnChannel(ch Channel, fn func(payload string)) {
	panic(unimpl.Error("pg.Listener.OnChannel", "WP-01"))
}

// OnReconnect registers fn to run after every (re)connect, e.g. the revocation cache's catch-up read.
func (l *Listener) OnReconnect(fn func()) {
	panic(unimpl.Error("pg.Listener.OnReconnect", "WP-01"))
}

// Healthy reports whether the LISTEN connection is up (part of /healthz, spec §2.1).
func (l *Listener) Healthy() bool {
	return false
}
