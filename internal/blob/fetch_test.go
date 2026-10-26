package blob

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"kontra.sh/kontra/internal/tunnel"
)

// fakeGate answers one blob stream: it reads the preamble the node wrote, records it, writes answer and
// closes the stream unless keepOpen.
type fakeGate struct {
	pre      chan tunnel.Preamble
	answer   []byte
	keepOpen bool
}

func (g *fakeGate) serve(t *testing.T, c net.Conn) {
	t.Helper()
	go func() {
		br := bufio.NewReader(c)
		p, err := tunnel.ReadPreamble(br)
		if err != nil {
			_ = c.Close()
			return
		}
		g.pre <- p
		_, _ = c.Write(g.answer)
		if !g.keepOpen {
			_ = c.Close()
		}
	}()
}

func fetchFrom(t *testing.T, answer []byte, keepOpen bool) (int64, []byte, tunnel.Preamble, error) {
	t.Helper()
	node, gate := net.Pipe()
	t.Cleanup(func() { _ = gate.Close() })
	g := &fakeGate{pre: make(chan tunnel.Preamble, 1), answer: answer, keepOpen: keepOpen}
	g.serve(t, gate)
	ref, err := NewRef()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var buf bytes.Buffer
	n, err := Fetch(ctx, node, ref, &buf)
	var p tunnel.Preamble
	select {
	case p = <-g.pre:
		if p.Op != tunnel.OpBlob || p.Ref != string(ref) || p.V != tunnel.Version || p.Service != "" {
			t.Errorf("the preamble %+v, want op blob and ref %s alone", p, ref)
		}
	default:
	}
	return n, buf.Bytes(), p, err
}

// The node's half of spec/tunnel.md §8.3 (D181): one size line, exactly n bytes, the gate's close.
func TestFetch(t *testing.T) {
	body := bytes.Repeat([]byte("kontra"), 50_000) // 300,000 bytes
	n, got, _, err := fetchFrom(t, append([]byte(`{"size":`+strconv.Itoa(len(body))+"}\n"), body...), false)
	if err != nil || n != int64(len(body)) || !bytes.Equal(got, body) {
		t.Fatalf("Fetch = %d, %d bytes, %v; want the %d bytes", n, len(got), err, len(body))
	}
}

// The gate's refusals are *FetchError with their reason; nothing is written to w.
func TestFetchRefusals(t *testing.T) {
	for _, reason := range []string{ReasonNotFound, ReasonForbidden, ReasonTooLarge} {
		var line bytes.Buffer
		if err := WriteRefusal(&line, reason); err != nil {
			t.Fatal(err)
		}
		_, got, _, err := fetchFrom(t, line.Bytes(), false)
		var fe *FetchError
		if !errors.As(err, &fe) || fe.Reason != reason || len(got) != 0 {
			t.Errorf("%s: %v (%d bytes written)", reason, err, len(got))
		}
	}
}

// An announced size over 64 MiB is refused before a byte of the object is read: the fake gate writes the line
// alone and keeps the stream open, so a Fetch that read on would hang.
func TestFetchRefusesAnnouncedSizeOver64MiB(t *testing.T) {
	start := time.Now()
	_, got, _, err := fetchFrom(t, []byte(`{"size":`+strconv.Itoa(65<<20)+"}\n"), true)
	if err == nil || !strings.Contains(err.Error(), "over") || len(got) != 0 {
		t.Fatalf("65 MiB announced: %v (%d bytes)", err, len(got))
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("Fetch waited for the body of a size it refuses")
	}
	if _, _, _, err := fetchFrom(t, []byte(`{"size":`+strconv.Itoa(MaxObject+1)+"}\n"), true); err == nil {
		t.Fatal("MaxObject+1 announced: accepted")
	}
}

// One byte more than announced is refused, as are fewer bytes than announced.
func TestFetchRefusesMoreOrFewerBytes(t *testing.T) {
	if _, _, _, err := fetchFrom(t, []byte("{\"size\":3}\nabcd"), false); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Fatalf("one byte more: %v", err)
	}
	if _, _, _, err := fetchFrom(t, []byte("{\"size\":5}\nabc"), false); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("two bytes fewer: %v", err)
	}
	if n, got, _, err := fetchFrom(t, []byte("{\"size\":3}\nabc"), false); err != nil || n != 3 || string(got) != "abc" {
		t.Fatalf("exactly: %d %q %v", n, got, err)
	}
}

// The size line is read with exact keys and one member (D172).
func TestFetchRefusesMalformedLines(t *testing.T) {
	for _, line := range []string{
		`{"size":1,"SIZE":2}`, `{"size":1,"size":2}`, `{"Size":1}`, `{"size":-1}`, `{"size":0}`, `{"size":1.5}`,
		`{"size":"1"}`, `{"size":1,"error":"not_found"}`, `{"error":"nope"}`, `{"error":1}`, `[1]`, `not json`,
		`{}`, `{"size":1} {}`, `{"size":` + strings.Repeat("1", 300) + `}`,
	} {
		_, got, _, err := fetchFrom(t, []byte(line+"\nx"), false)
		var fe *FetchError
		if err == nil || errors.As(err, &fe) || len(got) != 0 {
			t.Errorf("%q: %v (%d bytes)", line, err, len(got))
		}
	}
	// No line at all: the gate closed at once.
	if _, _, _, err := fetchFrom(t, nil, false); err == nil {
		t.Error("an empty answer was accepted")
	}
}

// A ref of the wrong shape is refused before anything is written; ctx's end closes the stream.
func TestFetchRefBeforeWriting(t *testing.T) {
	node, gate := net.Pipe()
	defer gate.Close()
	if _, err := Fetch(context.Background(), node, Ref("../../etc"), io.Discard); err == nil {
		t.Fatal("a malformed ref was fetched")
	}
	if _, err := node.Write([]byte("x")); err == nil {
		t.Fatal("Fetch did not close the stream it was given")
	}

	node2, gate2 := net.Pipe()
	defer gate2.Close()
	go func() { _, _ = io.Copy(io.Discard, gate2) }() // reads the preamble, never answers
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	ref, _ := NewRef()
	if _, err := Fetch(ctx, node2, ref, io.Discard); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a gate that never answers: %v, want the context's end", err)
	}
}

// WriteSize and WriteRefusal are the lines Fetch reads.
func TestSizeLines(t *testing.T) {
	var b bytes.Buffer
	_ = WriteSize(&b, 307200)
	if b.String() != "{\"size\":307200}\n" {
		t.Fatalf("WriteSize: %q", b.String())
	}
	if n, err := parseSizeLine(bytes.TrimSuffix(b.Bytes(), []byte("\n"))); err != nil || n != 307200 {
		t.Fatalf("parse: %d %v", n, err)
	}
	b.Reset()
	_ = WriteRefusal(&b, ReasonTooLarge)
	if b.String() != "{\"error\":\"too_large\"}\n" {
		t.Fatalf("WriteRefusal: %q", b.String())
	}
}
