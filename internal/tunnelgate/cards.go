package tunnelgate

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"kontra.sh/kontra/internal/tunnel"
	"kontra.sh/kontra/internal/upstream"
)

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

// fetchCard is upstream.FetchCard through this session: an upstream.Tunnel of the service, the transport
// of the dispatch's exchanges, whose every request is a fresh stream (DisableKeepAlives sends Connection:
// close and never reuses one, §8.2), in origin form, with no proxy and no redirect followed. Its open is bound
// to the request: a fetch abandoned at CardWait or the keeper's timeout while yamux's open waits returns, and
// opens no stray stream after it (M2 review, robustness-5).
func (s *Session) fetchCard(ctx context.Context, service string) (*upstream.Fetched, error) {
	tb, err := upstream.NewTunnel(service, tunnel.KindA2A, &sessionOpener{s: s, service: service, kind: tunnel.KindA2A})
	if err != nil {
		return nil, err
	}
	return upstream.FetchCard(ctx, tb)
}

// OnConnect registers fn to run when a session serving an a2a service is added, before it becomes a
// candidate (Add): the card keeper fetches then (relay.md §3). fn runs once per service of the session, all
// of them together, within ctx (CardWait); it should return when ctx ends.
func (r *Registry) OnConnect(fn func(ctx context.Context, tenantID uuid.UUID, service string)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onConnect = append(r.onConnect, fn)
}
