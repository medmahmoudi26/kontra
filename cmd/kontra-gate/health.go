package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

// DefaultHealthURL is what `kontra-gate health` asks without --url: the compose team gate's own public listener
// (compose.yaml: listen :8443, TLS from a provided certificate).
const DefaultHealthURL = "https://127.0.0.1:8443/healthz"

// health asks a gate's /healthz (spec §2.1) and succeeds only on 200. It is the container healthcheck of the
// compose team gate (D168): the image is distroless, with no shell or curl. A loopback URL is this container's own
// listener, so its certificate, issued for public_url's name, is not verified; any other host is verified as
// usual. No proxy is used, no redirect is followed, and the probe gives up after 5 s.
func health(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("health", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	raw := fs.String("url", DefaultHealthURL, "the /healthz URL to ask")
	if err := fs.Parse(args); err != nil {
		return usageError{"health: " + err.Error()}
	}
	if fs.NArg() > 0 {
		return usageError{"health: unexpected argument " + fs.Arg(0)}
	}
	u, err := url.Parse(*raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
		return usageError{fmt.Sprintf("health: --url %q is not an http(s) URL", *raw)}
	}
	tr := &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: loopbackHost(u.Hostname())}}
	defer tr.CloseIdleConnections()
	c := &http.Client{Transport: tr, Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("health: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health: %s answered %s", u.Redacted(), resp.Status)
	}
	fmt.Fprintln(stdout, "ok")
	return nil
}

// loopbackHost reports whether host is localhost or a loopback address.
func loopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
