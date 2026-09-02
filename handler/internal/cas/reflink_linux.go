//go:build linux

package cas

import (
	"os"

	"golang.org/x/sys/unix"
)

// reflinkFile clones src to dst with FICLONE: the destination is a separate inode that
// shares src's extents until one of them is written, which is exactly copy-on-write. btrfs
// and XFS with reflink=1 support it; ext4 and tmpfs do not, and say so with EOPNOTSUPP.
//
// dst must not exist. Every failure that is not a full disk becomes errNoReflink, including
// the ones that are really "the directory is not writable" — the copy rung is about to
// attempt the same thing and will report that properly, and a store that refused to install
// on ext4 because it could not distinguish the two would be worse than useless.
//
// golang.org/x/sys/unix rather than a hand-rolled ioctl number: FICLONE's encoding differs
// between architectures, and this module already carries x/sys.
func reflinkFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return errNoReflink
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if isNoSpace(err) {
			return DiskError("create reflink target", dst, err)
		}
		return errNoReflink
	}
	if err := unix.IoctlFileClone(int(out.Fd()), int(in.Fd())); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		if isNoSpace(err) {
			return DiskError("reflink to", dst, err)
		}
		return errNoReflink
	}
	if err := out.Close(); err != nil {
		return DiskError("close reflink target", dst, err)
	}
	return nil
}
