// Package pg owns the gate's Postgres connections: the pool, and the one dedicated LISTEN connection per
// replica whose notifications wake in-process waiters (spec/relay.md §5.2, §2.3, §13.5).
//
// Every connection sets TCP_USER_TIMEOUT and keepalives client-side and asks the server for the same
// (pg-store brief §5.3), so a silent failover ends in about 20 s. The LISTEN connection is never a pooled
// connection (a LISTEN on a pooled connection outlives its release) and never runs a transaction.
//
// Connections must be session-mode (no PgBouncer transaction pooling): LISTEN and the migration lock are
// session state.
package pg

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
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

// Channels is every channel the LISTEN connection listens on.
var Channels = []Channel{ChannelTaskEvents, ChannelServices, ChannelRevoke}

// Connection bounds (pg-store brief §5.3; the kontra-cloud operations doc): a peer that stops answering
// is given up after TCPUserTimeout of unacknowledged data, or after keepalives fail for
// KeepAliveIdle + KeepAliveCount × KeepAliveInterval of silence; both are about 20 s.
const (
	TCPUserTimeout    = 20 * time.Second
	KeepAliveIdle     = 5 * time.Second
	KeepAliveInterval = 5 * time.Second
	KeepAliveCount    = 3
	// ConnectTimeout bounds dialing and the startup handshake when the URL sets no connect_timeout.
	ConnectTimeout = 10 * time.Second
	// DefaultMaxConns is the pool size when the URL sets no pool_max_conns (pgx's own default is
	// max(4, NumCPU), too small for a replica with thousands of dispatch slots).
	DefaultMaxConns = 20
	// ApplicationName is reported in pg_stat_activity when the URL sets none.
	ApplicationName = "kontra-gate"
)

// ParseConfig parses a postgres:// URL into a connection config with the connection bounds applied:
// the client-side dialer (TCP_USER_TIMEOUT, keepalives, a connect timeout) and the server-side startup
// parameters tcp_user_timeout and tcp_keepalives_* (values in the URL win).
func ParseConfig(dsn string) (*pgx.ConnConfig, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	applyBounds(cfg)
	return cfg, nil
}

func applyBounds(cfg *pgx.ConnConfig) {
	if cfg.ConnectTimeout == 0 {
		cfg.ConnectTimeout = ConnectTimeout
	}
	d := &net.Dialer{
		Timeout: cfg.ConnectTimeout,
		KeepAliveConfig: net.KeepAliveConfig{
			Enable:   true,
			Idle:     KeepAliveIdle,
			Interval: KeepAliveInterval,
			Count:    KeepAliveCount,
		},
		Control: setTCPUserTimeout,
	}
	cfg.DialFunc = d.DialContext
	if cfg.RuntimeParams == nil {
		cfg.RuntimeParams = map[string]string{}
	}
	for k, v := range map[string]string{
		"tcp_user_timeout":        fmt.Sprint(TCPUserTimeout.Milliseconds()),
		"tcp_keepalives_idle":     fmt.Sprint(int(KeepAliveIdle.Seconds())),
		"tcp_keepalives_interval": fmt.Sprint(int(KeepAliveInterval.Seconds())),
		"tcp_keepalives_count":    fmt.Sprint(KeepAliveCount),
		"application_name":        ApplicationName,
	} {
		if _, set := cfg.RuntimeParams[k]; !set {
			cfg.RuntimeParams[k] = v
		}
	}
}

// NewPool returns the gate's connection pool for a postgres:// URL, with the connection bounds above
// and at most DefaultMaxConns connections unless the URL sets pool_max_conns. It does not wait for a
// connection: the first query does (call Ping to check).
func NewPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	pc, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	applyBounds(pc.ConnConfig)
	if !strings.Contains(dsn, "pool_max_conns") {
		pc.MaxConns = DefaultMaxConns
	}
	return pgxpool.NewWithConfig(ctx, pc)
}
