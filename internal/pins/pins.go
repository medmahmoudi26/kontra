// Package pins is kontra connect's pin per (gate, service) in ~/.kontra/pins (spec/ext-sealed.md §7, D31,
// D164): whether the service is sealed, the key ids of its recipients, the thumbprints of the seller keys that
// verified its upstream card, and whether that card was signed.
//
// What a hiring node learns from a gate is only what the gate serves, so an active gate could substitute its own
// recipient, drop the extension or strip the seller's signature. Pins make that visible and, where it matters,
// refused: a downgrade (sealed to unsealed, signed to unsigned) is always refused; with --pin-recipients any new
// recipient, and any new or removed seller key, is refused; without it such a change is refused until the operator
// runs kontra connect with --accept-change <service>, and is named on stderr and in the log (there is no local
// console, D164, D228).
// kontra seal reads and writes the same file.
//
// The file is JSON, 0600, rewritten atomically under flock(2) like the credentials file, since one
// kontra connect per gate and kontra seal may share it.
//
// Work package: M3 WP-01.
package pins

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// FileName is the pins file under ~/.kontra.
const FileName = "pins"

// maxFile bounds the pins file.
const maxFile = 16 << 20

// Pin is what was last accepted for one service at one gate.
type Pin struct {
	Sealed     bool     `json:"sealed"`
	Recipients []string `json:"recipients"` // key ids (k1_…), sorted
	// SellerKeys are the RFC 7638 thumbprints of the seller keys that verified the upstream card (a signature that
	// carries its key), and "kid:<kid>" for a signature that names its key only, which the node cannot verify
	// (sealing.SellerKeyIDs), sorted.
	SellerKeys []string  `json:"seller_keys"`
	Signed     bool      `json:"signed"` // the upstream card carried a seller signature the gate verified
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
	// ChangeLessSellerKey is a pinned seller key the card no longer verifies with: a gate that strips or replaces the
	// seller's signature loses it, so it is refused as a new one is (round 2 sealing-fixes-5, D228).
	ChangeLessSellerKey Change = "removed_seller_key"
	// ChangeSealed and ChangeSigned are accepted and recorded: an upgrade to sealed or signed gives an active gate
	// nothing (and makes a later downgrade refused); so is ChangeLessRecipient.
	ChangeSealed Change = "sealed"
	ChangeSigned Change = "signed"
)

// refusedAlways are refused whatever the options.
func (c Change) refusedAlways() bool { return c == ChangeUnsealed || c == ChangeUnsigned }

// needsAccept are refused under --pin-recipients, and otherwise until --accept-change names the service.
func (c Change) needsAccept() bool {
	return c == ChangeNewRecipient || c == ChangeNewSellerKey || c == ChangeLessSellerKey
}

// ChangeError is a refused change; the hiring node answers its local client without forwarding anything.
type ChangeError struct {
	Gate, Service string
	Changes       []Change
}

func (e *ChangeError) Error() string { return "pins: " + e.Service + " at " + e.Gate + " changed" }

// Detail names the changes and what would accept them, for kontra connect's stderr and log (D164, D187).
func (e *ChangeError) Detail(opts Options) string {
	var parts []string
	for _, c := range e.Changes {
		parts = append(parts, string(c))
	}
	msg := fmt.Sprintf("pins: %s at %s changed (%s)", e.Service, e.Gate, strings.Join(parts, ", "))
	for _, c := range e.Changes {
		if c.refusedAlways() {
			return msg + ": a downgrade is always refused"
		}
	}
	if opts.PinRecipients {
		return msg + ": refused under --pin-recipients"
	}
	return msg + ": run kontra connect with --accept-change " + e.Service + " to accept it"
}

// Options are the command line's pin policy.
type Options struct {
	PinRecipients bool     // --pin-recipients
	Accept        []string // --accept-change <service>, repeatable
}

// Outcome is what Check found and recorded, for kontra connect to print on stderr and in its log: the key ids
// and thumbprints on a first sight and on every change (D187).
type Outcome struct {
	First   bool     // no pin existed: Pin was recorded as seen (trust on first use)
	Changes []Change // the differences from Prev that Check accepted and recorded; empty when nothing changed
	Prev    Pin      // the pin before (zero on a first sight)
	Pin     Pin      // the pin now recorded
}

// Store is the pins file.
type Store struct {
	path string
	now  func() time.Time
}

// file is the pins file's JSON.
type file struct {
	V    int                       `json:"v"`
	Pins map[string]map[string]Pin `json:"pins"` // gate -> service -> pin
}

// Open opens path (absent is no pin), refusing a file readable or writable by group or others.
//
// An existing file is read once here, so that a bad file stops kontra connect at start; every Check reads it
// again under the lock.
func Open(path string) (*Store, error) {
	s := &Store{path: path, now: time.Now}
	fh, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("pins: %w", err)
	}
	defer fh.Close()
	if _, err := read(path, fh); err != nil {
		return nil, err
	}
	return s, nil
}

// Check compares what gate serves for service now with its pin. A first sight is recorded and accepted
// (trust on first use). An accepted change is recorded; a refused one is a *ChangeError.
//
// unsealed (sealed to unsealed) and unsigned (signed to unsigned) are always refused, --accept-change or not.
// new_recipient and new_seller_key are refused under --pin-recipients, and otherwise unless opts.Accept names
// the service. Fewer recipients or seller keys, and an upgrade to sealed or signed, are accepted; a service
// pinned unsealed that turns sealed brings new recipients, which wait for --accept-change like any others (they
// are exactly what an active gate would substitute). A refused check records nothing, so the next run sees the
// same change. Recipients and seller keys are compared as sets
// and recorded sorted; At is when the pin was recorded.
//
// The file is re-read, changed and rewritten atomically under an exclusive flock(2), so that two kontra connect
// processes and kontra seal never lose each other's pins. Where flock(2) does not exist the file is never
// rewritten (D153): a check that needs no write still answers, one that would record something is an error.
func (s *Store) Check(gate, service string, now Pin, opts Options) (Outcome, error) {
	if gate == "" || service == "" {
		return Outcome{}, errors.New("pins: a pin needs a gate and a service")
	}
	now = normalize(now)
	var out Outcome
	err := s.change(func(f *file) (bool, error) {
		prev, ok := f.Pins[gate][service]
		if !ok {
			now.At = s.now().UTC()
			out = Outcome{First: true, Pin: now}
			setPin(f, gate, service, now)
			return true, nil
		}
		changes := diff(prev, now)
		if len(changes) == 0 {
			out = Outcome{Prev: prev, Pin: prev}
			return false, nil
		}
		for _, c := range changes {
			if c.refusedAlways() || (c.needsAccept() && (opts.PinRecipients || !slices.Contains(opts.Accept, service))) {
				return false, &ChangeError{Gate: gate, Service: service, Changes: changes}
			}
		}
		now.At = s.now().UTC()
		out = Outcome{Changes: changes, Prev: prev, Pin: now}
		setPin(f, gate, service, now)
		return true, nil
	})
	if err != nil {
		return Outcome{}, err
	}
	return out, nil
}

// Get returns the pin of service at gate, ok false when there is none.
func (s *Store) Get(gate, service string) (Pin, bool, error) {
	fh, err := os.Open(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return Pin{}, false, nil
	}
	if err != nil {
		return Pin{}, false, fmt.Errorf("pins: %w", err)
	}
	defer fh.Close()
	f, err := read(s.path, fh)
	if err != nil {
		return Pin{}, false, err
	}
	p, ok := f.Pins[gate][service]
	return p, ok, nil
}

// diff lists how now differs from prev, in a fixed order.
func diff(prev, now Pin) []Change {
	var out []Change
	switch {
	case prev.Sealed && !now.Sealed:
		out = append(out, ChangeUnsealed)
	case !prev.Sealed && now.Sealed:
		out = append(out, ChangeSealed)
	}
	switch {
	case prev.Signed && !now.Signed:
		out = append(out, ChangeUnsigned)
	case !prev.Signed && now.Signed:
		out = append(out, ChangeSigned)
	}
	if !subset(now.Recipients, prev.Recipients) {
		out = append(out, ChangeNewRecipient)
	}
	if !subset(prev.Recipients, now.Recipients) {
		out = append(out, ChangeLessRecipient)
	}
	if !subset(now.SellerKeys, prev.SellerKeys) {
		out = append(out, ChangeNewSellerKey)
	}
	if !subset(prev.SellerKeys, now.SellerKeys) {
		out = append(out, ChangeLessSellerKey)
	}
	return out
}

func subset(a, b []string) bool {
	for _, x := range a {
		if !slices.Contains(b, x) {
			return false
		}
	}
	return true
}

// normalize sorts and deduplicates the key lists (never nil, so the file says [] rather than null).
func normalize(p Pin) Pin {
	for _, l := range []*[]string{&p.Recipients, &p.SellerKeys} {
		v := slices.Clone(*l)
		slices.Sort(v)
		v = slices.Compact(v)
		if v == nil {
			v = []string{}
		}
		*l = v
	}
	p.At = time.Time{}
	return p
}

func setPin(f *file, gate, service string, p Pin) {
	if f.Pins == nil {
		f.Pins = map[string]map[string]Pin{}
	}
	if f.Pins[gate] == nil {
		f.Pins[gate] = map[string]Pin{}
	}
	f.Pins[gate][service] = p
}

// change runs edit on the file's contents under its lock and writes the result when edit reports a change.
func (s *Store) change(edit func(*file) (bool, error)) error {
	if err := ensureDir(filepath.Dir(s.path)); err != nil {
		return err
	}
	target, fh, lockErr := lockPins(s.path)
	if fh == nil {
		return lockErr
	}
	defer fh.Close() // releases the lock
	f, err := read(s.path, fh)
	if err != nil {
		return err
	}
	changed, err := edit(f)
	if err != nil || !changed {
		return err
	}
	if lockErr != nil {
		return fmt.Errorf("pins: %s not recorded: %w", s.path, lockErr)
	}
	f.V = 1
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("pins: %w", err)
	}
	return replaceFile(target, append(b, '\n'))
}

// lockPins opens (creating it 0600 when missing) the file at path's target and holds an exclusive flock on it:
// closing the returned file releases it. A process that waited while another renamed a new file over the old
// one retakes the lock on the new one. Where flock(2) does not exist it returns the open file with the lock's
// error: the caller may read, never write.
func lockPins(path string) (target string, fh *os.File, err error) {
	target = path
	if t, err := filepath.EvalSymlinks(path); err == nil {
		target = t
	}
	for range 1000 {
		fh, err := os.OpenFile(target, os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil {
			return "", nil, fmt.Errorf("pins: %w", err)
		}
		if err := lockFile(fh); err != nil {
			return target, fh, fmt.Errorf("pins: lock %s: %w", target, err)
		}
		held, err := fh.Stat()
		if err != nil {
			fh.Close()
			return "", nil, fmt.Errorf("pins: %w", err)
		}
		if now, err := os.Lstat(target); err == nil && os.SameFile(held, now) {
			return target, fh, nil
		}
		fh.Close()
	}
	return "", nil, fmt.Errorf("pins: %s keeps being replaced", target)
}

// read checks the mode of the open file and parses it (empty is no pin).
func read(path string, fh *os.File) (*file, error) {
	fi, err := fh.Stat()
	if err != nil {
		return nil, fmt.Errorf("pins: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("pins: %s is not a regular file", path)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("pins: %s has mode %04o: it must not be readable or writable by group or others "+
			"(chmod 600 %s)", path, perm, path)
	}
	if _, err := fh.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("pins: %w", err)
	}
	b, err := io.ReadAll(io.LimitReader(fh, maxFile+1))
	if err != nil {
		return nil, fmt.Errorf("pins: %w", err)
	}
	if len(b) > maxFile {
		return nil, fmt.Errorf("pins: %s is over %d bytes", path, maxFile)
	}
	f := &file{V: 1, Pins: map[string]map[string]Pin{}}
	if len(bytes.TrimSpace(b)) == 0 {
		return f, nil
	}
	if err := json.Unmarshal(b, f); err != nil {
		return nil, fmt.Errorf("pins: %s is not a pins file this version reads: %w", path, err)
	}
	if f.V != 1 {
		return nil, fmt.Errorf("pins: %s has version %d, this version reads 1", path, f.V)
	}
	if f.Pins == nil {
		f.Pins = map[string]map[string]Pin{}
	}
	return f, nil
}

// replaceFile puts data at path atomically: a 0600 temporary file in the same directory, fsync, rename.
func replaceFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".pins-*.tmp")
	if err != nil {
		return fmt.Errorf("pins: %w", err)
	}
	name := tmp.Name()
	err = tmp.Chmod(0o600)
	if err == nil {
		_, err = tmp.Write(data)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(name, path)
	}
	if err != nil {
		os.Remove(name)
		return fmt.Errorf("pins: %w", err)
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}

// ensureDir creates a missing directory 0700 (~/.kontra). An existing one is left as it is.
func ensureDir(dir string) error {
	fi, err := os.Stat(dir)
	switch {
	case err == nil:
		if !fi.IsDir() {
			return fmt.Errorf("pins: %s is not a directory", dir)
		}
		return nil
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("pins: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("pins: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("pins: %w", err)
	}
	return nil
}
