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
	"runtime"
	"strconv"
	"strings"
)

// Other local users (spec/tunnel.md §9). The Host and Origin checks stop a browser; they do not stop another account
// on the same machine, which can connect to 127.0.0.1:7777 as well and would be served as the node's principal, with
// the node's per-job keys opening sealed answers for it. So on a loopback --listen without --local-token, the node
// accepts a connection only from a process of its own user: on Linux it reads the client socket's owner from the
// kernel's socket table (/proc/self/net/tcp and tcp6: the entry whose local address is the connection's remote one and
// whose remote address is its local one), and closes a connection it cannot attribute to its own uid. Every
// connection is checked, whatever its source address: a local process may bind any of the machine's addresses and
// connect to 127.0.0.1 from it (round 2 other-fixes-1). Only an entry of a socket some process still holds (a non-zero
// inode) is attributed: the kernel lists a socket whose owner closed it (orphaned, TIME_WAIT) with uid 0, and the
// overflow uid stands for every user a user namespace does not map (other-fixes-3). On Linux a table that cannot be
// read closes the connection, and the node says so once (other-fixes-4); on systems without that table the check is
// not made, and a machine shared with other users needs --local-token.

// errNoPeerTable is a system without the socket table (not Linux): the peer's user cannot be read.
var errNoPeerTable = errors.New("hiring: no socket table to read a connection's user from")

// errUnattributable is a socket table entry that names no owner: a socket no process holds, or the overflow uid.
var errUnattributable = errors.New("hiring: the connection's client socket has no owner the node can read")

// peerUIDFunc reads the uid owning the client end of a TCP connection (tests replace it).
var peerUIDFunc = peerUID

// openProc opens a socket table.
var openProc = func(p string) (io.ReadCloser, error) { return os.Open(p) }

// goos is runtime.GOOS (tests replace it).
var goos = runtime.GOOS

// procTables are the socket tables read: the process's own network namespace's (/proc/net is a link to
// /proc/self/net, hidden when procfs is mounted with subset=pid, as systemd's ProcSubset=pid does).
var procTables = []string{"/proc/self/net/tcp", "/proc/self/net/tcp6"}

// overflowUID is the kernel's overflow uid (/proc/sys/kernel/overflowuid, 65534 by default): what a user namespace
// lists for every uid it does not map, so that it attributes a connection to no one.
var overflowUID = func() int {
	b, err := os.ReadFile("/proc/sys/kernel/overflowuid")
	if err != nil {
		return 65534
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 65534
	}
	return n
}()

// peerCheckWorks reports whether the socket table can be read here: false on Linux when it cannot (every connection
// is then closed), true where there is no such table (the check is not made).
func peerCheckWorks() bool {
	if goos != "linux" {
		return true
	}
	for _, f := range procTables {
		if r, err := openProc(f); err == nil {
			_ = r.Close()
			return true
		}
	}
	return false
}

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
		if !lok || !rok {
			return c, nil // not a TCP connection
		}
		// Whatever the client's source address: the listener is a loopback one, so every client is a local process,
		// which may have bound any address of the machine.
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
	if goos != "linux" {
		return 0, errNoPeerTable
	}
	seen := false
	for _, f := range procTables {
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
			if uid == overflowUID {
				return 0, errUnattributable
			}
			return uid, nil
		}
	}
	if !seen {
		// Linux always has the table: one the node cannot read (procfs hidden, say) attributes nothing.
		return 0, errors.New("hiring: the socket table cannot be read: every connection is closed (use --local-token)")
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
		if len(f) < 10 {
			continue
		}
		la, err1 := procAddr(f[1])
		ra, err2 := procAddr(f[2])
		if err1 != nil || err2 != nil || la != cl || ra != sv {
			continue
		}
		// A socket no process holds any more (its owner closed it: orphaned, or in TIME_WAIT) has inode 0, and the
		// kernel lists it with uid 0 whoever made it: it is no one's.
		if f[9] == "0" {
			return 0, false, errUnattributable
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
