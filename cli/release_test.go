package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	applbundle "github.com/medmahmoudi26/kontra/cli/appliance/bundle"
)

// A NEW WORD WIRED TO NOTHING is a switch-statement bug, and `dispatch`'s own header says it was
// split out of `main` so that exact mistake could be tested. `release` is a new word.
func TestReleaseIsARoutedCommand(t *testing.T) {
	// A flag nothing defines: enough to prove the command was REACHED, without building anything.
	err := dispatch([]string{"release", "--no-such-flag"})
	if err == nil {
		t.Fatal("`kontra release --no-such-flag` succeeded")
	}
	if errors.Is(err, errUsage) {
		t.Fatal("`kontra release` fell through to the unknown-command branch, so it is not wired up")
	}
}

// Help is the one surface nothing else checks. A command that reaches the switch and not this
// block is a command nobody finds.
func TestReleaseIsDocumented(t *testing.T) {
	if !strings.Contains(usageText, "kontra release") {
		t.Fatal("usageText does not advertise `kontra release`")
	}
	// And it says what a release IS. "One file per platform" is the acceptance criterion, and a
	// user reading `--help` should not have to guess whether the file holds a binary, a bundle or
	// both.
	for _, want := range []string{"ONE FILE PER PLATFORM", "SHA256SUMS"} {
		if !strings.Contains(usageText, want) {
			t.Errorf("usageText does not say %q, so what the artifact contains is a guess", want)
		}
	}
	if !strings.Contains(usageText, "--platform goos/goarch|list|all") {
		t.Error("usageText does not advertise that --platform takes `all`, which is how the four are built")
	}
}

// `all` MUST COME FROM THE PIN TABLE. A second list of platforms — here, or in a CI matrix, or in
// a release script — is a second place to forget one, and the failure is silent: a release that
// ships three files and says nothing about the fourth.
func TestPlatformAllIsTheShippedFour(t *testing.T) {
	got, err := parsePlatformList("all")
	if err != nil {
		t.Fatal(err)
	}
	want := applbundle.Platforms()
	if len(got) != len(want) {
		t.Fatalf("`all` resolved %d platforms, and the appliance ships %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("`all`[%d] = %s, want %s", i, got[i], want[i])
		}
	}
}

func TestPlatformListReadsWhatItAccepts(t *testing.T) {
	host, err := parsePlatformList("")
	if err != nil {
		t.Fatal(err)
	}
	if len(host) != 1 || host[0] != applbundle.HostPlatform() {
		t.Errorf("no --platform resolved %v, want just this host", host)
	}

	list, err := parsePlatformList("darwin/arm64, linux/amd64 ,darwin/arm64")
	if err != nil {
		t.Fatal(err)
	}
	// Deduplicated, because building the same 450 MB tree twice in one command is a mistake to
	// absorb rather than to report.
	if len(list) != 2 || list[0].String() != "darwin/arm64" || list[1].String() != "linux/amd64" {
		t.Errorf("a comma list resolved %v", list)
	}

	if _, err := parsePlatformList("darwin-arm64"); err == nil {
		t.Error("a platform that is not goos/goarch was accepted")
	}
	if _, err := parsePlatformList(",,"); err == nil {
		t.Error("--platform with nothing in it was accepted")
	}
}

// A release named after nothing is a release nobody can report a bug against, so the fallback
// FAILS rather than inventing `unknown` — and it says which flag would fix it.
func TestReleaseVersionRefusesToInventAName(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git on PATH")
	}
	notARepo := t.TempDir()
	_, err := releaseVersion(notARepo, "")
	if err == nil {
		t.Fatal("a directory with no git history produced a version")
	}
	if !strings.Contains(err.Error(), "--version") {
		t.Errorf("the error does not name the flag that fixes it: %v", err)
	}
}

// The `v` is a tag convention, not part of the version, and the release FILENAMES must not carry
// it twice — `kontra_v0.2.0_linux_amd64.tar.gz` and `kontra_0.2.0_linux_amd64.tar.gz` are two
// names for one artifact and the installer only builds one of them.
func TestReleaseVersionStripsTheTagPrefix(t *testing.T) {
	got, err := releaseVersion(t.TempDir(), "v1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	if got != "1.2.3" {
		t.Errorf("--version v1.2.3 became %q", got)
	}
}

// SHA256SUMS IS WHAT MAKES THE INSTALL VERIFIABLE, so its format is `sha256sum -c`'s and its order
// is stable. Sorted rather than in build order: four releases of the same files should produce the
// same file, and build order depends on which platform failed and got retried.
func TestSHA256SUMSIsCheckableAndSorted(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"kontra_1.0.0_linux_amd64.tar.gz", "kontra_1.0.0_darwin_arm64.tar.gz"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	path, err := writeSHA256SUMS(dir, []string{
		filepath.Join(dir, "kontra_1.0.0_linux_amd64.tar.gz"),
		filepath.Join(dir, "kontra_1.0.0_darwin_arm64.tar.gz"),
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(body), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("SHA256SUMS has %d lines, want 2:\n%s", len(lines), body)
	}
	if !strings.HasSuffix(lines[0], "  kontra_1.0.0_darwin_arm64.tar.gz") {
		t.Errorf("SHA256SUMS is not sorted by filename:\n%s", body)
	}
	// The two-space separator is not cosmetic: `sha256sum -c` reads exactly that.
	for _, l := range lines {
		digest, name, ok := strings.Cut(l, "  ")
		if !ok || len(digest) != 64 || name == "" {
			t.Errorf("%q is not a line `sha256sum -c` can read", l)
		}
	}

	if _, err := exec.LookPath("sha256sum"); err != nil {
		t.Skip("no sha256sum on PATH to check the format against")
	}
	cmd := exec.Command("sha256sum", "-c", "SHA256SUMS")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("sha256sum -c refused the file this writes: %v\n%s", err, out)
	}
}
