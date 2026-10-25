// Package card produces the served A2A card from the upstream card, as raw JSON (spec/relay.md §3, D11,
// D12, D52).
//
// The upstream card is kept verbatim (services.upstream_card) and served at
// {svc}/.well-known/agent-card.upstream.json. The served card is the upstream card normalized to the 1.0
// shape (0.2.x and 0.3 fields renamed, unknown fields kept) with exactly these fields set by the gate:
// supportedInterfaces, securitySchemes, securityRequirements, capabilities.pushNotifications,
// capabilities.streaming, capabilities.extendedAgentCard, capabilities.extensions (operator entries replace
// same-URI seller entries) and signatures. A card is never round-tripped through a typed struct.
//
// M1 rewrites without signing (PRD §14): the served card carries no signatures field and the seller's
// signatures are not copied (D13). Signer is the M3 seam.
package card

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/medmahmoudi26/kontra/internal/unimpl"
)

// Refresh is how often a serving replica re-fetches a card (spec §3).
const Refresh = 10 * time.Minute

// Input is everything the rewrite needs besides the upstream card.
type Input struct {
	Upstream       []byte            // the upstream card, verbatim
	PublicURL      string            // {public_url}
	ServicePath    string            // {svc}: /agents/{name} or /agents/{tenant}/{name}
	OnlyAnonymous  bool              // allow: is exactly [anonymous]: no security fields
	OIDC           bool              // add an openIdConnect scheme (operator IdP)
	SealedURI      string            // https://kontra.sh/ext/sealed/v1 for a sealed service (M3), else ""
	CardExtensions []json.RawMessage // the operator's card_extensions annotation (spec §13.4)
}

// Normalize converts a pre-1.0 card to the 1.0 shape as raw JSON; a 1.0 card is returned as is.
func Normalize(upstream []byte) ([]byte, error) {
	return nil, unimpl.Error("card.Normalize", "WP-04")
}

// Rewrite returns the served card bytes.
func Rewrite(in Input) ([]byte, error) {
	return nil, unimpl.Error("card.Rewrite", "WP-04")
}

// Signer signs a served card (ES256 over RFC 8785 JCS, protected header with kontra.upstream, D13). M3.
type Signer interface {
	Sign(ctx context.Context, served []byte, upstreamSHA256 [32]byte, upstreamURL string, verified bool) (json.RawMessage, error)
}

// Served is one service's current cards.
type Served struct {
	Body         []byte // the served card
	Upstream     []byte // the upstream card, verbatim
	ETag         string // hash of Body
	LastModified time.Time
	Unavailable  bool // configured seller keys and no verifying signature (M3): card routes answer 503
}

// Handler serves a card with Cache-Control: max-age=60, ETag and Last-Modified (the TCK hard-fails
// without them). upstream selects the verbatim upstream card.
func Handler(get func(r *http.Request) (*Served, error), upstream bool) http.Handler {
	panic(unimpl.Error("card.Handler", "WP-06"))
}
