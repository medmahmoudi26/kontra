package hydrate

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// THE LISTING IS WHAT "COMPLETE" MEANS, WRITTEN DOWN.
//
// Issue 15 exists because `os.Stat(dest) == nil` is not a check. A working directory that
// died between the first and the last file of a copy-on-write expansion EXISTS, and every
// question of the form "is it there" answers yes about it. The only question that separates
// a whole artifact from most of one is "is each of these thirty-one thousand things there",
// and that question needs a list of the thirty-one thousand things.
//
// IT IS PRODUCED WHERE IT IS FREE. The listing is built during {@link expand}, off the same
// bytes the expansion is already streaming: one sha256 per member file, computed on data
// that is in registers on its way to the page cache. Walking a finished tree to build the
// same list would cost a second full read of 450 MB for the same answer.
//
// ONE LISTING DESCRIBES THE GOLDEN TREE AND EVERY WORKING COPY OF IT, and that is the whole
// reason this is cheap enough to run on every start. {@link cloneTree} reproduces the tree
// exactly — same relative paths, same bytes, same symlink targets — and cas.workingPerm
// carries the execute bits through untouched, so a clone matches the listing its source was
// expanded under. Nothing is re-hashed to hydrate a second working directory; the receipt
// beside it just names the same listing object.
//
// IT IS STORED IN THE CAS, addressed by its own sha256 like everything else. That gets three
// things at once: a listing shared by every copy of one artifact costs one object, reading it
// back through ReadVerified detects a listing that was itself tampered with, and there is no
// second store to keep in step (ADR 0031 §2).
//
// WHAT IT DELIBERATELY DOES NOT RECORD:
//
//   - Permissions, beyond the execute bit. A working copy's mode is DERIVED from the stored
//     object's by the materialization mode — Shared strips every write bit — so a mode column
//     would be a field the verifier has to un-transform before comparing, and getting that
//     wrong makes every Shared hydration report damage. The execute bit is the one that both
//     modes carry through unchanged, and it is also the only one whose loss breaks an exec.
//   - Timestamps. tar carries them, the copy-on-write ladder does not preserve them, and a
//     verifier that compared them would report damage on every reflinked file.
//   - Ownership. The appliance hydrates as whoever ran it.

// listingSchema is the first line of every listing. A listing with any other first line is
// refused rather than parsed leniently: this file decides whether something gets exec'd.
const listingSchema = "kontra.hydration.listing/v1"

// entryKind is the leading character of a listing line.
const (
	kindFile    = 'f'
	kindSymlink = 'l'
	kindDir     = 'd'
)

// entry is one thing that must be present for the artifact to be whole.
type entry struct {
	kind   byte
	path   string // slash-relative to the root; never empty, never absolute, never "."
	digest string // kindFile
	size   int64  // kindFile
	exec   bool   // kindFile
	target string // kindSymlink
}

// listing is the complete inventory of one expanded artifact.
type listing struct {
	entries  []entry
	files    int
	symlinks int
	dirs     int
	bytes    int64
}

func (l *listing) addFile(rel, digest string, size int64, exec bool) {
	l.entries = append(l.entries, entry{kind: kindFile, path: rel, digest: digest, size: size, exec: exec})
	l.files++
	l.bytes += size
}

func (l *listing) addSymlink(rel, target string) {
	l.entries = append(l.entries, entry{kind: kindSymlink, path: rel, target: target})
	l.symlinks++
}

func (l *listing) addDir(rel string) {
	l.entries = append(l.entries, entry{kind: kindDir, path: rel})
	l.dirs++
}

// encode renders the listing, sorted by path.
//
// SORTED BECAUSE THE BYTES ARE THE ADDRESS. Two expansions of one archive must produce the
// same listing object or the store holds the same inventory twice under two digests, and
// tar's member order is the archiver's business rather than ours. Sorting also puts a parent
// directory before everything under it, so a verification that walks the list in order
// reports "node/bin is missing" rather than the first of the two hundred files inside it.
//
// PATHS ARE QUOTED. A tar member may contain a space; an unquoted path-in-the-middle format
// would parse such a line into a different path than the one that was written, and a
// verifier that checks the wrong path is worse than no verifier.
func (l *listing) encode() []byte {
	sort.Slice(l.entries, func(i, j int) bool { return l.entries[i].path < l.entries[j].path })
	var b bytes.Buffer
	b.WriteString(listingSchema)
	b.WriteByte('\n')
	for _, e := range l.entries {
		switch e.kind {
		case kindFile:
			x := "-"
			if e.exec {
				x = "x"
			}
			fmt.Fprintf(&b, "f %s %d %s %s\n", e.digest, e.size, x, strconv.Quote(e.path))
		case kindSymlink:
			fmt.Fprintf(&b, "l %s %s\n", strconv.Quote(e.target), strconv.Quote(e.path))
		case kindDir:
			fmt.Fprintf(&b, "d %s\n", strconv.Quote(e.path))
		}
	}
	return b.Bytes()
}

// parseListing reads back what encode wrote. Every malformed line is a refusal, because a
// listing that parsed leniently would silently shrink the set of things being checked.
func parseListing(body []byte) (*listing, error) {
	sc := bufio.NewScanner(bytes.NewReader(body))
	// A single member path may be long; the default 64 KB token is not obviously enough.
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	if !sc.Scan() {
		return nil, errors.New("the artifact listing is empty")
	}
	if got := sc.Text(); got != listingSchema {
		return nil, fmt.Errorf("the artifact listing says its format is %q and this binary reads %q", got, listingSchema)
	}
	l := &listing{}
	for n := 2; sc.Scan(); n++ {
		line := sc.Text()
		if line == "" {
			continue
		}
		e, err := parseEntry(line)
		if err != nil {
			return nil, fmt.Errorf("artifact listing line %d: %w", n, err)
		}
		switch e.kind {
		case kindFile:
			l.files++
			l.bytes += e.size
		case kindSymlink:
			l.symlinks++
		case kindDir:
			l.dirs++
		}
		l.entries = append(l.entries, e)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read the artifact listing: %w", err)
	}
	return l, nil
}

func parseEntry(line string) (entry, error) {
	switch line[0] {
	case kindFile:
		// f <sha256> <size> <x|-> "path"
		fields := strings.SplitN(line, " ", 5)
		if len(fields) != 5 {
			return entry{}, fmt.Errorf("%q is not a file entry", line)
		}
		size, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil {
			return entry{}, fmt.Errorf("%q has an unreadable size: %w", line, err)
		}
		path, err := strconv.Unquote(fields[4])
		if err != nil {
			return entry{}, fmt.Errorf("%q has an unreadable path: %w", line, err)
		}
		if err := validateDigestField(fields[1]); err != nil {
			return entry{}, err
		}
		return entry{kind: kindFile, digest: fields[1], size: size, exec: fields[3] == "x", path: path}, nil

	case kindSymlink:
		// l "target" "path"
		fields := strings.SplitN(line, " ", 3)
		if len(fields) != 3 {
			return entry{}, fmt.Errorf("%q is not a symlink entry", line)
		}
		target, err := strconv.Unquote(fields[1])
		if err != nil {
			return entry{}, fmt.Errorf("%q has an unreadable target: %w", line, err)
		}
		path, err := strconv.Unquote(fields[2])
		if err != nil {
			return entry{}, fmt.Errorf("%q has an unreadable path: %w", line, err)
		}
		return entry{kind: kindSymlink, target: target, path: path}, nil

	case kindDir:
		// d "path"
		fields := strings.SplitN(line, " ", 2)
		if len(fields) != 2 {
			return entry{}, fmt.Errorf("%q is not a directory entry", line)
		}
		path, err := strconv.Unquote(fields[1])
		if err != nil {
			return entry{}, fmt.Errorf("%q has an unreadable path: %w", line, err)
		}
		return entry{kind: kindDir, path: path}, nil
	}
	return entry{}, fmt.Errorf("%q starts with %q, which names no kind of entry", line, string(line[0]))
}

func validateDigestField(d string) error {
	if len(d) != 64 {
		return fmt.Errorf("%q is not a sha256 digest", d)
	}
	for i := 0; i < len(d); i++ {
		if c := d[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return fmt.Errorf("%q is not a sha256 digest", d)
		}
	}
	return nil
}

// check answers whether root holds everything the listing names, in the shape it names it.
//
// EVERYTHING NAMED MUST BE THERE; ANYTHING ELSE MAY BE. This is a one-directional check on
// purpose, and the SPA is the reason it has to be: `kontra up` publishes a symlink at
// `frontend/dist` INSIDE the hydrated working directory, pointing at a separately
// hydrated browser bundle. A verifier that also demanded "and nothing else" would call every
// appliance that serves its own UI damaged, and would re-hydrate 450 MB on every start to
// fix a symlink it had put there itself. Extra entries are not a partial materialization;
// missing ones are the only thing that is.
//
// WHAT IT COSTS, MEASURED (TestWhatVerificationCosts): 7 µs per entry at Structure, which
// extrapolates to roughly 0.2 s for the appliance bundle's 31,823 entries, against 60-120 µs
// per entry at Contents. That 0.2 s is the honest price of the sentence "the second `kontra
// up` does no work at all" no longer being true, and it is the cheapest answer there is:
// there exists no summary of a directory tree that separates complete from nearly-complete
// without looking at the parts. The inventory itself measures ~94 bytes per entry, so about
// 3 MB for that bundle — one object, shared by every working copy of it.
func (l *listing) check(root string, level Level, artifact string) error {
	damaged := func(path, reason string) error {
		return &Damage{Artifact: artifact, Root: root, Path: path, Reason: reason}
	}
	for _, e := range l.entries {
		p := filepath.Join(root, filepath.FromSlash(e.path))
		info, err := os.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			return damaged(e.path, "missing")
		}
		if err != nil {
			return damaged(e.path, err.Error())
		}
		switch e.kind {
		case kindDir:
			if !info.IsDir() {
				return damaged(e.path, fmt.Sprintf("is %s, and the artifact says it is a directory", describe(info.Mode())))
			}

		case kindSymlink:
			if info.Mode()&fs.ModeSymlink == 0 {
				return damaged(e.path, fmt.Sprintf("is %s, and the artifact says it is a symlink", describe(info.Mode())))
			}
			target, err := os.Readlink(p)
			if err != nil {
				return damaged(e.path, err.Error())
			}
			if filepath.ToSlash(target) != e.target {
				return damaged(e.path, fmt.Sprintf("points at %q and the artifact says %q", target, e.target))
			}

		case kindFile:
			if !info.Mode().IsRegular() {
				return damaged(e.path, fmt.Sprintf("is %s, and the artifact says it is a file", describe(info.Mode())))
			}
			// SIZE IS THE TRUNCATION CHECK, and it is the reason this costs an lstat per
			// entry rather than a readdir per directory. A file that was cut short by a disk
			// that filled mid-write is present, is regular, and is the wrong length; nothing
			// cheaper than its length notices.
			if info.Size() != e.size {
				return damaged(e.path, fmt.Sprintf("is %d bytes and the artifact says %d", info.Size(), e.size))
			}
			if (info.Mode().Perm()&0o111 != 0) != e.exec {
				what := "is not executable and the artifact says it is"
				if !e.exec {
					what = "is executable and the artifact says it is not"
				}
				return damaged(e.path, what)
			}
			if level == Contents {
				got, err := sha256File(p)
				if err != nil {
					return damaged(e.path, err.Error())
				}
				if got != e.digest {
					return damaged(e.path, fmt.Sprintf("hashes to sha256:%s and the artifact says sha256:%s", got, e.digest))
				}
			}
		}
	}
	return nil
}

func describe(m fs.FileMode) string {
	switch {
	case m.IsDir():
		return "a directory"
	case m&fs.ModeSymlink != 0:
		return "a symlink"
	case m.IsRegular():
		return "a file"
	default:
		return m.Type().String()
	}
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
