package gate

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strings"
)

// E2EHostsEnv names, in a gate built with the kontra_e2e build tag (test/e2e builds it so; a release
// never is), host names the push SSRF policy resolves to fixed addresses: "name=addr[,name=addr…]". The
// e2e ssrf_webhook test needs "a name resolving to a private address" (spec §16, D80) on any machine, and
// /etc/hosts names one on none of them. Every other name resolves as usual. A gate built without the tag
// never reads it (e2e_hosts_default.go).
const E2EHostsEnv = "KONTRA_E2E_HOSTS"

// e2eHosts is a Resolver over fixed names, falling back to net.DefaultResolver.
type e2eHosts map[string][]netip.Addr

func parseE2EHosts(spec string) (e2eHosts, error) {
	h := e2eHosts{}
	for entry := range strings.SplitSeq(spec, ",") {
		name, addr, ok := strings.Cut(strings.TrimSpace(entry), "=")
		a, err := netip.ParseAddr(addr)
		if !ok || name == "" || err != nil {
			return nil, fmt.Errorf("%q: want name=address", entry)
		}
		h[strings.ToLower(name)] = append(h[strings.ToLower(name)], a)
	}
	return h, nil
}

func (h e2eHosts) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	addrs, ok := h[strings.ToLower(strings.TrimSuffix(host, "."))]
	if !ok {
		return net.DefaultResolver.LookupNetIP(ctx, network, host)
	}
	var out []netip.Addr
	for _, a := range addrs {
		if network == "ip" || (network == "ip4") == a.Is4() {
			out = append(out, a)
		}
	}
	return out, nil
}
