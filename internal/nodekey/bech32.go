package nodekey

import (
	"crypto/ecdh"
	"errors"
	"fmt"

	"filippo.io/age"

	"kontra.sh/kontra/internal/bech32"
	"kontra.sh/kontra/internal/tunnel"
)

// SecretKeyHRP is the human-readable part of an X25519 age identity (AGE-SECRET-KEY-1…).
const SecretKeyHRP = "AGE-SECRET-KEY-"

// privateKey is the X25519 scalar of an age identity for crypto/ecdh and crypto/hpke. age keeps the raw
// scalar unexported (x25519.go) and its bech32 package internal, so the identity's own string is decoded
// here (BIP-173 without the 90-character limit, internal/bech32). The result is checked against the
// identity's recipient, so a decoding fault can never yield a key that proves for another recipient.
func privateKey(id *age.X25519Identity) (*ecdh.PrivateKey, error) {
	if id == nil {
		return nil, errors.New("nodekey: no identity")
	}
	hrp, scalar, err := bech32.Decode(id.String())
	if err != nil {
		return nil, fmt.Errorf("nodekey: secret key: %w", err)
	}
	if hrp != SecretKeyHRP || len(scalar) != 32 {
		return nil, errors.New("nodekey: secret key: not an X25519 age identity")
	}
	priv, err := ecdh.X25519().NewPrivateKey(scalar)
	if err != nil {
		return nil, fmt.Errorf("nodekey: secret key: %w", err)
	}
	got, err := tunnel.FormatRecipient(priv.PublicKey())
	if err != nil {
		return nil, fmt.Errorf("nodekey: secret key: %w", err)
	}
	if got != id.Recipient().String() {
		return nil, errors.New("nodekey: secret key: the decoded scalar does not match the identity's recipient")
	}
	return priv, nil
}
