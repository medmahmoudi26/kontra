package card

import (
	"context"
	"encoding/json"
	"net/http"

	"kontra.sh/kontra/internal/jws"
	"kontra.sh/kontra/internal/unimpl"
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

// Sign implements Signer: the AgentCardSignature JSON for served (a card without signatures).
func (s *JWSSigner) Sign(ctx context.Context, served []byte, upstreamSHA256 [32]byte, upstreamURL string, verified bool) (json.RawMessage, error) {
	return nil, unimpl.Error("card.JWSSigner.Sign", "M3 WP-02")
}

// UpstreamHash is the SHA-256 over the JCS of the upstream card: kontra.upstream.sha256 and services.card_hash
// (D166 fills the column M1 left null).
func UpstreamHash(upstream []byte) ([32]byte, error) {
	return [32]byte{}, unimpl.Error("card.UpstreamHash", "M3 WP-02")
}

// JWKSHandler serves GET /.well-known/jwks.json: the key set's public JWKS with Cache-Control max-age=300
// (spec/relay.md §2.1, D166). Any other method is the closed router's empty 404.
func JWKSHandler(keys *jws.KeySet) (http.Handler, error) {
	return nil, unimpl.Error("card.JWKSHandler", "M3 WP-02")
}
