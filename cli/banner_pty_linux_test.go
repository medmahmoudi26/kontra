//go:build linux

// banner_pty_linux_test.go — the other half of the gate: with a REAL terminal on stderr the
// banner still prints. Allocating a pty without a dependency is three Linux ioctls, so this half
// of the pair builds on Linux only; banner_test.go carries the non-tty half everywhere.
package main

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"
	"unsafe"
)

// openPTY allocates a pty pair the way openpty(3) does — /dev/ptmx, unlockpt, ptsname — with no
// cgo and no module outside the stdlib.
func openPTY(t *testing.T) (master, slave *os.File) {
	t.Helper()
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("no pty available here: %v", err)
	}
	t.Cleanup(func() { m.Close() })
	var unlock int32
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, m.Fd(), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); e != 0 {
		t.Fatalf("unlockpt: %v", e)
	}
	var n uint32
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, m.Fd(), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&n))); e != 0 {
		t.Fatalf("ptsname: %v", e)
	}
	s, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open pts: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return m, s
}

// An operator at a terminal still gets the banner — that is the whole point of gating rather
// than deleting it — and stdout stays clean even then.
func TestBannerPrintsWhenStderrIsATerminal(t *testing.T) {
	master, slave := openPTY(t)

	cmd := kontraCmd(t, "mcp")
	cmd.Stdin = strings.NewReader(initializeFrame)
	var stdout bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, slave
	if err := cmd.Run(); err != nil {
		t.Fatalf("kontra mcp on a pty: %v", err)
	}
	slave.Close() // the child's copy went with it; the last one closing ends the master's reads

	var got strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := master.Read(buf)
		got.Write(buf[:n])
		if err != nil {
			break
		}
	}
	// A terminal turns every \n into \r\n on the way out.
	printed := strings.ReplaceAll(got.String(), "\r\n", "\n")
	if !strings.Contains(printed, strings.TrimSpace(banner)) {
		t.Errorf("banner missing from a tty stderr, got %q", printed)
	}
	if !strings.Contains(stdout.String(), `"serverInfo"`) {
		t.Errorf("handshake missing from stdout: %q", stdout.String())
	}
}
