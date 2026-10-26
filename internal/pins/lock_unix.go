//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly || illumos

package pins

import (
	"errors"
	"os"
	"syscall"
)

// lockFile takes an exclusive flock(2) on f, waiting for it. A flock belongs to the open file, so two
// opens in one process exclude each other as two processes do. (internal/node and internal/nodekey have the
// same helper; pins keeps its own because internal/node imports pins.)
func lockFile(f *os.File) error {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}
