package gate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"

	"kontra.sh/kontra/internal/config"
	"kontra.sh/kontra/internal/edge"
	"kontra.sh/kontra/internal/lifecycle"
	"kontra.sh/kontra/internal/tenant"
	"kontra.sh/kontra/internal/tunnelgate"
)

// Spike (http-tunnel): a service of kind http is a raw HTTP service a node serves. The gate serves it at its own host,
// <name>.<public_url host>, and passes every request through unchanged, WebSocket upgrades included: the caller's
// path, query, Host and body reach the service as sent, with X-Forwarded-For (the client address the edge resolved),
// X-Forwarded-Proto (public_url's scheme) and X-Forwarded-Host set by the gate; a caller's own are dropped. The
// edge's limits (rate per address, body cap) apply as on every route; the gate asks for no credential: the service
// does its own authentication.

// httpService is the route of one http service's host.
type httpService struct {
	name     string
	host     string // <name>.<public_url host>, without a port
	proto    string // public_url's scheme
	replica  string
	services *Services
	picker   *tunnelgate.Picker
	tunnels  *tunnelgate.Registry
	log      *slog.Logger
}

// mountHTTPServices registers the host route of each http service in the configuration (a team gate's).
func (r *Replica) mountHTTPServices(routes *edge.Router) error {
	var names []string
	for name, s := range r.cfg.Services {
		if s.Kind == config.KindHTTP {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil
	}
	if r.tunnels == nil {
		return errors.New("gate: an http service needs the tunnel registry")
	}
	pub, err := url.Parse(r.cfg.PublicURL)
	if err != nil || pub.Hostname() == "" {
		return fmt.Errorf("gate: public_url %q has no host for http services", r.cfg.PublicURL)
	}
	sort.Strings(names)
	picker := tunnelgate.NewPicker(r.pool)
	for _, name := range names {
		h := &httpService{name: name, host: name + "." + pub.Hostname(), proto: pub.Scheme, replica: r.id,
			services: r.services, picker: picker, tunnels: r.tunnels, log: r.log.With("component", "http-service", "service", name)}
		routes.Handle(h.host+"/", h)
		r.log.Info("http service routed", "service", name, "host", h.host)
	}
	return nil
}

func (h *httpService) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	svc, _ := h.services.Lookup(ctx, tenant.TeamSlug, h.name)
	if svc == nil || svc.Disabled {
		http.Error(w, "kontra: no such service", http.StatusNotFound)
		return
	}
	tenantID := svc.Ref.TenantID
	picked, err := h.picker.PickSession(ctx, lifecycle.SessionRequest{TenantID: tenantID, Service: h.name})
	switch {
	case errors.Is(err, lifecycle.ErrNoSession):
		http.Error(w, "kontra: no node serves "+h.name+" now", http.StatusBadGateway)
		return
	case err != nil:
		h.log.Warn("http service: no session picked", "error", err)
		http.Error(w, "kontra: the gate could not pick a node", http.StatusServiceUnavailable)
		return
	case picked.ReplicaID != h.replica:
		// Spike: one replica. A gate of several would hop to the replica holding the session (relay.md §11.2).
		http.Error(w, "kontra: the node is on another replica", http.StatusBadGateway)
		return
	}
	session := picked.SessionID
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			// Rewrite mode has removed Forwarded, X-Forwarded-* and the hop-by-hop headers (an Upgrade is put back).
			pr.Out.URL.Scheme, pr.Out.URL.Host = "http", h.host
			pr.Out.Host = pr.In.Host
			if a, ok := edge.ClientAddr(pr.In.Context()); ok && a.IsValid() {
				pr.Out.Header.Set("X-Forwarded-For", a.Unmap().String())
			}
			pr.Out.Header.Set("X-Forwarded-Proto", h.proto)
			pr.Out.Header.Set("X-Forwarded-Host", pr.In.Host)
		},
		Transport: &http.Transport{
			Proxy: nil,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return h.tunnels.HTTPExchange(ctx, session, tenantID, h.name)
			},
			DisableKeepAlives:  true, // one exchange per stream (tunnel.md §8.2)
			DisableCompression: true,
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if r.Context().Err() == nil {
				h.log.Warn("http service: the exchange failed", "error", err)
			}
			http.Error(w, "kontra: the node did not answer", http.StatusBadGateway)
		},
		ErrorLog: slog.NewLogLogger(h.log.Handler(), slog.LevelDebug),
	}
	rp.ServeHTTP(w, req)
}
