package node

import (
	"math/rand/v2"
	"time"
)

// Reconnect backoff (§13).
const (
	BackoffMin   = 1 * time.Second
	BackoffMax   = 60 * time.Second
	BackoffReset = 60 * time.Second // a session that lived this long resets the backoff
)

// Backoff is exponential from BackoffMin to BackoffMax with full jitter (§13): the n-th wait since the last
// reset (n from 0) is drawn uniformly from [0, min(BackoffMax, 2ⁿ × BackoffMin)). It is reset only by a
// session that lived BackoffReset or longer, so a node whose sessions keep dropping soon after the welcome
// backs off as far as one whose dials fail.
//
// The zero value is ready to use. It is not safe for concurrent use.
type Backoff struct {
	attempt int
	rand    func() float64 // [0, 1); math/rand/v2's Float64 when nil
}

// Next is the wait before the next dial.
func (b *Backoff) Next() time.Duration {
	ceil := BackoffMin
	for i := 0; i < b.attempt && ceil < BackoffMax; i++ {
		ceil *= 2
	}
	ceil = min(ceil, BackoffMax)
	if ceil < BackoffMax {
		b.attempt++ // stops growing once the ceiling is reached
	}
	r := b.rand
	if r == nil {
		r = rand.Float64
	}
	f := r()
	if f < 0 || f >= 1 {
		f = 0
	}
	return time.Duration(f * float64(ceil))
}

// Ceiling is the upper bound of the next wait, for a log line.
func (b *Backoff) Ceiling() time.Duration {
	ceil := BackoffMin
	for i := 0; i < b.attempt && ceil < BackoffMax; i++ {
		ceil *= 2
	}
	return min(ceil, BackoffMax)
}

// SessionEnded records how long the last session lived; BackoffReset or more resets the backoff.
func (b *Backoff) SessionEnded(lived time.Duration) {
	if lived >= BackoffReset {
		b.attempt = 0
	}
}
