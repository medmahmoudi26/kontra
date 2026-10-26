package edge

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

// Metrics is the separate metrics listener (spec §2.1, default :9090): GET /metrics and nothing else (an
// empty 404 otherwise), never on the public listener. What it serves is the handler the replica passes
// (the work package that adds the first metric adds the registry). It is for the operator's network: no
// TLS, no authentication, plain timeouts (a scrape is short).
type Metrics struct {
	listen string
	srv    *http.Server

	mu      sync.Mutex
	ln      net.Listener
	serving bool
}

// NewMetrics returns the metrics listener for addr serving h at /metrics.
func NewMetrics(addr string, h http.Handler) *Metrics {
	r := NewRouter()
	r.Handle("GET /metrics", h)
	return &Metrics{
		listen: addr,
		srv: &http.Server{
			Handler:           r,
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       30 * time.Second,
			WriteTimeout:      30 * time.Second,
			IdleTimeout:       60 * time.Second,
		},
	}
}

// Listen binds the address (once) and returns it.
func (m *Metrics) Listen() (net.Addr, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ln == nil {
		ln, err := net.Listen("tcp", m.listen)
		if err != nil {
			return nil, fmt.Errorf("edge: metrics listen %s: %w", m.listen, err)
		}
		m.ln = ln
	}
	return m.ln.Addr(), nil
}

// Serve serves until Shutdown or until ctx ends, then returns nil.
func (m *Metrics) Serve(ctx context.Context) error {
	if _, err := m.Listen(); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = m.srv.Close() })
	defer stop()
	m.mu.Lock()
	ln := m.ln
	m.serving = true
	m.mu.Unlock()
	if err := m.srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Shutdown stops the listener, waiting for scrapes in flight until ctx ends.
func (m *Metrics) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	ln, serving := m.ln, m.serving
	m.mu.Unlock()
	if ln != nil && !serving {
		_ = ln.Close() // bound but never served: the server does not know it
	}
	return m.srv.Shutdown(ctx)
}
