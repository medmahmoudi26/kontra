//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly || illumos)

package nodekey

// noFollow is 0 where the platform has no O_NOFOLLOW; Load then follows a link like any open.
const noFollow = 0
