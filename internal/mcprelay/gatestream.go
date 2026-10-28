package mcprelay

import (
	"bytes"
	"net/http"
	"strings"
)

// GateStream is the relay's gate-served event stream (gateStream, §11.7) for the gate's own /mcp (internal/gatemcp,
// M4 WP-04): a tasks/result response stream, whose events carry id: k.<taskId>.<seq>; a session's GET stream, whose
// events carry no id (relay.md §12.3); and a plain hire's answer, kept alive while it waits. Its headers are written
// once, by Start or the first event; the stream is flushed after each write.
type GateStream struct{ s *gateStream }

// NewGateStream returns a stream on w that names the client's gate Mcp-Session-Id sid ("" for none).
func NewGateStream(w http.ResponseWriter, sid string) *GateStream {
	return &GateStream{s: newGateStream(w, sid)}
}

// Start writes the stream's headers once.
func (g *GateStream) Start() error { return g.s.start() }

// Started reports whether the headers were written.
func (g *GateStream) Started() bool { return g.s.started }

// Event writes one event: with its id unless id is "", and msg as data.
func (g *GateStream) Event(id string, msg []byte) error {
	if id != "" {
		return g.s.event(id, msg)
	}
	if err := g.s.start(); err != nil {
		return err
	}
	var b bytes.Buffer
	for _, line := range strings.Split(lineBreaks.Replace(string(msg)), "\n") {
		b.WriteString("data: ")
		b.WriteString(line)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	if _, err := g.s.w.Write(b.Bytes()); err != nil {
		return err
	}
	return g.s.flush()
}

// KeepAlive writes a comment (spec §2.2), once the stream has started.
func (g *GateStream) KeepAlive() error { return g.s.keepAlive() }
