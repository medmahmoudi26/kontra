package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"

	"kontra.sh/kontra/internal/audit"
	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/pgtest"
)

func runSplit(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	err = run(args, &out, &errOut)
	return out.String(), errOut.String(), err
}

func writeConfig(t *testing.T, dsn, extra string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kontra.yaml")
	y := "public_url: http://localhost:8080\npostgres: \"" + dsn + "\"\n" + extra
	if err := os.WriteFile(path, []byte(y), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

var keyLine = regexp.MustCompile(`^kk_[a-z0-9]{1,16}_[A-Za-z0-9]{32}\n$`)

// The README order on a fresh database: migrate, then `enroll --api-key demo`, which upserts the team
// tenant and prints only the key on stdout; api_keys holds its sha256 and prefix; the key authenticates
// as key:<prefix> with the tenant, a buyer and the hire scope; the mint is an audit row.
func TestEnrollAPIKey(t *testing.T) {
	dsn := pgtest.Database(t)
	cfg := writeConfig(t, dsn, "auth: { api_keys: true }\n")
	ctx := context.Background()

	// Before migrate: refused, naming the fix.
	if _, _, err := runSplit(t, "enroll", "-f", cfg, "--api-key", "demo"); err == nil || !strings.Contains(err.Error(), "kontra-gate migrate") {
		t.Fatalf("enroll on an unmigrated database = %v", err)
	}
	if _, err := runCmd(t, "migrate", "-f", cfg); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, err := runSplit(t, "enroll", "-f", cfg, "--api-key", "demo")
	if err != nil {
		t.Fatal(err)
	}
	if !keyLine.MatchString(stdout) || !strings.HasPrefix(stdout, "kk_demo_") {
		t.Fatalf("stdout %q, want the key alone on one line", stdout)
	}
	key := strings.TrimSuffix(stdout, "\n")
	if strings.Contains(stderr, key) || strings.Contains(stderr, key[16:]) || !strings.Contains(stderr, "key:"+key[:16]) {
		t.Errorf("stderr %q: it names the principal and never the secret", stderr)
	}

	pool := pgtest.Pool(t, dsn)
	var (
		hash        []byte
		prefix      string
		scopes      []string
		tenantSlug  string
		tenantKind  string
		buyer       uuid.UUID
		tenantCount int
	)
	err = pool.QueryRow(ctx, `select k.hash, k.prefix, k.scopes, t.slug, t.kind, k.buyer_id, (select count(*) from kontra.tenants)
		from kontra.api_keys k join kontra.tenants t on t.id = k.tenant_id`).Scan(&hash, &prefix, &scopes, &tenantSlug, &tenantKind, &buyer, &tenantCount)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(key))
	if !bytes.Equal(hash, sum[:]) || prefix != key[:16] || len(scopes) != 1 || scopes[0] != "hire" ||
		tenantSlug != "default" || tenantKind != "team" || tenantCount != 1 {
		t.Errorf("row: prefix %q scopes %v tenant %s/%s (%d tenants)", prefix, scopes, tenantSlug, tenantKind, tenantCount)
	}
	p, err := auth.NewAPIKeys(pool, nil).Authenticate(ctx, key)
	if err != nil || p.ID != "key:"+prefix || p.TenantSlug != "default" || p.BuyerID != buyer || !p.HasScope(auth.ScopeHire) {
		t.Errorf("authenticate the printed key: %+v %v", p, err)
	}

	// A second key: operator prefix and scopes; the same tenant, its own buyer.
	stdout, _, err = runSplit(t, "enroll", "-f", cfg, "--api-key", "Node One", "--prefix", "acme", "--scope", "hire", "--scope", "enroll:hello")
	if err != nil {
		t.Fatal(err)
	}
	if !keyLine.MatchString(stdout) || !strings.HasPrefix(stdout, "kk_acme_") {
		t.Fatalf("second key %q", stdout)
	}
	p2, err := auth.NewAPIKeys(pool, nil).Authenticate(ctx, strings.TrimSpace(stdout))
	if err != nil || !p2.HasScope("enroll:hello") || p2.BuyerID == buyer || *p2.TenantID != *p.TenantID {
		t.Errorf("second key: %+v %v", p2, err)
	}
	rows, err := audit.Verify(ctx, pool)
	if err != nil || len(rows) != 2 {
		t.Errorf("audit: %d rows, %v", len(rows), err)
	}

	// Usage errors print no key.
	for _, args := range [][]string{
		{"enroll", "-f", cfg, "--api-key", ""},
		{"enroll", "-f", cfg, "--api-key", "x", "--prefix", "ACME"},
		{"enroll", "-f", cfg, "--api-key", "x", "--scope", "admin"},
		{"enroll", "-f", cfg, "--api-key", "x", "hello"},
		{"enroll", "-f", cfg, "--prefix", "acme"},
		{"enroll", "-f", cfg, "--api-key", "a\nb"},
	} {
		out, _, err := runSplit(t, args...)
		var u usageError
		if !errors.As(err, &u) || out != "" {
			t.Errorf("%q = %q, %v; want a usage error and no output", args, out, err)
		}
	}
	// Service tokens and --revoke are the tunnel's.
	if out, _, err := runSplit(t, "enroll", "-f", cfg, "hello"); err != nil || !strings.Contains(out, "not until M2") {
		t.Errorf("enroll <service> = %q %v", out, err)
	}

	// auth.api_keys off: the key is minted, with a warning.
	off := writeConfig(t, dsn, "")
	stdout, stderr, err = runSplit(t, "enroll", "-f", off, "--api-key", "later")
	if err != nil || !keyLine.MatchString(stdout) || !strings.Contains(stderr, "auth.api_keys") {
		t.Errorf("api_keys off: %q %q %v", stdout, stderr, err)
	}
	// A multi-tenant gate's operator writes its own keys.
	multi := writeConfig(t, dsn, "tenancy: multi\n")
	if stdout, _, err := runSplit(t, "enroll", "-f", multi, "--api-key", "x"); err == nil || stdout != "" {
		t.Errorf("multi-tenant gate: %q %v", stdout, err)
	}
}
