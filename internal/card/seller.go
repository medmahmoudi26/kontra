package card

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"time"

	"kontra.sh/kontra/internal/jws"
	"kontra.sh/kontra/internal/strictjson"
)

// SellerKeysMax bounds a fetched seller JWKS, as an upstream card is bounded (spec §6.6).
const SellerKeysMax = 256 << 10

// SellerKeys verifies an upstream card's signatures against the keys the operator configured for its service
// (services.<name>.seller_keys: an inline JWKS, or {"url": "https://…"} fetched with the gate's SSRF-safe dialer,
// never a jku taken from a card; spec/relay.md §3). With keys configured, a card none of whose signatures
// verifies makes the service unavailable: its card routes answer 503 and a new task fails service_unavailable
// at its first attempt; a failed fetch keeps the last verified card for 30 minutes (D89). Building it lifts
// D90's refusal and the seller_keys half of D144.
//
// A {url} set is fetched again with the card, every Refresh (10 minutes): Keeper.Refresh fetches it alongside the
// card, each within its own FetchTimeout, and a fetch that fails is a failed refresh of the card.
//
// Work package: M3 WP-02.
type SellerKeys struct {
	raw    json.RawMessage
	client *http.Client
	url    string // the {url} form's URL; empty for an inline set

	mu        sync.Mutex
	keys      *jws.KeySetPublic // the inline set, or the last {url} set fetched
	fetchedAt time.Time
}

// NewSellerKeys parses seller_keys (validated by internal/config). client fetches a {"url"} set; it must be the
// SSRF-safe one (spec/relay.md §10.3).
//
// The value is read with exact keys (internal/strictjson): an inline JWKS {"keys": [...]} whose EC P-256 keys
// are kept (jws.ParseJWKS), or exactly {"url": "https://…"}. A row an operator writes (spec §13.5) bypasses the
// configuration's checks, so they are made again here; a value that is neither is an error, and the service is
// left out of the registry rather than served unverified.
func NewSellerKeys(raw json.RawMessage, client *http.Client) (*SellerKeys, error) {
	m, err := strictjson.Members(raw)
	if err != nil {
		return nil, fmt.Errorf("card: seller_keys: %w", err)
	}
	s := &SellerKeys{raw: slices.Clone(raw), client: client}
	_, hasKeys := m["keys"]
	rawURL, hasURL := m["url"]
	switch {
	case hasKeys && hasURL:
		return nil, errors.New("card: seller_keys: keys and url are exclusive")
	case hasKeys:
		if s.keys, err = jws.ParseJWKS(raw); err != nil {
			return nil, fmt.Errorf("card: seller_keys: %w", err)
		}
	case hasURL:
		if len(m) != 1 {
			return nil, errors.New("card: seller_keys: {url} takes no other key")
		}
		var u string
		if err := json.Unmarshal(rawURL, &u); err != nil {
			return nil, errors.New("card: seller_keys: url is not a string")
		}
		p, err := url.Parse(u)
		if err != nil || p.Scheme != "https" || p.Host == "" || p.User != nil || p.Fragment != "" {
			return nil, errors.New("card: seller_keys: url must be an https:// URL without credentials (spec §3, §10.3)")
		}
		if client == nil {
			return nil, errors.New("card: seller_keys: a {url} set needs the SSRF-safe client")
		}
		s.url = u
	default:
		return nil, errors.New("card: seller_keys: want an inline JWKS {keys: [...]} or {url: https://...}")
	}
	return s, nil
}

// Raw is seller_keys as configured.
func (s *SellerKeys) Raw() json.RawMessage { return s.raw }

// Refresh fetches a {url} set (an inline set has nothing to fetch): one GET through the SSRF-safe client, a 200
// with at most SellerKeysMax bytes of a JWK Set holding at least one EC P-256 key, within FetchTimeout at most
// (the SSRF-safe client has no overall timeout, and a key host that accepts the request and never answers, or
// trickles its body, would otherwise hold the caller). A failure keeps the set last fetched and is returned.
func (s *SellerKeys) Refresh(ctx context.Context) error {
	if s == nil || s.url == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, FetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
	if err != nil {
		return fmt.Errorf("card: seller_keys: %w", err)
	}
	req.Header.Set("Accept", "application/jwk-set+json, application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("card: seller_keys: fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("card: seller_keys: fetch: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, SellerKeysMax+1))
	if err != nil {
		return fmt.Errorf("card: seller_keys: fetch: %w", err)
	}
	if len(body) > SellerKeysMax {
		return fmt.Errorf("card: seller_keys: the key set is over %d bytes", SellerKeysMax)
	}
	keys, err := jws.ParseJWKS(bytes.TrimSpace(body))
	if err != nil {
		return fmt.Errorf("card: seller_keys: %w", err)
	}
	s.mu.Lock()
	s.keys, s.fetchedAt = keys, time.Now()
	s.mu.Unlock()
	return nil
}

// ErrNoSellerKeys is a {url} set that no fetch has produced yet: nothing can be verified.
var ErrNoSellerKeys = errors.New("card: the seller's keys have not been fetched")

// current returns the set to verify with.
func (s *SellerKeys) current() (*jws.KeySetPublic, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.keys == nil {
		return nil, ErrNoSellerKeys
	}
	return s.keys, nil
}

// Verify reports whether any of the upstream card's signatures verifies over the JCS of the card without
// signatures, and the RFC 7638 thumbprints of the keys that verified one (pins record them, ext-sealed §7). A
// card without signatures does not verify.
//
// Signatures are A2A 1.0 AgentCardSignature objects in the card's top-level signatures array; one that is
// malformed, names a key the set does not hold, or does not verify is skipped (any one valid signature is
// enough, as a2a-go's verifier also accepts). An error is a set that cannot be used yet (ErrNoSellerKeys) or a
// card that is not one JSON object without repeated keys: the card is then neither verified nor refused.
func (s *SellerKeys) Verify(ctx context.Context, upstream []byte) (verified bool, thumbprints []string, err error) {
	verified, thumbprints, _, err = s.verify(ctx, upstream)
	return verified, thumbprints, err
}

// verify is Verify, and the key set it verified with (served at kontra.upstream.jwks, D223).
func (s *SellerKeys) verify(_ context.Context, upstream []byte) (verified bool, thumbprints []string, set *jws.KeySetPublic, err error) {
	keys, err := s.current()
	if err != nil {
		return false, nil, nil, err
	}
	verified, thumbprints, err = verifyWith(keys, upstream)
	return verified, thumbprints, keys, err
}

func verifyWith(keys *jws.KeySetPublic, upstream []byte) (verified bool, thumbprints []string, err error) {
	m, err := strictjson.Members(upstream)
	if err != nil {
		return false, nil, fmt.Errorf("%w: %v", ErrBadCard, err)
	}
	rawSigs, ok := m["signatures"]
	if !ok {
		return false, nil, nil
	}
	var sigs []json.RawMessage
	if json.Unmarshal(rawSigs, &sigs) != nil {
		return false, nil, nil
	}
	for _, raw := range sigs {
		sm, err := strictjson.Members(raw)
		if err != nil {
			continue
		}
		var sig jws.Signature
		if json.Unmarshal(sm["protected"], &sig.Protected) != nil || json.Unmarshal(sm["signature"], &sig.Signature) != nil {
			continue
		}
		sig.Header = sm["header"]
		h, err := jws.Verify(upstream, sig, keys)
		if err != nil {
			continue
		}
		verified = true
		if t, ok := keys.Thumbprint(h.Kid); ok && !slices.Contains(thumbprints, t) {
			thumbprints = append(thumbprints, t)
		}
	}
	slices.Sort(thumbprints)
	return verified, thumbprints, nil
}
