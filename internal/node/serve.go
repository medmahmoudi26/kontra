package node

import (
	"net/url"

	"kontra.sh/kontra/internal/unimpl"
)

// Serve is one --serve kind:name=target (note 2 §1). M2 accepts a2a:<name>=<http(s) URL> only; mcp: is
// refused with "not until M3".
type Serve struct {
	Kind   string   // tunnel.KindA2A
	Name   string   // the service name at the gate (relay.md §2.1's pattern)
	Target *url.URL // scheme://host[:port][/base]: every request goes here and nowhere else (§8.2)
}

// ParseServe parses one --serve value. A target whose path is not empty or "/" is refused in M2 (see the
// plan's spec conflict: the node prefixes the target's base path, and the gate sends the upstream card's
// interface path, which already holds it).
func ParseServe(s string) (Serve, error) {
	return Serve{}, unimpl.Error("node.ParseServe", "WP-06")
}
