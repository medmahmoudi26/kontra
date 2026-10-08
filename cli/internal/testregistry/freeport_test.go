// freeport_test.go — the port a test binds when it has to know the number BEFORE the listener
// exists.
//
// A COPY, DELIBERATELY. Twelve lines of test scaffolding is cheaper than a shared test helper
// package every role has to import, and it is the copy whose drift cannot reach a shipped binary.
//
// The dial-and-close dance is temporalio/cli's and is not superstition: on Linux a port released
// by bind(:0) can be handed to the next bind(:0) within seconds, and closing from the LISTENER's
// side parks it in TIME_WAIT, which stops that while still allowing an explicit bind.
package testregistry

import (
	"fmt"
	"net"
	"runtime"
)

func freePort(ip string) (int, error) {
	l, err := net.Listen("tcp", hostPort(ip, 0))
	if err != nil {
		return 0, fmt.Errorf("no free port on %s: %w", ip, err)
	}
	defer l.Close()
	port := l.Addr().(*net.TCPAddr).Port
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		return port, nil
	}
	conn, err := net.Dial("tcp", hostPort(ip, port))
	if err != nil {
		return 0, fmt.Errorf("no free port on %s: %w", ip, err)
	}
	defer conn.Close()
	accepted, err := l.Accept()
	if err != nil {
		return 0, fmt.Errorf("no free port on %s: %w", ip, err)
	}
	accepted.Close()
	return port, nil
}
