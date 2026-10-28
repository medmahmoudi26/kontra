package anon_test

import (
	"errors"
	"net/netip"
	"regexp"
	"testing"
	"time"

	"github.com/google/uuid"

	"kontra.sh/kontra/internal/anon"
	"kontra.sh/kontra/internal/config"
	"kontra.sh/kontra/internal/enroll"
)

// spec/tunnel.md §6.2: a client is counted and stored by its IPv4 /24 or its IPv6 /48; an IPv4-mapped IPv6 address
// is its IPv4 address; never the address itself.
func TestNetwork(t *testing.T) {
	for in, want := range map[string]string{
		"203.0.113.7":                 "203.0.113.0/24",
		"203.0.113.255":               "203.0.113.0/24",
		"10.1.2.3":                    "10.1.2.0/24",
		"::ffff:203.0.113.7":          "203.0.113.0/24",
		"::ffff:cb00:7107":            "203.0.113.0/24",
		"2001:db8:abcd:12:34::1":      "2001:db8:abcd::/48",
		"2001:db8:abcd:ffff:ffff::ff": "2001:db8:abcd::/48",
		"fe80::1%eth0":                "fe80::/48",
		"::1":                         "::/48",
	} {
		got := anon.Network(netip.MustParseAddr(in))
		if got.String() != want {
			t.Errorf("Network(%s) = %s, want %s", in, got, want)
		}
		if got.Addr().String() == netip.MustParseAddr(in).Unmap().WithZone("").String() && got.Bits() < 128 && in != "::1" {
			// The network's address is the address only when the host bits are zero, which these are not.
			t.Errorf("Network(%s) keeps the address", in)
		}
	}
	// Two clients of one /24, and of one /48, share a network; the next /24 and /48 do not.
	same := func(a, b string) bool {
		return anon.Network(netip.MustParseAddr(a)) == anon.Network(netip.MustParseAddr(b))
	}
	if !same("198.51.100.1", "198.51.100.254") || same("198.51.100.1", "198.51.101.1") ||
		!same("2001:db8:1::1", "2001:db8:1:ffff::2") || same("2001:db8:1::1", "2001:db8:2::1") ||
		!same("::ffff:198.51.100.9", "198.51.100.10") {
		t.Error("the networks do not group as /24 and /48")
	}
	if p := anon.Network(netip.Addr{}); p.IsValid() {
		t.Errorf("an invalid address has the network %s", p)
	}
}

// NewSlug mints anon-<16 lowercase hex>, the shape migration 0003's CHECK reserves to anonymous tenants (DM4-20),
// which IsSlug recognizes; slugs do not repeat.
func TestNewSlug(t *testing.T) {
	check := regexp.MustCompile(`^anon-[0-9a-f]{16}$`) // 0003's anonymous_slug
	name := config.NameRE                              // 0001's tenants.slug CHECK
	seen := map[string]bool{}
	for range 1000 {
		s, err := anon.NewSlug()
		if err != nil {
			t.Fatal(err)
		}
		if !check.MatchString(s) || !name.MatchString(s) || !anon.IsSlug(s) {
			t.Fatalf("slug %q", s)
		}
		if seen[s] {
			t.Fatalf("slug %q drawn twice", s)
		}
		seen[s] = true
	}
	for _, s := range []string{"anon-7f3a", "anon-0123456789ABCDEF", "anon-0123456789abcdef0", "xanon-0123456789abcdef", "default",
		"anonymous-users", "anon-0123456789abcdeg"} {
		if anon.IsSlug(s) {
			t.Errorf("IsSlug(%q)", s)
		}
	}
}

// tunnel.anonymous maps to Limits, every key left out taking §6.2's default.
func TestLimitsFrom(t *testing.T) {
	if got := anon.LimitsFrom(config.AnonymousSessions{Enabled: true}); got != anon.Defaults {
		t.Errorf("defaults %+v, want %+v", got, anon.Defaults)
	}
	d := anon.Defaults
	if d.TTL != 2*time.Hour || d.TenantsPerNetwork != 3 || d.Services != 4 || d.Tasks != 4 || d.Calls != 8 || d.HiringStreams != 8 ||
		d.PartsBytes != 64<<20 || d.PurgeAfter != 24*time.Hour || d.MaxTenants != 1000 {
		t.Errorf("§6.2's defaults: %+v", d)
	}
	got := anon.LimitsFrom(config.AnonymousSessions{Enabled: true, TTL: config.Duration(20 * time.Second), Tasks: 2, Calls: 3,
		MaxTenants: 200, PartsBytes: config.ByteSize(1 << 20)})
	want := anon.Defaults
	want.TTL, want.Tasks, want.Calls, want.MaxTenants, want.PartsBytes = 20*time.Second, 2, 3, 200, 1<<20
	if got != want {
		t.Errorf("LimitsFrom = %+v, want %+v", got, want)
	}
}

// The gate-internal NOTIFY payloads: an ended tenant's (kontra_tunnel), and a binding revocation's, which must be the
// one internal/enroll's revocation view reads (kontra_revoke).
func TestPayloads(t *testing.T) {
	id := uuid.Must(uuid.NewV7())
	if got, ok := anon.ParseExpiredPayload(anon.ExpiredPayload(id)); !ok || got != id {
		t.Errorf("expired payload round trip: %v %v", got, ok)
	}
	for _, bad := range []string{"expired:", "expired:x", "session:" + id.String(), "expired:" + id.String() + "/x",
		"expired:{" + id.String() + "}"} {
		if _, ok := anon.ParseExpiredPayload(bad); ok {
			t.Errorf("ParseExpiredPayload(%q) accepted it", bad)
		}
	}
	if a, e := anon.BindingRevokePayload("k1_abcdefghijklmnopqrstuvwxyz", "hello"), enroll.BindingRevokePayload("k1_abcdefghijklmnopqrstuvwxyz", "hello"); a != e {
		t.Errorf("binding payload %q, enroll's %q", a, e)
	}
}

// Every runtime refusal the relays answer 429 wraps ErrRateLimited; the storage refusal does not (it is
// InvalidParams); the handshake refusals are distinct.
func TestRefusals(t *testing.T) {
	for _, e := range []error{anon.ErrTooManyTasks, anon.ErrTooManyCalls} {
		if !errors.Is(e, anon.ErrRateLimited) {
			t.Errorf("%v does not wrap ErrRateLimited", e)
		}
	}
	for _, e := range []error{anon.ErrStorageFull, anon.ErrUnavailable, anon.ErrTooManyTenants, anon.ErrTooManyServices,
		anon.ErrNotDeclared, anon.ErrKindMismatch} {
		if errors.Is(e, anon.ErrRateLimited) {
			t.Errorf("%v wraps ErrRateLimited", e)
		}
	}
	if anon.ErrStorageFull.Error() != "anonymous storage limit" || anon.ErrRateLimited.Error() != "rate limited" {
		t.Error("the refusals' words are §6.2's")
	}
}

// A nil *Caps (a gate without tunnel.anonymous) checks nothing.
func TestNilCaps(t *testing.T) {
	var c *anon.Caps
	if err := c.Task(t.Context(), nil, uuid.Nil, "anon-0123456789abcdef", "node:k1_x"); err != nil {
		t.Error(err)
	}
	release, err := c.Call(t.Context(), uuid.Nil, "anon-0123456789abcdef")
	if err != nil || release == nil {
		t.Fatal(err)
	}
	release()
	if err := c.Message(t.Context(), uuid.Nil, "anon-0123456789abcdef", 1<<40); err != nil {
		t.Error(err)
	}
	if _, limit, applies, err := c.Budget(t.Context(), uuid.Nil, "anon-0123456789abcdef", uuid.Nil, 7); err != nil || applies || limit != 7 {
		t.Errorf("budget %d %v %v", limit, applies, err)
	}
}
