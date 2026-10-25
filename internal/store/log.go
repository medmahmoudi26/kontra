package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/medmahmoudi26/kontra/internal/unimpl"
)

// Log is the unscoped internal API over tasks and task_events. Its Append is the only code that writes
// the projection and the log; Store, the work queue, write_terminal and apply_metadata all go through it.
type Log struct {
	db *pgxpool.Pool
}

// NewLog returns the internal API over db.
func NewLog(db *pgxpool.Pool) *Log {
	return &Log{db: db}
}

// Write is one change to a task (spec §5.2).
type Write struct {
	TaskID      uuid.UUID
	PrevVersion int64 // must equal tasks.version; 0 is refused
	Kind        EventKind
	State       State     // the state after the event
	StatusAt    time.Time // the logged status.timestamp, truncated to microseconds
	Snapshot    *a2a.Task // the full projection after the event
	Payload     json.RawMessage
	PayloadRef  string
	// AfterTerminalOK is set only by apply_metadata and an accepted review-window message, the two
	// writers allowed after a terminal event (spec §5.1).
	AfterTerminalOK bool
	// Timeout and Retention fix tasks.deadline at the transition into submitted and
	// tasks.retention_until at the terminal event.
	Timeout   time.Duration
	Retention time.Duration
}

// Appended is what Append returns: the new seq (= version) and the stored deadline, which the workflow
// arms its task.timeout timer from (spec §14.1).
type Appended struct {
	Seq      int64
	Deadline *time.Time
}

// Append runs the one transaction of spec §5.2 for w, in tx when tx is not nil.
func (l *Log) Append(ctx context.Context, tx pgx.Tx, w Write) (Appended, error) {
	return Appended{}, unimpl.Error("store.Log.Append", "WP-02")
}

// NewTask is a gate task as the work queue pre-creates it (spec §15): gate ids, `submitted` (or `quoted`),
// the caller's first message in history.
type NewTask struct {
	ID             uuid.UUID
	Service        ServiceRef
	Protocol       Protocol
	Owner          Owner
	ContextID      string
	State          State // submitted or quoted
	Snapshot       *a2a.Task
	FirstMessageID string
	FirstRequest   [32]byte // SHA-256 of the request, for resend detection (spec §15)
	QuoteExpiresAt *time.Time
}

// CreateTask inserts tasks at version 1 and task_events seq 1 (kind task) in tx and notifies.
func (l *Log) CreateTask(ctx context.Context, tx pgx.Tx, t NewTask) (Appended, error) {
	return Appended{}, unimpl.Error("store.Log.CreateTask", "WP-02")
}

// FindResend returns the task a resend is answered with: same tenant, service, buyer and principal,
// the same first messageId and request hash, created after since (24 h, spec §15). Nil when none.
func (l *Log) FindResend(ctx context.Context, svc ServiceRef, o Owner, messageID string, requestHash [32]byte, since time.Time) (*TaskRow, error) {
	return nil, unimpl.Error("store.Log.FindResend", "WP-02")
}

// Load returns the task row without caller scope.
func (l *Log) Load(ctx context.Context, taskID uuid.UUID) (*TaskRow, error) {
	return nil, unimpl.Error("store.Log.Load", "WP-02")
}

// LoadScoped returns the task row if it belongs to the caller's scope, else an error wrapping
// a2a.ErrTaskNotFound. The SSE writer and push read through it.
func (l *Log) LoadScoped(ctx context.Context, svc ServiceRef, o Owner, taskID uuid.UUID) (*TaskRow, error) {
	return nil, unimpl.Error("store.Log.LoadScoped", "WP-02")
}

// Events returns up to limit rows with seq > after, in seq order.
func (l *Log) Events(ctx context.Context, taskID uuid.UUID, after int64, limit int) ([]Event, error) {
	return nil, unimpl.Error("store.Log.Events", "WP-02")
}

// Tail returns max(seq) for the task, 0 when it has no rows yet.
func (l *Log) Tail(ctx context.Context, taskID uuid.UUID) (int64, error) {
	return 0, unimpl.Error("store.Log.Tail", "WP-02")
}

// SnapshotAt folds rows 1..seq into the task as it was after seq (spec §9): a2a-go's a2aevent.ApplyUpdate
// for task, status, artifact and message rows, a top-level merge into metadata for metadata rows. It is
// an error wrapping a2a.ErrInvalidParams when seq is not a seq of this task.
func (l *Log) SnapshotAt(ctx context.Context, taskID uuid.UUID, seq int64) (*a2a.Task, error) {
	return nil, unimpl.Error("store.Log.SnapshotAt", "WP-02")
}

// TerminalWrite is what write_terminal logs when no relay can (spec §14.2): retries exhausted, the
// owning replica dead, timeout, the cancel of an input-required task.
type TerminalWrite struct {
	TaskID   uuid.UUID
	State    State         // failed or canceled (or completed/rejected relayed from an upstream answer)
	Reason   FailureReason // for failed: metadata["kontra.failure"]
	Canceled string        // for canceled: metadata["kontra.canceled"] = client | declined | quote_expired
	Text     string        // the agent-role status message's one text part
}

// WriteTerminal logs the terminal status event at the current version. A task that is already terminal
// is left alone and its terminal seq returned: the call is idempotent, as a retried activity needs.
func (l *Log) WriteTerminal(ctx context.Context, w TerminalWrite) (int64, error) {
	return 0, unimpl.Error("store.Log.WriteTerminal", "WP-02")
}

// ApplyMetadata logs one metadata event merging patch into the task's metadata at the top level (an
// operator key replaces a same-named key, spec §2.5), allowed after a terminal event.
func (l *Log) ApplyMetadata(ctx context.Context, taskID uuid.UUID, patch map[string]any) (int64, error) {
	return 0, unimpl.Error("store.Log.ApplyMetadata", "WP-02")
}
