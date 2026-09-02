//go:build darwin

package cas

import "golang.org/x/sys/unix"

// reflinkFile clones src to dst with clonefile(2), APFS's copy-on-write. It creates dst
// itself and fails if dst exists, which is the same contract the linux leg keeps.
//
// CLONE_NOFOLLOW: a symlink in the store would otherwise be resolved, and the store's
// objects are regular files by construction — following one would mean cloning something
// the address does not name.
func reflinkFile(src, dst string) error {
	if err := unix.Clonefile(src, dst, unix.CLONE_NOFOLLOW); err != nil {
		if isNoSpace(err) {
			return DiskError("clonefile to", dst, err)
		}
		return errNoReflink
	}
	return nil
}
