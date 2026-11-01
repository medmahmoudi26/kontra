package gate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"kontra.sh/kontra/internal/ids"
	"kontra.sh/kontra/internal/mcprelay/mcptest"
	"kontra.sh/kontra/internal/oidc"
	"kontra.sh/kontra/internal/oidc/oidctest"
)

// oidcConfig is replicaConfig with auth.oidc and auth.mcp_oauth naming issuer, both with the tenant claim "tenant"
// and the issuer's revocation feed (poll 1s, max_stale 3s), and the tenancy and services given.
func (e *gateEnv) oidcConfig(port int, as *oidctest.Server, tenancy, services string) string {
	e.t.Helper()
	feed := fmt.Sprintf(`{ url: %q, poll: 1s, max_stale: 3s }`, as.RevocationsURL())
	doc := fmt.Sprintf(`listen: 127.0.0.1:%d
public_url: http://127.0.0.1:%d
postgres: %s
temporal: { address: %s, namespace: kontra }
metrics: { listen: "127.0.0.1:0" }
tenancy: %s
auth:
  api_keys: true
  oidc: { issuer: %q, audience: kontra, tenant_claim: tenant, revocations: %s }
  mcp_oauth: { issuer: %q, tenant_claim: tenant, revocations: %s }
drain_timeout: 1m
%s`, port, port, e.dsn, e.temporal, tenancy, as.Issuer(), feed, as.Issuer(), feed, services)
	path := filepath.Join(e.t.TempDir(), "kontra.yaml")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		e.t.Fatal(err)
	}
	return path
}

// a2aSend posts an A2A 1.0 JSON-RPC body with bearer and returns the status and the body.
func a2aSend(t *testing.T, url, bearer, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("A2A-Version", "1.0")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, string(b)
}

// sentTask is the task id of a SendMessage answer, "" when it is not one.
func sentTask(body string) string {
	var r struct {
		Result struct {
			Task struct {
				ID string `json:"id"`
			} `json:"task"`
		} `json:"result"`
	}
	_ = json.Unmarshal([]byte(body), &r)
	return r.Result.Task.ID
}

func metricsText(t *testing.T, r *Replica) string {
	t.Helper()
	resp, err := http.Get("http://" + r.MetricsAddr().String() + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// M5-G1 through a whole team gate (spec/relay.md §2.3, §12.1; D314 to D316), against the in-repository issuer with its
// own JWKS and revocation feed, auth.oidc and auth.mcp_oauth naming it:
//
//   - a token whose aud is kontra is a principal on the A2A routes: its task is via oidc, principal oidc:sub:alice,
//     buyer UUIDv5(issuer ‖ sub), and the same token reads it back (GetTask); a token for an MCP route's canonical URI
//     is 401 there, and the kontra token is 401 on /mcp;
//   - the tenant claim: a token naming default is admitted by org:default on A2A and on /mcp/{svc}, one without the
//     claim is not (the unknown service), one naming a tenant the gate does not have is 401;
//   - a revoked sid: its tokens are 401 within the feed's poll, on A2A and MCP, and another session is unaffected;
//   - the feed down: past max_stale every token of the issuer is the gate's own failure (-32603 on JSON-RPC, 503 on
//     MCP), never accepted, while an API key still works; the gauges say so; the feed back, tokens work again;
//   - a key rotated at the issuer: the previous key's tokens still verify, and the new kid's are accepted once the
//     minute since the last fetch has passed, after exactly one refetch (about a minute of waiting).
func TestGateA2AOIDC(t *testing.T) {
	e := newGateEnv(t)
	as, err := oidctest.New(oidctest.Config{Subject: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(as.Close)
	up := mcptest.New()
	up.Tools = []json.RawMessage{mcptest.Tool("echo", "")}
	direct := httptest.NewServer(up)
	t.Cleanup(direct.Close)
	services := `services:
  hi:
    kind: a2a
    backend: { a2a: { url: "` + e.seller.srv.URL + `/base", allow_private: true } }
    allow: ["oidc:sub:alice", "key:*"]
    task: { timeout: 10m, retries: 1 }
  team:
    kind: a2a
    backend: { a2a: { url: "` + e.seller.srv.URL + `/base", allow_private: true } }
    allow: ["org:default"]
    task: { timeout: 10m, retries: 1 }
  fs:
    kind: mcp
    backend: { mcp: { url: "` + direct.URL + `/mcp", allow_private: true } }
    allow: ["org:default"]
`
	port := freePort(t)
	a := e.startWith("a", e.oidcConfig(port, as, "single", services), fastTiming, WithOperatorRoots(as.CAPool()))
	pub := fmt.Sprintf("http://127.0.0.1:%d", port)
	exp := func() time.Time { return time.Now().Add(10 * time.Minute) }
	tok := func(sid string, aud string, claims map[string]any) string {
		return as.MustSign(oidctest.Token{Audience: []string{aud}, Session: sid, Expiry: exp(), Claims: claims})
	}
	send := sendBody("SendMessage", "hi", false)

	// A kontra token on the A2A routes.
	alice := tok("s-alice", "kontra", nil)
	status, body := a2aSend(t, pub+"/agents/hi", alice, send)
	task := sentTask(body)
	if status != http.StatusOK || task == "" {
		t.Fatalf("SendMessage with a kontra token: %d %s", status, body)
	}
	buyer, _ := ids.OIDCBuyer(as.Issuer(), "alice")
	if !scalar[bool](t, e.pool, `select via = 'oidc' and principal = 'oidc:sub:alice' and buyer_id = $2 from kontra.tasks where id = $1`, task, buyer) {
		t.Error("the task is not the token's (via oidc, oidc:sub:alice, UUIDv5(issuer ‖ sub))")
	}
	get := fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":"GetTask","params":{"id":%q}}`, task)
	if status, body := a2aSend(t, pub+"/agents/hi", tok("s-alice-2", "kontra", nil), get); status != http.StatusOK || !strings.Contains(body, task) {
		t.Errorf("GetTask with another token of alice: %d %s", status, body)
	}
	// Audiences do not cross: an MCP route's token on A2A, the kontra token on /mcp.
	if status, body := a2aSend(t, pub+"/agents/hi", tok("s-alice", oidc.CanonicalURI(pub, ""), nil), send); status != http.StatusUnauthorized {
		t.Errorf("an MCP token on an A2A route: %d %s", status, body)
	}
	if status, www, _ := oauthSend(t, http.MethodPost, pub+"/mcp", alice, initialize); status != http.StatusUnauthorized || !strings.Contains(www, `error="invalid_token"`) {
		t.Errorf("a kontra token on /mcp: %d %q", status, www)
	}

	// The tenant claim (D315).
	teamTok := tok("s-team", "kontra", map[string]any{"tenant": "default"})
	if status, body := a2aSend(t, pub+"/agents/team", teamTok, send); status != http.StatusOK || sentTask(body) == "" {
		t.Errorf("a token of the team tenant on org:default: %d %s", status, body)
	}
	if status, body := a2aSend(t, pub+"/agents/team", alice, send); status != http.StatusNotFound || !strings.Contains(body, "unknown service") {
		t.Errorf("a token without the tenant claim on org:default: %d %s", status, body)
	}
	if status, body := a2aSend(t, pub+"/agents/hi", tok("s-x", "kontra", map[string]any{"tenant": "acme"}), send); status != http.StatusUnauthorized {
		t.Errorf("a token naming a tenant the gate does not have: %d %s", status, body)
	}
	fsTok := tok("s-team", oidc.CanonicalURI(pub, "fs"), map[string]any{"tenant": "default"})
	if status, _, body := oauthSend(t, http.MethodPost, pub+"/mcp/fs", fsTok, initialize); status != http.StatusOK || !strings.Contains(string(body), `"result"`) {
		t.Errorf("an MCP token of the team tenant on org:default: %d %s", status, body)
	}
	if status, _, body := oauthSend(t, http.MethodPost, pub+"/mcp/fs", tok("s-alice", oidc.CanonicalURI(pub, "fs"), nil), initialize); status != http.StatusNotFound {
		t.Errorf("an MCP token without the tenant claim on org:default: %d %s", status, body)
	}

	// A revoked session (D316): 401 within the poll, on A2A and on MCP; another session of alice is unaffected.
	as.Revoke("s-alice")
	mcpAlice := tok("s-alice", oidc.CanonicalURI(pub, ""), nil)
	eventually(t, 10*time.Second, "the revoked session refused", func() bool {
		status, _ := a2aSend(t, pub+"/agents/hi", alice, send)
		return status == http.StatusUnauthorized
	})
	if status, www, _ := oauthSend(t, http.MethodPost, pub+"/mcp", mcpAlice, initialize); status != http.StatusUnauthorized || !strings.Contains(www, `error="invalid_token"`) {
		t.Errorf("the revoked session on /mcp: %d %q", status, www)
	}
	other := tok("s-alice-3", "kontra", nil)
	if status, body := a2aSend(t, pub+"/agents/hi", other, send); status != http.StatusOK {
		t.Errorf("another session of alice: %d %s", status, body)
	}
	m := metricsText(t, a.r)
	if !strings.Contains(m, `kontra_gate_oidc_revocations_fresh{replica="`+a.r.ID()+`",issuer="`+as.Issuer()+`"} 1`) ||
		strings.Count(m, "kontra_gate_oidc_revocations_age_seconds{") != 1 {
		t.Errorf("/metrics with a fresh feed (one feed for the issuer of both blocks):\n%s", m)
	}

	// The feed down: fail closed past max_stale; an API key still works; then back.
	as.FeedDown(http.StatusBadGateway)
	key := e.mintKey("buyer")
	eventually(t, 10*time.Second, "the issuer's tokens refused on a stale feed", func() bool {
		status, body := a2aSend(t, pub+"/agents/hi", other, send)
		return status == http.StatusOK && strings.Contains(body, `"code":-32603`)
	})
	if status, _, _ := oauthSend(t, http.MethodPost, pub+"/mcp/fs", fsTok, initialize); status != http.StatusServiceUnavailable {
		t.Errorf("an MCP token on a stale feed: %d, want 503", status)
	}
	if status, body := a2aSend(t, pub+"/agents/hi", key, send); status != http.StatusOK || sentTask(body) == "" {
		t.Errorf("an API key while the feed is stale: %d %s", status, body)
	}
	if m := metricsText(t, a.r); !strings.Contains(m, `kontra_gate_oidc_revocations_fresh{replica="`+a.r.ID()+`",issuer="`+as.Issuer()+`"} 0`) {
		t.Errorf("/metrics with a stale feed:\n%s", m)
	}
	as.FeedDown(0)
	eventually(t, 10*time.Second, "the issuer's tokens accepted again", func() bool {
		status, body := a2aSend(t, pub+"/agents/hi", other, send)
		return status == http.StatusOK && sentTask(body) != ""
	})

	// A key rotated at the issuer (D276's refetch, on the A2A routes): the old key's tokens still verify; the new
	// kid's are refused until a minute has passed since the key set was fetched, then fetched once and accepted.
	oldKid := as.Current(oidctest.AlgES256)
	kid, err := as.Rotate(oidctest.AlgES256)
	if err != nil {
		t.Fatal(err)
	}
	fetched := as.Fetches(oidctest.PathJWKS)
	previous := as.MustSign(oidctest.Token{Kid: oldKid, Audience: []string{"kontra"}, Session: "s-old", Expiry: exp()})
	if status, body := a2aSend(t, pub+"/agents/hi", previous, send); status != http.StatusOK || sentTask(body) == "" {
		t.Errorf("a token of the previous key after a rotation: %d %s", status, body)
	}
	deadline := time.Now().Add(80 * time.Second)
	for {
		rotated := as.MustSign(oidctest.Token{Kid: kid, Audience: []string{"kontra"}, Session: "s-rot", Expiry: exp()})
		status, body := a2aSend(t, pub+"/agents/hi", rotated, send)
		if status == http.StatusOK && sentTask(body) != "" {
			break
		}
		if status != http.StatusUnauthorized || time.Now().After(deadline) {
			t.Fatalf("a token of the rotated key: %d %s", status, body)
		}
		time.Sleep(time.Second)
	}
	if n := as.Fetches(oidctest.PathJWKS) - fetched; n != 1 {
		t.Errorf("the rotated key set was fetched %d times, want once", n)
	}
}

// D315 on a multi-tenant gate: a token's tenant claim names one of the operator's tenants by slug; org:<slug> on a
// service of that tenant admits it; a claim naming an anonymous tenant, or none of the gate's, is 401; a token without
// the claim is not an org: member.
func TestGateA2AOIDCMultiTenant(t *testing.T) {
	e := newGateEnv(t)
	as, err := oidctest.New(oidctest.Config{Subject: "carol"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(as.Close)
	port := freePort(t)
	e.startWith("a", e.oidcConfig(port, as, "multi", ""), fastTiming, WithOperatorRoots(as.CAPool()))
	pub := fmt.Sprintf("http://127.0.0.1:%d", port)
	ctx := context.Background()
	acme := uuid.Must(uuid.NewV7())
	if _, err := e.pool.Exec(ctx, `insert into kontra.tenants (id, slug, kind) values ($1, 'acme', 'account')`, acme); err != nil {
		t.Fatal(err)
	}
	backend := `{"a2a": {"url": "` + e.seller.srv.URL + `/base", "allow_private": true}}`
	if _, err := e.pool.Exec(ctx, `insert into kontra.services (tenant_id, name, kind, backend, allow, source)
		values ($1, 'hi', 'a2a', $2::jsonb, '{org:acme}', 'db')`, acme, backend); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `select pg_notify('kontra_services', 'acme/hi')`); err != nil {
		t.Fatal(err)
	}
	e.seedAnonymousService(pub, "anon-00000000000000c3", "open")
	exp := time.Now().Add(10 * time.Minute)
	tok := func(claims map[string]any) string {
		return as.MustSign(oidctest.Token{Audience: []string{"kontra"}, Session: uuid.NewString(), Expiry: exp, Claims: claims})
	}
	acmeTok := tok(map[string]any{"tenant": "acme"})
	send := sendBody("SendMessage", "hi", false)
	var task string
	eventually(t, 20*time.Second, "acme/hi served to acme's token", func() bool {
		status, body := a2aSend(t, pub+"/agents/acme/hi", acmeTok, send)
		task = sentTask(body)
		return status == http.StatusOK && task != ""
	})
	if !scalar[bool](t, e.pool, `select via = 'oidc' and principal = 'oidc:sub:carol' from kontra.tasks where id = $1`, task) {
		t.Error("the task is not carol's via oidc")
	}
	if status, body := a2aSend(t, pub+"/agents/acme/hi", tok(nil), send); status != http.StatusNotFound {
		t.Errorf("a token without the claim on org:acme: %d %s", status, body)
	}
	for _, slug := range []string{"anon-00000000000000c3", "globex", "Acme"} {
		if status, body := a2aSend(t, pub+"/agents/acme/hi", tok(map[string]any{"tenant": slug}), send); status != http.StatusUnauthorized {
			t.Errorf("a token naming %q: %d %s", slug, status, body)
		}
	}
	// The anonymous tenant's service admits anyone: a signed-in principal of acme reaches it as itself.
	if status, body := a2aSend(t, pub+"/agents/anon-00000000000000c3/open", acmeTok, send); status != http.StatusOK || sentTask(body) == "" {
		t.Errorf("acme's token on an anonymous service: %d %s", status, body)
	}
}
