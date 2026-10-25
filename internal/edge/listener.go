package edge

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/medmahmoudi26/kontra/internal/config"
)

// capListener enforces the connection caps of spec §2.2 and D73 and reads PROXY protocol headers (D69).
//
// limits.max_conns bounds every open connection; limits.conns_per_ip bounds the open connections of one
// client address. With edge.client_ip.from remote that is the TCP peer. With x_forwarded_for, connections
// from a trusted proxy are exempt from conns_per_ip (every one carries the proxy's address). With
// proxy_protocol, a connection from a trusted peer is counted under the source address of its PROXY
// header, read on the connection's first use, never in Accept (a slow peer would stall every other).
// A connection over a cap is closed at once, before any byte is read. An IPv6 client address is counted by
// its /64 (countKey).
type capListener struct {
	net.Listener
	maxConns      int
	perIP         int
	ci            ClientIP
	headerTimeout time.Duration
	log           *slog.Logger

	mu     sync.Mutex
	total  int
	byAddr map[netip.Addr]int // by countKey
}

func newCapListener(ln net.Listener, l Limits, ci ClientIP, log *slog.Logger) *capListener {
	return &capListener{
		Listener:      ln,
		maxConns:      l.MaxConns,
		perIP:         l.ConnsPerIP,
		ci:            ci,
		headerTimeout: l.HeaderTimeout,
		log:           log,
		byAddr:        map[netip.Addr]int{},
	}
}

// Accept returns the next connection within the caps; connections over a cap are closed and skipped.
func (l *capListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		peer := addrOf(c.RemoteAddr())
		trusted := l.ci.Trusts(peer)
		proxied := trusted && l.ci.From == config.ClientIPProxyProtocol
		// A trusted proxy's own connections are exempt (x_forwarded_for); a PROXY connection is counted
		// under its header's address once that is read.
		countNow := !trusted
		if !l.admit(peer, countNow) {
			l.log.Debug("edge: connection refused at the connection cap", "peer", peer.String())
			_ = c.Close()
			continue
		}
		cc := &capConn{Conn: c, l: l}
		if countNow {
			cc.counted, cc.addr = true, peer
		}
		if proxied {
			cc.proxy = true
		}
		return cc, nil
	}
}

// admit takes a slot in max_conns and, when count is set, one of addr's conns_per_ip slots.
func (l *capListener) admit(addr netip.Addr, count bool) bool {
	k := countKey(addr)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.total >= l.maxConns {
		return false
	}
	if count && l.byAddr[k] >= l.perIP {
		return false
	}
	l.total++
	if count {
		l.byAddr[k]++
	}
	return true
}

// countAddr takes one of addr's conns_per_ip slots for a connection already holding a max_conns slot.
func (l *capListener) countAddr(addr netip.Addr) bool {
	k := countKey(addr)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.byAddr[k] >= l.perIP {
		return false
	}
	l.byAddr[k]++
	return true
}

// release gives back a connection's max_conns slot and, when it was counted, its address's slot.
func (l *capListener) release(addr netip.Addr, counted bool) {
	k := countKey(addr)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.total--
	if counted {
		if l.byAddr[k] <= 1 {
			delete(l.byAddr, k)
		} else {
			l.byAddr[k]--
		}
	}
}

// open is the number of open connections, in all and by countKey (tests).
func (l *capListener) open() (total int, byAddr map[netip.Addr]int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	m := make(map[netip.Addr]int, len(l.byAddr))
	for a, n := range l.byAddr {
		m[a] = n
	}
	return l.total, m
}

// errConnCap is a PROXY connection over conns_per_ip for its header's address.
var errConnCap = errors.New("edge: connection over the per-address cap")

// capConn releases its slots on Close and, on a trusted PROXY connection, reads the header first.
type capConn struct {
	net.Conn
	l         *capListener
	addr      netip.Addr
	counted   bool
	closeOnce sync.Once

	proxy     bool
	parseOnce sync.Once
	br        *bufio.Reader
	remote    net.Addr // the PROXY header's source, when it named one
	perr      error

	dlMu   sync.Mutex
	userRD time.Time // the read deadline the user (the HTTP server) set last
}

func (c *capConn) Close() error {
	c.closeOnce.Do(func() { c.l.release(c.addr, c.counted) })
	return c.Conn.Close()
}

func (c *capConn) Read(b []byte) (int, error) {
	if !c.proxy {
		return c.Conn.Read(b)
	}
	c.parseOnce.Do(c.parse)
	if c.perr != nil {
		return 0, c.perr
	}
	return c.br.Read(b)
}

// RemoteAddr is the client's address: the PROXY header's source on a trusted PROXY connection (the HTTP
// server asks first thing, so the header is read here), else the TCP peer.
func (c *capConn) RemoteAddr() net.Addr {
	if c.proxy {
		c.parseOnce.Do(c.parse)
		if c.remote != nil {
			return c.remote
		}
	}
	return c.Conn.RemoteAddr()
}

func (c *capConn) SetDeadline(t time.Time) error {
	c.dlMu.Lock()
	c.userRD = t
	c.dlMu.Unlock()
	return c.Conn.SetDeadline(t)
}

func (c *capConn) SetReadDeadline(t time.Time) error {
	c.dlMu.Lock()
	c.userRD = t
	c.dlMu.Unlock()
	return c.Conn.SetReadDeadline(t)
}

// NetConn returns the underlying connection.
func (c *capConn) NetConn() net.Conn { return c.Conn }

// parse reads an optional PROXY v1 or v2 header within the header timeout (or the user's earlier read
// deadline) and restores the user's deadline. A trusted peer that sends none (a health check) is served as
// itself; a malformed header closes the connection.
func (c *capConn) parse() {
	c.dlMu.Lock()
	user := c.userRD
	c.dlMu.Unlock()
	dl := time.Now().Add(c.l.headerTimeout)
	if !user.IsZero() && user.Before(dl) {
		dl = user
	}
	_ = c.Conn.SetReadDeadline(dl)
	c.br = bufio.NewReaderSize(c.Conn, proxyMaxHeader)
	src, err := readProxyHeader(c.br)
	_ = c.Conn.SetReadDeadline(user)
	if err != nil {
		c.perr = fmt.Errorf("edge: PROXY header: %w", err)
		_ = c.Conn.Close()
		return
	}
	if !src.IsValid() {
		return // no header, or LOCAL / UNKNOWN: the peer itself, exempt as a trusted proxy
	}
	a := src.Addr().Unmap()
	if !c.l.countAddr(a) {
		c.perr = errConnCap
		_ = c.Conn.Close()
		return
	}
	c.addr, c.counted = a, true
	c.remote = net.TCPAddrFromAddrPort(netip.AddrPortFrom(a, src.Port()))
}

// PROXY protocol (haproxy.org/download/2.9/doc/proxy-protocol.txt).
var proxyV2Sig = []byte("\r\n\r\n\x00\r\nQUIT\n")

const (
	proxyV1Max = 107 // the longest v1 line, CRLF included
	// proxyMaxHeader bounds a v2 header: 16 bytes and up to 4 KiB of addresses and TLVs.
	proxyMaxHeader = 16 + 4096
)

// readProxyHeader reads a PROXY v1 or v2 header if the stream starts with one and returns its source
// address; the zero AddrPort means there was no header, or a LOCAL / UNKNOWN one (use the peer).
func readProxyHeader(br *bufio.Reader) (netip.AddrPort, error) {
	first, err := br.Peek(1)
	if err != nil {
		return netip.AddrPort{}, err
	}
	switch first[0] {
	case 'P':
		b, err := br.Peek(6)
		if err != nil || string(b) != "PROXY " {
			return netip.AddrPort{}, nil // an HTTP method starting with P (POST, PUT, PATCH)
		}
		return readProxyV1(br)
	case proxyV2Sig[0]:
		b, err := br.Peek(len(proxyV2Sig))
		if err != nil || !bytes.Equal(b, proxyV2Sig) {
			return netip.AddrPort{}, nil
		}
		return readProxyV2(br)
	}
	return netip.AddrPort{}, nil
}

func readProxyV1(br *bufio.Reader) (netip.AddrPort, error) {
	var line []byte
	for len(line) < proxyV1Max {
		b, err := br.ReadByte()
		if err != nil {
			return netip.AddrPort{}, err
		}
		line = append(line, b)
		if b == '\n' {
			break
		}
	}
	s, ok := strings.CutSuffix(string(line), "\r\n")
	if !ok {
		return netip.AddrPort{}, errors.New("v1: no CRLF within 107 bytes")
	}
	f := strings.Split(s, " ")
	if len(f) >= 2 && f[1] == "UNKNOWN" {
		return netip.AddrPort{}, nil
	}
	if len(f) != 6 || (f[1] != "TCP4" && f[1] != "TCP6") {
		return netip.AddrPort{}, fmt.Errorf("v1: malformed line %q", s)
	}
	src, err := netip.ParseAddr(f[2])
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("v1: source address: %w", err)
	}
	if _, err := netip.ParseAddr(f[3]); err != nil {
		return netip.AddrPort{}, fmt.Errorf("v1: destination address: %w", err)
	}
	if (f[1] == "TCP4") != src.Is4() {
		return netip.AddrPort{}, fmt.Errorf("v1: %s with address %s", f[1], src)
	}
	port, err := strconv.ParseUint(f[4], 10, 16)
	if err != nil || (len(f[4]) > 1 && f[4][0] == '0') {
		return netip.AddrPort{}, fmt.Errorf("v1: source port %q", f[4])
	}
	return netip.AddrPortFrom(src, uint16(port)), nil
}

func readProxyV2(br *bufio.Reader) (netip.AddrPort, error) {
	var hdr [16]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil {
		return netip.AddrPort{}, err
	}
	if hdr[12]>>4 != 2 {
		return netip.AddrPort{}, fmt.Errorf("v2: version %d", hdr[12]>>4)
	}
	cmd := hdr[12] & 0x0f
	fam := hdr[13]
	n := int(binary.BigEndian.Uint16(hdr[14:16]))
	if 16+n > proxyMaxHeader {
		return netip.AddrPort{}, fmt.Errorf("v2: %d bytes of addresses and TLVs", n)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(br, body); err != nil {
		return netip.AddrPort{}, err
	}
	switch cmd {
	case 0x0: // LOCAL: the proxy's own connection (a health check)
		return netip.AddrPort{}, nil
	case 0x1: // PROXY
	default:
		return netip.AddrPort{}, fmt.Errorf("v2: command %d", cmd)
	}
	switch fam {
	case 0x11, 0x12: // TCP or UDP over IPv4
		if n < 12 {
			return netip.AddrPort{}, errors.New("v2: short IPv4 addresses")
		}
		a := netip.AddrFrom4([4]byte(body[0:4]))
		return netip.AddrPortFrom(a, binary.BigEndian.Uint16(body[8:10])), nil
	case 0x21, 0x22: // TCP or UDP over IPv6
		if n < 36 {
			return netip.AddrPort{}, errors.New("v2: short IPv6 addresses")
		}
		a := netip.AddrFrom16([16]byte(body[0:16]))
		return netip.AddrPortFrom(a, binary.BigEndian.Uint16(body[32:34])), nil
	default: // UNSPEC, unix sockets: no address to use
		return netip.AddrPort{}, nil
	}
}

// addrOf returns a net.Addr's IP, unmapped; the zero Addr when it has none.
func addrOf(a net.Addr) netip.Addr {
	switch v := a.(type) {
	case *net.TCPAddr:
		ip, _ := netip.AddrFromSlice(v.IP)
		return ip.Unmap()
	case nil:
		return netip.Addr{}
	}
	ap, err := netip.ParseAddrPort(a.String())
	if err != nil {
		return netip.Addr{}
	}
	return ap.Addr().Unmap()
}
