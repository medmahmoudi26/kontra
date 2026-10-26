package tunnel

import (
	"crypto/ecdh"
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"fmt"
	"strings"

	"filippo.io/age"

	"kontra.sh/kontra/internal/bech32"
)

// KeyIDPrefix versions the key id derivation (§2.2); a post-quantum age1pq1… recipient uses the same rule.
const KeyIDPrefix = "k1_"

// keyIDChars is the base32 characters a key id keeps: 26 of them, 130 bits.
const keyIDChars = 26

// RecipientHRP is the human-readable part of an X25519 age recipient (age1…).
const RecipientHRP = "age"

// KeyID is "k1_" + lowercase(base32_nopad(SHA-256(utf8(recipient))))[0:26] (§2.2, D29): 29 characters,
// 130 bits, the shape tunnel_keys.key_id's CHECK names. The gate always stores and checks the full public
// key; it never authenticates anything by a key id alone.
func KeyID(recipient string) string {
	sum := sha256.Sum256([]byte(recipient))
	enc := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:])
	return KeyIDPrefix + strings.ToLower(enc[:keyIDChars])
}

// ParseRecipient decodes a canonical age1… X25519 recipient into its public key (tunnel_keys.pubkey is
// its 32 bytes). A recipient that is not canonical (upper case, a hybrid age1pq1… key) is refused.
//
// Canonical means the string age itself writes for the key, so KeyID of it is the one key id of that
// key: age must parse it, it must re-encode to the same string, and its 32 bytes must be a canonical
// X25519 point (the top bit clear and below 2^255 - 19), not one of the low-order points no proof can
// be made for.
func ParseRecipient(recipient string) (*ecdh.PublicKey, error) {
	if _, err := age.ParseX25519Recipient(recipient); err != nil {
		return nil, fmt.Errorf("recipient: %w", err)
	}
	hrp, data, err := bech32.Decode(recipient)
	if err != nil {
		return nil, fmt.Errorf("recipient: %w", err)
	}
	if hrp != RecipientHRP || len(data) != 32 {
		return nil, errors.New("recipient: not an X25519 age1… recipient")
	}
	if again, err := bech32.Encode(RecipientHRP, data); err != nil || again != recipient {
		return nil, errors.New("recipient: not in canonical form (lowercase, as age writes it)")
	}
	if !canonicalPoint(data) {
		return nil, errors.New("recipient: not a canonical X25519 point")
	}
	pub, err := ecdh.X25519().NewPublicKey(data)
	if err != nil {
		return nil, fmt.Errorf("recipient: %w", err)
	}
	if lowOrder(pub) {
		return nil, errors.New("recipient: a low-order X25519 point")
	}
	return pub, nil
}

// FormatRecipient is the canonical age1… string of a public key (the gate's stored pubkey back to the
// form KeyID derives from).
func FormatRecipient(pub *ecdh.PublicKey) (string, error) {
	if pub == nil || pub.Curve() != ecdh.X25519() {
		return "", errors.New("recipient: not an X25519 public key")
	}
	return bech32.Encode(RecipientHRP, pub.Bytes())
}

// canonicalPoint reports whether b is the canonical little-endian encoding of an X25519 u-coordinate:
// the top bit clear and the value below p = 2^255 - 19. X25519 output (and so every key age generates)
// is always canonical; another encoding of the same point would give the same key a second key id.
func canonicalPoint(b []byte) bool {
	if len(b) != 32 || b[31]&0x80 != 0 {
		return false
	}
	// p = 0x7fff…ffed little endian: ed ff … ff 7f. b >= p only when b[31] == 0x7f, b[1..30] are all
	// 0xff and b[0] >= 0xed.
	if b[31] != 0x7f || b[0] < 0xed {
		return true
	}
	for _, x := range b[1:31] {
		if x != 0xff {
			return true
		}
	}
	return false
}

// lowOrderProbe is a fixed scalar: X25519 with any clamped scalar maps exactly the points of small order
// to the all-zero output, which crypto/ecdh refuses.
var lowOrderProbe = func() *ecdh.PrivateKey {
	k, err := ecdh.X25519().NewPrivateKey(sha256Of("kontra.sh/tunnel/v1 low-order probe"))
	if err != nil {
		panic(err)
	}
	return k
}()

func lowOrder(pub *ecdh.PublicKey) bool {
	_, err := lowOrderProbe.ECDH(pub)
	return err != nil
}

func sha256Of(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}
