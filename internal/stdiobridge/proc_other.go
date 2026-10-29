//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly || illumos)

package stdiobridge

import (
	"os"
	"os/exec"
	"syscall"
)

// inGroup does nothing where process groups do not exist: the child is stopped alone, and what it started may
// outlive it (D184 holds on Unix only).
func inGroup(*exec.Cmd) {}

// signalGroup signals the child itself.
func signalGroup(pid int, sig syscall.Signal) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if sig == sigKill {
		return p.Kill()
	}
	return p.Signal(sig)
}

// groupAlive cannot see a group: once the child itself has exited (the caller checks that first), nothing is left
// that the bridge can find.
func groupAlive(int) bool { return false }

const (
	sigTerm = syscall.SIGTERM
	sigKill = syscall.SIGKILL
)
