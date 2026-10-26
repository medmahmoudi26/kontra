package card

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"kontra.sh/kontra/internal/jcs"
	"kontra.sh/kontra/internal/jws"
)

// JWSSigner is the gate's Signer (spec/relay.md §3, D13, D166): ES256 over the JCS of the served card without
// signatures, with the protected header {alg, kid, typ JOSE, jku: {public_url}/.well-known/jwks.json,
// kontra.upstream: {sha256, url, verified}}. The served card carries exactly this one signature; the seller's
// stay on the upstream card, served verbatim.
//
// Work package: M3 WP-02.
type JWSSigner struct {
	Keys    *jws.KeySet
	JWKSURL string // {public_url}/.well-known/jwks.json
}

var _ Signer = (*JWSSigner)(nil)

// Sign implements Signer: the AgentCardSignature JSON for served (a card without signatures). The signature is
// deterministic (RFC 6979, jws.KeySet.Sign): replicas that rewrote the same upstream card with the same key
// serve the same bytes, and so the same ETag (spec §3's caching).
func (s *JWSSigner) Sign(_ context.Context, served []byte, upstreamSHA256 [32]byte, upstreamURL string, verified bool) (json.RawMessage, error) {
	if s == nil || s.Keys == nil {
		return nil, errors.New("card: no signing key")
	}
	sig, err := s.Keys.Sign(served, s.JWKSURL, &jws.Upstream{
		SHA256: hex.EncodeToString(upstreamSHA256[:]), URL: upstreamURL, Verified: verified,
	})
	if err != nil {
		return nil, fmt.Errorf("card: sign: %w", err)
	}
	b, err := json.Marshal(sig)
	if err != nil {
		return nil, fmt.Errorf("card: sign: %w", err)
	}
	return b, nil
}

// UpstreamHash is the SHA-256 over the JCS of the upstream card: kontra.upstream.sha256 and services.card_hash
// (D166 fills the column M1 left null). The whole card is hashed, its signatures included: they are what a
// verifier checks against the seller's keys once it has the bytes the hash binds.
func UpstreamHash(upstream []byte) ([32]byte, error) {
	canon, err := jcs.Transform(upstream)
	if err != nil {
		return [32]byte{}, fmt.Errorf("%w: %v", ErrBadCard, err)
	}
	return sha256.Sum256(canon), nil
}

// JWKSHandler serves GET /.well-known/jwks.json: the key set's public JWKS with Cache-Control max-age=300
// (spec/relay.md §2.1, D166). Any other method is the closed router's empty 404.
func JWKSHandler(keys *jws.KeySet) (http.Handler, error) {
	body, err := keys.JWKS()
	if err != nil {
		return nil, fmt.Errorf("card: JWKS: %w", err)
	}
	etag := ETag(body)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Content-Length", "0")
			w.WriteHeader(http.StatusNotFound)
			return
		}
		h := w.Header()
		h.Set("Cache-Control", jws.JWKSCacheControl)
		h.Set("ETag", etag)
		if notModified(r, etag, time.Time{}, false) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		h.Set("Content-Type", "application/jwk-set+json")
		h.Set("Content-Length", strconv.Itoa(len(body)))
		h.Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write(body)
		}
	}), nil
}

// signed returns the served card body with its signature: body (a card without signatures, Rewrite's output)
// with a signatures member holding exactly sig, appended last. Every other member keeps its bytes and place.
func signed(body []byte, sig json.RawMessage) ([]byte, error) {
	o, err := parseObject(body)
	if err != nil {
		return nil, err
	}
	sigs := make([]byte, 0, len(sig)+2)
	sigs = append(append(append(sigs, '['), sig...), ']')
	return o.set("signatures", sigs).marshal(), nil
}
