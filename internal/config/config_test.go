package config

import (
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
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

// tunnelled is minimal with a tunnel backend: sealing and seller keys belong to tunnel services, which
// this build does not serve, so ignoring them fails closed.
const tunnelled = `
public_url: http://localhost:8080
postgres: postgres://kontra@localhost:5432/kontra
services:
  hello:
    kind: a2a
    backend: { tunnel: {} }
`

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
		{"tunnel.anonymous", c.Tunnel.Anonymous, false},
		{"tunnel.enroll_with_api_key", c.Tunnel.EnrollWithAPIKey, false},
		{"edge.client_ip.from", c.Edge.ClientIP.From, ClientIPRemote},
		{"services.hello.allow", s.Allow, []string{}},
		{"services.hello.tools", s.Tools, (*Tools)(nil)},
		{"services.hello.tasks (a2a)", s.Tasks, ""},
		{"services.hello.sealed", s.Sealed.Mode, SealedOff},
		{"services.hello.seller_keys", s.SellerKeys.IsZero(), true},
		{"services.hello.session_idle_s", s.SessionIdle.D(), 3600 * time.Second},
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
		c := parse(t, tunnelled+"    sealed: "+tc.yaml+"\n")
		if got := c.Services["hello"].Sealed.Mode; got != tc.want {
			t.Errorf("sealed: %s loads as %q, want %q", tc.yaml, got, tc.want)
		}
	}
	for _, bad := range []string{`"yes"`, "1", "{require: true}", "{required: maybe}", "[true]"} {
		err := parseErr(t, tunnelled+"    sealed: "+bad+"\n")
		if !strings.Contains(err.Error(), "sealed") {
			t.Errorf("sealed: %s: error %q does not name sealed", bad, err)
		}
	}
}

// Sealing and seller-key verification are not built (M3). A key this build would ignore on a service it
// serves fails open, so it is refused: sealed on a direct backend (which can never be sealed: no node
// unseals for it) and seller_keys on a direct a2a backend (its card would be served unverified).
func TestFailOpenKeysRefused(t *testing.T) {
	for _, tc := range []struct{ name, yaml, want string }{
		{"sealed on a direct a2a backend", minimal + "    sealed: true\n", "services.hello.sealed: sealing needs backend tunnel"},
		{"optional sealing on a direct a2a backend", minimal + "    sealed: { required: false }\n", "sealing needs backend tunnel"},
		{"sealed on a direct mcp backend", minimal + "  fs:\n    kind: mcp\n    backend: { mcp: { url: \"https://mcp.example.com\" } }\n    sealed: true\n", "services.fs.sealed: sealing needs backend tunnel"},
		{"seller_keys on a direct a2a backend", minimal + "    seller_keys: { url: \"https://seller.example.com/jwks.json\" }\n", "services.hello.seller_keys: seller card signatures are not verified in this release"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := parseErr(t, tc.yaml); !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q\nwant it to contain %q", err, tc.want)
			}
		})
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
		{"allow without kind", svc("    kind: a2a\n    backend: { tunnel: {} }\n    allow: [everyone]\n"), "want key:, org:, oidc:sub:, node: or anonymous"},
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
		{"bad origin", head + "mcp: { allowed_origins: [\"https://a.example.com/path\"] }\n", "is not an origin"},
		{"xff without trusted (D69)", head + "edge: { client_ip: { from: x_forwarded_for } }\n", "needs edge.client_ip.trusted"},
		{"proxy_protocol without trusted (D69)", head + "edge: { client_ip: { from: proxy_protocol } }\n", "needs edge.client_ip.trusted"},
		{"trusted with remote", head + "edge: { client_ip: { trusted: [10.0.0.0/8] } }\n", "only used with from: proxy_protocol or x_forwarded_for"},
		{"bad trusted CIDR", head + "edge: { client_ip: { from: x_forwarded_for, trusted: [10.0.0.0/33] } }\n", "is not a CIDR"},
		{"bad client_ip source", head + "edge: { client_ip: { from: header } }\n", "want remote, proxy_protocol or x_forwarded_for"},
		{"tls-exporter without TLS", head + "tunnel: { channel_binding: tls-exporter }\n", "needs the gate to terminate TLS"},
		{"remote admission hook (M5)", head + "hooks: { admission: { url: https://cp.example.com/admit, secret_env: ADMIT } }\n", "remote Admission hook is not built"},
		{"admission over http to a public host", head + "hooks: { admission: { url: http://cp.example.com/admit, secret_env: ADMIT } }\n", "https:// unless the host is loopback"},
		{"remote lifecycle hook (M5)", head + "hooks: { lifecycle: { task_queue: cloud-lifecycle } }\n", "remote Lifecycle hook is not built"},
		{"oidc over http", head + "auth: { oidc: { issuer: http://login.example.com, audience: kontra } }\n", "auth.oidc.issuer"},
		{"oidc without audience", head + "auth: { oidc: { issuer: https://login.example.com } }\n", "auth.oidc.audience: required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := parseErr(t, tc.yaml)
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q\nwant it to contain %q", err, tc.want)
			}
		})
	}
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
			if len(c.Warnings) != 1 || !strings.Contains(c.Warnings[0], "services.fs: MCP services are not built") {
				t.Errorf("warnings = %q", c.Warnings)
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

// A remote Admission hook is well-formed over http:// to a loopback host (spec §13.1); this build refuses it
// only because it is not built, never by falling back to the built-in allow.
func TestRemoteAdmissionRefusedAsUnbuilt(t *testing.T) {
	err := parseErr(t, minimal+"hooks: { admission: { url: \"http://127.0.0.1:7000/admit\", secret_env: ADMIT, operations: [task, call] } }\n")
	var ve *ValidationError
	if !errors.As(err, &ve) || len(ve.Problems) != 1 || !strings.Contains(ve.Problems[0], "remote Admission hook is not built") {
		t.Errorf("error = %v, want only the not-built refusal", err)
	}
}

// The seller_keys mapping decodes (the skeleton's json.RawMessage could not), as JSON.
func TestSellerKeysDecodeAsJSON(t *testing.T) {
	c := parse(t, tunnelled+`
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
	c = parse(t, tunnelled+"    seller_keys: { url: \"https://seller.example.com/.well-known/jwks.json\" }\n")
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

func TestParseAllowEntry(t *testing.T) {
	for in, want := range map[string]AllowEntry{
		"anonymous":                          {Kind: AllowAnonymous},
		"key:kk_acme_*":                      {Kind: AllowKey, Value: "kk_acme_*"},
		"key:kk_acme_abc12345":               {Kind: AllowKey, Value: "kk_acme_abc12345"},
		"org:default":                        {Kind: AllowOrg, Value: "default"},
		"oidc:sub:auth0|abc:de":              {Kind: AllowOIDCSub, Value: "auth0|abc:de"},
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
		"org:", "org:a_b", "oidc:sub:", "oidc:", "node:", "user:x", "anonymous:x"} {
		if got, err := ParseAllowEntry(in); err == nil {
			t.Errorf("ParseAllowEntry(%q) = %+v, want an error", in, got)
		}
	}
}

// Fail-closed keys this build ignores are reported as warnings, so an operator is not surprised.
func TestUnbuiltWarnings(t *testing.T) {
	c := parse(t, minimal+`
  remote:
    kind: a2a
    backend: { tunnel: {} }
    sealed: true
    seller_keys: { url: "https://seller.example.com/jwks.json" }
blobs: s3://kontra-parts
signing_key: file:./kontra.key
auth: { api_keys: true, oidc: { issuer: "https://login.example.com", audience: kontra } }
`)
	want := []string{"auth.oidc", "blobs", "signing_key", "services.remote.sealed", "services.remote.seller_keys", "services.remote: tunnel backends"}
	for _, w := range want {
		if !slices.ContainsFunc(c.Warnings, func(s string) bool { return strings.HasPrefix(s, w) }) {
			t.Errorf("no warning starting %q in %q", w, c.Warnings)
		}
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

// The schema's fixture is also a configuration the gate loads, with the values it says.
func TestTeamFixtureLoads(t *testing.T) {
	c, err := Load("../../spec/schema/examples/config.team.json")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(c.TLS.ACME.Domains, []string{"agents.acme.io"}) {
		t.Errorf("acme domains = %v", c.TLS.ACME.Domains)
	}
	if c.Services["recon"].Sealed.Mode != SealedRequired || c.Services["fs"].Sealed.Mode != SealedOptional || c.Services["triage"].Sealed.Mode != SealedOff {
		t.Errorf("sealed = %q, %q, %q", c.Services["recon"].Sealed.Mode, c.Services["fs"].Sealed.Mode, c.Services["triage"].Sealed.Mode)
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
