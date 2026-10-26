package tunnel

import (
	"context"
	"time"

	"kontra.sh/kontra/internal/unimpl"
)

// Pinger is the part of *yamux.Session the ping loop uses.
type Pinger interface {
	Ping() (time.Duration, error)
	Close() error
}

// KeepAlive is §11's ping loop, run by both sides (D33): Ping() every `every`; `failures` consecutive
// failures close the session and return the last error. It returns nil when ctx ends.
func KeepAlive(ctx context.Context, p Pinger, every time.Duration, failures int) error {
	return unimpl.Error("tunnel.KeepAlive", "WP-03")
}
