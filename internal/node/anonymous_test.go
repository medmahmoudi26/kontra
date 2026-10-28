package node

import (
	"errors"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"kontra.sh/kontra/internal/enroll"
)

// anonymousDecision admits req as an anonymous session ending at end, at a multi-tenant URL.
func anonymousDecision(req enroll.Request, end time.Time) *enroll.Decision {
	d := admitAll(req)
	d.Key.Tenant = "anon-0123456789abcdef"
	for i := range d.Served {
		d.Served[i].URL = "https://gate.example/agents/anon-0123456789abcdef/" + d.Served[i].Name
	}
	d.Anonymous = &enroll.Anonymous{ExpiresAt: end, Public: req.Hello.Public}
	return d
}

// DM4-18: kontra connect asks for an anonymous session (hello.anonymous, with --public's names in hello.public) only
// when this origin's credentials hold no API key and no pending or consumed token and the command line gave neither
// --enroll nor --api-key; otherwise the hello carries neither field, and --public is only warned about. A welcome
// with anonymous prints the anonymous URLs, which ones are public, and the session's end (§6.1).
func TestAnonymousHelloOnlyWithoutCredentials(t *testing.T) {
	t.Run("no credentials", func(t *testing.T) {
		g := newTestGate(t, nil)
		end := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
		g.adm.set(func(_ int, req enroll.Request) (*enroll.Decision, error) { return anonymousDecision(req, end), nil })
		rec := newRecorder(t)
		cfg := testConfig(t, g.url, "", a2aServe("hello", rec.url()), a2aServe("private", rec.url()))
		cfg.Public = []string{"hello"}
		tn := mustNode(t, g, cfg).run(t)
		r := g.adm.next(t)
		if !r.Hello.Anonymous || !slices.Equal(r.Hello.Public, []string{"hello"}) || r.Hello.Auth != "" || len(r.Hello.Enroll) != 0 {
			t.Fatalf("hello: anonymous %v, public %v", r.Hello.Anonymous, r.Hello.Public)
		}
		g.session(t)
		tn.welcomed(t)
		out := tn.out.String()
		for _, want := range []string{
			"tenant anon-0123456789abcdef",
			"a2a hello  https://gate.example/agents/anon-0123456789abcdef/hello  (public: anyone may call it)",
			"a2a private  https://gate.example/agents/anon-0123456789abcdef/private\n",
			"anonymous session: it ends at " + end.Format(time.RFC3339),
		} {
			if !strings.Contains(out, want) {
				t.Errorf("stdout lacks %q:\n%s", want, out)
			}
		}
	})
	for _, c := range []struct {
		name  string
		setup func(t *testing.T, cfg *Config)
	}{
		{"--api-key", func(_ *testing.T, cfg *Config) { cfg.APIKey = "kk_test_x" }},
		{"--enroll", func(_ *testing.T, cfg *Config) { cfg.Enroll = []string{tokA} }},
		{"a stored API key", func(t *testing.T, cfg *Config) {
			mustUpdate(t, cfg.CredentialsPath, cfg.Gate.Scheme+"://"+cfg.Gate.Host, func(c *Credential) { c.APIKey = "kk_test_stored" })
		}},
		{"a consumed token", func(t *testing.T, cfg *Config) {
			mustUpdate(t, cfg.CredentialsPath, cfg.Gate.Scheme+"://"+cfg.Gate.Host, func(c *Credential) { c.Consumed = []string{TokenHash(tokB)} })
		}},
		{"another key's pending token", func(t *testing.T, cfg *Config) {
			mustUpdate(t, cfg.CredentialsPath, cfg.Gate.Scheme+"://"+cfg.Gate.Host, func(c *Credential) {
				c.key("k1_" + strings.Repeat("q", 26)).Enroll = []string{tokB}
			})
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := newTestGate(t, nil)
			rec := newRecorder(t)
			cfg := testConfig(t, g.url, "", a2aServe("hello", rec.url()))
			cfg.Public = []string{"hello"}
			c.setup(t, &cfg)
			tn := mustNode(t, g, cfg).run(t)
			r := g.adm.next(t)
			if r.Hello.Anonymous || r.Hello.Public != nil {
				t.Fatalf("hello: anonymous %v, public %v", r.Hello.Anonymous, r.Hello.Public)
			}
			g.session(t)
			eventually(t, 5*time.Second, "--public warned about", func() bool {
				return strings.Contains(tn.logs.String(), "--public applies to an anonymous session only")
			})
		})
	}
}

// DM4-18: the lost-welcome probe never asks for an anonymous session, even from a node that would otherwise ask (its
// tokens are pending then).
func TestProbeNeverAsksAnonymous(t *testing.T) {
	g := newTestGate(t, nil)
	g.adm.set(func(n int, req enroll.Request) (*enroll.Decision, error) {
		if n == 0 {
			return nil, gateFails // the outcome lost: the next hello is the probe
		}
		return admitAll(req), nil
	})
	rec := newRecorder(t)
	cfg := testConfig(t, g.url, "", a2aServe("hello", rec.url()))
	cfg.Enroll = []string{tokA}
	mustNode(t, g, cfg).run(t)
	first := g.adm.next(t)
	probe := g.adm.next(t)
	if first.Hello.Anonymous || probe.Hello.Anonymous || len(probe.Hello.Enroll) != 0 {
		t.Fatalf("first anonymous %v; probe anonymous %v enroll %v", first.Hello.Anonymous, probe.Hello.Anonymous, probe.Hello.Enroll)
	}
}

// §6.1, §13: the gate's {"t": "expiring"} is printed; at the end the gate sends {"t": "revoked"} and closes the
// session, and Run ends with an *AnonymousExpiredError naming the end (ErrAnonymousExpired), without a redial.
func TestAnonymousExpiryStops(t *testing.T) {
	g := newTestGate(t, nil)
	end := time.Now().Add(3 * time.Second).UTC().Truncate(time.Second).Add(time.Second)
	g.adm.set(func(_ int, req enroll.Request) (*enroll.Decision, error) { return anonymousDecision(req, end), nil })
	rec := newRecorder(t)
	tn := mustNode(t, g, testConfig(t, g.url, "", a2aServe("hello", rec.url()))).run(t)
	s := g.session(t)
	if at, ok := s.Anonymous(); !ok || !at.Equal(end) {
		t.Fatalf("the gate's session: anonymous %v until %s", ok, at)
	}
	eventually(t, 5*time.Second, "expiring printed", func() bool {
		return strings.Contains(tn.out.String(), "kontra connect: the anonymous session ends at "+end.Format(time.RFC3339))
	})
	err := tn.result(t, 15*time.Second)
	var ae *AnonymousExpiredError
	if !errors.As(err, &ae) || !errors.Is(err, ErrAnonymousExpired) || !ae.At.Equal(end) ||
		!strings.Contains(err.Error(), "the anonymous session expired at "+end.Format(time.RFC3339)) {
		t.Fatalf("Run = %v", err)
	}
	if time.Now().Before(end) {
		t.Fatal("Run ended before the session's end")
	}
	eventually(t, 5*time.Second, "the gate's session closed", s.Mux().IsClosed)
	if n := g.adm.count(); n != 1 {
		t.Fatalf("%d handshakes: the node redialed after its anonymous session ended", n)
	}
}

// §6.1: an anonymous session that closes at its end without the gate's message (lost on the way) ends Run too: a
// redial would open another session with new URLs.
func TestAnonymousSessionClosedAtItsEnd(t *testing.T) {
	g := newTestGate(t, nil)
	end := time.Now().Add(5 * time.Second).UTC() // inside expirySlack: a close now is the end
	g.adm.set(func(_ int, req enroll.Request) (*enroll.Decision, error) { return anonymousDecision(req, end), nil })
	rec := newRecorder(t)
	tn := mustNode(t, g, testConfig(t, g.url, "", a2aServe("hello", rec.url()))).run(t)
	g.session(t)
	tn.welcomed(t)
	for _, w := range g.wires.All() {
		_ = w.Cut()
	}
	if err := tn.result(t, 10*time.Second); !errors.Is(err, ErrAnonymousExpired) {
		t.Fatalf("Run = %v", err)
	}
	if n := g.adm.count(); n != 1 {
		t.Fatalf("%d handshakes", n)
	}
}

// --public names a --serve service (a usage error otherwise) and is kept once per name.
func TestParsePublic(t *testing.T) {
	home := t.TempDir()
	defaultEnv := func(k string) string {
		if k == "HOME" {
			return home
		}
		return ""
	}
	hostname = func() (string, error) { return "build-box-3", nil }
	t.Cleanup(func() { hostname = os.Hostname })
	cfg, err := parseArgs([]string{"--serve", "a2a:hello=http://127.0.0.1:9999", "--serve", "mcp:fs=http://127.0.0.1:9998/mcp",
		"--public", "hello", "--public", "fs", "--public", "hello"}, defaultEnv, nil)
	if err != nil || !slices.Equal(cfg.Public, []string{"hello", "fs"}) {
		t.Fatalf("--public: %v %v", cfg.Public, err)
	}
	_, err = parseArgs([]string{"--serve", "a2a:hello=http://127.0.0.1:9999", "--public", "other"}, defaultEnv, nil)
	var ue *UsageError
	if !errors.As(err, &ue) || !strings.Contains(err.Error(), "--public other: no --serve serves") {
		t.Fatalf("--public other: %v", err)
	}
	gate, _ := url.Parse("https://gate.example")
	if _, err := New(Config{Gate: gate, Public: []string{"x"}, Name: "n", KeyPath: "/k", CredentialsPath: "/c"}, nil); err == nil ||
		!strings.Contains(err.Error(), "--public x") {
		t.Fatalf("New with an unserved --public: %v", err)
	}
}

// §6.1, DM4-25 (M4 WP-03 review): the node tells the gate's end of its anonymous session from a revocation of its key
// whatever its own clock says. A node whose clock runs 5 minutes behind the gate's still exits with
// ErrAnonymousExpired when the gate sends {"t": "revoked"} at the session's end (welcome.anonymous.expires_at is the
// gate's time), and does not redial.
func TestAnonymousExpiryWithASkewedClock(t *testing.T) {
	g := newTestGate(t, nil)
	end := time.Now().Add(4 * time.Second).UTC().Truncate(time.Second).Add(time.Second)
	g.adm.set(func(_ int, req enroll.Request) (*enroll.Decision, error) { return anonymousDecision(req, end), nil })
	rec := newRecorder(t)
	tn := mustNode(t, g, testConfig(t, g.url, "", a2aServe("hello", rec.url())))
	tn.n.now = func() time.Time { return time.Now().Add(-5 * time.Minute) }
	tn.run(t)
	g.session(t)
	err := tn.result(t, 20*time.Second)
	var ae *AnonymousExpiredError
	if !errors.As(err, &ae) || !ae.At.Equal(end) {
		t.Fatalf("Run = %v, want the anonymous session's end", err)
	}
	if n := g.adm.count(); n != 1 {
		t.Fatalf("%d handshakes: the node redialed", n)
	}
}

// The node's estimate of an anonymous session's end, on its monotonic clock: from the welcome by its own wall clock,
// then from the gate's {"t": "expiring"}, which the gate sends expiringNotice before the end by its own clock (or at
// once when less is left), so a skew between the clocks no longer counts once it came. A close is the end when the
// estimate says so (anonymousOver); a {"t": "revoked"} also once expiring came (revokeEndsAnonymous).
func TestAnonymousEndEstimate(t *testing.T) {
	for _, c := range []struct {
		name string
		// skew is the node's clock less the gate's; opened is how long ago the welcome came, left what the gate had
		// left then; expiring how long after the welcome the gate's expiring came (-1: not yet).
		skew, opened, left, expiring time.Duration
		closed, revoked              bool // a close now is the end; a revoked now is the end
	}{
		{"in step, at the end", 0, 2 * time.Hour, 2 * time.Hour, 2*time.Hour - expiringNotice, true, true},
		{"in step, mid-session", 0, time.Hour, 2 * time.Hour, -1, false, false},
		{"behind, no expiring, at the end", -5 * time.Minute, 2 * time.Hour, 2 * time.Hour, -1, false, false},
		{"behind, at the end", -5 * time.Minute, 2 * time.Hour, 2 * time.Hour, 2*time.Hour - expiringNotice, true, true},
		{"ahead, at the end", 5 * time.Minute, 2 * time.Hour, 2 * time.Hour, 2*time.Hour - expiringNotice, true, true},
		{"ahead, 5 minutes before the end", 5 * time.Minute, 115 * time.Minute, 2 * time.Hour, 2*time.Hour - expiringNotice, false, true},
		{"ahead, no expiring, 5 minutes before the end", 5 * time.Minute, 115 * time.Minute, 2 * time.Hour, -1, true, true},
		{"behind, a quarter of an hour before", -5 * time.Minute, 105 * time.Minute, 2 * time.Hour, -1, false, false},
		{"behind, expiring at once, at the end", -5 * time.Minute, 3 * time.Minute, 3 * time.Minute, 0, false, true},
		{"in step, expiring at once, at the end", 0, 3 * time.Minute, 3 * time.Minute, 0, true, true},
		{"not anonymous", 0, 2 * time.Hour, 0, -1, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			now := time.Now()
			s := &session{opened: now.Add(-c.opened)}
			if c.left > 0 {
				// The gate's end and the node's wall clock at the welcome.
				s.setAnonymousEnd(now.Add(c.left-c.opened), now.Add(-c.opened+c.skew))
			}
			if c.expiring >= 0 {
				s.noteExpiring(c.expiring)
			}
			if got := s.anonymousOver(); got != c.closed {
				t.Errorf("a close now is the end: %v, want %v (left %s)", got, c.closed, time.Duration(s.anonLeft.Load()))
			}
			if got := s.revokeEndsAnonymous(); got != c.revoked {
				t.Errorf("a revoked now is the end: %v, want %v (left %s)", got, c.revoked, time.Duration(s.anonLeft.Load()))
			}
		})
	}
}
