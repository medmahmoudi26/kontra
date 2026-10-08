package main

// deploylock.go — one build of one actor version at a time, per machine.
//
// TWO DEPLOYS OF THE SAME ACTOR RACE, AND THE INSTALL MAKES IT THE NORMAL CASE. `workspace watch`
// runs `kontra deploy --override` for every actor directory it sees (workspace.go), so an operator
// or a gate that also runs `kontra deploy` on an actor under the watched tree has two builds of the
// same image in flight. They share everything the lifecycle keys on the image name: the build cache
// volume, the launch cache volume, and the tag at the far end.
//
// MEASURED, AS A FAILURE THAT BLAMES THE WRONG THING:
//
//	[exporter] ERROR: failed to export: saving image: failed to commit cache: committing cache:
//	           rename /launch-cache/staging /launch-cache/committed: no such file or directory
//	ERROR: failed to build: executing lifecycle: failed with status code: 62
//
// after a build that had detected, installed, exported every layer and printed `Saving …`. That is
// the loser of a race finding `staging` already renamed by the winner — nothing in the message says
// "concurrent", and the same deploy run alone succeeds, which is how it reads as flakiness in the
// lifecycle rather than as two processes doing one job.
//
// BLOCKING, NOT REFUSING. The waiter is usually the watcher, whose next turn would retry anyway, and
// what it finds after the wait is the steady state it already knows how to read: `already deployed`
// (watchDeploySettled). A refusal here would turn one redundant build into a reported failure.
//
// FLOCK AND NOT A LOCK FILE'S EXISTENCE. An advisory lock is released by the kernel when the holder
// exits, however it exits; an O_EXCL lock file outlives a SIGKILL and the next deploy has to decide
// how stale is stale — which is a second thing to be wrong about on a path that already has one.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// lockDeploy serializes builds of `<name>:<version>` on this machine and returns the release.
//
// The lock file sits beside the staged build context — the workspaces tree when there is one, so
// that two processes in DIFFERENT containers sharing that mount still see one lock, which is the
// case the install actually has (`cli` watches, `orchestrator-infra` builds on demand).
func lockDeploy(name, version string) (func(), error) {
	path := filepath.Join(lockDir(), deployLockName(name, version))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		// NOT FATAL. A read-only workspaces mount is a legitimate configuration, and refusing to
		// deploy because a lock could not be created would trade a rare race for a certain failure.
		return func() {}, nil
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return func() {}, nil
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

func lockDir() string {
	if d := stageParent(); d != "" {
		return d
	}
	return os.TempDir()
}

// deployLockName is one file per actor VERSION, because that is what the caches and the tag are
// keyed on: two different versions of one actor share nothing the lifecycle would rename.
func deployLockName(name, version string) string {
	safe := func(s string) string {
		return strings.Map(func(r rune) rune {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
				return r
			default:
				return '-'
			}
		}, s)
	}
	return fmt.Sprintf(".kontra-deploy-%s-%s.lock", safe(name), safe(version))
}
