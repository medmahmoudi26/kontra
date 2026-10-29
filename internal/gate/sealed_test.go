package gate

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"kontra.sh/kontra/internal/card"
	"kontra.sh/kontra/internal/enroll"
	"kontra.sh/kontra/internal/pgtest"
	"kontra.sh/kontra/internal/sealed"
	"kontra.sh/kontra/internal/tunnel"
)

// sealedParams reads the sealed extension's params of a served card (ext-sealed.md §1).
func sealedParams(t *testing.T, body []byte) (recipients []string, required bool, entryRequired bool) {
	t.Helper()
	var c struct {
		Capabilities struct {
			Extensions []struct {
				URI      string `json:"uri"`
				Required bool   `json:"required"`
				Params   struct {
					Recipients []string `json:"recipients"`
					Envelope   string   `json:"envelope"`
					Required   bool     `json:"required"`
				} `json:"params"`
			} `json:"extensions"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(body, &c); err != nil {
		t.Fatal(err)
	}
	for _, e := range c.Capabilities.Extensions {
		if e.URI == sealed.URI {
			if e.Params.Envelope != sealed.MediaType {
				t.Errorf("envelope %q", e.Params.Envelope)
			}
			return e.Params.Recipients, e.Params.Required, e.Required
		}
	}
	t.Fatalf("no sealed extension in %s", body)
	return nil, false, false
}

// A sealed tunnel service (ext-sealed.md §1, D61, tunnel.md §5, §12): its served card and its MCP initialize object
// carry the keys enrolled for it, neither revoked nor rotated, connected or not; an enrollment and a revocation
// republish them; a sealed A2A service whose upstream card offers no A2A 1.0 interface has no card (D165).
func TestSealedServiceRecipients(t *testing.T) {
	dsn := pgtest.Migrated(t)
	pool := pgtest.Pool(t, dsn)
	ctx := context.Background()
	cfg := parseConfig(t, `services:
  vault:
    kind: a2a
    backend: { tunnel: {} }
    allow: ["org:default"]
    sealed: true
  fs:
    kind: mcp
    backend: { tunnel: {} }
    allow: ["org:default"]
    sealed: { required: false }
`)
	fa := &fakeTunnels{}
	s := NewServices(pool, ServicesOptions{PublicURL: "http://localhost:8080", Tunnels: fa})
	if err := s.Sync(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	vault, _ := s.Lookup(ctx, TeamTenant, "vault")
	if vault == nil || vault.Ref.Sealed != "required" {
		t.Fatalf("the sealed tunnel service is not served: %+v", vault)
	}
	tid := vault.Ref.TenantID
	newKey := func(name string, services ...string) (string, string) {
		t.Helper()
		k, err := ecdh.X25519().GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		r, _ := tunnel.FormatRecipient(k.PublicKey())
		kid := tunnel.KeyID(r)
		if _, err := pool.Exec(ctx, `insert into kontra.tunnel_keys (key_id, tenant_id, pubkey, node_name) values ($1, $2, $3, $4)`,
			kid, tid, k.PublicKey().Bytes(), name); err != nil {
			t.Fatal(err)
		}
		for _, svc := range services {
			if _, err := pool.Exec(ctx, `insert into kontra.key_services (key_id, tenant_id, service) values ($1, $2, $3)`, kid, tid, svc); err != nil {
				t.Fatal(err)
			}
		}
		return kid, r
	}
	k1, r1 := newKey("n1", "vault", "fs")
	_, r2 := newKey("n2", "vault")
	k3, _ := newKey("revoked", "vault")
	k4, _ := newKey("rotated", "vault")
	if _, err := pool.Exec(ctx, `update kontra.tunnel_keys set revoked_at = now(), revoked_reason = 'operator' where key_id = $1`, k3); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `update kontra.tunnel_keys set revoked_reason = 'rotated' where key_id = $1`, k4); err != nil {
		t.Fatal(err)
	}
	if err := s.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	fa.set(tunnelCardV1, true)
	fa.connected(tid, "vault")
	vault, _ = s.Lookup(ctx, TeamTenant, "vault")
	served, err := vault.Cards()
	if err != nil {
		t.Fatal(err)
	}
	got, required, entryRequired := sealedParams(t, served.Body)
	if len(got) != 2 || got[0] != r1 || got[1] != r2 || !required || entryRequired {
		t.Fatalf("recipients %v required %v, entry required %v; want [%s %s] true false", got, required, entryRequired, r1, r2)
	}
	fs, _ := s.MCPService(ctx, TeamTenant, "fs")
	if fs == nil || !fs.Sealed || fs.SealedRequired {
		t.Fatalf("fs: %+v", fs)
	}
	var capObj struct {
		URI    string `json:"uri"`
		Params struct {
			Recipients []string `json:"recipients"`
			Required   bool     `json:"required"`
		} `json:"params"`
	}
	if err := json.Unmarshal(fs.SealedCapability, &capObj); err != nil || capObj.URI != sealed.URI || len(capObj.Params.Recipients) != 1 ||
		capObj.Params.Recipients[0] != r1 || capObj.Params.Required {
		t.Fatalf("fs's capability %s", fs.SealedCapability)
	}

	// A second node enrolls: its key is republished on the enrollment's notice.
	k5, r5 := newKey("n5", "vault")
	s.RecipientsChanged(enroll.EnrolledPayload(enroll.Binding{KeyID: k5, TenantID: tid, Service: "vault"}))
	fa.connected(tid, "vault")
	vault, _ = s.Lookup(ctx, TeamTenant, "vault")
	served, _ = vault.Cards()
	if got, _, _ = sealedParams(t, served.Body); len(got) != 3 || got[2] != r5 {
		t.Fatalf("after an enrollment: %v", got)
	}
	// k1's binding is revoked: it leaves the card at the revocation's notice.
	if _, err := pool.Exec(ctx, `update kontra.key_services set revoked_at = now() where key_id = $1 and service = 'vault'`, k1); err != nil {
		t.Fatal(err)
	}
	s.RecipientsChanged(enroll.BindingRevokePayload(k1, "vault"))
	fa.connected(tid, "vault")
	vault, _ = s.Lookup(ctx, TeamTenant, "vault")
	served, _ = vault.Cards()
	if got, _, _ = sealedParams(t, served.Body); len(got) != 2 || got[0] != r2 || got[1] != r5 {
		t.Fatalf("after a revocation: %v", got)
	}
	fs, _ = s.MCPService(ctx, TeamTenant, "fs")
	if err := json.Unmarshal(fs.SealedCapability, &capObj); err != nil || len(capObj.Params.Recipients) != 1 {
		t.Fatalf("fs keeps its binding: %s", fs.SealedCapability)
	}

	// D165: an upstream card that offers only A2A 0.3 leaves the sealed service without a card.
	const card03 = `{"name":"old","url":"http://127.0.0.1:9999/","protocolVersion":"0.3.0","preferredTransport":"JSONRPC",` +
		`"capabilities":{},"defaultInputModes":["text/plain"],"defaultOutputModes":["text/plain"],"skills":[],"version":"1"}`
	fa.set(card03, true)
	if _, err := pool.Exec(ctx, `update kontra.services set upstream_card = null, card = null, card_fetched_at = null where name = 'vault'`); err != nil {
		t.Fatal(err)
	}
	f2 := &fakeTunnels{}
	f2.set(card03, true)
	s2 := NewServices(pool, ServicesOptions{PublicURL: "http://localhost:8080", Tunnels: f2})
	if err := s2.Sync(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	f2.connected(tid, "vault")
	if f2.triesSince(time.Time{}) == 0 {
		t.Fatal("the card was not fetched")
	}
	v2, _ := s2.Lookup(ctx, TeamTenant, "vault")
	if _, err := v2.Cards(); !errors.Is(err, card.ErrUnavailable) {
		t.Fatalf("a sealed service with a 0.3 card: %v", err)
	}
	_ = uuid.Nil
}
