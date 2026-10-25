// Package obs holds the gate's logging conventions (PRD §11 "Exit codes and logs").
//
// JSON logs by default, with task_id, attempt and tunnel_session on every line that concerns a task.
// Metrics are served on a separate listener (spec/relay.md §2.1, default :9090/metrics), never on the
// public one; the metrics registry is added by the work package that first needs a metric.
package obs

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
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

// Redacted replaces the value of an attribute whose key names a credential.
const Redacted = "[redacted]"

// sensitive are attribute keys (compared case-insensitively, '-' read as '_') whose values are never
// written: request credentials and anything that may embed a password (spec/relay.md §5.3).
var sensitive = map[string]bool{
	"authorization": true, "proxy_authorization": true, "cookie": true, "set_cookie": true,
	"password": true, "secret": true, "token": true, "api_key": true, "bearer": true,
	"dsn": true, "postgres": true, "headers": true,
}

// NewLogger returns the process logger. Credentials and request headers are never logged
// (spec/relay.md §5.3); callers pass principals by their public id only. As a backstop, an attribute
// whose key names a credential (authorization, cookie, token, secret, password, api_key, dsn, postgres,
// headers, …) is written as "[redacted]", at any group depth.
func NewLogger(w io.Writer, f Format, level slog.Level) (*slog.Logger, error) {
	opts := &slog.HandlerOptions{Level: level, ReplaceAttr: redact}
	switch f {
	case FormatJSON, "":
		return slog.New(slog.NewJSONHandler(w, opts)), nil
	case FormatText:
		return slog.New(slog.NewTextHandler(w, opts)), nil
	}
	return nil, fmt.Errorf("obs: log format %q: want json or text", f)
}

func redact(_ []string, a slog.Attr) slog.Attr {
	if sensitive[strings.ReplaceAll(strings.ToLower(a.Key), "-", "_")] {
		return slog.String(a.Key, Redacted)
	}
	return a
}

// Task returns the attributes every log line about a task attempt carries.
func Task(taskID string, attempt int) []any {
	return []any{slog.String(KeyTaskID, taskID), slog.Int(KeyAttempt, attempt)}
}
