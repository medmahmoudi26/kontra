//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly || illumos

package stdiobridge

import (
	"errors"
	"os/exec"
	"syscall"
)

// inGroup starts the child as the leader of a process group of its own (D184), so that the group can be signalled
// whole: an npx launcher's node, and anything else the child starts, is in it.
func inGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// signalGroup sends sig to every process of the group pgid.
func signalGroup(pgid int, sig syscall.Signal) error {
	return syscall.Kill(-pgid, sig)
}

// groupAlive reports whether any process is left in the group pgid (a zombie not yet reaped counts; a process the
// node may not signal counts too).
func groupAlive(pgid int) bool {
	err := syscall.Kill(-pgid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

const (
	sigTerm = syscall.SIGTERM
	sigKill = syscall.SIGKILL
)
