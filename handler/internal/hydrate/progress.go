package hydrate

import (
	"io"
	"sync"
	"time"
)

// A HYDRATION THAT SAYS NOTHING FOR THIRTY SECONDS IS INDISTINGUISHABLE FROM A HANG.
//
// The measured artifact is 125 MB compressed, 457 MB expanded over 31,823 files, and every one
// of those numbers is a stretch of wall-clock time during which the only honest thing a binary
// can do is say what it is doing. Issue 14 asks for a progress line on first run; this is the
// half of it that only this package can see, because the phases are here — read the bytes,
// expand them once, clone the tree — and the caller is the half that decides how to draw it.
//
// PUSH, NOT POLL, and coalesced HERE rather than at the caller. A callback per file is 31,823
// calls that each cost a lock and a Fprintf on somebody else's terminal; a callback per 500 ms
// is a line a human reads. The floor lives in this file so that every caller gets the same
// rate — a second caller that forgot to coalesce would be a progress reporter slower than the
// work it reports on.

// Phase is which part of a hydration is running. Three, because they fail differently and take
// different amounts of time: a network (or a disk read) with a size, an expansion with a size,
// and a file-by-file clone with a count.
type Phase string

const (
	// PhaseFetch is bytes arriving into the store — over HTTP, or off a local file.
	PhaseFetch Phase = "fetch"
	// PhaseExpand is the one-time expansion of an archive into its golden tree.
	PhaseExpand Phase = "expand"
	// PhaseMaterialize is the copy-on-write clone of that tree into a working directory.
	PhaseMaterialize Phase = "materialize"
)

// Progress is one report. Total is 0 when nothing knows it — an HTTP response with no
// Content-Length, or a tar whose member count is only knowable by reading it — and a caller
// that renders a percentage from an unknown total is drawing a number it made up.
type Progress struct {
	Artifact string
	Phase    Phase
	Bytes    int64
	Files    int
	Total    int64
	Elapsed  time.Duration

	// Final marks the last report of a phase. A caller that throttles further — and one should,
	// because a terminal scrolling sixty lines has said less than one scrolling fifteen — must
	// let this one through, or the number a reader is left looking at is whichever partial total
	// the floor happened to admit. Measured: a 120 MB local read reported "32.0 KB of 119.9 MB"
	// and then nothing, which reads as a stall rather than as a phase that finished.
	Final bool
}

// SetProgress installs the reporter. nil disables reporting, which is the zero value and what
// every existing caller gets.
//
// Not part of Open's signature because the store outlives any one hydration and most callers
// want nothing: an appliance's first run wants a line per phase, a test wants silence, and
// issue 15's repair path will want the same reporter for a re-hydration it did not ask for.
func (s *Store) SetProgress(fn func(Progress)) { s.progress = fn }

// reporter is one phase's rate-limited channel back to the caller. The zero-ish value (a nil
// fn) is a no-op that costs one comparison per call, so the hot paths do not branch on it.
type reporter struct {
	fn       func(Progress)
	artifact string
	phase    Phase
	total    int64
	started  time.Time
	mu       sync.Mutex
	last     time.Time
	bytes    int64
	files    int
}

// progressFloor is the shortest gap between two reports of the same phase. 500 ms is fast
// enough that a 30-second hydration draws sixty lines' worth of movement and slow enough that
// the reporting is never the bottleneck.
const progressFloor = 500 * time.Millisecond

func (s *Store) reportFor(artifact string, phase Phase, total int64) *reporter {
	return &reporter{fn: s.progress, artifact: artifact, phase: phase, total: total, started: time.Now()}
}

// add records work and reports it if the floor has passed.
func (r *reporter) add(bytes int64, files int) {
	if r == nil || r.fn == nil {
		return
	}
	r.mu.Lock()
	r.bytes += bytes
	r.files += files
	if time.Since(r.last) < progressFloor {
		r.mu.Unlock()
		return
	}
	r.last = time.Now()
	p := r.snapshot()
	r.mu.Unlock()
	r.fn(p)
}

// done reports the final state of a phase, ignoring the floor. Without it a phase that
// finished quickly would report nothing at all, and the last line of a slow one would be
// whatever the floor happened to let through rather than the total.
func (r *reporter) done() {
	if r == nil || r.fn == nil {
		return
	}
	r.mu.Lock()
	p := r.snapshot()
	p.Final = true
	r.mu.Unlock()
	r.fn(p)
}

// snapshot builds a Progress. Caller holds the lock.
func (r *reporter) snapshot() Progress {
	return Progress{
		Artifact: r.artifact,
		Phase:    r.phase,
		Bytes:    r.bytes,
		Files:    r.files,
		Total:    r.total,
		Elapsed:  time.Since(r.started),
	}
}

// countingReader reports bytes as they stream past on their way into the store. It sits
// BETWEEN the source and the hash, so what it counts is what was actually read — a truncated
// transfer stops reporting where it stopped, rather than at the size somebody promised.
type countingReader struct {
	r io.Reader
	p *reporter
}

func (c *countingReader) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	if n > 0 {
		c.p.add(int64(n), 0)
	}
	return n, err
}
