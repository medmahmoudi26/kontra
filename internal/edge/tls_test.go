package edge

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"kontra.sh/kontra/internal/config"
)

// testCA is a throwaway certificate authority.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pool *x509.CertPool
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "kontra test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &testCA{cert: cert, key: key, pool: pool}
}

// leaf writes a certificate for names (DNS names and IP addresses) with the given serial, and its key,
// as PEM files in dir.
func (ca *testCA) leaf(t *testing.T, dir string, serial int64, names ...string) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: names[0]},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, n := range names {
		if ip := net.ParseIP(n); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, n)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certFile, keyFile = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	writePEM(t, certFile, "CERTIFICATE", der)
	writePEM(t, keyFile, "EC PRIVATE KEY", kder)
	return certFile, keyFile
}

func writePEM(t *testing.T, path, typ string, der []byte) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
}

func tlsClient(pool *x509.CertPool, serverName string) *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig:   &tls.Config{RootCAs: pool, ServerName: serverName},
			ForceAttemptHTTP2: true,
		},
	}
}

// NewStatic serves the provided certificate, over HTTP/2 and HTTP/1.1, with HSTS; a renewed certificate
// on disk is picked up without a restart; a broken pair is an error at start.
func TestStaticCertificate(t *testing.T) {
	ca := newTestCA(t)
	dir := t.TempDir()
	certFile, keyFile := ca.leaf(t, dir, 1001, "gate.example.test", "127.0.0.1")
	tlsCfg, sc, err := newStatic(certFile, keyFile, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	tr := newTestRoutes()
	_, addr := listen(t, Config{TLS: tlsCfg, PublicURL: "https://gate.example.test"}, tr.router)
	client := tlsClient(ca.pool, "gate.example.test")
	get := func() *http.Response {
		t.Helper()
		resp, err := client.Get("https://" + addr + "/healthz")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp
	}
	resp := get()
	if resp.StatusCode != 200 || resp.Header.Get("Strict-Transport-Security") != HSTS {
		t.Errorf("over TLS: %d, HSTS %q", resp.StatusCode, resp.Header.Get("Strict-Transport-Security"))
	}
	if resp.ProtoMajor != 2 {
		t.Errorf("negotiated %s, want HTTP/2", resp.Proto)
	}
	if s := resp.TLS.PeerCertificates[0].SerialNumber.Int64(); s != 1001 {
		t.Errorf("served certificate serial %d, want 1001", s)
	}
	if v := resp.TLS.Version; v < tls.VersionTLS12 {
		t.Errorf("TLS version %x", v)
	}
	// HTTP/1.1 only clients are served too.
	h1 := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: ca.pool, ServerName: "gate.example.test"}}}
	if resp, err := h1.Get("https://" + addr + "/healthz"); err != nil || resp.ProtoMajor != 1 {
		t.Errorf("HTTP/1.1 client: %v %v", resp, err)
	} else {
		resp.Body.Close()
	}
	// Plaintext on the TLS listener is not served.
	if resp, err := http.Get("http://" + addr + "/healthz"); err == nil && resp.StatusCode == 200 {
		t.Error("plaintext request served on the TLS listener")
	}

	// A renewed certificate: written in place, picked up at the next check.
	time.Sleep(10 * time.Millisecond) // a distinct mtime
	ca.leaf(t, dir, 1002, "gate.example.test", "127.0.0.1")
	sc.mu.Lock()
	sc.recheck, sc.lastCheck = 0, time.Time{}
	sc.mu.Unlock()
	client.CloseIdleConnections()
	if s := get().TLS.PeerCertificates[0].SerialNumber.Int64(); s != 1002 {
		t.Errorf("after renewal: serial %d, want 1002", s)
	}
	// A broken renewal keeps serving the last good certificate.
	if err := os.WriteFile(certFile, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	client.CloseIdleConnections()
	if s := get().TLS.PeerCertificates[0].SerialNumber.Int64(); s != 1002 {
		t.Errorf("after a broken renewal: serial %d, want 1002", s)
	}

	if _, err := NewStatic(filepath.Join(dir, "missing.pem"), keyFile); err == nil {
		t.Error("a missing certificate loads")
	}
	other := t.TempDir()
	otherCert, _ := ca.leaf(t, other, 1, "x.example.test")
	if _, err := NewStatic(otherCert, keyFile); err == nil {
		t.Error("a certificate with another certificate's key loads")
	}
}

// A gate behind a TLS-terminating proxy listens in plaintext and sends no HSTS (D69).
func TestTerminatedByProxyNoHSTS(t *testing.T) {
	tr := newTestRoutes()
	_, addr := listen(t, Config{PublicURL: "https://gate.example.com", TerminatedByProxy: true}, tr.router)
	resp, err := http.Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Strict-Transport-Security") != "" {
		t.Errorf("behind a proxy: %d HSTS %q", resp.StatusCode, resp.Header.Get("Strict-Transport-Security"))
	}
}

// TLSFromConfig picks the provided certificate, ACME, or nothing.
func TestTLSFromConfig(t *testing.T) {
	ca := newTestCA(t)
	certFile, keyFile := ca.leaf(t, t.TempDir(), 7, "gate.example.test")
	c, a, err := TLSFromConfig(config.TLS{Cert: certFile, Key: keyFile})
	if err != nil || c == nil || a != nil || c.GetCertificate == nil {
		t.Errorf("cert/key: %v %v %v", c, a, err)
	}
	cacheDir := filepath.Join(t.TempDir(), "acme")
	c, a, err = TLSFromConfig(config.TLS{ACME: &config.ACME{Domains: []string{"gate.example.test"}, CacheDir: cacheDir}})
	if err != nil || c == nil || a == nil || !strings.Contains(strings.Join(c.NextProtos, ","), "acme-tls/1") {
		t.Errorf("acme: %v %v %v", c, a, err)
	}
	if fi, err := os.Stat(cacheDir); err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Errorf("acme cache directory: %v %v", fi, err)
	}
	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := TLSFromConfig(config.TLS{ACME: &config.ACME{Domains: []string{"gate.example.test"}, CacheDir: filepath.Join(blocked, "acme")}}); err == nil {
		t.Error("an unusable cache directory is accepted")
	}
	c, a, err = TLSFromConfig(config.TLS{TerminatedByProxy: true})
	if err != nil || c != nil || a != nil {
		t.Errorf("proxy: %v %v %v", c, a, err)
	}
	if _, err := NewACME(ACMEConfig{Domains: []string{"localhost"}, Cache: DirCache(t.TempDir())}); err == nil {
		t.Error("ACME for a single-label name")
	}
	if _, err := NewACME(ACMEConfig{Domains: []string{"gate.example.test"}}); err == nil {
		t.Error("ACME without a cache")
	}
}

// pebbleBinary is the ACME test server: $KONTRA_TEST_PEBBLE, else pebble on PATH (pinned in CI:
// github.com/letsencrypt/pebble/v2/cmd/pebble@v2.10.1).
func pebbleBinary(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test: runs a Pebble ACME server; skipped under -short")
	}
	if p := os.Getenv("KONTRA_TEST_PEBBLE"); p != "" {
		return p
	}
	if p, err := exec.LookPath("pebble"); err == nil {
		return p
	}
	t.Skip("KONTRA_TEST_PEBBLE is not set and pebble is not on PATH (go install github.com/letsencrypt/pebble/v2/cmd/pebble@v2.10.1)")
	return ""
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// serveDNS answers every A question with 127.0.0.1 and every other question with no record, so the CA
// validates a test domain against this host. DNS over TCP (RFC 1035 §4.2.2), which is what Pebble's
// -dnsserver resolver speaks.
func serveDNS(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	answer := func(msg []byte) ([]byte, error) {
		var p dnsmessage.Parser
		h, err := p.Start(msg)
		if err != nil {
			return nil, err
		}
		q, err := p.Question()
		if err != nil {
			return nil, err
		}
		b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: h.ID, Response: true, Authoritative: true, RecursionDesired: h.RecursionDesired})
		_ = b.StartQuestions()
		_ = b.Question(q)
		_ = b.StartAnswers()
		if q.Type == dnsmessage.TypeA {
			_ = b.AResource(dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, TTL: 1}, dnsmessage.AResource{A: [4]byte{127, 0, 0, 1}})
		}
		return b.Finish()
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(10 * time.Second))
				for {
					var n [2]byte
					if _, err := io.ReadFull(c, n[:]); err != nil {
						return
					}
					msg := make([]byte, int(n[0])<<8|int(n[1]))
					if _, err := io.ReadFull(c, msg); err != nil {
						return
					}
					out, err := answer(msg)
					if err != nil {
						return
					}
					if _, err := c.Write(append([]byte{byte(len(out) >> 8), byte(len(out))}, out...)); err != nil {
						return
					}
				}
			}()
		}
	}()
	return ln.Addr().String()
}

// NewACME obtains a certificate from a local ACME server (Pebble) over TLS-ALPN-01 on the gate's own TLS
// listener, stores it in the directory cache, and serves it; a second manager over the same directory
// serves it with the CA gone.
func TestACMEIssuesIntoDirectoryCache(t *testing.T) {
	bin := pebbleBinary(t)
	dir := t.TempDir()
	const domain = "gate.kontra.test"

	// Pebble's own HTTPS (directory and management API) under a throwaway CA.
	ca := newTestCA(t)
	pebbleDir := filepath.Join(dir, "pebble")
	if err := os.MkdirAll(pebbleDir, 0o700); err != nil {
		t.Fatal(err)
	}
	wfeCert, wfeKey := ca.leaf(t, pebbleDir, 2, "localhost", "127.0.0.1")

	// The gate's TLS listener is the port Pebble validates TLS-ALPN-01 on.
	cacheDir := filepath.Join(dir, "acme-cache")
	dirPort, mgmtPort := freePort(t), freePort(t)
	directory := fmt.Sprintf("https://127.0.0.1:%d/dir", dirPort)
	acmeCfg := ACMEConfig{
		Email:        "ops@kontra.test",
		Domains:      []string{domain},
		Cache:        DirCache(cacheDir),
		DirectoryURL: directory,
		HTTPClient:   &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: ca.pool}}},
	}
	a, err := NewACME(acmeCfg)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{Listen: "127.0.0.1:0", PublicURL: "https://" + domain, TLS: a.TLSConfig(), ACME: a}, newTestRoutes().router)
	if err != nil {
		t.Fatal(err)
	}
	addr, err := s.Listen()
	if err != nil {
		t.Fatal(err)
	}
	tlsPort := addr.(*net.TCPAddr).Port

	cfg := map[string]any{"pebble": map[string]any{
		"listenAddress":                  fmt.Sprintf("127.0.0.1:%d", dirPort),
		"managementListenAddress":        fmt.Sprintf("127.0.0.1:%d", mgmtPort),
		"certificate":                    wfeCert,
		"privateKey":                     wfeKey,
		"httpPort":                       freePort(t),
		"tlsPort":                        tlsPort,
		"ocspResponderURL":               "",
		"externalAccountBindingRequired": false,
	}}
	cfgFile := filepath.Join(pebbleDir, "pebble.json")
	b, _ := json.Marshal(cfg)
	if err := os.WriteFile(cfgFile, b, 0o600); err != nil {
		t.Fatal(err)
	}
	var pebbleLog bytes.Buffer
	cmd := exec.Command(bin, "-config", cfgFile, "-dnsserver", serveDNS(t))
	cmd.Env = append(os.Environ(), "PEBBLE_VA_NOSLEEP=1", "PEBBLE_WFE_NONCEREJECT=0", "PEBBLE_AUTHZREUSE=0")
	cmd.Stdout, cmd.Stderr = &pebbleLog, &pebbleLog
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	stopped := false
	stop := func() {
		if !stopped {
			stopped = true
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}
	t.Cleanup(func() {
		stop()
		if t.Failed() {
			t.Logf("pebble:\n%s", pebbleLog.String())
		}
	})
	waitFor(t, 20*time.Second, func() bool {
		resp, err := acmeCfg.HTTPClient.Get(directory)
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == 200
	})

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- s.Serve(ctx) }()
	defer func() {
		cancel()
		<-served
	}()

	// Pebble's root, from its management API, verifies what the gate serves.
	resp, err := acmeCfg.HTTPClient.Get(fmt.Sprintf("https://127.0.0.1:%d/roots/0", mgmtPort))
	if err != nil {
		t.Fatal(err)
	}
	rootPEM, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(rootPEM) {
		t.Fatalf("pebble root: %q", rootPEM)
	}
	client := tlsClient(roots, domain)
	var hresp *http.Response
	waitFor(t, 60*time.Second, func() bool {
		r, err := client.Get("https://" + addr.String() + "/healthz")
		if err != nil {
			return false
		}
		r.Body.Close()
		hresp = r
		return r.StatusCode == 200
	})
	if hresp.Header.Get("Strict-Transport-Security") != HSTS {
		t.Errorf("HSTS %q", hresp.Header.Get("Strict-Transport-Security"))
	}
	leaf := hresp.TLS.PeerCertificates[0]
	if len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != domain || !strings.Contains(leaf.Issuer.CommonName, "Pebble") {
		t.Errorf("served certificate: names %v issuer %q", leaf.DNSNames, leaf.Issuer.CommonName)
	}
	for _, f := range []string{domain, "acme_account+key"} {
		if fi, err := os.Stat(filepath.Join(cacheDir, f)); err != nil || fi.Size() == 0 {
			t.Errorf("directory cache: %s: %v", f, err)
		}
	}
	if fi, err := os.Stat(cacheDir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("cache directory mode: %v %v", fi.Mode(), err)
	}
	// A name the gate does not serve gets no certificate.
	if _, err := tls.Dial("tcp", addr.String(), &tls.Config{RootCAs: roots, ServerName: "other.kontra.test"}); err == nil {
		t.Error("a certificate was issued for a name outside tls.acme.domains")
	}

	// With the CA gone, a new manager over the same directory serves the cached certificate.
	stop()
	cancel()
	<-served
	served = make(chan error, 1)
	a2, err := NewACME(acmeCfg)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := New(Config{Listen: "127.0.0.1:0", PublicURL: "https://" + domain, TLS: a2.TLSConfig()}, newTestRoutes().router)
	if err != nil {
		t.Fatal(err)
	}
	addr2, _ := s2.Listen()
	ctx2, cancel2 := context.WithCancel(context.Background())
	go func() { served <- s2.Serve(ctx2) }()
	defer cancel2()
	r, err := tlsClient(roots, domain).Get("https://" + addr2.String() + "/healthz")
	if err != nil {
		t.Fatalf("from the cache: %v", err)
	}
	r.Body.Close()
	if got := r.TLS.PeerCertificates[0].SerialNumber; got.Cmp(leaf.SerialNumber) != 0 {
		t.Errorf("from the cache: serial %v, want %v", got, leaf.SerialNumber)
	}
}

func waitFor(t *testing.T, d time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("condition not met within %v", d)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
