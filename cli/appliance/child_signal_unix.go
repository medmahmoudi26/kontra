//go:build unix

package appliance

import (
	"errors"
	"syscall"
)

// signalGroup signals the whole process group, which is the child and everything it started.
//
// The NEGATIVE pid is the whole mechanism: `kill(-pgid, sig)` delivers to every member, and the
// group id is the child's own pid because childProcAttr asked for Setpgid. One character between
// "stop the orchestrator" and "stop the orchestrator and anything it forked", and the second one
// is what makes an orphan impossible rather than unlikely.
func signalGroup(pid int, sig syscall.Signal) error {
	if pid <= 0 {
		return syscall.ESRCH
	}
	return syscall.Kill(-pid, sig)
}

// groupAlive asks the kernel whether ANY process is still in the group.
//
// Signal 0 is the question rather than an order: no error means somebody is there, ESRCH means
// nobody is, and EPERM means somebody is there and is not ours — still alive, so still an orphan,
// and reporting it as gone would be the one answer that is never safe.
func groupAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(-pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// isGone is "there is nothing there to signal" — a race with a child that exited between the
// check and the kill, not a failure to stop it.
func isGone(err error) bool { return errors.Is(err, syscall.ESRCH) }
