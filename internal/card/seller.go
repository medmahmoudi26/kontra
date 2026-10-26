package card

import (
	"context"
	"encoding/json"
	"net/http"

	"kontra.sh/kontra/internal/unimpl"
)

// SellerKeys verifies an upstream card's signatures against the keys the operator configured for its service
// (services.<name>.seller_keys: an inline JWKS, or {"url": "https://…"} fetched with the gate's SSRF-safe dialer,
// never a jku taken from a card; spec/relay.md §3). With keys configured, a card none of whose signatures
// verifies makes the service unavailable: its card routes answer 503 and a new task fails service_unavailable
// at its first attempt; a failed fetch keeps the last verified card for 30 minutes (D89). Building it lifts
// D90's refusal and the seller_keys half of D144.
//
// Work package: M3 WP-02.
type SellerKeys struct {
	raw    json.RawMessage
	client *http.Client
}

// NewSellerKeys parses seller_keys (validated by internal/config). client fetches a {"url"} set; it must be the
// SSRF-safe one (spec/relay.md §10.3).
func NewSellerKeys(raw json.RawMessage, client *http.Client) (*SellerKeys, error) {
	return nil, unimpl.Error("card.NewSellerKeys", "M3 WP-02")
}

// Verify reports whether any of the upstream card's signatures verifies over the JCS of the card without
// signatures, and the RFC 7638 thumbprints of the keys that verified one (pins record them, ext-sealed §7). A
// card without signatures does not verify.
func (s *SellerKeys) Verify(ctx context.Context, upstream []byte) (verified bool, thumbprints []string, err error) {
	return false, nil, unimpl.Error("card.SellerKeys.Verify", "M3 WP-02")
}
