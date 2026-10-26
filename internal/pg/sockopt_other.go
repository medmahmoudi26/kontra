//go:build !linux

package pg

import "syscall"

// setTCPUserTimeout is a no-op where TCP_USER_TIMEOUT does not exist; keepalives and the server-side
// tcp_user_timeout still bound the connection.
func setTCPUserTimeout(string, string, syscall.RawConn) error { return nil }
