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
// the sealing recipient of every service the node hosts (D29). Later lines are decrypt-only. `--rotate`
// (Rotate) writes a new first line and keeps the old one below it, stamped "# rotating: <time>"; when a
// gate's welcome confirms the rotation (a hello with rotate_from, §12), MarkRotated stamps it
// "# rotated: <time> <gate origin>", and Prune drops it RotationOverlap after the last gate's stamp (§12,
// PRD §8). Lines the node did not write (an operator's notes, a decrypt-only key without a stamp) are kept:
// Rotate leaves the file's leading notes at the top, and Prune keeps the notes of a key it drops.
//
// Rotate, MarkRotated and Prune rewrite the file under an exclusive flock(2), so that several processes on
// one key (one `kontra connect` per gate, §2.1) never lose each other's change. A symbolic link (a key on a
// mounted volume) is resolved first and its target rewritten in place. The CLI refuses to start when the
// file is readable or writable by group or others. One key per node, enrolled separately at each gate it
// dials.
//
// age keeps the raw scalar unexported: PrivateKey bech32-decodes the AGE-SECRET-KEY-1 line itself (BIP-173
// without the 90-character limit, internal/bech32) for crypto/ecdh and crypto/hpke.
//
// Per-job keys (~/.kontra/jobs/<gate host>/<job id>.key, §2.3) are sealing's (M3).
//
// Work package: WP-01.
package nodekey

import (
	"bytes"
	"crypto/ecdh"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"filippo.io/age"

	"kontra.sh/kontra/internal/tunnel"
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

// RotationOverlap is how long a rotated key's line stays in the file, decrypt-only, after a gate confirmed
// the rotation (§12: the gate sets overlap_until = now + 24 h when it commits the rotation, keeps the old
// key serving its tasks until then, and parts sealed to it still open on the node).
const RotationOverlap = 24 * time.Hour

// The comment lines of a key. age-keygen writes the first two and §2.1 adds the key id. Rotate stamps the
// key it replaces with CommentRotating; MarkRotated adds a CommentRotated line per gate that confirmed the
// rotation ("# rotated: <time> <gate origin>"), and Prune reads those.
const (
	CommentCreated  = "# created: "
	CommentPublic   = "# public key: "
	CommentKeyID    = "# kontra key id: "
	CommentRotating = "# rotating: "
	CommentRotated  = "# rotated: "
)

// nodeComments are the prefixes of the lines this package writes; every other line is the operator's.
var nodeComments = []string{CommentCreated, CommentPublic, CommentKeyID, CommentRotating, CommentRotated}

func nodeLine(l string) bool {
	for _, p := range nodeComments {
		if strings.HasPrefix(l, p) {
			return true
		}
	}
	return false
}

// maxFileSize is age.ParseIdentities' own limit.
const maxFileSize = 1 << 24

// Identity is a loaded identity file.
type Identity struct {
	Path    string
	Active  *age.X25519Identity   // the first line
	Older   []*age.X25519Identity // decrypt-only lines, newest first
	Created time.Time             // the active line's "# created:" comment

	all  []age.Identity // every key in the file, in order, as age parsed it
	file *keyFile       // the file's blocks, parallel to all
}

// Resolve picks the path: the --key flag, else $KONTRA_KEY, else ~/.kontra/identity.key. getenv reads the
// environment (os.Getenv when nil); the home directory is $HOME, else the user's home directory.
func Resolve(flag string, getenv func(string) string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	if getenv == nil {
		getenv = os.Getenv
	}
	if p := getenv(EnvPath); p != "" {
		return p, nil
	}
	home := getenv("HOME")
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil || h == "" {
			return "", fmt.Errorf("nodekey: no home directory for %s/%s: pass --key or set %s", DirName, FileName, EnvPath)
		}
		home = h
	}
	return filepath.Join(home, DirName, FileName), nil
}

// LoadOrCreate loads the file at path, or creates it (and its directory, DirMode) with a new X25519 key
// and the three comment lines when it does not exist. created reports which.
//
// The new file is written in full to a temporary file in the same directory and then linked into place,
// so a concurrent start never reads half a key and never overwrites one: the loser loads the winner's.
// Creating takes no lock: the link puts a file only where there is none, and Rotate, MarkRotated and Prune
// only change a file that exists.
func LoadOrCreate(path string, now time.Time) (id *Identity, created bool, err error) {
	if _, err := os.Stat(path); err == nil {
		id, err := Load(path)
		return id, false, err
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, false, fmt.Errorf("nodekey: %w", err)
	}
	if err := ensureDir(filepath.Dir(path)); err != nil {
		return nil, false, err
	}
	key, err := age.GenerateX25519Identity()
	if err != nil {
		return nil, false, fmt.Errorf("nodekey: generate: %w", err)
	}
	f := &keyFile{blocks: []block{newBlock(key, now)}}
	data := f.bytes()
	id, _, err = parse(path, data)
	if err != nil {
		return nil, false, err
	}
	switch err := createNew(path, data); {
	case errors.Is(err, fs.ErrExist):
		id, err := Load(path)
		return id, false, err
	case err != nil:
		return nil, false, err
	}
	return id, true, nil
}

// Load reads an existing file: CheckMode, then age.ParseIdentities; the first identity must be an
// X25519 one.
func Load(path string) (*Identity, error) {
	id, _, err := load(path)
	return id, err
}

// CheckMode refuses a file readable or writable by group or others (§2.1), and anything but a regular
// file. A symbolic link (a mounted secret) is followed.
func CheckMode(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("nodekey: %w", err)
	}
	return checkInfo(path, fi)
}

func checkInfo(path string, fi fs.FileInfo) error {
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("nodekey: %s is not a regular file", path)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("nodekey: %s has mode %04o: an identity key must not be readable or writable by group or "+
			"others (chmod 600 %s)", path, perm, path)
	}
	return nil
}

// Recipient is the active key's canonical age1… string.
func (i *Identity) Recipient() string { return i.Active.Recipient().String() }

// KeyID is tunnel.KeyID of the active recipient.
func (i *Identity) KeyID() string { return tunnel.KeyID(i.Recipient()) }

// PrivateKey is the active key's X25519 scalar for the proof (§4.1).
func (i *Identity) PrivateKey() (*ecdh.PrivateKey, error) { return privateKey(i.Active) }

// OldPrivateKey is the scalar of the decrypt-only key a rotation names (rotate_from's proof_old).
func (i *Identity) OldPrivateKey(recipient string) (*ecdh.PrivateKey, error) {
	for _, o := range i.Older {
		if o.Recipient().String() == recipient {
			return privateKey(o)
		}
	}
	return nil, fmt.Errorf("nodekey: %s holds no decrypt-only key for %s", i.Path, recipient)
}

// Identities is every key, the active one first, for age.Decrypt (M3 unsealing).
func (i *Identity) Identities() []age.Identity {
	if i.all != nil {
		return append([]age.Identity(nil), i.all...)
	}
	ids := []age.Identity{i.Active}
	for _, o := range i.Older {
		ids = append(ids, o)
	}
	return ids
}

// PendingRotation is the key the active one replaced (the decrypt-only line directly below it that
// Rotate stamped "# rotating:") while gate, a canonical origin, has not confirmed that rotation
// (MarkRotated). A hello to gate then names it in rotate_from and proves it (§12). ok is false when the
// active key replaced none, or gate has confirmed.
//
// After a connection lost between proof and welcome, or a restart, the node does not know whether gate
// committed the rotation, and a rotate_from naming an already rotated key is enroll_invalid: it redials
// with the active key and no rotate_from first, and a welcome means the rotation committed (MarkRotated),
// unknown_key that it did not. A gate that refuses rotate_from (it never held the old key, or revoked it)
// gets the hello again without it, and its welcome of the active key settles the rotation there too
// (internal/node, connect; the SPEC-CONFLICT on Prune).
func (i *Identity) PendingRotation(gate string) (old *age.X25519Identity, ok bool) {
	if i.file == nil || len(i.file.blocks) < 2 || len(i.all) != len(i.file.blocks) {
		return nil, false
	}
	b := i.file.blocks[1]
	if b.rotating.IsZero() {
		return nil, false
	}
	if _, done := b.rotated[gate]; done {
		return nil, false
	}
	old, ok = i.all[1].(*age.X25519Identity)
	return old, ok
}

// Rotate writes a new first line (with its comments, and a "# rotating:" stamp on the old one) and keeps
// every older line below it, atomically (write a temporary file in the same directory, fsync, rename),
// under the file's lock. The operator's lines at the top of the file stay at the top. It returns the new
// identity; the caller connects with rotate_from naming the old key (PendingRotation) and calls
// MarkRotated when the gate's welcome confirms it (§12).
func Rotate(path string, now time.Time) (*Identity, error) {
	var id *Identity
	err := change(path, func(f *keyFile) (bool, error) {
		key, err := age.GenerateX25519Identity()
		if err != nil {
			return false, fmt.Errorf("nodekey: generate: %w", err)
		}
		old := &f.blocks[0]
		// The lines above the old key's own comments are the file's header (an operator's note), not the
		// old key's: they stay above the new key.
		head := len(old.lines)
		for i, l := range old.lines {
			if nodeLine(l) {
				head = i
				break
			}
		}
		nb := newBlock(key, now)
		nb.lines = append(append([]string(nil), old.lines[:head]...), nb.lines...)
		old.lines = append([]string(nil), old.lines[head:]...)
		if old.rotating.IsZero() {
			old.lines = append(old.lines, CommentRotating+stamp(now))
			old.rotating = now
		}
		f.blocks = append([]block{nb}, f.blocks...)
		id, _, err = parse(path, f.bytes())
		return true, err
	})
	if err != nil {
		return nil, err
	}
	return id, nil
}

// MarkRotated records that gate (a canonical origin) confirmed the rotation away from recipient, a
// decrypt-only key in the file, at at: the welcome to a hello whose rotate_from named it, or to a later
// hello of the active key without rotate_from (the lost-welcome probe, PendingRotation), or any other
// welcome of the active key at that gate, which knows the new key and would refuse a rotate_from (a gate
// that refused rotate_from and enrolled the new key on the hello's other authorities). It appends
// "# rotated: <at> <gate>" to that key's lines; a gate already recorded keeps its first record. The gate
// set overlap_until when it committed, which is never after the node saw the welcome, so the node keeps
// the line at least as long as the gate keeps the old key serving.
func MarkRotated(path, recipient, gate string, at time.Time) error {
	if u, err := url.Parse(gate); err != nil {
		return fmt.Errorf("nodekey: gate %q is not an origin", gate)
	} else if c, err := tunnel.CanonicalOrigin(u); err != nil || c != gate {
		return fmt.Errorf("nodekey: gate %q is not a canonical origin (scheme://host[:port])", gate)
	}
	return change(path, func(f *keyFile) (bool, error) {
		for i := range f.blocks {
			if f.blocks[i].recipient != recipient {
				continue
			}
			if i == 0 {
				return false, fmt.Errorf("nodekey: %s is the active key of %s, not a rotated one", recipient, path)
			}
			b := &f.blocks[i]
			if _, ok := b.rotated[gate]; ok {
				return false, nil
			}
			b.lines = append(b.lines, CommentRotated+stamp(at)+" "+gate)
			return true, nil
		}
		return false, fmt.Errorf("nodekey: %s holds no decrypt-only key for %s", path, recipient)
	})
}

// Prune drops the decrypt-only keys whose rotation every gate that confirmed it did so RotationOverlap
// or more before now; it reports whether the file changed. The active key, a key no gate has confirmed
// rotating (only "# rotating:"), and a key without any stamp (the operator's) are never dropped. The
// operator's lines of a dropped key (anything but this package's comments) are kept.
//
// SPEC-CONFLICT: §12 times the overlap from the commit of the rotation at the one gate the node
// rotates at, and §2.1 enrolls one key at each gate a node dials. The file records the gates that
// confirmed (MarkRotated), not the gates the key is enrolled at, so a gate that has not seen rotate_from
// yet (one whose `kontra connect` has not redialed) is not waited for: once the last confirmation is
// 24 h old, the old key is dropped and that gate can no longer be shown rotate_from. Each node process
// should therefore redial its gate with rotate_from (PendingRotation) soon after a rotation; waiting for
// every enrolled gate needs the set of gates, which no spec gives the node.
//
// SPEC-CONFLICT (§12 with §2.1, §5 and §15; M2 WP-06 review), the gates' side of the one above: §12's
// rotate_from must name a key of the gate, neither revoked nor rotated, and §4 retries neither refusal, but
// a rotated key file does not say which gates hold the old key. A gate that never knew it (a first
// enrollment after a rotation elsewhere) or revoked it (rotate after a leak, then revoke the old key)
// refuses rotate_from for good, and as this file never drops a key no gate confirmed, every start would
// send it again. internal/node therefore sends the hello again without rotate_from when a gate refuses it,
// and stamps "# rotated:" for a gate whose welcome of the active key settles the rotation there, whether
// or not that gate made it. Such a stamp is never earlier than a confirming gate's, so it can only delay a
// prune; it also lets a key no gate could rotate (revoked before the rotation reached the gate) be pruned
// 24 h after the new key's welcome rather than never. Proposal for the owner: §12 says what a node does at
// a gate that does not hold the old key.
func Prune(path string, now time.Time) (bool, error) {
	var changed bool
	err := change(path, func(f *keyFile) (bool, error) {
		kept := []block{f.blocks[0]}
		var carry []string // the operator's lines of dropped keys, kept above the next line
		for _, b := range f.blocks[1:] {
			if b.expired(now) {
				for _, l := range b.lines {
					if !nodeLine(l) {
						carry = append(carry, l)
					}
				}
				continue
			}
			if carry != nil {
				b.lines = append(carry, b.lines...)
				carry = nil
			}
			kept = append(kept, b)
		}
		if len(kept) == len(f.blocks) {
			return false, nil
		}
		f.blocks = kept
		f.trailer = append(carry, f.trailer...)
		changed = true
		return true, nil
	})
	return changed, err
}

// change rewrites the key file at path under its lock: it resolves path's symbolic links, locks the file
// (lockKey), reads it, lets edit change it and, when edit reports a change, writes it back atomically in
// the target's directory. The lock is held from the read to the rename, so a concurrent Rotate, MarkRotated
// or Prune (another process on the same key) reads what this one wrote.
func change(path string, edit func(*keyFile) (bool, error)) error {
	target, fh, err := lockKey(path)
	if err != nil {
		return err
	}
	defer fh.Close() // releases the lock
	_, f, err := read(path, fh)
	if err != nil {
		return err
	}
	changed, err := edit(f)
	if err != nil || !changed {
		return err
	}
	data := f.bytes()
	if _, _, err := parse(path, data); err != nil {
		return err
	}
	return replace(target, data)
}

// lockKey resolves path's symbolic links and holds an exclusive flock on the file it names: closing the
// returned file releases it. A key file reached through a link (a key on a mounted volume) is rewritten
// at its target, so the link keeps pointing at the current key. The lock is on the file now at target: a
// process that waited while another renamed a new file over it retakes the lock on the new one.
func lockKey(path string) (target string, fh *os.File, err error) {
	target, err = filepath.EvalSymlinks(path)
	if err != nil {
		return "", nil, fmt.Errorf("nodekey: %w", err)
	}
	for range 1000 {
		fh, err := os.Open(target)
		if err != nil {
			return "", nil, fmt.Errorf("nodekey: %w", err)
		}
		if err := lockFile(fh); err != nil {
			fh.Close()
			return "", nil, fmt.Errorf("nodekey: lock %s: %w", target, err)
		}
		held, err := fh.Stat()
		if err != nil {
			fh.Close()
			return "", nil, fmt.Errorf("nodekey: %w", err)
		}
		if now, err := os.Lstat(target); err == nil && os.SameFile(held, now) {
			return target, fh, nil
		}
		fh.Close()
	}
	return "", nil, fmt.Errorf("nodekey: %s keeps being replaced", target)
}

// load opens the file, checks the mode of what it opened, and parses it.
func load(path string) (*Identity, *keyFile, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("nodekey: %w", err)
	}
	defer fh.Close()
	return read(path, fh)
}

// read checks the mode of the open file fh and parses it.
func read(path string, fh *os.File) (*Identity, *keyFile, error) {
	fi, err := fh.Stat()
	if err != nil {
		return nil, nil, fmt.Errorf("nodekey: %w", err)
	}
	if err := checkInfo(path, fi); err != nil {
		return nil, nil, err
	}
	data, err := io.ReadAll(io.LimitReader(fh, maxFileSize+1))
	if err != nil {
		return nil, nil, fmt.Errorf("nodekey: %w", err)
	}
	if len(data) > maxFileSize {
		return nil, nil, fmt.Errorf("nodekey: %s is over %d bytes", path, maxFileSize)
	}
	return parse(path, data)
}

// parse reads an identity file as age does, and its blocks as Rotate, MarkRotated and Prune rewrite them.
func parse(path string, data []byte) (*Identity, *keyFile, error) {
	ids, err := age.ParseIdentities(bytes.NewReader(data))
	if err != nil {
		return nil, nil, fmt.Errorf("nodekey: %s: %w", path, err)
	}
	active, ok := ids[0].(*age.X25519Identity)
	if !ok {
		return nil, nil, fmt.Errorf("nodekey: %s: the first key is not an X25519 key (AGE-SECRET-KEY-1…), §2.1", path)
	}
	f := parseFile(data)
	if len(f.blocks) != len(ids) {
		return nil, nil, fmt.Errorf("nodekey: %s: %d keys read, age reads %d", path, len(f.blocks), len(ids))
	}
	id := &Identity{Path: path, Active: active, Created: f.blocks[0].created, all: ids, file: f}
	for i, x := range ids {
		if o, ok := x.(*age.X25519Identity); ok {
			f.blocks[i].recipient = o.Recipient().String()
			if i > 0 {
				id.Older = append(id.Older, o)
			}
		}
	}
	return id, f, nil
}

// keyFile is an identity file as lines: each key with the comment and blank lines above it, and the lines
// after the last key. Rewriting it keeps every line it does not change.
type keyFile struct {
	blocks  []block
	trailer []string
}

type block struct {
	lines     []string // the comment and blank lines above the key
	key       string   // the AGE-SECRET-KEY… line
	recipient string   // its age1… recipient; empty for a key that is not X25519
	created   time.Time
	rotating  time.Time            // "# rotating:": when Rotate replaced it as the active key
	rotated   map[string]time.Time // "# rotated: <time> <gate>": the earliest confirmation by each gate
}

// expired reports whether Prune drops the key: at least one gate confirmed its rotation, and every gate
// that did so RotationOverlap or more before now.
func (b *block) expired(now time.Time) bool {
	if len(b.rotated) == 0 {
		return false
	}
	for _, at := range b.rotated {
		if now.Before(at.Add(RotationOverlap)) {
			return false
		}
	}
	return true
}

// parseFile splits the file the way age.ParseIdentities reads it: lines starting with "#" and empty lines
// are not keys, every other line is.
func parseFile(data []byte) *keyFile {
	text := strings.TrimSuffix(string(data), "\n")
	f := &keyFile{}
	var pending []string
	if text != "" {
		for _, l := range strings.Split(text, "\n") {
			l = strings.TrimSuffix(l, "\r")
			if l == "" || strings.HasPrefix(l, "#") {
				pending = append(pending, l)
				continue
			}
			b := block{lines: pending, key: l}
			for _, c := range pending {
				if v, ok := strings.CutPrefix(c, CommentCreated); ok {
					b.created, _ = time.Parse(time.RFC3339, strings.TrimSpace(v))
				}
				if v, ok := strings.CutPrefix(c, CommentRotating); ok {
					b.rotating, _ = time.Parse(time.RFC3339, strings.TrimSpace(v))
				}
				// "# rotated: <time> <gate>"; a line of another shape confirms nothing, so the key is kept.
				if v, ok := strings.CutPrefix(c, CommentRotated); ok {
					fields := strings.Fields(v)
					if len(fields) != 2 {
						continue
					}
					at, err := time.Parse(time.RFC3339, fields[0])
					if err != nil {
						continue
					}
					if prev, ok := b.rotated[fields[1]]; !ok || at.Before(prev) {
						if b.rotated == nil {
							b.rotated = map[string]time.Time{}
						}
						b.rotated[fields[1]] = at
					}
				}
			}
			f.blocks = append(f.blocks, b)
			pending = nil
		}
	}
	f.trailer = pending
	return f
}

func (f *keyFile) bytes() []byte {
	var b strings.Builder
	for _, bl := range f.blocks {
		for _, l := range bl.lines {
			b.WriteString(l + "\n")
		}
		b.WriteString(bl.key + "\n")
	}
	for _, l := range f.trailer {
		b.WriteString(l + "\n")
	}
	return []byte(b.String())
}

// newBlock is a new key with its three comment lines (§2.1).
func newBlock(key *age.X25519Identity, now time.Time) block {
	r := key.Recipient().String()
	ts := stamp(now)
	created, _ := time.Parse(time.RFC3339, ts)
	return block{
		lines:   []string{CommentCreated + ts, CommentPublic + r, CommentKeyID + tunnel.KeyID(r)},
		key:     key.String(),
		created: created,
	}
}

func stamp(t time.Time) string { return t.UTC().Truncate(time.Second).Format(time.RFC3339) }

// ensureDir creates a missing directory with DirMode (whatever the umask). An existing one is left as it
// is: --key may name a file in a directory the node does not own.
func ensureDir(dir string) error {
	fi, err := os.Stat(dir)
	switch {
	case err == nil:
		if !fi.IsDir() {
			return fmt.Errorf("nodekey: %s is not a directory", dir)
		}
		return nil
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("nodekey: %w", err)
	}
	if err := os.MkdirAll(dir, DirMode); err != nil {
		return fmt.Errorf("nodekey: %w", err)
	}
	if err := os.Chmod(dir, DirMode); err != nil {
		return fmt.Errorf("nodekey: %w", err)
	}
	return nil
}

// writeTemp writes data to a new FileMode file in dir and syncs it; the caller links or renames it.
func writeTemp(dir string, data []byte) (string, error) {
	tmp, err := os.CreateTemp(dir, ".identity-*.tmp")
	if err != nil {
		return "", fmt.Errorf("nodekey: %w", err)
	}
	name := tmp.Name()
	err = tmp.Chmod(FileMode)
	if err == nil {
		_, err = tmp.Write(data)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(name)
		return "", fmt.Errorf("nodekey: %w", err)
	}
	return name, nil
}

// createNew puts data at path only if nothing is there: fs.ErrExist otherwise.
func createNew(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := writeTemp(dir, data)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	if err := os.Link(tmp, path); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fs.ErrExist
		}
		// A file system without hard links: rename, unless the file appeared meanwhile.
		if _, serr := os.Lstat(path); serr == nil {
			return fs.ErrExist
		}
		if err := os.Rename(tmp, path); err != nil {
			return fmt.Errorf("nodekey: %w", err)
		}
	}
	return syncDir(dir)
}

// replace puts data at path atomically: a temporary file in the same directory, fsync, rename.
func replace(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := writeTemp(dir, data)
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("nodekey: %w", err)
	}
	return syncDir(dir)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("nodekey: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil && !errors.Is(err, os.ErrInvalid) {
		return fmt.Errorf("nodekey: sync %s: %w", dir, err)
	}
	return nil
}
