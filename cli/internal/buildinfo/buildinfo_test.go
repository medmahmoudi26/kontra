package buildinfo

// buildinfo_test.go — proving the version actually reaches the binary.
//
// THE FAILURE THIS EXISTS FOR IS SILENT. `-X` naming a symbol that does not exist is not an error:
// the linker ignores it, the build succeeds, and every release reports `dev` forever. Nothing about
// the build output says so. Asserting on the STRING that `LinkerFlag` returns would be worthless —
// it would pass just as happily for a path that is one rename out of date.
//
// So the only test worth having compiles the flag into a real binary and asks it.

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// wantEnv carries the expected version into the child `go test` below.
const wantEnv = "KONTRA_BUILDINFO_WANT"

// TestReportsInjectedVersion is the CHILD. It is a no-op in a normal run and only means something
// when the driver below re-invokes this package with `-ldflags` set.
func TestReportsInjectedVersion(t *testing.T) {
	want := os.Getenv(wantEnv)
	if want == "" {
		t.Skip("driver-only: run by TestLinkerFlagActuallyInjects with -ldflags set")
	}
	if got := Version(); got != want {
		t.Fatalf("linker injected %q but Version() reports %q — "+
			"the -X symbol path in LinkerFlag no longer matches this package", want, got)
	}
	if !IsRelease() {
		t.Fatal("version was injected but IsRelease() is false")
	}
}

// TestLinkerFlagActuallyInjects is the DRIVER: it builds this package with the real flag and runs
// the child above.
//
// Only this package is compiled, not the whole CLI — which matters, because a full `kontra` build
// takes minutes on a modest machine and a test nobody will wait for is a test that gets skipped.
func TestLinkerFlagActuallyInjects(t *testing.T) {
	const want = "9.9.9-injected"
	flag := LinkerFlag(want)
	if flag == "" {
		t.Fatal("LinkerFlag returned nothing for a real version")
	}

	cmd := exec.Command("go", "test", "-count=1", "-run", "TestReportsInjectedVersion", "-ldflags", flag, ".")
	cmd.Env = append(os.Environ(), wantEnv+"="+want)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the injected build failed its own check: %v\n%s", err, out)
	}
	// `go test` prints ok for a package whose only selected test SKIPPED, too — so the exit code
	// alone does not prove the child ran. Fail if it was skipped.
	if strings.Contains(string(out), "no tests to run") {
		t.Fatalf("the child test never ran, so nothing was proved:\n%s", out)
	}
}

// A build nobody linked must NOT claim to be a release. A working copy reporting `0.1.0` is how a
// bug arrives against a release that never contained the code.
func TestUnlinkedBuildIsNotARelease(t *testing.T) {
	if version != "" {
		t.Skip("this binary was linked with a version; the driver covers that case")
	}
	if IsRelease() {
		t.Fatal("IsRelease() is true with no injected version")
	}
	if got := Version(); !strings.HasPrefix(got, "dev") {
		t.Fatalf("an unlinked build reports %q; it must announce itself as dev", got)
	}
}

// LinkerFlag normalises what a human or a tag might hand it.
func TestLinkerFlagNormalises(t *testing.T) {
	if got := LinkerFlag("v1.2.3"); !strings.HasSuffix(got, "=1.2.3") {
		t.Errorf("a leading v must be stripped, got %q", got)
	}
	if got := LinkerFlag("  1.2.3  "); !strings.HasSuffix(got, "=1.2.3") {
		t.Errorf("surrounding space must be trimmed, got %q", got)
	}
	// Empty means "no version to inject", and must produce NO flag rather than an `-X ...=` that
	// sets the symbol to the empty string — which reads as "not a release" and would be a release.
	if got := LinkerFlag("   "); got != "" {
		t.Errorf("an empty version must produce no flag, got %q", got)
	}
}
