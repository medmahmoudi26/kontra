package tunnel

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hashicorp/yamux"
)

// Pinger is the part of *yamux.Session the ping loop uses.
type Pinger interface {
	Ping() (time.Duration, error)
	Close() error
}

// ErrPeerSilent is wrapped by KeepAlive's error when it closed the session after consecutive failed pings.
var ErrPeerSilent = errors.New("tunnel: the peer missed consecutive pings")

// KeepAlive is §11's ping loop, run by both sides (D33): Ping() every `every`; `failures` consecutive
// failures close the session and return the last error, wrapped with ErrPeerSilent. It returns nil when
// ctx ends, and when the session closed for another reason (a Pinger that has CloseChan, as
// *yamux.Session does, is watched; any other is seen closed at its next Ping). A successful ping resets
// the count. A zero `every` or `failures` is PingEvery or PingFailures.
//
// A ping fails when its pong does not come back within yamux's ConnectionWriteTimeout (15 s) or it cannot
// be queued within it, so with the defaults a silent peer is closed about 75 s after its last pong on
// either side, each side on its own loop: yamux answers pings without telling the answering side, so a
// side that only answers never learns its peer went silent.
func KeepAlive(ctx context.Context, p Pinger, every time.Duration, failures int) error {
	if every <= 0 {
		every = PingEvery
	}
	if failures <= 0 {
		failures = PingFailures
	}
	var closed <-chan struct{}
	if c, ok := p.(interface{ CloseChan() <-chan struct{} }); ok {
		closed = c.CloseChan()
	}
	t := time.NewTicker(every)
	defer t.Stop()
	missed := 0
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-closed:
			return nil
		case <-t.C:
		}
		_, err := p.Ping()
		switch {
		case err == nil:
			missed = 0
			continue
		case errors.Is(err, yamux.ErrSessionShutdown):
			return nil
		case ctx.Err() != nil:
			return nil
		}
		missed++
		if missed >= failures {
			_ = p.Close()
			return fmt.Errorf("%w: %d in a row, the session is closed; the last: %w", ErrPeerSilent, missed, err)
		}
	}
}
