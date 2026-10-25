// Package ids mints and parses the gate's identifiers (spec/relay.md §2.3, §4).
//
// Every id is minted in Go: task and context ids are UUIDv7 (Postgres before 18 has no uuidv7(), D49),
// derived buyer ids are UUIDv5. Anything a caller sends as a task id is parsed here before any query,
// so a malformed id is TaskNotFound and never a 500 (TCK CORE-GET-002).
package ids

import (
	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"

	"github.com/medmahmoudi26/kontra/internal/unimpl"
)

// NewTaskID mints a gate task id (UUIDv7). A caller can never choose one (spec §3.4.1).
func NewTaskID() (a2a.TaskID, error) {
	return "", unimpl.Error("ids.NewTaskID", "WP-01")
}

// NewContextID mints a gate context id (UUIDv7) for a message that carries none (D3).
func NewContextID() (string, error) {
	return "", unimpl.Error("ids.NewContextID", "WP-01")
}

// NewWorkItemID mints a work_items id (UUIDv7); it is also the kontra.message Update id (spec §14.1).
func NewWorkItemID() (uuid.UUID, error) {
	return uuid.Nil, unimpl.Error("ids.NewWorkItemID", "WP-01")
}

// ParseTaskID parses a caller-supplied task id. Anything that is not a UUID returns an error wrapping
// a2a.ErrTaskNotFound (spec §4), so the caller cannot tell a malformed id from an unknown one.
func ParseTaskID(s string) (uuid.UUID, error) {
	return uuid.Nil, unimpl.Error("ids.ParseTaskID", "WP-01")
}

// AnonymousBuyer is the buyer of the `anonymous` principal (spec §2.3, D20): on a team gate one fixed id
// for every anonymous caller (pass uuid.Nil), on a multi-tenant gate UUIDv5(service tenant ‖ "anonymous").
func AnonymousBuyer(serviceTenant uuid.UUID) (uuid.UUID, error) {
	return uuid.Nil, unimpl.Error("ids.AnonymousBuyer", "WP-03")
}

// OIDCBuyer is UUIDv5(issuer ‖ sub) for an `oidc:sub:` principal (spec §2.3).
func OIDCBuyer(issuer, sub string) (uuid.UUID, error) {
	return uuid.Nil, unimpl.Error("ids.OIDCBuyer", "WP-03")
}

// NodeBuyer is UUIDv5(gate namespace ‖ key_id) for a node hiring without an API key (spec §2.3, M2).
func NodeBuyer(gateNamespace uuid.UUID, keyID string) (uuid.UUID, error) {
	return uuid.Nil, unimpl.Error("ids.NodeBuyer", "M2")
}

// NewReplicaID returns a replica id unique per process start, "<host>-<8 hex>" (spec §14.2): it names the
// replica's own Temporal queues kontra-tunnel-<id> and kontra-ctl-<id>.
func NewReplicaID(host string) (string, error) {
	return "", unimpl.Error("ids.NewReplicaID", "WP-01")
}
