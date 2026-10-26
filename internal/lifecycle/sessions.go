package lifecycle

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

// M2 seam (WP-05): prepare's tunnel session choice (spec §6.1, §6.4, §14.2). Prepare places a tunnel
// service's attempt on the session SessionPicker returns, writes the attempt's tunnel_session, key_id and
// replica_id, and answers the queue QueueTunnel(replica); with no candidate it writes the attempt with
// outcome no_session and the workflow retries after 1 s, 2 s, 4 s … while task.retries allows (§6.4; the
// workflow change sits behind workflow.GetVersion, §14.4). Not wired before WP-05: prepare still refuses
// tunnel services (service_unavailable).

// ErrNoSession is SessionPicker's answer when no session of the service is a candidate.
var ErrNoSession = errors.New("lifecycle: no tunnel session serves the service")

// SessionRequest is what the session choice needs.
type SessionRequest struct {
	TenantID uuid.UUID
	Service  string
	TaskID   uuid.UUID
	N        int
	// Avoid is the session a lost attempt used: another candidate is preferred when one exists (§6.4,
	// "retry on another session if one exists").
	Avoid *uuid.UUID
}

// PickedSession is the chosen session.
type PickedSession struct {
	SessionID uuid.UUID
	KeyID     string
	ReplicaID string
}

// SessionPicker chooses a tunnel session for a new attempt (spec §6.1): the candidate query, round-robin,
// the context's previous session first while it is a candidate; ErrNoSession when there is none.
type SessionPicker interface {
	PickSession(ctx context.Context, req SessionRequest) (PickedSession, error)
}
