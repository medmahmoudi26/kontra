// Package config is kontra.yaml: the gate's configuration (PRD §11, note 1 §6, spec/relay.md §17).
//
// Team gates load services: from the file at start and on SIGHUP; Cloud writes services into Postgres
// and leaves the block empty (spec/relay.md §13.5). The file's JSON Schema, kontra.sh/schema/config/v1,
// is published beside the other schemas as spec/schema/config.json (§17); a test holds the two in step.
//
// Loading is strict: an unknown key is an error (a misspelt allow: would otherwise deny silently), every
// default of PRD §11 and relay §17 is applied, and Validate checks what the schema cannot. Durations and
// sizes in the file are positive: "0s" and "0MiB" are refused when decoded, so a zero value after
// decoding always means "absent, use the default".
//
// Nothing a TaskLifecycle workflow branches on is read from here inside the workflow: the relevant values
// are snapshotted into its start input (spec/relay.md §14.1).
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Config is the whole of kontra.yaml.
type Config struct {
	Listen    string `yaml:"listen"`     // public listener, default ":8080"
	PublicURL string `yaml:"public_url"` // the URL callers use; the served card carries it (spec §3)
	// SigningKey is the card-signing key: file:<path> of a PEM file of ECDSA P-256 private keys, the first signs
	// and every key is in the JWKS (spec §3, D166). Required off loopback when A2A services are configured.
	SigningKey string   `yaml:"signing_key"`
	TLS        TLS      `yaml:"tls"`
	Edge       Edge     `yaml:"edge"`
	Limits     Limits   `yaml:"limits"`
	Temporal   Temporal `yaml:"temporal"`
	Postgres   string   `yaml:"postgres"` // a postgres:// URL; the gate's tables are in schema kontra (D37)
	Blobs      string   `yaml:"blobs"`    // optional bucket (s3://…); without it parts over 256 KiB are refused (§10.4)
	Auth       Auth     `yaml:"auth"`
	Metrics    Metrics  `yaml:"metrics"`

	Tenancy                  Tenancy  `yaml:"tenancy"`                    // "single" (default) or "multi" (§2.1)
	CallerMetadata           []string `yaml:"caller_metadata"`            // kontra. keys callers may write (§2.5)
	OperatorMetadataPrefixes []string `yaml:"operator_metadata_prefixes"` // §13.2
	Push                     Push     `yaml:"push"`
	MCP                      MCP      `yaml:"mcp"`
	Peer                     *Peer    `yaml:"peer"` // spec §11.2, D163; M3 WP-04
	Hooks                    Hooks    `yaml:"hooks"`
	DrainTimeout             Duration `yaml:"drain_timeout"` // §14.3 and D76, default 10m
	Tunnel                   Tunnel   `yaml:"tunnel"`        // spec/tunnel.md; M2

	Services map[string]Service `yaml:"services"`

	// Warnings are facts about this configuration the operator should see in the log at start: a gate
	// behind a TLS-terminating proxy (D69), and keys this build accepts but does not act on yet. Filled by
	// Validate; never read from the file.
	Warnings []string `yaml:"-"`
}

// Tenancy selects the {svc} path shape (spec §2.1).
type Tenancy string

// Tenancy values.
const (
	TenancySingle Tenancy = "single" // /agents/{name}
	TenancyMulti  Tenancy = "multi"  // /agents/{tenant}/{name}
)

// TLS is how TLS reaches the gate's public listener (PRD §11, §12): the gate terminates it itself with
// ACME or a provided certificate, or a front proxy terminates it (D69). Off localhost one of the three
// is required, and they are exclusive.
type TLS struct {
	ACME *ACME  `yaml:"acme"`
	Cert string `yaml:"cert"` // PEM chain; a relative path is relative to the configuration file
	Key  string `yaml:"key"`  // PEM key; same
	// TerminatedByProxy lets the gate listen in plaintext with an https public_url because a front proxy
	// terminates TLS (D69): HSTS is left to the proxy, the tunnel's channel binding is none, and a warning
	// is logged at start.
	TerminatedByProxy bool `yaml:"terminated_by_proxy"`
}

// TerminatesTLS reports whether the gate itself terminates TLS (ACME or a provided certificate).
func (t TLS) TerminatesTLS() bool { return t.ACME != nil || t.Cert != "" }

// ACME configures built-in certificate management (golang.org/x/crypto/acme/autocert, HTTP-01 and
// TLS-ALPN-01). Built-in ACME serves one replica with a directory cache (D70): several replicas use
// provided certificates or a front proxy, because an unshared cache lets each replica order its own
// certificate and a challenge can land on the wrong replica.
type ACME struct {
	Email    string   `yaml:"email"`
	Domains  []string `yaml:"domains"`   // default: the host of public_url; must include it
	CacheDir string   `yaml:"cache_dir"` // default: <user cache dir>/kontra-gate/acme; relative to the file
}

// Edge configures where the public listener takes a client's address from (D69), for rps_per_ip and
// conns_per_ip. The default, remote, is the TCP peer.
type Edge struct {
	ClientIP ClientIP `yaml:"client_ip"`
}

// ClientIP is edge.client_ip (D69).
type ClientIP struct {
	// From is remote (default), proxy_protocol (PROXY v1/v2, accepted only from a trusted peer) or
	// x_forwarded_for (only from a trusted peer; the client is the rightmost untrusted hop).
	From ClientIPSource `yaml:"from"`
	// Trusted lists the proxies' CIDRs (a bare address is a single-host prefix). Required unless From is
	// remote, and only meaningful then.
	Trusted []string `yaml:"trusted"`
	// TrustedPrefixes is Trusted parsed by Validate.
	TrustedPrefixes []netip.Prefix `yaml:"-"`
}

// ClientIPSource is edge.client_ip.from.
type ClientIPSource string

// Client address sources (D69).
const (
	ClientIPRemote        ClientIPSource = "remote"
	ClientIPProxyProtocol ClientIPSource = "proxy_protocol"
	ClientIPXForwardedFor ClientIPSource = "x_forwarded_for"
)

// Limits are the edge limits of spec §2.2 and D73, and the worker capacities of §14.2.
type Limits struct {
	Body           ByteSize `yaml:"body"`             // default 4 MiB
	InflightPerKey int      `yaml:"inflight_per_key"` // default 32
	RPSPerIP       float64  `yaml:"rps_per_ip"`       // default 50
	HeaderTimeout  Duration `yaml:"header_timeout"`   // default 10s
	Idle           Duration `yaml:"idle"`             // default 60s
	ConnsPerIP     int      `yaml:"conns_per_ip"`     // default 256, enforced by the listener (D73)
	MaxConns       int      `yaml:"max_conns"`        // default 10000, enforced by the listener (D73)
	DispatchSlots  int      `yaml:"dispatch_slots"`   // default 10000 (§14.2)
	PushSlots      int      `yaml:"push_slots"`       // default 2000 (§14.2)
	TaskBytes      ByteSize `yaml:"task_bytes"`       // default 256 MiB (§6.6)
}

// Temporal is the gate's own namespace (spec §14.1, §14.4).
type Temporal struct {
	Address   string `yaml:"address"`   // default localhost:7233
	Namespace string `yaml:"namespace"` // default kontra
}

// Auth selects the caller credentials the gate accepts (spec §2.3, §12.1).
type Auth struct {
	APIKeys   bool       `yaml:"api_keys"`
	OIDC      *OIDC      `yaml:"oidc"`
	MCPOAuth  *MCPOAuth  `yaml:"mcp_oauth"`
	Anonymous *Anonymous `yaml:"anonymous"`
}

// OIDC is the operator's IdP for A2A routes (spec §2.3). Not built in M1.
type OIDC struct {
	Issuer   string `yaml:"issuer"`
	Audience string `yaml:"audience"`
}

// MCPOAuth is the MCP authorization server the gate is a resource server for (spec §12.1). M3.
type MCPOAuth struct {
	Issuer string `yaml:"issuer"`
}

// Anonymous is reserved for gate-wide anonymous settings; per service, anonymous is an allow: entry.
type Anonymous struct{}

// Metrics is the separate metrics listener (spec §2.1).
type Metrics struct {
	Listen string `yaml:"listen"` // default ":9090"
}

// Push holds push delivery settings (spec §10).
type Push struct {
	// AllowPrivate skips the SSRF address check for webhook URLs and permits http:// (D67). Tests only
	// (§10.3, D18).
	AllowPrivate bool `yaml:"allow_private"`
}

// MCP holds the MCP relay settings (spec §11). M3.
type MCP struct {
	AllowedOrigins []string `yaml:"allowed_origins"`
}

// Peer is the replica-to-replica hop for MCP requests whose tunnel session another replica holds (spec
// §11.2, D163). Absent, only this replica's tunnel sessions serve MCP requests, which is all one replica needs.
// M3 WP-04 validates and serves it.
type Peer struct {
	Listen    string `yaml:"listen"`     // host:port or :port; never the public listener
	Advertise string `yaml:"advertise"`  // what other replicas dial; default Listen, or the default route's address
	SecretEnv string `yaml:"secret_env"` // the variable holding the shared HMAC secret, at least 32 bytes
}

// Hooks selects the built-in or remote hook implementations (spec §13, D26).
type Hooks struct {
	Admission *AdmissionHook `yaml:"admission"` // nil: built in
	Lifecycle *LifecycleHook `yaml:"lifecycle"` // nil: built in
}

// AdmissionHook is the remote Admission (spec §13.1). M5.
type AdmissionHook struct {
	URL        string   `yaml:"url"`
	SecretEnv  string   `yaml:"secret_env"`
	Timeout    Duration `yaml:"timeout"`    // default 5s
	Operations []string `yaml:"operations"` // default [task]
}

// LifecycleHook is the remote Lifecycle (spec §13.3): the five activities by type name on that queue. Accepted
// since M3 (D186); the remote Admission hook stays refused until M5.
type LifecycleHook struct {
	TaskQueue string `yaml:"task_queue"`
}

// Tunnel holds the tunnel settings of spec/tunnel.md. M2.
type Tunnel struct {
	// ChannelBinding is tls-exporter or none; the default is tls-exporter when the gate terminates TLS
	// itself and none otherwise (tunnel.md §4, D69).
	ChannelBinding   string `yaml:"channel_binding"`
	Anonymous        bool   `yaml:"anonymous"`
	EnrollWithAPIKey bool   `yaml:"enroll_with_api_key"`
}

// Channel binding values (spec/tunnel.md §4).
const (
	ChannelBindingTLSExporter = "tls-exporter"
	ChannelBindingNone        = "none"
)

// Service is one services.<name> entry (note 1 §6; spec §1, §17).
type Service struct {
	Kind        string     `yaml:"kind"` // "a2a" or "mcp", required
	Backend     Backend    `yaml:"backend"`
	Allow       []string   `yaml:"allow"` // deny by default (spec §2.3); grammar in ParseAllowEntry
	Tools       *Tools     `yaml:"tools"` // mcp only (§11.3)
	Tasks       string     `yaml:"tasks"` // mcp only: durable (default) | passthrough | off (§11.5)
	Sealed      Sealed     `yaml:"sealed"`
	SellerKeys  JSON       `yaml:"seller_keys"` // inline JWKS {"keys": […]} or {"url": "https://…"} (§3); M3
	SessionIdle Seconds    `yaml:"session_idle_s"`
	Task        TaskPolicy `yaml:"task"`
	Listed      bool       `yaml:"listed"`
	Annotations JSON       `yaml:"annotations"` // a JSON object (§13.4), never interpreted; default {}

	// MaxSessionsPerPrincipal caps the open MCP sessions one principal other than anonymous holds on a tunnel
	// service (mcp only, spec §11.2, D213, D231): an initialize beyond it closes the principal's sessions idle for 5
	// minutes, the longest idle first, or is refused 429 when too few are. nil is DefaultMaxSessionsPerPrincipal.
	MaxSessionsPerPrincipal *int `yaml:"max_sessions_per_principal"`
}

// SessionsPerPrincipal is max_sessions_per_principal, its default when unset.
func (s Service) SessionsPerPrincipal() int {
	if s.MaxSessionsPerPrincipal == nil {
		return DefaultMaxSessionsPerPrincipal
	}
	return *s.MaxSessionsPerPrincipal
}

// Kinds of service.
const (
	KindA2A = "a2a"
	KindMCP = "mcp"
)

// MCP task modes (spec §11.5).
const (
	TasksDurable     = "durable"
	TasksPassthrough = "passthrough"
	TasksOff         = "off"
)

// Backend is exactly one of tunnel, a2a {url}, mcp {url} (spec §1, §6.1).
type Backend struct {
	Tunnel *TunnelBackend `yaml:"tunnel"`
	A2A    *URLBackend    `yaml:"a2a"`
	MCP    *URLBackend    `yaml:"mcp"`
}

// Direct returns the URL backend, or nil for a tunnel backend.
func (b Backend) Direct() *URLBackend {
	if b.A2A != nil {
		return b.A2A
	}
	return b.MCP
}

// TunnelBackend is a service hosted by nodes over the tunnel (M2).
type TunnelBackend struct{}

// URLBackend is a direct backend on the operator's own network, dialed through the SSRF-safe dialer
// (spec §10.3). URL is the base (D71): the card is fetched relative to its path, and the gate dials only
// its scheme://host:port. AllowPrivate skips the address check and permits http:// (D67), never the
// redirect rule.
type URLBackend struct {
	URL          string `yaml:"url"`
	AllowPrivate bool   `yaml:"allow_private"`
}

// Tools is the per-tool policy of an MCP service (spec §11.3).
type Tools struct {
	Allow []string `yaml:"allow"`
	Deny  []string `yaml:"deny"`
}

// SealedMode is what `sealed:` loads as (ext-sealed.md §1; services.sealed in 0001).
type SealedMode string

// Sealed modes.
const (
	SealedOff      SealedMode = "off"
	SealedRequired SealedMode = "required"
	SealedOptional SealedMode = "optional"
)

// Sealed is `false`, `true`, or `{required: bool}` (ext-sealed.md §1); it loads as off, required or
// optional. `{}` is `{required: true}`.
type Sealed struct {
	Mode SealedMode
}

// TaskPolicy is services.<name>.task (spec §5.4, §6.4, §6.5).
type TaskPolicy struct {
	Timeout   Duration `yaml:"timeout"`   // default 1h, wall clock from submission
	Retries   *int     `yaml:"retries"`   // default 2; nil means the default, 0 is honoured; at most 10
	Retention Duration `yaml:"retention"` // default 7d
}

// RetryCount is the number of retries, the default applied.
func (p TaskPolicy) RetryCount() int {
	if p.Retries == nil {
		return DefaultTaskRetries
	}
	return *p.Retries
}

// Duration is a positive time.Duration that also accepts a d (days) suffix ("7d", "1d12h").
type Duration time.Duration

// D returns the value as a time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// String formats the value as a Go duration.
func (d Duration) String() string { return time.Duration(d).String() }

// UnmarshalText parses a Go duration string or "<n>d". Zero, negative and unit-less values are refused.
func (d *Duration) UnmarshalText(b []byte) error {
	v, err := ParseDuration(string(b))
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// MarshalText formats the value as a Go duration.
func (d Duration) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

// ParseDuration parses a positive duration: a sequence of <number><unit> with the units of
// time.ParseDuration plus d (24 h), e.g. "90s", "1h30m", "7d", "1.5d".
func ParseDuration(s string) (time.Duration, error) {
	orig := s
	if s == "" {
		return 0, errors.New("duration: empty")
	}
	var total time.Duration
	for s != "" {
		i := 0
		for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.') {
			i++
		}
		j := i
		for j < len(s) && !(s[j] >= '0' && s[j] <= '9' || s[j] == '.') {
			j++
		}
		num, unit := s[:i], s[i:j]
		if num == "" {
			return 0, fmt.Errorf("duration %q: want a number followed by a unit (ns, us, ms, s, m, h, d)", orig)
		}
		if unit == "" {
			return 0, fmt.Errorf("duration %q: missing unit (ns, us, ms, s, m, h, d)", orig)
		}
		var part time.Duration
		if unit == "d" {
			f, err := strconv.ParseFloat(num, 64)
			if err != nil || f*float64(24*time.Hour) >= math.MaxInt64 {
				return 0, fmt.Errorf("duration %q: bad number of days", orig)
			}
			part = time.Duration(f * float64(24*time.Hour))
		} else {
			p, err := time.ParseDuration(num + unit)
			if err != nil {
				return 0, fmt.Errorf("duration %q: %v", orig, strings.TrimPrefix(err.Error(), "time: "))
			}
			part = p
		}
		if total+part < total {
			return 0, fmt.Errorf("duration %q: too large", orig)
		}
		total += part
		s = s[j:]
	}
	if total <= 0 {
		return 0, fmt.Errorf("duration %q: must be positive", orig)
	}
	return total, nil
}

// ByteSize is a positive size in bytes, written as an integer or with a binary suffix ("4MiB").
type ByteSize int64

// Int64 returns the size in bytes.
func (s ByteSize) Int64() int64 { return int64(s) }

// UnmarshalText parses a size with an optional B, KiB, MiB, GiB or TiB suffix. Decimal suffixes (KB, MB)
// and fractions are refused, so a size never means two things.
func (s *ByteSize) UnmarshalText(b []byte) error {
	v, err := ParseByteSize(string(b))
	if err != nil {
		return err
	}
	*s = ByteSize(v)
	return nil
}

// MarshalText formats the size in bytes.
func (s ByteSize) MarshalText() ([]byte, error) { return []byte(strconv.FormatInt(int64(s), 10)), nil }

var byteSizeRE = regexp.MustCompile(`^([0-9]+) ?([A-Za-z]*)$`)

var byteUnits = map[string]int64{"": 1, "B": 1, "KiB": 1 << 10, "MiB": 1 << 20, "GiB": 1 << 30, "TiB": 1 << 40}

// ParseByteSize parses a positive size: "4194304", "4MiB", "512 KiB".
func ParseByteSize(s string) (int64, error) {
	m := byteSizeRE.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("size %q: want an integer with an optional B, KiB, MiB, GiB or TiB suffix", s)
	}
	mult, ok := byteUnits[m[2]]
	if !ok {
		return 0, fmt.Errorf("size %q: unknown unit %q (binary units only: B, KiB, MiB, GiB, TiB)", s, m[2])
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil || n > math.MaxInt64/mult {
		return 0, fmt.Errorf("size %q: too large", s)
	}
	if n == 0 {
		return 0, fmt.Errorf("size %q: must be positive", s)
	}
	return n * mult, nil
}

// Seconds is a positive whole number of seconds (session_idle_s).
type Seconds int

// UnmarshalText parses a positive integer.
func (s *Seconds) UnmarshalText(b []byte) error {
	n, err := strconv.Atoi(string(b))
	if err != nil || n <= 0 {
		return fmt.Errorf("%q: want a positive whole number of seconds", string(b))
	}
	*s = Seconds(n)
	return nil
}

// D returns the value as a time.Duration.
func (s Seconds) D() time.Duration { return time.Duration(s) * time.Second }

// UnmarshalYAML accepts the three shapes of `sealed:`.
func (s *Sealed) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		if n.ShortTag() != "!!bool" {
			break
		}
		var b bool
		if err := n.Decode(&b); err != nil {
			return err
		}
		s.Mode = SealedOff
		if b {
			s.Mode = SealedRequired
		}
		return nil
	case yaml.MappingNode:
		required := true
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			if k.Value != "required" || v.Kind != yaml.ScalarNode || v.ShortTag() != "!!bool" {
				return fmt.Errorf("line %d: sealed: want true, false or {required: true|false}", k.Line)
			}
			if err := v.Decode(&required); err != nil {
				return err
			}
		}
		s.Mode = SealedOptional
		if required {
			s.Mode = SealedRequired
		}
		return nil
	}
	return fmt.Errorf("line %d: sealed: want true, false or {required: true|false}", n.Line)
}

// JSON is a YAML value carried as JSON (seller_keys, annotations): the gate stores it in a jsonb column
// and never interprets it here. A mapping key must be a string; a YAML alias, merge key, NaN or infinity
// has no JSON form and is refused.
type JSON json.RawMessage

// UnmarshalYAML converts the node to JSON.
func (j *JSON) UnmarshalYAML(n *yaml.Node) error {
	v, err := nodeToJSON(n)
	if err != nil {
		return err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	*j = b
	return nil
}

// MarshalJSON returns the value, or null when absent.
func (j JSON) MarshalJSON() ([]byte, error) {
	if len(j) == 0 {
		return []byte("null"), nil
	}
	return j, nil
}

// IsZero reports whether the key was absent.
func (j JSON) IsZero() bool { return len(j) == 0 }

func nodeToJSON(n *yaml.Node) (any, error) {
	switch n.Kind {
	case yaml.DocumentNode:
		if len(n.Content) != 1 {
			return nil, fmt.Errorf("line %d: want one value", n.Line)
		}
		return nodeToJSON(n.Content[0])
	case yaml.AliasNode:
		return nil, fmt.Errorf("line %d: YAML aliases are not supported here", n.Line)
	case yaml.MappingNode:
		m := make(map[string]any, len(n.Content)/2)
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i]
			if k.Kind != yaml.ScalarNode || k.ShortTag() == "!!merge" {
				return nil, fmt.Errorf("line %d: a mapping key must be a plain string", k.Line)
			}
			if _, dup := m[k.Value]; dup {
				return nil, fmt.Errorf("line %d: duplicate key %q", k.Line, k.Value)
			}
			v, err := nodeToJSON(n.Content[i+1])
			if err != nil {
				return nil, err
			}
			m[k.Value] = v
		}
		return m, nil
	case yaml.SequenceNode:
		a := make([]any, 0, len(n.Content))
		for _, c := range n.Content {
			v, err := nodeToJSON(c)
			if err != nil {
				return nil, err
			}
			a = append(a, v)
		}
		return a, nil
	case yaml.ScalarNode:
		switch n.ShortTag() {
		case "!!null":
			return nil, nil
		case "!!bool":
			var b bool
			err := n.Decode(&b)
			return b, err
		case "!!int":
			var i int64
			if err := n.Decode(&i); err != nil {
				return nil, err
			}
			return json.Number(strconv.FormatInt(i, 10)), nil
		case "!!float":
			var f float64
			if err := n.Decode(&f); err != nil {
				return nil, err
			}
			if math.IsInf(f, 0) || math.IsNaN(f) {
				return nil, fmt.Errorf("line %d: %s has no JSON form", n.Line, n.Value)
			}
			return f, nil
		default: // !!str, !!timestamp, !!binary and custom tags keep their text
			return n.Value, nil
		}
	}
	return nil, fmt.Errorf("line %d: unsupported YAML value", n.Line)
}

// Option changes a configuration after it is decoded and before defaults and validation, e.g. the
// per-replica overrides of `kontra-gate serve`.
type Option func(*Config)

// WithListen overrides listen.
func WithListen(addr string) Option { return func(c *Config) { c.Listen = addr } }

// WithMetricsListen overrides metrics.listen.
func WithMetricsListen(addr string) Option { return func(c *Config) { c.Metrics.Listen = addr } }

// WithPeerListen overrides peer.listen (spec §11.2), so the replicas of one host can share one kontra.yaml. A file
// without peer: gets one with this listen only, which validation refuses until it names peer.secret_env.
func WithPeerListen(addr string) Option {
	return func(c *Config) {
		if c.Peer == nil {
			c.Peer = &Peer{}
		}
		c.Peer.Listen = addr
	}
}

// Load reads and validates kontra.yaml, applying the defaults. Relative file paths in it (tls.cert,
// tls.key, tls.acme.cache_dir, signing_key file:…) are relative to the file's directory.
func Load(path string, opts ...Option) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	c, err := Parse(b, filepath.Dir(abs), opts...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// Parse decodes and validates one kontra.yaml document. baseDir resolves relative file paths; empty
// leaves them as written.
func Parse(b []byte, baseDir string, opts ...Option) (*Config, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(b, &root); err != nil {
		return nil, err
	}
	if root.Kind == 0 || len(root.Content) == 0 {
		return nil, errors.New("empty configuration")
	}
	if top := root.Content[0]; top.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(top.Content); i += 2 {
			if top.Content[i].Value == "agents" {
				return nil, fmt.Errorf("line %d: agents: is now services:, each entry with a kind (note 1 §6)", top.Content[i].Line)
			}
		}
	}

	c := Defaults()
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(c); err != nil {
		return nil, err
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, errors.New("want one YAML document")
	}
	for _, o := range opts {
		o(c)
	}
	c.applyDerivedDefaults()
	if baseDir != "" {
		c.resolvePaths(baseDir)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// resolvePaths makes the file's relative paths relative to baseDir.
func (c *Config) resolvePaths(baseDir string) {
	abs := func(p string) string {
		if p == "" || filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(baseDir, p)
	}
	c.TLS.Cert, c.TLS.Key = abs(c.TLS.Cert), abs(c.TLS.Key)
	if c.TLS.ACME != nil {
		c.TLS.ACME.CacheDir = abs(c.TLS.ACME.CacheDir)
	}
	if p, ok := strings.CutPrefix(c.SigningKey, "file:"); ok {
		c.SigningKey = "file:" + abs(p)
	}
}

// ValidationError lists every problem Validate found, one per line.
type ValidationError struct {
	Problems []string
}

func (e *ValidationError) Error() string {
	if len(e.Problems) == 1 {
		return e.Problems[0]
	}
	return fmt.Sprintf("%d problems:\n  %s", len(e.Problems), strings.Join(e.Problems, "\n  "))
}

// Validate checks what the schema cannot: exactly one backend per service and of the service's kind,
// names matching ^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$, TLS required when public_url is not a localhost
// origin (or terminated by a declared front proxy, D69), https backend URLs unless allow_private (D67),
// URL ports from 1 to 65535, kind-only keys, allow: entries that can match (D68), durations and seconds
// that fit the integer columns of services, listen and metrics.listen on different ports, a signing_key
// off loopback when A2A services are configured (D166), and no key this build would ignore by failing open
// (sealed on a direct backend, the remote hooks). Errors quote URLs without their userinfo. It also fills Warnings.
// Defaults must have been applied (Parse and Load do).
func (c *Config) Validate() error {
	v := &validator{}
	c.Warnings = nil
	c.validateListeners(v)
	pub := c.validatePublicURL(v)
	c.validateTLS(v, pub)
	c.validateSigningKey(v, pub)
	c.validateEdge(v)
	c.validateLimits(v)
	c.validateBackends(v)
	c.validateAuth(v)
	c.validateMetadata(v)
	c.validateHooks(v)
	c.validateTunnel(v)
	for _, name := range sortedKeys(c.Services) {
		c.validateService(v, name, c.Services[name])
	}
	c.Warnings = append(c.Warnings, c.unbuilt()...)
	if len(v.problems) > 0 {
		return &ValidationError{Problems: v.problems}
	}
	return nil
}

type validator struct{ problems []string }

func (v *validator) add(format string, args ...any) {
	v.problems = append(v.problems, fmt.Sprintf(format, args...))
}

// NameRE is the shape of service and tenant names (spec §2.1; 0001 CHECKs).
var NameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
