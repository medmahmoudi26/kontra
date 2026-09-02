//go:build linux

package appliance

import "syscall"

// childProcAttr puts the child in its own process group AND asks the kernel to kill it if this
// binary dies.
//
// SETPGID IS THE SHUTDOWN LADDER'S SUBJECT. The group is what `Child.Stop` signals, so a
// grandchild the orchestrator forked cannot survive the parent it was forked from. The group id
// equals the child's pid, which is why every message here can say "pgid <pid>".
//
// PDEATHSIG IS THE `kill -9` CASE, and it is the one an orderly shutdown cannot cover. `kontra
// up` killed with SIGKILL runs no deferred Stop and no signal handler; without this the Node
// process keeps running, keeps port 8088, and keeps the DuckLake catalog lock — and the next
// `kontra up` is refused by `catalogLock.ts` with the pid of a process the operator never
// started. `catalogLock.ts` steals a lock whose holder is DEAD, so making the holder die is
// exactly what turns that refusal back into a clean start.
//
// IT IS A THREAD-DEATH SIGNAL, NOT A PROCESS-DEATH ONE, which is the trap. Linux delivers
// PR_SET_PDEATHSIG when the parent THREAD exits, not when the parent process does — so a Go
// runtime that retired the thread `cmd.Start()` ran on would kill a perfectly healthy child for
// no reason. {@link StartChild} pins that thread with runtime.LockOSThread for the child's whole
// lifetime, which is what makes this flag safe to set here.
//
// SIGKILL rather than SIGTERM, deliberately: this fires only when the supervisor is already gone,
// so there is nobody left to read the graceful shutdown's output or to escalate if it hangs. The
// clean path — SIGTERM, the catalog lock released, exit 143 — is Stop's, and it runs first
// whenever there is a supervisor alive to run it.
func childProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}
