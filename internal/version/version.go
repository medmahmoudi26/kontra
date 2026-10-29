// Package version carries the build version of both binaries.
package version

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// Version is set at link time: -ldflags "-X kontra.sh/kontra/internal/version.Version=<v>".
// It is written to gate_replicas.version and the tunnel hello (spec/tunnel.md).
var Version = "dev"

// Get is the version to report: Version when set at link time, else "dev" with the VCS revision the Go
// toolchain stamped into the binary ("dev+1a2b3c4d5e6f", "-dirty" when the tree had changes), else the module's
// version when the binary was built by `go install kontra.sh/kontra/cmd/kontra@<version>` (no VCS stamp then; a
// pseudo-version such as v0.0.0-20261029101500-1a2b3c4d5e6f names the commit, which the README demo's post-merge run
// checks, D298), else "dev".
func Get() string {
	if Version != "dev" {
		return Version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return Version
	}
	return fromBuildInfo(Version, info)
}

// fromBuildInfo is Get's reading of the build information, for a binary whose Version was not set at link time.
func fromBuildInfo(v string, info *debug.BuildInfo) string {
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
	if rev == "" && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return withRevision(v, rev, dirty)
}

func withRevision(v, rev string, dirty bool) string {
	if rev == "" {
		return v
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	v += "+" + rev
	if dirty {
		v += "-dirty"
	}
	return v
}

// String is the line `kontra version` and `kontra-gate version` print.
func String(binary string) string {
	return fmt.Sprintf("%s %s (%s, %s/%s)", binary, Get(), runtime.Version(), runtime.GOOS, runtime.GOARCH)
}
