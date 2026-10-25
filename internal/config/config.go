// Package config is kontra.yaml: the gate's configuration (PRD §11, note 1 §6, spec/relay.md §17).
//
// Team gates load services: from the file at start and on SIGHUP; Cloud writes services into Postgres
// and leaves the block empty (spec/relay.md §13.5). The file's JSON Schema, kontra.sh/schema/config/v1,
// is published beside the other schemas under spec/schema/ with the first gate code (§17).
//
// Nothing a TaskLifecycle workflow branches on is read from here inside the workflow: the relevant values
// are snapshotted into its start input (spec/relay.md §14.1).
package config

import (
	"encoding/json"
	"time"

	"github.com/medmahmoudi26/kontra/internal/unimpl"
)

// Config is the whole of kontra.yaml.
type Config struct {
	Listen    string `yaml:"listen"`     // public listener, default ":8080"
	PublicURL string `yaml:"public_url"` // the URL callers use; the served card carries it (spec §3)
	// SigningKey is the card-signing key (file:… or a KMS reference). Unused in M1: cards are rewritten
	// but not signed until M3 (PRD §14).
	SigningKey string   `yaml:"signing_key"`
	TLS        TLS      `yaml:"tls"`
	Limits     Limits   `yaml:"limits"`
	Temporal   Temporal `yaml:"temporal"`
	Postgres   string   `yaml:"postgres"` // a postgres:// URL; the gate's tables are in schema kontra (D37)
	Blobs      string   `yaml:"blobs"`    // optional bucket; without it parts over 256 KiB are refused (§10.4)
	Auth       Auth     `yaml:"auth"`
	Metrics    Metrics  `yaml:"metrics"`

	Tenancy                  Tenancy  `yaml:"tenancy"`                    // "single" (default) or "multi" (§2.1)
	CallerMetadata           []string `yaml:"caller_metadata"`            // kontra. keys callers may write (§2.5)
	OperatorMetadataPrefixes []string `yaml:"operator_metadata_prefixes"` // §13.2
	Push                     Push     `yaml:"push"`
	MCP                      MCP      `yaml:"mcp"`
	Hooks                    Hooks    `yaml:"hooks"`
	DrainTimeout             Duration `yaml:"drain_timeout"` // §14.3, default 10m
	Tunnel                   Tunnel   `yaml:"tunnel"`        // spec/tunnel.md; M2

	Services map[string]Service `yaml:"services"`
}

// Tenancy selects the {svc} path shape (spec §2.1).
type Tenancy string

// Tenancy values.
const (
	TenancySingle Tenancy = "single" // /agents/{name}
	TenancyMulti  Tenancy = "multi"  // /agents/{tenant}/{name}
)

// TLS is how the gate terminates TLS itself (PRD §11, §12): ACME or a provided certificate. Off
// localhost one of the two is required.
type TLS struct {
	ACME *ACME  `yaml:"acme"`
	Cert string `yaml:"cert"`
	Key  string `yaml:"key"`
}

// ACME configures built-in certificate management (golang.org/x/crypto/acme/autocert).
type ACME struct {
	Email    string   `yaml:"email"`
	Domains  []string `yaml:"domains"`   // default: the host of public_url
	CacheDir string   `yaml:"cache_dir"` // M1: a directory cache, one replica (see the M1 plan's ACME note)
}

// Limits are the edge limits of spec §2.2 and the worker capacities of §14.2.
type Limits struct {
	Body           ByteSize `yaml:"body"`             // default 4 MiB
	InflightPerKey int      `yaml:"inflight_per_key"` // default 32
	RPSPerIP       float64  `yaml:"rps_per_ip"`       // default 50
	HeaderTimeout  Duration `yaml:"header_timeout"`   // default 10s
	Idle           Duration `yaml:"idle"`             // default 60s
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
	AllowPrivate bool `yaml:"allow_private"` // tests only (§10.3, D18)
}

// MCP holds the MCP relay settings (spec §11). M3.
type MCP struct {
	AllowedOrigins []string `yaml:"allowed_origins"`
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
	Timeout    Duration `yaml:"timeout"` // default 5s
	Operations []string `yaml:"operations"`
}

// LifecycleHook is the remote Lifecycle (spec §13.3): the five activities by type name on that queue. M5.
type LifecycleHook struct {
	TaskQueue string `yaml:"task_queue"`
}

// Tunnel holds the tunnel settings of spec/tunnel.md. M2.
type Tunnel struct {
	ChannelBinding   string `yaml:"channel_binding"`
	Anonymous        bool   `yaml:"anonymous"`
	EnrollWithAPIKey bool   `yaml:"enroll_with_api_key"`
}

// Service is one services.<name> entry (note 1 §6; spec §1, §17).
type Service struct {
	Kind         string          `yaml:"kind"` // "a2a" or "mcp", required
	Backend      Backend         `yaml:"backend"`
	Allow        []string        `yaml:"allow"` // deny by default (spec §2.3)
	Tools        *Tools          `yaml:"tools"` // mcp only (§11.3)
	Tasks        string          `yaml:"tasks"` // mcp only: durable (default) | passthrough | off (§11.5)
	Sealed       Sealed          `yaml:"sealed"`
	SellerKeys   json.RawMessage `yaml:"seller_keys"` // inline JWKS or {"url": …} (§3); M3
	SessionIdleS int             `yaml:"session_idle_s"`
	Task         TaskPolicy      `yaml:"task"`
	Listed       bool            `yaml:"listed"`
	Annotations  map[string]any  `yaml:"annotations"` // §13.4, never interpreted
}

// Backend is exactly one of tunnel, a2a {url}, mcp {url} (spec §1, §6.1).
type Backend struct {
	Tunnel *TunnelBackend `yaml:"tunnel"`
	A2A    *URLBackend    `yaml:"a2a"`
	MCP    *URLBackend    `yaml:"mcp"`
}

// TunnelBackend is a service hosted by nodes over the tunnel (M2).
type TunnelBackend struct{}

// URLBackend is a direct backend on the operator's own network, dialed through the SSRF-safe dialer
// (spec §10.3). AllowPrivate skips the address check, never the redirect rule.
type URLBackend struct {
	URL          string `yaml:"url"`
	AllowPrivate bool   `yaml:"allow_private"`
}

// Tools is the per-tool policy of an MCP service (spec §11.3).
type Tools struct {
	Allow []string `yaml:"allow"`
	Deny  []string `yaml:"deny"`
}

// Sealed is `false`, `true`, or `{required: bool}` (ext-sealed.md §1); it loads as off, required or optional.
type Sealed struct {
	Mode string // "off", "required", "optional"
}

// TaskPolicy is services.<name>.task (spec §5.4, §6.4, §6.5).
type TaskPolicy struct {
	Timeout   Duration `yaml:"timeout"`   // default 1h, wall clock from submission
	Retries   *int     `yaml:"retries"`   // default 2; nil means the default, 0 is honoured
	Retention Duration `yaml:"retention"` // default 7d
}

// Duration is a time.Duration that also accepts a d (days) suffix ("7d").
type Duration time.Duration

// UnmarshalText parses a Go duration string or "<n>d".
func (d *Duration) UnmarshalText(b []byte) error {
	return unimpl.Error("config.Duration.UnmarshalText", "WP-01")
}

// ByteSize is a size with an optional KiB/MiB/GiB suffix ("4MiB").
type ByteSize int64

// UnmarshalText parses a size with an optional binary suffix.
func (s *ByteSize) UnmarshalText(b []byte) error {
	return unimpl.Error("config.ByteSize.UnmarshalText", "WP-01")
}

// UnmarshalYAML accepts the three shapes of `sealed:`.
func (s *Sealed) UnmarshalYAML(unmarshal func(any) error) error {
	return unimpl.Error("config.Sealed.UnmarshalYAML", "WP-01")
}

// Load reads and validates kontra.yaml, applying the defaults below.
func Load(path string) (*Config, error) {
	return nil, unimpl.Error("config.Load", "WP-01")
}

// Validate checks what the schema cannot: exactly one backend per service, names matching
// ^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$, TLS required when public_url is not a localhost origin, kind-only
// keys, parseable allow: entries.
func (c *Config) Validate() error {
	return unimpl.Error("config.Config.Validate", "WP-01")
}
