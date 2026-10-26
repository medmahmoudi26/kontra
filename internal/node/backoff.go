package node

import (
	"time"

	"kontra.sh/kontra/internal/unimpl"
)

// Reconnect backoff (§13).
const (
	BackoffMin   = 1 * time.Second
	BackoffMax   = 60 * time.Second
	BackoffReset = 60 * time.Second // a session that lived this long resets the backoff
)

// Backoff is exponential from BackoffMin to BackoffMax with full jitter.
type Backoff struct {
	attempt int
	rand    func() float64
}

// Next is the wait before the next dial.
func (b *Backoff) Next() time.Duration {
	panic(unimpl.Error("node.Backoff.Next", "WP-06"))
}

// SessionEnded records how long the last session lived; BackoffReset or more resets the backoff.
func (b *Backoff) SessionEnded(lived time.Duration) {
	panic(unimpl.Error("node.Backoff.SessionEnded", "WP-06"))
}
