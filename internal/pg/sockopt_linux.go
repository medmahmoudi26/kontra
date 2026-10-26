package pg

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// setTCPUserTimeout sets TCP_USER_TIMEOUT on a TCP socket before it connects (Linux): unacknowledged data
// older than TCPUserTimeout closes the connection instead of waiting for the kernel's retransmission limit
// (about 15 minutes).
func setTCPUserTimeout(network, _ string, c syscall.RawConn) error {
	switch network {
	case "tcp", "tcp4", "tcp6":
	default:
		return nil
	}
	var serr error
	if err := c.Control(func(fd uintptr) {
		serr = unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_USER_TIMEOUT, int(TCPUserTimeout.Milliseconds()))
	}); err != nil {
		return err
	}
	return serr
}
