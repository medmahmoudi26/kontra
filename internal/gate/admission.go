package gate

import (
	"fmt"
	"log/slog"
	"os"

	"kontra.sh/kontra/internal/config"
	"kontra.sh/kontra/internal/hooks"
)

// buildAdmission is the replica's Admission (spec/relay.md §13.1): the built-in allow without hooks.admission, else
// the remote hook (DM4-8), dialed as §10.3's operator-configured endpoint (DM4-17), with its bearer secret read from
// the variable secret_env names. A secret that is unset or empty refuses the start: falling back to the built-in would
// admit what the operator's hook denies. The operations are those that consult it (hooks.admission.operations): the
// A2A interceptors ask it about every new task and RemoteAdmission answers the operations it is not asked about as
// the built-in does; the MCP relay asks it only about the operations listed (session, call, task); the gate's own /mcp
// (M4 WP-04) asks it about each hire.
func buildAdmission(cfg *config.Config, rules hooks.KeyRules, log *slog.Logger) (hooks.Admission, []hooks.Operation, error) {
	h := cfg.Hooks.Admission
	if h == nil {
		return hooks.BuiltinAdmission{}, nil, nil
	}
	secret := os.Getenv(h.SecretEnv)
	if secret == "" {
		return nil, nil, fmt.Errorf("gate: hooks.admission.secret_env: %s is unset or empty; it holds the bearer secret the "+
			"Admission hook is called with (spec §13.1)", h.SecretEnv)
	}
	ops := make([]hooks.Operation, 0, len(h.Operations))
	for _, op := range h.Operations {
		ops = append(ops, hooks.Operation(op))
	}
	ra, err := hooks.NewRemoteAdmission(hooks.RemoteAdmissionConfig{
		URL: h.URL, Secret: secret, Timeout: h.Timeout.D(), Operations: ops, Rules: rules,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("gate: %w", err)
	}
	log.Info("the remote Admission hook is configured (spec §13.1)", "url", h.URL, "operations", ra.Operations(),
		"timeout", h.Timeout.D().String())
	return ra, ra.Operations(), nil
}
