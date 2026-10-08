package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// WHAT THIS PINS. `workspace watch` runs `kontra deploy --override` for every actor directory it
// sees, so the parity gate — which creates an actor under the watched tree and then deploys it —
// had TWO lifecycles building one image. They share the volumes pack keys on the image name, and
// the loser reported
//
//	failed to commit cache: committing cache: rename /launch-cache/staging /launch-cache/committed:
//	no such file or directory
//
// after a build that had exported every layer. Nothing in that sentence says "concurrent", and the
// same deploy alone succeeds — which is how a race reads as flakiness in somebody else's tool.

func TestTwoDeploysOfOneVersionDoNotOverlap(t *testing.T) {
	t.Setenv("KONTRA_WORKSPACES", t.TempDir())

	first, err := lockDeploy("probe", "0.2.0")
	if err != nil {
		t.Fatal(err)
	}

	// A SECOND PROCESS, not a second goroutine: flock is per open file description, so two
	// goroutines in one process would both be granted it and the test would pass while the real
	// case — the watcher and a gate, two `kontra` processes — still raced.
	done := make(chan error, 1)
	go func() { done <- waitForLockInChildProcess(t, os.Getenv("KONTRA_WORKSPACES")) }()

	select {
	case err := <-done:
		t.Fatalf("the second deploy acquired the lock while the first held it (%v)", err)
	case <-time.After(400 * time.Millisecond):
		// Still blocked, which is the point: it waits rather than building beside the holder.
	}

	first()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the waiter must proceed once the lock is released: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the waiter never acquired the lock after it was released")
	}
}

// waitForLockInChildProcess re-runs this test binary as a helper that takes the same lock.
func waitForLockInChildProcess(t *testing.T, workspaces string) error {
	cmd := exec.Command(os.Args[0], "-test.run=TestDeployLockHelperProcess")
	cmd.Env = append(os.Environ(), "KONTRA_DEPLOY_LOCK_HELPER=1", "KONTRA_WORKSPACES="+workspaces)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("helper: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if !strings.Contains(string(out), lockHelperMarker) {
		return fmt.Errorf("helper did not report acquiring the lock: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// Printed rather than t.Log'd: the child runs without -test.v, which discards t.Log.
const lockHelperMarker = "helper acquired the deploy lock"

func TestDeployLockHelperProcess(t *testing.T) {
	if os.Getenv("KONTRA_DEPLOY_LOCK_HELPER") == "" {
		t.Skip("helper for TestTwoDeploysOfOneVersionDoNotOverlap")
	}
	release, err := lockDeploy("probe", "0.2.0")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	fmt.Println(lockHelperMarker)
}

func TestADifferentVersionIsNotBlocked(t *testing.T) {
	// The caches and the tag are keyed on the version too, so two versions of one actor share
	// nothing the lifecycle renames — serialising them would make a deploy wait for a build it
	// cannot collide with.
	t.Setenv("KONTRA_WORKSPACES", t.TempDir())
	a, err := lockDeploy("probe", "0.2.0")
	if err != nil {
		t.Fatal(err)
	}
	defer a()
	done := make(chan struct{})
	go func() {
		b, err := lockDeploy("probe", "0.3.0")
		if err == nil {
			b()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("0.3.0 blocked on 0.2.0's lock")
	}
}

func TestTheLockFileIsDotPrefixedAndNamedForTheVersion(t *testing.T) {
	ws := t.TempDir()
	t.Setenv("KONTRA_WORKSPACES", ws)
	release, err := lockDeploy("probe", "0.2.0")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	// Dot-prefixed because it lives in the workspaces tree, which `workspace watch` and
	// `workspace list` enumerate — both skip a leading dot, and a lock file read as a workspace
	// would be deployed.
	name := deployLockName("probe", "0.2.0")
	if !strings.HasPrefix(name, ".") {
		t.Errorf("%q would be enumerated as a workspace", name)
	}
	if _, err := os.Stat(filepath.Join(ws, name)); err != nil {
		t.Errorf("the lock is not where the staged context is: %v", err)
	}
	// An actor name the filesystem cannot hold must not become a path.
	if got := deployLockName("../../etc/probe", "0.2.0/../x"); strings.Contains(got, "/") {
		t.Errorf("a name must not escape the directory: %q", got)
	}
}

func TestAnUnwritableLockDirectoryDoesNotStopADeploy(t *testing.T) {
	// A read-only workspaces mount is a legitimate configuration. Trading a rare race for a certain
	// failure would be the wrong way round.
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	t.Setenv("KONTRA_WORKSPACES", dir)
	release, err := lockDeploy("probe", "0.2.0")
	if err != nil {
		t.Fatalf("an unwritable lock directory must not be an error: %v", err)
	}
	release()
}
