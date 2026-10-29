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

	"kontra.sh/kontra/internal/config"
)

// DefaultHealthURL is what `kontra-gate health` asks without --url: the compose team gate's own public listener
// (compose.yaml: listen :8443, TLS from a provided certificate or built-in ACME).
const DefaultHealthURL = "https://127.0.0.1:8443/healthz"

// health asks a gate's /healthz (spec §2.1) and succeeds only on 200. It is the container healthcheck of the
// compose team gate (D168): the image is distroless, with no shell or curl. A loopback URL is this container's own
// listener, so its certificate, issued for public_url's name, is not verified; any other host is verified as
// usual. No proxy is used, no redirect is followed, and the probe gives up after 5 s.
//
// The TLS server name (SNI) it sends is --server-name, else the host of public_url in the configuration -f names,
// else the URL's host (none for an IP address). Built-in ACME (autocert) answers a handshake without a server name,
// or with one it does not issue for, with an alert, so the compose healthcheck of the ACME variant names the gate's
// configuration (DM4-14; the M4 plan's audit, gaps 17). A provided certificate is served whatever the name.
func health(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("health", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	raw := fs.String("url", DefaultHealthURL, "the /healthz URL to ask")
	serverName := fs.String("server-name", "", "the TLS server name (SNI) to send")
	file := fs.String("f", "", "the gate's configuration: send its public_url host as the TLS server name")
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
	name := *serverName
	if name == "" && *file != "" {
		if name, err = publicHost(*file); err != nil {
			return fmt.Errorf("health: %w", err)
		}
	}
	tr := &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{
		MinVersion:         tls.VersionTLS12,
		ServerName:         name, // empty: the URL's host, as net/http does
		InsecureSkipVerify: loopbackHost(u.Hostname()),
	}}
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

// publicHost is the host of public_url in the configuration file at path (loaded and validated as serve loads it).
func publicHost(path string) (string, error) {
	c, err := config.Load(path)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(c.PublicURL)
	if err != nil || u.Hostname() == "" {
		return "", fmt.Errorf("%s: public_url %q has no host", path, c.PublicURL)
	}
	return u.Hostname(), nil
}

// loopbackHost reports whether host is localhost or a loopback address.
func loopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
