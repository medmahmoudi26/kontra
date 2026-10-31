package config

import (
	"encoding/json"
	"errors"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

// minimal is the smallest valid file: a localhost gate with one direct a2a service.
const minimal = `
public_url: http://localhost:8080
postgres: postgres://kontra@localhost:5432/kontra
services:
  hello:
    kind: a2a
    backend: { a2a: { url: "https://hello.example.com" } }
`

// tunnelled is minimal with a tunnel backend, which this build serves (M2).
const tunnelled = `
public_url: http://localhost:8080
postgres: postgres://kontra@localhost:5432/kontra
services:
  hello:
    kind: a2a
    backend: { tunnel: {} }
`

// mcpDirect is minimal with a direct MCP service: an MCP service has no card, so seller_keys on it verify nothing
// and load with a note.
const mcpDirect = `
public_url: http://localhost:8080
postgres: postgres://kontra@localhost:5432/kontra
services:
  hello:
    kind: mcp
    backend: { mcp: { url: "https://mcp.example.com/mcp" } }
`

// decodeSealed decodes one sealed: value as Parse does, without validating the file: sealing is refused
// on every backend until M3, so no configuration that loads carries it.
func decodeSealed(t *testing.T, y string) (Sealed, error) {
	t.Helper()
	var v struct {
		Sealed Sealed `yaml:"sealed"`
	}
	dec := yaml.NewDecoder(strings.NewReader("sealed: " + y + "\n"))
	dec.KnownFields(true)
	err := dec.Decode(&v)
	return v.Sealed, err
}

func parse(t *testing.T, y string, opts ...Option) *Config {
	t.Helper()
	c, err := Parse([]byte(y), "", opts...)
	if err != nil {
		t.Fatalf("Parse: %v\n%s", err, y)
	}
	return c
}

func parseErr(t *testing.T, y string) error {
	t.Helper()
	_, err := Parse([]byte(y), "")
	if err == nil {
		t.Fatalf("Parse succeeded, want an error\n%s", y)
	}
	return err
}

// Every default of relay §17 and PRD §11 is applied to a file that sets none of them.
func TestDefaultsApplied(t *testing.T) {
	c := parse(t, minimal)
	s := c.Services["hello"]
	checks := []struct {
		key       string
		got, want any
	}{
		{"listen", c.Listen, ":8080"},
		{"metrics.listen", c.Metrics.Listen, ":9090"},
		{"limits.body", c.Limits.Body.Int64(), int64(4 << 20)},
		{"limits.inflight_per_key", c.Limits.InflightPerKey, 32},
		{"limits.rps_per_ip", c.Limits.RPSPerIP, 50.0},
		{"limits.header_timeout", c.Limits.HeaderTimeout.D(), 10 * time.Second},
		{"limits.idle", c.Limits.Idle.D(), 60 * time.Second},
		{"limits.conns_per_ip", c.Limits.ConnsPerIP, 256},
		{"limits.max_conns", c.Limits.MaxConns, 10000},
		{"limits.dispatch_slots", c.Limits.DispatchSlots, 10000},
		{"limits.push_slots", c.Limits.PushSlots, 2000},
		{"limits.task_bytes", c.Limits.TaskBytes.Int64(), int64(256 << 20)},
		{"temporal.address", c.Temporal.Address, "localhost:7233"},
		{"temporal.namespace", c.Temporal.Namespace, "kontra"},
		{"tenancy", c.Tenancy, TenancySingle},
		{"caller_metadata", c.CallerMetadata, []string{}},
		{"operator_metadata_prefixes", c.OperatorMetadataPrefixes, []string{}},
		{"push.allow_private", c.Push.AllowPrivate, false},
		{"blobs", c.Blobs, ""},
		{"mcp.allowed_origins", c.MCP.AllowedOrigins, []string{}},
		{"auth.mcp_oauth", c.Auth.MCPOAuth, (*MCPOAuth)(nil)},
		{"hooks.admission", c.Hooks.Admission, (*AdmissionHook)(nil)},
		{"hooks.lifecycle", c.Hooks.Lifecycle, (*LifecycleHook)(nil)},
		{"drain_timeout", c.DrainTimeout.D(), 10 * time.Minute},
		{"tunnel.channel_binding (no TLS)", c.Tunnel.ChannelBinding, "none"},
		{"tunnel.anonymous", c.Tunnel.Anonymous, AnonymousSessions{}},
		{"tunnel.enroll_with_api_key", c.Tunnel.EnrollWithAPIKey, false},
		{"edge.client_ip.from", c.Edge.ClientIP.From, ClientIPRemote},
		{"services.hello.allow", s.Allow, []string{}},
		{"services.hello.tools", s.Tools, (*Tools)(nil)},
		{"services.hello.tasks (a2a)", s.Tasks, ""},
		{"services.hello.sealed", s.Sealed.Mode, SealedOff},
		{"services.hello.seller_keys", s.SellerKeys.IsZero(), true},
		{"services.hello.session_idle_s", s.SessionIdle.D(), 3600 * time.Second},
		{"services.hello.max_sessions_per_principal", s.SessionsPerPrincipal(), 4},
		{"services.hello.task.timeout", s.Task.Timeout.D(), time.Hour},
		{"services.hello.task.retries", s.Task.RetryCount(), 2},
		{"services.hello.task.retention", s.Task.Retention.D(), 7 * 24 * time.Hour},
		{"services.hello.listed", s.Listed, false},
		{"services.hello.annotations", string(s.Annotations), "{}"},
	}
	for _, ch := range checks {
		if !reflect.DeepEqual(ch.got, ch.want) {
			t.Errorf("%s = %#v, want %#v", ch.key, ch.got, ch.want)
		}
	}
	if len(c.Warnings) != 0 {
		t.Errorf("warnings = %q, want none", c.Warnings)
	}

	// Kind-dependent and TLS-dependent defaults.
	m := parse(t, minimal+`
  fs:
    kind: mcp
    backend: { tunnel: {} }
`)
	if got := m.Services["fs"].Tasks; got != TasksDurable {
		t.Errorf("mcp tasks default = %q, want durable", got)
	}
	tls := parse(t, `
public_url: https://localhost:8443
postgres: postgres://kontra@localhost/kontra
tls: { cert: c.pem, key: k.pem }
`)
	if tls.Tunnel.ChannelBinding != ChannelBindingTLSExporter {
		t.Errorf("channel_binding with a gate-terminated TLS = %q, want tls-exporter", tls.Tunnel.ChannelBinding)
	}
	adm := Defaults()
	adm.Hooks.Admission = &AdmissionHook{}
	adm.applyDerivedDefaults()
	if adm.Hooks.Admission.Timeout.D() != 5*time.Second || !slices.Equal(adm.Hooks.Admission.Operations, []string{"task"}) {
		t.Errorf("hooks.admission defaults = %+v, want timeout 5s and operations [task]", *adm.Hooks.Admission)
	}
}

// A key present in the file wins over its default, including an explicit retries: 0.
func TestExplicitValuesWin(t *testing.T) {
	c := parse(t, `
listen: 127.0.0.1:9000
public_url: http://127.0.0.1:9000
postgres: postgres://kontra@localhost/kontra
limits: { body: 1MiB, inflight_per_key: 4, rps_per_ip: 2.5, header_timeout: 3s, idle: 2m, conns_per_ip: 8, max_conns: 64, dispatch_slots: 5, push_slots: 6, task_bytes: 512KiB }
temporal: { address: "temporal:7233", namespace: other }
metrics: { listen: "127.0.0.1:9191" }
tenancy: multi
caller_metadata: [kontra.escrow.dispute]
operator_metadata_prefixes: [kontra.escrow.]
push: { allow_private: true }
drain_timeout: 90s
services:
  hello:
    kind: a2a
    backend: { a2a: { url: "http://127.0.0.1:9999", allow_private: true } }
    allow: [org:default]
    session_idle_s: 60
    listed: true
    task: { timeout: 10m, retries: 0, retention: 1d }
`)
	s := c.Services["hello"]
	if c.Limits.Body != 1<<20 || c.Limits.TaskBytes != 512<<10 || c.Limits.RPSPerIP != 2.5 || c.Limits.Idle.D() != 2*time.Minute ||
		c.Limits.ConnsPerIP != 8 || c.Limits.MaxConns != 64 || c.Limits.DispatchSlots != 5 || c.Limits.PushSlots != 6 {
		t.Errorf("limits = %+v", c.Limits)
	}
	if c.Tenancy != TenancyMulti || c.DrainTimeout.D() != 90*time.Second || !c.Push.AllowPrivate || c.Temporal.Namespace != "other" {
		t.Errorf("top level = tenancy %q drain %s push %v ns %q", c.Tenancy, c.DrainTimeout, c.Push.AllowPrivate, c.Temporal.Namespace)
	}
	if s.Task.RetryCount() != 0 || s.Task.Timeout.D() != 10*time.Minute || s.Task.Retention.D() != 24*time.Hour ||
		s.SessionIdle != 60 || !s.Listed {
		t.Errorf("service = %+v", s)
	}
}

// `sealed: true`, `false` and `{required: false}` load as required, off and optional (ext-sealed.md §1).
func TestSealedShapes(t *testing.T) {
	for _, tc := range []struct {
		yaml string
		want SealedMode
	}{
		{"true", SealedRequired},
		{"false", SealedOff},
		{"{required: false}", SealedOptional},
		{"{required: true}", SealedRequired},
		{"{}", SealedRequired},
	} {
		got, err := decodeSealed(t, tc.yaml)
		if err != nil || got.Mode != tc.want {
			t.Errorf("sealed: %s decodes as %q (%v), want %q", tc.yaml, got.Mode, err, tc.want)
		}
	}
	for _, bad := range []string{`"yes"`, "1", "{require: true}", "{required: maybe}", "[true]"} {
		if _, err := decodeSealed(t, bad); err == nil {
			t.Errorf("sealed: %s decoded", bad)
		}
		err := parseErr(t, tunnelled+"    sealed: "+bad+"\n")
		if !strings.Contains(err.Error(), "sealed") {
			t.Errorf("sealed: %s: error %q does not name sealed", bad, err)
		}
	}
	// sealed: false is the default, on any backend.
	if c := parse(t, tunnelled+"    sealed: false\n"); c.Services["hello"].Sealed.Mode != SealedOff {
		t.Errorf("sealed: false on a tunnel service loads as %q", c.Services["hello"].Sealed.Mode)
	}
}

// sealed: on a direct backend is refused (D91: no node unseals for it, so it would fail open). Sealing is built
// since M3 WP-08, which lifts D144's refusal on tunnel services: sealed: true and {required: false} load on tunnel
// a2a and mcp services, with seller_keys or without, and with no warning. Seller card signatures are verified (M3
// WP-02 lifts D90 and the seller_keys half of D144): seller_keys loads on direct and tunnel a2a services, with no
// warning, and on MCP services with a note (no card to verify).
func TestFailOpenKeysRefused(t *testing.T) {
	for _, tc := range []struct{ name, yaml, want string }{
		{"sealed on a direct a2a backend", minimal + "    sealed: true\n", "services.hello.sealed: sealing needs backend tunnel"},
		{"optional sealing on a direct a2a backend", minimal + "    sealed: { required: false }\n", "sealing needs backend tunnel"},
		{"sealed on a direct mcp backend", minimal + "  fs:\n    kind: mcp\n    backend: { mcp: { url: \"https://mcp.example.com\" } }\n    sealed: true\n", "services.fs.sealed: sealing needs backend tunnel"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := parseErr(t, tc.yaml); !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q\nwant it to contain %q", err, tc.want)
			}
		})
	}
	for _, tc := range []struct {
		name, yaml, svc string
		want            SealedMode
	}{
		{"sealed on a tunnel service", tunnelled + "    sealed: true\n", "hello", SealedRequired},
		{"optional sealing on a tunnel service", tunnelled + "    sealed: { required: false }\n", "hello", SealedOptional},
		{"sealed on a tunnel mcp service", minimal + "  fs:\n    kind: mcp\n    backend: { tunnel: {} }\n    sealed: true\n", "fs", SealedRequired},
		{"sealed with seller_keys on a tunnel service", tunnelled + "    sealed: true\n    seller_keys: { url: \"https://seller.example.com/jwks.json\" }\n", "hello", SealedRequired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := parse(t, tc.yaml)
			if got := c.Services[tc.svc].Sealed.Mode; got != tc.want {
				t.Errorf("sealed loads as %q, want %q", got, tc.want)
			}
			for _, w := range c.Warnings {
				if strings.Contains(w, "sealed") {
					t.Errorf("a warning about sealed: %q", w)
				}
			}
		})
	}
	const inline = "    seller_keys: { keys: [ { kty: EC, crv: P-256, x: \"f83OJ3D2xF1Bg8vub9tLe1gHMzV76e8Tus9uPHvRVEU\", y: \"x_FEzRu9m36HLN_tue659LNpXW6pCyStikYjKIWI5a0\" } ] }\n"
	for _, tc := range []struct{ name, yaml, svc string }{
		{"seller_keys {url} on a direct a2a backend", minimal + "    seller_keys: { url: \"https://seller.example.com/jwks.json\" }\n", "hello"},
		{"inline seller_keys on a direct a2a backend", minimal + inline, "hello"},
		{"seller_keys {url} on a tunnel service", tunnelled + "    seller_keys: { url: \"https://seller.example.com/jwks.json\" }\n", "hello"},
		{"inline seller_keys on a tunnel service", tunnelled + inline, "hello"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := parse(t, tc.yaml)
			if c.Services[tc.svc].SellerKeys.IsZero() || len(c.Warnings) != 0 {
				t.Errorf("seller_keys %s, warnings %q", c.Services[tc.svc].SellerKeys, c.Warnings)
			}
		})
	}
	c := parse(t, minimal+"  fs:\n    kind: mcp\n    backend: { tunnel: {} }\n    seller_keys: { url: \"https://seller.example.com/jwks.json\" }\n")
	if !slices.ContainsFunc(c.Warnings, func(w string) bool {
		return strings.HasPrefix(w, "services.fs.seller_keys: an MCP service has no card")
	}) {
		t.Errorf("seller_keys on a tunnel mcp service: warnings %q", c.Warnings)
	}
	// An inline set the gate cannot verify with is refused at load, not left to fail every fetch.
	if err := parseErr(t, minimal+"    seller_keys: { keys: [ { kty: RSA, n: abc, e: AQAB } ] }\n"); !strings.Contains(err.Error(), "no EC P-256 signing key") {
		t.Errorf("an RSA-only set: %v", err)
	}
	if err := parseErr(t, minimal+"    seller_keys: { keys: [ { kty: EC, crv: P-256, x: AAAA, y: AAAA } ] }\n"); !strings.Contains(err.Error(), "services.hello.seller_keys") {
		t.Errorf("a malformed key: %v", err)
	}
}

// signing_key (spec §3, D166): file:<path> only; required when public_url is not a loopback origin and the
// configuration serves A2A services, naming D166 and the services; not required on loopback (the gate mints a key
// at start) or for a gate without A2A services.
func TestSigningKey(t *testing.T) {
	const public = "public_url: https://agents.acme.io\npostgres: postgres://k@db/k\ntls: { terminated_by_proxy: true }\n"
	const a2aSvc = "services:\n  hello:\n    kind: a2a\n    backend: { tunnel: {} }\n  book:\n    kind: a2a\n    backend: { a2a: { url: \"https://seller.example.com\" } }\n"
	const mcpSvc = "services:\n  fs:\n    kind: mcp\n    backend: { tunnel: {} }\n"
	err := parseErr(t, public+a2aSvc)
	var ve *ValidationError
	if !errors.As(err, &ve) || len(ve.Problems) != 1 || !strings.Contains(ve.Problems[0], "signing_key: required (D166)") ||
		!strings.Contains(ve.Problems[0], "(book, hello)") {
		t.Fatalf("a public gate with A2A services and no signing_key: %v", err)
	}
	c := parse(t, public+"signing_key: file:/etc/kontra/signing/kontra.key\n"+a2aSvc)
	if c.SigningKey != "file:/etc/kontra/signing/kontra.key" {
		t.Errorf("signing_key %q", c.SigningKey)
	}
	parse(t, public+mcpSvc) // no A2A service: no card to sign
	parse(t, public)        // no service at all (Cloud: the operator writes rows)
	parse(t, minimal)       // loopback: a key is minted at start
	parse(t, strings.Replace(minimal, "http://localhost:8080", "http://127.0.0.1:8080", 1))
	for _, bad := range []string{"signing_key: kms:scaleway/key-1\n", "signing_key: \"file:\"\n", "signing_key: ./kontra.key\n"} {
		if err := parseErr(t, minimal+bad); !strings.Contains(err.Error(), "signing_key: want file:<path>") {
			t.Errorf("%s: %v", strings.TrimSpace(bad), err)
		}
	}
}

func TestParseDuration(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"7d": 7 * 24 * time.Hour, "1d12h": 36 * time.Hour, "1.5d": 36 * time.Hour, "90s": 90 * time.Second,
		"1h30m": 90 * time.Minute, "10m": 10 * time.Minute, "250ms": 250 * time.Millisecond,
	} {
		got, err := ParseDuration(in)
		if err != nil || got != want {
			t.Errorf("ParseDuration(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "7", "0s", "0d", "-1h", "d", "7w", "1e3d", "99999999999d", "1h-1m"} {
		if got, err := ParseDuration(in); err == nil {
			t.Errorf("ParseDuration(%q) = %v, want an error", in, got)
		}
	}
}

func TestParseByteSize(t *testing.T) {
	for in, want := range map[string]int64{
		"4MiB": 4 << 20, "256MiB": 256 << 20, "512KiB": 512 << 10, "1GiB": 1 << 30, "2TiB": 2 << 40,
		"4194304": 4194304, "10B": 10, "4 MiB": 4 << 20,
	} {
		got, err := ParseByteSize(in)
		if err != nil || got != want {
			t.Errorf("ParseByteSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "0", "0MiB", "4MB", "4mib", "1.5MiB", "-1", "MiB", "99999999999TiB"} {
		if got, err := ParseByteSize(in); err == nil {
			t.Errorf("ParseByteSize(%q) = %d, want an error", in, got)
		}
	}
	// Integers in YAML decode as bytes, too.
	c := parse(t, minimal+"limits: { body: 1048576 }\n")
	if c.Limits.Body != 1<<20 {
		t.Errorf("body: 1048576 = %d", c.Limits.Body)
	}
}

// Each of these is a load error, with a message naming what is wrong.
func TestLoadErrors(t *testing.T) {
	const head = "postgres: postgres://kontra@localhost/kontra\npublic_url: http://localhost:8080\n"
	svc := func(body string) string { return head + "services:\n  s:\n" + body }
	for _, tc := range []struct {
		name, yaml, want string
	}{
		{"zero backends", svc("    kind: a2a\n    backend: {}\n"), "exactly one of tunnel, a2a {url} or mcp {url}, found 0"},
		{"no backend key", svc("    kind: a2a\n"), "found 0"},
		{"two backends", svc("    kind: a2a\n    backend: { tunnel: {}, a2a: { url: https://x.example.com } }\n"), "found 2"},
		{"bad service name", head + "services:\n  Bad_Name:\n    kind: a2a\n    backend: { tunnel: {} }\n", "services.Bad_Name: the name must match"},
		{"name too long", head + "services:\n  " + strings.Repeat("a", 64) + ":\n    kind: a2a\n    backend: { tunnel: {} }\n", "the name must match"},
		{"unknown allow kind", svc("    kind: a2a\n    backend: { tunnel: {} }\n    allow: [\"user:bob\"]\n"), `unknown allow: kind "user"`},
		{"allow without kind", svc("    kind: a2a\n    backend: { tunnel: {} }\n    allow: [everyone]\n"), "want key:, org:, oidc:sub:, oidc:*, node: or anonymous"},
		{"PRD glob key:acme-* (D68)", svc("    kind: a2a\n    backend: { tunnel: {} }\n    allow: [\"key:acme-*\"]\n"), "write e.g. key:kk_acme_*, D68"},
		{"short exact key prefix", svc("    kind: a2a\n    backend: { tunnel: {} }\n    allow: [\"key:kk\"]\n"), "4 to 32 characters"},
		{"bad org", svc("    kind: a2a\n    backend: { tunnel: {} }\n    allow: [\"org:Acme\"]\n"), "org: needs a tenant name"},
		{"bad node key", svc("    kind: a2a\n    backend: { tunnel: {} }\n    allow: [\"node:k1_x\"]\n"), "node: needs a key id"},
		{"empty oidc subject", svc("    kind: a2a\n    backend: { tunnel: {} }\n    allow: [\"oidc:sub:\"]\n"), "needs a subject"},
		{"kind missing", svc("    backend: { tunnel: {} }\n"), "services.s.kind: required"},
		{"kind unknown", svc("    kind: grpc\n    backend: { tunnel: {} }\n"), `kind "grpc"`},
		{"a2a with mcp backend", svc("    kind: a2a\n    backend: { mcp: { url: https://x.example.com } }\n"), "an a2a service takes backend tunnel or a2a"},
		{"mcp with a2a backend", svc("    kind: mcp\n    backend: { a2a: { url: https://x.example.com } }\n"), "an mcp service takes backend tunnel or mcp"},
		{"tools on a2a", svc("    kind: a2a\n    backend: { tunnel: {} }\n    tools: { allow: [x] }\n"), "tools: mcp services only"},
		{"tasks on a2a", svc("    kind: a2a\n    backend: { tunnel: {} }\n    tasks: durable\n"), "tasks: mcp services only"},
		{"bad tasks mode", svc("    kind: mcp\n    backend: { tunnel: {} }\n    tasks: sometimes\n"), `tasks "sometimes"`},
		{"http backend without allow_private (D67)", svc("    kind: a2a\n    backend: { a2a: { url: http://10.0.0.5:9000 } }\n"), "http:// is refused unless allow_private: true (D67)"},
		{"backend with credentials", svc("    kind: a2a\n    backend: { a2a: { url: https://u:p@x.example.com } }\n"), "credentials do not belong"},
		{"backend with query", svc("    kind: a2a\n    backend: { a2a: { url: \"https://x.example.com/?a=1\" } }\n"), "no query or fragment"},
		{"backend ftp", svc("    kind: a2a\n    backend: { a2a: { url: ftp://x.example.com } }\n"), "want https://"},
		{"backend no url", svc("    kind: a2a\n    backend: { a2a: { allow_private: true } }\n"), "url: required"},
		{"backend port out of range", svc("    kind: a2a\n    backend: { a2a: { url: \"https://x.example.com:99999\" } }\n"), "port \"99999\" is not a number from 1 to 65535"},
		{"backend port 0", svc("    kind: a2a\n    backend: { a2a: { url: \"https://x.example.com:0/a2a\" } }\n"), "is not a number from 1 to 65535"},
		{"issuer port out of range", head + "auth: { oidc: { issuer: \"https://login.example.com:70000\", audience: kontra } }\n", "auth.oidc.issuer"},
		{"public_url port out of range", "postgres: postgres://k@db/k\npublic_url: http://localhost:65536\n", "public_url"},
		{"timeout over int4 seconds", svc("    kind: a2a\n    backend: { tunnel: {} }\n    task: { timeout: 30000d }\n"), "services.s.task.timeout 720000h0m0s: at most 2147483647 seconds"},
		{"retention over int4 seconds", svc("    kind: a2a\n    backend: { tunnel: {} }\n    task: { retention: 24856d }\n"), "services.s.task.retention"},
		{"session idle over int4 seconds", svc("    kind: a2a\n    backend: { tunnel: {} }\n    session_idle_s: 3000000000\n"), "services.s.session_idle_s 3000000000: at most 2147483647"},
		{"retries over 10", svc("    kind: a2a\n    backend: { tunnel: {} }\n    task: { retries: 11 }\n"), "want 0 to 10"},
		{"negative retries", svc("    kind: a2a\n    backend: { tunnel: {} }\n    task: { retries: -1 }\n"), "want 0 to 10"},
		{"sub-second timeout", svc("    kind: a2a\n    backend: { tunnel: {} }\n    task: { timeout: 1500ms }\n"), "whole number of seconds"},
		{"zero timeout", svc("    kind: a2a\n    backend: { tunnel: {} }\n    task: { timeout: 0s }\n"), "must be positive"},
		{"zero session idle", svc("    kind: a2a\n    backend: { tunnel: {} }\n    session_idle_s: 0\n"), "positive whole number of seconds"},
		{"sessions per principal on a2a", svc("    kind: a2a\n    backend: { tunnel: {} }\n    max_sessions_per_principal: 3\n"), "max_sessions_per_principal: mcp services only"},
		{"zero sessions per principal", svc("    kind: mcp\n    backend: { tunnel: {} }\n    max_sessions_per_principal: 0\n"), "max_sessions_per_principal 0: want 1 to 10000"},
		{"too many sessions per principal", svc("    kind: mcp\n    backend: { tunnel: {} }\n    max_sessions_per_principal: 10001\n"), "max_sessions_per_principal 10001: want 1 to 10000"},
		{"seller_keys both", svc("    kind: a2a\n    backend: { tunnel: {} }\n    seller_keys: { url: https://x.example.com/jwks, keys: [{kty: EC}] }\n"), "keys and url are exclusive"},
		{"seller_keys http url", svc("    kind: a2a\n    backend: { tunnel: {} }\n    seller_keys: { url: http://x.example.com/jwks }\n"), "want https://"},
		{"annotations not a map", svc("    kind: a2a\n    backend: { tunnel: {} }\n    annotations: [1]\n"), "annotations: want a mapping"},
		{"unknown service key", svc("    kind: a2a\n    backend: { tunnel: {} }\n    alow: [org:default]\n"), "field alow not found"},
		{"unknown top-level key", head + "listn: :1\n", "field listn not found"},
		{"agents: from the PRD", head + "agents: {}\n", "agents: is now services:"},
		{"duplicate service", head + "services:\n  s: { kind: a2a, backend: { tunnel: {} } }\n  s: { kind: a2a, backend: { tunnel: {} } }\n", "already defined"},
		{"two documents", minimal + "---\nlisten: :1\n", "want one YAML document"},
		{"empty", "", "empty configuration"},

		{"plaintext with a non-localhost public_url", "postgres: postgres://k@db/k\npublic_url: http://agents.acme.io\n", "not a localhost origin, so plaintext is refused"},
		{"https public_url but no TLS", "postgres: postgres://k@db/k\npublic_url: https://agents.acme.io\n", "not a localhost origin"},
		{"https localhost but no TLS", "postgres: postgres://k@db/k\npublic_url: https://localhost:8443\n", "no TLS is configured"},
		{"0.0.0.0 is not localhost", "postgres: postgres://k@db/k\npublic_url: http://0.0.0.0:8080\n", "not a localhost origin"},
		{"TLS with an http public_url", "postgres: postgres://k@db/k\npublic_url: http://agents.acme.io\ntls: { cert: c.pem, key: k.pem }\n", "must be https://"},
		{"proxy with an http public_url (D69)", "postgres: postgres://k@db/k\npublic_url: http://agents.acme.io\ntls: { terminated_by_proxy: true }\n", "must be https://"},
		{"cert without key", "postgres: postgres://k@db/k\npublic_url: https://agents.acme.io\ntls: { cert: c.pem }\n", "cert and key go together"},
		{"acme and cert", "postgres: postgres://k@db/k\npublic_url: https://agents.acme.io\ntls: { acme: {cache_dir: /tmp/a}, cert: c.pem, key: k.pem }\n", "are exclusive"},
		{"acme and proxy", "postgres: postgres://k@db/k\npublic_url: https://agents.acme.io\ntls: { acme: {cache_dir: /tmp/a}, terminated_by_proxy: true }\n", "are exclusive"},
		{"acme on localhost", "postgres: postgres://k@db/k\npublic_url: https://localhost\ntls: { acme: {cache_dir: /tmp/a} }\n", "not a DNS name a CA can validate"},
		{"acme on an IP", "postgres: postgres://k@db/k\npublic_url: https://203.0.113.7\ntls: { acme: {cache_dir: /tmp/a} }\n", "not a DNS name"},
		{"acme domains miss the public host", "postgres: postgres://k@db/k\npublic_url: https://agents.acme.io\ntls: { acme: {cache_dir: /tmp/a, domains: [other.acme.io]} }\n", "must include the public_url host"},
		{"public_url with a query", "postgres: postgres://k@db/k\npublic_url: http://localhost:8080/?x=1\n", "no credentials, query or fragment"},
		{"public_url relative", "postgres: postgres://k@db/k\npublic_url: /agents\n", "want an http:// or https:// URL"},
		{"no public_url", "postgres: postgres://k@db/k\n", "public_url: required"},
		{"no postgres", "public_url: http://localhost:8080\n", "postgres: required"},
		{"postgres not a URL", "public_url: http://localhost:8080\npostgres: host=db dbname=k\n", "want a postgres:// URL"},
		{"postgres bad port", "public_url: http://localhost:8080\npostgres: postgres://k@db:99999/k\n", "postgres:"},
		{"postgres bad pool size", "public_url: http://localhost:8080\npostgres: \"postgres://k@db/k?pool_max_conns=many\"\n", "pool_max_conns"},
		{"postgres misspelt pool parameter", "public_url: http://localhost:8080\npostgres: \"postgres://k@db/k?pool_max_conn=3\"\n", "unknown pool parameter \"pool_max_conn\""},
		{"blobs not s3", head + "blobs: https://bucket.example.com\n", "want s3://"},
		{"blobs with an unknown parameter", head + "blobs: \"s3://parts?bucket=x\"\n", "unknown parameter \"bucket\""},
		{"blobs with an endpoint path", head + "blobs: \"s3://parts?endpoint=https://s3.example.com/x\"\n", "endpoint"},
		{"blobs with a bad bucket name", head + "blobs: s3://Parts\n", "not an S3 bucket name"},
		{"listen without port", head + "listen: localhost\n", "listen \"localhost\""},
		{"metrics on the public listener", head + "listen: :8080\nmetrics: { listen: \":8080\" }\n", "must differ from listen"},
		{"metrics on the public port, wildcard spelt out", head + "listen: :8080\nmetrics: { listen: \"0.0.0.0:8080\" }\n", "must differ from listen"},
		{"metrics on the wildcard's port", head + "listen: \"[::]:8080\"\nmetrics: { listen: \"127.0.0.1:8080\" }\n", "must differ from listen"},
		{"metrics on localhost's port", head + "listen: \"localhost:9000\"\nmetrics: { listen: \"127.0.0.1:9000\" }\n", "must differ from listen"},
		{"metrics on the same IP, spelt differently", head + "listen: \"[::ffff:127.0.0.1]:9000\"\nmetrics: { listen: \"127.0.0.1:9000\" }\n", "must differ from listen"},
		{"zero inflight", head + "limits: { inflight_per_key: 0 }\n", "limits.inflight_per_key: must be positive"},
		{"zero conns_per_ip (D73)", head + "limits: { conns_per_ip: 0 }\n", "limits.conns_per_ip: must be positive"},
		{"negative max_conns (D73)", head + "limits: { max_conns: -5 }\n", "limits.max_conns: must be positive"},
		{"zero rps", head + "limits: { rps_per_ip: 0 }\n", "rps_per_ip: must be positive"},
		{"zero body", head + "limits: { body: 0 }\n", "must be positive"},
		{"decimal size", head + "limits: { body: 4MB }\n", "binary units only"},
		{"bare duration", head + "drain_timeout: 600\n", "missing unit"},
		{"bad tenancy", head + "tenancy: shared\n", "want single or multi"},
		{"caller_metadata outside kontra.", head + "caller_metadata: [acme.x]\n", "is not a kontra. key"},
		// A service could write keys under a prefix outside kontra. (§2.5 strips only kontra. keys), which
		// TaskRef.metadata would hand the hooks as the operator's (§13.3).
		{"operator prefix outside kontra.", head + "operator_metadata_prefixes: [acme.]\n", "is not under kontra."},
		{"operator prefix kontra. alone", head + "operator_metadata_prefixes: [kontra.]\n", "is not under kontra."},
		{"bad origin", head + "mcp: { allowed_origins: [\"https://a.example.com/path\"] }\n", "is not an origin"},
		{"xff without trusted (D69)", head + "edge: { client_ip: { from: x_forwarded_for } }\n", "needs edge.client_ip.trusted"},
		{"proxy_protocol without trusted (D69)", head + "edge: { client_ip: { from: proxy_protocol } }\n", "needs edge.client_ip.trusted"},
		{"trusted with remote", head + "edge: { client_ip: { trusted: [10.0.0.0/8] } }\n", "only used with from: proxy_protocol or x_forwarded_for"},
		{"bad trusted CIDR", head + "edge: { client_ip: { from: x_forwarded_for, trusted: [10.0.0.0/33] } }\n", "is not a CIDR"},
		{"bad client_ip source", head + "edge: { client_ip: { from: header } }\n", "want remote, proxy_protocol or x_forwarded_for"},
		{"tls-exporter without TLS", head + "tunnel: { channel_binding: tls-exporter }\n", "needs the gate to terminate TLS"},
		{"admission without secret_env", head + "hooks: { admission: { url: https://cp.example.com/admit } }\n", "hooks.admission.secret_env"},
		{"admission secret_env not a variable name", head + "hooks: { admission: { url: https://cp.example.com/admit, secret_env: \"admit-secret\" } }\n", "hooks.admission.secret_env"},
		{"admission with an unknown operation", head + "hooks: { admission: { url: https://cp.example.com/admit, secret_env: ADMIT, operations: [settle] } }\n", "hooks.admission.operations"},
		{"admission without a url", head + "hooks: { admission: { secret_env: ADMIT } }\n", "hooks.admission.url"},
		{"admission with a zero timeout", head + "hooks: { admission: { url: https://cp.example.com/admit, secret_env: ADMIT, timeout: 0s } }\n", "must be positive"},
		// The MCP relay cuts a modern client's shared session decision at 30 s (spec §13.1, D284).
		{"admission with a timeout over 30s", head + "hooks: { admission: { url: https://cp.example.com/admit, secret_env: ADMIT, timeout: 31s } }\n", "hooks.admission.timeout 31s: at most 30s"},
		{"admission with a timeout of days", head + "hooks: { admission: { url: https://cp.example.com/admit, secret_env: ADMIT, timeout: 2d } }\n", "hooks.admission.timeout 48h0m0s: at most 30s"},
		// tunnel.anonymous (tunnel.md §6, §6.2; D277, D287).
		{"anonymous on a single-tenant gate (D277)", head + "tunnel: { anonymous: true }\n", "tunnel.anonymous: needs tenancy: multi"},
		{"anonymous limits on a single-tenant gate (D277)", head + "tunnel: { anonymous: { ttl: 1h } }\n", "D277"},
		{"anonymous hiring_streams over 32", head + "tenancy: multi\ntunnel: { anonymous: { hiring_streams: 33 } }\n", "hiring_streams 33: at most 32"},
		{"anonymous zero services", head + "tenancy: multi\ntunnel: { anonymous: { services: 0 } }\n", "tunnel.anonymous.services"},
		{"anonymous negative tasks", head + "tenancy: multi\ntunnel: { anonymous: { tasks: -1 } }\n", "tunnel.anonymous.tasks"},
		{"anonymous zero max_tenants", head + "tenancy: multi\ntunnel: { anonymous: { max_tenants: 0 } }\n", "tunnel.anonymous.max_tenants"},
		{"anonymous zero ttl", head + "tenancy: multi\ntunnel: { anonymous: { ttl: 0s } }\n", "must be positive"},
		{"anonymous zero purge_after", head + "tenancy: multi\ntunnel: { anonymous: { purge_after: 0h } }\n", "must be positive"},
		{"anonymous zero parts_bytes", head + "tenancy: multi\ntunnel: { anonymous: { parts_bytes: 0 } }\n", "must be positive"},
		{"anonymous ttl without a unit", head + "tenancy: multi\ntunnel: { anonymous: { ttl: 7200 } }\n", "missing unit"},
		{"anonymous unknown limit", head + "tenancy: multi\ntunnel: { anonymous: { ttl: 1h, sessions: 3 } }\n", "field sessions not found in tunnel.anonymous"},
		{"anonymous a string", head + "tenancy: multi\ntunnel: { anonymous: \"yes\" }\n", "want true, false or"},
		{"anonymous a list", head + "tenancy: multi\ntunnel: { anonymous: [ttl] }\n", "want true, false or"},
		{"anonymous calls a string", head + "tenancy: multi\ntunnel: { anonymous: { calls: many } }\n", "tunnel.anonymous.calls"},
		{"admission over http to a public host", head + "hooks: { admission: { url: http://cp.example.com/admit, secret_env: ADMIT } }\n", "https:// unless the host is loopback"},
		{"remote lifecycle hook without a queue", head + "hooks: { lifecycle: {} }\n", "hooks.lifecycle.task_queue: required"},
		{"oidc over http", head + "auth: { oidc: { issuer: http://login.example.com, audience: kontra } }\n", "auth.oidc.issuer"},
		{"oidc without audience", head + "auth: { oidc: { issuer: https://login.example.com } }\n", "auth.oidc.audience: required"},
		// D296 (the M4 WP-06 review): the RFC 9728 documents of a public_url with a path are not where RFC 9728 §3.1
		// and kontra login look for them.
		{"mcp_oauth with a public_url path", "postgres: postgres://k@db/k\npublic_url: http://localhost:8080/kontra\n" +
			"auth: { mcp_oauth: { issuer: https://login.example.com } }\n", "auth.mcp_oauth needs a public_url without a path"},
		{"mcp_oauth with a public_url path and slash", "postgres: postgres://k@db/k\npublic_url: http://localhost:8080/a/\n" +
			"auth: { mcp_oauth: { issuer: https://login.example.com } }\n", "auth.mcp_oauth needs a public_url without a path"},
		// peer: (spec §11.2, §17; D163, D179).
		{"peer without secret_env", head + "peer: { listen: \"127.0.0.1:8444\" }\n", "peer.secret_env"},
		{"peer secret_env not a variable name", head + "peer: { listen: \"127.0.0.1:8444\", secret_env: \"peer-secret\" }\n", "peer.secret_env"},
		{"peer without listen", head + "peer: { secret_env: PEER }\n", "peer.listen: required"},
		{"peer on the public listener's port", head + "listen: \":9000\"\npeer: { listen: \"127.0.0.1:9000\", secret_env: PEER }\n", "the peer listener is never the public one"},
		{"peer on the metrics port", head + "metrics: { listen: \"127.0.0.1:9100\" }\npeer: { listen: \":9100\", secret_env: PEER }\n", "must differ from metrics.listen"},
		{"peer listen not host:port", head + "peer: { listen: \"8444\", secret_env: PEER }\n", "peer.listen"},
		{"peer advertise without a host", head + "peer: { listen: \":8444\", advertise: \":8444\", secret_env: PEER }\n", "peer.advertise"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := parseErr(t, tc.yaml)
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q\nwant it to contain %q", err, tc.want)
			}
		})
	}
}

// D296: auth.mcp_oauth takes a public_url with no path or only "/", in any case and with an explicit default port
// (kontra login compares the resource as an origin); a path is refused only with auth.mcp_oauth.
func TestMCPOAuthPublicURL(t *testing.T) {
	for _, pub := range []string{"http://localhost:8080", "http://localhost:8080/", "http://LOCALHOST:8080", "http://localhost:80"} {
		parse(t, "postgres: postgres://k@db/k\npublic_url: "+pub+"\nauth: { mcp_oauth: { issuer: https://login.example.com } }\n")
	}
	parse(t, "postgres: postgres://k@db/k\npublic_url: http://localhost:8080/kontra\n")
}

// A load error never prints a password written into a URL: the gate refuses credentials in its URLs, and
// the refusal goes to stderr and the journal.
func TestLoadErrorsHideURLCredentials(t *testing.T) {
	const pw = "s3cretpw"
	const head = "postgres: postgres://kontra@localhost/kontra\npublic_url: http://localhost:8080\n"
	svc := func(body string) string { return head + "services:\n  s:\n" + body }
	for name, y := range map[string]string{
		"backend url":              svc("    kind: a2a\n    backend: { a2a: { url: \"https://bob:" + pw + "@seller.example/a2a\" } }\n"),
		"backend url, bad port":    svc("    kind: a2a\n    backend: { a2a: { url: \"https://bob:" + pw + "@seller.example:99999/a2a\" } }\n"),
		"backend url, unparseable": svc("    kind: a2a\n    backend: { a2a: { url: \"https://bob:" + pw + "@seller.example:x/a2a\" } }\n"),
		"backend url, no scheme":   svc("    kind: a2a\n    backend: { a2a: { url: \"bob:" + pw + "@seller.example/a2a\" } }\n"),
		"backend url, http":        svc("    kind: a2a\n    backend: { a2a: { url: \"http://bob:" + pw + "@seller.example/a2a\" } }\n"),
		"mcp backend url":          svc("    kind: mcp\n    backend: { mcp: { url: \"https://bob:" + pw + "@mcp.example/mcp\" } }\n"),
		"seller_keys url":          svc("    kind: a2a\n    backend: { tunnel: {} }\n    seller_keys: { url: \"https://u:" + pw + "@x.example/jwks\" }\n"),
		"oidc issuer":              head + "auth: { oidc: { issuer: \"https://u:" + pw + "@login.example\", audience: kontra } }\n",
		"oidc issuer, unparseable": head + "auth: { oidc: { issuer: \"https://u:" + pw + "@login.example:p\", audience: kontra } }\n",
		"mcp_oauth issuer":         head + "auth: { mcp_oauth: { issuer: \"https://u:" + pw + "@login.example\" } }\n",
		"admission hook url":       head + "hooks: { admission: { url: \"https://u:" + pw + "@cp.example/admit\", secret_env: ADMIT } }\n",
		"public_url":               "postgres: postgres://k@db/k\npublic_url: \"http://u:" + pw + "@localhost:8080\"\n",
		"public_url, unparseable":  "postgres: postgres://k@db/k\npublic_url: \"http://u:" + pw + "@localhost:port\"\n",
		"blobs":                    head + "blobs: \"s3://AKIAEXAMPLE:" + pw + "@bucket/prefix\"\n",
		"blobs, unparseable":       head + "blobs: \"s3://AKIAEXAMPLE:" + pw + "@bucket:x/prefix\"\n",
		"mcp allowed origin":       head + "mcp: { allowed_origins: [\"https://u:" + pw + "@a.example.com\"] }\n",
		"postgres, bad port":       "public_url: http://localhost:8080\npostgres: \"postgres://k:" + pw + "@db:99999/k\"\n",
		"postgres, bad pool size":  "public_url: http://localhost:8080\npostgres: \"postgres://k:" + pw + "@db/k?pool_max_conns=x\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			err := parseErr(t, y)
			if strings.Contains(err.Error(), pw) {
				t.Errorf("the error prints the password: %v", err)
			}
		})
	}
}

// Several problems are reported together, each on its own line.
func TestValidationListsEveryProblem(t *testing.T) {
	err := parseErr(t, `
public_url: http://agents.acme.io
services:
  Bad: { kind: a2a, backend: {} }
`)
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error %T %v, want *ValidationError", err, err)
	}
	if len(ve.Problems) < 4 {
		t.Errorf("problems = %q, want postgres, public_url, the name and the backend", ve.Problems)
	}
}

func TestAccepted(t *testing.T) {
	for _, tc := range []struct {
		name, yaml string
		check      func(*testing.T, *Config)
	}{
		{"gate behind a TLS proxy warns (D69)", `
public_url: https://agents.acme.io
postgres: postgres://k@db/k
tls: { terminated_by_proxy: true }
edge: { client_ip: { from: x_forwarded_for, trusted: [10.0.0.0/8, "192.0.2.10", "2001:db8::/32"] } }
`, func(t *testing.T, c *Config) {
			if c.Tunnel.ChannelBinding != ChannelBindingNone {
				t.Errorf("channel_binding = %q, want none behind a proxy", c.Tunnel.ChannelBinding)
			}
			if len(c.Warnings) != 1 || !strings.Contains(c.Warnings[0], "terminated_by_proxy") {
				t.Errorf("warnings = %q, want the D69 warning", c.Warnings)
			}
			want := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("192.0.2.10/32"), netip.MustParsePrefix("2001:db8::/32")}
			if !slices.Equal(c.Edge.ClientIP.TrustedPrefixes, want) {
				t.Errorf("trusted = %v, want %v", c.Edge.ClientIP.TrustedPrefixes, want)
			}
		}},
		{"ACME defaults its domain to the public host (D70)", `
public_url: https://Agents.Acme.io
postgres: postgres://k@db/k
tls: { acme: { email: ops@acme.io, cache_dir: /var/lib/kontra/acme } }
`, func(t *testing.T, c *Config) {
			if !slices.Equal(c.TLS.ACME.Domains, []string{"agents.acme.io"}) {
				t.Errorf("acme domains = %v", c.TLS.ACME.Domains)
			}
			if c.Tunnel.ChannelBinding != ChannelBindingTLSExporter {
				t.Errorf("channel_binding = %q", c.Tunnel.ChannelBinding)
			}
		}},
		{"plaintext on loopback addresses", `
public_url: http://[::1]:8080
postgres: postgres://k@db/k
`, nil},
		{"plaintext on 127.0.0.2", `
public_url: http://127.0.0.2:8080
postgres: postgres://k@db/k
`, nil},
		{"https backend with a path is a base URL (D71)", minimal + `
  multi:
    kind: a2a
    backend: { a2a: { url: "https://seller.example.com/agents/one" } }
    allow: ["key:kk_acme_*", "key:kk_acme_9f2c41aa", "key:*", "org:default", "oidc:sub:auth0|42", "node:k1_abcdefghijklmnopqrstuvwxyz", anonymous]
`, func(t *testing.T, c *Config) {
			if got := c.Services["multi"].Backend.Direct().URL; got != "https://seller.example.com/agents/one" {
				t.Errorf("url = %q", got)
			}
		}},
		{"listeners on distinct addresses or ephemeral ports", `
public_url: http://localhost:8080
postgres: postgres://k@db/k
listen: "127.0.0.1:8080"
metrics: { listen: "127.0.0.2:8080" }
`, nil},
		{"both listeners on ephemeral ports", `
public_url: http://localhost:8080
postgres: postgres://k@db/k
listen: ":0"
metrics: { listen: ":0" }
`, nil},
		{"pool parameters in the postgres URL", `
public_url: http://localhost:8080
postgres: "postgres://k@db/k?pool_max_conns=7&pool_max_conn_lifetime=1h"
`, nil},
		{"http backend with allow_private (D67)", minimal + `
  local:
    kind: a2a
    backend: { a2a: { url: "http://127.0.0.1:9999", allow_private: true } }
`, nil},
		{"mcp service with tools and tasks", minimal + `
  fs:
    kind: mcp
    backend: { mcp: { url: "https://mcp.example.com/mcp" } }
    tools: { allow: ["read_*"], deny: ["delete_*"] }
    tasks: passthrough
`, func(t *testing.T, c *Config) {
			if c.Services["fs"].Tasks != TasksPassthrough {
				t.Errorf("tasks = %q", c.Services["fs"].Tasks)
			}
			if len(c.Warnings) != 0 {
				t.Errorf("MCP services are built since M3 WP-04; warnings = %q", c.Warnings)
			}
		}},
		{"peer hop (M3 WP-04)", minimal + `
peer: { listen: ":8444", advertise: "gate-0.gate.kontra.svc:8444", secret_env: KONTRA_PEER_SECRET }
`, func(t *testing.T, c *Config) {
			if c.Peer == nil || c.Peer.Listen != ":8444" || c.Peer.SecretEnv != "KONTRA_PEER_SECRET" || len(c.Warnings) != 0 {
				t.Errorf("peer %+v, warnings %q", c.Peer, c.Warnings)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := parse(t, tc.yaml)
			if tc.check != nil {
				tc.check(t, c)
			}
		})
	}
}

// The remote Admission hook loads (D275 lifts the M1-M3 refusal): over https://, or http:// to a loopback host
// (spec §13.1, §10.3), with its secret_env, timeout and operations as written and their defaults otherwise, and no
// warning. Its secret is read from the environment at start, not here: a file whose variable is unset still loads,
// so that migrate and enroll need not hold it.
func TestRemoteAdmissionLoads(t *testing.T) {
	c := parse(t, minimal+"hooks: { admission: { url: \"http://127.0.0.1:7000/admit\", secret_env: ADMIT, timeout: 2s, operations: [task, call] } }\n")
	h := c.Hooks.Admission
	if h == nil || h.URL != "http://127.0.0.1:7000/admit" || h.SecretEnv != "ADMIT" || h.Timeout.D() != 2*time.Second ||
		!slices.Equal(h.Operations, []string{"task", "call"}) {
		t.Fatalf("hooks.admission = %+v", h)
	}
	if len(c.Warnings) != 0 {
		t.Errorf("warnings %q, want none", c.Warnings)
	}
	t.Setenv("KONTRA_TEST_UNSET_ADMIT", "")
	c = parse(t, minimal+"hooks: { admission: { url: \"https://cp.example.com/admit\", secret_env: KONTRA_TEST_UNSET_ADMIT } }\n")
	if h := c.Hooks.Admission; h == nil || h.Timeout.D() != 5*time.Second || !slices.Equal(h.Operations, []string{"task"}) {
		t.Errorf("hooks.admission defaults = %+v, want timeout 5s and operations [task]", h)
	}
	for _, ok := range []string{"https://10.0.0.7:8443/admit", "http://localhost:7000/admit", "http://[::1]:7000/admit"} {
		parse(t, minimal+"hooks: { admission: { url: \""+ok+"\", secret_env: ADMIT } }\n")
	}
	if c = parse(t, minimal+"hooks: { admission: { url: https://cp.example.com/admit, secret_env: ADMIT, timeout: 30s } }\n"); c.Hooks.Admission.Timeout.D() != MaxAdmissionWait {
		t.Errorf("a 30s timeout: %v", c.Hooks.Admission.Timeout)
	}
}

// The remote Lifecycle hook loads (D186, M3 WP-10): its queue is kept as written, the file gives no other
// problem, and a remote Admission hook beside it loads too (D275).
func TestRemoteLifecycleAccepted(t *testing.T) {
	c := parse(t, minimal+"hooks: { lifecycle: { task_queue: m3pentest-lifecycle } }\n")
	if c.Hooks.Lifecycle == nil || c.Hooks.Lifecycle.TaskQueue != "m3pentest-lifecycle" {
		t.Fatalf("hooks.lifecycle = %+v, want task_queue m3pentest-lifecycle", c.Hooks.Lifecycle)
	}
	if c.Hooks.Admission != nil {
		t.Errorf("hooks.admission = %+v, want the built-in", c.Hooks.Admission)
	}
	for _, w := range c.Warnings {
		if strings.Contains(w, "lifecycle") {
			t.Errorf("a warning names the accepted hook: %q", w)
		}
	}
	c = parse(t, minimal+"hooks: { lifecycle: { task_queue: q }, admission: { url: \"http://127.0.0.1:7000/admit\", secret_env: ADMIT } }\n")
	if c.Hooks.Lifecycle == nil || c.Hooks.Admission == nil || c.Hooks.Admission.URL != "http://127.0.0.1:7000/admit" {
		t.Errorf("hooks = %+v", c.Hooks)
	}
}

// The seller_keys mapping decodes (the skeleton's json.RawMessage could not), as JSON.
func TestSellerKeysDecodeAsJSON(t *testing.T) {
	c := parse(t, mcpDirect+`
    seller_keys:
      keys:
        - { kty: EC, crv: P-256, x: "f83OJ3D2xF1Bg8vub9tLe1gHMzV76e8Tus9uPHvRVEU", y: "x_FEzRu9m36HLN_tue659LNpXW6pCyStikYjKIWI5a0", kid: "k1", use: sig }
      extra: 3
`)
	got := c.Services["hello"].SellerKeys
	var v struct {
		Keys  []map[string]string `json:"keys"`
		Extra int                 `json:"extra"`
	}
	if err := json.Unmarshal(got, &v); err != nil {
		t.Fatalf("seller_keys %s: %v", got, err)
	}
	if len(v.Keys) != 1 || v.Keys[0]["kid"] != "k1" || v.Keys[0]["crv"] != "P-256" || v.Extra != 3 {
		t.Errorf("seller_keys = %s", got)
	}
	c = parse(t, mcpDirect+"    seller_keys: { url: \"https://seller.example.com/.well-known/jwks.json\" }\n")
	if string(c.Services["hello"].SellerKeys) != `{"url":"https://seller.example.com/.well-known/jwks.json"}` {
		t.Errorf("seller_keys {url} = %s", c.Services["hello"].SellerKeys)
	}
	c = parse(t, minimal+"    annotations: { search: { price: \"1.00 EUR\" }, card_extensions: [ { uri: \"https://x.example.com/ext\", required: false } ], n: 1.5, z: null }\n")
	want := `{"card_extensions":[{"required":false,"uri":"https://x.example.com/ext"}],"n":1.5,"search":{"price":"1.00 EUR"},"z":null}`
	if string(c.Services["hello"].Annotations) != want {
		t.Errorf("annotations = %s\nwant %s", c.Services["hello"].Annotations, want)
	}
	for _, bad := range []string{"{ x: -.inf }", "{ n: .nan }", "{ a: &x 1, b: *x }", "{ ? [1] : 2 }"} {
		if _, err := Parse([]byte(minimal+"    annotations: "+bad+"\n"), ""); err == nil {
			t.Errorf("annotations: %s decoded, want an error", bad)
		}
	}
}

// An oidc:* entry (D301) is accepted; on a gate without an OIDC issuer it admits nobody, which the load says.
func TestAllowOIDCAnyWarnsWithoutAnIssuer(t *testing.T) {
	const svc = "    allow: [\"oidc:*\", \"key:kk_acme_*\"]\n"
	c := parse(t, minimal+svc)
	if len(c.Warnings) != 1 || !strings.Contains(c.Warnings[0], "services.hello.allow: oidc:* admits nobody") {
		t.Errorf("warnings without auth.mcp_oauth = %q, want one naming services.hello.allow", c.Warnings)
	}
	c = parse(t, "auth: { mcp_oauth: { issuer: https://login.example.com } }\n"+minimal+svc)
	if len(c.Warnings) != 0 || !slices.Equal(c.Services["hello"].Allow, []string{"oidc:*", "key:kk_acme_*"}) {
		t.Errorf("with auth.mcp_oauth: warnings %q, allow %q", c.Warnings, c.Services["hello"].Allow)
	}
	if err := parseErr(t, minimal+"    allow: [\"oidc:sub:*\"]\n"); err == nil || !strings.Contains(err.Error(), "write oidc:*") {
		t.Errorf("oidc:sub:*: %v, want an error naming oidc:*", err)
	}
}

func TestParseAllowEntry(t *testing.T) {
	for in, want := range map[string]AllowEntry{
		"anonymous":                          {Kind: AllowAnonymous},
		"key:kk_acme_*":                      {Kind: AllowKey, Value: "kk_acme_*"},
		"key:kk_acme_abc12345":               {Kind: AllowKey, Value: "kk_acme_abc12345"},
		"org:default":                        {Kind: AllowOrg, Value: "default"},
		"oidc:sub:auth0|abc:de":              {Kind: AllowOIDCSub, Value: "auth0|abc:de"},
		"oidc:sub:*x":                        {Kind: AllowOIDCSub, Value: "*x"},
		"oidc:*":                             {Kind: AllowOIDCAny},
		"node:k1_abcdefghijklmnopqrstuvwxyz": {Kind: AllowNode, Value: "k1_abcdefghijklmnopqrstuvwxyz"},
	} {
		got, err := ParseAllowEntry(in)
		if err != nil || got != want {
			t.Errorf("ParseAllowEntry(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	// A key: glob matches prefixes of at least its non-* length and, without a *, of exactly its length; one
	// that cannot meet [A-Za-z0-9_]{4,32} is refused (D68).
	for _, in := range []string{"key:kk_?", "key:????", "key:" + strings.Repeat("?", 32), "key:*?", "key:" + strings.Repeat("a", 31) + "?*", "key:k*"} {
		if _, err := ParseAllowEntry(in); err != nil {
			t.Errorf("ParseAllowEntry(%q) = %v, want it accepted", in, err)
		}
	}
	for _, in := range []string{"", " anonymous", "Anonymous", "key:", "key:acme-*", "key:kk.acme.*", "key:" + strings.Repeat("a", 33) + "*",
		"key:ab?", "key:???", "key:" + strings.Repeat("?", 33), "key:" + strings.Repeat("a", 30) + "???*", "key:" + strings.Repeat("a", 33),
		"org:", "org:a_b", "oidc:sub:", "oidc:", "oidc:sub:*", "oidc:**", "oidc:* ", "oidc:any", "oidc:sub", "node:", "user:x", "anonymous:x"} {
		if got, err := ParseAllowEntry(in); err == nil {
			t.Errorf("ParseAllowEntry(%q) = %+v, want an error", in, got)
		}
	}
}

// Fail-closed keys this build ignores are reported as warnings, so an operator is not surprised. A tunnel
// service is served (M2): it gets none; neither do blobs: and signing_key, built in M3 WP-02, nor MCP services and
// peer:, built in M3 WP-04.
func TestUnbuiltWarnings(t *testing.T) {
	c := parse(t, minimal+`
  remote:
    kind: a2a
    backend: { tunnel: {} }
  files:
    kind: mcp
    backend: { mcp: { url: "https://mcp.example.com/mcp" } }
    seller_keys: { url: "https://seller.example.com/jwks.json" }
blobs: s3://kontra-parts
signing_key: file:./kontra.key
auth: { api_keys: true, oidc: { issuer: "https://login.example.com", audience: kontra }, mcp_oauth: { issuer: "https://login.example.com" } }
tenancy: multi
tunnel: { anonymous: true }
peer: { listen: "127.0.0.1:8444", secret_env: KONTRA_PEER_SECRET }
`)
	// tunnel.anonymous (M4 WP-03) and auth.mcp_oauth (M4 WP-06) are built: they load with no warning.
	want := []string{"auth.oidc", "services.files.seller_keys"}
	for _, w := range want {
		if !slices.ContainsFunc(c.Warnings, func(s string) bool { return strings.HasPrefix(s, w) }) {
			t.Errorf("no warning starting %q in %q", w, c.Warnings)
		}
	}
	if len(c.Warnings) != len(want) {
		t.Errorf("warnings %q, want only %q", c.Warnings, want)
	}
	for _, w := range c.Warnings {
		if strings.Contains(w, "services.remote") || strings.Contains(w, "tunnel backends") {
			t.Errorf("a served tunnel service is warned about: %q", w)
		}
		if strings.Contains(w, "tunnel.anonymous") || strings.Contains(w, "mcp_oauth") {
			t.Errorf("tunnel.anonymous and auth.mcp_oauth are built, but warned about: %q", w)
		}
	}
	// The plain tunnel service loads with no warning at all.
	if c := parse(t, tunnelled); len(c.Warnings) != 0 {
		t.Errorf("a tunnel service: warnings %q", c.Warnings)
	}
}

func TestPublicURLWithoutTrailingSlash(t *testing.T) {
	for in, want := range map[string]string{
		"http://localhost:8080/":         "http://localhost:8080",
		"http://localhost:8080":          "http://localhost:8080",
		"http://localhost:8080/gate//":   "http://localhost:8080/gate",
		"http://127.0.0.1/kontra/agents": "http://127.0.0.1/kontra/agents",
	} {
		c := parse(t, "postgres: postgres://k@db/k\npublic_url: "+in+"\n")
		if c.PublicURL != want {
			t.Errorf("public_url %q loads as %q, want %q", in, c.PublicURL, want)
		}
	}
	parseErr(t, "postgres: postgres://k@db/k\npublic_url: http://\n")
}

func TestOverrides(t *testing.T) {
	c := parse(t, minimal, WithListen("127.0.0.1:18080"), WithMetricsListen("127.0.0.1:19090"))
	if c.Listen != "127.0.0.1:18080" || c.Metrics.Listen != "127.0.0.1:19090" {
		t.Errorf("listen %q metrics %q", c.Listen, c.Metrics.Listen)
	}
	if _, err := Parse([]byte(minimal), "", WithMetricsListen("nope")); err == nil {
		t.Error("an override is validated like the file")
	}
}

// Load resolves relative file paths against the file's directory.
func TestLoadResolvesPaths(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "kontra.yaml")
	y := `
public_url: https://localhost:8443
postgres: postgres://k@db/k
signing_key: file:./kontra.key
tls: { cert: certs/fullchain.pem, key: /etc/kontra/privkey.pem }
`
	if err := os.WriteFile(p, []byte(y), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.TLS.Cert != filepath.Join(dir, "certs/fullchain.pem") || c.TLS.Key != "/etc/kontra/privkey.pem" ||
		c.SigningKey != "file:"+filepath.Join(dir, "kontra.key") {
		t.Errorf("cert %q key %q signing_key %q", c.TLS.Cert, c.TLS.Key, c.SigningKey)
	}
	if _, err := Load(filepath.Join(dir, "missing.yaml")); err == nil {
		t.Error("a missing file loads")
	}
}

// The hello-exec seller's configuration loads as its README describes.
func TestHelloExecLoads(t *testing.T) {
	c, err := Load("../../examples/sellers/hello-exec/kontra.yaml")
	if err != nil {
		t.Fatal(err)
	}
	s, ok := c.Services["hello"]
	if !ok || s.Kind != KindA2A || s.Backend.A2A == nil || s.Backend.A2A.URL != "http://127.0.0.1:9999" || !s.Backend.A2A.AllowPrivate {
		t.Fatalf("services.hello = %+v", s)
	}
	if !slices.Equal(s.Allow, []string{"org:default"}) || s.Task.Timeout.D() != 10*time.Minute || s.Task.RetryCount() != 2 {
		t.Errorf("services.hello = %+v", s)
	}
	if !c.Auth.APIKeys || c.Listen != "127.0.0.1:8080" || len(c.Warnings) != 0 {
		t.Errorf("auth %+v listen %q warnings %q", c.Auth, c.Listen, c.Warnings)
	}
}

// The examples' tunnel gates (M2) load as their READMEs describe: one tunnel service each, for the team's
// tenant, on a plaintext localhost gate whose tunnel channel binding is therefore none, with no warning (no
// sealed or seller_keys, which a tunnel service refuses in M2).
func TestTunnelExamplesLoad(t *testing.T) {
	for path, name := range map[string]string{
		"../../examples/sellers/hello-exec/kontra-tunnel.yaml": "hello",
		"../../examples/sellers/helloworld/kontra.yaml":        "hello",
		"../../examples/sellers/booking/kontra.yaml":           "booking",
	} {
		c, err := Load(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if len(c.Services) != 1 {
			t.Errorf("%s: services %v, want only %s", path, slices.Sorted(maps.Keys(c.Services)), name)
		}
		s, ok := c.Services[name]
		if !ok || s.Kind != KindA2A || s.Backend.Tunnel == nil || s.Backend.Direct() != nil {
			t.Fatalf("%s: services.%s = %+v, want an a2a tunnel service", path, name, s)
		}
		if !slices.Equal(s.Allow, []string{"org:default"}) || s.Task.Timeout.D() != 10*time.Minute || s.Task.RetryCount() != 2 ||
			s.Sealed.Mode != SealedOff || len(s.SellerKeys) != 0 {
			t.Errorf("%s: services.%s = %+v", path, name, s)
		}
		if c.PublicURL != "http://localhost:8080" || c.Listen != "127.0.0.1:8080" || !c.Auth.APIKeys ||
			c.Tunnel.ChannelBinding != ChannelBindingNone || len(c.Warnings) != 0 {
			t.Errorf("%s: public_url %q listen %q auth %+v channel binding %q warnings %q",
				path, c.PublicURL, c.Listen, c.Auth, c.Tunnel.ChannelBinding, c.Warnings)
		}
	}
}

// The sealed variants of the tunnel examples (M3, examples/sealing/README.md) load as the plain ones do, with
// sealed: required and no warning.
func TestSealedExamplesLoad(t *testing.T) {
	for path, name := range map[string]string{
		"../../examples/sellers/hello-exec/kontra-sealed.yaml": "hello",
		"../../examples/sellers/booking/kontra-sealed.yaml":    "booking",
	} {
		c, err := Load(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		s := c.Services[name]
		if len(c.Services) != 1 || s.Kind != KindA2A || s.Backend.Tunnel == nil || s.Sealed.Mode != SealedRequired || len(c.Warnings) != 0 {
			t.Errorf("%s: services.%s = %+v, warnings %q", path, name, s, c.Warnings)
		}
	}
}

// The schema's fixture is also a configuration the gate loads, with the values it says, but for sealing:
// its two tunnel services set sealed, which this build refuses until M3 (M2 serves tunnel services and
// does not seal; D91 as amended for M2). The fixture is refused for exactly those two keys, and loads
// without them.
func TestTeamFixtureLoads(t *testing.T) {
	const path = "../../spec/schema/examples/config.team.json"
	// Sealing is built (M3 WP-08 lifts D144): the team fixture, with its sealed tunnel services, loads as it is.
	team, err := Load(path)
	if err != nil {
		t.Fatalf("Load(%s) = %v", path, err)
	}
	if team.Services["recon"].Sealed.Mode != SealedRequired || team.Services["fs"].Sealed.Mode != SealedOptional {
		t.Errorf("the fixture's sealed services load as %q and %q", team.Services["recon"].Sealed.Mode, team.Services["fs"].Sealed.Mode)
	}
	if n := team.Services["fs"].SessionsPerPrincipal(); n != 2 {
		t.Errorf("the fixture's fs service caps a principal's sessions at %d, want 2 (D213)", n)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	sealed := map[string]Sealed{}
	for name, svc := range doc["services"].(map[string]any) {
		m := svc.(map[string]any)
		if v, ok := m["sealed"]; ok {
			vb, _ := json.Marshal(v)
			if sealed[name], err = decodeSealed(t, string(vb)); err != nil {
				t.Fatalf("services.%s.sealed: %v", name, err)
			}
			delete(m, "sealed")
		}
	}
	b, _ = json.Marshal(doc)
	unsealed := filepath.Join(t.TempDir(), "config.team.json")
	if err := os.WriteFile(unsealed, b, 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(unsealed)
	if err != nil {
		t.Fatal(err)
	}
	if c.SigningKey != "file:"+filepath.Join(filepath.Dir(unsealed), "kontra.key") {
		t.Errorf("signing_key %q", c.SigningKey)
	}
	if sealed["recon"].Mode != SealedRequired || sealed["fs"].Mode != SealedOptional || c.Services["triage"].Sealed.Mode != SealedOff {
		t.Errorf("sealed = %q, %q, %q", sealed["recon"].Mode, sealed["fs"].Mode, c.Services["triage"].Sealed.Mode)
	}
	if !slices.Equal(c.TLS.ACME.Domains, []string{"agents.acme.io"}) {
		t.Errorf("acme domains = %v", c.TLS.ACME.Domains)
	}
	if tr := c.Services["triage"]; tr.Task.RetryCount() != 0 || tr.Task.Retention.D() != 14*24*time.Hour {
		t.Errorf("triage task = %+v", tr.Task)
	}
	if fs := c.Services["fs"]; fs.Tasks != TasksPassthrough || fs.SessionIdle != 1800 || fs.Tools == nil {
		t.Errorf("fs = %+v", fs)
	}
}

// spec/schema/config.json and the Go types name the same keys, in both directions.
func TestSchemaInStep(t *testing.T) {
	b, err := os.ReadFile("../../spec/schema/config.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(b, &schema); err != nil {
		t.Fatal(err)
	}
	resolve := func(n map[string]any) map[string]any {
		for {
			// A value with several shapes (tunnel.anonymous: a boolean or the limits object) is walked through its
			// object branch, the one whose keys the Go type names.
			if alts, ok := n["oneOf"].([]any); ok {
				for _, a := range alts {
					if am, ok := a.(map[string]any); ok && am["type"] == "object" {
						n = am
						break
					}
				}
			}
			ref, ok := n["$ref"].(string)
			if !ok {
				return n
			}
			name, ok := strings.CutPrefix(ref, "#/$defs/")
			if !ok {
				t.Fatalf("unexpected $ref %q", ref)
			}
			n = schema["$defs"].(map[string]any)[name].(map[string]any)
		}
	}
	leaf := map[reflect.Type]bool{
		reflect.TypeFor[Duration](): true, reflect.TypeFor[ByteSize](): true, reflect.TypeFor[Seconds](): true,
		reflect.TypeFor[Sealed](): true, reflect.TypeFor[JSON](): true,
	}
	var walk func(path string, typ reflect.Type, node map[string]any)
	walk = func(path string, typ reflect.Type, node map[string]any) {
		node = resolve(node)
		for typ.Kind() == reflect.Pointer {
			typ = typ.Elem()
		}
		if leaf[typ] {
			return
		}
		switch typ.Kind() {
		case reflect.Map:
			ap, ok := node["additionalProperties"].(map[string]any)
			if !ok {
				t.Errorf("%s: a map in Go, no additionalProperties schema", path)
				return
			}
			walk(path+".<name>", typ.Elem(), ap)
			return
		case reflect.Struct:
		default:
			return
		}
		props, _ := node["properties"].(map[string]any)
		inGo := map[string]bool{}
		for i := range typ.NumField() {
			f := typ.Field(i)
			tag := strings.Split(f.Tag.Get("yaml"), ",")[0]
			if tag == "" || tag == "-" {
				continue
			}
			inGo[tag] = true
			sub, ok := props[tag].(map[string]any)
			if !ok {
				t.Errorf("%s.%s: in Go, missing from spec/schema/config.json", path, tag)
				continue
			}
			walk(path+"."+tag, f.Type, sub)
		}
		for k := range props {
			if !inGo[k] {
				t.Errorf("%s.%s: in spec/schema/config.json, missing from the Go types", path, k)
			}
		}
		if typ.NumField() > 0 && node["additionalProperties"] != false {
			t.Errorf("%s: the schema must refuse unknown keys (additionalProperties: false), as Load does", path)
		}
	}
	walk("config", reflect.TypeFor[Config](), schema)
}

// The MCP examples' gates (M3 WP-05) load as their READMEs describe: one MCP tunnel service each, the dangerous
// tools denied (D185 for server-everything; the write tools for server-filesystem), with no warning.
func TestMCPExamplesLoad(t *testing.T) {
	for path, want := range map[string]struct {
		name string
		deny []string
	}{
		"../../examples/mcp/fs-stdio/kontra.yaml":   {"fs", []string{"write_file", "edit_file", "create_directory", "move_file"}},
		"../../examples/mcp/everything/kontra.yaml": {"everything", []string{"get-env", "gzip-file-as-resource"}},
	} {
		c, err := Load(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		s, ok := c.Services[want.name]
		if len(c.Services) != 1 || !ok || s.Kind != KindMCP || s.Backend.Tunnel == nil || s.Backend.Direct() != nil {
			t.Fatalf("%s: services %v, want the MCP tunnel service %s", path, slices.Sorted(maps.Keys(c.Services)), want.name)
		}
		if s.Tools == nil || !slices.Equal(s.Tools.Deny, want.deny) || !slices.Equal(s.Allow, []string{"org:default"}) {
			t.Errorf("%s: services.%s = %+v (tools %+v)", path, want.name, s, s.Tools)
		}
		if c.PublicURL != "http://localhost:8080" || !c.Auth.APIKeys || len(c.Warnings) != 0 {
			t.Errorf("%s: public_url %q auth %+v warnings %q", path, c.PublicURL, c.Auth, c.Warnings)
		}
	}
}

// tunnel.anonymous (tunnel.md §6, §6.2; relay.md §17; D277, D287) is false by default, true for §6.2's limits, or
// the limits object, each absent key taking its default; the staging gate's fixture loads with its own.
func TestTunnelAnonymous(t *testing.T) {
	defaults := AnonymousSessions{Enabled: true, TTL: Duration(2 * time.Hour), TenantsPerNetwork: 3, Services: 4, Tasks: 4,
		Calls: 8, Streams: 8, HiringStreams: 8, PartsBytes: 64 << 20, PurgeAfter: Duration(24 * time.Hour), MaxTenants: 1000}
	multi := minimal + "tenancy: multi\n"
	for _, y := range []string{"tunnel: { anonymous: true }\n", "tunnel: { anonymous: {} }\n", "tunnel:\n  anonymous:\n"} {
		c, err := Parse([]byte(multi+y), "")
		if y == "tunnel:\n  anonymous:\n" {
			// A key with no value is null: absent, as everywhere in the file, so off.
			if err != nil || c.Tunnel.Anonymous != (AnonymousSessions{}) {
				t.Errorf("%q = %+v, %v; want off", y, c.Tunnel.Anonymous, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%q: %v", y, err)
		}
		if c.Tunnel.Anonymous != defaults {
			t.Errorf("%q = %+v, want §6.2's defaults %+v", y, c.Tunnel.Anonymous, defaults)
		}
	}
	if c := parse(t, multi+"tunnel: { anonymous: false }\n"); c.Tunnel.Anonymous != (AnonymousSessions{}) {
		t.Errorf("anonymous: false = %+v", c.Tunnel.Anonymous)
	}
	// The object form as the e2e harness compresses it: what it names wins, the rest is defaulted.
	c := parse(t, multi+`tunnel:
  anonymous: { ttl: 20s, purge_after: 10s, tenants_per_network: 3, services: 4, tasks: 4, hiring_streams: 8, parts_bytes: 1MiB, calls: 2 }
  enroll_with_api_key: true
`)
	want := AnonymousSessions{Enabled: true, TTL: Duration(20 * time.Second), TenantsPerNetwork: 3, Services: 4, Tasks: 4, Calls: 2,
		Streams: 8, HiringStreams: 8, PartsBytes: 1 << 20, PurgeAfter: Duration(10 * time.Second), MaxTenants: 1000}
	if c.Tunnel.Anonymous != want || !c.Tunnel.EnrollWithAPIKey {
		t.Errorf("the compressed limits = %+v, want %+v", c.Tunnel.Anonymous, want)
	}
	if c := parse(t, multi+"tunnel: { anonymous: { hiring_streams: 32, max_tenants: 1, parts_bytes: \"512 KiB\" } }\n"); c.Tunnel.Anonymous.HiringStreams != 32 ||
		c.Tunnel.Anonymous.MaxTenants != 1 || c.Tunnel.Anonymous.PartsBytes != 512<<10 || c.Tunnel.Anonymous.TTL.D() != 2*time.Hour {
		t.Errorf("the bounds = %+v", c.Tunnel.Anonymous)
	}
	// streams is its own limit (D308).
	if c := parse(t, multi+"tunnel: { anonymous: { streams: 3 } }\n"); c.Tunnel.Anonymous.Streams != 3 || c.Tunnel.Anonymous.Calls != 8 {
		t.Errorf("streams: 3 = %+v", c.Tunnel.Anonymous)
	}
	// enabled: false (D309): off, keeping the limits it names for the anonymous tenants that live (their purge_after
	// above all), the others left to §6.2's defaults; enabled: true is the object form's default. A disabled block's
	// limits are not refused on a single-tenant gate, which has no anonymous tenant, but are still checked.
	for _, y := range []string{multi, minimal} {
		c := parse(t, y+"tunnel: { anonymous: { enabled: false, purge_after: 168h, ttl: 1h } }\n")
		if want := (AnonymousSessions{PurgeAfter: Duration(168 * time.Hour), TTL: Duration(time.Hour)}); c.Tunnel.Anonymous != want {
			t.Errorf("enabled: false with limits = %+v, want %+v", c.Tunnel.Anonymous, want)
		}
	}
	if c := parse(t, multi+"tunnel: { anonymous: { enabled: true, calls: 3 } }\n"); !c.Tunnel.Anonymous.Enabled || c.Tunnel.Anonymous.Calls != 3 ||
		c.Tunnel.Anonymous.TTL.D() != 2*time.Hour {
		t.Errorf("enabled: true = %+v", c.Tunnel.Anonymous)
	}
	for y, want := range map[string]string{
		"tunnel: { anonymous: { enabled: no } }\n":                        "tunnel.anonymous.enabled",
		"tunnel: { anonymous: { enabled: 0 } }\n":                         "tunnel.anonymous.enabled",
		"tunnel: { anonymous: { enabled: false, enabled: true } }\n":      "repeated",
		"tunnel: { anonymous: { enabled: false, hiring_streams: 33 } }\n": "hiring_streams 33: at most 32",
		"tunnel: { anonymous: { enabled: false, streams: 0 } }\n":         "tunnel.anonymous.streams",
	} {
		if err := parseErr(t, multi+y); !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", y, err, want)
		}
	}
	// Refused on a single-tenant gate, naming D277's rule, and nothing else is wrong with the file.
	err := parseErr(t, minimal+"tunnel: { anonymous: true }\n")
	var ve *ValidationError
	if !errors.As(err, &ve) || len(ve.Problems) != 1 || !strings.Contains(ve.Problems[0], "needs tenancy: multi") || !strings.Contains(ve.Problems[0], "D277") {
		t.Errorf("a single-tenant gate with tunnel.anonymous: %v", err)
	}
	// A repeated key is refused by the YAML decoder itself; an unknown one by name.
	if _, err := Parse([]byte(multi+"tunnel:\n  anonymous:\n    ttl: 1h\n    ttl: 2h\n"), ""); err == nil {
		t.Error("a repeated tunnel.anonymous key loaded")
	}

	staging, err := Load("../../spec/schema/examples/config.staging.json")
	if err != nil {
		t.Fatalf("the staging fixture: %v", err)
	}
	if a := staging.Tunnel.Anonymous; !a.Enabled || a.MaxTenants != 200 || a.Calls != 8 || a.TTL.D() != 2*time.Hour ||
		a.PurgeAfter.D() != 24*time.Hour || a.PartsBytes != 64<<20 || staging.Tenancy != TenancyMulti {
		t.Errorf("the staging fixture's tunnel.anonymous = %+v", a)
	}
}
