package gate

import (
	"context"
	"errors"
	"net/netip"
	"net/url"
	"slices"
	"testing"

	"kontra.sh/kontra/internal/ssrf"
)

// The e2e build's fixed names (KONTRA_E2E_HOSTS): a name mapped to a private address is refused by the
// push SSRF policy as an address, not as a name that does not resolve, so ssrf_webhook's "a name resolving
// to a private address" row runs on any machine (spec §16, D80).
func TestE2EHostsResolver(t *testing.T) {
	h, err := parseE2EHosts("webhook.private.e2e.invalid=10.0.0.1, Dual.E2E.invalid=fd00::1,dual.e2e.invalid=10.0.0.2")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	u, _ := url.Parse("https://webhook.private.e2e.invalid/hook")
	if err := ssrf.CheckURL(ctx, u, ssrf.Policy{Resolver: h}); !errors.Is(err, ssrf.ErrAddress) {
		t.Errorf("a name resolving to 10.0.0.1: %v, want ErrAddress", err)
	}
	for network, want := range map[string][]netip.Addr{
		"ip":  {netip.MustParseAddr("fd00::1"), netip.MustParseAddr("10.0.0.2")},
		"ip4": {netip.MustParseAddr("10.0.0.2")},
		"ip6": {netip.MustParseAddr("fd00::1")},
	} {
		if got, err := h.LookupNetIP(ctx, network, "dual.e2e.invalid."); err != nil || !slices.Equal(got, want) {
			t.Errorf("%s: %v %v, want %v", network, got, err, want)
		}
	}
	for _, bad := range []string{"x", "=10.0.0.1", "x=not-an-ip", "x=10.0.0.1,"} {
		if _, err := parseE2EHosts(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}
