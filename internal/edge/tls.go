package edge

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"

	"github.com/medmahmoudi26/kontra/internal/config"
)

// CertCache is the ACME certificate cache, the shape of autocert.Cache: Get returns ErrCacheMiss for a
// key it does not hold. It holds the account key, certificates and the challenge tokens of HTTP-01 and
// TLS-ALPN-01, so replicas that share one cache can answer each other's challenges. M1 ships a directory
// cache (one replica, D70); a shared cache needs a table 0001 does not have (kontra.acme_cache, later).
type CertCache interface {
	Get(ctx context.Context, key string) ([]byte, error)
	Put(ctx context.Context, key string, data []byte) error
	Delete(ctx context.Context, key string) error
}

// ErrCacheMiss is what a CertCache's Get returns for a missing key.
var ErrCacheMiss = autocert.ErrCacheMiss

// DirCache is a CertCache in a directory (created 0700 on first write; files 0600).
func DirCache(dir string) CertCache { return autocert.DirCache(dir) }

var _ autocert.Cache = CertCache(nil)

// LetsEncrypt is the default ACME directory.
const LetsEncrypt = autocert.DefaultACMEDirectory

// ACMEConfig configures built-in certificate management.
type ACMEConfig struct {
	Email   string
	Domains []string // the names certificates are issued for; any other SNI is refused
	Cache   CertCache
	// DirectoryURL is the ACME directory; empty is Let's Encrypt's production directory.
	DirectoryURL string
	// HTTPClient talks to the directory (nil: the default client); tests trust a test CA with it.
	HTTPClient *http.Client
}

// ACME is built-in certificate management (golang.org/x/crypto/acme/autocert): one replica with a
// directory cache (D70). TLS-ALPN-01 is answered on the TLS listener itself; HTTP-01 only when
// HTTPHandler is mounted on a port-80 listener.
//
// SPEC-CONFLICT: spec/relay.md §2.2 and D70 name HTTP-01 and TLS-ALPN-01, but HTTP-01 is validated on
// port 80 and no configuration key (§17) opens a second, plaintext listener there; the gate's one public
// listener is its TLS one. M1 therefore validates with TLS-ALPN-01 only (the CA connects to port 443, so
// listen: must be :443 or forwarded from it), and HTTPHandler is the seam for a port-80 listener once a key
// for it exists.
type ACME struct {
	m       *autocert.Manager
	domains []string
}

// NewACME returns the ACME manager for cfg; TLSConfig is the listener's TLS configuration.
func NewACME(cfg ACMEConfig) (*ACME, error) {
	if len(cfg.Domains) == 0 {
		return nil, errors.New("edge: acme: no domains")
	}
	if cfg.Cache == nil {
		return nil, errors.New("edge: acme: no certificate cache")
	}
	domains := make([]string, len(cfg.Domains))
	for i, d := range cfg.Domains {
		domains[i] = strings.ToLower(strings.TrimSuffix(d, "."))
		if !strings.Contains(domains[i], ".") {
			return nil, fmt.Errorf("edge: acme: %q is not a domain a CA issues for", d)
		}
	}
	m := &autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		Cache:      cfg.Cache,
		HostPolicy: autocert.HostWhitelist(domains...),
		Email:      cfg.Email,
	}
	if cfg.DirectoryURL != "" || cfg.HTTPClient != nil {
		dir := cfg.DirectoryURL
		if dir == "" {
			dir = LetsEncrypt
		}
		m.Client = &acme.Client{DirectoryURL: dir, HTTPClient: cfg.HTTPClient, UserAgent: "kontra-gate"}
	}
	return &ACME{m: m, domains: domains}, nil
}

// TLSConfig is the TLS configuration of the listener: certificates from ACME, TLS 1.2 or later, h2 and
// http/1.1, and the acme-tls/1 protocol of TLS-ALPN-01 challenges.
func (a *ACME) TLSConfig() *tls.Config {
	c := a.m.TLSConfig()
	c.MinVersion = tls.VersionTLS12
	return c
}

// HTTPHandler answers HTTP-01 challenges, for a listener on port 80, and passes everything else to
// fallback (nil: redirect to https).
func (a *ACME) HTTPHandler(fallback http.Handler) http.Handler { return a.m.HTTPHandler(fallback) }

// Warm obtains (or loads from the cache) the certificate of every domain now, so the first caller's
// handshake does not wait for issuance and its 10 s handshake timeout.
func (a *ACME) Warm(ctx context.Context) error {
	var errs []error
	for _, d := range a.domains {
		done := make(chan error, 1)
		go func() {
			_, err := a.m.GetCertificate(&tls.ClientHelloInfo{
				ServerName:   d,
				CipherSuites: []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256},
			})
			done <- err
		}()
		select {
		case err := <-done:
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", d, err))
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return errors.Join(errs...)
}

// NewStatic returns a TLS config over a provided certificate chain and key (PEM). Both files are read at
// once (an unreadable pair is an error here, not at the first handshake) and again when either changes on
// disk, checked at most every staticRecheck, so a renewed certificate is picked up without a restart; a
// pair that fails to load keeps the previous one and is logged.
func NewStatic(certFile, keyFile string) (*tls.Config, error) {
	c, _, err := newStatic(certFile, keyFile, staticRecheck)
	return c, err
}

func newStatic(certFile, keyFile string, recheck time.Duration) (*tls.Config, *staticCert, error) {
	s := &staticCert{certFile: certFile, keyFile: keyFile, recheck: recheck, log: slog.Default()}
	if err := s.load(); err != nil {
		return nil, nil, err
	}
	return &tls.Config{
		MinVersion:     tls.VersionTLS12,
		NextProtos:     []string{"h2", "http/1.1"},
		GetCertificate: s.get,
	}, s, nil
}

const staticRecheck = 30 * time.Second

type staticCert struct {
	certFile, keyFile string
	recheck           time.Duration
	log               *slog.Logger

	mu        sync.Mutex
	cert      *tls.Certificate
	stamp     [2]fileStamp
	lastCheck time.Time
}

type fileStamp struct {
	mod  time.Time
	size int64
}

func stampOf(path string) (fileStamp, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return fileStamp{}, err
	}
	return fileStamp{mod: fi.ModTime(), size: fi.Size()}, nil
}

func (s *staticCert) load() error {
	cs, err := stampOf(s.certFile)
	if err != nil {
		return fmt.Errorf("edge: tls.cert: %w", err)
	}
	ks, err := stampOf(s.keyFile)
	if err != nil {
		return fmt.Errorf("edge: tls.key: %w", err)
	}
	c, err := tls.LoadX509KeyPair(s.certFile, s.keyFile)
	if err != nil {
		return fmt.Errorf("edge: tls.cert/tls.key: %w", err)
	}
	s.mu.Lock()
	s.cert, s.stamp, s.lastCheck = &c, [2]fileStamp{cs, ks}, time.Now()
	s.mu.Unlock()
	return nil
}

func (s *staticCert) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	s.mu.Lock()
	due := time.Since(s.lastCheck) >= s.recheck
	if due {
		s.lastCheck = time.Now()
	}
	stamp, cert := s.stamp, s.cert
	s.mu.Unlock()
	if due {
		cs, cerr := stampOf(s.certFile)
		ks, kerr := stampOf(s.keyFile)
		if cerr == nil && kerr == nil && (cs != stamp[0] || ks != stamp[1]) {
			if err := s.load(); err != nil {
				s.log.Warn("edge: the changed certificate does not load; serving the previous one", "error", err)
			} else {
				s.mu.Lock()
				cert = s.cert
				s.mu.Unlock()
			}
		}
	}
	return cert, nil
}

// TLSFromConfig returns the listener's TLS configuration for tls: a provided certificate, built-in ACME
// with the directory cache (its manager returned too), or nil (plaintext: a localhost public_url, or a
// front proxy, D69).
func TLSFromConfig(t config.TLS) (*tls.Config, *ACME, error) {
	switch {
	case t.ACME != nil:
		// The cache directory holds the account key and private keys: made 0700 now, so an unwritable
		// path fails at start rather than at the first handshake.
		if err := os.MkdirAll(t.ACME.CacheDir, 0o700); err != nil {
			return nil, nil, fmt.Errorf("edge: tls.acme.cache_dir: %w", err)
		}
		a, err := NewACME(ACMEConfig{Email: t.ACME.Email, Domains: t.ACME.Domains, Cache: DirCache(t.ACME.CacheDir)})
		if err != nil {
			return nil, nil, err
		}
		return a.TLSConfig(), a, nil
	case t.Cert != "":
		c, err := NewStatic(t.Cert, t.Key)
		return c, nil, err
	}
	return nil, nil, nil
}
