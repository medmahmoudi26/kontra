//go:build kontra_e2e

package gate

import (
	"log/slog"
	"os"

	"kontra.sh/kontra/internal/ssrf"
)

// pushResolver is the push policy's resolver in an e2e build: E2EHostsEnv's names, then the system's.
func pushResolver(log *slog.Logger) ssrf.Resolver {
	spec := os.Getenv(E2EHostsEnv)
	if spec == "" {
		return nil
	}
	h, err := parseE2EHosts(spec)
	if err != nil {
		log.Error("e2e hosts ignored", "env", E2EHostsEnv, "error", err)
		return nil
	}
	log.Info("e2e hosts: push URLs resolve these names to fixed addresses", "env", E2EHostsEnv, "hosts", spec)
	return h
}
