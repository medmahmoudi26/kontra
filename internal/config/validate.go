package config

import (
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"kontra.sh/kontra/internal/pg"
)

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// checkHostPort reports whether addr is host:port with a numeric port (the host may be empty).
func checkHostPort(addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 0 || n > 65535 {
		return fmt.Errorf("port %q is not a number from 0 to 65535", port)
	}
	return nil
}

// checkURLPort checks the port of a URL the gate dials or publishes: absent, or 1 to 65535 (url.Parse
// checks only that it is digits).
func checkURLPort(u *url.URL) error {
	p := u.Port()
	if p == "" {
		return nil
	}
	if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("port %q is not a number from 1 to 65535", p)
	}
	return nil
}

// parseURL is url.Parse with an error that does not repeat the URL (url.Error quotes it whole,
// credentials included).
func parseURL(s string) (*url.URL, error) {
	u, err := url.Parse(s)
	if ue, ok := err.(*url.Error); ok {
		return nil, ue.Err
	}
	return u, err
}

// redactURL is s as a validation error may quote it: with any userinfo replaced by "xxxxx". Load errors
// reach stderr and the journal, and a URL the gate refuses for carrying credentials must not print
// them. A string that does not parse as a URL with an authority (no scheme, a bad port) loses everything
// between "://" (or the start) and the authority's last "@".
func redactURL(s string) string {
	if u, err := url.Parse(s); err == nil && !strings.Contains(u.Opaque, "@") {
		if u.User == nil {
			return s
		}
		u.User = url.User("xxxxx")
		return u.String()
	}
	scheme, rest, ok := strings.Cut(s, "://")
	if !ok {
		scheme, rest = "", s
	} else {
		scheme += "://"
	}
	authority := rest
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		authority = rest[:i]
	}
	if i := strings.LastIndex(authority, "@"); i >= 0 {
		return scheme + "xxxxx@" + rest[i+1:]
	}
	return s
}

// listenersCollide reports whether two listen addresses (both valid host:port) bind the same port: the
// same port other than 0 (each :0 gets its own), and either host is a wildcard ("", 0.0.0.0 or ::, which
// Go binds dual-stack) or both name one address (localhost is 127.0.0.1 and ::1).
func listenersCollide(a, b string) bool {
	ha, pa, err1 := net.SplitHostPort(a)
	hb, pb, err2 := net.SplitHostPort(b)
	na, err3 := strconv.Atoi(pa)
	nb, err4 := strconv.Atoi(pb)
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil || na != nb || na == 0 {
		return false
	}
	addrs := func(h string) (set []string, wildcard bool) {
		h = strings.ToLower(strings.TrimSuffix(h, "."))
		if h == "" {
			return nil, true
		}
		if h == "localhost" {
			return []string{"127.0.0.1", "::1"}, false
		}
		if ip, err := netip.ParseAddr(h); err == nil {
			ip = ip.Unmap().WithZone("")
			return []string{ip.String()}, ip.IsUnspecified()
		}
		return []string{h}, false
	}
	sa, wa := addrs(ha)
	sb, wb := addrs(hb)
	if wa || wb {
		return true
	}
	for _, x := range sa {
		if slices.Contains(sb, x) {
			return true
		}
	}
	return false
}

// IsLocalhost reports whether host (a URL's Hostname()) is localhost or a loopback address.
func IsLocalhost(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	if h == "localhost" {
		return true
	}
	if ip, err := netip.ParseAddr(h); err == nil {
		return ip.Unmap().IsLoopback()
	}
	return false
}

func (c *Config) validateListeners(v *validator) {
	if err := checkHostPort(c.Listen); err != nil {
		v.add("listen %q: %v", c.Listen, err)
	}
	if err := checkHostPort(c.Metrics.Listen); err != nil {
		v.add("metrics.listen %q: %v", c.Metrics.Listen, err)
	} else if checkHostPort(c.Listen) == nil && listenersCollide(c.Metrics.Listen, c.Listen) {
		v.add("metrics.listen %q: must differ from listen %q, which binds the same port (metrics never share the public listener, spec §2.1)",
			c.Metrics.Listen, c.Listen)
	}
	if err := checkHostPort(c.Temporal.Address); err != nil {
		v.add("temporal.address %q: %v", c.Temporal.Address, err)
	}
	if c.Temporal.Namespace == "" {
		v.add("temporal.namespace: required")
	}
	if c.DrainTimeout <= 0 {
		v.add("drain_timeout: must be positive")
	}
	switch c.Tenancy {
	case TenancySingle, TenancyMulti:
	default:
		v.add("tenancy %q: want single or multi", c.Tenancy)
	}
}

// validatePublicURL checks public_url and returns it parsed (nil when invalid).
func (c *Config) validatePublicURL(v *validator) *url.URL {
	if c.PublicURL == "" {
		v.add("public_url: required (the URL callers use; the served card carries it)")
		return nil
	}
	shown := redactURL(c.PublicURL)
	u, err := parseURL(c.PublicURL)
	switch {
	case err != nil:
		v.add("public_url %q: %v", shown, err)
		return nil
	case u.Scheme != "https" && u.Scheme != "http":
		v.add("public_url %q: want an http:// or https:// URL", shown)
		return nil
	case u.Host == "" || u.Hostname() == "":
		v.add("public_url %q: no host", shown)
		return nil
	case u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "":
		v.add("public_url %q: no credentials, query or fragment", shown)
		return nil
	}
	if err := checkURLPort(u); err != nil {
		v.add("public_url %q: %v", shown, err)
		return nil
	}
	return u
}

func (c *Config) validateTLS(v *validator, pub *url.URL) {
	t := c.TLS
	modes := 0
	if t.ACME != nil {
		modes++
	}
	if t.Cert != "" || t.Key != "" {
		modes++
		if t.Cert == "" || t.Key == "" {
			v.add("tls: cert and key go together")
		}
	}
	if t.TerminatedByProxy {
		modes++
	}
	if modes > 1 {
		v.add("tls: acme, cert/key and terminated_by_proxy are exclusive")
	}
	if t.TerminatedByProxy {
		c.Warnings = append(c.Warnings, "tls.terminated_by_proxy: the gate listens in plaintext and trusts a front proxy to "+
			"terminate TLS; HSTS is the proxy's, the tunnel's channel binding is none (D69)")
	}
	if pub == nil {
		return
	}
	local := IsLocalhost(pub.Hostname())
	tlsAtAll := t.TerminatesTLS() || t.TerminatedByProxy
	switch {
	case !tlsAtAll && !local:
		v.add("public_url %q is not a localhost origin, so plaintext is refused: set tls.acme or tls.cert/key, "+
			"or tls.terminated_by_proxy behind a TLS-terminating proxy (PRD §11, D69)", c.PublicURL)
	case tlsAtAll && pub.Scheme != "https":
		v.add("public_url %q: must be https:// when TLS is terminated (by the gate or by a front proxy)", c.PublicURL)
	case !tlsAtAll && pub.Scheme == "https":
		v.add("public_url %q is https:// but no TLS is configured: set tls.cert/key, tls.acme or tls.terminated_by_proxy", c.PublicURL)
	}
	if a := t.ACME; a != nil {
		if a.Email != "" && !strings.Contains(a.Email, "@") {
			v.add("tls.acme.email %q: not an email address", a.Email)
		}
		if a.CacheDir == "" {
			v.add("tls.acme.cache_dir: required (no user cache directory to default to)")
		}
		host := strings.ToLower(pub.Hostname())
		for _, d := range a.Domains {
			if _, err := netip.ParseAddr(d); err == nil || IsLocalhost(d) || !dnsNameRE.MatchString(d) {
				v.add("tls.acme.domains: %q is not a DNS name a CA can validate", d)
			}
		}
		if !slices.Contains(a.Domains, host) {
			v.add("tls.acme.domains %v: must include the public_url host %q", a.Domains, host)
		}
	}
}

var dnsNameRE = regexp.MustCompile(`^(?i:[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)(\.(?i:[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?))+$`)

func (c *Config) validateEdge(v *validator) {
	ci := &c.Edge.ClientIP
	ci.TrustedPrefixes = nil
	for _, s := range ci.Trusted {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			a, aerr := netip.ParseAddr(s)
			if aerr != nil {
				v.add("edge.client_ip.trusted: %q is not a CIDR or an address", s)
				continue
			}
			p = netip.PrefixFrom(a, a.BitLen())
		}
		ci.TrustedPrefixes = append(ci.TrustedPrefixes, p.Masked())
	}
	switch ci.From {
	case ClientIPRemote:
		if len(ci.Trusted) > 0 {
			v.add("edge.client_ip.trusted is only used with from: proxy_protocol or x_forwarded_for")
		}
	case ClientIPProxyProtocol, ClientIPXForwardedFor:
		if len(ci.Trusted) == 0 {
			v.add("edge.client_ip.from: %s needs edge.client_ip.trusted, the proxies' CIDRs (D69)", ci.From)
		}
	default:
		v.add("edge.client_ip.from %q: want remote, proxy_protocol or x_forwarded_for", ci.From)
	}
}

func (c *Config) validateLimits(v *validator) {
	l := c.Limits
	positive := func(key string, n int64) {
		if n <= 0 {
			v.add("limits.%s: must be positive", key)
		}
	}
	positive("body", l.Body.Int64())
	positive("inflight_per_key", int64(l.InflightPerKey))
	if !(l.RPSPerIP > 0) {
		v.add("limits.rps_per_ip: must be positive")
	}
	positive("header_timeout", int64(l.HeaderTimeout))
	positive("idle", int64(l.Idle))
	positive("conns_per_ip", int64(l.ConnsPerIP))
	positive("max_conns", int64(l.MaxConns))
	positive("dispatch_slots", int64(l.DispatchSlots))
	positive("push_slots", int64(l.PushSlots))
	positive("task_bytes", l.TaskBytes.Int64())
}

func (c *Config) validateBackends(v *validator) {
	if c.Postgres == "" {
		v.add("postgres: required (a postgres:// URL)")
	} else if !strings.HasPrefix(c.Postgres, "postgres://") && !strings.HasPrefix(c.Postgres, "postgresql://") {
		v.add("postgres: want a postgres:// URL")
	} else if _, err := pg.ParseConfig(c.Postgres); err != nil {
		// The parse the gate connects with, pool_* parameters included; pgx's message quotes the URL
		// with its password masked.
		v.add("postgres: %v", err)
	}
	if c.Blobs != "" {
		if u, err := url.Parse(c.Blobs); err != nil || u.Scheme != "s3" || u.Host == "" {
			v.add("blobs %q: want s3://<bucket>[/<prefix>]", redactURL(c.Blobs))
		} else if u.User != nil {
			v.add("blobs %q: credentials do not belong in the URL (the bucket's come from the environment)", redactURL(c.Blobs))
		}
	}
}

func (c *Config) validateAuth(v *validator) {
	if o := c.Auth.OIDC; o != nil {
		if err := checkHTTPSURL(o.Issuer, false); err != nil {
			v.add("auth.oidc.issuer: %v", err)
		}
		if o.Audience == "" {
			v.add("auth.oidc.audience: required")
		}
	}
	if m := c.Auth.MCPOAuth; m != nil {
		if err := checkHTTPSURL(m.Issuer, false); err != nil {
			v.add("auth.mcp_oauth.issuer: %v", err)
		}
	}
}

// checkHTTPSURL checks an operator-configured URL: absolute, https (or http to a loopback host when
// loopbackHTTP), a port from 1 to 65535 if any, no credentials or fragment. Errors quote the URL without
// its userinfo.
func checkHTTPSURL(s string, loopbackHTTP bool) error {
	shown := redactURL(s)
	u, err := parseURL(s)
	if err != nil {
		return fmt.Errorf("%q: %w", shown, err)
	}
	if u.Host == "" || u.Hostname() == "" {
		return fmt.Errorf("%q: want an absolute URL", shown)
	}
	if u.User != nil || u.Fragment != "" {
		return fmt.Errorf("%q: no credentials or fragment", shown)
	}
	if err := checkURLPort(u); err != nil {
		return fmt.Errorf("%q: %w", shown, err)
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && loopbackHTTP && IsLocalhost(u.Hostname()):
	default:
		return fmt.Errorf("%q: want https://", shown)
	}
	return nil
}

func (c *Config) validateMetadata(v *validator) {
	for _, k := range c.CallerMetadata {
		if !strings.HasPrefix(k, "kontra.") || len(k) == len("kontra.") {
			v.add("caller_metadata: %q is not a kontra. key (only kontra. keys are stripped from callers, spec §2.5)", k)
		}
	}
	// SPEC-CONFLICT (D113): spec §13.2 and spec/schema/config.json accept any non-empty prefix, but §2.5 strips only
	// kontra. keys from callers and services, and a2a-go copies a service event's metadata into the task's.
	// Under another prefix (acme.) a seller could write keys that TaskRef.metadata hands the Lifecycle hooks
	// as operator-written, which §13.3 says it never holds ("never caller or service metadata"). The closest
	// faithful behaviour: an operator prefix must lie under kontra. (as caller_metadata keys must, and as
	// Cloud's kontra.escrow. does), so §2.5's stripping vouches for every key under it; kontra. alone, which
	// would cover the gate's own keys, is refused too.
	for _, p := range c.OperatorMetadataPrefixes {
		switch {
		case p == "" || strings.TrimSpace(p) != p:
			v.add("operator_metadata_prefixes: %q is not a key prefix", p)
		case !strings.HasPrefix(p, "kontra.") || len(p) == len("kontra."):
			v.add("operator_metadata_prefixes: %q is not under kontra. (only kontra. keys are stripped from callers and services, spec §2.5, so a key under another prefix could be theirs, §13.3)", p)
		}
	}
	for _, o := range c.MCP.AllowedOrigins {
		u, err := url.Parse(o)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.Path != "" ||
			u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			v.add("mcp.allowed_origins: %q is not an origin (scheme://host[:port])", redactURL(o))
		}
	}
}

var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func (c *Config) validateHooks(v *validator) {
	if h := c.Hooks.Admission; h != nil {
		if err := checkHTTPSURL(h.URL, true); err != nil {
			v.add("hooks.admission.url: %v (https:// unless the host is loopback, spec §13.1)", err)
		}
		if !envNameRE.MatchString(h.SecretEnv) {
			v.add("hooks.admission.secret_env %q: want an environment variable name", h.SecretEnv)
		}
		for _, op := range h.Operations {
			if op != "task" && op != "session" && op != "call" {
				v.add("hooks.admission.operations: %q is not task, session or call", op)
			}
		}
		// Not built until M5. Using the built-in hook instead would admit what the operator's hook
		// would deny, so the gate refuses to start rather than fail open.
		v.add("hooks.admission: the remote Admission hook is not built in this release (M5); remove it to use the built-in")
	}
	if h := c.Hooks.Lifecycle; h != nil {
		if h.TaskQueue == "" {
			v.add("hooks.lifecycle.task_queue: required")
		}
		v.add("hooks.lifecycle: the remote Lifecycle hook is not built in this release (M5); remove it to use the built-in")
	}
}

func (c *Config) validateTunnel(v *validator) {
	switch c.Tunnel.ChannelBinding {
	case ChannelBindingNone:
	case ChannelBindingTLSExporter:
		if !c.TLS.TerminatesTLS() {
			v.add("tunnel.channel_binding: tls-exporter needs the gate to terminate TLS itself (tunnel.md §4)")
		}
	default:
		v.add("tunnel.channel_binding %q: want tls-exporter or none", c.Tunnel.ChannelBinding)
	}
}

func (c *Config) validateService(v *validator, name string, s Service) {
	at := "services." + name
	if !NameRE.MatchString(name) {
		v.add("%s: the name must match %s", at, NameRE)
	}
	switch s.Kind {
	case KindA2A, KindMCP:
	case "":
		v.add("%s.kind: required (a2a or mcp)", at)
	default:
		v.add("%s.kind %q: want a2a or mcp", at, s.Kind)
	}

	n := 0
	for _, set := range []bool{s.Backend.Tunnel != nil, s.Backend.A2A != nil, s.Backend.MCP != nil} {
		if set {
			n++
		}
	}
	switch {
	case n != 1:
		v.add("%s.backend: want exactly one of tunnel, a2a {url} or mcp {url}, found %d", at, n)
	case s.Backend.A2A != nil && s.Kind == KindMCP:
		v.add("%s.backend: an mcp service takes backend tunnel or mcp {url}, not a2a", at)
	case s.Backend.MCP != nil && s.Kind == KindA2A:
		v.add("%s.backend: an a2a service takes backend tunnel or a2a {url}, not mcp", at)
	}
	if d := s.Backend.Direct(); d != nil && n == 1 {
		key := at + ".backend." + s.Kind
		if err := CheckBackendURL(d.URL, d.AllowPrivate); err != nil {
			v.add("%s.url: %v", key, err)
		}
	}

	for _, e := range s.Allow {
		if _, err := ParseAllowEntry(e); err != nil {
			v.add("%s.allow: %v", at, err)
		}
	}

	if s.Kind == KindA2A {
		if s.Tools != nil {
			v.add("%s.tools: mcp services only (spec §11.3)", at)
		}
		if s.Tasks != "" {
			v.add("%s.tasks: mcp services only (spec §11.5)", at)
		}
	}
	if s.Kind == KindMCP {
		switch s.Tasks {
		case TasksDurable, TasksPassthrough, TasksOff:
		default:
			v.add("%s.tasks %q: want durable, passthrough or off", at, s.Tasks)
		}
	}
	if s.Tools != nil {
		for _, g := range slices.Concat(s.Tools.Allow, s.Tools.Deny) {
			if g == "" || strings.ContainsAny(g, "?[]\\") {
				v.add("%s.tools: %q is not a tool glob (names and * only, spec §11.3)", at, g)
			}
		}
	}
	switch s.Sealed.Mode {
	case SealedOff, SealedRequired, SealedOptional:
	default:
		v.add("%s.sealed: want true, false or {required: bool}", at)
	}
	direct := s.Backend.Direct() != nil && n == 1
	if s.Sealed.Mode != SealedOff && direct {
		// SPEC-CONFLICT (D91): relay §17 and ext-sealed.md §1 do not tie sealed: to a backend. But the recipients
		// are the service's enrolled node keys and the hosting node unseals (ext-sealed §1, PRD §8); a
		// direct backend has neither, so a sealed direct service could never be served as declared (and
		// this build, which does not seal, would relay plaintext for it). Refused rather than ignored.
		v.add("%s.sealed: sealing needs backend tunnel: the recipients are the service's enrolled node keys and "+
			"the hosting node unseals (ext-sealed.md §1); a direct backend has neither", at)
	}
	tunnelled := s.Backend.Tunnel != nil && n == 1
	if s.Sealed.Mode != SealedOff && tunnelled {
		// SPEC-CONFLICT (D91 amended for M2, M2 plan WP-04): D91 accepted sealed: on a tunnel service with a
		// warning while tunnel backends were not served. This build serves them and does not seal (M3), so
		// it would relay the service's parts in plaintext (fail open): refused, as on a direct backend.
		v.add("%s.sealed: sealing is not built in this release (M3), and this build serves tunnel services: it "+
			"would relay the service's parts in plaintext; remove sealed until sealing is built (ext-sealed.md §1)", at)
	}
	if !s.SellerKeys.IsZero() {
		if err := checkSellerKeys(s.SellerKeys); err != nil {
			v.add("%s.seller_keys: %v", at, err)
		} else if (direct && s.Kind == KindA2A) || tunnelled {
			// SPEC-CONFLICT (D90; amended for tunnel services in M2, M2 plan WP-04): spec §3 makes a service
			// whose card no configured key verifies unavailable (503 card routes, service_unavailable at the
			// first attempt). Verification is M3; ignoring the keys would serve the card unverified (fail
			// open), so the gate refuses to start instead, as for the remote hooks. This build serves tunnel
			// services and their cards, so D90's warning for them no longer fails closed.
			v.add("%s.seller_keys: seller card signatures are not verified in this release (M3); remove seller_keys "+
				"rather than serve the card unverified (spec §3)", at)
		}
	}
	if !s.Annotations.IsZero() && (len(s.Annotations) == 0 || s.Annotations[0] != '{') {
		v.add("%s.annotations: want a mapping", at)
	}
	// services stores these as integer seconds (timeout_s, retention_s, session_idle_s in 0001).
	if s.SessionIdle <= 0 {
		v.add("%s.session_idle_s: must be positive", at)
	} else if int64(s.SessionIdle) > math.MaxInt32 {
		v.add("%s.session_idle_s %d: at most %d (stored as integer seconds)", at, s.SessionIdle, math.MaxInt32)
	}
	checkSeconds := func(key string, d Duration) {
		switch {
		case d < Duration(time.Second) || d.D()%time.Second != 0:
			v.add("%s.task.%s %s: want a whole number of seconds, at least 1s", at, key, d)
		case d.D()/time.Second > math.MaxInt32:
			v.add("%s.task.%s %s: at most %d seconds (stored as integer seconds)", at, key, d, math.MaxInt32)
		}
	}
	checkSeconds("timeout", s.Task.Timeout)
	checkSeconds("retention", s.Task.Retention)
	if r := s.Task.RetryCount(); r < 0 || r > MaxTaskRetries {
		v.add("%s.task.retries %d: want 0 to %d", at, r, MaxTaskRetries)
	}
}

// CheckBackendURL checks a direct backend's configured url: an absolute https:// URL, or http:// when
// allowPrivate (D67), with a port from 1 to 65535 if any, and no credentials, query or fragment. It is the
// base URL (D71): its path is where the card is fetched from, and its scheme://host:port is the only
// address the gate dials. Errors quote the URL without its userinfo.
func CheckBackendURL(s string, allowPrivate bool) error {
	if s == "" {
		return fmt.Errorf("required")
	}
	shown := redactURL(s)
	u, err := parseURL(s)
	if err != nil {
		return fmt.Errorf("%q: %w", shown, err)
	}
	if u.Host == "" || u.Hostname() == "" || u.Opaque != "" {
		return fmt.Errorf("%q: want an absolute URL", shown)
	}
	if u.User != nil {
		return fmt.Errorf("%q: credentials do not belong in the URL (the gate holds no seller secret)", shown)
	}
	if u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return fmt.Errorf("%q: no query or fragment (the URL is the base the card is fetched from, D71)", shown)
	}
	if err := checkURLPort(u); err != nil {
		return fmt.Errorf("%q: %w", shown, err)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !allowPrivate {
			return fmt.Errorf("%q: http:// is refused unless allow_private: true (D67)", shown)
		}
	default:
		return fmt.Errorf("%q: want https:// (or http:// with allow_private)", shown)
	}
	return nil
}

func checkSellerKeys(j JSON) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(j, &m); err != nil {
		return fmt.Errorf("want an inline JWKS {keys: [...]} or {url: https://...}")
	}
	_, hasKeys := m["keys"]
	rawURL, hasURL := m["url"]
	switch {
	case hasKeys && hasURL:
		return fmt.Errorf("keys and url are exclusive")
	case hasKeys:
		var keys []map[string]any
		if err := json.Unmarshal(m["keys"], &keys); err != nil || len(keys) == 0 {
			return fmt.Errorf("keys: want a non-empty list of JWKs")
		}
	case hasURL:
		var u string
		if err := json.Unmarshal(rawURL, &u); err != nil {
			return fmt.Errorf("url: want a string")
		}
		if err := checkHTTPSURL(u, false); err != nil {
			return fmt.Errorf("url: %v", err)
		}
		if len(m) != 1 {
			return fmt.Errorf("{url} takes no other key")
		}
	default:
		return fmt.Errorf("want an inline JWKS {keys: [...]} or {url: https://...}")
	}
	return nil
}

// AllowKind is the kind of an allow: entry (spec §2.3, D20).
type AllowKind string

// Allow entry kinds.
const (
	AllowKey       AllowKind = "key"       // key:<prefix glob>
	AllowOrg       AllowKind = "org"       // org:<tenant>
	AllowOIDCSub   AllowKind = "oidc:sub"  // oidc:sub:<subject>
	AllowNode      AllowKind = "node"      // node:<key_id>
	AllowAnonymous AllowKind = "anonymous" // anonymous
)

// AllowEntry is one parsed allow: entry.
type AllowEntry struct {
	Kind AllowKind
	// Value is the prefix glob, tenant slug, subject or key id; empty for anonymous.
	Value string
}

var (
	// KeyPrefixRE is the stored shape of an API key prefix (api_keys.prefix CHECK in 0001; D68: keys are
	// kk_<operator prefix>_<random>).
	KeyPrefixRE = regexp.MustCompile(`^[A-Za-z0-9_]{4,32}$`)
	keyGlobRE   = regexp.MustCompile(`^[A-Za-z0-9_*?]+$`)
	// NodeKeyIDRE is the shape of a node key id (tunnel_keys.key_id CHECK in 0001).
	NodeKeyIDRE = regexp.MustCompile(`^k1_[a-z2-7]{26}$`)
)

// ParseAllowEntry parses one allow: entry: key:<prefix glob>, org:<tenant>, oidc:sub:<subject>,
// node:<key_id> or anonymous. An unknown kind, and a key: glob that can never match a stored prefix (a
// prefix is [A-Za-z0-9_]{4,32}, so write key:kk_acme_*, D68), are errors.
func ParseAllowEntry(s string) (AllowEntry, error) {
	if s != strings.TrimSpace(s) || s == "" {
		return AllowEntry{}, fmt.Errorf("%q: an entry has no surrounding spaces", s)
	}
	if s == string(AllowAnonymous) {
		return AllowEntry{Kind: AllowAnonymous}, nil
	}
	if sub, ok := strings.CutPrefix(s, "oidc:sub:"); ok {
		if sub == "" {
			return AllowEntry{}, fmt.Errorf("%q: oidc:sub: needs a subject", s)
		}
		return AllowEntry{Kind: AllowOIDCSub, Value: sub}, nil
	}
	kind, val, ok := strings.Cut(s, ":")
	if !ok {
		return AllowEntry{}, fmt.Errorf("%q: want key:, org:, oidc:sub:, node: or anonymous", s)
	}
	switch AllowKind(kind) {
	case AllowKey:
		// * matches any run of characters and ? exactly one: a glob without * matches prefixes of exactly
		// its length, one with * prefixes of at least its non-* length. It can match a stored prefix only
		// if that range meets 4..32.
		star := strings.Contains(val, "*")
		minLen := len(val) - strings.Count(val, "*")
		switch {
		case !keyGlobRE.MatchString(val):
			return AllowEntry{}, fmt.Errorf("%q: a key: glob can never match: API key prefixes are [A-Za-z0-9_] "+
				"(keys are kk_<operator prefix>_<random>; write e.g. key:kk_acme_*, D68)", s)
		case !star && (len(val) < 4 || len(val) > 32):
			return AllowEntry{}, fmt.Errorf("%q: a key prefix is 4 to 32 characters, and this matches only %d-character prefixes", s, len(val))
		case minLen > 32:
			return AllowEntry{}, fmt.Errorf("%q: a key: glob longer than any prefix can never match", s)
		}
		return AllowEntry{Kind: AllowKey, Value: val}, nil
	case AllowOrg:
		if !NameRE.MatchString(val) {
			return AllowEntry{}, fmt.Errorf("%q: org: needs a tenant name matching %s", s, NameRE)
		}
		return AllowEntry{Kind: AllowOrg, Value: val}, nil
	case AllowNode:
		if !NodeKeyIDRE.MatchString(val) {
			return AllowEntry{}, fmt.Errorf("%q: node: needs a key id matching %s", s, NodeKeyIDRE)
		}
		return AllowEntry{Kind: AllowNode, Value: val}, nil
	}
	return AllowEntry{}, fmt.Errorf("%q: unknown allow: kind %q (want key:, org:, oidc:sub:, node: or anonymous)", s, kind)
}

// unbuilt lists the keys this build accepts but does not act on yet. Each one fails closed when ignored
// (nobody is admitted by it, nothing is served for it); keys that would fail open are refused by
// Validate instead (the remote hooks, sealed on any backend, seller_keys on a direct a2a backend or a
// tunnel service). seller_keys loads only on a direct MCP service, which has no card and is not built
// either. Remove an entry when the work that builds it lands. Tunnel backends are served since M2.
func (c *Config) unbuilt() []string {
	var w []string
	note := func(format string, args ...any) { w = append(w, fmt.Sprintf(format, args...)) }
	if c.Auth.OIDC != nil {
		note("auth.oidc: OIDC callers are not built in this release; their bearer tokens are refused (401)")
	}
	if c.Auth.MCPOAuth != nil {
		note("auth.mcp_oauth: the MCP endpoints are not built in this release (M3)")
	}
	if c.Blobs != "" {
		note("blobs: the claim-check bucket is not built in this release (M3); parts over 256 KiB are refused")
	}
	if c.SigningKey != "" {
		note("signing_key: cards are not signed in this release (M3)")
	}
	if c.Peer != nil {
		note("peer: the replica-to-replica hop is not built in this release (M3 WP-04)")
	}
	if c.Tunnel.Anonymous {
		// Fails closed: an M2 gate creates no anonymous tenant, so a hello with neither enroll nor auth from an
		// unknown key is unknown_key (spec/tunnel.md §4, §6).
		note("tunnel.anonymous: anonymous sessions are not built in this release (M4); a node key that is not enrolled is refused unknown_key")
	}
	for _, name := range sortedKeys(c.Services) {
		s := c.Services[name]
		if s.Kind == KindMCP {
			note("services.%s: MCP services are not built in this release", name)
		}
		if !s.SellerKeys.IsZero() {
			note("services.%s.seller_keys: seller card signatures are not verified in this release (M3)", name)
		}
	}
	return w
}
