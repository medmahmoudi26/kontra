package main

import "testing"

// A dash and a zero must NOT look the same. "no heartbeat yet" being rendered as 0 would
// reintroduce exactly the ambiguity this column exists to remove.
func TestIsolatedColDistinguishesUnknownFromZero(t *testing.T) {
	if got := isolatedCol(monHeartbeat{}, false); got != "-" {
		t.Errorf("no heartbeat should render %q, got %q", "-", got)
	}
	if got := isolatedCol(monHeartbeat{Isolated: 0}, true); got != "0" {
		t.Errorf("known-zero should render %q, got %q", "0", got)
	}
}

// Loss must be visually loud — a bare number in a wide table is missable, and this is the
// number whose absence let a wiped run read as a success.
func TestIsolatedColFlagsLoss(t *testing.T) {
	got := isolatedCol(monHeartbeat{Isolated: 1582}, true)
	if got != "1582 !" {
		t.Errorf("loss should be flagged, got %q", got)
	}
}
