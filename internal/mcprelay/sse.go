package mcprelay

import (
	"bytes"
	"errors"
	"net/http"
	"strings"

	"kontra.sh/kontra/internal/mcpwire"
)

// gateStream is an event stream the gate serves itself (spec/relay.md §11.7): the response stream of a client's
// tasks/result, and a GET resumed with Last-Event-ID k.<taskId>.<n>. Each event carries id: k.<taskId>.<seq> and one
// JSON-RPC message the gate wrote; nothing of a relayed upstream stream is ever written into it, and the gate never
// writes into a relayed stream.
type gateStream struct {
	w       http.ResponseWriter
	rc      *http.ResponseController
	sid     string // the client's gate Mcp-Session-Id, "" for none
	started bool
}

func newGateStream(w http.ResponseWriter, sid string) *gateStream {
	return &gateStream{w: w, rc: http.NewResponseController(w), sid: sid}
}

// start writes the stream's headers once.
func (s *gateStream) start() error {
	if s.started {
		return nil
	}
	s.started = true
	h := s.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Del("Content-Length")
	if s.sid != "" {
		h.Set(mcpwire.HeaderSessionID, s.sid)
	}
	s.w.WriteHeader(http.StatusOK)
	return s.flush()
}

// event writes one event: its id and the message as data, one data line per line of the message (a stored
// message keeps the bytes its writer sent, line breaks included).
func (s *gateStream) event(id string, msg []byte) error {
	if err := s.start(); err != nil {
		return err
	}
	var b bytes.Buffer
	b.WriteString("id: ")
	b.WriteString(id)
	b.WriteByte('\n')
	for _, line := range strings.Split(strings.ReplaceAll(string(msg), "\r\n", "\n"), "\n") {
		b.WriteString("data: ")
		b.WriteString(strings.TrimSuffix(line, "\r"))
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	if _, err := s.w.Write(b.Bytes()); err != nil {
		return err
	}
	return s.flush()
}

// keepAlive writes a comment (spec §2.2), once the stream has started.
func (s *gateStream) keepAlive() error {
	if !s.started {
		return nil
	}
	if _, err := s.w.Write([]byte(keepAlive)); err != nil {
		return err
	}
	return s.flush()
}

func (s *gateStream) flush() error {
	if err := s.rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	return nil
}
