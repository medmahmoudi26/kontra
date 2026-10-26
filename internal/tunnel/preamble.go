package tunnel

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Op is a preamble's operation (§8.1, §8.3, §13).
type Op string

// Operations.
const (
	OpHTTP    Op = "http"    // one HTTP/1.1 exchange for one service (gate-opened: hosting; node-opened: hiring, M3)
	OpControl Op = "control" // the control stream, once per session, the first stream the gate opens (§13)
	OpBlob    Op = "blob"    // a node-opened blob fetch (§8.3, M3)
)

// Preamble is the one JSON line, at most MaxPreamble bytes with its '\n', that starts every stream
// (§8.1). Unknown fields are ignored; the node never forwards it.
type Preamble struct {
	V       int    `json:"v"`
	Op      Op     `json:"op"`
	Service string `json:"service,omitempty"` // hosting: the hosted service the exchange is for
	Kind    string `json:"kind,omitempty"`    // hosting: its kind
	Task    string `json:"task,omitempty"`    // the gate task the exchange belongs to (sealed headers, ext-sealed.md)
	Context string `json:"context,omitempty"` // the gate context of that task
	// MCPSession and MCPRequestID are MCP's (M3): the hex SHA-256 of the client's gate Mcp-Session-Id, and
	// a tools/call's original JSON-RPC id as a string.
	MCPSession   string `json:"mcp_session,omitempty"`
	MCPRequestID string `json:"mcp_request_id,omitempty"`
	Ref          string `json:"ref,omitempty"` // blob streams (M3)
}

// ErrLineTooLong is a preamble or control line longer than its limit.
var ErrLineTooLong = errors.New("tunnel: line too long")

// WritePreamble writes p as one JSON line, refusing one over MaxPreamble. A zero V is written as Version;
// the op must be one this version knows.
func WritePreamble(w io.Writer, p Preamble) error {
	if p.V == 0 {
		p.V = Version
	}
	if !knownOp(p.Op) {
		return fmt.Errorf("tunnel: preamble: unknown op %q", p.Op)
	}
	b, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("tunnel: preamble: %w", err)
	}
	b = append(b, '\n')
	if len(b) > MaxPreamble {
		return fmt.Errorf("tunnel: preamble of %d bytes, over %d: %w", len(b), MaxPreamble, ErrLineTooLong)
	}
	if _, err := w.Write(b); err != nil {
		return fmt.Errorf("tunnel: preamble: %w", err)
	}
	return nil
}

// ReadPreamble reads one line of at most MaxPreamble bytes and decodes it; a longer line, a v other than
// Version or an unknown op is an error (the node answers it 400 before any target is dialed). It reads
// nothing past the line's '\n', so the HTTP exchange that follows is read from the same r.
func ReadPreamble(r *bufio.Reader) (Preamble, error) {
	line, err := readLine(r, MaxPreamble)
	if err != nil {
		return Preamble{}, fmt.Errorf("tunnel: preamble: %w", err)
	}
	var p Preamble
	if err := json.Unmarshal(line, &p); err != nil {
		return Preamble{}, fmt.Errorf("tunnel: preamble: %w", err)
	}
	if p.V != Version {
		return Preamble{}, fmt.Errorf("tunnel: preamble: v %d, want %d", p.V, Version)
	}
	if !knownOp(p.Op) {
		return Preamble{}, fmt.Errorf("tunnel: preamble: unknown op %q", clip(string(p.Op)))
	}
	return p, nil
}

func knownOp(op Op) bool {
	switch op {
	case OpHTTP, OpControl, OpBlob:
		return true
	}
	return false
}

// readLine reads up to and including the next '\n', which must come within max bytes (the '\n'
// included), and returns the line without it. It never reads past the '\n' from r, whatever r's buffer
// size. An EOF before any byte is io.EOF, after some bytes io.ErrUnexpectedEOF.
func readLine(r *bufio.Reader, max int) ([]byte, error) {
	var line []byte
	for {
		frag, err := r.ReadSlice('\n')
		if len(line)+len(frag) > max {
			return nil, ErrLineTooLong
		}
		line = append(line, frag...)
		switch {
		case err == nil:
			return line[:len(line)-1], nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			if len(line) == 0 {
				return nil, io.EOF
			}
			return nil, io.ErrUnexpectedEOF
		default:
			return nil, err
		}
	}
}
