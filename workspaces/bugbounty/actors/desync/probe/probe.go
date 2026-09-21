package probe

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"strings"
	"syscall"
	"time"

	fhttp "github.com/sw33tLie/http"
)

// Run dials one connection, writes every step onto it, and records what came back.
// It never returns a nil *ConnObs and never returns an error: a refused connection,
// a TLS failure and a mid-stream reset are all OBSERVATIONS, not errors — they are
// often the finding. Callers persist the row either way.
func Run(ctx context.Context, t Target, steps []Step, o Options) *ConnObs {
	obs := &ConnObs{
		Target:       t,
		Mode:         o.Mode.String(),
		ClosedAfter:  -1,
		StartedAtUTC: time.Now().UTC().Format(time.RFC3339Nano),
		Steps:        make([]StepObs, len(steps)),
	}
	for i := range steps {
		obs.Steps[i].Label = steps[i].Label
	}
	start := time.Now()
	defer func() { obs.TotalMs = msSince(start) }()

	dialStart := time.Now()
	sock, err := (&net.Dialer{Timeout: o.DialTimeout}).DialContext(ctx, "tcp", t.addr())
	obs.DialMs = msSince(dialStart)
	if err != nil {
		obs.DialErr = err.Error()
		return obs
	}
	defer sock.Close()

	conn := net.Conn(sock)
	if strings.EqualFold(t.Scheme, "https") {
		tc, err := handshake(ctx, sock, t, o, obs)
		if err != nil {
			return obs
		}
		conn = tc
	}

	// Close the socket as soon as the context dies, so a hung peer cannot outlive
	// the unit. Read/write deadlines cover the common case; this covers cancellation.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.SetDeadline(time.Now())
		case <-done:
		}
	}()

	rec := &recorder{src: conn, limit: o.MaxStreamBytes}
	br := bufio.NewReaderSize(rec, 64<<10)
	wroteAt := make([]time.Time, len(steps))

	write := func(i int) bool {
		so := &obs.Steps[i]
		so.SentBytes = len(steps[i].Raw)
		so.SentSHA = sha(steps[i].Raw)
		_ = conn.SetWriteDeadline(time.Now().Add(o.WriteTimeout))
		if _, err := conn.Write(steps[i].Raw); err != nil {
			so.WriteErr = err.Error()
			// A write failure is usually the peer having already hung up on the
			// PREVIOUS step's payload — which is itself the signal.
			so.PeerClosed, so.PeerReset = classify(err)
			if obs.ClosedAfter < 0 {
				obs.ClosedAfter = i
			}
			return false
		}
		wroteAt[i] = time.Now()
		return true
	}

	read := func(i int) bool {
		so := &obs.Steps[i]
		consumedBefore := rec.consumed(br)
		wasClosed := rec.closed

		_ = conn.SetReadDeadline(time.Now().Add(o.ReadTimeout))
		rec.mark()

		resp, perr := fhttp.ReadResponse(br, &fhttp.Request{Method: methodOf(steps[i].Raw)})
		if perr != nil {
			so.ParseErr = perr.Error()
		} else {
			so.Status = resp.StatusCode
			so.Proto = resp.Proto
			body, rerr := io.ReadAll(io.LimitReader(resp.Body, o.MaxBodyBytes))
			_ = resp.Body.Close()
			if rerr != nil {
				so.ReadErr = rerr.Error()
			}
			so.BodyLen = len(body)
			so.BodySHA = sha(body)
			so.BodyPreview = clip(body, o.BodyPreviewBytes)
		}

		if !rec.firstAt.IsZero() && !wroteAt[i].IsZero() {
			so.TTFBMs = float64(rec.firstAt.Sub(wroteAt[i]).Microseconds()) / 1000
		}
		if !wroteAt[i].IsZero() {
			so.ElapsedMs = msSince(wroteAt[i])
		}

		consumedAfter := rec.consumed(br)
		so.RespBytes = consumedAfter - consumedBefore
		if raw := rec.slice(consumedBefore, consumedAfter); len(raw) > 0 {
			so.RespSHA = sha(raw)
			so.HeaderNames = rawHeaderNames(raw)
			if hb := headerBlock(raw); len(hb) > 0 {
				so.HeaderSHA = sha(hb)
			}
			if o.KeepRawResponses {
				so.RespRaw = raw
			}
		}

		so.Timeout = rec.timedOut
		if rec.closed && !wasClosed {
			so.PeerClosed = true
			if obs.ClosedAfter < 0 {
				obs.ClosedAfter = i
			}
		}
		so.PeerReset = rec.reset

		// Stop reading once the peer is gone: every later step would record the same
		// EOF and drown the real event.
		return perr == nil && !rec.closed
	}

	switch o.Mode {
	case ModePipelined:
		// Write everything first. This is the ONLY way to observe CL.0: the smuggled
		// prefix must already be in the server's buffer when the next request lands.
		for i := range steps {
			if !write(i) {
				break
			}
		}
		for i := range steps {
			if obs.Steps[i].WriteErr != "" {
				continue
			}
			if !read(i) {
				break
			}
		}
	default:
		for i := range steps {
			if !write(i) {
				break
			}
			if !read(i) {
				break
			}
		}
	}

	// Anything the server volunteered after the last response we asked for. Stacked
	// or duplicated responses land here, and they are one of the strongest desync
	// tells available without a second connection.
	_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	if tail, _ := io.ReadAll(io.LimitReader(br, 8<<10)); len(tail) > 0 {
		obs.TrailingRaw = clip(tail, o.BodyPreviewBytes*4)
	}

	obs.StreamBytes = rec.total
	obs.StreamSHA = sha(rec.buf.Bytes())
	return obs
}

// handshake performs TLS and records the negotiated facts on obs.
func handshake(ctx context.Context, sock net.Conn, t Target, o Options, obs *ConnObs) (*tls.Conn, error) {
	alpn := t.ALPN
	if len(alpn) == 0 {
		// Offer http/1.1 only. Offering h2 and then writing HTTP/1.1 bytes produces
		// garbage that looks like a finding; opting into h2 must be deliberate.
		alpn = []string{"http/1.1"}
	}
	sni := t.SNI
	if sni == "" && net.ParseIP(t.Host) == nil {
		sni = t.Host
	}
	tc := tls.Client(sock, &tls.Config{
		ServerName:         sni,
		InsecureSkipVerify: o.InsecureSkipVerify,
		NextProtos:         alpn,
		MinVersion:         tls.VersionTLS10,
	})
	hs := time.Now()
	if err := tc.HandshakeContext(ctx); err != nil {
		obs.TLSMs = msSince(hs)
		obs.TLSErr = err.Error()
		return nil, err
	}
	obs.TLSMs = msSince(hs)
	st := tc.ConnectionState()
	obs.ALPN = st.NegotiatedProtocol
	obs.TLSVersion = tlsVersionName(st.Version)
	obs.CipherSuite = tls.CipherSuiteName(st.CipherSuite)
	return tc, nil
}

// recorder tees every byte read off the socket into a capped buffer, and classifies
// the errors the socket produced. It is the only thing that sees the true stream —
// bufio, the parser and the body reader all sit downstream of it.
type recorder struct {
	src   io.Reader
	buf   bytes.Buffer
	limit int
	total int

	closed   bool
	reset    bool
	timedOut bool

	pending bool
	firstAt time.Time
}

func (rc *recorder) Read(p []byte) (int, error) {
	n, err := rc.src.Read(p)
	if n > 0 {
		if rc.pending {
			rc.firstAt = time.Now()
			rc.pending = false
		}
		rc.total += n
		if room := rc.limit - rc.buf.Len(); room > 0 {
			rc.buf.Write(p[:min(n, room)])
		}
	}
	if err != nil {
		c, r := classify(err)
		rc.closed = rc.closed || c
		rc.reset = rc.reset || r
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			rc.timedOut = true
		}
	}
	return n, err
}

func (rc *recorder) mark() { rc.pending = true; rc.firstAt = time.Time{} }

// consumed is how many bytes of the stream the PARSER has taken, as opposed to how
// many the socket has delivered. bufio.Buffered() is exactly the difference.
func (rc *recorder) consumed(br *bufio.Reader) int { return rc.total - br.Buffered() }

// slice returns the retained stream bytes in [from,to). It clamps to what was kept:
// past MaxStreamBytes the offsets remain accurate but the bytes are gone.
func (rc *recorder) slice(from, to int) []byte {
	b := rc.buf.Bytes()
	if from < 0 || from >= len(b) || to <= from {
		return nil
	}
	return b[from:min(to, len(b))]
}

func classify(err error) (closed, reset bool) {
	if err == nil {
		return false, false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		closed = true
	}
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) ||
		strings.Contains(err.Error(), "connection reset by peer") ||
		strings.Contains(err.Error(), "broken pipe") {
		reset, closed = true, true
	}
	return closed, reset
}

// methodOf extracts the request method from raw bytes. It exists only so ReadResponse
// can apply the HEAD special case; it must never influence what goes on the wire.
func methodOf(raw []byte) string {
	if i := bytes.IndexAny(raw, " \r\n"); i > 0 {
		return string(raw[:i])
	}
	return "GET"
}

// headerBlock returns the status line plus header block, up to and including the
// terminating blank line.
func headerBlock(raw []byte) []byte {
	if i := bytes.Index(raw, []byte("\r\n\r\n")); i >= 0 {
		return raw[:i+4]
	}
	if i := bytes.Index(raw, []byte("\n\n")); i >= 0 {
		return raw[:i+2]
	}
	return raw
}

// rawHeaderNames pulls header names off the wire in ORDER and with CASE PRESERVED.
// http.Header is a canonicalising map, so it cannot answer either question — and both
// are fingerprints: order identifies the emitting stack, case identifies whether a
// proxy rewrote the block on the way out.
func rawHeaderNames(raw []byte) []string {
	block := headerBlock(raw)
	lines := bytes.Split(block, []byte("\n"))
	names := make([]string, 0, len(lines))
	for i, ln := range lines {
		if i == 0 {
			continue // status line
		}
		ln = bytes.TrimRight(ln, "\r")
		if len(ln) == 0 {
			break
		}
		if ln[0] == ' ' || ln[0] == '\t' {
			// obs-fold continuation: report it, it is exactly the kind of line that
			// makes two parsers disagree.
			names = append(names, "<fold>")
			continue
		}
		if c := bytes.IndexByte(ln, ':'); c >= 0 {
			names = append(names, string(ln[:c]))
		} else {
			names = append(names, "<nocolon:"+string(clip(ln, 32))+">")
		}
	}
	return names
}

// BuildRaw substitutes {HOST} in a template and returns the exact bytes to send.
// It deliberately does NOT normalise line endings: a template that needs a bare LF
// where a CRLF belongs is the entire point of this package. Write templates with
// explicit \r\n, and use the wire-assertion test to prove what leaves the socket.
func BuildRaw(tmpl string, t Target) []byte {
	host := t.Host
	if t.SNI != "" {
		host = t.SNI
	}
	return []byte(strings.ReplaceAll(tmpl, "{HOST}", host))
}

func sha(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:8])
}

func clip(b []byte, n int) []byte {
	if n <= 0 || len(b) <= n {
		return b
	}
	return b[:n]
}

func msSince(t time.Time) float64 { return float64(time.Since(t).Microseconds()) / 1000 }

func tlsVersionName(v uint16) string {
	switch v {
	case tls.VersionTLS13:
		return "TLS1.3"
	case tls.VersionTLS12:
		return "TLS1.2"
	case tls.VersionTLS11:
		return "TLS1.1"
	case tls.VersionTLS10:
		return "TLS1.0"
	}
	return "unknown"
}
