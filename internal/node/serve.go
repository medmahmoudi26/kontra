package node

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"kontra.sh/kontra/internal/tunnel"
)

// Serve is one --serve kind:name=target (note 2 §1). M2 accepts a2a:<name>=<http(s) URL> only; mcp: is
// refused with "not until M3".
type Serve struct {
	Kind   string   // tunnel.KindA2A
	Name   string   // the service name at the gate (relay.md §2.1's pattern)
	Target *url.URL // scheme://host[:port][/base]: every request goes here and nowhere else (§8.2)
}

// serviceNameRE is relay.md §2.1's service name (config.NameRE, services.name's CHECK); a test keeps them
// equal. The node checks it before it dials: the gate would refuse the hello service_denied, which the
// node does not retry.
var serviceNameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ErrBasePath is a --serve target with a path other than "/" (see ParseServe).
var ErrBasePath = errors.New("a --serve target with a base path is refused in M2")

// ParseServe parses one --serve value. A target whose path is not empty or "/" is refused in M2 (see the
// plan's spec conflict: the node prefixes the target's base path, and the gate sends the upstream card's
// interface path, which already holds it).
//
// SPEC-CONFLICT (M2 plan C1; spec/tunnel.md §8.2 against relay.md §6.2 and §3, D71): §8.2 makes the node
// prefix the target's base path to every request path, while the gate takes the path of an exchange from
// the upstream card's interface URL, which is the seller's absolute path and already holds that base:
// target http://h:9/base with interface http://h:9/base/rpc would be called at /base/base/rpc. The gate
// cannot strip a base it does not know without new wire protocol. Until the owner amends §8.2 (proposal:
// a path already under the base is forwarded as is, any other is prefixed), M2 fails closed and refuses
// such a target with an error naming the conflict. Root bases (helloworld at http://127.0.0.1:9999, a2a
// servers at their root) are unaffected. Consequence (M2 plan audit): with a root base, §8.2's decode,
// clean and prefix confine every path to the target, so sidecar_not_a_proxy's "outside the base" row
// cannot be reached through the binary; the Proxy still enforces it, and its unit tests construct a Serve
// with a non-root base directly.
func ParseServe(s string) (Serve, error) {
	kind, rest, ok := strings.Cut(s, ":")
	if !ok || kind == "" {
		return Serve{}, errors.New("--serve: want kind:name=url, e.g. a2a:hello=http://127.0.0.1:9999")
	}
	switch kind {
	case tunnel.KindA2A:
	case tunnel.KindMCP:
		return Serve{}, errors.New("--serve mcp:…: hosting MCP servers is not until M3")
	default:
		return Serve{}, fmt.Errorf("--serve: kind %q: want a2a (mcp is not until M3)", clipValue(kind))
	}
	name, target, ok := strings.Cut(rest, "=")
	if !ok {
		return Serve{}, fmt.Errorf("--serve %s:%q: want %s:name=url", kind, clipValue(rest), kind)
	}
	if !serviceNameRE.MatchString(name) {
		return Serve{}, fmt.Errorf("--serve %s:%q: the name must match %s (relay.md §2.1)", kind, clipValue(name), serviceNameRE)
	}
	u, err := checkTarget(target)
	if err != nil {
		return Serve{}, fmt.Errorf("--serve %s:%s: %w", kind, name, err)
	}
	return Serve{Kind: kind, Name: name, Target: u}, nil
}

// checkServe is ParseServe's check of a Serve built by hand (New's).
func checkServe(s Serve) error {
	switch s.Kind {
	case tunnel.KindA2A:
	case tunnel.KindMCP:
		return fmt.Errorf("--serve mcp:%s: hosting MCP servers is not until M3", s.Name)
	default:
		return fmt.Errorf("--serve: kind %q: want a2a", clipValue(s.Kind))
	}
	if !serviceNameRE.MatchString(s.Name) {
		return fmt.Errorf("--serve %s:%q: the name must match %s (relay.md §2.1)", s.Kind, clipValue(s.Name), serviceNameRE)
	}
	if s.Target == nil {
		return fmt.Errorf("--serve %s:%s: no target URL", s.Kind, s.Name)
	}
	if _, err := checkTarget(s.Target.String()); err != nil {
		return fmt.Errorf("--serve %s:%s: %w", s.Kind, s.Name, err)
	}
	return nil
}

// checkTarget parses a --serve target: http:// or https://, a host, an optional port, no userinfo, query
// or fragment, and in M2 no base path (ParseServe's SPEC-CONFLICT). A refusal quotes at most
// scheme://host[:port]: the rest of a URL can hold a credential.
func checkTarget(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("the target is not a URL")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	switch {
	case u.User != nil:
		return nil, errors.New("no userinfo in a target URL")
	case u.Scheme != "http" && u.Scheme != "https":
		return nil, fmt.Errorf("target scheme %q: want http or https", clipValue(u.Scheme))
	case u.Opaque != "" || u.Host == "" || u.Hostname() == "":
		return nil, errors.New("the target has no host: want http(s)://host[:port]")
	case u.RawQuery != "" || u.ForceQuery:
		return nil, fmt.Errorf("target %s: no query", origin(u))
	case u.Fragment != "" || strings.Contains(raw, "#"):
		return nil, fmt.Errorf("target %s: no fragment", origin(u))
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("target %s: port %q", origin(u), clipValue(p))
		}
	} else if strings.HasSuffix(u.Host, ":") {
		return nil, fmt.Errorf("target %s: empty port", origin(u))
	}
	if u.Path != "" && u.Path != "/" {
		return nil, fmt.Errorf("%w (target %s, base path %q): the gate calls the upstream card's interface path, "+
			"which already holds the base, and the node would prefix it again (spec/tunnel.md §8.2 against relay.md §6.2 "+
			"and D71; spec conflict C1 of the M2 plan, for the owner); serve the agent at its root", ErrBasePath, origin(u), clipValue(u.Path))
	}
	if u.Path == "" {
		u.Path = "/"
	}
	u.RawPath = ""
	return u, nil
}

// origin is scheme://host[:port] of u, for a message.
func origin(u *url.URL) string { return u.Scheme + "://" + u.Host }

// hostPort is the address a target is dialed at: its host and port, the scheme's default port when it
// names none.
func hostPort(u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[u.Scheme]
	}
	return net.JoinHostPort(strings.ToLower(u.Hostname()), port)
}

// basePath is a target's base path without its trailing slash: "" for the root.
func basePath(u *url.URL) string { return strings.TrimSuffix(u.Path, "/") }

// clipValue shortens a value an error quotes back (with %q).
func clipValue(s string) string {
	const max = 80
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
