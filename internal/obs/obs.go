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
	"net/url"
	"reflect"
	"regexp"
	"slices"
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

// sensitiveWords name a credential wherever they appear in an attribute key (compared case-insensitively,
// '-' and '.' read as '_'): request credentials, anything that may embed a password, and request headers
// (spec/relay.md §5.3). A key ending in _id is an identifier, never a credential (api_key_id).
var sensitiveWords = []string{
	"authorization", "cookie", "password", "passwd", "secret", "token", "api_key", "apikey", "bearer",
	"credential", "private_key", "dsn", "headers",
}

// sensitiveKeys are whole keys that name a credential-bearing value.
var sensitiveKeys = map[string]bool{"postgres": true, "header": true}

var keyNormalizer = strings.NewReplacer("-", "_", ".", "_", " ", "_")

func sensitive(key string) bool {
	k := keyNormalizer.Replace(strings.ToLower(key))
	if sensitiveKeys[k] {
		return true
	}
	if strings.HasSuffix(k, "_id") {
		return false
	}
	for _, w := range sensitiveWords {
		if strings.Contains(k, w) {
			return true
		}
	}
	return false
}

// NewLogger returns the process logger. Credentials and request headers are never logged
// (spec/relay.md §5.3); callers pass principals by their public id only. As a backstop:
//   - an attribute whose key names a credential (contains authorization, cookie, token, secret, password,
//     api_key, dsn, headers, …) is written as "[redacted]", and so is every attribute inside a group
//     (slog.Group or WithGroup) whose name does, at any depth;
//   - in any string, error or *url.URL value, a URL's password and a password=… parameter are replaced;
//   - a map logged with slog.Any (an http.Header, a map[string]any) is walked the same way.
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

// redact is the handlers' ReplaceAttr. slog calls it for every leaf attribute, never for a group itself,
// with the names of the enclosing groups.
func redact(groups []string, a slog.Attr) slog.Attr {
	if sensitive(a.Key) || slices.ContainsFunc(groups, sensitive) {
		return slog.String(a.Key, Redacted)
	}
	switch a.Value.Kind() {
	case slog.KindString:
		if r := redactText(a.Value.String()); r != a.Value.String() {
			return slog.String(a.Key, r)
		}
	case slog.KindAny:
		if r, changed := redactAny(a.Value.Any(), 0); changed {
			return slog.Any(a.Key, r)
		}
	}
	return a
}

var (
	// urlPassword matches scheme://user:password@ up to the authority's last @.
	urlPassword = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.\-]*://[^\s/?#@:]*):[^\s/?#]*@`)
	// kvPassword matches password=… and sslpassword=… in a keyword/value DSN or a query string.
	kvPassword = regexp.MustCompile(`(?i)\b((?:ssl)?password)=('[^']*'|[^\s&]*)`)
)

// redactText removes URL passwords and password=… parameters from s.
func redactText(s string) string {
	if strings.Contains(s, "://") && strings.Contains(s, "@") {
		s = urlPassword.ReplaceAllString(s, "${1}:"+Redacted+"@")
	}
	if strings.Contains(strings.ToLower(s), "password=") {
		s = kvPassword.ReplaceAllString(s, "${1}="+Redacted)
	}
	return s
}

// redactAny returns v with credentials removed, and whether anything was: errors and URLs by their text,
// maps with string keys and slices element by element (a sensitive map key redacts its value). Other
// values (structs included) are returned unchanged.
func redactAny(v any, depth int) (any, bool) {
	if depth > 8 {
		return v, false
	}
	switch x := v.(type) {
	case nil:
		return v, false
	case string:
		r := redactText(x)
		return r, r != x
	case *url.URL:
		if x != nil && x.User != nil {
			return redactText(x.Redacted()), true
		}
		return v, false
	case url.URL:
		if x.User != nil {
			return redactText(x.Redacted()), true
		}
		return v, false
	case error:
		s := x.Error()
		if r := redactText(s); r != s {
			return r, true
		}
		return v, false
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Map:
		if rv.Type().Key().Kind() != reflect.String {
			return v, false
		}
		out := make(map[string]any, rv.Len())
		changed := false
		for it := rv.MapRange(); it.Next(); {
			k := it.Key().String()
			if sensitive(k) {
				out[k], changed = Redacted, true
				continue
			}
			r, c := redactAny(it.Value().Interface(), depth+1)
			out[k], changed = r, changed || c
		}
		if changed {
			return out, true
		}
	case reflect.Slice, reflect.Array:
		if rv.Type().Elem().Kind() == reflect.Uint8 {
			return v, false
		}
		out := make([]any, rv.Len())
		changed := false
		for i := range rv.Len() {
			r, c := redactAny(rv.Index(i).Interface(), depth+1)
			out[i], changed = r, changed || c
		}
		if changed {
			return out, true
		}
	}
	return v, false
}

// Task returns the attributes every log line about a task attempt carries.
func Task(taskID string, attempt int) []any {
	return []any{slog.String(KeyTaskID, taskID), slog.Int(KeyAttempt, attempt)}
}
