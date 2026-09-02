// banner_test.go — the banner is printed by main(), before dispatch, so the only way to see what
// a caller sees is to RUN the binary: these are the package's only subprocess tests. The build
// costs a few seconds against a warm cache and is shared by every test in the file.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// initializeFrame is the real first line of an MCP session — what an agent sends down stdio.
const initializeFrame = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"banner_test","version":"0"}}}` + "\n"

// THE BUILD OUTPUT IS A DETERMINISTIC PATH AND THE SWEEP RUNS BEFORE THE TESTS, and both facts
// exist because of how this process DIES rather than how it runs.
//
// This used to build into `os.MkdirTemp("", "kontra-banner")` and remove the directory after
// `m.Run()` returned. `m.Run()` does not return when the package hits Go's timeout — `panic: test timed out`
// takes the process down — and it does not return for a SIGKILL either. Every such run stranded a
// 193 MB binary. Measured 2026-08-28: six of them in /tmp, ~1.2 GB, on a box with 1.3 GB free, and
// disk exhaustion here is the silent failure (SeaweedFS answers a bare HTTP 500 on every write).
// The timeouts are not exotic: `go test ./cli/...` takes ~21 minutes on this box against a 10-minute
// default package timeout, so the panic path is the common path.
//
// So nothing about cleanup may depend on the process ending politely, which rules out `defer` in
// `TestMain` as well — the timeout panic is not catchable from there.
//
//   - ONE DIRECTORY PER CHECKOUT (`bannerBinDir`). A killed run's tree is the next run's tree, so
//     repeated interruptions OVERWRITE rather than accumulate. Bounded at one copy however the
//     process dies, with no signal handling.
//   - THE SWEEP RUNS FIRST (`sweepStrandedBinaries`). It reclaims the `MkdirTemp` strands this
//     already left behind, and the deterministic trees of checkouts that have gone away.
//
// The `RemoveAll` after `m.Run()` is kept: it is not the fix, it is the happy path staying tidy on
// a box that lives near full.
const bannerBinPrefix = "kontra-banner"

// A stranded tree is reclaimed once it is older than this. The window has to clear the longest a
// LIVE run could legitimately be sitting on its own directory: the build happens at the first
// banner test and the package can then run for its full timeout, which is `-timeout 40m` here.
// Six hours is that with room to spare, and still short enough that /tmp does not grow across a
// working day. Erring long is the cheap direction — the cost of waiting is one extra tree, the cost
// of sweeping too eagerly is deleting the binary out from under somebody else's run.
const bannerBinStaleAfter = 6 * time.Hour

var (
	kontraBinMu   sync.Mutex
	kontraBinPath string
	kontraBinErr  error
	kontraBinDir  string // removed by TestMain on the happy path; empty when nothing built
)

// bannerBinBase is where the tree lives. GOTMPDIR first: concurrent agents on this box each set a
// private one precisely so their builds stop colliding, and this is a build output.
func bannerBinBase() string {
	if dir := os.Getenv("GOTMPDIR"); dir != "" {
		return dir
	}
	return os.TempDir()
}

// bannerBinDir names one directory per checkout — same checkout, same name, every run. Hashing the
// package directory rather than using a fixed name is what keeps two worktrees off each other's
// binary; both properties matter and neither survives `MkdirTemp`.
func bannerBinDir(base, pkgDir string) string {
	sum := sha256.Sum256([]byte(pkgDir))
	return filepath.Join(base, bannerBinPrefix+"-"+hex.EncodeToString(sum[:])[:12])
}

// sweepStrandedBinaries removes every `kontra-banner*` tree under base that is not `keep` and has
// not been touched for `staleAfter`. It returns what it removed, so a test can assert it rather
// than argue it. `keep` is skipped by name, so a run never sweeps the tree it is about to build.
func sweepStrandedBinaries(base, keep string, now time.Time, staleAfter time.Duration) []string {
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil
	}
	var swept []string
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), bannerBinPrefix) {
			continue
		}
		full := filepath.Join(base, e.Name())
		if full == keep {
			continue
		}
		info, err := e.Info()
		if err != nil || now.Sub(info.ModTime()) < staleAfter {
			continue
		}
		if os.RemoveAll(full) == nil {
			swept = append(swept, e.Name())
		}
	}
	return swept
}

// kontraBinary builds the CLI once for the whole package.
//
// A mutex and an existence check rather than `sync.Once`: the output path is now shared by every
// run in this checkout, so a sibling run's exit cleanup can take the binary away underneath us. A
// vanished binary is rebuilt instead of being a confusing `exec: no such file`.
func kontraBinary(t *testing.T) string {
	t.Helper()
	kontraBinMu.Lock()
	defer kontraBinMu.Unlock()
	if kontraBinPath != "" {
		if _, err := os.Stat(kontraBinPath); err == nil {
			return kontraBinPath
		}
	}
	kontraBinErr = nil
	// The working directory of a package's test binary is the package's own source directory, so
	// this names the checkout. An unreadable one just means every such checkout shares one tree.
	wd, _ := os.Getwd()
	kontraBinDir = bannerBinDir(bannerBinBase(), wd)
	if kontraBinErr = os.MkdirAll(kontraBinDir, 0o700); kontraBinErr != nil {
		t.Fatalf("building the CLI: %v", kontraBinErr)
	}
	kontraBinPath = filepath.Join(kontraBinDir, "kontra")
	// Build beside the destination and rename onto it. Rename is atomic within the directory, so a
	// sibling run sharing this path sees either the previous whole binary or this one — never the
	// half-written file a plain `go build -o` would leave it exec'ing.
	staging := fmt.Sprintf("%s.building.%d", kontraBinPath, os.Getpid())
	if out, err := exec.Command("go", "build", "-o", staging, ".").CombinedOutput(); err != nil {
		os.Remove(staging)
		kontraBinErr = fmt.Errorf("go build: %v\n%s", err, out)
	} else if err := os.Rename(staging, kontraBinPath); err != nil {
		os.Remove(staging)
		kontraBinErr = fmt.Errorf("installing the built CLI: %w", err)
	}
	if kontraBinErr != nil {
		t.Fatalf("building the CLI: %v", kontraBinErr)
	}
	return kontraBinPath
}

func TestMain(m *testing.M) {
	// BEFORE `m.Run()`. Everything after it is the happy path only.
	wd, _ := os.Getwd()
	base := bannerBinBase()
	sweepStrandedBinaries(base, bannerBinDir(base, wd), time.Now(), bannerBinStaleAfter)

	code := m.Run()

	if kontraBinDir != "" {
		os.RemoveAll(kontraBinDir)
	}
	os.Exit(code)
}

// kontraCmd is `kontra <args...>` with this installation OUT of the way: a real ~/.kontra with a
// malformed config.yaml puts a `warning:` line on the very stderr these tests read.
func kontraCmd(t *testing.T, args ...string) *exec.Cmd {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.Command(kontraBinary(t), args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"HOME="+dir,
		"KONTRA_HOME="+filepath.Join(dir, ".kontra"),
	)
	return cmd
}

// A `kontra mcp` session is stdio JSON-RPC for as long as the agent lives, and the agent keeps
// this process's stderr as that server's log. Nothing may land there that the CLI did not have a
// reason to say — least of all four lines of ASCII art at the top of every session.
func TestMCPOnANonTTYSaysNothingOnStderr(t *testing.T) {
	cmd := kontraCmd(t, "mcp")
	cmd.Stdin = strings.NewReader(initializeFrame)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("kontra mcp: %v (stderr=%q)", err, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("mcp stderr must be empty, got %q", stderr.String())
	}
	// And the handshake still lands: the gate silences the banner, not the server.
	if !strings.Contains(stdout.String(), `"serverInfo"`) {
		t.Errorf("handshake missing from stdout: %q", stdout.String())
	}
}

// The gate is not a special case for `mcp`. Every non-interactive caller — a redirect, a cron
// line, a CI step — gets the same silence, while the command's OWN diagnostics still go out.
func TestPipedInvocationDiagnosesWithoutTheBanner(t *testing.T) {
	cmd := kontraCmd(t, "not-a-command")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	_ = cmd.Run() // unknown command exits 2
	if strings.Contains(stderr.String(), strings.TrimSpace(banner)) {
		t.Errorf("banner printed to a piped stderr: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "unknown command") {
		t.Errorf("the usage error itself must still print, got %q", stderr.String())
	}
}

// THE STRANDING ITSELF, ASSERTED. What went wrong before was arithmetic on /tmp, not a wrong
// answer from a test — so the two properties that stop it are pinned here rather than described in
// the comment above.

// One directory per checkout: a run that is killed leaves a tree the NEXT run builds into, which is
// the whole reason six of these could pile up before.
func TestTheBinaryTreeIsOnePerCheckoutSoAKilledRunCannotAccumulate(t *testing.T) {
	base := t.TempDir()
	first := bannerBinDir(base, "/src/kontra")
	second := bannerBinDir(base, "/src/kontra")
	if first != second {
		t.Errorf("the same checkout must resolve to one tree, got %q then %q", first, second)
	}
	if other := bannerBinDir(base, "/src/kontra-worktree"); other == first {
		t.Errorf("two checkouts must not share a tree, both got %q", first)
	}
	if got := filepath.Dir(first); got != base {
		t.Errorf("the tree belongs under %q, got %q", base, got)
	}
	if name := filepath.Base(first); !strings.HasPrefix(name, bannerBinPrefix) {
		t.Errorf("the sweep finds trees by prefix %q, but the name is %q", bannerBinPrefix, name)
	}
}

// And the sweep reclaims what the old code already stranded, without touching a live run's tree.
func TestTheSweepReclaimsStrandedTreesAndSparesLiveOnes(t *testing.T) {
	base := t.TempDir()
	now := time.Now()

	mk := func(name string, age time.Duration) string {
		dir := filepath.Join(base, name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		// 193 MB in production; a byte here. What is being swept is the directory.
		if err := os.WriteFile(filepath.Join(dir, "kontra"), []byte("x"), 0o700); err != nil {
			t.Fatal(err)
		}
		when := now.Add(-age)
		if err := os.Chtimes(dir, when, when); err != nil {
			t.Fatal(err)
		}
		return dir
	}

	// The exact shape `os.MkdirTemp("", "kontra-banner")` produced, which is what is in /tmp today.
	stranded := mk("kontra-banner1234567890", 48*time.Hour)
	deterministic := mk(filepath.Base(bannerBinDir(base, "/a/gone/checkout")), 48*time.Hour)
	// A sibling run that started twenty minutes ago and is still going.
	live := mk("kontra-banner-aaaaaaaaaaaa", 20*time.Minute)
	// Ours. Skipped by name however old it is — a checkout that runs these tests once a week must
	// not have its own tree swept out from under the build that is about to reuse it.
	mine := mk("kontra-banner-bbbbbbbbbbbb", 72*time.Hour)
	// Somebody else's unrelated directory.
	untouched := mk("kontra-run-cache", 48*time.Hour)

	swept := sweepStrandedBinaries(base, mine, now, bannerBinStaleAfter)

	for _, gone := range []string{stranded, deterministic} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("%s should have been swept, stat gave %v", gone, err)
		}
	}
	for _, kept := range []string{live, mine, untouched} {
		if _, err := os.Stat(kept); err != nil {
			t.Errorf("%s must survive the sweep: %v", kept, err)
		}
	}
	if len(swept) != 2 {
		t.Errorf("the sweep must report what it removed, got %v", swept)
	}
}
