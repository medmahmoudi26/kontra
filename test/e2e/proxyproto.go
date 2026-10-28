package e2e

import (
	"encoding/binary"
	"net"
	"net/netip"
)

// M4 (WP-07): the Front as a PROXY v2 forwarder (spec/relay.md §2.2, D69; spec/tunnel.md §6.2). With
// Options.ProxyProtocol the replicas take a client's address from a PROXY header sent by a trusted peer
// (edge.client_ip: {from: proxy_protocol, trusted: [127.0.0.1/32]}), and the Front writes one in front of every
// connection it forwards, naming the source address the test chose last (Front.SetSource): a node it forwards
// then appears to come from that address, so a test places nodes in chosen IPv4 /24s and IPv6 /48s. A connection
// the test makes to a replica directly carries no header and is served as itself (127.0.0.1), which the edge
// allows from a trusted peer.

// proxyV2Signature opens every PROXY v2 header (haproxy.org/download/2.9/doc/proxy-protocol.txt, §2.2).
var proxyV2Signature = []byte("\r\n\r\n\x00\r\nQUIT\n")

// proxyV2Header is a PROXY v2 header (command PROXY, TCP) naming src as the connection's source and dst as its
// destination; an IPv4 source with an IPv6 destination is written as IPv4-mapped, so both are of one family.
func proxyV2Header(src netip.AddrPort, dst netip.AddrPort) []byte {
	s, d := src.Addr(), dst.Addr()
	var fam byte
	var addrs []byte
	if s.Is4() && d.Unmap().Is4() {
		fam = 0x11 // TCP over IPv4
		a, b := s.As4(), d.Unmap().As4()
		addrs = append(append(addrs, a[:]...), b[:]...)
	} else {
		fam = 0x21 // TCP over IPv6
		a, b := s.As16(), d.As16()
		addrs = append(append(addrs, a[:]...), b[:]...)
	}
	addrs = binary.BigEndian.AppendUint16(addrs, src.Port())
	addrs = binary.BigEndian.AppendUint16(addrs, dst.Port())
	h := append([]byte(nil), proxyV2Signature...)
	h = append(h, 0x21, fam) // version 2, command PROXY
	h = binary.BigEndian.AppendUint16(h, uint16(len(addrs)))
	return append(h, addrs...)
}

// SetSource makes every connection the Front forwards from now on start with a PROXY v2 header naming addr (on an
// ephemeral port) as its source; the zero Addr sends none again. Established connections are left alone.
func (f *Front) SetSource(addr netip.Addr) {
	f.mu.Lock()
	f.src = addr
	f.mu.Unlock()
}

// Source is the address the Front's next connections name, the zero Addr when they carry no header.
func (f *Front) Source() netip.Addr {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.src
}

// proxyHeader is the header to write before forwarding c to u, nil when the Front names no source.
func (f *Front) proxyHeader(c, u net.Conn) []byte {
	f.mu.Lock()
	src := f.src
	f.mu.Unlock()
	if !src.IsValid() {
		return nil
	}
	port := uint16(40000)
	if a, ok := c.RemoteAddr().(*net.TCPAddr); ok {
		port = uint16(a.Port)
	}
	dst := netip.MustParseAddrPort("127.0.0.1:443")
	if a, ok := u.RemoteAddr().(*net.TCPAddr); ok {
		dst = a.AddrPort()
	}
	return proxyV2Header(netip.AddrPortFrom(src, port), dst)
}
