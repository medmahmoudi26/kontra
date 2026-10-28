package node

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"kontra.sh/kontra/internal/stdiobridge"
	"kontra.sh/kontra/internal/tunnel"
)

// Serve is one --serve kind:name=target (note 2 §1): a2a:<name>=<http(s) URL>, mcp:<name>=<http(s) URL> or
// mcp:<name>=stdio:<command line> (spec/tunnel.md §10).
type Serve struct {
	Kind   string   // tunnel.KindA2A or tunnel.KindMCP
	Name   string   // the service name at the gate (relay.md §2.1's pattern)
	Target *url.URL // scheme://host[:port][/base]: every request goes here and nowhere else (§8.2); nil for stdio
	// Command is a stdio MCP server's command line, split (stdiobridge.SplitCommand): the node runs one child per
	// MCP session and bridges it (§10). Nil for a URL target.
	Command []string
}

// stdioPrefix introduces a stdio MCP server's command line in --serve mcp:<name>=stdio:<command line>.
const stdioPrefix = "stdio:"

// serviceNameRE is relay.md §2.1's service name (config.NameRE, services.name's CHECK); a test keeps them
// equal. The node checks it before it dials: the gate would refuse the hello service_denied, which the
// node does not retry.
var serviceNameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ErrBasePath is an A2A --serve target with a path other than "/" (see ParseServe; D123, D210).
var ErrBasePath = errors.New("an A2A --serve target with a base path is refused (D123, D210)")

// ParseServe parses one --serve value, a stdio command line split with HOME from the process's environment
// (ParseArgs splits it with the HOME it was given). An A2A target whose path is not empty or "/" is refused (see
// the plan's spec conflict: the node prefixes the target's base path, and the gate sends the upstream card's
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
//
// An MCP URL target may have a path: it names the server's one MCP endpoint (MCP 2025-11-25 Streamable HTTP: "a
// single HTTP endpoint path"; server-everything@1.0.0 serves /mcp only), and the gate sends every MCP exchange of a
// tunnel service to "/" (upstream.Tunnel.Base), which the node forwards to that endpoint (targetPath).
//
// Plan deviation (M3 plan WP-05: "--serve mcp:<name>=<http(s) URL> (root-only path rule of D123)"): a root-only MCP
// target cannot host the pinned server-everything@1.0.0 (examples/mcp/everything), whose endpoint is /mcp, and
// tunnel.md §8.2 allows a target with a path once the node forwards by its rule ("a path already under the
// target's base path as it is ... prefixes any other"), which targetPath now does. D123's refusal is kept for A2A
// targets, as D123 decided for M2 (D210).
func ParseServe(s string) (Serve, error) {
	return parseServe(s, os.Getenv("HOME"))
}

func parseServe(s, home string) (Serve, error) {
	kind, rest, ok := strings.Cut(s, ":")
	if !ok || kind == "" {
		return Serve{}, errors.New("--serve: want kind:name=url, e.g. a2a:hello=http://127.0.0.1:9999")
	}
	switch kind {
	case tunnel.KindA2A, tunnel.KindMCP:
	default:
		return Serve{}, fmt.Errorf("--serve: kind %q: want a2a or mcp", clipValue(kind))
	}
	name, target, ok := strings.Cut(rest, "=")
	if !ok {
		return Serve{}, fmt.Errorf("--serve %s:%q: want %s:name=url", kind, clipValue(rest), kind)
	}
	if !serviceNameRE.MatchString(name) {
		return Serve{}, fmt.Errorf("--serve %s:%q: the name must match %s (relay.md §2.1)", kind, clipValue(name), serviceNameRE)
	}
	if kind == tunnel.KindMCP && strings.HasPrefix(target, stdioPrefix) {
		cmd, err := stdiobridge.SplitCommand(strings.TrimPrefix(target, stdioPrefix), home)
		if err != nil {
			return Serve{}, fmt.Errorf("--serve mcp:%s=stdio:…: %w", name, err)
		}
		return Serve{Kind: kind, Name: name, Command: cmd}, nil
	}
	u, err := checkTarget(target, kind)
	if err != nil {
		return Serve{}, fmt.Errorf("--serve %s:%s: %w", kind, name, err)
	}
	return Serve{Kind: kind, Name: name, Target: u}, nil
}

// checkServe is ParseServe's check of a Serve built by hand (New's).
func checkServe(s Serve) error {
	switch s.Kind {
	case tunnel.KindA2A, tunnel.KindMCP:
	default:
		return fmt.Errorf("--serve: kind %q: want a2a or mcp", clipValue(s.Kind))
	}
	if !serviceNameRE.MatchString(s.Name) {
		return fmt.Errorf("--serve %s:%q: the name must match %s (relay.md §2.1)", s.Kind, clipValue(s.Name), serviceNameRE)
	}
	switch {
	case s.Command != nil && (s.Kind != tunnel.KindMCP || s.Target != nil):
		return fmt.Errorf("--serve %s:%s: a command line is for an MCP service with no URL", s.Kind, s.Name)
	case s.Command != nil && (len(s.Command) == 0 || s.Command[0] == ""):
		return fmt.Errorf("--serve %s:%s: an empty command line", s.Kind, s.Name)
	case s.Command != nil:
		return nil
	case s.Target == nil:
		return fmt.Errorf("--serve %s:%s: no target URL", s.Kind, s.Name)
	}
	if _, err := checkTarget(s.Target.String(), s.Kind); err != nil {
		return fmt.Errorf("--serve %s:%s: %w", s.Kind, s.Name, err)
	}
	return nil
}

// checkTarget parses a --serve target: http:// or https://, a host, an optional port, no userinfo, query
// or fragment, and for an A2A service no base path (ParseServe's SPEC-CONFLICT). A refusal quotes at most
// scheme://host[:port]: the rest of a URL can hold a credential.
func checkTarget(raw, kind string) (*url.URL, error) {
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
	if kind != tunnel.KindMCP && u.Path != "" && u.Path != "/" {
		return nil, fmt.Errorf("%w (target %s, base path %q): serve the agent at its root; an MCP target may name "+
			"its endpoint path (spec/tunnel.md §8.2)", ErrBasePath, origin(u), clipValue(u.Path))
	}
	if p := u.Path; kind == tunnel.KindMCP && p != "" {
		c := path.Clean(p)
		if strings.HasSuffix(p, "/") && c != "/" {
			c += "/"
		}
		if c != p || p[0] != '/' {
			return nil, fmt.Errorf("target %s: the path %q is not clean (no ., .. or //)", origin(u), clipValue(p))
		}
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

// upstreamGroups maps every URL service that shares its upstream with another service of the node to the key of
// their group's one sealed state (sealing.HostingConfig.Shared, D236): two services share an upstream when their
// targets have one origin (a loopback host by any of its names, a default port written or not) and the base path of
// one is the other's or under it, the root covering every path, since the node forwards any path under a service's
// base (spec/tunnel.md §8.2). The key is the group's first name in order. Stdio services each run children of their
// own and share nothing.
func upstreamGroups(serves []Serve) map[string]string {
	var urls []Serve
	for _, s := range serves {
		if s.Target != nil && s.Command == nil {
			urls = append(urls, s)
		}
	}
	parent := map[string]string{}
	var find func(string) string
	find = func(n string) string {
		if p, ok := parent[n]; ok && p != n {
			r := find(p)
			parent[n] = r
			return r
		}
		return n
	}
	for i := range urls {
		for j := i + 1; j < len(urls); j++ {
			a, b := urls[i], urls[j]
			if upstreamOrigin(a.Target) != upstreamOrigin(b.Target) || !basesOverlap(basePath(a.Target), basePath(b.Target)) {
				continue
			}
			ra, rb := find(a.Name), find(b.Name)
			if ra == rb {
				continue
			}
			if rb < ra {
				ra, rb = rb, ra
			}
			parent[ra], parent[rb] = ra, ra
		}
	}
	out := map[string]string{}
	for _, s := range urls {
		if r := find(s.Name); r != s.Name || parent[s.Name] != "" {
			out[s.Name] = r
		}
	}
	return out
}

// upstreamOrigin is a target's origin as upstreamGroups compares it: scheme, host (every loopback name as one) and
// port (the scheme's default when none is written).
func upstreamOrigin(u *url.URL) string {
	host, port := strings.ToLower(u.Hostname()), u.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[strings.ToLower(u.Scheme)]
	}
	if a, err := netip.ParseAddr(host); host == "localhost" || err == nil && a.Unmap().IsLoopback() {
		host = "loopback"
	}
	return strings.ToLower(u.Scheme) + "://" + net.JoinHostPort(host, port)
}

// basesOverlap reports two base paths (without a trailing slash, "" the root) one of which is the other or under it.
func basesOverlap(a, b string) bool {
	return a == "" || b == "" || a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}
