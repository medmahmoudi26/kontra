// Package buildinfo answers "which kontra is this" — for `kontra version`, for a bug report, and
// for anything that has to decide whether the binary in front of it is a release or a working copy.
//
// THERE IS NO VERSION CONSTANT IN THIS REPOSITORY, and that is deliberate. `cli/release.go` already
// made the argument and it still holds: "a version constant is a thing somebody forgets to bump and
// then two different releases wear one name." So `version` below is EMPTY in the source and filled
// at LINK time by `kontra release`, from the same value that names the tarball — one number,
// computed once, reaching both the filename and the binary.
//
// WHAT CHANGED IS ONLY THAT THE NUMBER HAS A HOME. `version.txt` at the repository root is
// release-please's file: it is bumped by automation on merge, never by hand, which is the objection
// above answered rather than ignored. Nothing reads it at runtime.
//
// A BUILD THAT WAS NOT LINKED BY `kontra release` SAYS SO. `go build ./cli` produces a binary whose
// version is the VCS stamp Go embeds on its own — revision and dirty flag — prefixed `dev`. A
// working copy claiming to be `0.1.0` is how a bug gets reported against a release that never
// contained the code.
package buildinfo

import (
	"runtime/debug"
	"strings"
)

// version is injected at link time:
//
//	-ldflags "-X github.com/medmahmoudi26/kontra/cli/internal/buildinfo.version=0.1.0"
//
// Lowercase and unexported so nothing can set it at runtime: the value's whole worth is that it was
// fixed when the binary was made.
var version = ""

// Version is what this binary calls itself. A released build returns its release version; anything
// else returns `dev` with whatever the toolchain recorded about the tree it came from.
func Version() string {
	if version != "" {
		return version
	}
	return devVersion()
}

// IsRelease reports whether this binary was linked by `kontra release`.
//
// Useful to a caller that must not treat a working copy as a shipped artifact — and it is a
// question `Version()` alone cannot answer, since a `dev` string is a value like any other.
func IsRelease() bool { return version != "" }

// devVersion reads Go's own VCS stamp. `debug.ReadBuildInfo` carries `vcs.revision` and
// `vcs.modified` for any build made inside a git work tree, so an unreleased binary can still say
// which commit it is without this package shelling out to git or embedding anything.
func devVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	var rev string
	var dirty bool
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return "dev"
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	out := "dev (" + rev
	if dirty {
		out += ", dirty"
	}
	return out + ")"
}

// LinkerFlag is the `-X` argument that injects a version, built in ONE place so the release command
// and the test that proves injection works cannot spell it differently.
//
// A WRONG PATH HERE FAILS SILENTLY. `-X` on an unknown symbol is not an error: the linker ignores
// it, the build succeeds, and the binary reports `dev` for every release forever. That is why
// `TestLinkerFlagActuallyInjects` builds a real binary and runs it rather than asserting on this
// string.
func LinkerFlag(v string) string {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if v == "" {
		return ""
	}
	return "-X github.com/medmahmoudi26/kontra/cli/internal/buildinfo.version=" + v
}
