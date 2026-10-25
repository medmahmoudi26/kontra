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
// toolchain stamped into the binary ("dev+1a2b3c4d5e6f", "-dirty" when the tree had changes), else "dev".
func Get() string {
	if Version != "dev" {
		return Version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return Version
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
	return withRevision(Version, rev, dirty)
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
