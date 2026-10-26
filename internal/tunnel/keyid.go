package tunnel

import (
	"crypto/ecdh"

	"kontra.sh/kontra/internal/unimpl"
)

// KeyIDPrefix versions the key id derivation (§2.2); a post-quantum age1pq1… recipient uses the same rule.
const KeyIDPrefix = "k1_"

// KeyID is "k1_" + lowercase(base32_nopad(SHA-256(utf8(recipient))))[0:26] (§2.2, D29): 29 characters,
// 130 bits, the shape tunnel_keys.key_id's CHECK names. The gate always stores and checks the full public
// key; it never authenticates anything by a key id alone.
func KeyID(recipient string) string {
	panic(unimpl.Error("tunnel.KeyID", "WP-01"))
}

// ParseRecipient decodes a canonical age1… X25519 recipient into its public key (tunnel_keys.pubkey is
// its 32 bytes). A recipient that is not canonical (upper case, a hybrid age1pq1… key) is refused.
func ParseRecipient(recipient string) (*ecdh.PublicKey, error) {
	return nil, unimpl.Error("tunnel.ParseRecipient", "WP-01")
}

// FormatRecipient is the canonical age1… string of a public key (the gate's stored pubkey back to the
// form KeyID derives from).
func FormatRecipient(pub *ecdh.PublicKey) (string, error) {
	return "", unimpl.Error("tunnel.FormatRecipient", "WP-01")
}
