package mcprelay

import (
	"context"
	"sync"
	"time"
)

// inUse is this replica's legacy sessions with a request or a GET stream in flight (D231). While one is, the
// replica writes its last_seen_at at least every IdleWrite and a half, so that no replica's initialize beyond
// max_sessions_per_principal takes it for idle (EvictIdle is longer), and once more when the last ends when it has
// not been written for IdleWrite; this replica's own initializes never close one of them at all.
type inUse struct {
	mu sync.Mutex
	m  map[[32]byte]*use
}

// use is one session's requests in flight here.
type use struct {
	n     int
	seen  time.Time     // when this replica last read or wrote the session's last_seen_at
	every time.Duration // IdleWrite/2 when the session came into use
	timer *time.Timer   // the next keepSeen
}

// hold counts a request of s in flight, whose last_seen_at was seen at seen, until the returned func runs.
func (h *Handler) hold(s *Session, seen time.Time) func() {
	b := &h.busy
	b.mu.Lock()
	u := b.m[s.Hash]
	if u == nil {
		u = &use{seen: seen, every: IdleWrite / 2}
		hash := s.Hash
		u.timer = time.AfterFunc(u.every, func() { h.keepSeen(hash, u) })
		b.m[s.Hash] = u
	}
	u.n++
	b.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { h.release(s.Hash, u) }) }
}

// keepSeen writes the last_seen_at of a session in use (Touch writes it at most once per IdleWrite) and runs again
// IdleWrite/2 later, until the session's last request here ends.
func (h *Handler) keepSeen(hash [32]byte, u *use) {
	now := h.deps.now()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	err := h.deps.Sessions.Touch(ctx, hash, now)
	cancel()
	if err != nil {
		h.log.Debug("mcp: the last_seen_at of a session in use was not written", "error", err)
	}
	b := &h.busy
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.m[hash] != u {
		return // its last request ended meanwhile
	}
	if err == nil {
		u.seen = now
	}
	u.timer.Reset(u.every)
}

// release ends one request of a session; the last one stops keepSeen and writes last_seen_at when this replica has
// not for IdleWrite.
func (h *Handler) release(hash [32]byte, u *use) {
	b := &h.busy
	b.mu.Lock()
	if u.n--; u.n > 0 {
		b.mu.Unlock()
		return
	}
	if b.m[hash] == u {
		delete(b.m, hash)
	}
	u.timer.Stop()
	seen := u.seen
	b.mu.Unlock()
	if now := h.deps.now(); now.Sub(seen) >= IdleWrite {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := h.deps.Sessions.Touch(ctx, hash, now); err != nil {
				h.log.Debug("mcp: last_seen_at not written", "error", err)
			}
		}()
	}
}

// busyHashes are the sessions with a request or a stream in flight on this replica.
func (h *Handler) busyHashes() [][32]byte {
	b := &h.busy
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([][32]byte, 0, len(b.m))
	for k := range b.m {
		out = append(out, k)
	}
	return out
}
