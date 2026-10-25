package card

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/medmahmoudi26/kontra/internal/upstream"
)

// UnavailableAfter is how long a service keeps serving its last good card while every fetch fails: past
// it the service is unavailable (spec §3), its card routes answer 503 and a new task fails
// service_unavailable at its first attempt (§6.4).
const UnavailableAfter = 30 * time.Minute

// Backoff bounds of the first fetch, and of the retries after a failed refresh.
const (
	retryMin = time.Second
	retryMax = time.Minute
)

// ErrUnavailable is a service with no usable card: none fetched yet, or none fetched for UnavailableAfter.
var ErrUnavailable = errors.New("card: the service's card is unavailable")

// Keeper keeps one direct service's card (spec §3; the card lifecycle of the M1 audit):
//
//   - it fetches the card when the service is configured, retrying the first fetch with backoff (1 s
//     doubling to 1 min), so a gate started before its seller gets the card as soon as the seller is up;
//   - it fetches again every Refresh (10 min); a failed fetch keeps the last good card and is retried on
//     the backoff schedule;
//   - after UnavailableAfter without a successful fetch, or before the first one, the service is
//     unavailable: Current marks the card Unavailable and Dispatch answers ErrUnavailable.
//
// Every replica runs its own keeper per service; there is no leader. Replicas that fetched the same
// upstream bytes produce the same served bytes and ETag (the rewrite is deterministic), so they converge
// within one Refresh. The Persist hook is how the replica that fetched a changed card records it in
// services (upstream_card, card, card_fetched_at); Seed installs the stored card at start, so a restarted
// replica serves the last good card at once.
type Keeper struct {
	backend upstream.Backend
	in      Input
	now     func() time.Time
	fetch   func(context.Context, upstream.Backend) (*upstream.Fetched, error)
	persist func(ctx context.Context, s *Served, fetchedAt time.Time) error

	mu      sync.RWMutex
	served  *Served
	info    upstream.Info
	lastOK  time.Time
	lastErr error
}

// KeeperOption configures a Keeper.
type KeeperOption func(*Keeper)

// WithPersist sets the hook called after a fetch that changed the upstream card.
func WithPersist(fn func(ctx context.Context, s *Served, fetchedAt time.Time) error) KeeperOption {
	return func(k *Keeper) { k.persist = fn }
}

// WithClock replaces time.Now (tests).
func WithClock(now func() time.Time) KeeperOption { return func(k *Keeper) { k.now = now } }

// WithFetch replaces upstream.FetchCard (tests, and the tunnel backend's fetch in M2).
func WithFetch(fn func(context.Context, upstream.Backend) (*upstream.Fetched, error)) KeeperOption {
	return func(k *Keeper) { k.fetch = fn }
}

// NewKeeper returns a keeper for backend b; in is the rewrite input (its Upstream is ignored).
func NewKeeper(b upstream.Backend, in Input, opts ...KeeperOption) *Keeper {
	k := &Keeper{backend: b, in: in, now: time.Now, fetch: upstream.FetchCard}
	for _, o := range opts {
		o(k)
	}
	return k
}

// Seed installs a stored upstream card (services.upstream_card) fetched at fetchedAt as the last good one.
func (k *Keeper) Seed(raw []byte, fetchedAt time.Time) error {
	_, err := k.install(raw, fetchedAt)
	return err
}

// Refresh fetches the card once. On success it replaces the last good card (and calls the persist hook
// when the upstream bytes changed); on failure the last good card stays and the error is returned.
func (k *Keeper) Refresh(ctx context.Context) error {
	f, err := k.fetch(ctx, k.backend)
	if err == nil {
		var changed bool
		if changed, err = k.install(f.Raw, k.now()); err == nil {
			if changed && k.persist != nil {
				k.mu.RLock()
				s := k.served
				k.mu.RUnlock()
				if perr := k.persist(ctx, s, s.LastModified); perr != nil {
					return fmt.Errorf("card: persist: %w", perr)
				}
			}
			return nil
		}
	}
	k.mu.Lock()
	k.lastErr = err
	k.mu.Unlock()
	return err
}

// install rewrites raw and makes it the current card. changed reports new upstream bytes.
func (k *Keeper) install(raw []byte, at time.Time) (bool, error) {
	info, err := upstream.ReadInfo(raw)
	if err != nil {
		return false, err
	}
	in := k.in
	in.Upstream = raw
	body, err := Rewrite(in)
	if err != nil {
		return false, err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	changed := k.served == nil || !bytes.Equal(k.served.Upstream, raw)
	lastMod := at
	if k.served != nil && bytes.Equal(k.served.Body, body) {
		lastMod = k.served.LastModified // the same body keeps its Last-Modified, so caches stay valid
	}
	k.served = &Served{Body: body, Upstream: raw, ETag: ETag(body), LastModified: lastMod}
	k.info = info
	if at.After(k.lastOK) {
		k.lastOK = at
	}
	k.lastErr = nil
	return changed, nil
}

// Run keeps the card until ctx ends: the first fetch and every failed one are retried with backoff, a
// good card is refreshed every Refresh.
func (k *Keeper) Run(ctx context.Context) {
	wait := time.Duration(0)
	backoff := retryMin
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if err := k.Refresh(ctx); err != nil {
			wait = backoff
			backoff = min(backoff*2, retryMax)
			continue
		}
		wait, backoff = Refresh, retryMin
	}
}

// available reports whether the last good card is still servable at now.
func (k *Keeper) available(now time.Time) bool {
	return k.served != nil && now.Sub(k.lastOK) < UnavailableAfter
}

// Current returns the service's cards. With no card yet it returns ErrUnavailable; past UnavailableAfter
// without a good fetch it returns the last card marked Unavailable, and ErrUnavailable.
func (k *Keeper) Current() (*Served, error) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	if k.served == nil {
		return nil, k.unavailable()
	}
	if !k.available(k.now()) {
		s := *k.served
		s.Unavailable = true
		return &s, k.unavailable()
	}
	return k.served, nil
}

// Dispatch returns what a dispatch needs: the backend and the Info read from the last good card, or
// ErrUnavailable (the task fails service_unavailable, spec §6.4).
func (k *Keeper) Dispatch() (upstream.Backend, upstream.Info, error) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	if !k.available(k.now()) {
		return nil, upstream.Info{}, k.unavailable()
	}
	return k.backend, k.info, nil
}

func (k *Keeper) unavailable() error {
	if k.lastErr != nil {
		return fmt.Errorf("%w: %w", ErrUnavailable, k.lastErr)
	}
	return ErrUnavailable
}
