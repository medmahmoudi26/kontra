package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"kontra.sh/kontra/internal/migrate"
	"kontra.sh/kontra/internal/pgtest"
)

func runCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	err := run(args, &out, &errOut)
	return out.String(), err
}

func TestUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"frobnicate"},
		{"migrate", "--postgres", "postgres://x@127.0.0.1:1/x", "sideways"},
		{"migrate", "--postgres", "postgres://x@127.0.0.1:1/x", "up", "extra"},
		{"migrate", "--nope"},
		{"migrate", "up", "--nope"},
		{"serve", "--nope"},
		{"serve", "-f", "x.yaml", "extra"},
		{"enroll"},
		{"enroll", "a", "b"},
		{"enroll", "Not_A_Service"},
		{"enroll", "--service", "hello"},
		{"enroll", "--revoke", "not-a-key"},
		{"enroll", "--revoke", "k1_aaaaaaaaaaaaaaaaaaaaaaaaaa", "hello"},
		{"enroll", "--revoke", "k1_aaaaaaaaaaaaaaaaaaaaaaaaaa", "--service", "Bad_Name"},
		{"enroll", "--api-key", "x", "--service", "hello"},
	} {
		_, err := runCmd(t, args...)
		var u usageError
		if !errors.As(err, &u) {
			t.Errorf("run %q = %v, want a usage error", args, err)
		}
	}
}

// serve applies the per-replica overrides on top of the file, and validates them.
func TestServeConfigOverrides(t *testing.T) {
	cfg, err := serveConfig([]string{"-f", "../../examples/sellers/hello-exec/kontra.yaml", "--listen", "127.0.0.1:18081", "--metrics-listen", "127.0.0.1:19091"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "127.0.0.1:18081" || cfg.Metrics.Listen != "127.0.0.1:19091" {
		t.Errorf("listen %q metrics %q", cfg.Listen, cfg.Metrics.Listen)
	}
	cfg, err = serveConfig([]string{"-f", "../../examples/sellers/hello-exec/kontra.yaml"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "127.0.0.1:8080" || cfg.Metrics.Listen != ":9090" {
		t.Errorf("without overrides: listen %q metrics %q", cfg.Listen, cfg.Metrics.Listen)
	}
	if _, err := serveConfig([]string{"-f", "../../examples/sellers/hello-exec/kontra.yaml", "--metrics-listen", "127.0.0.1:8080", "--listen", "127.0.0.1:8080"}); err == nil ||
		!strings.Contains(err.Error(), "must differ from listen") {
		t.Errorf("colliding overrides = %v", err)
	}
	if _, err := serveConfig([]string{"-f", "does-not-exist.yaml"}); err == nil {
		t.Error("a missing file loads")
	}
}

func TestVersion(t *testing.T) {
	out, err := runCmd(t, "version")
	if err != nil || !strings.HasPrefix(out, "kontra-gate ") {
		t.Errorf("version = %q, %v", out, err)
	}
}

// `kontra-gate migrate` against a real database: up on an empty database, status, down, up again, with
// the URL from --postgres or from the configuration file, and flags before or after the action.
func TestMigrateCommand(t *testing.T) {
	dsn := pgtest.Database(t)
	out, err := runCmd(t, "migrate", "--postgres", dsn, "up")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`(?m)^OK up 0001_init\.sql \(`).MatchString(out) || !strings.Contains(out, "database at version 1; this gate needs 1") {
		t.Errorf("migrate up printed:\n%s", out)
	}
	m, err := migrate.Open(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.Check(context.Background()); err != nil {
		t.Fatalf("after migrate up: %v", err)
	}

	out, err = runCmd(t, "migrate", "up", "--postgres", dsn)
	if err != nil || !strings.Contains(out, "no migrations to apply") {
		t.Errorf("second migrate up = %q, %v", out, err)
	}
	out, err = runCmd(t, "migrate", "status", "--postgres", dsn)
	if err != nil || !regexp.MustCompile(`(?m)^1\s+applied\s+\d{4}-\d\d-\d\dT\S+\s+0001_init\.sql$`).MatchString(out) {
		t.Errorf("migrate status = %v:\n%s", err, out)
	}

	// The URL from the configuration file; down, then status shows it pending.
	dir := t.TempDir()
	cfg := filepath.Join(dir, "kontra.yaml")
	y := "public_url: http://localhost:8080\npostgres: \"" + dsn + "\"\n"
	if err := os.WriteFile(cfg, []byte(y), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err = runCmd(t, "migrate", "-f", cfg, "down")
	if err != nil || !strings.Contains(out, "OK down 0001_init.sql") || !strings.Contains(out, "database at version 0") {
		t.Errorf("migrate down = %v:\n%s", err, out)
	}
	if err := m.Check(context.Background()); !errors.Is(err, migrate.ErrSchemaNotCurrent) {
		t.Errorf("Check after down = %v", err)
	}
	out, err = runCmd(t, "migrate", "-f", cfg, "status")
	if err != nil || !regexp.MustCompile(`(?m)^1\s+pending\s+-\s+0001_init\.sql$`).MatchString(out) {
		t.Errorf("status after down = %v:\n%s", err, out)
	}
	out, err = runCmd(t, "migrate", "-f", cfg, "down")
	if err != nil || !strings.Contains(out, "no migration to roll back") {
		t.Errorf("down with nothing applied = %q, %v", out, err)
	}
	out, err = runCmd(t, "migrate", "-f", cfg) // up is the default action
	if err != nil || !strings.Contains(out, "OK up 0001_init.sql") {
		t.Errorf("migrate (default up) = %q, %v", out, err)
	}
	if err := m.Check(context.Background()); err != nil {
		t.Errorf("Check after down and up: %v", err)
	}
}
