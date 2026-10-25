package store

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/medmahmoudi26/kontra/internal/unimpl"
)

// Backend is attempts.backend.
type Backend string

// Backends.
const (
	BackendDirect Backend = "direct"
	BackendTunnel Backend = "tunnel" // M2
)

// Outcome is attempts.outcome.
type Outcome string

// Attempt outcomes (0001_init.sql).
const (
	OutcomeCompleted           Outcome = "completed"
	OutcomeFailed              Outcome = "failed"
	OutcomeCanceled            Outcome = "canceled"
	OutcomeRejected            Outcome = "rejected"
	OutcomeInterrupted         Outcome = "interrupted"
	OutcomeNoSession           Outcome = "no_session"
	OutcomeTunnelLost          Outcome = "tunnel_lost"
	OutcomeUpstreamUnreachable Outcome = "upstream_unreachable"
	OutcomeUpstreamError       Outcome = "upstream_error"
	OutcomeServiceUnavailable  Outcome = "service_unavailable"
	OutcomeTimeout             Outcome = "timeout"
	OutcomeSealedRejected      Outcome = "sealed_rejected"
	OutcomeAborted             Outcome = "aborted"
)

// Attempt is one dispatch of a gate task to one upstream (spec §4). Upstream ids live only here.
type Attempt struct {
	TaskID            uuid.UUID
	N                 int
	Backend           Backend
	TunnelSession     *uuid.UUID
	KeyID             string
	ReplicaID         string
	UpstreamTaskID    string
	UpstreamContextID string
	UpstreamSession   string
	StartedAt         time.Time
	FirstUpstreamIDAt *time.Time
	EndedAt           *time.Time
	Outcome           Outcome
}

// BeginAttempt inserts attempt n (prepare, spec §14.2).
func (l *Log) BeginAttempt(ctx context.Context, a Attempt) error {
	return unimpl.Error("store.Log.BeginAttempt", "WP-02")
}

// SetAttemptReplica records the replica running the dispatch before any upstream call, for both
// backends (spec §4, §14.2). A re-attach (D42) overwrites it with the new replica.
func (l *Log) SetAttemptReplica(ctx context.Context, taskID uuid.UUID, n int, replicaID string) error {
	return unimpl.Error("store.Log.SetAttemptReplica", "WP-02")
}

// SetFirstUpstreamID records the attempt's first upstream task id and context id, and
// first_upstream_id_at, before the event that carried them is logged (spec §4). It is a no-op when the
// same id is already recorded and an error when a different one is.
func (l *Log) SetFirstUpstreamID(ctx context.Context, taskID uuid.UUID, n int, upstreamTaskID, upstreamContextID string) error {
	return unimpl.Error("store.Log.SetFirstUpstreamID", "WP-02")
}

// EndAttempt records the attempt's outcome and ended_at.
func (l *Log) EndAttempt(ctx context.Context, taskID uuid.UUID, n int, o Outcome) error {
	return unimpl.Error("store.Log.EndAttempt", "WP-02")
}

// GetAttempt returns attempt n. attempt_state reads it: the retry decision is taken on what Postgres
// recorded, never on heartbeat details (spec §14.2).
func (l *Log) GetAttempt(ctx context.Context, taskID uuid.UUID, n int) (*Attempt, error) {
	return nil, unimpl.Error("store.Log.GetAttempt", "WP-02")
}

// PreviousContextAttempt returns the latest attempt of the previous task in the same gate context and
// backend, for the upstream context id reuse of spec §4 (nil when there is none).
func (l *Log) PreviousContextAttempt(ctx context.Context, svc ServiceRef, o Owner, contextID string, backend Backend) (*Attempt, error) {
	return nil, unimpl.Error("store.Log.PreviousContextAttempt", "WP-02")
}
