package hostengine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// THE ASSET LIST IS READ OUT OF THE PROGRAM, and this asserts the extraction against the program as
// committed rather than against a list of names.
//
// THAT DISTINCTION IS NOT THEORETICAL — it was earned mid-session. This test was first written
// asserting exactly three assets, because `Pulumi.yaml` had three `fn::readFile: ${assetsDir}/…` lines
// (`:529`, `:894`, `:898`). Another lane then landed `control/images/Dockerfile.logship`, which bakes
// `logship.sh` and `logline.py` into an image, and the program dropped to ONE — precisely what
// `control/pulumi/README.md:291` had predicted ("Issue 16 baking the two logship scripts into an image
// removes two of the three"). `Materialise` needed no change, because it never knew the names; only
// this assertion did. So what is asserted is the PROPERTY: the extraction finds what the program
// actually reads, every name it finds is a file that exists beside the program, and `postgres-init.sh`
// — which creates the `kontra_ducklake` database and cannot be baked into the upstream postgres image
// — is among them.
func TestAssetsAreReadOutOfTheRealProgram(t *testing.T) {
	root := repoRoot(t)
	b, err := os.ReadFile(filepath.Join(root, "control", "pulumi", ProgramFile))
	if err != nil {
		t.Fatal(err)
	}
	got := Assets(b)
	if len(got) == 0 {
		t.Fatal("Assets found nothing in the committed program — if this extraction finds nothing, " +
			"Materialise copies nothing and the failure lands at converge time, as `Error reading file at " +
			"path …` naming a path under ~/.kontra the operator never wrote")
	}
	// NON-VACUITY: count the loads a second, independent way and require the same answer, so a regex
	// that matched one of several would not read as "the program only has one".
	//
	// COMMENTS ARE EXCLUDED, and getting that wrong is how this check first failed: `Pulumi.yaml`
	// mentions `fn::readFile` three times in its own header prose (`:70`, `:77`, `:83`) and uses it
	// once, so a bare substring count says four. `cli/retired_words_test.go:25` already draws the same
	// line for the same reason — it scans Go string literals and exempts prose — and a check that
	// counts a design note as a file to copy would fail on every edit to a comment.
	if want := countLoads(b); want != len(got) {
		t.Errorf("Assets = %v (%d), but the program loads %d files — the extraction is missing some, "+
			"and a missed asset is a converge that fails before any resource", got, len(got), want)
	}
	found := false
	for _, name := range got {
		if name == "postgres-init.sh" {
			found = true
		}
		// Every one of them must exist beside the program, because that is what Materialise copies from.
		if _, err := os.Stat(filepath.Join(root, "control", "images", name)); err != nil {
			t.Errorf("the program reads %s and it is not in control/images: %v", name, err)
		}
	}
	if !found {
		t.Errorf("Assets = %v and postgres-init.sh is not in it. Without it postgres comes up HEALTHY "+
			"with no kontra_ducklake database, which is the failure mode control/pulumi/Pulumi.yaml:66-75 "+
			"describes", got)
	}
}

// countLoads counts `fn::readFile:` used as a YAML key, skipping comment lines.
func countLoads(program []byte) int {
	n := 0
	for _, line := range strings.Split(string(program), "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "#") {
			continue
		}
		if strings.Contains(t, "fn::readFile:") {
			n++
		}
	}
	return n
}

// A PROGRAM THAT DECLARES ANOTHER PROJECT IS REFUSED RATHER THAN RUN. `README.md:52-56` calls the
// fixed location plus `name:` as the first field a security property — "cannot be pointed at another
// project by a caller" — so `--program` is checked against the file's own name, not trusted.
func TestResolveRefusesAProgramThatIsAnotherProject(t *testing.T) {
	tmp := t.TempDir()
	other := filepath.Join(tmp, "elsewhere")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, ProgramFile), []byte("name: kontra-fleet\nruntime: yaml\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Resolve(NewLayout(tmp), other, "", fakeEnv(nil))
	if err == nil {
		t.Fatal("a program named kontra-fleet was accepted by the host engine")
	}
	for _, want := range []string{"kontra-fleet", Project, "ADR 0052 §1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must name both projects and the rule; %q missing from:\n%v", want, err)
		}
	}
}

// A MISSING PROGRAM IS NOBODY'S MISTAKE — it is what a released binary looks like before the
// installer ships the program beside it — so the message is a list of where to put one.
func TestResolveNamesEveryPlaceItLooked(t *testing.T) {
	tmp := t.TempDir()
	_, err := Resolve(NewLayout(tmp), "", filepath.Join(tmp, "checkout"), fakeEnv(nil))
	if err == nil {
		t.Fatal("no program anywhere must be an error")
	}
	for _, want := range []string{"the checkout", "already installed", "--program"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must list where it looked; %q missing from:\n%v", want, err)
		}
	}
}

// An explicitly-named program that is not there is a TYPO, not a fallthrough. Falling through would
// converge the installed copy and never mention that `--program` was ignored.
func TestResolveDoesNotFallThroughPastAnExplicitProgram(t *testing.T) {
	tmp := t.TempDir()
	lay := NewLayout(tmp)
	if err := os.MkdirAll(lay.Program, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lay.Program, ProgramFile), []byte("name: "+Project+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Resolve(lay, filepath.Join(tmp, "nope"), "", fakeEnv(nil))
	if err == nil {
		t.Fatal("--program pointing at nothing silently used the installed copy instead")
	}
	if !strings.Contains(err.Error(), "--program") {
		t.Errorf("the error must name the flag that was wrong: %v", err)
	}
}

// Materialise puts the program somewhere this engine may WRITE, because `pulumi stack select --create`
// drops `Pulumi.<stack>.yaml` beside `Pulumi.yaml` (measured on 3.244.0) and that must not land in a
// checkout. This asserts the whole layout the converge then depends on.
func TestMaterialiseCopiesTheProgramAndItsAssets(t *testing.T) {
	root := repoRoot(t)
	tmp := t.TempDir()
	lay := NewLayout(tmp)
	src, err := Resolve(lay, filepath.Join(root, "control", "pulumi"), "", fakeEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	wrote, err := Materialise(lay, src)
	if err != nil {
		t.Fatal(err)
	}
	// The program plus every asset it reads — a count derived from the program, not typed here. See
	// TestAssetsAreReadOutOfTheRealProgram for what a typed count cost once already.
	if want := 1 + len(Assets(src.YAML)); len(wrote) != want {
		t.Errorf("wrote %v (%d), want the program and its %d assets", wrote, len(wrote), want-1)
	}
	if _, err := os.Stat(filepath.Join(lay.Program, ProgramFile)); err != nil {
		t.Fatalf("no program in the engine's own directory: %v", err)
	}
	for _, name := range Assets(src.YAML) {
		info, err := os.Stat(filepath.Join(lay.Assets, name))
		if err != nil {
			t.Fatalf("asset %s was not materialised: %v", name, err)
		}
		// 0755 and not 0744: postgres's entrypoint tests `-x` AS THE `postgres` USER and SOURCES a
		// script it cannot execute, which leaks the script's `set -eu` into an entrypoint that
		// deliberately runs without `-u` (control/pulumi/README.md, "the upload's mode").
		if info.Mode().Perm() != 0o755 {
			t.Errorf("%s is mode %04o, want 0755", name, info.Mode().Perm())
		}
	}
	// THE COPY IS DERIVED: a second call writes nothing, and a changed source is rewritten.
	again, err := Materialise(lay, src)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Errorf("a second Materialise rewrote %v — an unchanged program must be left alone so the "+
			"stack file beside it is never disturbed", again)
	}
	src.YAML = append(src.YAML, []byte("\n# edited\n")...)
	if again, err = Materialise(lay, src); err != nil || len(again) != 1 {
		t.Errorf("an edited program must be re-copied so the committed file always wins: %v %v", again, err)
	}
}

// --- the mutex ----------------------------------------------------------------------------------

// ADR 0052 §1 gives the host a lock FILE because it has no Temporal at the moment it needs one — it
// is the thing being installed. `control/pulumi/README.md:277-280` states the failure: "Two
// concurrent `kontra up`s against one `file://` backend is a corrupted checkpoint."
func TestLockRefusesASecondConvergeAndNamesTheHolder(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "engine")
	release, err := Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Lock(dir)
	if err == nil {
		release()
		t.Fatal("two converges took the same lock")
	}
	for _, want := range []string{"already running", "corrupt", filepath.Join(dir, LockFileName)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must name what is happening, why it matters and the file; %q missing from:\n%v", want, err)
		}
	}
	release()
	release2, err := Lock(dir)
	if err != nil {
		t.Fatalf("the lock was not released: %v", err)
	}
	release2()
}

// A STALE LOCK IS CLEARED, NOT WORSHIPPED. `kontra up` blocks for minutes and gets Ctrl-C'd; a
// SIGKILL leaves the file behind, and requiring a manual `rm` of a file the operator has never heard
// of is a worse failure than the one the lock prevents.
func TestLockClearsALockWhoseHolderIsGone(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "engine")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// pid 0 is never a live process id, and a lock file with nothing readable in it takes the same
	// path — which is the state a kill -9 mid-write leaves.
	if err := os.WriteFile(filepath.Join(dir, LockFileName), []byte("0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	release, err := Lock(dir)
	if err != nil {
		t.Fatalf("a lock held by a pid that is gone must be taken over: %v", err)
	}
	release()

	if err := os.WriteFile(filepath.Join(dir, LockFileName), []byte("not a pid"), 0o600); err != nil {
		t.Fatal(err)
	}
	release, err = Lock(dir)
	if err != nil {
		t.Fatalf("a lock file with nothing readable in it must be taken over: %v", err)
	}
	release()
}

// A PREVIEW TAKES NO LOCK, and that is a decision rather than an omission (issue 04's acceptance
// criterion: "Preview takes no lock and needs no retry dance"). It is asserted from the caller's side
// in cli/control_test.go; here it is asserted that the lock is not something Preview could be reading
// off disk by accident — an existing lock does not change what a preview would do.
func TestAPreviewIsNotBlockedByAConvergeLock(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "engine")
	release, err := Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	// Passphrase is the one thing a preview does write, and it must still work under a held lock:
	// otherwise "preview takes no lock" would be true of the lock and false in practice.
	if _, err := Passphrase(dir); err != nil {
		t.Fatalf("a preview cannot mint its passphrase while a converge holds the lock: %v", err)
	}
}
