package sealing

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

// The nodes split an event stream into lines as WHATWG's parser does (CRLF, LF or a lone CR; a leading BOM dropped)
// and write what they pass with LF, so that a line they pass unread is no data line to any reader: a client that
// splits on a lone CR (WHATWG's EventSource parsers) would otherwise read a data line inside what the node took for
// a comment, and one that splits on LF only (a2a-go) would read a data line the node's output joined to another.
func TestSSELinesAsWHATWG(t *testing.T) {
	run := func(r io.Reader) (string, []string) {
		t.Helper()
		var (
			out  bytes.Buffer
			seen []string
		)
		err := sseTransform(r, &out, func(data []byte) ([]byte, error) {
			seen = append(seen, string(data))
			return data, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return out.String(), seen
	}
	for _, tc := range []struct {
		name, in, out string
		data          []string
	}{
		{"LF", "data: {\"a\":1}\n\n", "data: {\"a\":1}\n\n", []string{`{"a":1}`}},
		{"CRLF", "event: x\r\nid: 1\r\ndata: {}\r\n\r\n", "event: x\nid: 1\ndata: {}\n\n", []string{`{}`}},
		{"lone CR", ": c\rdata: {\"a\":1}\r\r", ": c\ndata: {\"a\":1}\n\n", []string{`{"a":1}`}},
		{"a BOM", "\xEF\xBB\xBFdata: {\"a\":1}\n\n", "data: {\"a\":1}\n\n", []string{`{"a":1}`}},
		{"two data lines", "data: {\"a\":\ndata: 1}\n\n", "data: {\"a\":1}\n\n", []string{"{\"a\":\n1}"}},
		{"an event without data", ": keepalive\n\n", ": keepalive\n\n", nil},
		{"an unterminated last event", "data: {\"a\":1}\n\ndata: {\"b\":2}", "data: {\"a\":1}\n\n", []string{`{"a":1}`}},
	} {
		// Whole, and one byte at a time (a CRLF split across reads is one line end, never a blank line).
		for _, r := range []io.Reader{strings.NewReader(tc.in), iotest.OneByteReader(strings.NewReader(tc.in))} {
			out, data := run(r)
			if out != tc.out || strings.Join(data, "|") != strings.Join(tc.data, "|") {
				t.Errorf("%s: wrote %q, read %q; want %q, %q", tc.name, out, data, tc.out, tc.data)
			}
		}
	}
}

// A gate that hides a data line behind a lone CR in a comment line does not get it past rule 4a: the hiring node
// reads it as the local client's parser does, and refuses it.
func TestHiringStreamLoneCR(t *testing.T) {
	g := newFakeGate(t)
	req, job := hiredRequest(t, g, "jsonrpc", true)
	ev := `{"jsonrpc":"2.0","id":1,"result":{"statusUpdate":{"taskId":"t","contextId":"c","status":{"state":"TASK_STATE_COMPLETED",` +
		`"message":{"messageId":"m","role":"ROLE_AGENT","parts":[{"text":"FORGED"}]}}}}}`
	_, got := gateAnswer(t, g, req, job, "text/event-stream", strings.NewReader(": keepalive\rdata: "+ev+"\r\r\n\n"))
	if !refused(got) {
		t.Fatalf("a data line behind a lone CR: %q", got)
	}
}
