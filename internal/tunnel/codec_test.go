package tunnel

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// preambleLine is a preamble line of exactly n bytes, its '\n' included.
func preambleLine(t *testing.T, n int) []byte {
	t.Helper()
	head := `{"v":1,"op":"http","service":"hello","kind":"a2a","task":"`
	tail := "\"}\n"
	if n < len(head)+len(tail) {
		t.Fatalf("a %d-byte preamble is too short", n)
	}
	return []byte(head + strings.Repeat("a", n-len(head)-len(tail)) + tail)
}

// §8.1: one JSON line of at most 4096 bytes, the '\n' included.
func TestPreambleLimit(t *testing.T) {
	for _, size := range []int{16, 4096} { // bufio's minimum buffer and the default
		ok := preambleLine(t, MaxPreamble)
		if len(ok) != 4096 {
			t.Fatal(len(ok))
		}
		p, err := ReadPreamble(bufio.NewReaderSize(bytes.NewReader(ok), size))
		if err != nil {
			t.Fatalf("buffer %d: a 4096-byte preamble: %v", size, err)
		}
		if p.Service != "hello" || len(p.Task) != 4096-len(`{"v":1,"op":"http","service":"hello","kind":"a2a","task":""}`)-1 {
			t.Fatalf("parsed %+v", p)
		}
		_, err = ReadPreamble(bufio.NewReaderSize(bytes.NewReader(preambleLine(t, MaxPreamble+1)), size))
		if !errors.Is(err, ErrLineTooLong) {
			t.Fatalf("buffer %d: a 4097-byte preamble: %v", size, err)
		}
	}
	// No newline at all: refused after 4096 bytes, not read to the end.
	r := &countingReader{r: strings.NewReader(strings.Repeat("{", 1<<20))}
	if _, err := ReadPreamble(bufio.NewReader(r)); !errors.Is(err, ErrLineTooLong) {
		t.Fatal(err)
	}
	if r.n > 2*MaxPreamble {
		t.Fatalf("read %d bytes looking for a 4096-byte line", r.n)
	}

	big := Preamble{Op: OpHTTP, Service: "hello", Kind: KindA2A, Task: strings.Repeat("t", MaxPreamble)}
	var buf bytes.Buffer
	if err := WritePreamble(&buf, big); !errors.Is(err, ErrLineTooLong) || buf.Len() != 0 {
		t.Fatalf("WritePreamble of a long preamble: %v, wrote %d", err, buf.Len())
	}
}

type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// §8.1: a preamble round-trips, unknown fields are ignored, and nothing past its '\n' is consumed, so the
// HTTP request that follows reads from the same reader.
func TestPreambleRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	in := Preamble{Op: OpHTTP, Service: "hello", Kind: KindA2A, Task: "019a0000-0000-7000-8000-000000000001",
		Context: "019a0000-0000-7000-8000-000000000002"}
	if err := WritePreamble(&buf, in); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(buf.String(), "\n") || strings.Count(buf.String(), "\n") != 1 {
		t.Fatalf("not one line: %q", buf.String())
	}
	buf.WriteString("POST /rpc HTTP/1.1\r\nHost: x\r\nContent-Length: 2\r\nConnection: close\r\n\r\n{}")
	r := bufio.NewReader(&buf)
	got, err := ReadPreamble(r)
	if err != nil {
		t.Fatal(err)
	}
	in.V = Version
	if got != in {
		t.Fatalf("got %+v, want %+v", got, in)
	}
	req, err := http.ReadRequest(r)
	if err != nil {
		t.Fatalf("the request after the preamble: %v", err)
	}
	body, _ := io.ReadAll(req.Body)
	if req.URL.Path != "/rpc" || string(body) != "{}" {
		t.Fatalf("request %s %s %q", req.Method, req.URL, body)
	}

	for _, line := range []string{
		`{"v":1,"op":"control","future":{"a":[1]}}` + "\n",
		`{"op":"http","service":"hello","kind":"a2a","v":1}` + "\r\n",
	} {
		if _, err := ReadPreamble(bufio.NewReader(strings.NewReader(line))); err != nil {
			t.Fatalf("%q: %v", line, err)
		}
	}
}

func TestPreambleRefusals(t *testing.T) {
	for _, line := range []string{
		`{"v":2,"op":"http"}` + "\n",
		`{"op":"http"}` + "\n",
		`{"v":1,"op":"socks"}` + "\n",
		`{"v":1}` + "\n",
		`{"v":1,"op":"http"` + "\n",
		`null` + "\n",
		`{"v":1,"op":"http"}`, // no '\n'
		"\n",
		"",
	} {
		if p, err := ReadPreamble(bufio.NewReader(strings.NewReader(line))); err == nil {
			t.Fatalf("ReadPreamble(%q) = %+v", line, p)
		}
	}
	if _, err := ReadPreamble(bufio.NewReader(strings.NewReader(""))); !errors.Is(err, io.EOF) {
		t.Fatalf("an empty stream: %v", err)
	}
	if _, err := ReadPreamble(bufio.NewReader(strings.NewReader(`{"v":1`))); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("a cut line: %v", err)
	}
	var buf bytes.Buffer
	if err := WritePreamble(&buf, Preamble{Op: "socks"}); err == nil {
		t.Fatal("WritePreamble of an unknown op")
	}
}

// §13: control lines round-trip; an unknown type is returned for the caller to ignore, whatever its
// fields; blank lines are skipped; the end of the stream is io.EOF.
func TestControl(t *testing.T) {
	at := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	msgs := []Control{
		{T: CtlDrain},
		{T: CtlRevoked},
		{T: CtlRevoked, Service: "hello"},
		{T: CtlAuthRevoked},
		{T: CtlEnrolled, Service: "hello", Key: "k1_abcdefghijklmnopqrstuvwxyz"},
		{T: CtlExpiring, At: &at},
	}
	var buf bytes.Buffer
	for _, m := range msgs {
		if err := WriteControl(&buf, m); err != nil {
			t.Fatal(err)
		}
	}
	if !strings.HasPrefix(buf.String(), `{"t":"drain"}`+"\n") {
		t.Fatalf("drain is written as %q", strings.SplitN(buf.String(), "\n", 2)[0])
	}
	buf.WriteString("\n")
	buf.WriteString(`{"t":"from_m9","service":5,"at":"soon","extra":{"x":1}}` + "\n")
	buf.WriteString(`{"t":"drain","why":"rolling deploy"}` + "\n")
	r := bufio.NewReader(&buf)
	for _, want := range msgs {
		got, err := ReadControl(r)
		if err != nil {
			t.Fatal(err)
		}
		if got.T != want.T || got.Service != want.Service || got.Key != want.Key ||
			(want.At != nil) != (got.At != nil) || (want.At != nil && !got.At.Equal(*want.At)) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	}
	got, err := ReadControl(r)
	if err != nil || got.T != "from_m9" || KnownControl(got.T) {
		t.Fatalf("an unknown type: %+v, %v", got, err)
	}
	if got, err := ReadControl(r); err != nil || got.T != CtlDrain {
		t.Fatalf("a known type with an unknown field: %+v, %v", got, err)
	}
	if _, err := ReadControl(r); !errors.Is(err, io.EOF) {
		t.Fatalf("end of stream: %v", err)
	}

	for _, line := range []string{
		`{"t":"expiring","at":"soon"}` + "\n", // a known type of the wrong shape
		`{"t":5}` + "\n",
		`not json` + "\n",
		`{"t":"drain"}`, // cut
	} {
		if c, err := ReadControl(bufio.NewReader(strings.NewReader(line))); err == nil {
			t.Fatalf("ReadControl(%q) = %+v", line, c)
		}
	}
	long := `{"t":"x","pad":"` + strings.Repeat("p", MaxControlLine) + `"}` + "\n"
	if _, err := ReadControl(bufio.NewReader(strings.NewReader(long))); !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("a long control line: %v", err)
	}
	if err := WriteControl(&buf, Control{}); err == nil {
		t.Fatal("a control line without a type")
	}
}

// §4 hello "origin" and §3 --gate.
func TestCanonicalOrigin(t *testing.T) {
	for raw, want := range map[string]string{
		"HTTPS://Gate.Example:443/x":      "https://gate.example",
		"https://gate.example":            "https://gate.example",
		"https://gate.example:8443/a?b#c": "https://gate.example:8443",
		"http://LOCALHOST:80":             "http://localhost",
		"http://localhost:8080":           "http://localhost:8080",
		"http://[::1]:1":                  "http://[::1]:1",
		"https://[2001:DB8::1]:443/":      "https://[2001:db8::1]",
		"https://u:p@gate.example:0443/":  "https://gate.example",
		"https://gate.example:0080":       "https://gate.example:80",
		"http://127.0.0.1:9999/v1/tunnel": "http://127.0.0.1:9999",
	} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		got, err := CanonicalOrigin(u)
		if err != nil || got != want {
			t.Fatalf("CanonicalOrigin(%s) = %q, %v; want %q", raw, got, err, want)
		}
	}
	for _, raw := range []string{"ws://gate.example", "https:///x", "https://gate.example:0", "https://gate.example:70000",
		"mailto:a@b", "https://gate.example:", "https://[fe80::1%25eth0]/"} {
		u, err := url.Parse(raw)
		if err != nil {
			continue
		}
		if got, err := CanonicalOrigin(u); err == nil {
			t.Fatalf("CanonicalOrigin(%s) = %q", raw, got)
		}
	}
	if _, err := CanonicalOrigin(nil); err == nil {
		t.Fatal("CanonicalOrigin(nil)")
	}
}

func TestCheckGateURL(t *testing.T) {
	for _, raw := range []string{
		"https://gateway.kontra.sh", DefaultGate, "https://gate.team.internal:8443/", "http://localhost:8080",
		"http://127.0.0.1:9999", "http://[::1]:1", "HTTP://LocalHost:8080", "https://10.0.0.1",
	} {
		u, err := CheckGateURL(raw)
		if err != nil || u == nil {
			t.Fatalf("CheckGateURL(%s): %v", raw, err)
		}
	}
	for _, raw := range []string{
		"http://example.com", "http://127.0.0.2:8080", "http://10.0.0.1", "http://localhost.:80",
		"https://u:p@h", "https://u@h", "https://h/?q=1", "https://h/?", "https://h/#frag", "https://h#",
		"ws://localhost:8080", "gate.example", "https://", "https://h:99999", "", "::",
	} {
		if u, err := CheckGateURL(raw); err == nil {
			t.Fatalf("CheckGateURL(%q) = %v", raw, u)
		}
	}
	for _, raw := range []string{"https://user:hunter2@h", "ftp://user:hunter2@h", "http://user:hunter2@example.com/?q#f"} {
		_, err := CheckGateURL(raw)
		if err == nil || strings.Contains(err.Error(), "hunter2") {
			t.Fatalf("a refused --gate with credentials quotes them: %v", err)
		}
	}
}

// FuzzReadPreamble: no panic, never more than one line consumed, and an accepted preamble round-trips.
func FuzzReadPreamble(f *testing.F) {
	f.Add([]byte(`{"v":1,"op":"http","service":"hello","kind":"a2a"}` + "\nGET / HTTP/1.1\r\n\r\n"))
	f.Add([]byte(`{"v":1,"op":"control"}` + "\n"))
	f.Add([]byte(`{"v":1,"op":"blob","ref":"r"}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		r := bufio.NewReaderSize(bytes.NewReader(b), 16)
		p, err := ReadPreamble(r)
		if err != nil {
			return
		}
		nl := bytes.IndexByte(b, '\n')
		if nl < 0 || nl >= MaxPreamble {
			t.Fatalf("accepted a line without '\\n' within %d bytes", MaxPreamble)
		}
		rest, _ := io.ReadAll(r)
		if !bytes.Equal(rest, b[nl+1:]) {
			t.Fatal("bytes after the preamble's line were consumed")
		}
		var buf bytes.Buffer
		if err := WritePreamble(&buf, p); err != nil {
			return // re-encoding may grow past the limit (escapes)
		}
		p2, err := ReadPreamble(bufio.NewReader(&buf))
		if err != nil || p2 != p {
			t.Fatalf("round trip %+v -> %+v, %v", p, p2, err)
		}
	})
}

// FuzzReadControl: no panic, and an accepted line of a known type round-trips.
func FuzzReadControl(f *testing.F) {
	f.Add([]byte(`{"t":"drain"}` + "\n"))
	f.Add([]byte(`{"t":"revoked","service":"hello"}` + "\n"))
	f.Add([]byte(`{"t":"expiring","at":"2026-10-25T12:00:00Z"}` + "\n"))
	f.Add([]byte(`{"t":"zzz","x":[1]}` + "\n\n"))
	f.Fuzz(func(t *testing.T, b []byte) {
		c, err := ReadControl(bufio.NewReader(bytes.NewReader(b)))
		if err != nil || !KnownControl(c.T) {
			return
		}
		var buf bytes.Buffer
		if err := WriteControl(&buf, c); err != nil {
			return
		}
		c2, err := ReadControl(bufio.NewReader(&buf))
		if err != nil || c2.T != c.T || c2.Service != c.Service || c2.Key != c.Key {
			t.Fatalf("round trip %+v -> %+v, %v", c, c2, err)
		}
	})
}
