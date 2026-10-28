package gatemcp

import (
	"context"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"kontra.sh/kontra/internal/store"
	"kontra.sh/kontra/internal/unimpl"
)

// CodeURLElicitationRequired is URLElicitationRequiredError (MCP 2025-11-25), hire's answer to a legacy client that
// declared elicitation.url when Admission quotes (§12.3, D21). Never on a modern request (D161).
const CodeURLElicitationRequired = -32042

// Elicitations keeps the URL elicitations hire sent: (elicitationId, task, session) in tasks.elicitation_id and
// tasks.elicitation_session (§12.3), and writes notifications/elicitation/complete on the session's GET streams.
type Elicitations struct {
	db    *pgxpool.Pool
	waker store.TaskWaker
}

// NewElicitations writes to db and is woken by waker.
func NewElicitations(db *pgxpool.Pool, waker store.TaskWaker) *Elicitations {
	return &Elicitations{db: db, waker: waker}
}

// Record mints an elicitationId for a quoted task and stores it with the session that receives the -32042, or
// returns the one already stored (a retry, §12.3).
func (e *Elicitations) Record(ctx context.Context, taskID uuid.UUID, session [32]byte) (string, error) {
	return "", unimpl.Error("gatemcp.Elicitations.Record", "M4 WP-04")
}

// Stream serves a legacy session's GET stream (§12.3, DM4-6): keep-alives every 15 s (§2.2) and, for each task
// whose -32042 went to this session and that left quoted while the stream is open or less than RetryWindow before
// it opened, one notifications/elicitation/complete {elicitationId}. No event ids; the stream is not resumed.
func (e *Elicitations) Stream(ctx context.Context, w http.ResponseWriter, session [32]byte) {
	panic(unimpl.Error("gatemcp.Elicitations.Stream", "M4 WP-04"))
}
