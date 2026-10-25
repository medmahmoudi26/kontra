// Package gate assembles one kontra-gate replica: configuration, Postgres, the LISTEN connection,
// Temporal workers, the relay and the public listener (spec/relay.md §2, §14; spec/tunnel.md §11).
package gate

import (
	"context"

	"github.com/medmahmoudi26/kontra/internal/config"
	"github.com/medmahmoudi26/kontra/internal/unimpl"
)

// Replica is one gate process. Its replica id is unique per process start; it is inserted into
// gate_replicas before any worker polls, upserted every 10 s, and every 10 s each replica deletes the rows
// of replicas silent for more than 30 s (spec/tunnel.md §11).
type Replica struct {
	cfg *config.Config
	id  string
}

// New connects everything a replica needs and checks that the schema is at the embedded version (the
// gate never migrates implicitly).
func New(ctx context.Context, cfg *config.Config) (*Replica, error) {
	return nil, unimpl.Error("gate.New", "WP-08")
}

// ID is the replica id.
func (r *Replica) ID() string { return r.id }

// Run serves until ctx ends. On SIGTERM (ctx canceled by the caller with the drain cause) it drains:
// gate_replicas.draining_at, /healthz 503, workers drained (lifecycle.Workers.Drain), and exit once no
// dispatch is in flight or drain_timeout passes (spec §14.3). On SIGHUP it reloads services from the file.
func (r *Replica) Run(ctx context.Context) error {
	return unimpl.Error("gate.Replica.Run", "WP-08")
}
