package main

// gotoolchain_test.go — the `go` directive has eight copies, and seven of them are not Go files.
//
// RAISING `go.work` AND THE go.mod FILES IS NOT RAISING THE TOOLCHAIN. The container paths pin it
// separately: three `ENV GOTOOLCHAIN=`, one `FROM golang:`, one `GO_VERSION=` in `install.sh`. A bump
// that lands in the module files alone compiles everywhere a developer looks and fails only inside
// Docker, with `go.mod requires go >= X (running go Y; GOTOOLCHAIN=goY)` — a message that names the
// module file rather than the pin that is actually wrong.
//
// That is not hypothetical; it is why this file exists. See ADR 0061.

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// `go.work` IS THE SOURCE and every other copy is a derivative. One of them has to be, and this is
// the one the compiler reads first.
var goDirective = regexp.MustCompile(`(?m)^go (\d+\.\d+\.\d+)$`)

// The three patterns that are always FUNCTIONAL — a pin something executes, never prose. Comments
// about a past version ("eight were in the standard library at 1.26.4") must not be swept up, which
// rules out matching a bare version number.
var functionalPins = []struct {
	what string
	re   *regexp.Regexp
}{
	{"GOTOOLCHAIN", regexp.MustCompile(`GOTOOLCHAIN=go(\d+\.\d+\.\d+)`)},
	{"golang image tag", regexp.MustCompile(`golang:(\d+\.\d+\.\d+)`)},
	{"GO_VERSION default", regexp.MustCompile(`GO_VERSION:-(\d+\.\d+\.\d+)`)},
	{"GO_VERSION literal", regexp.MustCompile(`GO_VERSION="(\d+\.\d+\.\d+)"`)},
}

func wantGoVersion(t *testing.T) string {
	t.Helper()
	m := goDirective.FindStringSubmatch(repoFile(t, "go.work"))
	if m == nil {
		t.Fatal("go.work has no `go X.Y.Z` directive, so there is nothing to compare against")
	}
	return m[1]
}

// Every module in the workspace carries the same directive.
//
// THE LIST COMES FROM `go.work`, not from here. A fifth module added to the workspace is covered the
// day it is added; a hand-written list would be one module short and silent about it.
func TestEveryWorkspaceModuleAgreesWithGoWork(t *testing.T) {
	want := wantGoVersion(t)
	work := repoFile(t, "go.work")

	use := regexp.MustCompile(`(?s)use\s*\((.*?)\)`).FindStringSubmatch(work)
	if use == nil {
		t.Fatal("go.work has no `use (...)` block; this test cannot enumerate the modules")
	}
	var modules []string
	for _, line := range strings.Split(use[1], "\n") {
		if p := strings.TrimSpace(line); strings.HasPrefix(p, "./") {
			modules = append(modules, strings.TrimPrefix(p, "./"))
		}
	}
	if len(modules) == 0 {
		t.Fatal("go.work's `use (...)` block listed no modules; nothing below is checking anything")
	}

	for _, mod := range modules {
		got := goDirective.FindStringSubmatch(repoFile(t, filepath.Join(mod, "go.mod")))
		if got == nil {
			t.Errorf("%s/go.mod has no `go X.Y.Z` directive", mod)
			continue
		}
		if got[1] != want {
			t.Errorf("%s/go.mod says go %s, go.work says go %s — a workspace build uses the "+
				"highest and a `GOWORK=off` build uses this one, so they disagree only inside Docker",
				mod, got[1], want)
		}
	}
}

// Every pin something EXECUTES agrees too, wherever it lives.
//
// Swept rather than listed, for the same reason as above: the failure this catches is a pin in a file
// nobody remembered, and a list of files cannot name one that does not exist yet.
func TestEveryFunctionalToolchainPinAgreesWithGoWork(t *testing.T) {
	want := wantGoVersion(t)
	root := ".."

	var found int
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "dist", "vendor", ".venv", ".scratch":
				return fs.SkipDir
			}
			return nil
		}
		// Only text a build reads. A lockfile or a binary fixture carrying a version string is not a
		// pin anybody edits.
		switch {
		case strings.HasPrefix(d.Name(), "Dockerfile"),
			strings.HasSuffix(d.Name(), ".go"),
			strings.HasSuffix(d.Name(), ".sh"),
			strings.HasSuffix(d.Name(), ".yml"),
			strings.HasSuffix(d.Name(), ".yaml"):
		default:
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		for _, pin := range functionalPins {
			for _, m := range pin.re.FindAllStringSubmatch(string(raw), -1) {
				found++
				if m[1] != want {
					t.Errorf("%s pins %s %s, go.work says %s — raise them together or the "+
						"container build is the only thing that notices", rel, pin.what, m[1], want)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}

	// THE GUARD AGAINST A GREEN SWEEP THAT SWEPT NOTHING. If a rename or a moved directory means the
	// patterns match zero files, every assertion above passes by vacuity — which is precisely the
	// shape of bug this file exists to catch, one level up.
	const atLeast = 5
	if found < atLeast {
		t.Fatalf("found only %d functional toolchain pins, expected at least %d — the sweep is "+
			"matching nothing and would pass whatever the versions were", found, atLeast)
	}
	t.Logf("%d functional toolchain pins checked against go.work's %s", found, want)
}

