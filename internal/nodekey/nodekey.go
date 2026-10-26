// Package nodekey is the node's identity key file (spec/tunnel.md §2.1–§2.2, §12; D29).
//
// On first run `kontra connect` creates ~/.kontra/identity.key (file 0600, directory ~/.kontra 0700;
// `--key` or KONTRA_KEY overrides the path, e.g. a mounted secret). It is an age identity file, the format
// age-keygen writes, so `age -d -i ~/.kontra/identity.key` opens a sealed part:
//
//	# created: 2026-10-25T10:00:00Z
//	# public key: age1…
//	# kontra key id: k1_…
//	AGE-SECRET-KEY-1…
//
// The first AGE-SECRET-KEY-1 line is the active key: it proves possession in the handshake (§4.1) and is
// the sealing recipient of every service the node hosts (D29). Later lines are decrypt-only: `--rotate`
// writes a new first line and keeps the old one below it for RotationOverlap, then drops it (§12, PRD §8).
// The CLI refuses to start when the file is readable by group or others. One key per node, enrolled
// separately at each gate it dials.
//
// age keeps the raw scalar unexported: PrivateKey bech32-decodes the AGE-SECRET-KEY-1 line itself (BIP-173
// without the 90-character limit) for crypto/ecdh and crypto/hpke.
//
// Per-job keys (~/.kontra/jobs/<gate host>/<job id>.key, §2.3) are sealing's (M3).
//
// Work package: WP-01.
package nodekey

import (
	"crypto/ecdh"
	"time"

	"filippo.io/age"

	"kontra.sh/kontra/internal/unimpl"
)

// Paths (§2.1).
const (
	DirName  = ".kontra"
	FileName = "identity.key"
	// EnvPath overrides the path, as --key does.
	EnvPath = "KONTRA_KEY"
	// FileMode and DirMode are what the CLI creates; a group- or other-readable file is refused.
	FileMode = 0o600
	DirMode  = 0o700
)

// RotationOverlap is how long a rotated key's line stays in the file, decrypt-only (§12: the gate keeps
// the old key serving its tasks for 24 h, and parts sealed to it still open).
const RotationOverlap = 24 * time.Hour

// Identity is a loaded identity file.
type Identity struct {
	Path    string
	Active  *age.X25519Identity   // the first line
	Older   []*age.X25519Identity // decrypt-only lines, newest first
	Created time.Time             // the active line's "# created:" comment
}

// Resolve picks the path: the --key flag, else $KONTRA_KEY, else ~/.kontra/identity.key.
func Resolve(flag string, getenv func(string) string) (string, error) {
	return "", unimpl.Error("nodekey.Resolve", "WP-01")
}

// LoadOrCreate loads the file at path, or creates it (and its directory, DirMode) with a new X25519 key
// and the three comment lines when it does not exist. created reports which.
func LoadOrCreate(path string, now time.Time) (id *Identity, created bool, err error) {
	return nil, false, unimpl.Error("nodekey.LoadOrCreate", "WP-01")
}

// Load reads an existing file: CheckMode, then age.ParseIdentities; the first identity must be an
// X25519 one.
func Load(path string) (*Identity, error) {
	return nil, unimpl.Error("nodekey.Load", "WP-01")
}

// CheckMode refuses a file readable or writable by group or others (§2.1).
func CheckMode(path string) error {
	return unimpl.Error("nodekey.CheckMode", "WP-01")
}

// Recipient is the active key's canonical age1… string.
func (i *Identity) Recipient() string {
	panic(unimpl.Error("nodekey.Identity.Recipient", "WP-01"))
}

// KeyID is tunnel.KeyID of the active recipient.
func (i *Identity) KeyID() string {
	panic(unimpl.Error("nodekey.Identity.KeyID", "WP-01"))
}

// PrivateKey is the active key's X25519 scalar for the proof (§4.1).
func (i *Identity) PrivateKey() (*ecdh.PrivateKey, error) {
	return nil, unimpl.Error("nodekey.Identity.PrivateKey", "WP-01")
}

// OldPrivateKey is the scalar of the decrypt-only key a rotation names (rotate_from's proof_old).
func (i *Identity) OldPrivateKey(recipient string) (*ecdh.PrivateKey, error) {
	return nil, unimpl.Error("nodekey.Identity.OldPrivateKey", "WP-01")
}

// Identities is every key, the active one first, for age.Decrypt (M3 unsealing).
func (i *Identity) Identities() []age.Identity {
	panic(unimpl.Error("nodekey.Identity.Identities", "WP-01"))
}

// Rotate writes a new first line (with its comments and a "# rotated:" stamp on the old one) and keeps
// every older line below it, atomically (write a temporary file in the same directory, fsync, rename).
// It returns the new identity; the caller connects with rotate_from naming the old key (§12).
func Rotate(path string, now time.Time) (*Identity, error) {
	return nil, unimpl.Error("nodekey.Rotate", "WP-01")
}

// Prune drops the decrypt-only lines rotated more than RotationOverlap before now; it reports whether the
// file changed.
func Prune(path string, now time.Time) (bool, error) {
	return false, unimpl.Error("nodekey.Prune", "WP-01")
}
