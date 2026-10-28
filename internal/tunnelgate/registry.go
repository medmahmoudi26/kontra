package tunnelgate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/hashicorp/yamux"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"kontra.sh/kontra/internal/enroll"
	"kontra.sh/kontra/internal/obs"
	"kontra.sh/kontra/internal/pg"
	"kontra.sh/kontra/internal/tunnel"
)

// ErrSessionGone is a session this replica does not hold (closed, superseded, never here), or one whose
// key or binding no longer allows the stream (key_scope, relay.md §6.1): the dispatch fails tunnel_lost
// and no stream is opened.
var ErrSessionGone = errors.New("tunnelgate: the tunnel session is gone or no longer serves the service")

// ErrRevoked wraps ErrSessionGone for a session whose key or binding was revoked, or whose key is past its
// rotation's overlap (relay.md §6.1: a pinned MCP session's row then closes revoked, not tunnel_lost, M3 WP-04).
// For A2A it is a lost session like any other.
var ErrRevoked = fmt.Errorf("%w: its key or binding was revoked", ErrSessionGone)

// ErrCheckUnavailable wraps ErrSessionGone for a key_scope check that could not read the key's revocation state
// (the view not fresh and Postgres not answering): the stream is refused (fail closed), but nothing says the
// session is gone, so a pinned MCP session's row is left open.
var ErrCheckUnavailable = fmt.Errorf("%w: the key's revocation state cannot be read", ErrSessionGone)

// Timing of the registry.
const (
	// CardWait bounds the card fetch through a session that has just connected, before its tunnels row
	// makes it a candidate (relay.md §3; M2 plan audit, gap 3).
	CardWait = 10 * time.Second
	// writeTimeout bounds the registry's own statements (the tunnels row, a revocation's updates).
	writeTimeout = 10 * time.Second
)

// RegistryConfig configures a Registry.
type RegistryConfig struct {
	DB          *pgxpool.Pool
	ReplicaID   string
	Revocations *enroll.Revocations
	Logger      *slog.Logger
	// LastPingEvery is how often last_ping_at is written and the rows are checked (§11):
	// tunnel.LastPingEvery when zero. Tests shorten it.
	LastPingEvery time.Duration
	// CardWait replaces CardWait when positive (tests).
	CardWait time.Duration
}

// Registry is this replica's tunnel sessions (§11, §12, §13).
//
// One session per key per gate (§11). Add runs one transaction under a row lock on the key's tunnel_keys
// row, so the registrations of one key serialize (M2 plan audit): it re-reads the key and the bindings the
// session serves, deletes the rows of the key's older sessions whose replica is not draining, and inserts
// the new row. A superseded session on this replica is closed once the transaction commits; one on another
// replica is told by NOTIFY kontra_tunnel 'session:<id>', and closes at the latest at its replica's next row
// check (Run), which closes every session whose row is gone (superseded, reaped with its replica, or
// deleted with its key or tenant).
//
// "Older" is the order of the handshakes, not of the registrations' commits (M2 review of WP-04): a session
// commits only after its card fetch (up to CardWait), so an older handshake whose fetch is slow (a lost
// welcome's connection, half-open, while the node has already redialed: M2 plan audit gap 1) would
// otherwise commit last and close the node's live session. A session's id is a UUIDv7 minted at its
// handshake (the challenge), so the order is the ids'. A registration that finds a newer session of its key
// on a replica that is not draining inserts nothing and closes. Within a replica the ids are monotonic;
// across replicas they follow the replicas' clocks (NTP), so a skew larger than the time between two
// handshakes of one key can order them wrongly: the wrongly refused session closes, and its node redials.
//
// SPEC-CONFLICT (D138, D199; M2 plan, confirmed conflict on §11 and §5): tunnel.md names no way for one replica to
// act on a session another replica holds (a supersede, an "enrolled" notice): kontra_revoke's payloads cover keys,
// bindings and API keys only. The closest faithful behaviour is a gate-internal NOTIFY channel between
// replicas, pg.ChannelTunnel ('session:<id>', 'enrolled:<key_id>/<tenant_id>/<service>', and since M3
// 'mcp:<tenant_id>/<service>/<replica_id>', a node of an MCP service connecting or disconnecting, which drops the
// service's era on every replica, relay.md §11.12, D177): Postgres between replicas, not tunnel wire protocol, and no
// migration. Without it a superseded session on another replica would serve sticky follow-ups until that replica's
// 60 s row check; the row check stays as the fallback.
type Registry struct {
	cfg   RegistryConfig
	log   *slog.Logger
	every time.Duration
	wait  time.Duration

	mu        sync.Mutex
	sessions  map[uuid.UUID]*Session
	drained   bool // Drain ran: a session added later is told at once
	closing   bool // CloseAll ran: Add refuses
	onConnect []func(ctx context.Context, tenantID uuid.UUID, service string)
	onMCP     []func(tenantID uuid.UUID, service string)

	async sync.WaitGroup // control messages and revocation writes in flight
}

// NewRegistry returns an empty registry.
func NewRegistry(cfg RegistryConfig) *Registry {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	r := &Registry{
		cfg: cfg, log: cfg.Logger.With("component", "tunnel"), every: cfg.LastPingEvery, wait: cfg.CardWait,
		sessions: map[uuid.UUID]*Session{},
	}
	if r.every <= 0 {
		r.every = tunnel.LastPingEvery
	}
	if r.wait <= 0 {
		r.wait = CardWait
	}
	return r
}

// errClosing is Add's refusal once CloseAll has run.
var errClosing = errors.New("tunnelgate: the replica is shutting down its tunnel sessions")

// Add registers a session after its admission (SessionSink). The Handler has sent welcome, opened the
// control stream (s.control) and started the ping loop; Add returns once the session is a candidate for
// streams (its tunnels row committed). In order:
//
//  1. The session joins this replica's registry, not yet a candidate (no row): a revocation from now on
//     reaches it, and the session is removed when mux.CloseChan() closes.
//  2. A served binding the revocation view still holds as revoked (a binding this handshake enrolled
//     again: an enrollment sends no NOTIFY kontra_revoke) makes the view read again
//     (enroll.Revocations.Catchup), so the stream checks do not refuse it for up to 3 s.
//  3. The cards of its a2a services are fetched through it (OnConnect, within CardWait), before it is a
//     candidate: a task placed on it then finds the card (relay.md §3; M2 plan audit, gap 3). A fetch that
//     fails (the node's agent not listening yet: 502 target_unreachable) does not keep the session from
//     becoming a candidate, since it may serve other services; a dispatch for a tunnel service whose
//     keeper has no card yet is a retryable outcome instead (the audit's second option: gate.Services.Backend's
//     relay.ErrCardPending).
//  4. One transaction, under a row lock on the key (§11, §12): the key must still be usable (not revoked,
//     not past a rotation's overlap); the session must still be open (a closed one supersedes nothing) and
//     no newer session of the key may be registered on a replica that is not draining (s is then the
//     superseded one: it inserts nothing); bindings revoked since the admission leave the served set; the
//     rows of the key's older sessions whose replica is not draining are deleted (superseded), with NOTIFY
//     kontra_tunnel 'session:<id>' for those of other replicas; and the tunnels row (session_id, key_id,
//     tenant_id, replica_id, services, hire_principal, cli_version) is inserted.
//  5. After the commit: s is registered (Get finds it) at once, since its row is a candidate on every replica from
//     the commit on; then the superseded sessions of this replica are closed, whatever became of s meanwhile
//     (their rows are gone; closing one closes its pinned MCP sessions in Postgres, relay.md §11.2, which s's
//     registration does not wait for); then s, if it closed while its row went in, has its row deleted again; and
//     a session added after Drain is told {"t": "drain"} (§13).
//
// The enrollments of the session's handshake are not Add's to announce: Admit's transaction sends NOTIFY
// kontra_tunnel 'enrolled:…' (enroll.EnrolledPayload), which every replica applies, this one included (§5),
// so the notice is not lost when the session never registers.
//
// An error closes the session (the Handler closes it): the node has its welcome and redials as after any
// close.
func (r *Registry) Add(ctx context.Context, s *Session, mux *yamux.Session, served []tunnel.WelcomeService) error {
	if mux == nil {
		return errors.New("tunnelgate: Add: no multiplexer")
	}
	s.mu.Lock()
	s.mux, s.reg = mux, r
	if s.served == nil {
		s.served = make(map[string]string, len(served))
		for _, sv := range served {
			s.served[sv.Name] = sv.Kind
		}
	}
	for _, kind := range s.served {
		s.hostsMCP = s.hostsMCP || kind == tunnel.KindMCP
	}
	s.mu.Unlock()

	r.mu.Lock()
	if r.closing {
		r.mu.Unlock()
		return errClosing
	}
	r.sessions[s.ID] = s
	r.mu.Unlock()
	go func() {
		<-mux.CloseChan()
		// The Handler logs the end of a session its node or its ping loop closed.
		_ = s.close("the session closed", false)
	}()
	fail := func(err error) error {
		s.shut(err.Error())
		r.forget(s)
		return err
	}

	if rev := r.cfg.Revocations; rev != nil {
		for _, sv := range served {
			if rev.BindingRevoked(s.KeyID, sv.Name) {
				rev.Catchup()
				break
			}
		}
	}

	r.connected(ctx, s)

	superseded, err := r.insert(ctx, s)
	if err != nil {
		return fail(err)
	}
	s.mu.Lock()
	s.registered = true
	closed := s.closed
	s.mu.Unlock()
	// The commit deleted their rows: they close whatever becomes of s.
	for _, id := range superseded {
		if old := r.lookup(id); old != nil {
			old.mu.Lock()
			old.rowGone = true // deleted by the commit
			old.mu.Unlock()
			_ = old.Close("superseded by session " + s.ID.String() + " of its key (§11)")
		}
	}
	r.mu.Lock()
	closing, drained := r.closing, r.drained
	r.mu.Unlock()
	if closed || closing || mux.IsClosed() {
		// Closed (or CloseAll ran) while the row went in: whoever closed it may have deleted the row before
		// it existed, so take it out again here.
		s.shut("the session closed while it was registered")
		_ = mux.Close()
		r.forget(s)
		if err := r.deleteRow(s.ID); err != nil {
			r.log.Warn("tunnel session closed but its tunnels row was not deleted", obs.KeyTunnelSession, s.ID, "error", err)
		}
		if closing {
			return errClosing
		}
		return fmt.Errorf("%w: closed while it was registered", ErrSessionGone)
	}
	if drained {
		r.tellDrain(s)
	}
	r.mcpChanged(s)
	return nil
}

// OnMCPChange registers fn to run, once per MCP service of the session, when a session serving MCP services becomes a
// candidate (Add) and when it is removed, on this replica and on every other one (NOTIFY kontra_tunnel
// MCPChangedPayload): the MCP relay drops what it learned of those services' upstreams (their era, relay.md §11.12,
// D177), since another node may answer next, and through the peer hop every replica uses the nodes another one holds.
// fn must not block.
func (r *Registry) OnMCPChange(fn func(tenantID uuid.UUID, service string)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onMCP = append(r.onMCP, fn)
}

// mcpChanged runs the OnMCPChange callbacks for each MCP service s serves, then tells the other replicas (NOTIFY
// kontra_tunnel 'mcp:…', which this replica's Notify does not apply a second time). A NOTIFY that is not sent, or
// lost with a replica's LISTEN connection, leaves that replica's era stale for at most its period (RefreshTools);
// the gate drops every era on a LISTEN reconnect.
func (r *Registry) mcpChanged(s *Session) {
	s.mu.Lock()
	var names []string
	for name, kind := range s.served {
		if kind == tunnel.KindMCP {
			names = append(names, name)
		}
	}
	s.mu.Unlock()
	if len(names) == 0 {
		return
	}
	r.runMCP(s.TenantID, names)
	if r.cfg.DB == nil {
		return
	}
	payloads := make([]string, 0, len(names))
	for _, name := range names {
		payloads = append(payloads, MCPChangedPayload(s.TenantID, name, r.cfg.ReplicaID))
	}
	ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()
	if _, err := r.cfg.DB.Exec(ctx, `select pg_notify($1, p) from unnest($2::text[]) as p`, string(pg.ChannelTunnel), payloads); err != nil {
		r.log.Warn("the other replicas were not told of a change in an MCP service's nodes; their eras stay until they expire",
			obs.KeyTunnelSession, s.ID, "error", err)
	}
}

// runMCP runs the OnMCPChange callbacks for each of the tenant's services.
func (r *Registry) runMCP(tenantID uuid.UUID, services []string) {
	r.mu.Lock()
	fns := slices.Clone(r.onMCP)
	r.mu.Unlock()
	for _, name := range services {
		for _, fn := range fns {
			fn(tenantID, name)
		}
	}
}

// insert is Add's transaction; it returns the superseded sessions of this replica.
func (r *Registry) insert(ctx context.Context, s *Session) ([]uuid.UUID, error) {
	if r.cfg.DB == nil {
		return nil, errors.New("tunnelgate: the registry has no database")
	}
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	var local []uuid.UUID
	err := pgx.BeginFunc(ctx, r.cfg.DB, func(tx pgx.Tx) error {
		local = local[:0]
		if _, err := tx.Exec(ctx, `select set_config('statement_timeout', $1, true)`, fmt.Sprint(writeTimeout.Milliseconds())); err != nil {
			return err
		}
		// One session per key per gate (§11): the key's row lock serializes its registrations.
		var (
			revoked bool
			overlap *time.Time
			now     time.Time
		)
		err := tx.QueryRow(ctx, `select revoked_at is not null, overlap_until, statement_timestamp()
			from kontra.tunnel_keys where key_id = $1 and tenant_id = $2 for update`, s.KeyID, s.TenantID).Scan(&revoked, &overlap, &now)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("%w: key %s is gone", ErrSessionGone, s.KeyID)
		case err != nil:
			return err
		case revoked:
			return fmt.Errorf("%w: key %s was revoked", ErrSessionGone, s.KeyID)
		case overlap != nil && !overlap.After(now):
			return fmt.Errorf("%w: key %s is past its rotation's overlap", ErrSessionGone, s.KeyID)
		}
		// A session that closed while it waited (its connection cut during the card fetch) supersedes
		// nothing: its node is gone, and the key's registered session may be the live one.
		if !s.open() {
			return fmt.Errorf("%w: it closed before its row went in", ErrSessionGone)
		}
		// The handshakes' order (see Registry): a newer session of the key, registered on a replica that is
		// not draining, supersedes this one, which inserts nothing.
		var newer uuid.UUID
		err = tx.QueryRow(ctx, `select t.session_id from kontra.tunnels t join kontra.gate_replicas g on g.replica_id = t.replica_id
			where t.key_id = $1 and t.session_id > $2 and g.draining_at is null
			order by t.session_id desc limit 1`, s.KeyID, s.ID).Scan(&newer)
		switch {
		case err == nil:
			return fmt.Errorf("%w: a newer session %s of its key is registered (§11)", ErrSessionGone, newer)
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		// What the session serves: the welcome's services less the bindings revoked since (§5, §12).
		names := s.Served()
		rows, err := tx.Query(ctx, `select service from kontra.key_services
			where key_id = $1 and service = any($2::text[]) and revoked_at is null`, s.KeyID, names)
		if err != nil {
			return err
		}
		active, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		keep := make(map[string]bool, len(active))
		for _, n := range active {
			keep[n] = true
		}
		if dropped := s.keepServed(keep); len(dropped) > 0 {
			r.log.Info("tunnel session: bindings revoked since its admission are not served", obs.KeyTunnelSession, s.ID, "services", dropped)
		}
		// Supersede the key's older sessions, except on a draining replica (make-before-break, §11, §13).
		rows, err = tx.Query(ctx, `delete from kontra.tunnels t using kontra.gate_replicas g
			where t.key_id = $1 and t.session_id < $2 and g.replica_id = t.replica_id and g.draining_at is null
			returning t.session_id, t.replica_id`, s.KeyID, s.ID)
		if err != nil {
			return err
		}
		type old struct {
			id      uuid.UUID
			replica string
		}
		olds, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (old, error) {
			var o old
			err := row.Scan(&o.id, &o.replica)
			return o, err
		})
		if err != nil {
			return err
		}
		for _, o := range olds {
			if o.replica == r.cfg.ReplicaID {
				local = append(local, o.id)
				continue
			}
			if _, err := tx.Exec(ctx, `select pg_notify($1, $2)`, string(pg.ChannelTunnel), SessionPayload(o.id)); err != nil {
				return err
			}
		}
		cli := s.CLIVersion
		if cli == "" {
			cli = "unknown"
		}
		var hire *string
		if s.HirePrincipal != "" {
			hire = &s.HirePrincipal
		}
		if _, err := tx.Exec(ctx, `insert into kontra.tunnels (session_id, key_id, tenant_id, replica_id, services, hire_principal, cli_version)
			values ($1, $2, $3, $4, $5::text[], $6, $7)`, s.ID, s.KeyID, s.TenantID, r.cfg.ReplicaID, s.Served(), hire, cli); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrSessionGone) {
			return nil, err
		}
		return nil, fmt.Errorf("tunnelgate: the tunnels row of session %s: %w", s.ID, err)
	}
	return local, nil
}

// connected runs the OnConnect callbacks for each a2a service the session serves, within the card wait.
func (r *Registry) connected(ctx context.Context, s *Session) {
	r.mu.Lock()
	fns := slices.Clone(r.onConnect)
	r.mu.Unlock()
	if len(fns) == 0 {
		return
	}
	s.mu.Lock()
	var names []string
	for name, kind := range s.served {
		if kind == tunnel.KindA2A {
			names = append(names, name)
		}
	}
	s.mu.Unlock()
	if len(names) == 0 {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, r.wait)
	defer cancel()
	var wg sync.WaitGroup
	for _, name := range names {
		for _, fn := range fns {
			wg.Go(func() { fn(cctx, s.TenantID, name) })
		}
	}
	wg.Wait()
}

// Get returns the session with this id when this replica holds it, it is registered and it is open.
func (r *Registry) Get(id uuid.UUID) (*Session, bool) {
	s := r.lookup(id)
	if s == nil {
		return nil, false
	}
	s.mu.Lock()
	ok := s.registered && !s.closed
	s.mu.Unlock()
	if !ok || (s.mux != nil && s.mux.IsClosed()) {
		return nil, false
	}
	return s, true
}

// lookup is the session with this id in the registry, whatever its state.
func (r *Registry) lookup(id uuid.UUID) *Session {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sessions[id]
}

// ForService returns this replica's open sessions serving the service (a tunnel service's card fetch),
// the newest first (session ids are UUIDv7). A session still being registered is included: its card is
// fetched before it becomes a candidate (Add).
func (r *Registry) ForService(tenantID uuid.UUID, service string) []*Session {
	r.mu.Lock()
	all := make([]*Session, 0, len(r.sessions))
	for _, s := range r.sessions {
		if s.TenantID == tenantID {
			all = append(all, s)
		}
	}
	r.mu.Unlock()
	out := all[:0]
	for _, s := range all {
		if s.open() && s.Serves(service) {
			out = append(out, s)
		}
	}
	slices.SortFunc(out, func(a, b *Session) int { return -bytes.Compare(a.ID[:], b.ID[:]) })
	return out
}

// Serves reports whether this replica holds an open session serving the a2a service: the card keeper of a
// tunnel service refetches through one every 10 minutes while serving (relay.md §3).
func (r *Registry) Serves(tenantID uuid.UUID, service string) bool {
	for _, s := range r.ForService(tenantID, service) {
		if s.allowsLocal(service, tunnel.KindA2A) == nil {
			return true
		}
	}
	return false
}

// Count is the number of sessions this replica holds.
func (r *Registry) Count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.sessions)
}

// all is every session in the registry.
func (r *Registry) all() []*Session {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*Session, 0, len(r.sessions))
	for _, s := range r.sessions {
		out = append(out, s)
	}
	return out
}

// forget removes s from the registry (no row to delete: Add failed before it committed).
func (r *Registry) forget(s *Session) {
	r.mu.Lock()
	if r.sessions[s.ID] == s {
		delete(r.sessions, s.ID)
	}
	r.mu.Unlock()
}

// remove removes a closed session and deletes its tunnels row (once); logged says whether this close is
// the registry's own (it logs it).
func (r *Registry) remove(s *Session, reason string, logged bool) error {
	r.forget(s)
	s.mu.Lock()
	del := !s.rowGone
	s.rowGone = true
	changed := s.hostsMCP && !s.mcpGone
	s.mcpGone = s.mcpGone || s.hostsMCP
	s.mu.Unlock()
	if logged {
		r.log.Info("tunnel session closed", obs.KeyTunnelSession, s.ID, "key_id", s.KeyID, "reason", reason)
	}
	// The MCP sessions pinned to it die with it (relay.md §11.2): revoked when a revocation closed it (its own
	// statement ran first, Revoked), tunnel_lost otherwise. Only a session that hosted an MCP service can have any.
	// The OnMCPChange hooks run for its first removal only (a close and the multiplexer's end both remove it).
	if s.hostedMCP() {
		r.closeMCP(s.ID, "", s.closeReason())
	}
	if changed {
		r.mcpChanged(s)
	}
	if !del {
		return nil
	}
	if err := r.deleteRow(s.ID); err != nil {
		// The session itself is closed; Run's row check deletes the row once it has gone two rounds without
		// a last_ping_at.
		r.log.Warn("tunnel session closed but its tunnels row was not deleted", obs.KeyTunnelSession, s.ID, "error", err)
		s.mu.Lock()
		s.rowGone = false
		s.mu.Unlock()
		return fmt.Errorf("tunnelgate: delete the tunnels row of session %s: %w", s.ID, err)
	}
	return nil
}

// deleteRow deletes a session's tunnels row.
func (r *Registry) deleteRow(id uuid.UUID) error {
	if r.cfg.DB == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()
	_, err := r.cfg.DB.Exec(ctx, `delete from kontra.tunnels where session_id = $1`, id)
	return err
}

// Run keeps the registry's rows current until ctx ends: every LastPingEvery (60 s, §11: not per ping) it
// writes last_ping_at of every registered session in one statement, and closes each session whose row is
// gone (superseded on another replica whose NOTIFY was lost, reaped with this replica's gate_replicas row,
// or deleted with its key or tenant); it also deletes the rows this replica left behind (check). A failed
// statement is logged and closes nothing. When ctx ends it runs CloseAll.
func (r *Registry) Run(ctx context.Context) error {
	t := time.NewTicker(r.every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
			r.CloseAll(cctx)
			cancel()
			return nil
		case <-t.C:
			if err := r.check(ctx); err != nil && ctx.Err() == nil {
				r.log.Warn("tunnel sessions: last_ping_at not written; their rows are checked again next time", "error", err)
			}
		}
	}
}

// check is one round of Run. It also deletes this replica's rows of sessions it no longer holds (a close
// whose delete failed) once they have gone two rounds without a last_ping_at; a session still being
// registered has a fresh row.
func (r *Registry) check(ctx context.Context) error {
	if r.cfg.DB == nil {
		return nil
	}
	// held is never nil: pgx sends a nil slice as NULL, and "not (session_id = any(NULL))" is NULL, which
	// would delete nothing while the replica holds no session (M2 review of WP-04). The SQL coalesces too.
	all := r.all()
	held := make([]uuid.UUID, 0, len(all))
	var ids []uuid.UUID
	byID := map[uuid.UUID]*Session{}
	for _, s := range all {
		held = append(held, s.ID)
		s.mu.Lock()
		ok := s.registered && !s.closed
		s.mu.Unlock()
		if ok {
			ids = append(ids, s.ID)
			byID[s.ID] = s
		}
	}
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	if _, err := r.cfg.DB.Exec(ctx, `delete from kontra.tunnels where replica_id = $1 and not (session_id = any(coalesce($2::uuid[], '{}')))
		and last_ping_at < now() - $3::bigint * interval '1 microsecond'`, r.cfg.ReplicaID, held, (2 * r.every).Microseconds()); err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := r.cfg.DB.Query(ctx, `update kontra.tunnels set last_ping_at = now()
		where replica_id = $1 and session_id = any($2::uuid[]) returning session_id`, r.cfg.ReplicaID, ids)
	if err != nil {
		return err
	}
	alive, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return err
	}
	for _, id := range alive {
		delete(byID, id)
	}
	for id, s := range byID {
		r.log.Info("tunnel session's row is gone: closing it", obs.KeyTunnelSession, id)
		s.mu.Lock()
		s.rowGone = true // nothing to delete
		s.mu.Unlock()
		_ = s.Close("its tunnels row is gone: superseded on another replica, reaped with this replica, or its key or tenant deleted")
	}
	return nil
}

// CloseAll closes every session and deletes their tunnels rows (the end of a drain, or Run's end); a
// session added afterwards is refused. It waits, within ctx, for the control messages and revocation
// writes in flight.
func (r *Registry) CloseAll(ctx context.Context) {
	r.mu.Lock()
	r.closing = true
	all := make([]*Session, 0, len(r.sessions))
	for _, s := range r.sessions {
		all = append(all, s)
	}
	r.mu.Unlock()
	const reason = "the replica is shutting down"
	for _, s := range all {
		if s.shut(reason) {
			r.log.Info("tunnel session closed", obs.KeyTunnelSession, s.ID, "key_id", s.KeyID, "reason", reason)
		}
		s.mu.Lock()
		s.rowGone = true // deleted below, all at once
		s.mu.Unlock()
		if s.mux != nil {
			_ = s.mux.Close()
		}
		r.forget(s)
	}
	if r.cfg.DB != nil {
		if _, err := r.cfg.DB.Exec(ctx, `delete from kontra.tunnels where replica_id = $1`, r.cfg.ReplicaID); err != nil {
			r.log.Warn("tunnel sessions closed but their rows were not deleted; other replicas reap them with this replica's row", "error", err)
		}
		// The MCP sessions pinned to them die with them (relay.md §11.2); replica_id names the holder (D179).
		if _, err := r.cfg.DB.Exec(ctx, `update kontra.mcp_sessions set closed_at = now(), close_reason = 'tunnel_lost'
			where replica_id = $1 and closed_at is null`, r.cfg.ReplicaID); err != nil {
			r.log.Warn("mcp_sessions pinned to this replica's tunnel sessions not closed; the reaper closes them with its row", "error", err)
		}
	}
	done := make(chan struct{})
	go func() { r.async.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
}
