package config

import (
	"fmt"
	"math"
	"net"
	"time"

	"go.yaml.in/yaml/v3"
)

// Profile is kontra.yaml's profile: absent (a team gate, as before) or "local", the local development profile
// (docs/local.md, spec/relay.md §18): plain HTTP on loopback, every client the shared anonymous caller, nodes
// served without enrollment, traffic recorded and the console served at /.
type Profile string

// ProfileLocal is the local development profile.
const ProfileLocal Profile = "local"

// The local development profile's listener defaults: loopback only.
const (
	LocalListen        = "127.0.0.1:8080"
	LocalMetricsListen = "127.0.0.1:9090"
)

// DefaultTrafficRetention is traffic.retention's default, the same as a service's task retention.
const DefaultTrafficRetention = DefaultTaskRetention

// Local reports whether the gate runs in the local development profile.
func (c *Config) Local() bool { return c.Profile == ProfileLocal }

// Traffic is the A2A traffic record (spec/relay.md §13.6): one metadata-only exchange per A2A call, in
// a2a_exchanges. Off unless set, on in the local development profile.
type Traffic struct {
	Record    *bool     `yaml:"record"`    // nil: the profile's default (on in local, off otherwise)
	Retention Retention `yaml:"retention"` // default 7d; 0 keeps exchanges forever
}

// Recording reports whether exchanges are recorded.
func (t Traffic) Recording() bool { return t.Record != nil && *t.Record }

// Retention is a retention period: a positive duration ("7d", "36h"), or 0, which keeps forever. The zero
// value of the type means absent (the default applies); Forever is the explicit 0.
type Retention time.Duration

// Forever is an explicit `retention: 0`.
const Forever Retention = -1

// D returns the period, or 0 when kept forever.
func (r Retention) D() time.Duration {
	if r < 0 {
		return 0
	}
	return time.Duration(r)
}

// IsForever reports whether the value is an explicit 0.
func (r Retention) IsForever() bool { return r == Forever }

// String formats the value: "0" for forever, else a Go duration.
func (r Retention) String() string {
	if r.IsForever() {
		return "0"
	}
	return time.Duration(r).String()
}

// UnmarshalYAML accepts 0 (keep forever) or a positive duration.
func (r *Retention) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return fmt.Errorf("line %d: retention: want 0 or a duration such as 7d", n.Line)
	}
	if n.Value == "0" {
		*r = Forever
		return nil
	}
	d, err := ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: retention: %v (or 0 to keep forever)", n.Line, err)
	}
	*r = Retention(d)
	return nil
}

// MarshalText formats the value as String does.
func (r Retention) MarshalText() ([]byte, error) { return []byte(r.String()), nil }

// profileOf reads the top-level profile key of a kontra.yaml document before it is decoded, so the profile's
// defaults can sit under the file's own keys.
func profileOf(top *yaml.Node) Profile {
	if top == nil || top.Kind != yaml.MappingNode {
		return ""
	}
	for i := 0; i+1 < len(top.Content); i += 2 {
		if top.Content[i].Value == "profile" && top.Content[i+1].Kind == yaml.ScalarNode {
			return Profile(top.Content[i+1].Value)
		}
	}
	return ""
}

// applyProfileDefaults replaces the defaults that differ in the profile; called before the file is decoded.
func (c *Config) applyProfileDefaults(p Profile) {
	if p != ProfileLocal {
		return
	}
	c.Listen = LocalListen
	c.Metrics.Listen = LocalMetricsListen
}

// applyLocalDerived fills the profile's defaults that depend on other keys: public_url from listen, traffic on.
func (c *Config) applyLocalDerived() {
	if c.Traffic.Retention == 0 {
		c.Traffic.Retention = Retention(DefaultTrafficRetention)
	}
	if !c.Local() {
		if c.Traffic.Record == nil {
			off := false
			c.Traffic.Record = &off
		}
		return
	}
	if c.Traffic.Record == nil {
		on := true
		c.Traffic.Record = &on
	}
	// Every client is the one anonymous caller, admitted to every service whatever its allow: (relay.md §18).
	for name, s := range c.Services {
		s.Allow = []string{string(AllowAnonymous)}
		c.Services[name] = s
	}
	if c.PublicURL == "" {
		if host, port, err := net.SplitHostPort(c.Listen); err == nil && host != "" {
			c.PublicURL = "http://" + net.JoinHostPort(host, port)
		}
	}
}

// loopbackListen reports whether addr binds a loopback address only: host is localhost or a loopback IP
// literal; an empty host, 0.0.0.0 and :: bind every interface.
func loopbackListen(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	return err == nil && host != "" && IsLocalhost(host)
}

func (c *Config) validateProfile(v *validator) {
	switch c.Profile {
	case "":
	case ProfileLocal:
		c.validateLocal(v)
	default:
		v.add("profile %q: want local, or no profile", c.Profile)
	}
	if r := c.Traffic.Retention; !r.IsForever() && (r.D() < time.Second || r.D()%time.Second != 0) {
		v.add("traffic.retention %s: want 0 (keep forever) or a whole number of seconds, at least 1s", r)
	}
}

func (c *Config) validateLocal(v *validator) {
	const why = "the local development profile binds loopback only (docs/local.md)"
	if !loopbackListen(c.Listen) {
		v.add("listen %q: %s; use 127.0.0.1:<port>, localhost:<port> or [::1]:<port>", c.Listen, why)
	}
	if !loopbackListen(c.Metrics.Listen) {
		v.add("metrics.listen %q: %s", c.Metrics.Listen, why)
	}
	if p := c.Peer; p != nil {
		if p.Listen != "" && !loopbackListen(p.Listen) {
			v.add("peer.listen %q: %s", p.Listen, why)
		}
		if p.Advertise != "" && !loopbackListen(p.Advertise) {
			v.add("peer.advertise %q: %s", p.Advertise, why)
		}
	}
	if u, err := parseURL(c.PublicURL); err == nil && (u.Scheme != "http" || !IsLocalhost(u.Hostname())) {
		v.add("public_url %q: must be an http:// loopback origin in the local development profile", redactURL(c.PublicURL))
	}
	if c.TLS.TerminatesTLS() || c.TLS.TerminatedByProxy {
		v.add("tls: the local development profile serves plain HTTP on loopback; remove tls")
	}
	for _, name := range sortedKeys(c.Services) {
		if c.Services[name].Sealed.Mode != SealedOff {
			v.add("services.%s.sealed: sealed services are not served in the local development profile", name)
		}
	}
	if c.Tunnel.Anonymous.Enabled {
		v.add("tunnel.anonymous: the local development profile serves nodes without enrollment on its own tenant; remove tunnel.anonymous")
	}
	if c.Tenancy != TenancySingle {
		v.add("tenancy %q: the local development profile is single-tenant", c.Tenancy)
	}
}

// retentionSecondsOK checks a task retention fits services.retention_s: whole seconds, 0 for forever.
func retentionSecondsOK(r Retention) error {
	if r.IsForever() {
		return nil
	}
	switch d := r.D(); {
	case d < time.Second || d%time.Second != 0:
		return fmt.Errorf("want 0 (keep forever) or a whole number of seconds, at least 1s")
	case d/time.Second > math.MaxInt32:
		return fmt.Errorf("at most %d seconds (stored as integer seconds)", math.MaxInt32)
	}
	return nil
}
