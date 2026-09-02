//go:build unix && !linux

package appliance

import "syscall"

// childProcAttr puts the child in its own process group, which is what Child.Stop signals.
//
// NO PDEATHSIG HERE, AND IT IS NOT AN OVERSIGHT. `PR_SET_PDEATHSIG` is a Linux prctl; macOS and
// the BSDs have no equivalent, so a `kill -9` of this binary on those platforms can leave the
// child running — see child_linux.go for what that costs. The orderly paths are identical: Stop
// signals the group, waits, escalates and then checks the group is empty. What is missing is only
// the backstop for a supervisor that was given no chance to run.
func childProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}
