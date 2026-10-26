// Package pins is kontra connect's pin per (gate, service) in ~/.kontra/pins (spec/ext-sealed.md §7, D31,
// D164): whether the service is sealed, the key ids of its recipients, the thumbprints of the seller keys that
// verified its upstream card, and whether that card was signed.
//
// What a hiring node learns from a gate is only what the gate serves, so an active gate could substitute its own
// recipient, drop the extension or strip the seller's signature. Pins make that visible and, where it matters,
// refused: a downgrade (sealed to unsealed, signed to unsigned) is always refused; with --pin-recipients any new
// recipient or seller key is refused; without it a change is refused until the operator runs kontra connect
// with --accept-change <service>, and is named on stderr and in the log (there is no local console, D164).
// kontra seal reads and writes the same file.
//
// The file is JSON, 0600, rewritten atomically under flock(2) like the credentials file, since one
// kontra connect per gate and kontra seal may share it.
//
// Work package: M3 WP-01.
package pins

import (
	"time"

	"kontra.sh/kontra/internal/unimpl"
)

// FileName is the pins file under ~/.kontra.
const FileName = "pins"

// Pin is what was last accepted for one service at one gate.
type Pin struct {
	Sealed     bool      `json:"sealed"`
	Recipients []string  `json:"recipients"`  // key ids (k1_…), sorted
	SellerKeys []string  `json:"seller_keys"` // RFC 7638 thumbprints that verified the upstream card, sorted
	Signed     bool      `json:"signed"`      // the upstream card carried a seller signature the gate verified
	At         time.Time `json:"at"`
}

// Change is one difference between a pin and what a gate now serves.
type Change string

// Changes (ext-sealed §7).
const (
	ChangeUnsealed      Change = "unsealed"       // always refused
	ChangeUnsigned      Change = "unsigned"       // always refused
	ChangeNewRecipient  Change = "new_recipient"  // refused under --pin-recipients, else until accepted
	ChangeNewSellerKey  Change = "new_seller_key" // as new_recipient
	ChangeLessRecipient Change = "removed_recipient"
)

// ChangeError is a refused change; the hiring node answers its local client without forwarding anything.
type ChangeError struct {
	Gate, Service string
	Changes       []Change
}

func (e *ChangeError) Error() string { return "pins: " + e.Service + " at " + e.Gate + " changed" }

// Options are the command line's pin policy.
type Options struct {
	PinRecipients bool     // --pin-recipients
	Accept        []string // --accept-change <service>, repeatable
}

// Store is the pins file.
type Store struct {
	path string
}

// Open opens path (absent is no pin), refusing a file readable or writable by group or others.
func Open(path string) (*Store, error) {
	return nil, unimpl.Error("pins.Open", "M3 WP-01")
}

// Check compares what gate serves for service now with its pin. A first sight is recorded and accepted
// (trust on first use). An accepted change is recorded; a refused one is a *ChangeError.
func (s *Store) Check(gate, service string, now Pin, opts Options) error {
	return unimpl.Error("pins.Store.Check", "M3 WP-01")
}

// Get returns the pin of service at gate, ok false when there is none.
func (s *Store) Get(gate, service string) (Pin, bool, error) {
	return Pin{}, false, unimpl.Error("pins.Store.Get", "M3 WP-01")
}
