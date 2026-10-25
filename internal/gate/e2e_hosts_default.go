//go:build !kontra_e2e

package gate

import (
	"log/slog"

	"kontra.sh/kontra/internal/ssrf"
)

// pushResolver is the push policy's resolver: the system's (nil). A gate built with the kontra_e2e tag
// (test/e2e) can resolve fixed names instead (e2e_hosts.go); a release never reads that environment.
func pushResolver(*slog.Logger) ssrf.Resolver { return nil }
