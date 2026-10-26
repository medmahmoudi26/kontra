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
// writes a new first line and keeps the old one below it, stamped "# rotated: <time>", for
// RotationOverlap; Prune then drops it (§12, PRD §8). Lines the node did not write (an operator's notes, a
// decrypt-only key without a stamp) are kept as they are. The CLI refuses to start when the file is
// readable or writable by group or others. One key per node, enrolled separately at each gate it dials.
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

// RotationOverlap is how long a rotated key's line stays in the file, decrypt-only (§12: the gate keeps
// the old key serving its tasks for 24 h, and parts sealed to it still open).
const RotationOverlap = 24 * time.Hour

// The comment lines of a key (age-keygen writes the first two; §2.1 adds the key id). A rotated key gets
// a fourth, which Prune reads.
const (
	CommentCreated = "# created: "
	CommentPublic  = "# public key: "
	CommentKeyID   = "# kontra key id: "
	CommentRotated = "# rotated: "
)

// maxFileSize is age.ParseIdentities' own limit.
const maxFileSize = 1 << 24

// Identity is a loaded identity file.
type Identity struct {
	Path    string
	Active  *age.X25519Identity   // the first line
	Older   []*age.X25519Identity // decrypt-only lines, newest first
	Created time.Time             // the active line's "# created:" comment

	all []age.Identity // every key in the file, in order, as age parsed it
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

// Rotate writes a new first line (with its comments and a "# rotated:" stamp on the old one) and keeps
// every older line below it, atomically (write a temporary file in the same directory, fsync, rename).
// It returns the new identity; the caller connects with rotate_from naming the old key (§12).
func Rotate(path string, now time.Time) (*Identity, error) {
	_, f, err := load(path)
	if err != nil {
		return nil, err
	}
	key, err := age.GenerateX25519Identity()
	if err != nil {
		return nil, fmt.Errorf("nodekey: generate: %w", err)
	}
	if old := &f.blocks[0]; old.rotated.IsZero() {
		old.lines = append(old.lines, CommentRotated+stamp(now))
		old.rotated = now
	}
	f.blocks = append([]block{newBlock(key, now)}, f.blocks...)
	data := f.bytes()
	id, _, err := parse(path, data)
	if err != nil {
		return nil, err
	}
	if err := replace(path, data); err != nil {
		return nil, err
	}
	return id, nil
}

// Prune drops the decrypt-only lines rotated more than RotationOverlap before now; it reports whether the
// file changed. The active key, and a later key without a "# rotated:" stamp, are never dropped.
//
// The stamp is the time Rotate wrote the file, not the time a gate committed the rotation. A caller prunes
// only after the gates it uses have welcomed the rotated key (a hello with rotate_from, §12): a key pruned
// before then can no longer prove rotate_from, and the new key is unknown at a gate that has not
// committed the rotation.
func Prune(path string, now time.Time) (bool, error) {
	_, f, err := load(path)
	if err != nil {
		return false, err
	}
	kept := []block{f.blocks[0]}
	for _, b := range f.blocks[1:] {
		if !b.rotated.IsZero() && !now.Before(b.rotated.Add(RotationOverlap)) {
			continue
		}
		kept = append(kept, b)
	}
	if len(kept) == len(f.blocks) {
		return false, nil
	}
	f.blocks = kept
	data := f.bytes()
	if _, _, err := parse(path, data); err != nil {
		return false, err
	}
	if err := replace(path, data); err != nil {
		return false, err
	}
	return true, nil
}

// load opens the file, checks the mode of what it opened, and parses it.
func load(path string) (*Identity, *keyFile, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("nodekey: %w", err)
	}
	defer fh.Close()
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

// parse reads an identity file as age does, and its blocks as Rotate and Prune rewrite them.
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
	id := &Identity{Path: path, Active: active, Created: f.blocks[0].created, all: ids}
	for _, x := range ids[1:] {
		if o, ok := x.(*age.X25519Identity); ok {
			id.Older = append(id.Older, o)
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
	lines   []string // the comment and blank lines above the key
	key     string   // the AGE-SECRET-KEY… line
	created time.Time
	rotated time.Time // zero unless a "# rotated:" line stamps the key
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
				if v, ok := strings.CutPrefix(c, CommentRotated); ok {
					b.rotated, _ = time.Parse(time.RFC3339, strings.TrimSpace(v))
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
