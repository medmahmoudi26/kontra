package card

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
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
// after a call that failed, until one succeeds. A good fetch of a card already recorded calls the touch hook
// instead (WithTouch: D89's card_fetched_at).
//
// Signing (spec §3, D13, D166): with a Signer (WithSigner) the served card is the rewrite with exactly one
// signature, the gate's, whose protected header binds the upstream card's JCS hash, the URL it is served at and
// whether a seller signature verified. Served.Hash is that hash (services.card_hash).
//
// Seller keys (spec §3; WithSellerKeys): every refresh fetches a {url} set again with the card, and verifies the
// fetched card. A card none of whose signatures verifies is installed unavailable (its card routes answer 503,
// Dispatch fails service_unavailable) and is neither persisted nor touched; a refresh whose card or key fetch
// fails keeps the last verified card until UnavailableAfter past the last good refresh (D89). A stored card
// installed by Seed is verified at once against an inline set; against a {url} set not yet fetched it waits,
// unavailable, for the first refresh that fetches the keys (Run's first refresh is immediate).
type Keeper struct {
	backend      upstream.Backend
	in           Input
	now          func() time.Time
	fetch        func(context.Context, upstream.Backend) (*upstream.Fetched, error)
	fetchTimeout time.Duration
	persist      func(ctx context.Context, s *Served, fetchedAt time.Time) error
	touch        func(ctx context.Context, s *Served)
	signer       Signer
	upstreamURL  string // {public_url}{svc}/.well-known/agent-card.upstream.json
	seller       *SellerKeys

	mu       sync.RWMutex
	served   *Served
	servedAt time.Time // when the served card was fetched (or the stored one was), for a verdict given later
	recorded *Served   // the card the persist hook last recorded, nil before its first success
	info     upstream.Info
	lastOK   time.Time
	lastErr  error
	// unverified is a served card whose signatures no configured seller key verifies; verifyPending a seeded
	// card that waits for a {url} key set's first fetch. Either makes the service unavailable.
	unverified    bool
	verifyPending bool
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

// WithTouch sets the hook called after a good fetch of a card already recorded (D89: every good fetch moves
// card_fetched_at). It is never called for a card that does not verify against configured seller keys.
func WithTouch(fn func(ctx context.Context, s *Served)) KeeperOption {
	return func(k *Keeper) { k.touch = fn }
}

// WithSigner signs every served card with s (spec §3, D166). Without it (tests of the rewrite) the served card
// carries no signatures member.
func WithSigner(s Signer) KeeperOption { return func(k *Keeper) { k.signer = s } }

// WithSellerKeys verifies every upstream card against keys (spec §3); nil configures none (verified: false).
func WithSellerKeys(keys *SellerKeys) KeeperOption { return func(k *Keeper) { k.seller = keys } }

// UpstreamPath is where the upstream card is served under {svc} (spec §2.1).
const UpstreamPath = "/.well-known/agent-card.upstream.json"

// NewKeeper returns a keeper for backend b; in is the rewrite input (its Upstream is ignored).
func NewKeeper(b upstream.Backend, in Input, opts ...KeeperOption) *Keeper {
	k := &Keeper{backend: b, in: in, now: time.Now, fetch: upstream.FetchCard, fetchTimeout: FetchTimeout}
	for _, o := range opts {
		o(k)
	}
	k.upstreamURL = strings.TrimRight(in.PublicURL, "/") + in.ServicePath + UpstreamPath
	return k
}

// Seed installs a stored upstream card (services.upstream_card) fetched at fetchedAt as the last good one. With
// seller keys it is verified now when the keys are at hand, and otherwise waits, unavailable, for the first
// refresh that fetches them.
func (k *Keeper) Seed(raw []byte, fetchedAt time.Time) error {
	v, err := k.verify(context.Background(), raw)
	if errors.Is(err, ErrNoSellerKeys) {
		return k.install(context.Background(), raw, fetchedAt, verdict{pending: true})
	}
	if err != nil {
		return err
	}
	return k.install(context.Background(), raw, fetchedAt, v)
}

// verdict is what seller keys made of an upstream card.
type verdict struct {
	verified bool // a configured key verified one of its signatures
	refused  bool // keys are configured and none verified any
	pending  bool // keys are configured and not fetched yet
}

// verify checks raw against the seller keys; without keys the card is neither verified nor refused.
func (k *Keeper) verify(ctx context.Context, raw []byte) (verdict, error) {
	if k.seller == nil {
		return verdict{}, nil
	}
	ok, _, err := k.seller.Verify(ctx, raw)
	if err != nil {
		return verdict{}, err
	}
	return verdict{verified: ok, refused: !ok}, nil
}

// Refresh fetches the card once, within FetchTimeout, and a {url} seller key set with it, at the same time and
// within a FetchTimeout of its own (a slow key host does not eat into the card's budget). On success it
// replaces the last good card and calls the persist hook while that card is unrecorded (a failed call is
// returned, and made again at the next good fetch), or the touch hook when it is recorded; on failure, a fetch
// that ran out of time included, the last good card stays and the error is returned. A fetched card that no
// configured seller key verifies is installed unavailable and returned as an error. When the keys were fetched
// and the card was not, the current card is verified again against them (reverify).
func (k *Keeper) Refresh(ctx context.Context) error {
	keysDone := make(chan error, 1)
	if k.seller == nil {
		keysDone <- nil
	} else {
		go func() { keysDone <- k.refreshKeys(ctx) }()
	}
	fctx, cancel := context.WithTimeout(ctx, k.fetchTimeout)
	f, err := k.fetch(fctx, k.backend) // the body is read whole before fetch returns
	cancel()
	keysErr := <-keysDone
	if err != nil && ctx.Err() == nil && errors.Is(fctx.Err(), context.DeadlineExceeded) {
		err = fmt.Errorf("card: the fetch did not finish within %v: %w", k.fetchTimeout, err)
	}
	if err != nil && keysErr == nil {
		k.reverify(ctx)
	}
	if err == nil && keysErr != nil {
		err = keysErr
	}
	if err != nil {
		k.mu.Lock()
		k.lastErr = err
		k.mu.Unlock()
		return err
	}
	v, err := k.verify(ctx, f.Raw)
	if err == nil {
		err = k.install(ctx, f.Raw, k.now(), v)
	}
	if err == nil && v.refused {
		err = ErrNotVerified
	}
	if err != nil {
		k.mu.Lock()
		k.lastErr = err
		k.mu.Unlock()
		return err
	}
	return k.record(ctx)
}

// RefreshSellerKeys fetches a {url} seller key set (an inline set, or none, does nothing), within FetchTimeout,
// and verifies the current card against it again when it waits for it or was refused (reverify): a replica that
// holds no session of a tunnel service fetches no card of its own, and its seeded card would otherwise wait for
// keys forever, or stay refused against a set from before the seller rotated its key.
func (k *Keeper) RefreshSellerKeys(ctx context.Context) error {
	if k.seller == nil {
		return nil
	}
	if err := k.refreshKeys(ctx); err != nil {
		return err
	}
	k.reverify(ctx)
	return nil
}

// refreshKeys fetches a {url} seller key set within a FetchTimeout of its own: a key host that accepts the request
// and never answers, or trickles its body, fails that fetch instead of holding the caller (the tunnel card loop's
// context never ends).
func (k *Keeper) refreshKeys(ctx context.Context) error {
	kctx, cancel := context.WithTimeout(ctx, k.fetchTimeout)
	defer cancel()
	err := k.seller.Refresh(kctx)
	if err != nil && ctx.Err() == nil && errors.Is(kctx.Err(), context.DeadlineExceeded) {
		err = fmt.Errorf("card: the seller's keys were not fetched within %v: %w", k.fetchTimeout, err)
	}
	return err
}

// ErrNotVerified is a card none of whose signatures the service's configured seller keys verify (spec §3).
var ErrNotVerified = errors.New("card: no signature of the seller's card verifies against the configured seller keys")

// reverify verifies the current card again once a refresh fetched the {url} keys although the card itself was not
// fetched (the seller down while its key host is up, or a replica with no session, which serves the stored card):
// a seeded card that waited for the keys, and a card refused against the set fetched before (the seller rotated
// its key and re-signed its card, which reached this replica before its own key refresh). A card that verifies is
// installed as of when it was fetched, so D89's 30 minutes run from its fetch, not from now.
func (k *Keeper) reverify(ctx context.Context) {
	k.mu.RLock()
	s, at, waiting := k.served, k.servedAt, k.verifyPending || k.unverified
	k.mu.RUnlock()
	if !waiting || s == nil {
		return
	}
	v, err := k.verify(ctx, s.Upstream)
	if err != nil {
		return
	}
	_ = k.installOver(ctx, s.Upstream, at, v, s) // unless a concurrent refresh installed another card meanwhile
}

// record calls the persist hook when the current card is not the one it last recorded, and the touch hook when
// it is.
func (k *Keeper) record(ctx context.Context) error {
	k.mu.RLock()
	s, done := k.served, sameCard(k.served, k.recorded)
	k.mu.RUnlock()
	if done || k.persist == nil {
		if k.touch != nil {
			k.touch(ctx, s)
		}
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

// install rewrites raw, signs it, and makes it the current card, with v's verdict.
func (k *Keeper) install(ctx context.Context, raw []byte, at time.Time, v verdict) error {
	return k.installOver(ctx, raw, at, v, nil)
}

// installOver is install, which changes nothing when prev is not nil and is no longer the current card.
func (k *Keeper) installOver(ctx context.Context, raw []byte, at time.Time, v verdict, prev *Served) error {
	info, err := upstream.ReadInfo(raw)
	if err != nil {
		return err
	}
	if len(k.in.Sealed) > 0 {
		// D165 (ext-sealed.md §1): the nodes seal and open the A2A 1.0 shapes only, so a sealed service whose
		// upstream card offers no 1.0 interface has no card: its card routes answer 503 and its tasks fail
		// service_unavailable.
		if err := upstream.Require10(info); err != nil {
			return err
		}
	}
	// SPEC-CONFLICT (spec §3 with D49): §3 keeps the upstream card verbatim and serves every seller member byte
	// for byte, and signs the served card over its RFC 8785 JCS with the upstream card's JCS hash in the header;
	// JCS is defined for valid Unicode only (I-JSON), so a card that is not valid UTF-8, or carries a lone
	// surrogate, can be neither hashed nor signed, though M1 served such bytes as they were. The closest faithful
	// behaviour: such a card is not installed (ErrBadCard, as for any card the gate cannot serve), so the service
	// has no card until the seller publishes one; it is never served unsigned or altered.
	hash, err := UpstreamHash(raw)
	if err != nil {
		return err
	}
	in := k.in
	in.Upstream = raw
	body, err := Rewrite(in)
	if err != nil {
		return err
	}
	if k.signer != nil {
		sig, err := k.signer.Sign(ctx, body, hash, k.upstreamURL, v.verified)
		if err != nil {
			return err
		}
		if body, err = signed(body, sig); err != nil {
			return err
		}
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if prev != nil && k.served != prev {
		return nil
	}
	lastMod := at
	if k.served != nil && bytes.Equal(k.served.Body, body) {
		lastMod = k.served.LastModified // the same body keeps its Last-Modified, so caches stay valid
	}
	k.served = &Served{Body: body, Upstream: raw, ETag: ETag(body), LastModified: lastMod, Hash: hash, Verified: v.verified}
	k.servedAt = at
	k.info = info
	k.unverified, k.verifyPending = v.refused, v.pending
	if at.After(k.lastOK) && !v.refused {
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

// available reports whether the last good card is still servable at now: one exists, it was fetched within
// UnavailableAfter, and no configured seller key refused it or waits to be fetched for it.
func (k *Keeper) available(now time.Time) bool {
	return k.served != nil && !k.unverified && !k.verifyPending && now.Sub(k.lastOK) < UnavailableAfter
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
	switch {
	case k.unverified:
		return fmt.Errorf("%w: %w", ErrUnavailable, ErrNotVerified)
	case k.verifyPending && k.lastErr == nil:
		return fmt.Errorf("%w: %w", ErrUnavailable, ErrNoSellerKeys)
	case k.lastErr != nil:
		return fmt.Errorf("%w: %w", ErrUnavailable, k.lastErr)
	}
	return ErrUnavailable
}
