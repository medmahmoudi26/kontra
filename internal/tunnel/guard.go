package tunnel

import (
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// What the gate lets a node's yamux frames make it do (M2 review, security-1; D155).
//
// yamux v0.1.2 answers every ping SYN on a goroutine of its own, which waits up to ConnectionWriteTimeout
// to queue its pong while the session's send queue (64 frames) is full, as it is as soon as the peer stops
// reading. Twelve bytes of input then pin a goroutine for 15 s, and a node that floods pings while it does
// not read takes the replica's memory, every tenant's sessions with it. A node pings once per welcome.ping_s
// (at least 1 s, §11) and waits for each pong, so the gate's yamux.Server takes PingBurst ping SYNs at
// once and one more every PingRefill; over that the session closes. At most PingBurst + 15 s / PingRefill
// pongs (46) can be waiting on one session.
const (
	PingBurst  = 16
	PingRefill = 500 * time.Millisecond
)

// yamux v0.1.2's frame header (spec.md of hashicorp/yamux): version, type, flags, stream id, length.
const (
	frameHeader = 12
	frameData   = 0
	framePing   = 2
	flagSYN     = 1
)

// ErrPingFlood is the error a gate's session reads when its node sent more ping SYNs than PingBurst and
// PingRefill allow: the session ends (yamux logs it, and its owner sees the session close).
var ErrPingFlood = fmt.Errorf("tunnel: the peer sent pings faster than %d at once and one every %v: the session ends", PingBurst, PingRefill)

// pingGuard reads the frames yamux reads, in step with yamux's own parse: each frame header, and the body
// that follows a data frame's header (its length; every other type carries its value in the length field
// and no body). It counts the ping SYNs against a token bucket and fails the read once the bucket is empty,
// which ends the session before yamux has seen the frame.
type pingGuard struct {
	net.Conn
	hdr   [frameHeader]byte
	held  int    // bytes of hdr read so far
	body  uint32 // body bytes of the current data frame still to come
	pings *rate.Limiter
	err   error
}

func newPingGuard(c net.Conn) *pingGuard {
	return &pingGuard{Conn: c, pings: rate.NewLimiter(rate.Every(PingRefill), PingBurst)}
}

func (g *pingGuard) Read(p []byte) (int, error) {
	if g.err != nil {
		return 0, g.err
	}
	n, err := g.Conn.Read(p)
	if ferr := g.inspect(p[:n]); ferr != nil {
		g.err = ferr
		return 0, ferr
	}
	return n, err
}

// inspect follows the frames in b, which continue those of the earlier reads.
func (g *pingGuard) inspect(b []byte) error {
	for len(b) > 0 {
		if g.body > 0 {
			k := min(uint64(len(b)), uint64(g.body))
			g.body -= uint32(k)
			b = b[k:]
			continue
		}
		k := copy(g.hdr[g.held:], b)
		g.held += k
		b = b[k:]
		if g.held < frameHeader {
			return nil
		}
		g.held = 0
		flags := binary.BigEndian.Uint16(g.hdr[2:4])
		length := binary.BigEndian.Uint32(g.hdr[8:12])
		switch g.hdr[1] {
		case frameData:
			g.body = length
		case framePing:
			if flags&flagSYN != 0 && !g.pings.Allow() {
				return ErrPingFlood
			}
		}
	}
	return nil
}

// Yamux's own log lines are rate-limited per session (M2 review, security-2; D156): yamux v0.1.2 writes one
// line for every frame of a stream it does not hold ("frame for missing stream", "Discarding data for
// stream"), so a node could make the gate write some 25 bytes of log for each byte it sends. A session's
// lines pass LogBurst at once and one more every LogRefill; the first line over it says lines are being
// suppressed, and the next line let through is preceded by the count of those suppressed.
const (
	LogBurst  = 10
	LogRefill = time.Second
)

// limitHandler is a slog.Handler that lets through what its limiter allows; the handlers WithAttrs and
// WithGroup derive share the limiter.
type limitHandler struct {
	next slog.Handler
	lim  *logLimit
}

type logLimit struct {
	mu         sync.Mutex
	allow      *rate.Limiter
	suppressed int
}

func newLimitHandler(next slog.Handler) *limitHandler {
	return &limitHandler{next: next, lim: &logLimit{allow: rate.NewLimiter(rate.Every(LogRefill), LogBurst)}}
}

func (h *limitHandler) Enabled(ctx context.Context, l slog.Level) bool { return h.next.Enabled(ctx, l) }

func (h *limitHandler) Handle(ctx context.Context, r slog.Record) error {
	h.lim.mu.Lock()
	if !h.lim.allow.Allow() {
		h.lim.suppressed++
		first := h.lim.suppressed == 1
		h.lim.mu.Unlock()
		if first {
			n := slog.NewRecord(r.Time, r.Level, "yamux: more lines than the session's log limit: suppressing them", 0)
			n.AddAttrs(slog.Int("burst", LogBurst), slog.Duration("refill", LogRefill))
			return h.next.Handle(ctx, n)
		}
		return nil
	}
	suppressed := h.lim.suppressed
	h.lim.suppressed = 0
	h.lim.mu.Unlock()
	if suppressed > 0 {
		n := slog.NewRecord(r.Time, r.Level, "yamux: lines were suppressed by the session's log limit", 0)
		n.AddAttrs(slog.Int("suppressed", suppressed))
		if err := h.next.Handle(ctx, n); err != nil {
			return err
		}
	}
	return h.next.Handle(ctx, r)
}

func (h *limitHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return &limitHandler{next: h.next.WithAttrs(as), lim: h.lim}
}

func (h *limitHandler) WithGroup(name string) slog.Handler {
	return &limitHandler{next: h.next.WithGroup(name), lim: h.lim}
}
