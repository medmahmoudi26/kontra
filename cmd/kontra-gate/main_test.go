package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
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
	// --peer-listen (spec §11.2): a file without peer: still needs its secret_env.
	if _, err := serveConfig([]string{"-f", "../../examples/sellers/hello-exec/kontra.yaml", "--peer-listen", "127.0.0.1:18444"}); err == nil ||
		!strings.Contains(err.Error(), "peer.secret_env") {
		t.Errorf("--peer-listen without peer: = %v", err)
	}
	dir := t.TempDir()
	doc, err := os.ReadFile("../../examples/sellers/hello-exec/kontra.yaml")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "kontra.yaml")
	if err := os.WriteFile(path, append(doc, []byte("\npeer: { listen: \"127.0.0.1:8444\", secret_env: KONTRA_PEER_SECRET }\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = serveConfig([]string{"-f", path, "--peer-listen", "127.0.0.1:18444"})
	if err != nil || cfg.Peer.Listen != "127.0.0.1:18444" || cfg.Peer.SecretEnv != "KONTRA_PEER_SECRET" {
		t.Errorf("--peer-listen: %+v %v", cfg, err)
	}
}

// --log-level sets the replica's lowest logged level (info by default); anything but the four names is a usage error.
func TestServeLogLevel(t *testing.T) {
	file := "../../examples/sellers/hello-exec/kontra.yaml"
	for flag, want := range map[string]slog.Level{"": slog.LevelInfo, "debug": slog.LevelDebug, "info": slog.LevelInfo,
		"warn": slog.LevelWarn, "error": slog.LevelError} {
		args := []string{"-f", file}
		if flag != "" {
			args = append(args, "--log-level", flag)
		}
		a, err := parseServe(args)
		if err != nil || a.level != want || a.file != file {
			t.Errorf("--log-level %q: level %v file %q err %v, want %v", flag, a.level, a.file, err, want)
		}
	}
	for _, bad := range []string{"DEBUG", "trace", "debug+1", ""} {
		_, err := parseServe([]string{"-f", file, "--log-level", bad})
		var u usageError
		if !errors.As(err, &u) {
			t.Errorf("--log-level %q = %v, want a usage error", bad, err)
		}
	}
	// The level never reaches the configuration: serveConfig (and so a SIGHUP reload) loads the same file.
	if cfg, err := serveConfig([]string{"-f", file, "--log-level", "debug"}); err != nil || cfg.Listen != "127.0.0.1:8080" {
		t.Errorf("serveConfig with --log-level: %v", err)
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
	if !regexp.MustCompile(`(?m)^OK up 0001_init\.sql \(`).MatchString(out) || !regexp.MustCompile(`(?m)^OK up 0002_m3\.sql \(`).MatchString(out) ||
		!regexp.MustCompile(`(?m)^OK up 0003_m4\.sql \(`).MatchString(out) || !regexp.MustCompile(`(?m)^OK up 0004_m5\.sql \(`).MatchString(out) ||
		!strings.Contains(out, "database at version 4; this gate needs 4") {
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
	if err != nil || !regexp.MustCompile(`(?m)^1\s+applied\s+\d{4}-\d\d-\d\dT\S+\s+0001_init\.sql$`).MatchString(out) ||
		!regexp.MustCompile(`(?m)^2\s+applied\s+\d{4}-\d\d-\d\dT\S+\s+0002_m3\.sql$`).MatchString(out) ||
		!regexp.MustCompile(`(?m)^3\s+applied\s+\d{4}-\d\d-\d\dT\S+\s+0003_m4\.sql$`).MatchString(out) ||
		!regexp.MustCompile(`(?m)^4\s+applied\s+\d{4}-\d\d-\d\dT\S+\s+0004_m5\.sql$`).MatchString(out) {
		t.Errorf("migrate status = %v:\n%s", err, out)
	}

	// The URL from the configuration file; down (0004, 0003, 0002, then 0001), then status shows all four pending.
	dir := t.TempDir()
	cfg := filepath.Join(dir, "kontra.yaml")
	y := "public_url: http://localhost:8080\npostgres: \"" + dsn + "\"\n"
	if err := os.WriteFile(cfg, []byte(y), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err = runCmd(t, "migrate", "-f", cfg, "down")
	if err != nil || !strings.Contains(out, "OK down 0004_m5.sql") || !strings.Contains(out, "database at version 3") {
		t.Errorf("migrate down = %v:\n%s", err, out)
	}
	out, err = runCmd(t, "migrate", "-f", cfg, "down")
	if err != nil || !strings.Contains(out, "OK down 0003_m4.sql") || !strings.Contains(out, "database at version 2") {
		t.Errorf("migrate down = %v:\n%s", err, out)
	}
	out, err = runCmd(t, "migrate", "-f", cfg, "down")
	if err != nil || !strings.Contains(out, "OK down 0002_m3.sql") || !strings.Contains(out, "database at version 1") {
		t.Errorf("migrate down = %v:\n%s", err, out)
	}
	out, err = runCmd(t, "migrate", "-f", cfg, "down")
	if err != nil || !strings.Contains(out, "OK down 0001_init.sql") || !strings.Contains(out, "database at version 0") {
		t.Errorf("second migrate down = %v:\n%s", err, out)
	}
	if err := m.Check(context.Background()); !errors.Is(err, migrate.ErrSchemaNotCurrent) {
		t.Errorf("Check after down = %v", err)
	}
	out, err = runCmd(t, "migrate", "-f", cfg, "status")
	if err != nil || !regexp.MustCompile(`(?m)^1\s+pending\s+-\s+0001_init\.sql$`).MatchString(out) ||
		!regexp.MustCompile(`(?m)^2\s+pending\s+-\s+0002_m3\.sql$`).MatchString(out) ||
		!regexp.MustCompile(`(?m)^3\s+pending\s+-\s+0003_m4\.sql$`).MatchString(out) ||
		!regexp.MustCompile(`(?m)^4\s+pending\s+-\s+0004_m5\.sql$`).MatchString(out) {
		t.Errorf("status after down = %v:\n%s", err, out)
	}
	out, err = runCmd(t, "migrate", "-f", cfg, "down")
	if err != nil || !strings.Contains(out, "no migration to roll back") {
		t.Errorf("down with nothing applied = %q, %v", out, err)
	}
	out, err = runCmd(t, "migrate", "-f", cfg) // up is the default action
	if err != nil || !strings.Contains(out, "OK up 0001_init.sql") || !strings.Contains(out, "OK up 0002_m3.sql") || !strings.Contains(out, "OK up 0003_m4.sql") ||
		!strings.Contains(out, "OK up 0004_m5.sql") {
		t.Errorf("migrate (default up) = %q, %v", out, err)
	}
	if err := m.Check(context.Background()); err != nil {
		t.Errorf("Check after down and up: %v", err)
	}
}

// health succeeds only on a 200 from /healthz; a loopback listener's certificate is not verified (the compose
// healthcheck probes the container's own listener, whose certificate names public_url's host).
func TestHealth(t *testing.T) {
	var code atomic.Int32
	code.Store(http.StatusOK)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(int(code.Load()))
	}))
	defer srv.Close()
	out, err := runCmd(t, "health", "--url", srv.URL+"/healthz")
	if err != nil || strings.TrimSpace(out) != "ok" {
		t.Fatalf("health on a 200 = %q, %v", out, err)
	}
	code.Store(http.StatusServiceUnavailable)
	if _, err := runCmd(t, "health", "--url", srv.URL+"/healthz"); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("health on a 503 = %v, want an error naming it", err)
	}
	if _, err := runCmd(t, "health", "--url", "http://127.0.0.1:1/healthz"); err == nil {
		t.Fatal("health with nothing listening succeeded")
	}
	for _, args := range [][]string{{"health", "--url", "ftp://127.0.0.1/healthz"}, {"health", "extra"}, {"health", "--nope"}} {
		var u usageError
		if _, err := runCmd(t, args...); !errors.As(err, &u) {
			t.Errorf("run %q = %v, want a usage error", args, err)
		}
	}
	// A host that is not loopback is verified: httptest's certificate is for 127.0.0.1 and example.com, issued by
	// a CA no system pool holds.
	if loopbackHost("gate.example.com") || !loopbackHost("127.0.0.1") || !loopbackHost("::1") || !loopbackHost("localhost") {
		t.Error("loopbackHost misclassifies a host")
	}
}
