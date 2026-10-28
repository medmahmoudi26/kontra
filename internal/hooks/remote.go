package hooks

import (
	"context"
	"net/http"
	"time"

	"kontra.sh/kontra/internal/unimpl"
)

// RemoteAdmissionConfig is hooks.admission (spec/relay.md §13.1): the operator's control plane, never derived
// from a request and never a node.
type RemoteAdmissionConfig struct {
	URL        string        // https://, or http:// on a loopback host only
	Secret     string        // the value of secret_env, sent as Authorization: Bearer
	Timeout    time.Duration // default 5 s
	Operations []Operation   // which of task, session, call consult the hook (default task)
	Rules      KeyRules      // §13.2's key and size rules for metadata and mcpMeta
	Client     *http.Client  // no redirects followed; nil: a client of the gate's own making
}

// RemoteAdmission is the remote Admission hook (§13.1; built in M4, DM4-8). It POSTs the AdmitRequest and reads a
// Decision, and fails closed: a timeout, a non-2xx, an unparsable answer, a key outside §13.2's rules or a map over
// 16 KiB is a deny with -32603 "admission unavailable" (HTTP 503). A quote for any operation but task on a
// gate-owned task (an A2A task, a hire, an MCP task with tasks: durable) is a deny with -32603 "priced tasks need
// tasks: durable". An operation the hook does not consult is answered as BuiltinAdmission answers it.
//
// Work package: M4 WP-01.
type RemoteAdmission struct {
	cfg RemoteAdmissionConfig
}

var _ Admission = (*RemoteAdmission)(nil)

// NewRemoteAdmission checks cfg (the URL's scheme and host, a non-empty secret, the operations).
func NewRemoteAdmission(cfg RemoteAdmissionConfig) (*RemoteAdmission, error) {
	return nil, unimpl.Error("hooks.NewRemoteAdmission", "M4 WP-01")
}

// Consults reports whether op is one the hook is asked about.
func (r *RemoteAdmission) Consults(op Operation) bool {
	panic(unimpl.Error("hooks.RemoteAdmission.Consults", "M4 WP-01"))
}

// Admit implements Admission.
func (r *RemoteAdmission) Admit(ctx context.Context, req AdmitRequest) (Decision, error) {
	return Decision{}, unimpl.Error("hooks.RemoteAdmission.Admit", "M4 WP-01")
}
