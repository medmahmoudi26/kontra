package tunnelgate

import (
	"context"

	"github.com/google/uuid"

	"kontra.sh/kontra/internal/unimpl"
	"kontra.sh/kontra/internal/upstream"
)

// FetchCard fetches a tunnel service's upstream card through one of this replica's sessions serving it
// (GET /.well-known/agent-card.json, then /.well-known/agent.json, at most 256 KiB, relay.md §3, §6.6), the
// card.Keeper fetch of a tunnel service. ErrSessionGone when this replica holds none: the keeper then
// serves the card another replica stored.
func (r *Registry) FetchCard(ctx context.Context, tenantID uuid.UUID, service string) (*upstream.Fetched, error) {
	return nil, unimpl.Error("tunnelgate.Registry.FetchCard", "WP-04")
}

// OnConnect registers fn to run when a session serving a service is added (the card keeper fetches then,
// relay.md §3).
func (r *Registry) OnConnect(fn func(tenantID uuid.UUID, service string)) {
	panic(unimpl.Error("tunnelgate.Registry.OnConnect", "WP-04"))
}
