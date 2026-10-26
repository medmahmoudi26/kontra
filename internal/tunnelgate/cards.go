package tunnelgate

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"

	"github.com/google/uuid"

	"kontra.sh/kontra/internal/tunnel"
	"kontra.sh/kontra/internal/upstream"
)

// exchangeOrigin is the origin of the requests the gate writes on a tunnel stream: a placeholder, since
// the node sends each request to its --serve target and sets Host itself, whatever the request says
// (§8.2). .invalid never resolves (RFC 6761), so it can never be dialed by mistake.
const exchangeOrigin = "http://kontra-node.invalid"

// maxCardHeaderBytes bounds the response header of a card fetch through the tunnel.
const maxCardHeaderBytes = 64 << 10

// FetchCard fetches a tunnel service's upstream card through one of this replica's sessions serving it
// (GET /.well-known/agent-card.json, then /.well-known/agent.json, at most 256 KiB, relay.md §3, §6.6), the
// card.Keeper fetch of a tunnel service. Each GET is one exchange on a fresh stream whose preamble names the
// service and its kind, without a task (§8.1); the stream checks of Allows apply (relay.md §6.1: a card fetch
// is a stream like any other). The newest session is tried first, so a session that has just connected is
// the one whose card is read (Add), then the others. ErrSessionGone when this replica holds none: the keeper
// then serves the card another replica stored.
func (r *Registry) FetchCard(ctx context.Context, tenantID uuid.UUID, service string) (*upstream.Fetched, error) {
	var errs []error
	for _, s := range r.ForService(tenantID, service) {
		f, err := s.fetchCard(ctx, service)
		if err == nil {
			return f, nil
		}
		errs = append(errs, fmt.Errorf("session %s: %w", s.ID, err))
		if ctx.Err() != nil {
			break
		}
	}
	if len(errs) == 0 {
		return nil, fmt.Errorf("%w: this replica holds no session serving %s", ErrSessionGone, service)
	}
	return nil, errors.Join(errs...)
}

// fetchCard is upstream.FetchCard through this session.
func (s *Session) fetchCard(ctx context.Context, service string) (*upstream.Fetched, error) {
	return upstream.FetchCard(ctx, &cardBackend{s: s, service: service})
}

// cardBackend is the upstream.Backend of one card fetch over one session: every request is a fresh stream
// (DisableKeepAlives sends Connection: close and never reuses one, §8.2), in origin form, with no proxy and
// no redirect followed. The dispatch's backend over a session is Registry.Backend's upstream.Tunnel.
type cardBackend struct {
	s       *Session
	service string
}

var _ upstream.Backend = (*cardBackend)(nil)

func (b *cardBackend) Kind() upstream.Kind { return upstream.KindTunnel }

func (b *cardBackend) Origin() *url.URL {
	u, _ := url.Parse(exchangeOrigin)
	return u
}

func (b *cardBackend) Base() *url.URL {
	u := b.Origin()
	u.Path = "/"
	return u
}

func (b *cardBackend) HTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy: nil,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return b.s.OpenExchange(ctx, tunnel.Preamble{Op: tunnel.OpHTTP, Service: b.service, Kind: tunnel.KindA2A})
			},
			DisableKeepAlives:      true,
			MaxResponseHeaderBytes: maxCardHeaderBytes,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// OnConnect registers fn to run when a session serving an a2a service is added, before it becomes a
// candidate (Add): the card keeper fetches then (relay.md §3). fn runs once per service of the session, all
// of them together, within ctx (CardWait); it should return when ctx ends.
func (r *Registry) OnConnect(fn func(ctx context.Context, tenantID uuid.UUID, service string)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onConnect = append(r.onConnect, fn)
}
