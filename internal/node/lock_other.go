//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly || illumos)

package node

import "os"

// lockFile does nothing where flock(2) does not exist. Two `kontra connect` processes (one per gate,
// §2.1) that rewrite ~/.kontra/credentials at the same moment can then lose one's change: at worst a
// token that a welcome confirmed is sent once more and refused enroll_invalid. Refusing to save would
// stop the node on those platforms altogether.
func lockFile(*os.File) error { return nil }
