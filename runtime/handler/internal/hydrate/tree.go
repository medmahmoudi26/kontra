package hydrate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/medmahmoudi26/kontra/runtime/handler/internal/cas"
)

// A tar.gz artifact is expanded ONCE, into a golden tree named by the archive's digest, and
// every working directory is a copy-on-write clone of that tree.
//
// The alternative — content-addressing each member file and materializing from a manifest —
// is what a package manager does, and it is wrong here for a measured reason: the Node
// runtime tarball carries tens of thousands of small files, and store-if-absent on each one
// means tens of thousands of fsyncs on a path whose whole purpose is to make a cold start
// fast. The archive is the unit the digest names, so the archive is the unit the store
// holds. The tree beside it is derived state, rebuildable from the object at any time.

// ensureTree expands the artifact's archive into <root>/trees/<digest>, once, and returns
// the path and the CAS address of the tree's inventory. It is safe to call concurrently from
// any number of processes: the expansion happens in a temporary directory and is published by
// a single rename, so the loser of the race discards its work and uses the winner's. A
// partially expanded tree is never visible under the name callers read.
//
// THE TREE IS VERIFIED BEFORE IT IS CLONED, and that is issue 15's half of this function. The
// golden tree is the SOURCE of every working directory, so a tree that lost files to a
// half-finished `rm -rf` or to a filesystem repair does not produce a detectable failure — it
// produces a perfectly faithful copy of a broken artifact, in every working directory, for as
// long as the tree survives. An unvouched-for or damaged tree is thrown away and expanded
// again out of the object this machine already holds: no network, and derived state is the
// one thing that is always cheaper to rebuild than to argue about.
func (s *Store) ensureTree(a Artifact) (string, string, error) {
	final := filepath.Join(s.trees, a.Digest)
	switch st, err := os.Stat(final); {
	case err == nil:
		if !st.IsDir() {
			return "", "", fmt.Errorf("expanded tree path %s is not a directory", final)
		}
		if digest, ok := s.treeIsSound(a, final); ok {
			return final, digest, nil
		}
		// THE TREE GOES FIRST AND ITS RECEIPT SECOND, which is the opposite order from a
		// working directory's and is deliberate in both places. A working copy must never
		// have a receipt while it is being deleted; a TREE must never be visible WITHOUT
		// one, because a second process that finds an unvouched-for tree throws it away —
		// possibly out from under a third that is cloning it. Removing the directory first
		// means that window never opens; the leftover this can produce, a receipt naming a
		// tree that is not there, reads as damage and is expanded again.
		if err := discard(final); err != nil {
			return "", "", err
		}
		if err := dropReceipt(final); err != nil {
			return "", "", err
		}
	case !errors.Is(err, fs.ErrNotExist):
		return "", "", fmt.Errorf("inspect expanded tree %s: %w", final, err)
	}

	tmp, err := os.MkdirTemp(s.trees, ".expanding-")
	if err != nil {
		return "", "", cas.DiskError("create expansion directory in", s.trees, err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	// The expansion's total is the artifact's own expanded size, which nothing knows before
	// reading it — so this phase reports bytes and files with no denominator, deliberately.
	rep := s.reportFor(a.Name, PhaseExpand, 0)
	inv, err := s.readArchive(a, tmp, rep)
	rep.done()
	if err != nil {
		return "", "", err
	}

	// INVENTORY, THEN RECEIPT, THEN THE TREE ITSELF — and that last ordering is the one that
	// is easy to get backwards. Publishing the tree first would leave a window, however
	// short, in which a complete tree carries no receipt; a second process arriving inside
	// that window reads it as unvouched-for and throws away a directory a third process may
	// be halfway through cloning. Receipt first means a tree is never visible without one,
	// and the leftover a crash can produce — a receipt naming a tree that is not there — is
	// read as damage and expanded again, which is exactly what it should be.
	listingDigest, err := s.putListing(inv)
	if err != nil {
		return "", "", err
	}
	if err := s.receipt(a, final, listingDigest, "", cas.Shared); err != nil {
		return "", "", err
	}

	if err := os.Rename(tmp, final); err != nil {
		if !lostTheRename(err) {
			return "", "", cas.DiskError("publish expanded tree", final, err)
		}
		// Another process published first. Its bytes came from the same object under the
		// same digest, so its tree is ours — and its inventory is the same object as ours,
		// because the listing is addressed by its own content.
		if st, serr := os.Stat(final); serr != nil || !st.IsDir() {
			return "", "", fmt.Errorf("publish expanded tree %s: %w", final, err)
		}
	}
	return final, listingDigest, nil
}

// treeIsSound answers whether the tree at path is vouched for AND still whole, and hands back
// the inventory it was checked against.
func (s *Store) treeIsSound(a Artifact, path string) (string, bool) {
	r, err := readReceipt(path, a.Digest)
	if err != nil || r == nil || r.Listing == "" {
		return "", false
	}
	l, err := s.readListing(r.Listing)
	if err != nil {
		return "", false
	}
	if err := l.check(path, Structure, a.Name); err != nil {
		return "", false
	}
	return r.Listing, true
}

// readArchive streams the stored object through the expander AND through sha256 at the same
// time, because the read that expands an archive is the only read of those bytes that is
// going to happen — and it is therefore the only chance to notice that they are not the bytes
// the address names.
//
// dest may be empty, which reads the archive and produces its inventory without writing a
// single file. That is what lets an existing working directory be checked, and adopted,
// without first rebuilding a 450 MB tree to compare it against.
//
// WHICH ERROR WINS IS THE INTERESTING PART. A corrupt object and a genuinely malformed
// archive both surface as "unexpected EOF" out of gzip, and they want opposite responses:
// re-fetch, or stop and say the artifact is broken. So on ANY failure the rest of the object
// is drained through the hash first, and the digest decides. A mismatch means the disk
// changed the bytes and the expansion failure is a symptom; a match means the object is
// exactly what upstream published and the archive really is malformed, so fetching it again
// would produce the same file and the same failure.
func (s *Store) readArchive(a Artifact, dest string, rep *reporter) (*listing, error) {
	blob, err := s.cas.Open(a.Digest)
	if err != nil {
		return nil, err
	}
	defer blob.Close()

	limit := a.MaxExpanded
	if limit <= 0 {
		limit = defaultMaxExpanded
	}
	hashed := &hashingReader{r: blob, h: sha256.New()}
	inv, expandErr := expand(hashed, dest, a.StripComponents, limit, rep)

	got, drainErr := hashed.rest()
	if got != a.Digest && drainErr == nil {
		return nil, fmt.Errorf("expand %s: %w", a.Name, &cas.ObjectMutated{
			Path: s.objectPath(a.Digest), Want: a.Digest, Got: got, Size: hashed.n,
		})
	}
	if expandErr != nil {
		return nil, fmt.Errorf("expand %s: %w", a.Name, expandErr)
	}
	if drainErr != nil {
		return nil, fmt.Errorf("expand %s: read the stored object: %w", a.Name, drainErr)
	}
	return inv, nil
}

// inventoryFromObject produces an artifact's inventory from the bytes in the store, writing
// nothing to disk but the (small) inventory itself.
func (s *Store) inventoryFromObject(a Artifact) (string, error) {
	inv, err := s.readArchive(a, "", nil)
	if err != nil {
		return "", err
	}
	return s.putListing(inv)
}

// putListing stores an inventory under its own address. Content-addressed like everything
// else, so every working copy of one artifact names one object and a second expansion of the
// same archive stores nothing new.
//
// AN OCCUPIED ADDRESS IS CHECKED, WHICH STORE-IF-ABSENT DOES NOT DO. Everywhere else in the
// store that is right: the address is the hash, so bytes already at an address are by
// construction the bytes being written. An inventory that was corrupted ON DISK breaks that
// assumption in the one place it matters most — the corrupt inventory cannot be read, so
// every working copy it describes reads as damaged, so every start re-hydrates, so every
// start writes the same inventory again and store-if-absent keeps the corruption. Here, and
// only here, the caller is holding the authoritative bytes in memory, so it can simply
// replace them.
func (s *Store) putListing(l *listing) (string, error) {
	body := l.encode()
	want := cas.Sha256Hex(body)
	if has, err := s.cas.Has(want); err == nil && has {
		if s.cas.Verify(want) != nil {
			if err := s.cas.Discard(want); err != nil {
				return "", err
			}
		}
	}
	digest, _, err := s.cas.Put(bytes.NewReader(body), "the artifact listing")
	if err != nil {
		return "", err
	}
	return digest, nil
}

func (s *Store) objectPath(digest string) string {
	p, err := s.cas.Path(digest)
	if err != nil {
		return digest
	}
	return p
}

// hashingReader hashes everything read through it and can finish the job when its reader
// stops early — which gzip and tar both do, since neither has any reason to look at the bytes
// after the end of the archive.
type hashingReader struct {
	r io.Reader
	h hash.Hash
	n int64
}

func (h *hashingReader) Read(p []byte) (int, error) {
	n, err := h.r.Read(p)
	if n > 0 {
		h.h.Write(p[:n])
		h.n += int64(n)
	}
	return n, err
}

// rest drains whatever is left and returns the digest of the WHOLE stream.
func (h *hashingReader) rest() (string, error) {
	if _, err := io.Copy(io.Discard, h); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.h.Sum(nil)), nil
}

// lostTheRename distinguishes "somebody else got there first" from a real failure. Renaming
// onto a populated directory is ENOTEMPTY on Linux and macOS; EEXIST shows up on some
// filesystems, and ENOTDIR if the winner published something that is not a directory (which
// the caller then checks).
func lostTheRename(err error) bool {
	return errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, fs.ErrExist) || errors.Is(err, syscall.ENOTDIR)
}

// expand untars gz into root and returns the inventory of what came out of it. It refuses
// anything that would write outside root, anything that is not a file, directory, symlink or
// hardlink, and anything that would expand past the cap.
//
// AN EMPTY root READS WITHOUT WRITING. Every check, every refusal and every inventory entry
// is identical; only the filesystem calls are skipped. One function rather than two because
// the two would have to agree about the inventory forever, and the day they stopped agreeing
// would be a day a working directory verified against a listing describing something else.
//
// THE INVENTORY IS BUILT FROM THE SAME BYTES THE EXPANSION WRITES. Each member is hashed as
// it streams past on its way to disk, so the listing costs a sha256 over data that is already
// in hand and no second pass at all.
func expand(gz io.Reader, root string, strip int, limit int64, rep *reporter) (*listing, error) {
	zr, err := gzip.NewReader(gz)
	if err != nil {
		return nil, fmt.Errorf("read gzip: %w", err)
	}
	defer zr.Close()

	tr := tar.NewReader(zr)
	inv := &listing{}
	// files remembers what each member expanded to, so that a tar hardlink — which carries no
	// bytes of its own — can be inventoried as the file it points at. Without it a hardlinked
	// entry would have no digest and no size, and every verification of it would be a check
	// that checked nothing.
	files := map[string]entry{}
	var written int64
	// File modes are applied with an explicit chmod rather than trusting the create mode:
	// the process umask masks O_CREATE's permissions, and an artifact whose binaries lose
	// their execute bit to a 077 umask fails later, somewhere else, as "permission denied"
	// on a file that visibly exists.
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return inv, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read tar: %w", err)
		}

		rel, ok := safeRel(hdr.Name, strip)
		if !ok {
			return nil, fmt.Errorf("tar entry %q escapes the destination directory", hdr.Name)
		}
		if rel == "" {
			continue // the stripped-away top-level directory itself
		}
		target := ""
		if root != "" {
			target = filepath.Join(root, filepath.FromSlash(rel))
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if target != "" {
				if err := os.MkdirAll(target, 0o755); err != nil {
					return nil, cas.DiskError("create", target, err)
				}
			}
			inv.addDir(rel)

		case tar.TypeReg:
			if written+hdr.Size > limit {
				return nil, fmt.Errorf("expands past the %d-byte cap at %q; raise Artifact.MaxExpanded if the artifact really is this big", limit, hdr.Name)
			}
			perm := fs.FileMode(hdr.Mode).Perm()
			h := sha256.New()
			var n int64
			if target == "" {
				if n, err = io.Copy(h, tr); err != nil {
					return nil, err
				}
			} else {
				if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
					return nil, cas.DiskError("create", filepath.Dir(target), err)
				}
				f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
				if err != nil {
					return nil, cas.DiskError("create", target, err)
				}
				n, err = io.Copy(io.MultiWriter(f, h), tr)
				if err != nil {
					f.Close()
					return nil, cas.DiskError("expand into", target, err)
				}
				if err := f.Chmod(perm); err != nil {
					f.Close()
					return nil, err
				}
				// CLOSE IS A WRITE, and on more filesystems than not it is the one that
				// fails: buffered pages are flushed here, so a disk that filled during the
				// copy reports ENOSPC out of close rather than out of io.Copy.
				if err := f.Close(); err != nil {
					return nil, cas.DiskError("finish writing", target, err)
				}
			}
			e := entry{kind: kindFile, path: rel, digest: hex.EncodeToString(h.Sum(nil)), size: n, exec: perm&0o111 != 0}
			files[rel] = e
			inv.addFile(e.path, e.digest, e.size, e.exec)
			written += n
			rep.add(n, 1)

		case tar.TypeSymlink:
			if !safeLink(rel, hdr.Linkname) {
				return nil, fmt.Errorf("tar entry %q is a symlink to %q, which leaves the destination directory", hdr.Name, hdr.Linkname)
			}
			if target != "" {
				if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
					return nil, cas.DiskError("create", filepath.Dir(target), err)
				}
				if err := os.Symlink(filepath.FromSlash(hdr.Linkname), target); err != nil {
					return nil, cas.DiskError("link", target, err)
				}
			}
			inv.addSymlink(rel, hdr.Linkname)

		case tar.TypeLink:
			linkRel, ok := safeRel(hdr.Linkname, strip)
			if !ok || linkRel == "" {
				return nil, fmt.Errorf("tar entry %q is a hardlink to %q, which leaves the destination directory", hdr.Name, hdr.Linkname)
			}
			src, ok := files[linkRel]
			if !ok {
				// tar hardlinks always follow their target, so a link to something this
				// archive has not produced is an archive that is not internally consistent.
				return nil, fmt.Errorf("tar entry %q is a hardlink to %q, which the archive has not written", hdr.Name, hdr.Linkname)
			}
			if target != "" {
				if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
					return nil, cas.DiskError("create", filepath.Dir(target), err)
				}
				if err := os.Link(filepath.Join(root, filepath.FromSlash(linkRel)), target); err != nil {
					return nil, cas.DiskError("link", target, err)
				}
			}
			// The link IS the file it points at — same inode, same bytes, same mode — so it
			// is inventoried as that file under its own name.
			inv.addFile(rel, src.digest, src.size, src.exec)

		case tar.TypeXGlobalHeader, tar.TypeXHeader, tar.TypeGNULongName, tar.TypeGNULongLink:
			// pax/GNU metadata; archive/tar has already applied it to the next header.

		default:
			// A device node, a fifo or a socket in a hydrated artifact is not a packaging
			// quirk to route around. Refuse and name it.
			return nil, fmt.Errorf("tar entry %q has unsupported type %q", hdr.Name, string(hdr.Typeflag))
		}
	}
}

// safeRel strips leading components and returns a path guaranteed to stay under the
// destination. The second return is false for anything that would not.
//
// A ".." component is REFUSED, not resolved. The obvious implementation —
// path.Clean("/"+name) — is a trap: it turns "../escaped" into "escaped", which does stay
// inside the destination and is therefore "safe", and quietly writes the entry under a name
// the archive never used. An artifact that is not what its archive says is not an artifact
// this store should be handing to anything.
func safeRel(name string, strip int) (string, bool) {
	name = strings.ReplaceAll(name, `\`, "/")
	if path.IsAbs(name) {
		return "", false
	}
	var parts []string
	for _, p := range strings.Split(name, "/") {
		switch p {
		case "", ".":
			// tar writes directories as "dir/"; both are no-ops.
		case "..":
			return "", false
		default:
			parts = append(parts, p)
		}
	}
	if len(parts) == 0 {
		return "", true
	}
	if strip > 0 {
		if len(parts) <= strip {
			return "", true
		}
		parts = parts[strip:]
	}
	return strings.Join(parts, "/"), true
}

// safeLink checks that a symlink's target, resolved from the link's own directory, stays
// inside the tree. Absolute targets are refused outright: an artifact that links to
// /usr/lib is describing the machine it was built on, not itself.
func safeLink(linkRel, linkname string) bool {
	if linkname == "" || path.IsAbs(linkname) || strings.HasPrefix(linkname, `\`) {
		return false
	}
	resolved := path.Join(path.Dir(linkRel), linkname)
	return resolved != ".." && !strings.HasPrefix(resolved, "../")
}

// cloneTree materializes the golden tree at dst by copy-on-write, file by file. It builds
// under a sibling temporary and publishes with one rename, so a caller that dies halfway
// leaves debris rather than a working directory that exists and is incomplete — which is
// worse, because "does the directory exist" reports success. (Not producing one is this
// function's job; DETECTING one that something else produced is verify.go's, and the two are
// not substitutes — an atomic publish says nothing about what happened to the directory
// afterwards.)
func cloneTree(src, dst string, mode cas.Mode, rep *reporter) (method cas.Method, files int, bytes int64, existing bool, err error) {
	tmp, err := os.MkdirTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".hydrating-")
	if err != nil {
		return "", 0, 0, false, cas.DiskError("create working directory beside", dst, err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	walkErr := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(tmp, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}

		switch {
		case d.IsDir():
			// |0700: the directory has to be traversable and writable while its contents
			// are being placed into it, whatever the artifact said.
			return cas.DiskError("create", target, os.MkdirAll(target, info.Mode().Perm()|0o700))

		case d.Type()&fs.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return cas.DiskError("link", target, os.Symlink(link, target))

		case info.Mode().IsRegular():
			m, err := cas.CopyOnWrite(p, target, mode)
			if err != nil {
				return err
			}
			method = cas.Weakest(method, m)
			files++
			bytes += info.Size()
			rep.add(info.Size(), 1)
			return nil

		default:
			return fmt.Errorf("%s is %s, which does not belong in an artifact", p, info.Mode().Type())
		}
	})
	if walkErr != nil {
		return "", 0, 0, false, walkErr
	}

	if err := os.Rename(tmp, dst); err != nil {
		if !lostTheRename(err) {
			return "", 0, 0, false, cas.DiskError("publish working directory", dst, err)
		}
		return "", 0, 0, true, nil
	}
	return method, files, bytes, false, nil
}
