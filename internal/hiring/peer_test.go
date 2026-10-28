package hiring

import (
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
