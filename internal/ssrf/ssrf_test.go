package ssrf

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"golang.org/x/net/http/httpproxy"
)

// countingListener is a loopback listener that records every connection it accepts.
type countingListener struct {
	net.Listener
	accepts atomic.Int64
}

func listen(t *testing.T) *countingListener {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cl := &countingListener{Listener: l}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			cl.accepts.Add(1)
			c.Close()
		}
	}()
	t.Cleanup(func() { l.Close() })
	return cl
}

func (l *countingListener) port() string {
	_, p, _ := net.SplitHostPort(l.Addr().String())
	return p
}

// fakeResolver answers from a table, like DNS a seller or a caller controls.
type fakeResolver map[string][]string

func (r fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	addrs, ok := r[host]
	if !ok {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	var out []netip.Addr
	for _, a := range addrs {
		out = append(out, netip.MustParseAddr(a))
	}
	return out, nil
}

// refused is every address the acceptance names, plus the rest of the registries' ranges.
var refused = []string{
	"127.0.0.1", "10.0.0.1", "169.254.169.254", "100.100.100.200", "::1", "::ffff:10.0.0.1",
	"64:ff9b::a00:1",                       // NAT64 embedding 10.0.0.1
	"2002:a00:1::1",                        // 6to4 embedding 10.0.0.1
	"2001:0:a00:1::1",                      // Teredo whose server is 10.0.0.1
	"2001:0:4136:e378:8000:63bf:f5ff:fffe", // Teredo whose (obfuscated) client is 10.0.0.1
	"0.0.0.0", "172.16.5.5", "192.168.1.1", "100.64.0.1", "198.18.0.1", "240.0.0.1", "255.255.255.255",
	"224.0.0.1", "192.0.2.1", "198.51.100.7", "203.0.113.9", "192.0.0.8", "192.88.99.1", "168.63.129.16",
	"::", "fe80::1", "fc00::1", "fd00:ec2::254", "ff02::1", "2001:db8::1", "3fff::1", "64:ff9b:1::a00:1",
	"::a00:1", "100::1", "2001:2::1", "5f00::1", "::ffff:127.0.0.1",
}

var allowed = []string{
	"8.8.8.8", "1.1.1.1", "192.0.0.9", "2606:4700:4700::1111", "2a00:1450:4001:80b::200e",
	"64:ff9b::808:808",                     // NAT64 embedding 8.8.8.8
	"2002:808:808::1",                      // 6to4 embedding 8.8.8.8
	"2001:0:4136:e378:8000:63bf:f7f7:f7f7", // Teredo, server 65.54.227.120, client 8.8.8.8
	"2001:4:112::1", "::ffff:8.8.8.8",
}

func TestCheckAddr(t *testing.T) {
	for _, s := range refused {
		if err := CheckAddr(netip.MustParseAddr(s)); !errors.Is(err, ErrAddress) {
			t.Errorf("%s: %v, want ErrAddress", s, err)
		}
	}
	for _, s := range allowed {
		if err := CheckAddr(netip.MustParseAddr(s)); err != nil {
			t.Errorf("%s: %v, want allowed", s, err)
		}
	}
	if err := CheckAddr(netip.MustParseAddr("fe80::1%eth0")); !errors.Is(err, ErrAddress) {
		t.Errorf("a scoped address: %v", err)
	}
}

// Every refused address, and every name resolving to one, is refused before any connection: the dial hook
// records zero attempts and the loopback listener zero accepts (spec §10.3, acceptance).
func TestDialRefusesBeforeConnecting(t *testing.T) {
	l := listen(t)
	res := fakeResolver{
		"private.test": {"127.0.0.1"},
		"mixed.test":   {"8.8.8.8", "10.0.0.1"}, // one refused address refuses the name
		"meta.test":    {"169.254.169.254"},
		"v6.test":      {"::ffff:10.0.0.1"},
	}
	d := NewDialer(Policy{Resolver: res})
	var attempts atomic.Int64
	inner := d.dial
	d.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		attempts.Add(1)
		return inner(ctx, network, address)
	}
	targets := append([]string{}, refused...)
	targets = append(targets, "private.test", "mixed.test", "meta.test", "v6.test", "localhost")
	ctx := context.Background()
	for _, host := range targets {
		addr := net.JoinHostPort(host, l.port())
		if host == "localhost" {
			// The system resolver: localhost is loopback on every host.
			d.policy.Resolver = nil
		}
		c, err := d.DialContext(ctx, "tcp", addr)
		if c != nil {
			c.Close()
		}
		if !errors.Is(err, ErrAddress) {
			t.Errorf("dial %s: %v, want ErrAddress", addr, err)
		}
		d.policy.Resolver = res
	}
	if n := attempts.Load(); n != 0 {
		t.Errorf("%d connection attempts reached the network", n)
	}
	if n := l.accepts.Load(); n != 0 {
		t.Errorf("the listener accepted %d connections", n)
	}

	// The same through the HTTP client, and through CheckURL (set time), which never connects.
	client := NewClient(Policy{Resolver: res})
	for _, u := range []string{
		"https://127.0.0.1:" + l.port() + "/", "https://[::1]:" + l.port() + "/",
		"https://private.test:" + l.port() + "/", "https://[::ffff:10.0.0.1]/", "https://169.254.169.254/latest/meta-data",
	} {
		resp, err := client.Get(u)
		if resp != nil {
			resp.Body.Close()
		}
		if !errors.Is(err, ErrAddress) {
			t.Errorf("GET %s: %v, want ErrAddress", u, err)
		}
		pu, _ := url.Parse(u)
		if err := CheckURL(ctx, pu, Policy{Resolver: res}); !errors.Is(err, ErrAddress) {
			t.Errorf("CheckURL %s: %v, want ErrAddress", u, err)
		}
	}
	if n := l.accepts.Load(); n != 0 {
		t.Errorf("the listener accepted %d connections through the client", n)
	}
}

func TestDialAllowsPublicAndPins(t *testing.T) {
	d := NewDialer(Policy{Resolver: fakeResolver{"public.test": {"8.8.8.8", "2606:4700:4700::1111"}}})
	var got []string
	d.dial = func(_ context.Context, _, address string) (net.Conn, error) {
		got = append(got, address)
		return nil, errors.New("test: no network")
	}
	_, err := d.DialContext(context.Background(), "tcp", "public.test:443")
	if err == nil || errors.Is(err, ErrAddress) {
		t.Fatalf("dial: %v", err)
	}
	// Each checked address is dialed by its literal: the name is never resolved a second time.
	if strings.Join(got, ",") != "8.8.8.8:443,[2606:4700:4700::1111]:443" {
		t.Errorf("dialed %v", got)
	}
}

// http:// is refused unless allow_private (D67); other schemes and userinfo always.
func TestSchemes(t *testing.T) {
	l := listen(t)
	ctx := context.Background()
	for _, tc := range []struct {
		raw     string
		private bool
		want    error
	}{
		{"http://127.0.0.1:" + l.port() + "/", false, ErrScheme},
		{"http://example.com/", false, ErrScheme},
		{"ftp://example.com/", true, ErrScheme},
		{"file:///etc/passwd", true, ErrScheme},
		{"https://user:pw@example.com/", true, ErrURL},
		{"https:///nohost", true, ErrURL},
		{"https://example.com:99999/", true, ErrURL},
		{"http://127.0.0.1:" + l.port() + "/", true, nil},
		{"http://10.0.0.1/", true, nil},
	} {
		u, err := url.Parse(tc.raw)
		if err != nil {
			t.Fatal(err)
		}
		if err := CheckURL(ctx, u, Policy{AllowPrivate: tc.private}); !errors.Is(err, tc.want) && !(tc.want == nil && err == nil) {
			t.Errorf("CheckURL(%s, allow_private=%v): %v, want %v", tc.raw, tc.private, err, tc.want)
		}
	}
	// The client applies the rule per request, before any connection.
	resp, err := NewClient(Policy{}).Get("http://127.0.0.1:" + l.port() + "/")
	if resp != nil {
		resp.Body.Close()
	}
	if !errors.Is(err, ErrScheme) {
		t.Errorf("http:// without allow_private: %v", err)
	}
	if n := l.accepts.Load(); n != 0 {
		t.Errorf("the listener accepted %d connections", n)
	}
	if err := CheckURL(ctx, mustURL(t, "https://example.com/hook"), Policy{Resolver: fakeResolver{"example.com": {"93.184.215.14"}}}); err != nil {
		t.Errorf("a public https URL: %v", err)
	}
}

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// A 3xx is returned as the response and its target receives no connection (§10.2, §10.3).
func TestRedirectNotFollowed(t *testing.T) {
	var targetHits atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		fmt.Fprint(w, "target")
	}))
	defer target.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/landed", http.StatusFound)
	}))
	defer redirector.Close()
	for _, code := range []int{301, 302, 303, 307, 308} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL, code)
		}))
		resp, err := NewClient(Policy{AllowPrivate: true}).Post(srv.URL, "application/json", strings.NewReader("{}"))
		srv.Close()
		if err != nil {
			t.Fatalf("%d: %v", code, err)
		}
		resp.Body.Close()
		if resp.StatusCode != code {
			t.Errorf("status %d, want the %d itself", resp.StatusCode, code)
		}
	}
	resp, err := NewClient(Policy{AllowPrivate: true}).Get(redirector.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || targetHits.Load() != 0 {
		t.Errorf("status %d, target hits %d: the redirect was followed", resp.StatusCode, targetHits.Load())
	}
}

// HTTPS_PROXY, HTTP_PROXY and ALL_PROXY are ignored: the request goes to its own host, the proxy receives
// no connection (§10.3, D17: a2a-go's push sender honours the environment).
func TestProxyIgnored(t *testing.T) {
	proxy := listen(t)
	var targetHits atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		io.WriteString(w, "direct")
	}))
	defer target.Close()
	_, targetPort, _ := net.SplitHostPort(strings.TrimPrefix(target.URL, "http://"))
	proxyURL := "http://127.0.0.1:" + proxy.port()
	for _, k := range []string{"HTTPS_PROXY", "HTTP_PROXY", "ALL_PROXY", "https_proxy", "http_proxy"} {
		t.Setenv(k, proxyURL)
	}
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")

	// upstream.test is not loopback by name, so the environment's proxy would apply to it (the control
	// below), while the dialer maps it to the test server.
	reqURL := "http://upstream.test:" + targetPort + "/"
	if p, err := httpproxy.FromEnvironment().ProxyFunc()(mustURL(t, reqURL)); err != nil || p == nil {
		t.Fatalf("control: the environment does not proxy %s (%v, %v)", reqURL, p, err)
	}
	pol := Policy{AllowPrivate: true}
	d := NewDialer(pol)
	d.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address == "upstream.test:"+targetPort {
			address = "127.0.0.1:" + targetPort
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	client := &http.Client{Transport: NewTransportWithDial(pol, d.DialContext), CheckRedirect: NoRedirects}
	resp, err := client.Get(reqURL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "direct" || targetHits.Load() != 1 {
		t.Errorf("body %q, target hits %d", body, targetHits.Load())
	}
	if n := proxy.accepts.Load(); n != 0 {
		t.Errorf("the proxy accepted %d connections", n)
	}
}

// errResolver fails every lookup with err.
type errResolver struct{ err error }

func (r errResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return nil, r.err
}

// TestLookupFailuresAreClassified: a name that does not exist (or has no address) is ErrNoHost, the URL's
// fault; any other lookup failure (a timeout, SERVFAIL) is ErrLookup, the gate's; the resolver's own
// error stays in the chain, so a dial error still reads as a DNS failure.
func TestLookupFailuresAreClassified(t *testing.T) {
	ctx := context.Background()
	u := mustURL(t, "https://hooks.example/cb")
	for _, tc := range []struct {
		name string
		res  Resolver
		want error
	}{
		{"nxdomain", fakeResolver{}, ErrNoHost},
		{"no address", fakeResolver{"hooks.example": nil}, ErrNoHost},
		{"servfail", errResolver{&net.DNSError{Err: "server misbehaving", Name: "hooks.example", Server: "10.0.0.53:53", IsTemporary: true}}, ErrLookup},
		{"timeout", errResolver{&net.DNSError{Err: "i/o timeout", Name: "hooks.example", IsTimeout: true}}, ErrLookup},
		{"other", errResolver{errors.New("resolver down")}, ErrLookup},
	} {
		err := CheckURL(ctx, u, Policy{Resolver: tc.res})
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.want)
		}
		if errors.Is(err, ErrAddress) {
			t.Errorf("%s: %v is an address refusal", tc.name, err)
		}
		var dnsErr *net.DNSError
		if _, ok := tc.res.(errResolver); ok && tc.name != "other" && !errors.As(err, &dnsErr) {
			t.Errorf("%s: the resolver's error left the chain: %v", tc.name, err)
		}
	}
}
