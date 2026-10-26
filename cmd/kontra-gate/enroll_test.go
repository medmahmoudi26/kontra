package main

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"kontra.sh/kontra/internal/audit"
	"kontra.sh/kontra/internal/auth"
	enrollpkg "kontra.sh/kontra/internal/enroll"
	"kontra.sh/kontra/internal/pgtest"
	"kontra.sh/kontra/internal/tenant"
	"kontra.sh/kontra/internal/tunnel"
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
	// A service token for a service this gate does not have: an error, and nothing on stdout.
	if out, _, err := runSplit(t, "enroll", "-f", cfg, "hello"); err == nil || out != "" {
		t.Errorf("enroll <unknown service> = %q %v", out, err)
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

var tokenLine = regexp.MustCompile(`^ket_[0-9A-Za-z]{32}\n$`)

// tunnelServices is a services: block with a tunnel service, a direct one and one removed later.
const tunnelServices = `services:
  hello:
    kind: a2a
    backend: { tunnel: {} }
  recon:
    kind: a2a
    backend: { tunnel: {} }
  direct:
    kind: a2a
    backend: { a2a: { url: "http://127.0.0.1:9999", allow_private: true } }
`

// gateRows writes the services rows `kontra-gate serve` loads from tunnelServices (the sync), plus a
// disabled tunnel service.
func gateRows(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	team, err := tenant.EnsureTeam(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`insert into kontra.services (tenant_id, name, kind, backend, source) values ($1, 'hello', 'a2a', '{"tunnel": {}}', 'config')`,
		`insert into kontra.services (tenant_id, name, kind, backend, source) values ($1, 'recon', 'a2a', '{"tunnel": {}}', 'config')`,
		`insert into kontra.services (tenant_id, name, kind, backend, source) values ($1, 'direct', 'a2a', '{"a2a": {"url": "http://127.0.0.1:9999"}}', 'config')`,
		`insert into kontra.services (tenant_id, name, kind, backend, source, disabled_at) values ($1, 'gone', 'a2a', '{"tunnel": {}}', 'config', now())`,
	} {
		if _, err := pool.Exec(ctx, q, team); err != nil {
			t.Fatal(err)
		}
	}
	return team
}

type nodeKey struct {
	pub       []byte
	recipient string
	kid       string
}

func newNodeKey(t *testing.T) nodeKey {
	t.Helper()
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	r, err := tunnel.FormatRecipient(priv.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	return nodeKey{pub: priv.PublicKey().Bytes(), recipient: r, kid: tunnel.KeyID(r)}
}

// admitWith is the gate's admission of a proven hello from k declaring services, with tokens.
func admitWith(t *testing.T, pool *pgxpool.Pool, k nodeKey, services []string, tokens ...string) (*enrollpkg.Decision, error) {
	t.Helper()
	h := &tunnel.Hello{T: tunnel.MsgHello, V: tunnel.Version, Origin: "http://localhost:8080", KID: k.kid, Recipient: k.recipient,
		Name: "box", Enroll: tokens, Agent: "kontra/test"}
	for _, s := range services {
		h.Services = append(h.Services, tunnel.Service{Kind: tunnel.KindA2A, Name: s})
	}
	store := enrollpkg.NewStore(pool, enrollpkg.Options{PublicURL: "http://localhost:8080"})
	return store.Admit(context.Background(), enrollpkg.Request{Hello: h, PubKey: k.pub, SessionID: uuid.Must(uuid.NewV7())})
}

// `kontra-gate enroll <service>` prints a service token alone on stdout: one line, ket_ and 32 base62
// characters. The row holds its sha256 and expires 15 minutes after it was created; the mint is an audit
// row. An unknown, disabled or non-tunnel service is an error that writes nothing. The token enrolls a node.
func TestEnrollServiceToken(t *testing.T) {
	dsn := pgtest.Database(t)
	cfg := writeConfig(t, dsn, tunnelServices)
	ctx := context.Background()
	if _, err := runCmd(t, "migrate", "-f", cfg); err != nil {
		t.Fatal(err)
	}
	pool := pgtest.Pool(t, dsn)

	// Before serve ever loaded the services: an error that names the fix.
	if out, _, err := runSplit(t, "enroll", "-f", cfg, "hello"); err == nil || out != "" || !strings.Contains(err.Error(), "kontra-gate serve") {
		t.Fatalf("enroll before serve = %q %v", out, err)
	}
	gateRows(t, pool)

	stdout, stderr, err := runSplit(t, "enroll", "-f", cfg, "hello")
	if err != nil {
		t.Fatal(err)
	}
	if !tokenLine.MatchString(stdout) {
		t.Fatalf("stdout %q, want the token alone on one line", stdout)
	}
	tok := strings.TrimSuffix(stdout, "\n")
	if strings.Contains(stderr, tok[4:]) || !strings.Contains(stderr, "service hello") || !strings.Contains(stderr, "shown once") {
		t.Errorf("stderr %q: it names the service and never the token", stderr)
	}
	var (
		hash             []byte
		scope, service   string
		created, expires time.Time
	)
	if err := pool.QueryRow(ctx, `select token_hash, scope, service, created_at, expires_at from kontra.enrollment_tokens`).
		Scan(&hash, &scope, &service, &created, &expires); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(tok))
	if !bytes.Equal(hash, sum[:]) || scope != "service" || service != "hello" || !expires.Equal(created.Add(15*time.Minute)) {
		t.Errorf("row: scope %s service %s created %s expires %s", scope, service, created, expires)
	}
	rows, err := audit.Verify(ctx, pool)
	if err != nil || len(rows) != 1 || !bytes.Contains(rows[0].RowJCS, []byte(`"event":"tunnel.token_minted"`)) ||
		bytes.Contains(rows[0].RowJCS, []byte(tok[4:])) {
		t.Errorf("audit: %d rows, %v", len(rows), err)
	}

	// The service may come before the flags.
	stdout2, _, err := runSplit(t, "enroll", "recon", "-f", cfg)
	if err != nil || !tokenLine.MatchString(stdout2) {
		t.Errorf("enroll recon -f: %q %v", stdout2, err)
	}

	// Refused, with nothing written.
	for svc, want := range map[string]string{"nope": "no such service", "gone": "disabled", "direct": "not tunnel"} {
		out, _, err := runSplit(t, "enroll", "-f", cfg, svc)
		if err == nil || out != "" || !strings.Contains(err.Error(), want) {
			t.Errorf("enroll %s = %q %v, want %q", svc, out, err, want)
		}
	}
	var n int
	if err := pool.QueryRow(ctx, `select count(*) from kontra.enrollment_tokens`).Scan(&n); err != nil || n != 2 {
		t.Errorf("%d token rows, want 2", n)
	}
	if rows, err := audit.Verify(ctx, pool); err != nil || len(rows) != 2 {
		t.Errorf("audit: %d rows, %v", len(rows), err)
	}
	// A multi-tenant gate's operator mints its own.
	multi := writeConfig(t, dsn, "tenancy: multi\n")
	if out, _, err := runSplit(t, "enroll", "-f", multi, "hello"); err == nil || out != "" {
		t.Errorf("multi-tenant gate: %q %v", out, err)
	}

	// The printed token enrolls a node once.
	k := newNodeKey(t)
	d, err := admitWith(t, pool, k, []string{"hello"}, tok)
	if err != nil || len(d.Served) != 1 || d.Served[0].URL != "http://localhost:8080/agents/hello" {
		t.Fatalf("admit with the printed token: %+v %v", d, err)
	}
	_, err = admitWith(t, pool, newNodeKey(t), []string{"hello"}, tok)
	var herr *tunnel.HandshakeError
	if !errors.As(err, &herr) || herr.Code != tunnel.CodeEnrollInvalid {
		t.Errorf("the token again: %v", err)
	}
}

// `kontra-gate enroll --revoke <key_id>` revokes a node key, and with --service one of its bindings: the row,
// an audit row and NOTIFY kontra_revoke; nothing on stdout. The key's next handshake is revoked, or leaves
// the service out.
func TestEnrollRevoke(t *testing.T) {
	dsn := pgtest.Database(t)
	cfg := writeConfig(t, dsn, tunnelServices)
	ctx := context.Background()
	if _, err := runCmd(t, "migrate", "-f", cfg); err != nil {
		t.Fatal(err)
	}
	pool := pgtest.Pool(t, dsn)
	gateRows(t, pool)
	tokHello, _, err := runSplit(t, "enroll", "-f", cfg, "hello")
	if err != nil {
		t.Fatal(err)
	}
	tokRecon, _, err := runSplit(t, "enroll", "-f", cfg, "recon")
	if err != nil {
		t.Fatal(err)
	}
	k := newNodeKey(t)
	if _, err := admitWith(t, pool, k, []string{"hello", "recon"}, strings.TrimSpace(tokHello), strings.TrimSpace(tokRecon)); err != nil {
		t.Fatal(err)
	}
	listen, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer listen.Close(context.Background())
	if _, err := listen.Exec(ctx, "listen kontra_revoke"); err != nil {
		t.Fatal(err)
	}
	next := func() string {
		wctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		n, err := listen.WaitForNotification(wctx)
		if err != nil {
			return ""
		}
		return n.Payload
	}

	// One binding.
	stdout, stderr, err := runSplit(t, "enroll", "-f", cfg, "--revoke", k.kid, "--service", "recon")
	if err != nil || stdout != "" || !strings.Contains(stderr, "recon") {
		t.Fatalf("--revoke --service: %q %q %v", stdout, stderr, err)
	}
	if p := next(); p != "binding:"+k.kid+"/recon" {
		t.Errorf("NOTIFY %q", p)
	}
	d, err := admitWith(t, pool, k, []string{"hello", "recon"})
	if err != nil || len(d.Served) != 1 || d.Served[0].Name != "hello" {
		t.Errorf("after revoking recon: %+v %v", d, err)
	}
	if _, _, err := runSplit(t, "enroll", "-f", cfg, "--revoke", k.kid, "--service", "direct"); err == nil {
		t.Error("revoking a binding the key does not have")
	}

	// The key.
	stdout, stderr, err = runSplit(t, "enroll", "-f", cfg, "--revoke", k.kid)
	if err != nil || stdout != "" || !strings.Contains(stderr, "revoked node key "+k.kid) {
		t.Fatalf("--revoke: %q %q %v", stdout, stderr, err)
	}
	if p := next(); p != "key:"+k.kid {
		t.Errorf("NOTIFY %q", p)
	}
	var reason string
	if err := pool.QueryRow(ctx, `select revoked_reason from kontra.tunnel_keys where key_id = $1 and revoked_at is not null`, k.kid).Scan(&reason); err != nil || reason != "operator" {
		t.Errorf("revoked_reason %q %v", reason, err)
	}
	_, err = admitWith(t, pool, k, []string{"hello"})
	var herr *tunnel.HandshakeError
	if !errors.As(err, &herr) || herr.Code != tunnel.CodeRevoked {
		t.Errorf("the revoked key's handshake: %v", err)
	}
	// Again: said, not an error, and nothing new.
	if _, stderr, err := runSplit(t, "enroll", "-f", cfg, "--revoke", k.kid); err != nil || !strings.Contains(stderr, "already revoked") {
		t.Errorf("revoking twice: %q %v", stderr, err)
	}
	rows, err := audit.Verify(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	var revocations int
	for _, r := range rows {
		if bytes.Contains(r.RowJCS, []byte(`"event":"tunnel.key_revoked"`)) || bytes.Contains(r.RowJCS, []byte(`"event":"tunnel.binding_revoked"`)) {
			revocations++
		}
	}
	if revocations != 2 {
		t.Errorf("%d revocation audit rows, want 2", revocations)
	}
	// An unknown key.
	if _, _, err := runSplit(t, "enroll", "-f", cfg, "--revoke", "k1_aaaaaaaaaaaaaaaaaaaaaaaaaa"); err == nil || !strings.Contains(err.Error(), "no node key") {
		t.Errorf("an unknown key: %v", err)
	}
}
