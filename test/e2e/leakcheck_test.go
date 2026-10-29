package e2e

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The harness's own `npm ci` carries the run's marker variables (D185), so that scripts/test_e2e.sh finds an install
// that outlived a test binary which died during it (a go test -timeout panic, a crash): npm runs in a process group
// of its own, with a minimal environment, under a command line that does not name the run's directory.
func TestNpmInstallCarriesTheMarkers(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the harness runs on Linux")
	}
	envBin, err := exec.LookPath("env")
	if err != nil {
		t.Skip("no env(1)")
	}
	if _, err := TempRoot(); err != nil { // before SessionEnv may change: TempRoot reads it once
		t.Fatal(err)
	}
	session := os.Getenv(SessionEnv)
	if session == "" {
		session = "e2e-unit-" + randomHex(4)
		t.Setenv(SessionEnv, session)
	}
	tmp := t.TempDir()
	bin, pkg := filepath.Join(tmp, "bin"), filepath.Join(tmp, "pkg")
	for _, d := range []string{bin, pkg} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range map[string]string{
		filepath.Join(pkg, "package.json"):      "{}\n",
		filepath.Join(pkg, "package-lock.json"): "{}\n",
		filepath.Join(bin, "npm"):               "#!/bin/sh\nexec " + envBin + " > npm.env\n", // run in the install's directory
		filepath.Join(bin, "node"):              "#!/bin/sh\nexit 0\n",
	} {
		if err := os.WriteFile(name, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
	root, err := filepath.Abs(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	src, err := filepath.Rel(root, pkg) // npmInstall reads a directory relative to the repository's root
	if err != nil {
		t.Fatal(err)
	}
	dir, err := npmInstall(src)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "npm.env"))
	if err != nil {
		t.Fatal(err)
	}
	env := strings.Split(strings.TrimSpace(string(b)), "\n")
	var run, sess bool
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		run = run || k == RunEnv && v != ""
		sess = sess || k == SessionEnv && v == session
	}
	if !run || !sess {
		t.Fatalf("npm ci's environment lacks %s or %s=%s: %q", RunEnv, SessionEnv, session, env)
	}
}

// scripts/test_e2e.sh finds a leftover by its working directory too: a process whose cwd is under the run's directory
// is reported and killed even when it carries no marker variable, runs a binary from elsewhere, leads a process group
// of its own and has a command line that does not name the directory, as an `npm ci` of a test binary that died
// mid-install does.
func TestLeakCheckFindsAProcessByItsDirectory(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the leak check reads /proc")
	}
	for _, tool := range []string{"bash", "setsid", "sleep", "env"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("no %s", tool)
		}
	}
	tmp := t.TempDir()
	pidFile := filepath.Join(tmp, "pid")
	stub := filepath.Join(tmp, "go")
	// The stand-in for `go test` leaves one such process behind, then succeeds.
	script := `#!/usr/bin/env bash
d="${TMPDIR:-/tmp}/kontra-e2e-$KONTRA_E2E_SESSION/npm-everything-1"
mkdir -p "$d" && cd "$d" || exit 1
env -i PATH=/usr/bin:/bin setsid sleep 300 </dev/null >/dev/null 2>&1 &
echo $! >"$PID_FILE"
exit 0
`
	if err := os.WriteFile(stub, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", filepath.Join(repoRoot, "scripts", "test_e2e.sh"))
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "TMPDIR=" + tmp, "GO=" + stub,
		TemporalCLIEnv + "=unused", "PID_FILE=" + pidFile}
	out, runErr := cmd.CombinedOutput()
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("the stub left no pid (%v): %s", err, out)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 1 {
		t.Fatalf("the stub's pid %q", b)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	var ee *exec.ExitError
	if !errors.As(runErr, &ee) {
		t.Fatalf("the script with a leftover: %v, want a failed run\n%s", runErr, out)
	}
	if want := fmt.Sprintf("outlived the run (pid %d)", pid); !strings.Contains(string(out), want) {
		t.Fatalf("the script did not report the leftover by its working directory (%q):\n%s", want, out)
	}
	deadline := time.Now().Add(5 * time.Second)
	for alive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("pid %d is still running after the script", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// alive reports whether pid runs (a zombie, killed and not yet reaped, does not).
func alive(pid int) bool {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	s := string(b)
	if i := strings.LastIndexByte(s, ')'); i >= 0 && i+2 < len(s) {
		return s[i+2] != 'Z' && s[i+2] != 'X'
	}
	return true
}
