// Package version carries the build version of both binaries.
package version

import (
	"fmt"
	"runtime"
)

// Version is set at link time: -ldflags "-X github.com/medmahmoudi26/kontra/internal/version.Version=<v>".
// It is written to gate_replicas.version and the tunnel hello (spec/tunnel.md).
var Version = "dev"

// String is the line `kontra version` and `kontra-gate version` print.
func String(binary string) string {
	return fmt.Sprintf("%s %s (%s, %s/%s)", binary, Version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}
