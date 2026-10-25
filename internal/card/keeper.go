package card

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"kontra.sh/kontra/internal/upstream"
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

// FetchTimeout bounds one fetch (both well-known paths). A seller that accepts the request and never
// answers (a deadlock, a half-dead proxy) fails that fetch, which Run retries on its backoff schedule;
// without it, Run would wait on the one fetch forever and never refetch, and the service would go
// unavailable 30 minutes later and stay so after the seller recovered.
const FetchTimeout = 30 * time.Second

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
// within one Refresh. The persist hook (WithPersist) is how a replica records the card it serves in
// services (upstream_card, card, card_fetched_at); Seed installs the stored card at start, so a restarted
// replica serves the last good card at once.
//
// The hook is called after a good fetch whenever the card the keeper serves has not been recorded yet: the
// first good fetch after the keeper was built or seeded (the stored served card may be another body: the
// rewrite input changed), a fetch that changed the upstream bytes or the served body, and every good fetch
// after a call that failed, until one succeeds. A fetch of a card already recorded calls nothing.
type Keeper struct {
	backend      upstream.Backend
	in           Input
	now          func() time.Time
	fetch        func(context.Context, upstream.Backend) (*upstream.Fetched, error)
	fetchTimeout time.Duration
	persist      func(ctx context.Context, s *Served, fetchedAt time.Time) error

	mu       sync.RWMutex
	served   *Served
	recorded *Served // the card the persist hook last recorded, nil before its first success
	info     upstream.Info
	lastOK   time.Time
	lastErr  error
}

// KeeperOption configures a Keeper.
type KeeperOption func(*Keeper)

// WithPersist sets the hook that records the served card after a good fetch, while it is unrecorded (see
// Keeper).
func WithPersist(fn func(ctx context.Context, s *Served, fetchedAt time.Time) error) KeeperOption {
	return func(k *Keeper) { k.persist = fn }
}

// WithClock replaces time.Now (tests).
func WithClock(now func() time.Time) KeeperOption { return func(k *Keeper) { k.now = now } }

// WithFetch replaces upstream.FetchCard (tests, and the tunnel backend's fetch in M2).
func WithFetch(fn func(context.Context, upstream.Backend) (*upstream.Fetched, error)) KeeperOption {
	return func(k *Keeper) { k.fetch = fn }
}

// WithFetchTimeout replaces FetchTimeout (tests).
func WithFetchTimeout(d time.Duration) KeeperOption { return func(k *Keeper) { k.fetchTimeout = d } }

// NewKeeper returns a keeper for backend b; in is the rewrite input (its Upstream is ignored).
func NewKeeper(b upstream.Backend, in Input, opts ...KeeperOption) *Keeper {
	k := &Keeper{backend: b, in: in, now: time.Now, fetch: upstream.FetchCard, fetchTimeout: FetchTimeout}
	for _, o := range opts {
		o(k)
	}
	return k
}

// Seed installs a stored upstream card (services.upstream_card) fetched at fetchedAt as the last good one.
func (k *Keeper) Seed(raw []byte, fetchedAt time.Time) error {
	return k.install(raw, fetchedAt)
}

// Refresh fetches the card once, within FetchTimeout. On success it replaces the last good card and calls
// the persist hook while that card is unrecorded (a failed call is returned, and made again at the next
// good fetch); on failure, a fetch that ran out of time included, the last good card stays and the error
// is returned.
func (k *Keeper) Refresh(ctx context.Context) error {
	fctx, cancel := context.WithTimeout(ctx, k.fetchTimeout)
	f, err := k.fetch(fctx, k.backend) // the body is read whole before fetch returns
	cancel()
	if err != nil && ctx.Err() == nil && errors.Is(fctx.Err(), context.DeadlineExceeded) {
		err = fmt.Errorf("card: the fetch did not finish within %v: %w", k.fetchTimeout, err)
	}
	if err == nil {
		if err = k.install(f.Raw, k.now()); err == nil {
			return k.record(ctx)
		}
	}
	k.mu.Lock()
	k.lastErr = err
	k.mu.Unlock()
	return err
}

// record calls the persist hook when the current card is not the one it last recorded.
func (k *Keeper) record(ctx context.Context) error {
	if k.persist == nil {
		return nil
	}
	k.mu.RLock()
	s, done := k.served, sameCard(k.served, k.recorded)
	k.mu.RUnlock()
	if done {
		return nil
	}
	if err := k.persist(ctx, s, s.LastModified); err != nil {
		return fmt.Errorf("card: persist: %w", err)
	}
	k.mu.Lock()
	k.recorded = s
	k.mu.Unlock()
	return nil
}

// sameCard reports whether a and b are the same upstream bytes served as the same body.
func sameCard(a, b *Served) bool {
	return a != nil && b != nil && bytes.Equal(a.Upstream, b.Upstream) && bytes.Equal(a.Body, b.Body)
}

// install rewrites raw and makes it the current card.
func (k *Keeper) install(raw []byte, at time.Time) error {
	info, err := upstream.ReadInfo(raw)
	if err != nil {
		return err
	}
	in := k.in
	in.Upstream = raw
	body, err := Rewrite(in)
	if err != nil {
		return err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
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
	return nil
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
