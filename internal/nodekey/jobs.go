package nodekey

import (
	"time"

	"filippo.io/age"

	"kontra.sh/kontra/internal/unimpl"
)

// JobsDirName is the per-job keys' directory under ~/.kontra (spec/tunnel.md §2.3).
const JobsDirName = "jobs"

// JobKeys are a hiring node's per-job keys for one gate: ~/.kontra/jobs/<gate host>/<job id>.key, 0600, in
// the identity file's format (spec/tunnel.md §2.3, spec/ext-sealed.md §4). A job is an A2A task, an MCP task,
// or an MCP session for its plain calls. The public half travels as replyTo inside the sealed request header
// and as metadata["kontra.sealed.replyTo"]; the private half never leaves the node.
//
// Work package: M3 WP-01 (the store), M3 WP-08 (minting at send time, deletion at terminal + retention).
type JobKeys struct {
	dir string
}

// OpenJobKeys opens (creating, 0700) the jobs directory of one gate host under home's ~/.kontra.
func OpenJobKeys(home, gateHost string) (*JobKeys, error) {
	return nil, unimpl.Error("nodekey.OpenJobKeys", "M3 WP-01")
}

// Mint creates a new X25519 key for jobID and writes it before returning; an existing jobID is an error (a
// job's key is never replaced: replies already sealed to it must still open).
func (j *JobKeys) Mint(jobID string, now time.Time) (*age.X25519Identity, error) {
	return nil, unimpl.Error("nodekey.JobKeys.Mint", "M3 WP-01")
}

// Load returns jobID's key, refusing a file readable by group or others.
func (j *JobKeys) Load(jobID string) (*age.X25519Identity, error) {
	return nil, unimpl.Error("nodekey.JobKeys.Load", "M3 WP-01")
}

// Rename moves a key minted under a provisional id (a new task's first message, sent before the gate's task
// id exists) to the gate's job id once the first answer names it.
func (j *JobKeys) Rename(from, to string) error {
	return unimpl.Error("nodekey.JobKeys.Rename", "M3 WP-01")
}

// Delete removes jobID's key: when the job is terminal and any kontra.keyRetainUntil has passed; for an MCP
// session, when it closes and every task created in it is done (spec/tunnel.md §2.3).
func (j *JobKeys) Delete(jobID string) error {
	return unimpl.Error("nodekey.JobKeys.Delete", "M3 WP-01")
}
