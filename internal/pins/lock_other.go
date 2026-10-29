//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly || illumos)

package pins

import (
	"errors"
	"os"
)

// lockFile refuses: without flock(2), two processes recording pins at once could lose one's pin, so the file
// is never rewritten (D153's rule for the key file). Reading works.
func lockFile(*os.File) error {
	return errors.New("recording a pin needs flock(2), which this platform lacks")
}
