// Package obs holds the gate's logging conventions (PRD §11 "Exit codes and logs").
//
// JSON logs by default, with task_id, attempt and tunnel_session on every line that concerns a task.
// Metrics are served on a separate listener (spec/relay.md §2.1, default :9090/metrics), never on the
// public one; the metrics registry is added by the work package that first needs a metric.
package obs

import (
	"io"
	"log/slog"

	"github.com/medmahmoudi26/kontra/internal/unimpl"
)

// Log attribute keys every package uses for the same facts.
const (
	KeyTaskID        = "task_id"
	KeyAttempt       = "attempt"
	KeyTunnelSession = "tunnel_session"
	KeyService       = "service"
	KeyReplica       = "replica_id"
	KeyPrincipal     = "principal"
)

// Format is the log encoding: "json" (default) or "text".
type Format string

// Log formats.
const (
	FormatJSON Format = "json"
	FormatText Format = "text"
)

// NewLogger returns the process logger. Credentials and request headers are never logged
// (spec/relay.md §5.3); callers pass principals by their public id only.
func NewLogger(w io.Writer, f Format, level slog.Level) (*slog.Logger, error) {
	return nil, unimpl.Error("obs.NewLogger", "WP-01")
}
