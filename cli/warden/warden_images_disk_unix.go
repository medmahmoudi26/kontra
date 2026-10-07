//go:build unix

package warden

// warden_images_disk_unix.go — how much room is left on the filesystem the image store is on.
//
// THIS IS THE FIRST DISK-SPACE READ IN THE WARDEN, and it is the only one. Nothing else in this package
// has ever asked; the two `Statfs` call sites elsewhere in the repository
// (`cli/appliance/bundle/stage.go`, `runtime/handler/internal/cas`) both refuse work rather than
// reclaim space. The value here is a TRIGGER and never a quota: it decides when a prune happens, never
// what the prune removes (see `prunePlan`, which keeps the same policy under pressure).

import "syscall"

// diskFreePercent is free space as a percentage, and whether the question could be answered at all.
//
// UNREADABLE IS NOT FULL. A path that cannot be stat'ed returns false, and `pruneDue` then falls back
// to the timer alone — a `(0, true)` would make every single turn a prune turn on a Machine whose
// engine root moved.
//
// `Bavail` AND NOT `Bfree`, so the number matches the "Avail" column of `df` that an operator is
// reading in the other window. On a Machine where the Warden is root the two differ by the reserved
// blocks, which makes this the conservative reading: a prune a little early costs nothing.
func diskFreePercent(path string) (float64, bool) {
	if path == "" {
		return 0, false
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, false
	}
	block := float64(uint64(st.Bsize))
	total := float64(st.Blocks) * block
	if total <= 0 {
		return 0, false
	}
	return float64(st.Bavail) * block / total * 100, true
}
