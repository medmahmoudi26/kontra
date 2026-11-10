package config

import (
	"strings"
	"testing"
	"time"
)

const localMinimal = `
profile: local
postgres: postgres://kontra@localhost:5432/kontra
`

// A two-line file loads with the profile's defaults: loopback listeners, a public_url derived from listen,
// traffic on for 7 days.
func TestLocalDefaults(t *testing.T) {
	c := parse(t, localMinimal)
	if !c.Local() {
		t.Fatal("Local() = false")
	}
	for _, tc := range []struct {
		key       string
		got, want any
	}{
		{"listen", c.Listen, "127.0.0.1:8080"},
		{"metrics.listen", c.Metrics.Listen, "127.0.0.1:9090"},
		{"public_url", c.PublicURL, "http://127.0.0.1:8080"},
		{"traffic.record", c.Traffic.Recording(), true},
		{"traffic.retention", c.Traffic.Retention.D(), 7 * 24 * time.Hour},
		{"tenancy", c.Tenancy, TenancySingle},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %v, want %v", tc.key, tc.got, tc.want)
		}
	}
	// public_url follows a changed listen, IPv6 included.
	for listen, want := range map[string]string{"127.0.0.1:18080": "http://127.0.0.1:18080", "[::1]:8080": "http://[::1]:8080",
		"localhost:9000": "http://localhost:9000"} {
		if got := parse(t, localMinimal+"listen: \""+listen+"\"\n").PublicURL; got != want {
			t.Errorf("listen %s: public_url = %q, want %q", listen, got, want)
		}
	}
	// The serve overrides apply over the profile's defaults and are checked the same way.
	if c := parse(t, localMinimal, WithListen("127.0.0.1:7000")); c.Listen != "127.0.0.1:7000" || c.PublicURL != "http://127.0.0.1:7000" {
		t.Errorf("WithListen: listen %q public_url %q", c.Listen, c.PublicURL)
	}
}

// In the profile every service admits the one anonymous caller, whatever its allow: says.
func TestLocalAllowsEveryone(t *testing.T) {
	c := parse(t, localMinimal+"services:\n  hello:\n    kind: a2a\n    backend: { tunnel: {} }\n    allow: [\"key:kk_a_*\"]\n")
	if a := c.Services["hello"].Allow; len(a) != 1 || a[0] != "anonymous" {
		t.Fatalf("allow = %v, want [anonymous]", a)
	}
}

// Each refusal of the profile names its setting.
func TestLocalRefusals(t *testing.T) {
	for _, tc := range []struct{ name, extra, want string }{
		{"all interfaces", `listen: ":8080"`, `listen ":8080": the local development profile binds loopback only`},
		{"0.0.0.0", `listen: "0.0.0.0:8080"`, `listen "0.0.0.0:8080"`},
		{"::", `listen: "[::]:8080"`, `listen "[::]:8080"`},
		{"lan ip", `listen: "192.168.1.5:8080"`, `listen "192.168.1.5:8080"`},
		{"metrics", `metrics: { listen: ":9090" }`, `metrics.listen ":9090"`},
		{"peer", "peer: { listen: \"10.0.0.1:7946\", secret_env: KONTRA_PEER }", `peer.listen "10.0.0.1:7946"`},
		{"public_url remote", `public_url: http://gate.example.com`, `public_url "http://gate.example.com": must be an http:// loopback origin`},
		{"public_url https", `public_url: https://localhost:8443`, `public_url "https://localhost:8443": must be an http:// loopback origin`},
		{"tls", "tls: { terminated_by_proxy: true }", "tls: the local development profile serves plain HTTP"},
		{"sealed", "services:\n  hello:\n    kind: a2a\n    backend: { tunnel: {} }\n    sealed: true",
			"services.hello.sealed: sealed services are not served in the local development profile"},
		{"multi", "tenancy: multi", `tenancy "multi": the local development profile is single-tenant`},
		{"unknown profile", "", `profile "team": want local`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			y := localMinimal + tc.extra + "\n"
			if tc.name == "unknown profile" {
				y = strings.Replace(localMinimal, "profile: local", "profile: team\npublic_url: http://localhost:8080", 1)
			}
			err := parseErr(t, y)
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

// Without profile: nothing changes: listen keeps its all-interface default, traffic is off.
func TestNoProfileUnchanged(t *testing.T) {
	c := parse(t, minimal)
	if c.Local() || c.Listen != ":8080" || c.Metrics.Listen != ":9090" || c.Traffic.Recording() || c.PublicURL != "http://localhost:8080" {
		t.Fatalf("profile %q listen %q metrics %q traffic %v public_url %q", c.Profile, c.Listen, c.Metrics.Listen,
			c.Traffic.Recording(), c.PublicURL)
	}
	if c.Traffic.Retention.D() != 7*24*time.Hour {
		t.Errorf("traffic.retention = %s", c.Traffic.Retention)
	}
}

// traffic: record and retention, each default and each explicit value, in and out of the profile.
func TestTrafficConfig(t *testing.T) {
	if c := parse(t, minimal+"traffic: { record: true, retention: 36h }\n"); !c.Traffic.Recording() || c.Traffic.Retention.D() != 36*time.Hour {
		t.Errorf("explicit: %+v", c.Traffic)
	}
	if c := parse(t, localMinimal+"traffic: { record: false }\n"); c.Traffic.Recording() {
		t.Error("record: false in the profile is not honoured")
	}
	for _, y := range []string{"traffic: { retention: 0 }", `traffic: { retention: "0" }`} {
		if c := parse(t, localMinimal+y+"\n"); !c.Traffic.Retention.IsForever() || c.Traffic.Retention.D() != 0 {
			t.Errorf("%s: retention %s, want forever", y, c.Traffic.Retention)
		}
	}
	for _, y := range []string{"traffic: { retention: 0s }", "traffic: { retention: 1500ms }", "traffic: { retention: -1h }"} {
		if err := parseErr(t, localMinimal+y+"\n"); !strings.Contains(err.Error(), "retention") {
			t.Errorf("%s: %v", y, err)
		}
	}
}

// A service's task retention: absent is 7 days, 0 is forever (distinct from absent), a duration is itself.
func TestTaskRetentionZero(t *testing.T) {
	svc := func(r string) Service {
		y := minimal
		if r != "" {
			y += "    task: { retention: " + r + " }\n"
		}
		return parse(t, y).Services["hello"]
	}
	if s := svc(""); s.Task.Retention.IsForever() || s.Task.Retention.D() != DefaultTaskRetention {
		t.Errorf("absent: %s", s.Task.Retention)
	}
	if s := svc("0"); !s.Task.Retention.IsForever() || s.Task.Retention.D() != 0 {
		t.Errorf("0: %s", s.Task.Retention)
	}
	if s := svc("2d"); s.Task.Retention.D() != 48*time.Hour {
		t.Errorf("2d: %s", s.Task.Retention)
	}
	if err := parseErr(t, minimal+"    task: { retention: 0s }\n"); !strings.Contains(err.Error(), "retention") {
		t.Errorf("0s: %v", err)
	}
}
