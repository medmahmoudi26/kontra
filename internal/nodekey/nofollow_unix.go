//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly || illumos

package nodekey

import "syscall"

// noFollow makes an open fail on a symbolic link: a per-job key is always a file the node wrote itself.
const noFollow = syscall.O_NOFOLLOW
