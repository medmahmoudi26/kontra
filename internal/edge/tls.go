package edge

import (
	"context"
	"crypto/tls"

	"github.com/medmahmoudi26/kontra/internal/unimpl"
)

// CertCache is the ACME certificate cache, the shape of autocert.Cache. It holds certificates and the
// challenge tokens of HTTP-01 and TLS-ALPN-01, so replicas that share one cache can answer each other's
// challenges. M1 ships a directory cache (one replica); a shared cache needs a table 0001 does not have.
type CertCache interface {
	Get(ctx context.Context, key string) ([]byte, error)
	Put(ctx context.Context, key string, data []byte) error
	Delete(ctx context.Context, key string) error
}

// ACMEConfig configures built-in certificate management.
type ACMEConfig struct {
	Email   string
	Domains []string
	Cache   CertCache
}

// NewACME returns a TLS config whose certificates come from ACME (TLS-ALPN-01 on the TLS listener).
func NewACME(cfg ACMEConfig) (*tls.Config, error) {
	return nil, unimpl.Error("edge.NewACME", "WP-03")
}

// NewStatic returns a TLS config over a provided certificate and key.
func NewStatic(certFile, keyFile string) (*tls.Config, error) {
	return nil, unimpl.Error("edge.NewStatic", "WP-03")
}
