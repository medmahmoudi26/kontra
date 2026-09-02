//go:build unix

package cas

import (
	"errors"
	"syscall"
)

// isNoSpace covers both ways a filesystem says "no room": the device is full, or the user's
// quota is spent. They read identically to a caller and differently to an operator, so both
// map to ErrDiskFull and the errno survives in the wrapped error.
func isNoSpace(err error) bool {
	return errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT)
}

func freeBytes(dir string) (uint64, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, false
	}
	// Bavail, not Bfree: the reserved blocks are not ours to spend.
	return uint64(st.Bsize) * uint64(st.Bavail), true
}
