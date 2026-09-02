package cas

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// Local is the same content-addressed protocol as CAS, over a directory instead of an
// object store: objects live at <root>/cas/<ab>/<digest>, temporaries at <root>/tmp.
//
// It exists because the appliance's two customers need something the S3-backed CAS cannot
// give them. Hydration needs a real file with a real inode, because copy-on-write is a
// filesystem operation and there is no reflinking an S3 key. The embedded registry needs to
// stream layers that do not fit in a []byte. Both need the SAME bytes on disk under the
// SAME address, which is why this is a second backing of one protocol and not a second
// store (ADR 0031 §2).
//
// Its concurrency story is one sentence: nothing partial is ever visible under an address.
// See put for how, and for why there is no lock file.
type Local struct {
	root string
	dir  string // <root>/cas
	// tmp MUST share a filesystem with dir. Publishing an object is os.Link, which is
	// EXDEV across devices — so a tmp directory somewhere else does not make the store
	// slower, it makes every write fail.
	tmp string
}

// NewLocal opens (and creates) the store rooted at root.
func NewLocal(root string) (*Local, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve store root %s: %w", root, err)
	}
	l := &Local{root: abs, dir: filepath.Join(abs, "cas"), tmp: filepath.Join(abs, "tmp")}
	for _, d := range []string{l.dir, l.tmp} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, DiskError("create store directory", d, err)
		}
	}
	return l, nil
}

// Root is the directory the store owns. The appliance's data directory.
func (l *Local) Root() string { return l.root }

// ValidateDigest refuses anything that is not 64 lower-hex characters. Uppercase is
// refused rather than folded: two spellings of one address is how a store ends up holding
// the same bytes twice and reporting a cache miss on a hit.
func ValidateDigest(digest string) error {
	if len(digest) != 64 {
		return fmt.Errorf("%w: %q is %d characters, want 64 lower-hex", ErrBadDigest, digest, len(digest))
	}
	for i := 0; i < len(digest); i++ {
		c := digest[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return fmt.Errorf("%w: %q is not lower-hex at offset %d", ErrBadDigest, digest, i)
		}
	}
	return nil
}

// Path is where digest lives on disk. It validates first, so no caller-supplied string can
// address anything outside the store.
func (l *Local) Path(digest string) (string, error) {
	if err := ValidateDigest(digest); err != nil {
		return "", err
	}
	return filepath.Join(l.root, filepath.FromSlash(RelKey(digest))), nil
}

// Has answers whether the address is taken. It does NOT re-hash: verifying what is already
// stored on every lookup would make a hit cost a full read. Verify is the call that answers
// the stronger question, and hydration asks it on the read path — where the bytes are being
// consumed anyway, so the hash rides along with a pass that had to happen.
func (l *Local) Has(digest string) (bool, error) {
	p, err := l.Path(digest)
	if err != nil {
		return false, err
	}
	st, err := os.Stat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("stat object %s: %w", p, err)
	}
	return st.Mode().IsRegular(), nil
}

// Put streams r into the store and returns the digest of what arrived. source names where
// the bytes came from (a URL, a path) so that a failure can say so.
func (l *Local) Put(r io.Reader, source string) (digest string, size int64, err error) {
	return l.put(r, "", source)
}

// PutExpecting streams r into the store and refuses to store anything whose digest is not
// want. This is the fetch path's guard: the bytes are hashed as they are written, and a
// mismatch discards the temporary file rather than publishing it under any address at all.
// A wrong artifact never becomes a stored artifact, so it can never be materialized, so
// there is no window in which something could exec it.
//
// An empty want is a REFUSAL, not "no expectation". Put is the call that means no
// expectation; a caller that reaches here with an unset pin has a bug, and treating the
// empty string as a wildcard would turn that bug into an unverified install.
func (l *Local) PutExpecting(r io.Reader, want, source string) (size int64, err error) {
	if err := ValidateDigest(want); err != nil {
		return 0, err
	}
	_, size, err = l.put(r, want, source)
	return size, err
}

func (l *Local) put(r io.Reader, want, source string) (string, int64, error) {
	if want != "" {
		if err := ValidateDigest(want); err != nil {
			return "", 0, err
		}
	}
	if source == "" {
		source = "the supplied stream"
	}

	tmp, err := os.CreateTemp(l.tmp, "put-")
	if err != nil {
		return "", 0, DiskError("create temporary object in", l.tmp, err)
	}
	name := tmp.Name()
	// Unconditionally: on failure the temporary is garbage, and on success it is a SECOND
	// name for an inode that already has the one it will be found by.
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(name)
	}()

	// io.Copy returns one error for two very different failures — the network dropped, or
	// the disk filled. Tagging the source side is what lets the message say which.
	src := &taggedReader{r: r}
	h := sha256.New()
	size, copyErr := io.Copy(tmp, io.TeeReader(src, h))
	if copyErr != nil {
		if src.err != nil {
			return "", 0, fmt.Errorf("read %s after %d bytes: %w", source, size, src.err)
		}
		return "", 0, DiskError("write object to", name, copyErr)
	}
	if err := tmp.Sync(); err != nil {
		return "", 0, DiskError("flush object to", name, err)
	}
	// 0444, and this is load-bearing rather than tidy. A Shared materialization HARDLINKS
	// the stored object — one inode, two names — so the mode on the working copy IS the
	// mode on the store. Read-only is what makes that rung safe to offer at all.
	if err := tmp.Chmod(0o444); err != nil {
		return "", 0, fmt.Errorf("seal object %s read-only: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return "", 0, DiskError("close object", name, err)
	}

	digest := hex.EncodeToString(h.Sum(nil))
	if want != "" && digest != want {
		return "", 0, &DigestMismatch{Source: source, Want: want, Got: digest, Size: size}
	}

	dest := filepath.Join(l.root, filepath.FromSlash(RelKey(digest)))
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", 0, DiskError("create object directory", filepath.Dir(dest), err)
	}

	// PUBLISH BY LINK, AND THAT IS THE WHOLE CONCURRENCY DESIGN.
	//
	// os.Link is one atomic syscall that fails with EEXIST when the address is already
	// taken, which is exactly store-if-absent. Two `kontra up` invocations racing the same
	// first-run hydration each write their own temporary and exactly one wins the link; the
	// loser's bytes were identical by construction, because the address is the hash. The
	// store is therefore never half-written under an address, only half-written under a
	// temporary name nobody reads.
	//
	// There is no lock file, deliberately. A lock needs a liveness story — who clears it
	// after a SIGKILL, and how does the clearer know the holder is dead — and getting that
	// wrong turns a slow first run into a permanently wedged one. An atomic publish has no
	// such story to get wrong. The price is that a cold-start race fetches the bytes twice;
	// correctness is not something to buy with a lock when it is already free.
	if err := os.Link(name, dest); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return "", 0, DiskError("publish object to", dest, err)
		}
		// EEXIST is the expected outcome of losing the race, and it is also what a
		// DIRECTORY sitting on the address looks like. Treating the two the same would
		// report a successful store for bytes that are not retrievable, and the complaint
		// would surface much later somewhere that cannot explain it.
		if st, serr := os.Stat(dest); serr != nil || !st.Mode().IsRegular() {
			return "", 0, fmt.Errorf("object address %s is occupied by something that is not a stored object: %w", dest, err)
		}
	}
	// The object's own bytes are fsync'd above; this makes its NAME durable too, so a power
	// loss cannot leave an object that exists but cannot be found.
	syncDir(filepath.Dir(dest))
	return digest, size, nil
}

// Open returns the stored object for streaming. It does not verify — a 54 MB runtime should
// not be hashed twice on every start — so callers that must be certain hash as they read.
func (l *Local) Open(digest string) (*os.File, error) {
	p, err := l.Path(digest)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrMissing, p)
	}
	if err != nil {
		return nil, fmt.Errorf("open object %s: %w", p, err)
	}
	return f, nil
}

// Verify re-hashes a stored object and answers whether it is still the bytes its address
// names. It streams, so a 450 MB artifact costs a read and not a heap.
//
// THIS IS THE READ SIDE OF THE PROTOCOL AND IT IS NOT FREE, which is why Has does not do it
// and why nothing calls this on a hot path. Put proves what ENTERS the store; only a read
// can prove what is still there afterwards, and "afterwards" is where a truncating disk, a
// well-meaning `rsync` of the data directory and a filesystem repair all live.
//
// A mismatch is *ObjectMutated, which is a different fact from *DigestMismatch: the pin is
// not in question here — these bytes hashed to this digest when they were published under
// it, so a mismatch now means the disk changed, and the answer is to discard and re-fetch.
func (l *Local) Verify(digest string) error {
	p, err := l.Path(digest)
	if err != nil {
		return err
	}
	f, err := os.Open(p)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: %s", ErrMissing, p)
	}
	if err != nil {
		return fmt.Errorf("open object %s: %w", p, err)
	}
	defer f.Close()

	h := sha256.New()
	size, err := io.Copy(h, f)
	if err != nil {
		return fmt.Errorf("read object %s after %d bytes: %w", p, size, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != digest {
		return &ObjectMutated{Path: p, Want: digest, Got: got, Size: size}
	}
	return nil
}

// Discard removes an object from the store, so that the next Has is a miss and the next
// fetch actually fetches. It is the recovery half of Verify: a mutated object is not
// something to warn about, it is something to throw away.
//
// Removing the NAME is enough and is atomic. A process already reading the old inode keeps
// reading it — which is right, because tearing a read out from under a caller would turn a
// detected problem into a second, worse one — and the address is free the instant the
// unlink returns.
func (l *Local) Discard(digest string) error {
	p, err := l.Path(digest)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("discard object %s: %w", p, err)
	}
	return nil
}

// ReadVerified reads an object whole and re-checks its digest, with the same "noun" shaping
// as CAS.GetVerified so that both backings of the protocol produce the same error surface.
// For SMALL objects — a manifest, a checksum list. Artifacts stream through Open.
func (l *Local) ReadVerified(digest, noun string) ([]byte, error) {
	p, err := l.Path(digest)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%s object missing: %s", noun, p)
	}
	if err != nil {
		return nil, fmt.Errorf("read %s object %s: %w", noun, p, err)
	}
	if Sha256Hex(data) != digest {
		return nil, fmt.Errorf("%s integrity check failed for %s", noun, p)
	}
	return data, nil
}

// Materialize puts the stored object at dest as a working copy, by the cheapest rung mode
// and the filesystem allow. See CopyOnWrite — and the package header, which says what a
// working copy is not.
func (l *Local) Materialize(digest, dest string, mode Mode) (Method, error) {
	src, err := l.Path(digest)
	if err != nil {
		return "", err
	}
	has, err := l.Has(digest)
	if err != nil {
		return "", err
	}
	if !has {
		return "", fmt.Errorf("%w: cannot materialize sha256:%s at %s", ErrMissing, digest, dest)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", DiskError("create working directory", filepath.Dir(dest), err)
	}
	return CopyOnWrite(src, dest, mode)
}

// taggedReader remembers whether the SOURCE failed, so that io.Copy's single error can be
// attributed to the side that produced it.
type taggedReader struct {
	r   io.Reader
	err error
}

func (t *taggedReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		t.err = err
	}
	return n, err
}

func syncDir(dir string) {
	f, err := os.Open(dir)
	if err != nil {
		return
	}
	// Best effort: some filesystems refuse fsync on a directory, and a store that works
	// is worth more than one that refuses to start on such a filesystem.
	_ = f.Sync()
	_ = f.Close()
}
