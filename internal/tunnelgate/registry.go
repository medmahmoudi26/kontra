package tunnelgate

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync"

	"github.com/google/uuid"
	"github.com/hashicorp/yamux"
	"github.com/jackc/pgx/v5/pgxpool"

	"kontra.sh/kontra/internal/enroll"
	"kontra.sh/kontra/internal/tunnel"
	"kontra.sh/kontra/internal/unimpl"
)

// ErrSessionGone is a session this replica does not hold (closed, superseded, never here), or one whose
// key or binding no longer allows the stream (key_scope, relay.md §6.1): the dispatch fails tunnel_lost
// and no stream is opened.
var ErrSessionGone = errors.New("tunnelgate: the tunnel session is gone or no longer serves the service")

// RegistryConfig configures a Registry.
type RegistryConfig struct {
	DB          *pgxpool.Pool
	ReplicaID   string
	Revocations *enroll.Revocations
	Logger      *slog.Logger
}

// Registry is this replica's tunnel sessions (§11, §12, §13).
type Registry struct {
	cfg RegistryConfig

	mu       sync.Mutex
	sessions map[uuid.UUID]*Session
}

// NewRegistry returns an empty registry.
func NewRegistry(cfg RegistryConfig) *Registry {
	return &Registry{cfg: cfg, sessions: map[uuid.UUID]*Session{}}
}

// Session is one live tunnel session on this replica.
type Session struct {
	ID            uuid.UUID
	KeyID         string
	TenantID      uuid.UUID
	Tenant        string
	HirePrincipal string
	CLIVersion    string
	NodeName      string

	mux     *yamux.Session
	mu      sync.Mutex
	served  map[string]string // service name → kind
	control net.Conn          // the control stream, the first one the gate opened
}

// Add registers a session after its admission: it inserts the tunnels row (session_id, key_id, tenant_id,
// replica_id, services, hire_principal, cli_version), supersedes the key's other non-draining session at
// this gate (closing it here, or telling the replica that holds it; its row is deleted in the same
// transaction), opens the control stream, and starts the session's ping loop and last_ping_at updates.
// It returns once the session can carry streams.
func (r *Registry) Add(ctx context.Context, s *Session, mux *yamux.Session, served []tunnel.WelcomeService) error {
	return unimpl.Error("tunnelgate.Registry.Add", "WP-04")
}

// Get returns the session with this id when this replica holds it and it is open.
func (r *Registry) Get(id uuid.UUID) (*Session, bool) {
	panic(unimpl.Error("tunnelgate.Registry.Get", "WP-04"))
}

// ForService returns this replica's open sessions serving the service (a tunnel service's card fetch).
func (r *Registry) ForService(tenantID uuid.UUID, service string) []*Session {
	panic(unimpl.Error("tunnelgate.Registry.ForService", "WP-04"))
}

// Revoked applies one revocation (enroll.Revocations.Subscribe): a key's sessions get {"t": "revoked"} and
// close; a binding's service leaves the served set and tunnels.services, the session's in-flight
// dispatches for it are aborted (tunnel_lost, or retried before the first upstream id), and the node gets
// {"t": "revoked", "service"} (§12). The gate never relies on the node honouring it.
func (r *Registry) Revoked(rev enroll.Revocation) {
	panic(unimpl.Error("tunnelgate.Registry.Revoked", "WP-04"))
}

// Enrolled tells this replica's sessions hosting the binding's service that another key was enrolled for
// it: {"t": "enrolled", "service", "key"} (§5).
func (r *Registry) Enrolled(b enroll.Binding) {
	panic(unimpl.Error("tunnelgate.Registry.Enrolled", "WP-04"))
}

// Notify applies one gate-internal NOTIFY of the tunnel registry (a superseded session to close, an
// enrollment to announce) sent by the replica that ran the handshake (see the M2 plan's spec conflicts:
// tunnel.md names no channel for either).
func (r *Registry) Notify(payload string) {
	panic(unimpl.Error("tunnelgate.Registry.Notify", "WP-04"))
}

// Drain sends {"t": "drain"} on every session's control stream (§13, D35): the nodes dial new sessions at
// once and keep serving the streams on these, which close when CloseAll runs at the end of the drain.
func (r *Registry) Drain(ctx context.Context) {
	panic(unimpl.Error("tunnelgate.Registry.Drain", "WP-04"))
}

// CloseAll closes every session and deletes their tunnels rows (the end of a drain, or Run's end).
func (r *Registry) CloseAll(ctx context.Context) {
	panic(unimpl.Error("tunnelgate.Registry.CloseAll", "WP-04"))
}

// Run keeps the registry's rows current until ctx ends: last_ping_at every tunnel.LastPingEvery, and a
// session whose row is gone (superseded or reaped) is closed.
func (r *Registry) Run(ctx context.Context) error {
	return unimpl.Error("tunnelgate.Registry.Run", "WP-04")
}

// Allows is the key_scope check made before every stream (relay.md §6.1): the session is open, its key is
// neither revoked nor past a rotation's overlap, and the service is in its served set with this kind.
func (s *Session) Allows(service, kind string) error {
	return unimpl.Error("tunnelgate.Session.Allows", "WP-04")
}

// OpenExchange checks Allows, opens a stream and writes its preamble (§8.1); the caller writes one HTTP/1.1
// request on it. A stream that could not be opened, or a session closing, is ErrSessionGone.
func (s *Session) OpenExchange(ctx context.Context, p tunnel.Preamble) (net.Conn, error) {
	return nil, unimpl.Error("tunnelgate.Session.OpenExchange", "WP-04")
}

// Close closes the session with a reason for the log (its dispatches fail tunnel_lost) and deletes its row.
func (s *Session) Close(reason string) error {
	return unimpl.Error("tunnelgate.Session.Close", "WP-04")
}
