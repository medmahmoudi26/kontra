//go:build !linux

package main

// driver_proctable_other.go — everywhere that is not Linux, the `process` driver cannot enumerate.
//
// A REFUSAL AND NOT AN EMPTY SLICE. `list` is the read a reconcile loop trusts (driver.go), so an
// empty answer from a machine full of Workers is worse than no answer at all: it says "nothing is
// running", and the caller acts on it by starting rivals. `kontra release` ships darwin binaries and
// this is what they get.
//
// It costs nothing that matters. A **Machine** runs Linux, every container ADR 0036 puts a Worker in
// runs Linux, and the developer path on a mac — `kontra serve --actor <dir>` — uses `start` and
// `stop`, which are portable. When a driver has to enumerate on darwin, `ps -E` is where to start
// and its output is the reason this is not written speculatively: the environment column is
// truncated and SIP-restricted, so it needs measuring rather than assuming.

import (
	"fmt"
	"runtime"
)

type labelledProc struct {
	pid   int
	ppid  int
	label string
}

func labelledProcs() ([]labelledProc, error) {
	return nil, fmt.Errorf("the `process` driver reads the process table through /proc and cannot enumerate Workers on %s — "+
		"start and stop work here, list does not", runtime.GOOS)
}
