package tunnel

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// ControlType is a control stream message's "t" (§13, D35). The control stream carries JSON lines, gate to
// node only; the node answers nothing on it and ignores types it does not know.
type ControlType string

// Control messages.
const (
	CtlDrain       ControlType = "drain"        // this replica is shutting down: dial a new session at once (make-before-break)
	CtlRevoked     ControlType = "revoked"      // the key (no Service) or one binding (Service) was revoked (§12)
	CtlAuthRevoked ControlType = "auth_revoked" // the session's hiring API key was revoked; hosting goes on
	CtlEnrolled    ControlType = "enrolled"     // another key was enrolled for a service this node hosts (§5)
	CtlExpiring    ControlType = "expiring"     // an anonymous session ends at At (§6, M4)
)

// Control is one control line.
type Control struct {
	T       ControlType `json:"t"`
	Service string      `json:"service,omitempty"`
	Key     string      `json:"key,omitempty"`
	At      *time.Time  `json:"at,omitempty"`
}

// MaxControlLine bounds one control line on the reading side. §13's messages are a few dozen bytes; the
// bound only stops a peer from making the node buffer without limit (tunnel.md names none).
const MaxControlLine = MaxHandshakeMessage

// WriteControl writes c as one JSON line.
func WriteControl(w io.Writer, c Control) error {
	if c.T == "" {
		return errors.New("tunnel: control: no type")
	}
	b, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("tunnel: control: %w", err)
	}
	b = append(b, '\n')
	if len(b) > MaxControlLine {
		return fmt.Errorf("tunnel: control line of %d bytes: %w", len(b), ErrLineTooLong)
	}
	if _, err := w.Write(b); err != nil {
		return fmt.Errorf("tunnel: control: %w", err)
	}
	return nil
}

// ReadControl reads the next control line; a line of an unknown type is returned as is, for the caller to
// ignore.
//
// Only "t" of an unknown type is read: a later version may give its own messages fields of any shape.
// A known type whose fields are of the wrong shape is an error. Blank lines are skipped. The end of the
// stream is io.EOF.
func ReadControl(r *bufio.Reader) (Control, error) {
	for {
		line, err := readLine(r, MaxControlLine)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return Control{}, io.EOF
			}
			return Control{}, fmt.Errorf("tunnel: control: %w", err)
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var head struct {
			T ControlType `json:"t"`
		}
		if err := json.Unmarshal(line, &head); err != nil {
			return Control{}, fmt.Errorf("tunnel: control: %w", err)
		}
		if !KnownControl(head.T) {
			return Control{T: head.T}, nil
		}
		var c Control
		if err := json.Unmarshal(line, &c); err != nil {
			return Control{}, fmt.Errorf("tunnel: control %s: %w", head.T, err)
		}
		return c, nil
	}
}

// KnownControl reports whether this version acts on a control type (§13); the node ignores the others.
func KnownControl(t ControlType) bool {
	switch t {
	case CtlDrain, CtlRevoked, CtlAuthRevoked, CtlEnrolled, CtlExpiring:
		return true
	}
	return false
}
