package mcprelay

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
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

// MaxTool is the longest tool name a calls row holds (0001: tool between 1 and 128 characters). A longer name
// is cut to it in the row; the call itself is relayed with its name as sent.
const MaxTool = 128

// Meter writes calls rows.
type Meter struct {
	pool *pgxpool.Pool
}

// NewMeter returns the meter.
func NewMeter(pool *pgxpool.Pool) *Meter { return &Meter{pool: pool} }

// Record writes one row when the response completes: its id a UUIDv7, at now. It never sees the call's
// arguments or content, and logs nothing of them.
func (m *Meter) Record(ctx context.Context, c Call) error {
	if m == nil || m.pool == nil {
		return errors.New("mcprelay: the meter has no database")
	}
	tool := c.Tool
	if r := []rune(tool); len(r) > MaxTool {
		tool = string(r[:MaxTool])
	}
	if tool == "" {
		return errors.New("mcprelay: a calls row needs its tool")
	}
	id, err := uuid.NewV7()
	if err != nil {
		return err
	}
	var session []byte
	if c.MCPSession != nil {
		session = c.MCPSession[:]
	}
	ms := c.Duration.Milliseconds()
	if ms < 0 {
		ms = 0
	}
	if ms > 1<<31-1 {
		ms = 1<<31 - 1
	}
	_, err = m.pool.Exec(ctx, `insert into kontra.calls (id, tenant_id, service, tool, buyer_id, principal, mcp_session,
			task_id, bytes_in, bytes_out, duration_ms, is_error)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		id, c.TenantID, c.Service, tool, c.BuyerID, c.Principal, session, c.TaskID, max(c.BytesIn, 0), max(c.BytesOut, 0),
		int32(ms), c.IsError)
	if err != nil {
		return fmt.Errorf("mcprelay: record a call: %w", err)
	}
	return nil
}
