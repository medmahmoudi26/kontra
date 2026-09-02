package cas

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
)

// COPY-ON-WRITE IS NOT A SECURITY BOUNDARY. It is stated in the package header and repeated
// here because this file is where somebody will land when they are looking for a cheap way
// to give untrusted code a filesystem. This mechanism deduplicates bytes and removes a copy
// from a cold start. It shares a kernel, a page cache, a network namespace and a filesystem
// view with the process that made it, and it guarantees exactly one thing: a write through
// the working copy does not reach the stored object. Isolation is the container runtime's
// job (ADR 0031 §2, locked decision 3).

// Mode is the caller's promise about the working copy, and it decides whether hardlinking
// is on the table.
//
// It has to be a promise, because the filesystem cannot answer it for us. A hardlink IS the
// stored object — one inode, two names — so an in-place write through the working copy
// writes the store. Tools that replace by rename would be perfectly safe; tools that open
// and append would not, and the store does not get to choose which tool the caller runs.
// The read-only 0444 the store seals its objects with turns that from silent corruption
// into EACCES, but a promise the caller can keep is better than an error it has to handle.
//
// AND THE 0444 IS A BACKSTOP, NOT AN ENFORCEMENT. uid 0 ignores it: as root — which is how
// this repo's controllers, its droplets and most of its containers run — an in-place write
// through a hardlinked Shared working copy succeeds and lands in the store. The contract is
// therefore the caller's promise, and the mode bit only catches the unprivileged case.
// Anything that might be written to must ask for Writable and mean it.
type Mode int

const (
	// Writable: the caller may write to the working copy. reflink or a copy — never a
	// hardlink, whatever the filesystem would happily allow.
	Writable Mode = iota
	// Shared: the caller promises never to write to the working copy in place. This is the
	// hydrated-runtime case — a Node tarball is read and exec'd, never edited — and it is
	// what makes the second install of the same artifact cost no bytes on a filesystem
	// without reflink.
	Shared
)

func (m Mode) String() string {
	if m == Shared {
		return "shared"
	}
	return "writable"
}

// Method is the rung actually used. It is returned rather than logged internally because
// "did this machine give us copy-on-write or a 120 MB copy" is a question about the
// operator's filesystem that only the caller can put in front of them.
type Method string

const (
	MethodReflink  Method = "reflink"
	MethodHardlink Method = "hardlink"
	MethodCopy     Method = "copy"
)

// rank orders the rungs cheapest-first, so a caller materializing a tree can report the
// WEAKEST rung it actually needed rather than the first one it happened to hit.
func (m Method) rank() int {
	switch m {
	case MethodReflink:
		return 0
	case MethodHardlink:
		return 1
	default:
		return 2
	}
}

// Weakest returns whichever of a and b is the more expensive rung, treating the empty
// Method as "nothing materialized yet".
func Weakest(a, b Method) Method {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	if b.rank() > a.rank() {
		return b
	}
	return a
}

// errNoReflink means "this filesystem or this platform does not do extent sharing" — a
// reason to try the next rung, never a reason to fail.
var errNoReflink = errors.New("reflink unsupported here")

// CopyOnWrite materializes src at dst by the cheapest rung the filesystem and mode allow:
// reflink (btrfs, XFS with reflink=1, APFS) -> hardlink (Shared only) -> a plain copy.
//
// A rung that is merely unsupported falls through to the next one. A rung that failed
// because the disk is full does NOT fall through, because the next rung wants the same
// bytes and would only produce a second, vaguer error — the first one already names the
// disk, and that is the one worth keeping.
//
// dst is published by rename from a sibling temporary, so a process killed mid-copy leaves
// a leftover temporary rather than a truncated file under the name callers use.
func CopyOnWrite(src, dst string, mode Mode) (Method, error) {
	info, err := os.Stat(src)
	if err != nil {
		return "", fmt.Errorf("stat copy-on-write source %s: %w", src, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("copy-on-write source %s is %s, not a regular file", src, info.Mode().Type())
	}

	tmp, err := tempName(dst)
	if err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(tmp) }()

	switch err := reflinkFile(src, tmp); {
	case err == nil:
		if err := publish(tmp, dst, workingPerm(info.Mode().Perm(), mode)); err != nil {
			return "", err
		}
		return MethodReflink, nil
	case !errors.Is(err, errNoReflink):
		return "", err // a full disk, and the copy below would only say it worse
	}

	if mode == Shared {
		switch err := os.Link(src, tmp); {
		case err == nil:
			// NO chmod here. The hardlink shares the stored object's inode, so a chmod on
			// the working copy is a chmod on the store — which would undo the 0444 that
			// makes this rung safe in the first place.
			if err := renameOnto(tmp, dst); err != nil {
				return "", err
			}
			return MethodHardlink, nil
		case isNoSpace(err):
			return "", DiskError("hardlink working copy to", tmp, err)
		}
		// EXDEV (a different filesystem), EMLINK (link count exhausted), EPERM (a
		// filesystem or a sandbox that refuses): all mean "copy instead", not "fail".
	}

	if err := copyFile(src, tmp); err != nil {
		return "", err
	}
	if err := publish(tmp, dst, workingPerm(info.Mode().Perm(), mode)); err != nil {
		return "", err
	}
	return MethodCopy, nil
}

// workingPerm derives the working copy's mode from the stored object's. Writable adds owner
// write to a 0444 object; Shared strips every write bit, so that a caller who breaks its
// promise gets EACCES instead of a corrupted store. The execute bits are carried through
// untouched — an artifact's binaries have to stay runnable.
func workingPerm(srcPerm fs.FileMode, mode Mode) fs.FileMode {
	if mode == Writable {
		return srcPerm | 0o200
	}
	return srcPerm &^ 0o222
}

func publish(tmp, dst string, perm fs.FileMode) error {
	if err := os.Chmod(tmp, perm); err != nil {
		return fmt.Errorf("set working copy mode on %s: %w", tmp, err)
	}
	return renameOnto(tmp, dst)
}

func renameOnto(tmp, dst string) error {
	if err := os.Rename(tmp, dst); err != nil {
		return DiskError("publish working copy to", dst, err)
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open copy-on-write source %s: %w", src, err)
	}
	defer in.Close()
	// O_EXCL: tempName produced a name nothing should hold. If something does, that is a
	// collision worth failing on, not one to overwrite.
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return DiskError("create working copy", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return DiskError("copy into", dst, err)
	}
	// Deliberately NOT fsync'd. A working copy is derived state that hydration can rebuild
	// from the store, and one fsync per file turns a tree of thirty thousand files into a
	// minutes-long install for durability nobody needs.
	if err := out.Close(); err != nil {
		return DiskError("close working copy", dst, err)
	}
	return nil
}

// tempName produces a sibling of dst that does not exist. A sibling and not a temp
// directory, so the publishing rename is within one filesystem and therefore atomic.
func tempName(dst string) (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate a temporary name beside %s: %w", dst, err)
	}
	return dst + ".cow-" + hex.EncodeToString(b[:]), nil
}
