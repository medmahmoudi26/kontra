package node

import "time"

// An anonymous session's end, as the node tells it (spec/tunnel.md §6.1, D292). welcome.anonymous.expires_at is the
// gate's time, and the node's wall clock may differ from the gate's by more than expirySlack: compared with it, the
// gate's {"t": "revoked"} at the end would read as a revocation of the key (the M4 WP-03 review). So the node measures
// the end on its monotonic clock from the session's opening: first by its own wall clock at the welcome, then from the
// gate's {"t": "expiring"}, which the gate sends expiringNotice before the end by its own clock (at once when less is
// left), so a skew between the clocks stops counting once it arrived; and a {"t": "revoked"} without a service after
// it is the end. A revocation of the key in the session's last expiringNotice therefore reads as its end too: the node
// stops either way, and its next hello learns `revoked`.

// expiringNotice is how long before an anonymous session's end the gate sends {"t": "expiring"} (§6.1: ten minutes,
// or at once when less is left).
const expiringNotice = 10 * time.Minute

// setAnonymousEnd records an anonymous session's end at its welcome: at is welcome.anonymous.expires_at, now the node's
// wall clock then (Node.now). The end is kept as a duration from session.opened.
func (s *session) setAnonymousEnd(at, now time.Time) {
	s.anonEnd = at
	s.anonLeft.Store(int64(at.Sub(now)))
}

// noteExpiring is the gate's {"t": "expiring"}, which came after since the session opened. From it the end is at most
// expiringNotice away, and exactly that when it came later than the gate's at-once message would (expirySlack after the
// welcome): the gate's timer sent it.
func (s *session) noteExpiring(after time.Duration) {
	if s.anonEnd.IsZero() {
		return
	}
	left := after + expiringNotice
	if after < expirySlack {
		left = min(left, time.Duration(s.anonLeft.Load()))
	}
	s.anonLeft.Store(int64(left))
	s.expiring.Store(true)
}

// anonymousOver reports whether an anonymous session has reached its end by the node's estimate (within expirySlack):
// a close then is its end, not one to redial.
func (s *session) anonymousOver() bool {
	return !s.anonEnd.IsZero() && time.Since(s.opened) >= time.Duration(s.anonLeft.Load())-expirySlack
}

// revokeEndsAnonymous reports whether a {"t": "revoked"} without a service is an anonymous session's end rather than a
// revocation of the key: once the gate's expiring arrived, or at the end by the node's estimate.
func (s *session) revokeEndsAnonymous() bool {
	return !s.anonEnd.IsZero() && (s.expiring.Load() || s.anonymousOver())
}

// controlDrain bounds how long Run, seeing an anonymous session closed, waits for its control stream's reader to read
// what the gate sent before the close (drainControl).
const controlDrain = 2 * time.Second

// drainControl waits, at most controlDrain, until the session's control stream reader has returned. The gate sends
// {"t": "revoked"} at an anonymous session's end and then closes it; the node's yamux has the message before it sees
// the close, but Run may see the close before the reader has read the message, and would then take the close for a
// drop and redial (opening another anonymous session) where the message says the session is over.
func (s *session) drainControl() {
	if s.controlDone == nil || !s.control.Load() {
		return
	}
	t := time.NewTimer(controlDrain)
	defer t.Stop()
	select {
	case <-s.controlDone:
	case <-t.C:
	}
}
