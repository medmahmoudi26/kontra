package hiring

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

// The socket table read: the client's end of 127.0.0.1:40000 -> 127.0.0.1:7777 is the entry whose local address is
// the client's, and the server's own end (uid 0) is not taken for it.
func TestFindPeer(t *testing.T) {
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		t.Skip("the sample table is little-endian")
	}
	const table = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:1E61 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1 1 0 100 0 0 10 0
   1: 0100007F:1E61 0100007F:9C40 01 00000000:00000000 00:00000000 00000000     0        0 2 1 0 20 4 30 10 -1
   2: 0100007F:9C40 0100007F:1E61 01 00000000:00000000 00:00000000 00000000  1000        0 3 1 0 20 4 30 10 -1
`
	local := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 7777}
	remote := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 40000}
	uid, ok, err := findPeer(strings.NewReader(table), local, remote)
	if err != nil || !ok || uid != 1000 {
		t.Fatalf("uid %d ok %v err %v, want 1000", uid, ok, err)
	}
	// IPv6 loopback, and an IPv4-mapped server address.
	const table6 = `  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000000000000000000001000000:9C40 00000000000000000000000001000000:1E61 01 00000000:00000000 00:00000000 00000000  1001        0 4 1 0 20 4 30 10 -1
`
	uid, ok, err = findPeer(strings.NewReader(table6), &net.TCPAddr{IP: net.IPv6loopback, Port: 7777}, &net.TCPAddr{IP: net.IPv6loopback, Port: 40000})
	if err != nil || !ok || uid != 1001 {
		t.Fatalf("IPv6: uid %d ok %v err %v", uid, ok, err)
	}
	if _, ok, _ := findPeer(strings.NewReader(table), local, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 40001}); ok {
		t.Fatal("a connection that is not in the table was attributed")
	}
	// Round 2 other-fixes-3: a client socket its owner closed (orphaned, FIN_WAIT1 here, inode 0) is listed with uid
	// 0 whoever made it: it is attributed to no one, not to root.
	const orphan = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   1: 0100007F:1E61 0100007F:9C41 01 00000000:00000000 00:00000000 00000000     0        0 9 1 0 20 4 30 10 -1
   2: 0100007F:9C41 0100007F:1E61 05 00000000:00000000 00:00000000 00000000     0        0 0 1 0 20 4 30 10 -1
`
	if uid, ok, err := findPeer(strings.NewReader(orphan), local, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 40001}); ok || err == nil {
		t.Fatalf("an orphaned client socket was attributed to uid %d (ok %v, err %v)", uid, ok, err)
	}
	// A client bound to a non-loopback address of the machine (curl --interface): its entry is found all the same.
	const bound = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   2: 02007C0A:9C42 0100007F:1E61 01 00000000:00000000 00:00000000 00000000 65533        0 7 1 0 20 4 30 10 -1
`
	if uid, ok, err := findPeer(strings.NewReader(bound), local, &net.TCPAddr{IP: net.IPv4(10, 124, 0, 2), Port: 40002}); err != nil || !ok || uid != 65533 {
		t.Fatalf("a client bound to 10.124.0.2: uid %d ok %v err %v", uid, ok, err)
	}
}

// fakeConn is a connection with TCP addresses, recording its Close.
type fakeConn struct {
	net.Conn
	local, remote *net.TCPAddr
	closed        bool
}

func (c *fakeConn) LocalAddr() net.Addr  { return c.local }
func (c *fakeConn) RemoteAddr() net.Addr { return c.remote }
func (c *fakeConn) Close() error         { c.closed = true; return nil }

// fakeListener hands out its connections, then fails.
type fakeListener struct {
	conns []*fakeConn
}

func (l *fakeListener) Accept() (net.Conn, error) {
	if len(l.conns) == 0 {
		return nil, net.ErrClosed
	}
	c := l.conns[0]
	l.conns = l.conns[1:]
	return c, nil
}
func (l *fakeListener) Close() error   { return nil }
func (l *fakeListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 7777} }

// Round 2 records-5 and other-fixes-1: peerListener's refusals, without root. Every connection is checked, whatever
// its source address (a local process may bind any address of the machine and connect to 127.0.0.1 from it): one of
// another user, or one whose user cannot be read, is closed; one of the node's user is served; errNoPeerTable (no
// socket table: not Linux) lets it through.
func TestPeerListenerAccept(t *testing.T) {
	server := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 7777}
	sources := []*net.TCPAddr{{IP: net.IPv4(127, 0, 0, 1), Port: 40000}, {IP: net.IPv4(10, 124, 0, 2), Port: 40001},
		{IP: net.ParseIP("2604:a880:4:1d0::3d0d:4000"), Port: 40002}, {IP: net.IPv4(172, 17, 0, 1), Port: 40003}}
	defer func(f func(local, remote *net.TCPAddr) (int, error)) { peerUIDFunc = f }(peerUIDFunc)
	for _, c := range []struct {
		name   string
		uid    int
		err    error
		served bool
	}{
		{"the node's user", 1000, nil, true},
		{"another user", 65533, nil, false},
		{"an unreadable user", 0, errors.New("not in the table"), false},
		{"no socket table", 0, errNoPeerTable, true},
	} {
		for _, src := range sources {
			asked := false
			peerUIDFunc = func(local, remote *net.TCPAddr) (int, error) {
				asked = true
				if !remote.IP.Equal(src.IP) || remote.Port != src.Port || !local.IP.Equal(server.IP) {
					t.Fatalf("asked about %v -> %v", remote, local)
				}
				return c.uid, c.err
			}
			conn := &fakeConn{local: server, remote: src}
			l := &peerListener{Listener: &fakeListener{conns: []*fakeConn{conn}}, uid: 1000, log: slog.New(slog.DiscardHandler)}
			got, err := l.Accept()
			switch {
			case !asked:
				t.Errorf("%s from %v: the connection's user was not checked", c.name, src)
			case c.served && (err != nil || got != conn || conn.closed):
				t.Errorf("%s from %v: not served (%v, closed %v)", c.name, src, err, conn.closed)
			case !c.served && (got != nil || !conn.closed):
				t.Errorf("%s from %v: served", c.name, src)
			}
		}
	}
}

// Round 2 other-fixes-4: the tables are read from /proc/self/net (procfs mounted with subset=pid hides /proc/net),
// and on Linux a table that cannot be read closes the connection rather than letting it through; elsewhere there is
// no table and the check is not made. The overflow uid (a user a user namespace does not map) attributes nothing.
func TestPeerUIDTables(t *testing.T) {
	defer func(o func(string) (io.ReadCloser, error), g string) { openProc, goos = o, g }(openProc, goos)
	local := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 7777}
	remote := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 40000}
	var opened []string
	openProc = func(p string) (io.ReadCloser, error) {
		opened = append(opened, p)
		return nil, os.ErrNotExist
	}
	goos = "linux"
	if _, err := peerUID(local, remote); err == nil || errors.Is(err, errNoPeerTable) {
		t.Fatalf("an unreadable table on Linux: %v, want an error that closes the connection", err)
	}
	if peerCheckWorks() {
		t.Fatal("peerCheckWorks with no readable table")
	}
	for _, p := range opened {
		if !strings.HasPrefix(p, "/proc/self/net/") {
			t.Fatalf("read %s, want /proc/self/net/...", p)
		}
	}
	goos = "darwin"
	if _, err := peerUID(local, remote); !errors.Is(err, errNoPeerTable) {
		t.Fatalf("no table off Linux: %v", err)
	}
	goos = "linux"
	table := fmt.Sprintf(`  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   2: 0100007F:9C40 0100007F:1E61 01 00000000:00000000 00:00000000 00000000 %5d        0 3 1 0 20 4 30 10 -1
`, overflowUID)
	openProc = func(p string) (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(table)), nil }
	if uid, err := peerUID(local, remote); err == nil {
		t.Fatalf("the overflow uid was attributed: %d", uid)
	}
}

// Finding security-7: another account on the machine could use 127.0.0.1:7777 as the node's principal (and read
// sealed answers its per-job keys open). The hiring server serves only the node's own user on a loopback --listen
// without --local-token now: a request from another uid never reaches the handler.
func TestHiringRefusesOtherLocalUsers(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the peer's user is read from Linux's socket table")
	}
	ts := startServer(t, nil)
	// The node's own user is served (the Host check answers, so the connection was accepted).
	res, _ := ts.do(t, http.MethodGet, "/nothing", "", nil, "")
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("the node's own user: %d", res.StatusCode)
	}
	if os.Getuid() != 0 {
		t.Skip("another user's connection needs root to make")
	}
	curl, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("no curl to connect as another user")
	}
	cmd := exec.Command(curl, "-sS", "-o", "/dev/null", "-w", "%{http_code}", "-H", "Host: 127.0.0.1:"+ts.port, ts.url+"/nothing")
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 65534, Gid: 65534}}
	cmd.Dir = "/"
	out, err := cmd.CombinedOutput()
	if err == nil || strings.HasPrefix(string(out), "404") {
		t.Fatalf("another user's request was served: %v %s", err, out)
	}
	// Round 2 other-fixes-1: from a non-loopback source address of the machine (curl --interface), IPv4 and IPv6.
	addrs, _ := net.InterfaceAddrs()
	tried := map[bool]bool{}
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok || ipn.IP.IsLoopback() || ipn.IP.IsLinkLocalUnicast() || tried[ipn.IP.To4() != nil] {
			continue
		}
		v4 := ipn.IP.To4() != nil
		srv := ts
		if !v4 {
			srv = startServerOn(t, "[::1]:0")
		}
		cmd := exec.Command(curl, "-sS", "-o", "/dev/null", "-w", "%{http_code}", "--interface", ipn.IP.String(),
			"-H", "Host: "+srv.host, srv.url+"/nothing")
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 65534, Gid: 65534}}
		cmd.Dir = "/"
		out, err := cmd.CombinedOutput()
		if err == nil || strings.HasPrefix(string(out), "404") {
			t.Fatalf("another user's request from %s was served: %v %s", ipn.IP, err, out)
		}
		tried[v4] = true
	}
	if !tried[true] {
		t.Log("no non-loopback IPv4 address to connect from")
	}
	// With --local-token every user may connect, and the token authenticates.
	tok := startServer(t, func(c *Config) { c.LocalToken = "0123456789abcdef0123" })
	cmd = exec.Command(curl, "-sS", "-o", "/dev/null", "-w", "%{http_code}", "-H", "Host: 127.0.0.1:"+tok.port,
		"-H", "Authorization: Bearer 0123456789abcdef0123", tok.url+"/nothing")
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 65534, Gid: 65534}}
	cmd.Dir = "/"
	if out, err := cmd.CombinedOutput(); err != nil || string(out) != "404" {
		t.Fatalf("another user with the local token: %v %s", err, out)
	}
}
