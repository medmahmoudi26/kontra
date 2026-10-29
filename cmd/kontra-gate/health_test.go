package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"kontra.sh/kontra/internal/edge"
)

// healthDomain is the name the ACME tests issue for: a DNS name autocert accepts, never resolved.
const healthDomain = "gate.kontra.test"

// seedACMECache writes a certificate for domain into dir as autocert's directory cache stores one (the key's PEM,
// then the chain's), under the name autocert looks an ECDSA certificate up by. The certificate is self-signed: autocert
// checks a cached certificate's name, validity and key, not its issuer.
func seedACMECache(t *testing.T, dir, domain string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: domain},
		DNSNames:     []string{domain},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(90 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	b := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder})
	b = append(b, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, domain), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// acmeHealthz serves /healthz (200) over TLS from the gate's own built-in ACME (internal/edge, autocert) with a cache
// that already holds healthDomain's certificate; its ACME directory is a closed port, so nothing is ever ordered.
func acmeHealthz(t *testing.T) (port string) {
	t.Helper()
	cache := filepath.Join(t.TempDir(), "acme")
	seedACMECache(t, cache, healthDomain)
	a, err := edge.NewACME(edge.ACMEConfig{Domains: []string{healthDomain}, Cache: edge.DirCache(cache),
		DirectoryURL: "https://127.0.0.1:1/directory", HTTPClient: &http.Client{Timeout: time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", a.TLSConfig())
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	}), ErrorLog: log.New(io.Discard, "", 0), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	_, port, _ = net.SplitHostPort(ln.Addr().String())
	return port
}

// writeVPSConfig writes deploy/compose/kontra.vps.yaml with public_url set to https://<host> (and its ACME domain with
// it) into dir, as an operator edits it, and returns its path.
func writeVPSConfig(t *testing.T, dir, host string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "deploy", "compose", "kontra.vps.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	s := strings.ReplaceAll(string(b), "gate.example.com", host)
	if s == string(b) {
		t.Fatal("deploy/compose/kontra.vps.yaml no longer names gate.example.com, the host an operator replaces")
	}
	p := filepath.Join(dir, "kontra.yaml")
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// composeHealthcheck is the gate's healthcheck in compose.yaml: its arguments after the binary, with the URL's port
// replaced by port and the configuration path by cfg.
func composeHealthcheck(t *testing.T, port, cfg string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		Services map[string]struct {
			Healthcheck struct {
				Test []string `yaml:"test"`
			} `yaml:"healthcheck"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	test := c.Services["gate"].Healthcheck.Test
	if len(test) < 3 || test[0] != "CMD" || test[1] != "/usr/local/bin/kontra-gate" || test[2] != "health" {
		t.Fatalf("compose.yaml's gate healthcheck %q is not CMD kontra-gate health …", test)
	}
	args := append([]string(nil), test[2:]...)
	var sawURL, sawFile bool
	for i := 0; i+1 < len(args); i++ {
		switch args[i] {
		case "--url":
			u, err := url.Parse(args[i+1])
			if err != nil {
				t.Fatal(err)
			}
			if !loopbackHost(u.Hostname()) || u.Port() != "8443" {
				t.Errorf("the healthcheck asks %s, want the container's own listener on 8443", u)
			}
			u.Host = net.JoinHostPort(u.Hostname(), port)
			args[i+1], sawURL = u.String(), true
		case "-f":
			if args[i+1] != "/etc/kontra/kontra.yaml" {
				t.Errorf("the healthcheck reads %s, want the mounted configuration", args[i+1])
			}
			args[i+1], sawFile = cfg, true
		}
	}
	if !sawURL || !sawFile {
		t.Fatalf("compose.yaml's gate healthcheck %q: want --url and -f (the server name from public_url)", test)
	}
	return args
}

// The compose ACME variant's healthcheck sends the gate's name (the M4 plan's audit, gaps 17; D281): built-in ACME
// refuses a handshake without a server name, so health without one fails; with --server-name, or with -f naming a
// configuration whose public_url is that name, it succeeds, skipping verification on the loopback dial; and the
// healthcheck compose.yaml runs, against kontra.vps.yaml as an operator edits it, succeeds.
func TestHealthSendsTheServerName(t *testing.T) {
	port := acmeHealthz(t)
	u := "https://127.0.0.1:" + port + "/healthz"

	_, err := runCmd(t, "health", "--url", u)
	if err == nil {
		t.Fatal("health without a server name succeeded against built-in ACME (the test server does not refuse it)")
	}
	if !strings.Contains(err.Error(), "tls") {
		t.Errorf("health without a server name: %v, want the TLS alert", err)
	}
	if _, err := runCmd(t, "health", "--url", u, "--server-name", "other.kontra.test"); err == nil {
		t.Error("health with a name autocert does not issue for succeeded")
	}
	if out, err := runCmd(t, "health", "--url", u, "--server-name", healthDomain); err != nil || strings.TrimSpace(out) != "ok" {
		t.Errorf("health --server-name %s = %q, %v", healthDomain, out, err)
	}

	dir := t.TempDir()
	cfg := writeVPSConfig(t, dir, healthDomain)
	if out, err := runCmd(t, "health", "--url", u, "-f", cfg); err != nil || strings.TrimSpace(out) != "ok" {
		t.Errorf("health -f (public_url https://%s) = %q, %v", healthDomain, out, err)
	}
	// --server-name wins over -f.
	if _, err := runCmd(t, "health", "--url", u, "-f", cfg, "--server-name", "other.kontra.test"); err == nil {
		t.Error("--server-name did not override -f's public_url")
	}
	args := composeHealthcheck(t, port, cfg)
	if out, err := runCmd(t, args...); err != nil || strings.TrimSpace(out) != "ok" {
		t.Errorf("compose.yaml's healthcheck %q against the ACME variant = %q, %v", args, out, err)
	}

	// A configuration that does not load is an error naming it, and no request is made.
	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte("listen: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runCmd(t, "health", "--url", u, "-f", bad); err == nil || !strings.Contains(err.Error(), "bad.yaml") {
		t.Errorf("health -f with a broken configuration = %v", err)
	}
	if _, err := runCmd(t, "health", "--url", u, "-f", filepath.Join(dir, "missing.yaml")); err == nil {
		t.Error("health -f with a missing configuration succeeded")
	}
}

// The provided-certificate variant (deploy/compose/kontra.yaml, public_url https://localhost:8443) keeps working with
// the same healthcheck: a static certificate is served whatever the server name, and the loopback dial skips
// verification.
func TestHealthProvidedCertificateVariant(t *testing.T) {
	srv := newStaticHealthz(t)
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv, "https://"))
	args := composeHealthcheck(t, port, filepath.Join("..", "..", "deploy", "compose", "kontra.yaml"))
	if out, err := runCmd(t, args...); err != nil || strings.TrimSpace(out) != "ok" {
		t.Errorf("compose.yaml's healthcheck %q against the provided certificate = %q, %v", args, out, err)
	}
}

// newStaticHealthz serves /healthz (200) over TLS with a fixed self-signed certificate, as a provided tls.cert/key is.
func newStaticHealthz(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "static")
	seedACMECache(t, dir, "gate.static.test")
	b, err := os.ReadFile(filepath.Join(dir, "gate.static.test"))
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(b, b)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}), ErrorLog: log.New(io.Discard, "", 0), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return "https://" + ln.Addr().String()
}
