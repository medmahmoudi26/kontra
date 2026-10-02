package probe

import (
	"bytes"
	"context"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	fhttp "github.com/sw33tLie/http"
)

// serve starts a one-shot TCP server and returns a Target pointing at it.
func serve(t *testing.T, handler func(net.Conn)) Target {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		handler(conn)
	}()
	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)
	return Target{Host: host, Port: port, Scheme: "http"}
}

// readFor drains whatever arrives within d and returns it.
func readFor(conn net.Conn, d time.Duration) []byte {
	_ = conn.SetReadDeadline(time.Now().Add(d))
	var got []byte
	buf := make([]byte, 4096)
	for {
		n, err := conn.Read(buf)
		got = append(got, buf[:n]...)
		if err != nil {
			return got
		}
	}
}

const okResp = "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nX-Odd_Name: 1\r\n\r\nhi"

// TestRawBytesReachTheWireVerbatim is the regression test that matters most.
//
// The failure it guards against is silent: a payload gets normalised somewhere in the
// stack, the scanner keeps running at full speed, and every target reports clean. That
// is exactly how the dual-CL bug in HTTP Request Smuggler went unnoticed for months.
// Asserting on status codes would never catch it. Only a byte comparison does.
func TestRawBytesReachTheWireVerbatim(t *testing.T) {
	var mu sync.Mutex
	var got []byte
	target := serve(t, func(conn net.Conn) {
		b := readFor(conn, 300*time.Millisecond)
		mu.Lock()
		got = b
		mu.Unlock()
		_, _ = conn.Write([]byte(okResp))
	})

	// Every hostile construction from the corpus in one payload: a header with a
	// space before the colon, a tab-prefixed header, a plus-prefixed value, a line
	// with no colon at all, and a bare LF inside a value.
	payload := "POST / HTTP/1.1\r\n" +
		"Host: {HOST}\r\n" +
		"Content-Length \r\n : 31\r\n" +
		"\tContent-Length: +30\r\n" +
		"X:BarContent-Length: 0\r\n" +
		"NoColonHere\r\n" +
		"X-Bare-LF: a\nb\r\n" +
		"\r\n"

	raw := BuildRaw(payload, target)
	obs := Run(context.Background(), target, []Step{{Label: "mutated", Raw: raw}}, DefaultOptions())

	if obs.DialErr != "" {
		t.Fatalf("dial: %s", obs.DialErr)
	}
	mu.Lock()
	defer mu.Unlock()
	if !bytes.Equal(got, raw) {
		t.Fatalf("wire bytes were altered in transit\n want %q\n  got %q", raw, got)
	}
	if obs.Steps[0].Status != 200 {
		t.Fatalf("status = %d, want 200 (parse: %s)", obs.Steps[0].Status, obs.Steps[0].ParseErr)
	}
	// Header names come off the wire, so case is preserved and the underscore survives.
	if want := "X-Odd_Name"; !contains(obs.Steps[0].HeaderNames, want) {
		t.Fatalf("header names %v missing %q", obs.Steps[0].HeaderNames, want)
	}
}

// TestHeaderMapFlattensNewlines locks in the trap in the fork.
//
// ValidHeaderFieldValue is patched to return true, which makes it look as though the
// Header map can carry anything. It cannot: writeSubset still runs
// headerNewlineToSpace over every value. Disabled validation buys the NAME path, not
// the VALUE path. Anything carrying a raw newline must be written as raw bytes.
func TestHeaderMapFlattensNewlines(t *testing.T) {
	req, err := fhttp.NewRequest("GET", "http://example.com/", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("X-Probe", "canary\r\nX-Injected: yes")
	req.Header["X Invalid Name"] = []string{"kept"}

	var buf bytes.Buffer
	if err := req.Write(&buf); err != nil {
		t.Fatalf("write: %v", err)
	}
	wire := buf.String()

	if strings.Contains(wire, "canary\r\nX-Injected") {
		t.Fatal("CRLF survived the Header map - the fork changed; re-check header.go writeSubset")
	}
	if !strings.Contains(wire, "canary  X-Injected: yes") {
		t.Fatalf("expected newlines flattened to spaces, got:\n%s", wire)
	}
	// The name path IS unlocked by the patch: stock net/http drops invalid names.
	if !strings.Contains(wire, "X Invalid Name: kept") {
		t.Fatalf("expected the invalid header name to survive, got:\n%s", wire)
	}
}

// TestPipelinedStepsGetTheirOwnResponses proves per-step attribution when two
// responses arrive in one read. Without exact bufio accounting the second step would
// be credited with zero bytes and the desync would be invisible.
func TestPipelinedStepsGetTheirOwnResponses(t *testing.T) {
	target := serve(t, func(conn net.Conn) {
		_ = readFor(conn, 300*time.Millisecond)
		_, _ = conn.Write([]byte(
			"HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nhi" +
				"HTTP/1.1 404 Not Found\r\nContent-Length: 3\r\n\r\nbye"))
	})

	o := DefaultOptions()
	o.Mode = ModePipelined
	obs := Run(context.Background(), target, []Step{
		{Label: "baseline", Raw: BuildRaw("GET / HTTP/1.1\r\nHost: {HOST}\r\n\r\n", target)},
		{Label: "canary", Raw: BuildRaw("GET /canary HTTP/1.1\r\nHost: {HOST}\r\n\r\n", target)},
	}, o)

	if obs.Mode != "pipelined" {
		t.Fatalf("mode = %s", obs.Mode)
	}
	if obs.Steps[0].Status != 200 || obs.Steps[1].Status != 404 {
		t.Fatalf("statuses = %d,%d want 200,404 (errs %q %q)",
			obs.Steps[0].Status, obs.Steps[1].Status, obs.Steps[0].ParseErr, obs.Steps[1].ParseErr)
	}
	if obs.Steps[1].RespBytes == 0 {
		t.Fatal("second step credited with 0 response bytes")
	}
	if !bytes.HasPrefix(obs.Steps[1].RespRaw, []byte("HTTP/1.1 404")) {
		t.Fatalf("second step raw = %q", obs.Steps[1].RespRaw)
	}
	if string(obs.Steps[1].BodyPreview) != "bye" {
		t.Fatalf("second body = %q", obs.Steps[1].BodyPreview)
	}
}

// TestUnparseableResponseIsStillRecorded is the "no expectations about response
// format" property: when the peer emits something that is not HTTP, the bytes must
// survive to the dataset. A scanner that drops them can only ever find bugs whose
// shape it already knows.
func TestUnparseableResponseIsStillRecorded(t *testing.T) {
	target := serve(t, func(conn net.Conn) {
		_ = readFor(conn, 200*time.Millisecond)
		_, _ = conn.Write([]byte("GARBAGE\x00\x01 not http at all\r\n\r\n"))
	})

	obs := Run(context.Background(), target,
		[]Step{{Label: "mutated", Raw: BuildRaw("GET / HTTP/1.1\r\nHost: {HOST}\r\n\r\n", target)}},
		DefaultOptions())

	s := obs.Steps[0]
	if s.ParseErr == "" {
		t.Fatal("expected a parse error")
	}
	if obs.StreamBytes == 0 {
		t.Fatal("garbage response was not retained")
	}
	all := append(append([]byte{}, s.RespRaw...), obs.TrailingRaw...)
	if !bytes.Contains(all, []byte("GARBAGE")) {
		t.Fatalf("raw bytes lost: step=%q trailing=%q", s.RespRaw, obs.TrailingRaw)
	}
}

// TestPeerCloseIsAttributedToAStep - knowing WHICH request the server hung up on is
// half the signal in desync detection.
func TestPeerCloseIsAttributedToAStep(t *testing.T) {
	target := serve(t, func(conn net.Conn) {
		_ = readFor(conn, 150*time.Millisecond)
		_, _ = conn.Write([]byte(okResp))
		// Hang up rather than serve the second request.
	})

	obs := Run(context.Background(), target, []Step{
		{Label: "baseline", Raw: BuildRaw("GET / HTTP/1.1\r\nHost: {HOST}\r\n\r\n", target)},
		{Label: "canary", Raw: BuildRaw("GET /canary HTTP/1.1\r\nHost: {HOST}\r\n\r\n", target)},
	}, DefaultOptions())

	if obs.ClosedAfter != 1 {
		t.Fatalf("ClosedAfter = %d, want 1 (steps: %+v)", obs.ClosedAfter, obs.Steps)
	}
	if obs.Steps[0].Status != 200 {
		t.Fatalf("first step should have completed, got %d", obs.Steps[0].Status)
	}
}

// TestDialFailureIsAnObservation - a refused connection is data, not an error.
func TestDialFailureIsAnObservation(t *testing.T) {
	obs := Run(context.Background(),
		Target{Host: "127.0.0.1", Port: 1, Scheme: "http"},
		[]Step{{Label: "baseline", Raw: []byte("GET / HTTP/1.1\r\n\r\n")}},
		DefaultOptions())
	if obs == nil {
		t.Fatal("nil observation")
	}
	if obs.DialErr == "" {
		t.Fatal("expected a dial error to be recorded")
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// TestRequestsSentCountsTheWireAndNotThePlan.
//
// The campaign number this feeds — "total requests sent" — is only worth printing if it is a
// count of bytes that left, not of steps that were intended. The three cases below are the three
// that make len(Steps) wrong, and all three occur in a real sweep:
//
//	a dial that never connects   0 sent, 1 planned
//	a peer that hangs up early   1 sent, 2 planned
//	the ordinary case            n sent, n planned
func TestRequestsSentCountsTheWireAndNotThePlan(t *testing.T) {
	t.Run("a refused dial sends nothing", func(t *testing.T) {
		obs := Run(context.Background(),
			Target{Host: "127.0.0.1", Port: 1, Scheme: "http"},
			[]Step{{Label: "baseline", Raw: []byte("GET / HTTP/1.1\r\n\r\n")}},
			DefaultOptions())
		if obs.RequestsSent != 0 {
			t.Fatalf("RequestsSent = %d on a connection that never dialled, want 0", obs.RequestsSent)
		}
	})

	t.Run("every step that lands is counted", func(t *testing.T) {
		target := serve(t, func(conn net.Conn) {
			for i := 0; i < 3; i++ {
				_ = readFor(conn, 100*time.Millisecond)
				_, _ = conn.Write([]byte(okResp))
			}
		})
		raw := BuildRaw("GET / HTTP/1.1\r\nHost: {HOST}\r\n\r\n", target)
		obs := Run(context.Background(), target, []Step{
			{Label: "pre", Raw: raw}, {Label: "attack", Raw: raw}, {Label: "post", Raw: raw},
		}, DefaultOptions())
		if obs.RequestsSent != 3 {
			t.Fatalf("RequestsSent = %d across three delivered steps, want 3", obs.RequestsSent)
		}
	})

	t.Run("it never exceeds the steps that were planned", func(t *testing.T) {
		target := serve(t, func(conn net.Conn) {
			_ = readFor(conn, 100*time.Millisecond)
			_, _ = conn.Write([]byte(okResp))
		})
		raw := BuildRaw("GET / HTTP/1.1\r\nHost: {HOST}\r\n\r\n", target)
		obs := Run(context.Background(), target, []Step{
			{Label: "pre", Raw: raw}, {Label: "attack", Raw: raw},
		}, DefaultOptions())
		if obs.RequestsSent > len(obs.Steps) {
			t.Fatalf("RequestsSent = %d for %d steps — a count above the plan is impossible",
				obs.RequestsSent, len(obs.Steps))
		}
	})
}
