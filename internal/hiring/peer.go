package hiring

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
)

// Other local users (spec/tunnel.md §9). The Host and Origin checks stop a browser; they do not stop another account
// on the same machine, which can connect to 127.0.0.1:7777 as well and would be served as the node's principal, with
// the node's per-job keys opening sealed answers for it. So on a loopback --listen without --local-token, the node
// accepts a connection only from a process of its own user: on Linux it reads the client socket's owner from the
// kernel's socket table (/proc/net/tcp and /proc/net/tcp6: the entry whose local address is the connection's remote
// one and whose remote address is its local one), and closes a connection it cannot attribute to its own uid. On
// systems without that table the check is not made, and a machine shared with other users needs --local-token.

// errNoPeerTable is a system without /proc/net/tcp: the peer's user cannot be read.
var errNoPeerTable = errors.New("hiring: no socket table to read a connection's user from")

// peerUIDFunc reads the uid owning the client end of a TCP connection (tests replace it).
var peerUIDFunc = peerUID

// openProc opens a socket table.
var openProc = func(p string) (io.ReadCloser, error) { return os.Open(p) }

// peerListener closes every accepted loopback connection whose client is not a process of the node's user.
type peerListener struct {
	net.Listener
	uid int
	log *slog.Logger
}

// Accept returns the next connection of the node's own user.
func (l *peerListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		local, lok := c.LocalAddr().(*net.TCPAddr)
		remote, rok := c.RemoteAddr().(*net.TCPAddr)
		if !lok || !rok || !remote.IP.IsLoopback() {
			return c, nil // not a loopback TCP connection: the listener's own rules apply (--local-token)
		}
		uid, err := peerUIDFunc(local, remote)
		switch {
		case errors.Is(err, errNoPeerTable):
			return c, nil
		case err != nil:
			l.log.Warn("a local connection is closed: its user could not be read (spec/tunnel.md §9)", "err", err)
		case uid != l.uid:
			l.log.Warn("a local connection of another user is closed (spec/tunnel.md §9): use --local-token to serve other users",
				"uid", uid)
		default:
			return c, nil
		}
		_ = c.Close()
	}
}

// peerUID is the uid owning the client end of the loopback connection local <- remote.
func peerUID(local, remote *net.TCPAddr) (int, error) {
	seen := false
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		r, err := openProc(f)
		if err != nil {
			continue
		}
		seen = true
		uid, ok, err := findPeer(r, local, remote)
		_ = r.Close()
		if err != nil {
			return 0, err
		}
		if ok {
			return uid, nil
		}
	}
	if !seen {
		return 0, errNoPeerTable
	}
	return 0, errors.New("hiring: the connection's client socket is not in the socket table")
}

// findPeer reads a /proc/net/tcp table for the socket whose local address is remote and whose remote address is
// local (the client's end of the connection), and returns its uid.
func findPeer(r io.Reader, local, remote *net.TCPAddr) (int, bool, error) {
	want := func(a *net.TCPAddr) (netip.AddrPort, bool) {
		ip, ok := netip.AddrFromSlice(a.IP)
		if !ok {
			return netip.AddrPort{}, false
		}
		return netip.AddrPortFrom(ip.Unmap(), uint16(a.Port)), true
	}
	cl, ok1 := want(remote)
	sv, ok2 := want(local)
	if !ok1 || !ok2 {
		return 0, false, errors.New("hiring: a connection address that is not an IP")
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 4096), 1<<20)
	first := true
	for sc.Scan() {
		if first {
			first = false // the header
			continue
		}
		f := strings.Fields(sc.Text())
		if len(f) < 8 {
			continue
		}
		la, err1 := procAddr(f[1])
		ra, err2 := procAddr(f[2])
		if err1 != nil || err2 != nil || la != cl || ra != sv {
			continue
		}
		uid, err := strconv.Atoi(f[7])
		if err != nil {
			return 0, false, errors.New("hiring: a socket table entry without a uid")
		}
		return uid, true, nil
	}
	return 0, false, sc.Err()
}

// procAddr decodes a socket table address: the IP as 32-bit words in the kernel's byte order (8 or 32 hex digits),
// a colon, the port in 4 hex digits.
func procAddr(s string) (netip.AddrPort, error) {
	h, p, ok := strings.Cut(s, ":")
	if !ok {
		return netip.AddrPort{}, errors.New("no port")
	}
	port, err := strconv.ParseUint(p, 16, 16)
	if err != nil {
		return netip.AddrPort{}, err
	}
	raw, err := hex.DecodeString(h)
	if err != nil || len(raw) != 4 && len(raw) != 16 {
		return netip.AddrPort{}, errors.New("not an address")
	}
	b := make([]byte, len(raw))
	for i := 0; i < len(raw); i += 4 {
		binary.NativeEndian.PutUint32(b[i:], binary.BigEndian.Uint32(raw[i:]))
	}
	ip, _ := netip.AddrFromSlice(b)
	return netip.AddrPortFrom(ip.Unmap(), uint16(port)), nil
}
