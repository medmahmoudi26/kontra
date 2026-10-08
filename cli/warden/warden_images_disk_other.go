//go:build !unix

package warden

// The non-unix answer, which is "this platform is not asked".
//
// A Machine is a Linux host (ADR 0037's provisioning, and `machine.ts`'s cloud-init), so there is no
// platform here to write a second implementation for. What this file buys is that `cli/warden` still
// COMPILES everywhere — `driver_proctable_other.go` exists for the same reason — and that the failure,
// if a Warden ever ran somewhere else, is the safe one: no disk reading, so only the timer brings a
// prune forward and nothing is ever removed because of a number nobody could read.
func diskFreePercent(string) (float64, bool) { return 0, false }
