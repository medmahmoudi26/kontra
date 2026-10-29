package config

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Defaults from PRD §11 and spec/relay.md §2.2, §5.4, §6.4–§6.6, §11.2, §13.1, §14.2–§14.3, §17, and D73.
const (
	DefaultListen         = ":8080"
	DefaultMetricsListen  = ":9090"
	DefaultBody           = 4 << 20
	DefaultInflightPerKey = 32
	DefaultRPSPerIP       = 50
	DefaultHeaderTimeout  = 10 * time.Second
	DefaultIdle           = 60 * time.Second
	DefaultConnsPerIP     = 256   // D73
	DefaultMaxConns       = 10000 // D73
	DefaultDispatchSlots  = 10000
	DefaultPushSlots      = 2000
	DefaultTaskBytes      = 256 << 20
	DefaultTemporalAddr   = "localhost:7233"
	DefaultNamespace      = "kontra"
	DefaultTaskTimeout    = time.Hour
	DefaultTaskRetries    = 2
	MaxTaskRetries        = 10 // services.retries CHECK in 0001
	DefaultTaskRetention  = 7 * 24 * time.Hour
	DefaultSessionIdle    = 3600 // seconds
	DefaultDrainTimeout   = 10 * time.Minute
	DefaultAdmissionWait  = 5 * time.Second
	DefaultTasksMode      = TasksDurable

	// DefaultMaxSessionsPerPrincipal is services.<name>.max_sessions_per_principal's default (spec §11.2, D213, D231:
	// half the stdio bridge's default --max-sessions, so that one principal keeping its sessions in use leaves another
	// room on a default bridge), and MaxSessionsPerPrincipal its bound (services.max_sessions_per_principal CHECK in
	// 0002).
	DefaultMaxSessionsPerPrincipal = 4
	MaxSessionsPerPrincipal        = 10000
)

// Defaults returns a configuration holding every default that does not depend on another key. Parse
// decodes the file over it, so a key present in the file always wins and an explicit zero is seen (and
// refused) rather than silently replaced.
func Defaults() *Config {
	return &Config{
		Listen: DefaultListen,
		Limits: Limits{
			Body:           DefaultBody,
			InflightPerKey: DefaultInflightPerKey,
			RPSPerIP:       DefaultRPSPerIP,
			HeaderTimeout:  Duration(DefaultHeaderTimeout),
			Idle:           Duration(DefaultIdle),
			ConnsPerIP:     DefaultConnsPerIP,
			MaxConns:       DefaultMaxConns,
			DispatchSlots:  DefaultDispatchSlots,
			PushSlots:      DefaultPushSlots,
			TaskBytes:      DefaultTaskBytes,
		},
		Temporal:     Temporal{Address: DefaultTemporalAddr, Namespace: DefaultNamespace},
		Metrics:      Metrics{Listen: DefaultMetricsListen},
		Tenancy:      TenancySingle,
		Edge:         Edge{ClientIP: ClientIP{From: ClientIPRemote}},
		DrainTimeout: Duration(DefaultDrainTimeout),
	}
}

// applyDerivedDefaults fills the defaults of keys inside optional blocks and map entries, and the ones
// that depend on other keys. Durations, sizes and Seconds refuse zero when decoded, so zero here means
// absent.
func (c *Config) applyDerivedDefaults() {
	// public_url is kept without a trailing slash, so {svc} is public_url + "/agents/…".
	if i := strings.Index(c.PublicURL, "://"); i >= 0 {
		c.PublicURL = c.PublicURL[:i+3] + strings.TrimRight(c.PublicURL[i+3:], "/")
	}
	if c.CallerMetadata == nil {
		c.CallerMetadata = []string{}
	}
	if c.OperatorMetadataPrefixes == nil {
		c.OperatorMetadataPrefixes = []string{}
	}
	if c.MCP.AllowedOrigins == nil {
		c.MCP.AllowedOrigins = []string{}
	}
	if c.Edge.ClientIP.From == "" {
		c.Edge.ClientIP.From = ClientIPRemote
	}
	if a := c.TLS.ACME; a != nil {
		if len(a.Domains) == 0 {
			if u, err := url.Parse(c.PublicURL); err == nil && u.Hostname() != "" {
				a.Domains = []string{u.Hostname()}
			}
		}
		for i, d := range a.Domains {
			a.Domains[i] = strings.ToLower(strings.TrimSuffix(d, "."))
		}
		if a.CacheDir == "" {
			if d, err := os.UserCacheDir(); err == nil {
				a.CacheDir = filepath.Join(d, "kontra-gate", "acme")
			}
		}
	}
	if h := c.Hooks.Admission; h != nil {
		if h.Timeout == 0 {
			h.Timeout = Duration(DefaultAdmissionWait)
		}
		if len(h.Operations) == 0 {
			h.Operations = []string{"task"}
		}
	}
	if c.Tunnel.ChannelBinding == "" {
		c.Tunnel.ChannelBinding = ChannelBindingNone
		if c.TLS.TerminatesTLS() {
			c.Tunnel.ChannelBinding = ChannelBindingTLSExporter
		}
	}
	if c.Services == nil {
		c.Services = map[string]Service{}
	}
	for name, s := range c.Services {
		if s.Sealed.Mode == "" {
			s.Sealed.Mode = SealedOff
		}
		if s.SessionIdle == 0 {
			s.SessionIdle = DefaultSessionIdle
		}
		if s.Task.Timeout == 0 {
			s.Task.Timeout = Duration(DefaultTaskTimeout)
		}
		if s.Task.Retries == nil {
			r := DefaultTaskRetries
			s.Task.Retries = &r
		}
		if s.Task.Retention == 0 {
			s.Task.Retention = Duration(DefaultTaskRetention)
		}
		if s.Kind == KindMCP && s.Tasks == "" {
			s.Tasks = DefaultTasksMode
		}
		if s.Allow == nil {
			s.Allow = []string{}
		}
		if s.Annotations.IsZero() {
			s.Annotations = JSON("{}")
		}
		c.Services[name] = s
	}
}
