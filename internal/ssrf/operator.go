package ssrf

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Operator-configured endpoints (spec/relay.md §10.3, D284): the Admission hook (hooks.admission.url), the MCP
// authorization server (auth.mcp_oauth.issuer, its RFC 8414 or OpenID metadata and the jwks_uri that metadata names)
// and the bucket are the operator's own infrastructure, written in kontra.yaml and never derived from a request, a
// card or a part. A team's control plane or IdP is often on a private network, so NewOperatorClient skips the
// address check of NewClient; everything else of §10.3 holds: https:// only, except http:// on a loopback host,
// which is then connected to a loopback address only; no redirect is followed (a 3xx is returned as is and its
// target receives no connection); the environment's proxy is ignored; every request has a timeout; and a caller
// reads a body through ReadBody, bounded. The bucket keeps the scheme its URL is written with and its own client
// (internal/blob), which already ignores the proxy and follows no redirect.

// OperatorTimeout is a request's timeout when OperatorOptions names none (§10.3: 5 s).
const OperatorTimeout = 5 * time.Second

// MaxOperatorBody bounds a body from an operator-configured endpoint: the issuer's metadata and its key set (256 KiB
// each, §10.3). The Admission decision is bounded tighter by its reader (hooks.MaxDecisionBytes).
const MaxOperatorBody = 256 << 10

// ErrBodyTooLarge is a body longer than its bound.
var ErrBodyTooLarge = errors.New("ssrf: response body over its bound")

// OperatorOptions configures NewOperatorClient.
type OperatorOptions struct {
	// Timeout bounds each request, from the dial to the last byte of the body; zero is OperatorTimeout.
	Timeout time.Duration
	// TLS is the client TLS configuration; nil uses the system roots (SSL_CERT_FILE and SSL_CERT_DIR apply).
	TLS *tls.Config
}

// NewOperatorClient returns the client of §10.3's operator-configured endpoints: no address check, https:// (or
// http:// on a loopback host, dialed to loopback addresses only), no redirects, no proxy, HTTP/1.1 only, and the
// options' timeout on every request.
func NewOperatorClient(o OperatorOptions) *http.Client {
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = OperatorTimeout
	}
	return &http.Client{Transport: newOperatorTransport(o.TLS, timeout), CheckRedirect: NoRedirects, Timeout: timeout}
}

// operatorTransport routes https:// requests to a transport over a plain dialer and http:// requests (to a loopback
// host only, CheckOperatorURL) to one whose dialer refuses every address that is not loopback.
type operatorTransport struct {
	https    http.RoundTripper
	loopback http.RoundTripper
}

func newOperatorTransport(tc *tls.Config, timeout time.Duration) operatorTransport {
	plain := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
	loop := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second, Control: loopbackOnly}
	return operatorTransport{https: operatorHTTP(plain.DialContext, tc, timeout), loopback: operatorHTTP(loop.DialContext, tc, timeout)}
}

func operatorHTTP(dial func(ctx context.Context, network, address string) (net.Conn, error), tc *tls.Config, timeout time.Duration) *http.Transport {
	if tc != nil {
		tc = tc.Clone()
	}
	return &http.Transport{
		Proxy:                  nil, // the environment's proxy is always ignored (§10.3)
		DialContext:            dial,
		TLSClientConfig:        tc,
		ForceAttemptHTTP2:      false,
		TLSNextProto:           map[string]func(string, *tls.Conn) http.RoundTripper{}, // HTTP/1.1 only
		MaxIdleConns:           16,
		MaxIdleConnsPerHost:    4,
		IdleConnTimeout:        90 * time.Second,
		TLSHandshakeTimeout:    timeout,
		ResponseHeaderTimeout:  timeout,
		ExpectContinueTimeout:  time.Second,
		MaxResponseHeaderBytes: 64 << 10,
	}
}

// loopbackOnly is the http:// dialer's Control: the address the kernel is asked to connect to must be loopback.
func loopbackOnly(network, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil || !ap.Addr().Unmap().IsLoopback() {
		return fmt.Errorf("%w: http:// is dialed to a loopback address only, not %s", ErrAddress, address)
	}
	return nil
}

func (t operatorTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if err := CheckOperatorURL(r.URL); err != nil {
		if r.Body != nil {
			r.Body.Close()
		}
		return nil, err
	}
	if strings.EqualFold(r.URL.Scheme, "http") {
		return t.loopback.RoundTrip(r)
	}
	return t.https.RoundTrip(r)
}

// CheckOperatorURL is §10.3's rule for an operator-configured endpoint: an absolute https:// URL, or http:// on a
// loopback host; no userinfo; a port from 1 to 65535 if any. It never resolves or connects.
func CheckOperatorURL(u *url.URL) error {
	if u == nil {
		return fmt.Errorf("%w: no URL", ErrURL)
	}
	if u.User != nil {
		return fmt.Errorf("%w: userinfo in a URL", ErrURL)
	}
	if u.Opaque != "" || u.Hostname() == "" {
		return fmt.Errorf("%w: no host", ErrURL)
	}
	if port := u.Port(); port != "" {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("%w: port %q", ErrURL, port)
		}
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return nil
	case "http":
		if IsLoopbackHost(u.Hostname()) {
			return nil
		}
		return fmt.Errorf("%w: http:// only to a loopback host (an operator-configured endpoint is https://)", ErrScheme)
	}
	return fmt.Errorf("%w: %q", ErrScheme, u.Scheme)
}

// IsLoopbackHost reports whether host is localhost or a loopback address literal (127.0.0.0/8, ::1, and their
// IPv4-mapped forms).
func IsLoopbackHost(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	if h == "localhost" {
		return true
	}
	a, err := netip.ParseAddr(strings.Trim(h, "[]"))
	return err == nil && a.Zone() == "" && a.Unmap().IsLoopback()
}

// ReadBody reads all of r, at most limit bytes: a longer body is ErrBodyTooLarge, and nothing past limit+1 bytes is
// read.
func ReadBody(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%w: more than %d bytes", ErrBodyTooLarge, limit)
	}
	return b, nil
}
