package gate

import (
	"bytes"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"kontra.sh/kontra/internal/config"
	"kontra.sh/kontra/internal/hooks"
)

// The replica's Admission (spec §13.1, DM4-8): the built-in without hooks.admission; the remote hook with the secret
// from secret_env and its operations; a secret that is unset or empty refuses the start, naming the variable and
// never a value. The start log names the hook without its secret.
func TestBuildAdmission(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	rules := hooks.KeyRules{OperatorPrefixes: []string{"kontra.escrow."}}

	a, ops, err := buildAdmission(&config.Config{}, rules, quiet)
	if _, ok := a.(hooks.BuiltinAdmission); err != nil || !ok || len(ops) != 0 {
		t.Errorf("without hooks.admission: %T %v %v", a, ops, err)
	}

	hook := func(url string, ops ...string) *config.Config {
		return &config.Config{Hooks: config.Hooks{Admission: &config.AdmissionHook{URL: url, SecretEnv: "KONTRA_TEST_ADMISSION_SECRET",
			Timeout: config.Duration(3 * time.Second), Operations: ops}}}
	}
	t.Setenv("KONTRA_TEST_ADMISSION_SECRET", "")
	if _, _, err := buildAdmission(hook("http://127.0.0.1:7000/admit", "task"), rules, quiet); err == nil ||
		!strings.Contains(err.Error(), "KONTRA_TEST_ADMISSION_SECRET is unset or empty") {
		t.Errorf("an empty secret: %v", err)
	}

	const secret = "Pq7vK2mX9zL4rT8wN3bY6cJ1dF5gH0sQ"
	t.Setenv("KONTRA_TEST_ADMISSION_SECRET", secret)
	var logs bytes.Buffer
	a, ops, err = buildAdmission(hook("http://127.0.0.1:7000/admit", "task", "call"), rules, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ra, ok := a.(*hooks.RemoteAdmission)
	if !ok || len(ops) != 2 || ops[0] != hooks.OpTask || ops[1] != hooks.OpCall || !ra.Consults(hooks.OpCall) || ra.Consults(hooks.OpSession) {
		t.Errorf("the remote hook: %T, operations %v", a, ops)
	}
	if strings.Contains(logs.String(), secret) || !strings.Contains(logs.String(), "http://127.0.0.1:7000/admit") {
		t.Errorf("the start log: %s", logs.String())
	}
	// A URL the configuration would refuse is refused here too (a hand-built configuration).
	if _, _, err := buildAdmission(hook("http://cp.example.com/admit"), rules, quiet); err == nil || strings.Contains(err.Error(), secret) {
		t.Errorf("http:// off loopback: %v", err)
	}
}
