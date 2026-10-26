package tunnel

import (
	"bufio"
	"io"
	"time"

	"kontra.sh/kontra/internal/unimpl"
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

// WriteControl writes c as one JSON line.
func WriteControl(w io.Writer, c Control) error {
	return unimpl.Error("tunnel.WriteControl", "WP-01")
}

// ReadControl reads the next control line; a line of an unknown type is returned as is, for the caller to
// ignore.
func ReadControl(r *bufio.Reader) (Control, error) {
	return Control{}, unimpl.Error("tunnel.ReadControl", "WP-01")
}
