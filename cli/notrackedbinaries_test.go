package main

import (
	"os/exec"
	"strings"
	"testing"
)

// NO TRACKED FILE IS A COMPILED BINARY.
//
// ── THE COMMIT THIS EXISTS BECAUSE OF ────────────────────────────────────────────────────────────
//
// `git rm -r workspaces/scraping/actors/gocanary` removed the three TRACKED files in that folder —
// `main.go`, `go.sum`, `main_test.go` — and left the UNTRACKED `gocanary` beside them, because
// `go build` in a directory writes its output next to the source and nothing ignored it. The next
// `git add -A` staged it, so one commit deleted 719 lines of Go and added this:
//
//	workspaces/scraping/actors/gocanary/gocanary | Bin 0 -> 53617532 bytes
//
// 51 MB, pushed, and the only thing that said so was a GitHub warning on the push output — which
// is not where anybody looks, and which the author of the commit had already scrolled past.
//
// ── WHY A TEST AND NOT A .gitignore LINE ─────────────────────────────────────────────────────────
//
// A compiled Go binary has NO EXTENSION and takes the name of its directory, so the pattern that
// would have caught this one is `workspaces/scraping/actors/gocanary/gocanary` — the instance, not
// the class. Every future actor written in Go has a different one, and the rule would have to be
// remembered each time, by the person least likely to be thinking about it (they are deleting
// something, not adding it).
//
// The class is checkable: run the tracked-file list past a magic-number check. ELF, Mach-O (all
// four flavours, including the fat ones) and PE cover every platform this repo is built on.
//
// ── IT ASKS GIT, NOT THE FILESYSTEM ──────────────────────────────────────────────────────────────
//
// `git ls-files` is the whole point: an untracked binary on a developer's disk is ordinary and
// none of this test's business, and a `filepath.Walk` would fail on exactly that case while
// missing a tracked binary somebody had since deleted locally. Skipped rather than failed outside
// a checkout, because a test that cannot see the repo must not claim it is clean.
func TestNoTrackedFileIsACompiledBinary(t *testing.T) {
	out, err := exec.Command("git", "-C", "..", "ls-files", "-z").Output()
	if err != nil {
		t.Skipf("not a git checkout, or git is unavailable: %v", err)
	}
	paths := strings.Split(strings.TrimRight(string(out), "\x00"), "\x00")
	if len(paths) < 100 {
		t.Fatalf("git ls-files returned %d paths; this guard is not looking at the repo", len(paths))
	}

	var found []string
	for _, p := range paths {
		if p == "" {
			continue
		}
		// The blob AS GIT HAS IT, so a file deleted from the working tree is still checked and a
		// `.gitattributes` filter cannot hide one.
		head, err := exec.Command("git", "-C", "..", "show", "HEAD:"+p).Output()
		if err != nil {
			continue // a path git lists but cannot show (a submodule, a broken link) is not this test's business
		}
		if kind := executableKind(head); kind != "" {
			found = append(found, kind+"  "+p)
		}
	}
	if len(found) > 0 {
		t.Errorf("tracked compiled binaries:\n  %s\n"+
			"A build artifact in git is bytes every clone downloads for ever, and removing one "+
			"after it is pushed needs a history rewrite. Delete it and add its path to .gitignore.",
			strings.Join(found, "\n  "))
	}
}

// executableKind names the executable format `data` starts with, or "" for anything else.
//
// MAGIC NUMBERS, NOT EXTENSIONS, because the file that caused this had none. The Mach-O list is
// four entries rather than one: 32- and 64-bit, each in both byte orders, plus the two fat/universal
// wrappers — a darwin/arm64 build is `cffaedfe` and would sail past a check that knew only `feedface`.
func executableKind(data []byte) string {
	for _, m := range []struct {
		magic []byte
		name  string
	}{
		{[]byte{0x7f, 'E', 'L', 'F'}, "ELF"},
		{[]byte{0xfe, 0xed, 0xfa, 0xce}, "Mach-O"},
		{[]byte{0xce, 0xfa, 0xed, 0xfe}, "Mach-O"},
		{[]byte{0xfe, 0xed, 0xfa, 0xcf}, "Mach-O"},
		{[]byte{0xcf, 0xfa, 0xed, 0xfe}, "Mach-O"},
		{[]byte{0xca, 0xfe, 0xba, 0xbe}, "Mach-O"},
		{[]byte{0xbe, 0xba, 0xfe, 0xca}, "Mach-O"},
		{[]byte{'M', 'Z'}, "PE"},
	} {
		if len(data) >= len(m.magic) && string(data[:len(m.magic)]) == string(m.magic) {
			return m.name
		}
	}
	return ""
}
