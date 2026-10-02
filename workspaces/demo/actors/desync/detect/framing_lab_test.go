package detect

// A CL.0 reproduction with known ground truth, and the tests that make the framing oracle
// falsifiable before it is pointed at anybody's production infrastructure.
//
// The lab is TWO parsers on one socket, which is the only honest way to model this bug: a real
// CL.0 is a disagreement between a front-end and a back-end, and a single-process mock that just
// "returns 400 sometimes" would let an oracle pass that has learned nothing.
//
//	frontend  reads headers with a PERMISSIVE parser. Finds Content-Length even when the header
//	          line is malformed, so it forwards exactly that many body bytes downstream.
//	backend   reads headers with a STRICT parser. A malformed Content-Length line is not a
//	          Content-Length, so it reads ZERO body bytes — and the bytes the frontend forwarded
//	          stay in the buffer and prefix whatever arrives next.
//
// mode=patched runs the STRICT parser on both sides: the malformed line is ignored by the
// frontend too, so no body is ever forwarded and there is nothing to leave behind.

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/medmahmoudi26/kontra-actors/go/desync/probe"
)

type clLab struct {
	ln       net.Listener
	mode     string // "vulnerable" | "patched" | "erratic"
	addr     string
	closed   chan struct{}
	wg       sync.WaitGroup
	erratics int
	mu       sync.Mutex
}

func startCLLab(t *testing.T, mode string) *clLab {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l := &clLab{ln: ln, mode: mode, addr: ln.Addr().String(), closed: make(chan struct{})}
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			l.wg.Add(1)
			go func() { defer l.wg.Done(); l.serve(c) }()
		}
	}()
	t.Cleanup(func() { ln.Close(); l.wg.Wait() })
	return l
}

// serve is the two-parser model. One goroutine, one socket, but the header block is read twice:
// once permissively (what the frontend would forward) and once strictly (what the backend would
// consume). The gap between the two answers is the bug.
func (l *clLab) serve(c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)
	var leftover []byte

	for {
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))

		reqLine, hdrs, err := readHeadBlock(br, leftover)
		leftover = nil
		if err != nil {
			return
		}

		// THE DISAGREEMENT. Both parsers look at the same bytes.
		fwd := permissiveCL(hdrs) // what the frontend believes and forwards
		read := strictCL(hdrs)    // what the backend believes and consumes

		// "pipelining" IS NOT A DESYNC AND THAT IS ITS WHOLE POINT.
		//
		// Both parsers AGREE — they agree to ignore Content-Length entirely — so nothing is ever
		// consumed as a body and whatever follows the header block becomes the next request on
		// the connection. That is ordinary HTTP pipelining. Every oracle in `AnalyzeFraming`
		// fires on it: the canary comes back, the post response differs, bytes arrive nobody
		// asked for. And none of it has anything to do with a malformed header, because a
		// perfectly well-formed request behaves identically.
		//
		// This is the server the 2026-09-18 campaign found and reported as a vulnerability
		// twice. The lab's own comment above already named the shape — "with fwd=0 nothing was
		// consumed, the body stayed in the client stream, and it became the next request in
		// EVERY mode" — it was treated as a lab bug to fix rather than a real server to detect.
		// It is both, and `control_lab_test.go` is what tells them apart.
		if l.mode == "pipelining" {
			fwd, read = 0, 0
		}

		// PATCHED IS NOT "IGNORE THE HEADER". A server that silently ignored an unparseable
		// Content-Length and left the body in the stream would still desync — the body would
		// become the next request. The actual fix vendors ship is to REJECT: RFC 9112 §6.3 says
		// reject rather than guess. So patched answers 4xx and closes.
		if l.mode == "patched" && fwd != read {
			write400(c, "malformed Content-Length")
			return
		}

		// THE FRONTEND ALWAYS CONSUMES WHAT IT BELIEVES. It owns the client socket, so `fwd`
		// bytes leave the client's stream whatever the backend then does with them. Getting this
		// wrong gave the lab an unconditional desync: with fwd=0 nothing was consumed, the body
		// stayed in the client stream, and it became the next request in EVERY mode — including
		// patched, which made the false-positive gate untestable.
		fwdBody := make([]byte, fwd)
		if fwd > 0 {
			if _, err := io.ReadFull(br, fwdBody); err != nil {
				return
			}
		}
		// The backend consumes only what IT thinks the body is. The remainder stays in the
		// BACKEND's buffer and prefixes the next request forwarded to it — the smuggled prefix.
		if extra := fwd - read; extra > 0 {
			leftover = fwdBody[read:]
		}

		// THE RESPONSE NAMES THE PATH IT ANSWERED. Without this the lab returns identical bytes
		// for every target, a poisoned response is indistinguishable from a clean one, and the
		// oracle looks broken when it is the model that is wrong.
		//
		// It is also the realistic case and the reason the oracle hashes the body rather than
		// only comparing status: a CL.0 poison makes the server answer the SMUGGLED request, and
		// that answer is very often still a 200. Status-only detection misses it.
		target := "?"
		if f := strings.Fields(reqLine); len(f) > 1 {
			target = f[1]
		}
		status, body := 200, "ok "+target+"\n"
		if l.mode == "erratic" {
			l.mu.Lock()
			l.erratics++
			status = 200 + (l.erratics*7)%5 // 200,204,201,203,200... never reproduces
			l.mu.Unlock()
			body = fmt.Sprintf("erratic-%d\n", status)
		}
		// A request line that does not parse is a 400, which is what a prefixed request looks
		// like: "GET /x-kontraGET /real HTTP/1.1" has no valid target.
		if !strings.HasSuffix(reqLine, "HTTP/1.1") || strings.Count(reqLine, " ") != 2 {
			status, body = 400, "bad request line\n"
		}
		fmt.Fprintf(c, "HTTP/1.1 %d %s\r\nContent-Length: %d\r\nConnection: keep-alive\r\n\r\n%s",
			status, map[int]string{200: "OK", 400: "Bad Request"}[status], len(body), body)
		if status == 400 {
			return
		}
	}
}

func readHeadBlock(br *bufio.Reader, prefix []byte) (string, []string, error) {
	r := br
	if len(prefix) > 0 {
		r = bufio.NewReader(io.MultiReader(strings.NewReader(string(prefix)), br))
	}
	first, err := r.ReadString('\n')
	if err != nil {
		return "", nil, err
	}
	var hdrs []string
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return "", nil, err
		}
		if strings.TrimRight(line, "\r\n") == "" {
			break
		}
		hdrs = append(hdrs, strings.TrimRight(line, "\r\n"))
	}
	// Drain the rewrapped reader back into br's stream position by copying what is buffered.
	if len(prefix) > 0 {
		if n := r.Buffered(); n > 0 {
			buf := make([]byte, n)
			io.ReadFull(r, buf)
			*br = *bufio.NewReader(io.MultiReader(strings.NewReader(string(buf)), br))
		}
	}
	return strings.TrimRight(first, "\r\n"), hdrs, nil
}

// permissiveCL finds a Content-Length however mangled the line is. This is the forgiving edge
// parser, and its exact forgiveness is the thing under test.
//
// A LINE WITH ITS OWN NAME IS ITS OWN HEADER, even when it starts with a tab. Treating
// "\tContent-Length: 35" as an obs-fold continuation of the line above is defensible by RFC 7230
// §3.2.4 and is what the STRICT side does — but a frontend that reads it that way finds no
// Content-Length and forwards no body, which is not the bug in #2357178. The frontends that paid
// out read it as a header with junk in front of the name. Only a continuation with NO name of its
// own (" : 31", the #2356849 gadget) is folded.
func permissiveCL(hdrs []string) int {
	joined := make([]string, 0, len(hdrs))
	for _, h := range hdrs {
		if len(joined) > 0 && strings.HasPrefix(strings.TrimLeft(h, " \t"), ":") {
			joined[len(joined)-1] += " " + strings.TrimLeft(h, " \t") // obs-fold, no name of its own
			continue
		}
		joined = append(joined, h)
	}
	for _, h := range joined {
		name, val, ok := strings.Cut(h, ":")
		if !ok {
			continue
		}
		clean := strings.Map(func(r rune) rune {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '-' {
				return r
			}
			return -1
		}, name)
		if !strings.EqualFold(clean, "Content-Length") {
			continue
		}
		v := strings.TrimSpace(val)
		v = strings.TrimPrefix(v, "+")
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return 0
}

// strictCL is RFC-correct: the name must be exactly the token, no folding, no leading '+'.
func strictCL(hdrs []string) int {
	for _, h := range hdrs {
		name, val, ok := strings.Cut(h, ":")
		if !ok || !strings.EqualFold(name, "Content-Length") {
			continue
		}
		if n, err := strconv.Atoi(strings.TrimSpace(val)); err == nil {
			return n
		}
	}
	return 0
}

func write400(c net.Conn, why string) {
	body := "rejected: " + why + "\n"
	fmt.Fprintf(c, "HTTP/1.1 400 Bad Request\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		len(body), body)
}

func labTarget(l *clLab) probe.Target {
	host, port, _ := net.SplitHostPort(l.addr)
	p, _ := strconv.Atoi(port)
	return probe.Target{Host: host, Port: p, Scheme: "http"}
}

func benign(host string) []byte {
	return []byte("GET /health HTTP/1.1\r\nHost: " + host + "\r\nConnection: keep-alive\r\n\r\n")
}

// attack builds `\tContent-Length: N` + a body — the #2357178 gadget, whose CL the permissive
// parser reads and the strict one does not.
func attack(host string, body string) []byte {
	return []byte("POST /x HTTP/1.1\r\nHost: " + host + "\r\n" +
		"\tContent-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body)
}

func scannerForLab() *Scanner {
	o := DefaultOptions()
	o.Rate = time.Millisecond // the lab is ours; pacing it only slows the suite
	o.Timeout = 2 * time.Second
	o.Repeats = 3
	return New(o)
}

// TestFramingOracleFiresOnVulnerable is the positive control. If this fails, the oracle cannot
// see a CL.0 that is definitely there.
func TestFramingOracleFiresOnVulnerable(t *testing.T) {
	l := startCLLab(t, "vulnerable")
	sc, tgt := scannerForLab(), labTarget(l)
	bn := benign(tgt.Host)

	base := sc.FramingBaselineOf(t.Context(), tgt, bn, 3)
	if !base.Stable {
		t.Fatalf("lab baseline unstable: %v", base.Statuses)
	}
	conn := sc.Connection(t.Context(), tgt, []probe.Step{
		{Label: "pre", Raw: bn},
		{Label: "attack", Raw: attack(tgt.Host, "GET /smuggled-kontra HTTP/1.1\r\nX: X")},
		{Label: "post", Raw: bn},
	}, probe.ModePipelined)

	_, _, post, sig, void := AnalyzeFraming(conn, base, "")
	if void.Is {
		t.Fatalf("void: %s", void.Reason)
	}
	if sig.Count == 0 {
		t.Fatalf("oracle silent on a known CL.0. post status=%d (baseline %d), signals=%+v",
			post.Status, base.Status, sig)
	}
	t.Logf("fired: post=%d baseline=%d signals=%+v", post.Status, base.Status, sig)
}

// TestFramingOracleSilentOnPatched is the FALSE-POSITIVE GATE, and it is the one that matters.
// The patched lab runs the strict parser on both sides, so the malformed header is ignored
// everywhere and no body is ever left behind. Any signal here is a false positive, and a false
// positive at scale is what burns a program relationship.
func TestFramingOracleSilentOnPatched(t *testing.T) {
	l := startCLLab(t, "patched")
	sc, tgt := scannerForLab(), labTarget(l)
	bn := benign(tgt.Host)

	base := sc.FramingBaselineOf(t.Context(), tgt, bn, 3)
	if !base.Stable {
		t.Fatalf("patched lab baseline unstable: %v", base.Statuses)
	}
	for _, gadget := range []string{
		"\tContent-Length: ", "Content-Length\t:\t", "Content-Length: +", "Content-Length \r\n : ",
	} {
		body := "GET /smuggled-kontra HTTP/1.1\r\nX: X"
		raw := []byte("POST /x HTTP/1.1\r\nHost: " + tgt.Host + "\r\n" +
			gadget + strconv.Itoa(len(body)) + "\r\n\r\n" + body)
		conn := sc.Connection(t.Context(), tgt, []probe.Step{
			{Label: "pre", Raw: bn}, {Label: "attack", Raw: raw}, {Label: "post", Raw: bn},
		}, probe.ModePipelined)

		_, _, post, sig, void := AnalyzeFraming(conn, base, "")
		if void.Is {
			continue // could not test is not the same as clean, and is not a false positive
		}
		if sig.Count > 0 {
			t.Errorf("FALSE POSITIVE on patched lab with %q: post=%d baseline=%d signals=%+v",
				gadget, post.Status, base.Status, sig)
		}
	}
}

// TestFramingRefusesErraticHost: a host that will not reproduce its own answer must be REFUSED,
// not scanned. Every signal downstream is "step 3 differed from the baseline", which an erratic
// host satisfies for free — so scanning one manufactures findings out of noise.
func TestFramingRefusesErraticHost(t *testing.T) {
	l := startCLLab(t, "erratic")
	sc, tgt := scannerForLab(), labTarget(l)

	base := sc.FramingBaselineOf(t.Context(), tgt, benign(tgt.Host), 3)
	if base.Stable {
		t.Fatalf("erratic lab reported STABLE (%v) — the gate would have scanned it", base.Statuses)
	}
	t.Logf("refused, as required: statuses=%v", base.Statuses)
}

// TestPreFlightMismatchIsVoidNotClean guards the distinction the Void type exists for. If the
// first benign request already disagrees with the baseline the socket was never in a known state,
// and reporting that as "no signal" is a scan claiming clean about ground it never covered.
func TestPreFlightMismatchIsVoidNotClean(t *testing.T) {
	base := FramingBaseline{Status: 200, BodySHA: "aaa", Stable: true, N: 3}
	conn := &probe.ConnObs{
		ALPN: "http/1.1", ClosedAfter: -1,
		Steps: []probe.StepObs{
			{Label: "pre", Status: 503, BodySHA: "zzz"},
			{Label: "attack", Status: 400},
			{Label: "post", Status: 200, BodySHA: "aaa"},
		},
	}
	_, _, _, sig, void := AnalyzeFraming(conn, base, "")
	if !void.Is {
		t.Fatal("a pre-flight mismatch was treated as a testable observation")
	}
	if sig.Count != 0 {
		t.Fatal("signals were computed on a void observation")
	}
}

// TestNegotiatedH2IsVoid: probe offers http/1.1 only unless a caller opts in, so this should be
// unreachable — which is exactly why it is worth asserting. If h2 is ever negotiated, the HTTP/1.1
// bytes written were nonsense and every signal derived from them is invalid.
func TestNegotiatedH2IsVoid(t *testing.T) {
	conn := &probe.ConnObs{ALPN: "h2", Steps: make([]probe.StepObs, 3)}
	_, _, _, _, void := AnalyzeFraming(conn, FramingBaseline{Status: 200, Stable: true}, "")
	if !void.Is || !strings.Contains(void.Reason, "h2") {
		t.Fatalf("h2 was not voided: %+v", void)
	}
}
