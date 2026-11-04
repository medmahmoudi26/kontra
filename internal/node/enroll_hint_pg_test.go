package node

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/enroll"
	"kontra.sh/kontra/internal/tunnel"
)

// The README's "Add a service to a node" against the real admission. A node whose key a tenant token enrolled for
// two services is restarted with a third --serve and no token naming it: the gate refuses it service_denied in the
// words the node's hint reads (nothing enrolled, nothing served), and the node logs how to add it. Restarted with a
// tenant token naming only the new service and every --serve, it is welcomed for all three: the key keeps its
// bindings, and the used token still on the command line is not sent. A later restart needs no --enroll.
//
// D331: the same with kontra login's API key (hire + enroll:*) on a gate that takes enroll: scopes, as Kontra
// Cloud's does: the scope names the new service but never creates it ("no such service", the same hint), and the
// tenant token naming it creates it although the scope is tried first.
func TestRealAdmissionAddAService(t *testing.T) {
	t.Run("no API key", func(t *testing.T) { testAddAService(t, false) })
	t.Run("kontra login's API key", func(t *testing.T) { testAddAService(t, true) })
}

func testAddAService(t *testing.T, login bool) {
	ctx := context.Background()
	rg := newRealGateWith(t, func(o *enroll.Options) { o.EnrollWithAPIKey = login })
	rec := newRecorder(t)
	first, err := rg.store.MintTenantToken(ctx, rg.team, []string{"search", "fetch"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t, rg.url, "", a2aServe("search", rec.url()), a2aServe("fetch", rec.url()))
	cfg.Enroll = []string{first}
	refusal := "translate: the key is not enrolled for it; nothing was enrolled"
	if login {
		key, err := rg.keys.Mint(ctx, auth.MintRequest{TenantID: rg.team, Name: "login",
			Scopes: []auth.Scope{auth.ScopeHire, auth.ScopeEnrollAll}})
		if err != nil {
			t.Fatal(err)
		}
		cfg.APIKey = key.Secret
		refusal = "translate: no such service; nothing was enrolled"
	}
	tn := mustNode(t, rg.testGate, cfg).run(t)
	rg.next(t)
	rg.session(t)
	tn.welcomed(t)
	kid := tn.n.cfgKeyID(t)
	tn.stop()
	tn.result(t, 10*time.Second)
	bindings := func() int {
		return rg.count(t, `select count(*) from kontra.key_services where key_id = $1 and revoked_at is null`, kid)
	}

	// A third --serve with no token naming it.
	cfg.Serves = append(cfg.Serves, a2aServe("translate", rec.url()))
	tn = mustNode(t, rg.testGate, cfg).run(t)
	if r := rg.next(t); len(r.Hello.Enroll) != 0 {
		t.Fatalf("the restart sent %v", r.Hello.Enroll)
	}
	err = tn.result(t, 10*time.Second)
	var herr *tunnel.HandshakeError
	if !errors.As(err, &herr) || herr.Code != tunnel.CodeServiceDenied || herr.Message != refusal {
		t.Fatalf("Run = %v", err)
	}
	logs := tn.logs.String()
	for _, w := range []string{"to add translate to this node", "--enroll <token> and every --serve flag",
		"without --serve translate serves the others"} {
		if !strings.Contains(logs, w) {
			t.Errorf("the node's log does not say %q:\n%s", w, logs)
		}
	}
	if n := bindings(); n != 2 {
		t.Fatalf("%d bindings after service_denied", n)
	}

	// A tenant token naming only the new service, and every --serve.
	second, err := rg.store.MintTenantToken(ctx, rg.team, []string{"translate"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Enroll = []string{first, second}
	tn = mustNode(t, rg.testGate, cfg).run(t)
	if r := rg.next(t); !slices.Equal(r.Hello.Enroll, []string{second}) {
		t.Fatalf("hello: %v, want the new token alone", r.Hello.Enroll)
	}
	s := rg.session(t)
	for _, svc := range []string{"search", "fetch", "translate"} {
		if resp, _ := exchange(t, s, svc, "/x"); resp.StatusCode != 200 {
			t.Fatalf("%s is not served: %d", svc, resp.StatusCode)
		}
	}
	if n := bindings(); n != 3 {
		t.Fatalf("%d bindings", n)
	}
	eventually(t, 5*time.Second, "both tokens consumed", func() bool {
		c := tn.creds(t)
		return slices.Contains(c.Consumed, TokenHash(first)) && slices.Contains(c.Consumed, TokenHash(second))
	})
	tn.stop()
	tn.result(t, 10*time.Second)

	// Later restarts carry no token.
	cfg.Enroll = nil
	mustNode(t, rg.testGate, cfg).run(t)
	if r := rg.next(t); len(r.Hello.Enroll) != 0 {
		t.Fatalf("a later restart sent %v", r.Hello.Enroll)
	}
	if resp, _ := exchange(t, rg.session(t), "translate", "/x"); resp.StatusCode != 200 {
		t.Fatalf("translate after a plain restart: %d", resp.StatusCode)
	}
}
