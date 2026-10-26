package tunnelgate

import (
	"github.com/google/uuid"

	"kontra.sh/kontra/internal/store"
	"kontra.sh/kontra/internal/unimpl"
	"kontra.sh/kontra/internal/upstream"
)

// Backend returns the upstream.Backend of an attempt placed on session id for svc (relay.md §6.2): an
// upstream.Tunnel whose StreamOpener is the session's OpenExchange with the preamble {"v": 1, "op":
// "http", "service", "kind", "task", "context"}. ErrSessionGone when this replica does not hold the
// session: the dispatch fails tunnel_lost at once (relay.md §14.2).
func (r *Registry) Backend(id uuid.UUID, svc store.ServiceRef) (upstream.Backend, error) {
	return nil, unimpl.Error("tunnelgate.Registry.Backend", "WP-05")
}
