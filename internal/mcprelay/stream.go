package mcprelay

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"

	"kontra.sh/kontra/internal/upstream"
)

// event is one server-sent event as the upstream wrote it (WHATWG SSE): its lines, without their terminators and
// without the blank line that ends it, its data (the data lines' values joined by "\n"), and its type (the value of
// its last event field, "" when it has none).
type event struct {
	lines   []string
	data    string
	hasData bool
	typ     string
	size    int
}

// message reports whether a client dispatches the event as a message: its type is absent or "message" (WHATWG SSE's
// default type). Streamable HTTP carries every JSON-RPC message in such events, and a stock client reads no other:
// the TypeScript SDK's streamableHttp dispatches only `!event.event || event.event === "message"`, the Python SDK's
// streamable_http only "message".
func (e *event) message() bool { return e.typ == "" || e.typ == "message" }

// errEventTooLarge is an event over upstream.MaxSSEEvent (spec §6.6).
var errEventTooLarge = fmt.Errorf("%w: one SSE event over %d bytes", upstream.ErrLimit, upstream.MaxSSEEvent)

// sseReader reads events from an upstream's event stream. Lines end in LF, CR or CRLF.
type sseReader struct {
	br  *bufio.Reader
	max int
}

func newSSEReader(r io.Reader) *sseReader {
	return &sseReader{br: bufio.NewReaderSize(r, 32<<10), max: upstream.MaxSSEEvent}
}

// next returns the next event, io.EOF at a clean end (an unterminated last event is dropped, as a client drops
// it), errEventTooLarge over the limit.
func (s *sseReader) next() (*event, error) {
	ev := &event{}
	var data []string
	for {
		line, err := s.line(ev)
		if err != nil {
			if errors.Is(err, io.EOF) && len(ev.lines) > 0 {
				return nil, io.ErrUnexpectedEOF
			}
			return nil, err
		}
		if line == "" {
			if len(ev.lines) == 0 {
				continue // blank lines between events
			}
			ev.data = strings.Join(data, "\n")
			return ev, nil
		}
		ev.lines = append(ev.lines, line)
		name, value, _ := strings.Cut(line, ":")
		switch name {
		case "data":
			ev.hasData = true
			data = append(data, strings.TrimPrefix(value, " "))
		case "event":
			ev.typ = strings.TrimPrefix(value, " ")
		}
	}
}

// line reads one line of the current event, counting it against the event's limit.
func (s *sseReader) line(ev *event) (string, error) {
	var b []byte
	for {
		c, err := s.br.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) && len(b) > 0 {
				return "", io.ErrUnexpectedEOF
			}
			return "", err
		}
		switch c {
		case '\n':
			return string(b), nil
		case '\r':
			if n, err := s.br.Peek(1); err == nil && n[0] == '\n' {
				_, _ = s.br.ReadByte()
			}
			return string(b), nil
		}
		b = append(b, c)
		if ev.size++; ev.size > s.max {
			return "", errEventTooLarge
		}
	}
}

// encode writes the event back, its data replaced by data when replace is set: the other lines (id, event, retry,
// comments) as they came, in order, and the data where its first line was, one data line per line of data.
func (e *event) encode(replace bool, data string) []byte {
	var b bytes.Buffer
	wrote := false
	for _, l := range e.lines {
		if replace && (l == "data" || strings.HasPrefix(l, "data:")) {
			if wrote {
				continue
			}
			wrote = true
			for _, dl := range strings.Split(data, "\n") {
				b.WriteString("data: ")
				b.WriteString(dl)
				b.WriteByte('\n')
			}
			continue
		}
		b.WriteString(l)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	return b.Bytes()
}

// keepAlive is the comment the gate writes on an event stream it relays when nothing has been written for
// keepAliveEvery (spec §2.2): not an event, so the gate writes no message into a relayed stream (§11.7).
const keepAlive = ": keep-alive\n\n"
