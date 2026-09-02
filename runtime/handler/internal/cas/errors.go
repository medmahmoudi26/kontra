package cas

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrBadDigest is a digest that is not 64 lower-hex characters. It is a refusal and not a
// sanitisation: RelKey slices the string into a path, so "../../etc" would otherwise
// address something outside the store.
var ErrBadDigest = errors.New("not a sha256 digest")

// ErrMissing is "this address holds nothing". Callers that can re-fetch (hydration) branch
// on it; callers that cannot report it.
var ErrMissing = errors.New("object not in store")

// ErrDiskFull is the one write failure the store could not have prevented and the caller
// cannot fix by retrying: the filesystem or the quota is out of room.
//
// It is a NAMED error because this repo has already paid the bill for the alternative. A
// SeaweedFS volume-slot ceiling turned every write into a bare HTTP 500 while the isolation
// counters reported nothing wrong; a root-owned Temporal volume reported "unable to open
// database file: out of memory (14)" on a box with 7.9 GB free. Both cost hours because the
// message named a symptom instead of the disk. A hydration that runs out of disk must say
// out of disk, and must say which filesystem.
var ErrDiskFull = errors.New("out of disk space")

// DiskFullError is ErrDiskFull with the operation, the path and — where the platform will
// tell us — how much room is actually left, because "0 bytes free" and "4 GB free but the
// quota is spent" send an operator to different places.
type DiskFullError struct {
	Op   string
	Path string
	Free int64 // bytes available on the filesystem holding Path; valid iff HaveFree
	// HaveFree distinguishes "we asked and it said zero" from "we could not ask", which is
	// the difference between a diagnosis and a guess.
	HaveFree bool
	Err      error
}

func (e *DiskFullError) Error() string {
	msg := fmt.Sprintf("%s %s: out of disk space", e.Op, e.Path)
	if e.HaveFree {
		msg += fmt.Sprintf(" (%s free on the filesystem holding it)", humanBytes(e.Free))
	}
	return msg + ": " + e.Err.Error()
}

func (e *DiskFullError) Unwrap() error { return e.Err }

// Is makes errors.Is(err, ErrDiskFull) work while Unwrap keeps errors.Is(err, syscall.ENOSPC)
// working too: the sentinel is for callers that only care that the disk is full, the errno
// for callers that care which errno.
func (e *DiskFullError) Is(target error) bool { return target == ErrDiskFull }

// DigestMismatch names BOTH sides. "integrity check failed" tells an operator that
// something is wrong and nothing at all about what — whether the pin in the source tree is
// stale, or the mirror served a different file, or the transfer truncated. Want, Got and
// the byte count separate those three without a second run.
type DigestMismatch struct {
	Source string // where the bytes came from: a URL, a path, a key
	Want   string
	Got    string
	Size   int64
}

func (e *DigestMismatch) Error() string {
	return fmt.Sprintf("digest mismatch for %s: expected sha256:%s, got sha256:%s (%d bytes read); refusing to store",
		e.Source, e.Want, e.Got, e.Size)
}

// ErrMutated is "the bytes under this address are no longer the bytes this address names".
// It is not a write failure and not a missing object: it is the one that means something
// outside the store changed what the store holds, and the only correct answer to it is to
// throw the object away and fetch it again.
var ErrMutated = errors.New("stored object no longer matches its address")

// ObjectMutated is ErrMutated with both digests and the path, for the same reason
// DigestMismatch names both sides: "integrity check failed" sends an operator looking at
// the pin, and the pin is the one thing here that cannot be wrong — the address WAS this
// digest when the bytes were published under it.
type ObjectMutated struct {
	Path string
	Want string
	Got  string
	Size int64
}

func (e *ObjectMutated) Error() string {
	return fmt.Sprintf("the stored object %s is no longer sha256:%s — it now hashes to sha256:%s over %d bytes; something changed it under the store",
		e.Path, e.Want, e.Got, e.Size)
}

func (e *ObjectMutated) Is(target error) bool { return target == ErrMutated }

// NotEnoughSpace is a disk refusal made BEFORE the write, from a size that is already
// known. It reports as ErrDiskFull because it is the same operator problem with the same
// fix, and it exists separately because this repo has measured what the alternative costs:
// a storage backend answered a bare 500 on every write while its counters reported nothing
// wrong, and a hydration that discovers the disk halfway through has already spent it.
type NotEnoughSpace struct {
	What string // "hydrating node-runtime": the work that will not fit
	Path string
	Need int64
	Free int64
}

func (e *NotEnoughSpace) Error() string {
	return fmt.Sprintf("out of disk space: %s needs about %s and %s has %s free",
		e.What, humanBytes(e.Need), e.Path, humanBytes(e.Free))
}

func (e *NotEnoughSpace) Is(target error) bool { return target == ErrDiskFull }

// RequireSpace refuses work that is already known not to fit. need is the caller's own
// estimate INCLUDING its headroom; this function does not invent one, because only the
// caller knows whether it is about to write a temporary copy of what it is reading.
//
// An unreadable statfs is not a refusal. A store that will not start on a filesystem whose
// statfs the kernel declines to answer is worse than one that lets the write fail with its
// own message.
func RequireSpace(what, path string, need int64) error {
	if need <= 0 {
		return nil
	}
	dir := path
	for {
		if _, err := os.Stat(dir); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil
		}
		dir = parent
	}
	free, ok := freeBytes(dir)
	if !ok || int64(free) >= need {
		return nil
	}
	return &NotEnoughSpace{What: what, Path: dir, Need: need, Free: int64(free)}
}

// DiskError is the ONE place a filesystem write error becomes an error string. Everything
// that writes goes through it, so the ENOSPC classification cannot be forgotten in one
// branch and remembered in another. Exported because hydration writes too — an expansion
// that fills the disk between the first and the last file of an artifact is exactly the
// failure this has to name — and a second classifier in that package would be a second
// place to forget it.
func DiskError(op, path string, err error) error {
	if err == nil {
		return nil
	}
	if isNoSpace(err) {
		e := &DiskFullError{Op: op, Path: path, Err: err}
		// Statfs wants a path that exists. The failed write's target usually does not, so
		// ask its directory.
		dir := path
		if st, serr := os.Stat(dir); serr != nil || !st.IsDir() {
			dir = filepath.Dir(path)
		}
		if free, ok := freeBytes(dir); ok {
			e.Free, e.HaveFree = int64(free), true
		}
		return e
	}
	return fmt.Errorf("%s %s: %w", op, path, err)
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 4; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTP"[exp])
}
