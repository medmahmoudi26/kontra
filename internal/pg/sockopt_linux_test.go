package pg_test

import (
	"context"
	"net"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sys/unix"

	"kontra.sh/kontra/internal/pg"
)

// checkClientSocket reads the options pg's dialer set on a pooled connection's TCP socket.
func checkClientSocket(t *testing.T, p *pgxpool.Pool) {
	t.Helper()
	c, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Release()
	tcp, ok := c.Conn().PgConn().Conn().(*net.TCPConn)
	if !ok {
		t.Skipf("not a plain TCP connection (%T): socket options not checked", c.Conn().PgConn().Conn())
	}
	raw, err := tcp.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][2]int{
		"TCP_USER_TIMEOUT": {unix.TCP_USER_TIMEOUT, int(pg.TCPUserTimeout.Milliseconds())},
		"TCP_KEEPIDLE":     {unix.TCP_KEEPIDLE, int(pg.KeepAliveIdle.Seconds())},
		"TCP_KEEPINTVL":    {unix.TCP_KEEPINTVL, int(pg.KeepAliveInterval.Seconds())},
		"TCP_KEEPCNT":      {unix.TCP_KEEPCNT, pg.KeepAliveCount},
	}
	if err := raw.Control(func(fd uintptr) {
		if v, err := unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_KEEPALIVE); err != nil || v != 1 {
			t.Errorf("SO_KEEPALIVE = %d, %v", v, err)
		}
		for name, w := range want {
			v, err := unix.GetsockoptInt(int(fd), unix.IPPROTO_TCP, w[0])
			if err != nil || v != w[1] {
				t.Errorf("%s = %d, %v; want %d", name, v, err, w[1])
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
}
