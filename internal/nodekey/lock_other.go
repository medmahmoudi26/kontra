//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly || illumos)

package nodekey

import (
	"errors"
	"os"
)

// lockFile refuses: without flock(2), Rotate, MarkRotated and Prune in two processes on one key could
// lose a rotated key, so they do not run.
func lockFile(*os.File) error {
	return errors.New("rotating or pruning the key file needs flock(2), which this platform lacks")
}
