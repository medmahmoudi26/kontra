// Package lab is a local reproduction of the voapi.8x8.com bug class, so the scanner can
// be proven against a target whose ground truth is known.
//
//	client ──▶ frontend ──▶ backend
//	           URL-decodes    strict-ish parser; a header line it
//	           the path,      cannot parse ENDS the header block,
//	           narrows it,    so an injected LF costs the request
//	           reforwards     its Host
//
// THE BUG is one line in the frontend: after percent-decoding it interprets the bytes as
// UTF-8 and truncates every codepoint to a byte. U+070A becomes 0x0A. That is the whole
// vulnerability, and it is why a blacklist can never fix it — the input space is every
// codepoint congruent to 0x0A mod 256.
//
// MODES
//
//	vulnerable — blacklists the payloads that were actually reported, exactly as the
//	             real target was patched five times. Every other fold still lands.
//	patched    — rejects control bytes in the decoded input and never narrows. The
//	             scanner must find nothing; this is the false-positive gate.
//	erratic    — answers randomly ~40% of the time, to prove the stability gate
//	             refuses noisy targets instead of drowning in diffs.
package lab

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"strings"
	"sync/atomic"
)

// flip drives the erratic mode's alternation.
var flip int64

// Lab is a running frontend+backend pair on ephemeral ports.
type Lab struct {
	Mode     string
	Frontend string // host:port to point a scanner at
	backend  string
	lns      []net.Listener
}

// Start brings up both listeners on ephemeral ports and returns once they are accepting.
// Tests use this: a fixed port is how a stale instance from an earlier run silently
// serves the next one.
func Start(mode string) (*Lab, error) { return StartAt(mode, "127.0.0.1:0", "127.0.0.1:0") }

// StartAt binds specific addresses, for manual poking at a known URL.
func StartAt(mode, feAddr, beAddr string) (*Lab, error) {
	beLn, err := net.Listen("tcp", beAddr)
	if err != nil {
		return nil, err
	}
	feLn, err := net.Listen("tcp", feAddr)
	if err != nil {
		beLn.Close()
		return nil, err
	}
	l := &Lab{Mode: mode, Frontend: feLn.Addr().String(), backend: beLn.Addr().String(),
		lns: []net.Listener{beLn, feLn}}
	go serveOn(beLn, func(c net.Conn) { backend(c, mode) })
	go serveOn(feLn, func(c net.Conn) { frontend(c, l.backend, mode, false) })
	return l, nil
}

// URL is the base URL a scanner should be pointed at.
func (l *Lab) URL() string { return "http://" + l.Frontend + "/" }

// Close stops both listeners.
func (l *Lab) Close() {
	for _, ln := range l.lns {
		_ = ln.Close()
	}
}

var blacklist = []string{"%0a", "%0d", "%c4%8a", "%e5%98%8a", "%e0%ac%8a"}

func serveOn(ln net.Listener, h func(net.Conn)) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() { defer c.Close(); h(c) }()
	}
}

// frontend is the vulnerable proxy. It reads a request, pulls the raw path out of the
// request line, decodes it, narrows it, and splices the result into a NEW request line
// sent to the backend — the secondary-context gadget.
func frontend(c net.Conn, backendAddr, mode string, verbose bool) {
	br := bufio.NewReader(c)
	line, err := br.ReadString('\n')
	if err != nil {
		return
	}
	parts := strings.Fields(strings.TrimRight(line, "\r\n"))
	if len(parts) < 2 {
		write(c, 400, "Bad Request", "malformed request line")
		return
	}
	rawPath := parts[1]
	var tenant string
	for {
		h, err := br.ReadString('\n')
		if err != nil || strings.TrimRight(h, "\r\n") == "" {
			break
		}
		if name, val, ok := strings.Cut(strings.TrimRight(h, "\r\n"), ":"); ok &&
			strings.EqualFold(strings.TrimSpace(name), advertisedHeader) {
			tenant = strings.TrimSpace(val)
		}
	}

	decoded := percentDecode(rawPath)
	var narrowed []byte

	if mode == "patched" {
		// What a correct fix looks like: reject control bytes in the DECODED input,
		// and never perform a lossy conversion. Folds become ordinary path characters.
		for _, b := range decoded {
			if b < 0x20 {
				write(c, 400, "Bad Request", "illegal character in path")
				return
			}
		}
		narrowed = decoded
	} else {
		// What was actually shipped, five times: block the payload that was reported
		// last time and leave the conversion alone.
		low := strings.ToLower(rawPath)
		for _, bad := range blacklist {
			if strings.Contains(low, bad) {
				write(c, 403, "Forbidden", "blocked by edge filter")
				return
			}
		}

		// ---- THE BUG ------------------------------------------------------------
		// Decode as UTF-8, then truncate each codepoint to a single byte.
		// U+070A -> 0x0A. Every codepoint congruent to 0x0A mod 256 is a bypass, so
		// a blacklist can only ever remove points from an infinite set.
		for _, r := range string(decoded) {
			narrowed = append(narrowed, byte(r))
		}
		// --------------------------------------------------------------------------
	}

	be, err := net.Dial("tcp", backendAddr)
	if err != nil {
		write(c, 502, "Bad Gateway", "backend unreachable")
		return
	}
	defer be.Close()

	// The reforward. Note the frontend's own Host header lands AFTER the attacker's
	// bytes, which is what makes an injected LF cost the request its Host.
	// The same narrowing bug on a SECOND surface — a header the response advertises but
	// the request never sent. This is the point a URL-only scanner cannot reach and a
	// static header wordlist cannot name.
	var tenantOut []byte
	if tenant != "" {
		if mode == "patched" {
			tenantOut = []byte(tenant)
		} else {
			for _, r := range string(percentDecode(tenant)) {
				tenantOut = append(tenantOut, byte(r))
			}
		}
	}
	hdrs := ""
	if len(tenantOut) > 0 {
		// Ahead of Host, so an injected newline can introduce one of its own.
		hdrs = fmt.Sprintf("%s: %s\r\n", advertisedHeader, tenantOut)
	}
	req := fmt.Sprintf("GET %s HTTP/1.1\r\n%sHost: lab-upstream\r\nConnection: close\r\n\r\n", narrowed, hdrs)
	if verbose {
		fmt.Fprintf(os.Stderr, "-> backend: %q\n", req)
	}
	if _, err := be.Write([]byte(req)); err != nil {
		write(c, 502, "Bad Gateway", "backend write failed")
		return
	}
	relay(c, be)
}

// backend models a parser strict enough that a line it cannot read as a header ends the
// header block. Real routers do this; it is why an injected newline surfaces as a
// missing Host rather than as a parse error.
func backend(c net.Conn, mode string) {
	br := bufio.NewReader(c)
	reqLine, err := br.ReadString('\n')
	if err != nil {
		return
	}
	var host string
	for {
		h, err := br.ReadString('\n')
		if err != nil {
			break
		}
		h = strings.TrimRight(h, "\r\n")
		if h == "" {
			break
		}
		name, val, ok := strings.Cut(h, ":")
		if !ok || strings.ContainsAny(name, " \t") || name == "" {
			break // unparseable line ends the header block
		}
		// First Host wins, which is what makes an injected one observable.
		if strings.EqualFold(strings.TrimSpace(name), "host") && host == "" {
			host = strings.TrimSpace(val)
		}
	}

	// Alternating rather than random, so the stability gate is tested deterministically.
	// Randomness would make the test flaky for the same reason the gate itself is
	// probabilistic: a host noisy at rate p survives N repeats with probability
	// p^N + (1-p)^N — 28% at p=0.4, N=3. The gate is a filter, not a proof.
	if mode == "erratic" && atomic.AddInt64(&flip, 1)%2 == 0 {
		write(c, 503, "Service Unavailable", "flaky upstream")
		return
	}
	if host == "" {
		// The oracle. This is the literal string the real target returns.
		write(c, 400, "Bad Request", "No Host")
		return
	}
	write(c, 200, "OK", "routed to host: "+host+" for "+strings.TrimSpace(reqLine))
}

func relay(dst net.Conn, src net.Conn) {
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// advertisedHeader is named ONLY in the response. A scanner that reads the response
// learns the application accepts it; one that does not, never tests it.
const advertisedHeader = "X-Tenant-Id"

func write(c net.Conn, code int, reason, body string) {
	fmt.Fprintf(c, "HTTP/1.1 %d %s\r\nContent-Type: text/plain\r\n"+
		"Access-Control-Allow-Headers: Content-Type, %s\r\nVary: %s\r\n"+
		"Content-Length: %d\r\nConnection: close\r\n\r\n%s",
		code, reason, advertisedHeader, advertisedHeader, len(body), body)
}

func percentDecode(s string) []byte {
	var out []byte
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			if h, ok := unhex(s[i+1]); ok {
				if l, ok2 := unhex(s[i+2]); ok2 {
					out = append(out, h<<4|l)
					i += 2
					continue
				}
			}
		}
		out = append(out, s[i])
	}
	return out
}

func unhex(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}
