// Package gate assembles one kontra-gate replica: configuration, Postgres, the LISTEN connection,
// Temporal workers, the relay and the public listener (spec/relay.md §2, §14; spec/tunnel.md §11).
package gate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/workqueue"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.temporal.io/sdk/client"

	"kontra.sh/kontra/internal/a2ahttp"
	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/card"
	"kontra.sh/kontra/internal/config"
	"kontra.sh/kontra/internal/edge"
	"kontra.sh/kontra/internal/enroll"
	"kontra.sh/kontra/internal/hooks"
	"kontra.sh/kontra/internal/ids"
	"kontra.sh/kontra/internal/jws"
	"kontra.sh/kontra/internal/lifecycle"
	"kontra.sh/kontra/internal/migrate"
	"kontra.sh/kontra/internal/pg"
	"kontra.sh/kontra/internal/push"
	"kontra.sh/kontra/internal/relay"
	"kontra.sh/kontra/internal/sse"
	"kontra.sh/kontra/internal/ssrf"
	"kontra.sh/kontra/internal/store"
	"kontra.sh/kontra/internal/tunnel"
	"kontra.sh/kontra/internal/tunnelgate"
	"kontra.sh/kontra/internal/version"
)

// ErrDrain is wrapped by the cause of the context `kontra-gate serve` passes to Run when SIGTERM or
// SIGINT arrives (context.Cause(ctx)). The context is canceled at that moment, so Run does its drain
// writes (draining_at, the gate_replicas heartbeat, stopping workers) on context.WithoutCancel(ctx).
var ErrDrain = errors.New("drain")

// Timing of the replica registry (spec/tunnel.md §11) and of the readiness check (spec §2.1).
const (
	// BeatEvery is how often a replica upserts gate_replicas.last_seen_at and deletes the rows of replicas
	// silent for more than ReplicaSilence.
	BeatEvery      = 10 * time.Second
	ReplicaSilence = 30 * time.Second
	// HealthEvery is how often the readiness check pings Postgres and Temporal; HealthTimeout bounds each.
	HealthEvery   = 5 * time.Second
	HealthTimeout = 3 * time.Second
	// CardWait is how long a starting replica waits for its direct services' first cards before its
	// workers poll (Services.WaitCards).
	CardWait = 10 * time.Second
	// ServeGrace is how long, after the workers have drained, the public listener waits for requests in
	// flight (SSE streams, long polls) before it closes their connections: a caller resumes on another
	// replica with Last-Event-ID (spec §9).
	ServeGrace = 5 * time.Second
	// TemporalDialTimeout bounds the Temporal connection at start.
	TemporalDialTimeout = 30 * time.Second
	// DrainMarkRetry is how often the drain retries writing gate_replicas.draining_at when it fails: the
	// tunnel nodes are told to reconnect elsewhere only once it has committed (M2 plan audit).
	DrainMarkRetry = time.Second
	// DrainMarkTimeout bounds one attempt at that write (a row held by another transaction, a Postgres that
	// stopped answering); the write is also abandoned once the workers have drained.
	DrainMarkTimeout = 5 * time.Second
)

// Option configures a replica.
type Option func(*options)

type options struct {
	logger *slog.Logger
	reload func() (*config.Config, error)
	// timing (tests shorten it)
	beatEvery, silence, healthEvery, servicesEvery, cardWait, serveGrace time.Duration
	orphanEvery, orphanAge, retentionEvery                               time.Duration
	tunnelPing, tunnelCards                                              time.Duration
	executor                                                             relay.ExecutorOptions
	hup                                                                  bool
	hupCh                                                                <-chan os.Signal
}

// WithLogger sets the replica's logger (default slog.Default()).
func WithLogger(l *slog.Logger) Option { return func(o *options) { o.logger = l } }

// WithReload sets how SIGHUP reads the configuration again: `kontra-gate serve` loads the file it started
// from, with the same per-replica overrides. Without it SIGHUP is logged and ignored.
func WithReload(fn func() (*config.Config, error)) Option { return func(o *options) { o.reload = fn } }

// Timing replaces the replica's intervals; a zero field keeps its default (tests).
type Timing struct {
	Beat, Silence, Health, ServicesRefresh, CardWait, ServeGrace time.Duration
	OrphanEvery, OrphanAge, RetentionEvery                       time.Duration
	// TunnelPing is the tunnel registry's last_ping_at interval and row check (tunnel.LastPingEvery);
	// TunnelCards a tunnel card's refetch while serving (card.Refresh).
	TunnelPing, TunnelCards time.Duration
	Executor                relay.ExecutorOptions
}

// WithTiming replaces the replica's intervals (tests).
func WithTiming(t Timing) Option {
	return func(o *options) {
		set := func(dst *time.Duration, v time.Duration) {
			if v > 0 {
				*dst = v
			}
		}
		set(&o.beatEvery, t.Beat)
		set(&o.silence, t.Silence)
		set(&o.healthEvery, t.Health)
		set(&o.servicesEvery, t.ServicesRefresh)
		set(&o.cardWait, t.CardWait)
		set(&o.serveGrace, t.ServeGrace)
		set(&o.orphanEvery, t.OrphanEvery)
		set(&o.orphanAge, t.OrphanAge)
		set(&o.retentionEvery, t.RetentionEvery)
		set(&o.tunnelPing, t.TunnelPing)
		set(&o.tunnelCards, t.TunnelCards)
		o.executor = t.Executor
	}
}

// WithoutSIGHUP leaves SIGHUP alone (tests that run several replicas in one process call Reload instead).
func WithoutSIGHUP() Option { return func(o *options) { o.hup = false } }

// WithSIGHUP makes Run reload on the signals ch delivers, which the caller registered for SIGHUP
// (signal.Notify) before New. Without it Run registers SIGHUP only once it starts, and until then SIGHUP
// keeps its default action, which ends the process: a reload sent during New (the migration check, up to
// TemporalDialTimeout of Temporal dial, the services sync) would stop the gate. A SIGHUP that arrives
// before Run is applied once Run starts.
func WithSIGHUP(ch <-chan os.Signal) Option {
	return func(o *options) { o.hup, o.hupCh = true, ch }
}

// Replica is one gate process. Its replica id is unique per process start; it is inserted into
// gate_replicas before any worker polls, upserted every 10 s, and every 10 s each replica deletes the rows
// of replicas silent for more than 30 s (spec/tunnel.md §11).
type Replica struct {
	cfg  *config.Config
	id   string
	log  *slog.Logger
	opts options

	pool      *pgxpool.Pool
	storeLog  *store.Log
	listener  *pg.Listener
	revoked   *auth.RevocationCache
	keys      *auth.APIKeys
	temporal  client.Client
	lifecycle *lifecycle.Client
	services  *Services
	handlers  *relay.Handlers
	dispatch  *countingDispatch
	workers   *lifecycle.Workers
	server    *edge.Server
	metrics   *edge.Metrics

	// The tunnel (spec/tunnel.md): the replica's view of revoked node keys and bindings, the enrollment
	// tables, and the registry of the sessions this replica holds.
	nodeKeys *enroll.Revocations
	enroll   *enroll.Store
	tunnels  *tunnelgate.Registry

	// Card signing (spec §3, D166): the key set, nil on a gate off loopback without signing_key (which serves no
	// a2a service).
	signingKeys *jws.KeySet

	refusals   lifecycle.PatchRefusals // operator patches refused (spec §13.3's metric)
	draining   atomic.Bool
	pgUp       atomic.Bool
	temporalUp atomic.Bool
	ran        atomic.Bool
	orphanFrom store.OrphanCandidate // the orphan sweep's cursor (RunSweeps' goroutine only)

	cfgMu sync.Mutex // cfg's services: block, replaced by Reload

	// regMu orders the heartbeat (beat) and deregister: a beat in flight ends before the row is deleted,
	// and once deregistered no beat inserts it again (the heartbeat loop runs until Run returns).
	regMu        sync.Mutex
	deregistered bool
}

// New connects everything a replica needs and checks that the schema is at the embedded version (the
// gate never migrates implicitly): the pool, the Temporal client, the service registry (the team tenant
// and the services: block are upserted, Services.Sync), the relay, the workers (not yet polling), the
// public and metrics listeners (bound, not yet serving), and the replica's gate_replicas row. Run serves.
func New(ctx context.Context, cfg *config.Config, opts ...Option) (r *Replica, err error) {
	o := options{
		logger: slog.Default(), beatEvery: BeatEvery, silence: ReplicaSilence, healthEvery: HealthEvery,
		servicesEvery: RefreshEvery, cardWait: CardWait, serveGrace: ServeGrace,
		orphanEvery: OrphanInterval, orphanAge: OrphanAge, retentionEvery: RetentionInterval, hup: true,
	}
	for _, opt := range opts {
		opt(&o)
	}
	log := o.logger

	m, err := migrate.Open(ctx, cfg.Postgres)
	if err != nil {
		return nil, err
	}
	err = m.Check(ctx)
	m.Close()
	if err != nil {
		return nil, err
	}

	host, _ := os.Hostname()
	id, err := ids.NewReplicaID(host)
	if err != nil {
		return nil, err
	}
	r = &Replica{cfg: cfg, id: id, log: log.With("replica", id), opts: o}
	defer func() {
		if err != nil {
			// Release what New bound or opened: the listeners (closed unserved), the client and the pool.
			if r.server != nil {
				_ = r.server.Shutdown(context.WithoutCancel(ctx))
			}
			if r.metrics != nil {
				_ = r.metrics.Shutdown(context.WithoutCancel(ctx))
			}
			r.close()
		}
	}()

	if r.pool, err = pg.NewPool(ctx, cfg.Postgres); err != nil {
		return nil, err
	}
	if err = r.pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("gate: postgres: %w", err)
	}
	r.pgUp.Store(true)
	dctx, cancel := context.WithTimeout(ctx, TemporalDialTimeout)
	r.temporal, err = lifecycle.Dial(dctx, cfg.Temporal.Address, cfg.Temporal.Namespace)
	cancel()
	if err != nil {
		return nil, err
	}
	r.temporalUp.Store(true)
	if r.signingKeys, err = loadSigningKeys(cfg, r.log); err != nil {
		return nil, err
	}
	if err = r.assemble(); err != nil {
		return nil, err
	}
	if err = r.services.Sync(ctx, cfg); err != nil {
		return nil, err
	}
	if _, err = r.server.Listen(); err != nil {
		return nil, err
	}
	if _, err = r.metrics.Listen(); err != nil {
		return nil, err
	}
	if err = r.register(ctx); err != nil {
		return nil, err
	}
	r.log.Info("replica ready to serve", "version", version.Get(), "listen", r.Addr().String(),
		"services", r.services.Count(), "tenancy", cfg.Tenancy)
	return r, nil
}

// assemble builds the replica's parts over its pool and Temporal client: the wiring of every package's
// notes (the M1 handoff).
func (r *Replica) assemble() error {
	cfg, log := r.cfg, r.log
	poolCfg := r.pool.Config().ConnConfig
	r.listener = pg.NewListener(poolCfg, pg.WithLogger(log))
	r.storeLog = store.NewLog(r.pool)
	r.revoked = auth.NewRevocationCache(r.pool)
	r.listener.OnChannel(pg.ChannelRevoke, r.revoked.Notify)
	r.listener.OnReconnect(r.revoked.Catchup)
	if cfg.Auth.APIKeys {
		r.keys = auth.NewAPIKeys(r.pool, r.revoked)
	}
	authn := auth.NewAuthenticator(r.keys) // nil keys: every bearer credential is refused (auth.api_keys false)
	r.lifecycle = lifecycle.NewClient(r.temporal)

	// The tunnel (spec/tunnel.md §11–§13): node-key and binding revocations reach the registry by NOTIFY
	// kontra_revoke (beside the API key cache), a re-read every 3 s and a catch-up on every LISTEN reconnect;
	// the registry's own gate-internal NOTIFY kontra_tunnel carries supersedes and enrollments between
	// replicas.
	r.nodeKeys = enroll.NewRevocations(r.pool, enroll.WithRevocationsLogger(log))
	r.listener.OnChannel(pg.ChannelRevoke, r.nodeKeys.Notify)
	r.listener.OnReconnect(r.nodeKeys.Catchup)
	r.tunnels = tunnelgate.NewRegistry(tunnelgate.RegistryConfig{
		DB: r.pool, ReplicaID: r.id, Revocations: r.nodeKeys, Logger: log, LastPingEvery: r.opts.tunnelPing,
	})
	r.nodeKeys.Subscribe(r.tunnels.Revoked)
	r.listener.OnChannel(pg.ChannelTunnel, r.tunnels.Notify)
	r.enroll = enroll.NewStore(r.pool, enroll.Options{
		PublicURL: cfg.PublicURL, Multi: cfg.Tenancy == config.TenancyMulti,
		EnrollWithAPIKey: cfg.Tunnel.EnrollWithAPIKey, Keys: r.keys, Logger: log,
	})

	policy := ssrf.Policy{AllowPrivate: cfg.Push.AllowPrivate, Resolver: pushResolver(log)}
	pushStore := push.NewConfigStore(r.pool, r.lifecycle, policy, log)
	rules := hooks.KeyRules{OperatorPrefixes: cfg.OperatorMetadataPrefixes, CallerMetadata: cfg.CallerMetadata}
	hookQueue := lifecycle.QueueLifecycle
	if cfg.Hooks.Lifecycle != nil && cfg.Hooks.Lifecycle.TaskQueue != "" {
		hookQueue = cfg.Hooks.Lifecycle.TaskQueue
	}
	snapshot := lifecycle.HooksSnapshot{LifecycleQueue: hookQueue, OperatorPrefixes: cfg.OperatorMetadataPrefixes}

	var signer card.Signer
	if r.signingKeys != nil {
		signer = &card.JWSSigner{Keys: r.signingKeys, JWKSURL: strings.TrimRight(cfg.PublicURL, "/") + jws.JWKSPath}
	}

	var handlers *relay.Handlers
	r.services = NewServices(r.pool, ServicesOptions{
		PublicURL: cfg.PublicURL, Multi: cfg.Tenancy == config.TenancyMulti, Logger: log, Signer: signer,
		// seller_keys {url} sets: the SSRF dialer with its address checks always on (spec §3, §10.3).
		SellerKeysClient: ssrf.NewClient(ssrf.Policy{Resolver: pushResolver(log)}),
		OnChange: func(ref store.ServiceRef) {
			if handlers != nil {
				handlers.Invalidate(ref)
			}
		},
		Tunnels: r.tunnels, Sessions: r.tunnels, TunnelCardRefresh: r.opts.tunnelCards,
	})
	r.listener.OnChannel(pg.ChannelServices, r.services.Notify)
	r.listener.OnReconnect(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := r.services.Reload(ctx); err != nil {
			log.Warn("services: re-read after the LISTEN connection reconnected failed", "error", err)
		}
	})

	registry := relay.NewRegistry()
	executor := r.opts.executor
	executor.TaskBytes = cfg.Limits.TaskBytes.Int64()
	handlers = relay.NewHandlers(relay.HandlerDeps{
		Log:       r.storeLog,
		Puller:    store.NewPullerProvider(r.storeLog, r.listener),
		PushStore: pushStore,
		Queue: func(svc store.ServiceRef) workqueue.Queue {
			return lifecycle.NewQueue(svc, r.storeLog, r.lifecycle, lifecycle.QueueOptions{
				Owner: relay.CallerOwner, Push: pushStore, Hooks: snapshot,
			})
		},
		Interceptors: func(svc store.ServiceRef) []a2asrv.CallInterceptor {
			i := &relay.Interceptors{Svc: svc, Log: r.storeLog, Rules: rules, Canceler: r.lifecycle,
				Reviews: r.lifecycle, PushPolicy: policy}
			return i.Chain()
		},
		Registry: registry,
		Backends: r.services,
		Signals:  r.lifecycle,
		Logger:   log,
		Executor: executor,
	})
	r.handlers = handlers
	r.dispatch = &countingDispatch{DispatchActivities: &relay.Dispatcher{
		ReplicaID: r.id, Log: r.storeLog, Registry: registry, Handlers: handlers, Backends: r.services,
		Signals: r.lifecycle, Logger: log,
	}}
	deliverer := push.NewDeliverer(r.storeLog, policy, push.DelivererOptions{Waker: r.listener, Logger: log})
	// prepare places a tunnel service's attempts on the sessions of every replica (spec §6.1); the
	// dispatch runs on kontra-tunnel-<the session's replica> (§14.2).
	acts := &lifecycle.LifecycleActivities{Log: r.storeLog, Backends: r.services, Sessions: tunnelgate.NewPicker(r.pool),
		Push: pushStore, Rules: rules, Refusals: &r.refusals, Logger: log}
	var err error
	r.workers, err = lifecycle.NewWorkers(r.temporal, lifecycle.WorkerConfig{
		ReplicaID:     r.id,
		DispatchSlots: cfg.Limits.DispatchSlots,
		PushSlots:     cfg.Limits.PushSlots,
		DrainTimeout:  cfg.DrainTimeout.D(),
		RemoteHooks:   cfg.Hooks.Lifecycle != nil,
	}, lifecycle.Activities{
		Lifecycle: acts,
		Dispatch:  r.dispatch,
		Push:      deliverer,
		Hooks:     hooks.BuiltinLifecycle{},
	})
	if err != nil {
		return err
	}

	writer := sse.NewWriterWithOptions(r.storeLog, r.listener, sse.Options{Logger: log})
	routes := edge.NewRouter()
	a2ahttp.New(a2ahttp.Deps{
		Services: r.services, Handlers: handlers, SSE: writer, Auth: authn,
		Multi: cfg.Tenancy == config.TenancyMulti, Logger: log,
	}).Register(routes)
	routes.Handle("GET /healthz", edge.Healthz(r.Ready))
	// GET /.well-known/jwks.json (spec §2.1, D166): the card-signing public keys, every key of the file.
	if r.signingKeys != nil {
		jwks, err := card.JWKSHandler(r.signingKeys)
		if err != nil {
			return err
		}
		routes.Handle("GET "+jws.JWKSPath, jwks)
	}
	// GET /v1/tunnel (spec §2.1, tunnel.md §3): the node's WebSocket upgrade. A draining replica refuses new
	// tunnels (relay.md §14.3).
	th, err := tunnelgate.NewHandler(tunnelgate.HandlerConfig{
		PublicURL: cfg.PublicURL, ChannelBinding: cfg.Tunnel.ChannelBinding, ReplicaID: r.id,
		Admitter: r.enroll, Registry: r.tunnels, Draining: r.draining.Load, Logger: log,
	})
	if err != nil {
		return err
	}
	routes.Handle("GET "+tunnel.Path, th)
	ecfg, err := edge.FromConfig(cfg, log)
	if err != nil {
		return err
	}
	if r.server, err = edge.New(ecfg, routes); err != nil {
		return err
	}
	r.metrics = edge.NewMetrics(cfg.Metrics.Listen, http.HandlerFunc(r.serveMetrics))
	return nil
}

// loadSigningKeys is the card-signing key set (spec §3, D166): the PEM file signing_key names (a file the gate
// cannot use refuses the start); without one, on a loopback public_url, a key minted at start, logged with its
// kid (one replica, development: another replica's cards would not verify against this one's JWKS); off
// loopback, none, and the registry leaves every a2a service out (the configuration refuses a file that names
// one).
func loadSigningKeys(cfg *config.Config, log *slog.Logger) (*jws.KeySet, error) {
	if p, ok := strings.CutPrefix(cfg.SigningKey, "file:"); ok {
		keys, err := jws.LoadKeySet(p)
		if err != nil {
			return nil, fmt.Errorf("gate: signing_key: %w", err)
		}
		kid, _ := keys.KeyID()
		log.Info("card-signing keys loaded", "kid", kid, "keys", len(keys.KeyIDs()))
		return keys, nil
	}
	if cfg.SigningKey != "" {
		return nil, fmt.Errorf("gate: signing_key: want file:<path> (D166)")
	}
	u, err := url.Parse(cfg.PublicURL)
	if err != nil || !config.IsLocalhost(u.Hostname()) {
		log.Warn("no signing_key: this gate serves no a2a service (their cards must be signed, D166)")
		return nil, nil
	}
	keys, err := jws.EphemeralKeySet()
	if err != nil {
		return nil, err
	}
	kid, _ := keys.KeyID()
	log.Warn("no signing_key: the served cards are signed with a key minted at start, which only this process's "+
		"JWKS lists (a loopback gate of one replica, for development; set signing_key: file:<path>, D166)", "kid", kid)
	return keys, nil
}

// ID is the replica id.
func (r *Replica) ID() string { return r.id }

// Addr is the public listener's bound address (New binds it).
func (r *Replica) Addr() net.Addr {
	a, _ := r.server.Listen()
	return a
}

// MetricsAddr is the metrics listener's bound address.
func (r *Replica) MetricsAddr() net.Addr {
	a, _ := r.metrics.Listen()
	return a
}

// Services is the replica's service registry.
func (r *Replica) Services() *Services { return r.services }

// Tunnels is the replica's tunnel session registry.
func (r *Replica) Tunnels() *tunnelgate.Registry { return r.tunnels }

// Draining reports whether the replica has begun its drain.
func (r *Replica) Draining() bool { return r.draining.Load() }

// Ready is /healthz (spec §2.1): Postgres reachable (the last ping and the LISTEN connection), Temporal
// reachable (the last health check), and the replica not draining. It does no I/O: the checks run every
// HealthEvery.
func (r *Replica) Ready() bool {
	return !r.draining.Load() && r.pgUp.Load() && r.listener.Healthy() && r.temporalUp.Load()
}

// InFlight is the number of dispatches running on this replica.
func (r *Replica) InFlight() int64 { return r.dispatch.n.Load() }

// Run serves until ctx ends. On SIGTERM (ctx canceled by the caller with the drain cause) it drains:
// gate_replicas.draining_at, /healthz 503, workers drained (lifecycle.Workers.Drain), and exit once no
// dispatch is in flight or drain_timeout passes (spec §14.3). On SIGHUP it reloads services from the file.
//
// Order at start: the LISTEN connection, the revocation cache, the card keepers (with a short wait for the
// first cards, CardWait), the heartbeat, the readiness check, the sweeps, then the workers (the replica's
// row is in gate_replicas since New), and last the listeners. The drain (spec §14.3, D76): the replica is
// marked draining (gate_replicas.draining_at, /healthz 503) and the workers drain, kontra-direct at once and
// the others only after its last dispatch has ended or drain_timeout passed, while the public listener
// still serves (on a gate of one replica a caller can still cancel); then the public listener stops,
// waiting ServeGrace for its streams, the replica's row is deleted, which ends its heartbeat (deregister),
// and the background loops stop. Run returns nil after a drain, or the error of a listener that failed
// (after draining all the same).
func (r *Replica) Run(ctx context.Context) error {
	if !r.ran.CompareAndSwap(false, true) {
		return errors.New("gate: Run called twice")
	}
	bg, stopBG := context.WithCancel(context.WithoutCancel(ctx))
	var wg sync.WaitGroup
	goLoop := func(name string, fn func(context.Context) error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := fn(bg); err != nil && bg.Err() == nil {
				r.log.Error("background loop ended", "loop", name, "error", err)
			}
		}()
	}
	defer func() {
		stopBG()
		wg.Wait()
		r.close()
	}()

	goLoop("listen", r.listener.Run)
	goLoop("revocation", r.revoked.Run)
	goLoop("node revocations", r.nodeKeys.Run)
	goLoop("tunnels", r.tunnels.Run)
	goLoop("services", func(ctx context.Context) error { r.services.Run(ctx, r.opts.servicesEvery); return nil })
	goLoop("replicas", r.replicaLoop)
	r.checkHealth(bg)
	goLoop("health", r.healthLoop)
	goLoop("sweeps", r.RunSweeps)
	if r.opts.hup {
		goLoop("sighup", r.hupLoop)
	}

	wctx, cancel := context.WithTimeout(ctx, r.opts.cardWait)
	if missing := r.services.WaitCards(wctx); len(missing) > 0 {
		r.log.Warn("starting without the cards of some direct services; their keepers keep fetching", "services", missing)
	}
	cancel()

	if err := r.workers.Start(); err != nil {
		// Nothing polls and nothing is served: release the bound listeners and the replica's row.
		dctx := context.WithoutCancel(ctx)
		_ = r.server.Shutdown(dctx)
		_ = r.metrics.Shutdown(dctx)
		if derr := r.deregister(dctx); derr != nil {
			r.log.Warn("gate_replicas row not deleted; other replicas reap it after 30 s", "error", derr)
		}
		return err
	}
	serveErr := make(chan error, 2)
	go func() { serveErr <- r.server.Serve(bg) }()
	go func() { serveErr <- r.metrics.Serve(bg) }()
	r.log.Info("serving", "addr", r.Addr().String(), "metrics", r.MetricsAddr().String())

	var failed error
	select {
	case <-ctx.Done():
	case err := <-serveErr:
		if err == nil {
			err = errors.New("gate: a listener stopped")
		}
		failed = err
		r.log.Error("a listener failed; draining", "error", err)
	}
	r.drain(context.WithoutCancel(ctx), context.Cause(ctx))
	return failed
}

// drain is spec §14.3 / D76 for this replica; ctx is not canceled.
//
// The tunnel (spec/tunnel.md §13, D35): the Handler refuses new tunnels from the start (draining). The
// connected nodes are told {"t": "drain"} once gate_replicas.draining_at has committed, and not before (M2
// plan audit): a node that redials another replica would otherwise supersede its session here and cut the
// tasks on it, since only a draining replica's sessions are kept (tunnel.md §11). The write is retried every
// DrainMarkRetry, each attempt within DrainMarkTimeout, until it commits or the workers have drained, which
// abandons it: then the nodes are not told. The sessions keep serving their streams while the workers
// drain, and are closed, their rows deleted, once the workers have stopped.
func (r *Replica) drain(ctx context.Context, cause error) {
	r.draining.Store(true)
	r.log.Info("draining", "cause", cause, "drain_timeout", r.cfg.DrainTimeout.String(), "dispatches", r.InFlight(),
		"tunnel_sessions", r.tunnels.Count())
	stopTelling := make(chan struct{})
	told := make(chan struct{})
	go func() {
		defer close(told)
		r.markThenTellTunnels(ctx, stopTelling)
	}()
	if err := r.workers.Drain(); err != nil {
		r.log.Warn("drain_timeout passed with dispatches still running; each is re-attached on another replica after its heartbeat timeout",
			"error", err, "dispatches", r.InFlight())
	} else {
		r.log.Info("the replica's dispatches have ended; its workers are stopped")
	}
	close(stopTelling)
	<-told
	r.tunnels.CloseAll(ctx)
	sctx, cancel := context.WithTimeout(ctx, r.opts.serveGrace)
	defer cancel()
	if err := r.server.Shutdown(sctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		r.log.Warn("public listener shutdown", "error", err)
	}
	if err := r.metrics.Shutdown(sctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		r.log.Warn("metrics listener shutdown", "error", err)
	}
	if r.keys != nil {
		r.keys.Wait()
	}
	if err := r.deregister(ctx); err != nil {
		r.log.Warn("gate_replicas row not deleted; other replicas reap it after 30 s", "error", err)
	}
	r.log.Info("drained")
}

// markThenTellTunnels writes gate_replicas.draining_at, retrying every DrainMarkRetry until it commits or
// stop closes, and then tells the tunnel sessions to reconnect elsewhere (tunnelgate.Registry.Drain).
//
// Each attempt is bounded by DrainMarkTimeout, and the one in progress is abandoned when stop closes (the
// workers have drained): ctx has no deadline, so a write waiting on the row (another transaction holding it,
// a connection that stopped answering) would otherwise hold the drain, and CloseAll after it, for as long
// as it waits (M2 review of WP-04).
func (r *Replica) markThenTellTunnels(ctx context.Context, stop <-chan struct{}) {
	mctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-stop:
			cancel()
		case <-mctx.Done():
		}
	}()
	const stopped = "the drain ended before gate_replicas.draining_at was written: the tunnel nodes were not told, and their sessions close now"
	for {
		actx, acancel := context.WithTimeout(mctx, DrainMarkTimeout)
		err := r.markDraining(actx)
		acancel()
		if err == nil {
			tctx, cancel := context.WithTimeout(ctx, tunnel.ConnectionWriteTimeout+time.Second)
			r.tunnels.Drain(tctx)
			cancel()
			return
		}
		if mctx.Err() != nil {
			r.log.Warn(stopped, "error", err)
			return
		}
		r.log.Warn("gate_replicas.draining_at not written; the tunnel nodes are told to reconnect elsewhere once it is", "error", err)
		select {
		case <-mctx.Done():
			r.log.Warn(stopped)
			return
		case <-time.After(DrainMarkRetry):
		}
	}
}

// close releases the Temporal client and the pool (Run's end, or a New that failed).
func (r *Replica) close() {
	if r.temporal != nil {
		r.temporal.Close()
	}
	if r.pool != nil {
		r.pool.Close()
	}
}

// Reload applies the services: block of the configuration file again (SIGHUP; spec §13.5): added
// services appear, changed ones (an allow: list, a backend, a timeout) apply to the next request, removed
// ones are disabled and answer the unknown-service 404. Every other key keeps the value the replica started
// with; a change to one is logged, for the next start. A file that does not load changes nothing.
func (r *Replica) Reload(ctx context.Context) error {
	if r.opts.reload == nil {
		return errors.New("gate: no configuration file to reload")
	}
	next, err := r.opts.reload()
	if err != nil {
		r.log.Error("SIGHUP: the configuration does not load; nothing changed", "error", err)
		return err
	}
	r.cfgMu.Lock()
	defer r.cfgMu.Unlock()
	if keys := restartKeys(r.cfg, next); len(keys) > 0 {
		r.log.Warn("SIGHUP reloads services: only; these keys changed and take effect at the next start", "keys", keys)
	}
	if err := r.services.Sync(ctx, next); err != nil {
		r.log.Error("SIGHUP: services not applied", "error", err)
		return err
	}
	r.cfg.Services = next.Services
	r.log.Info("SIGHUP: services applied", "services", r.services.Count())
	return nil
}

// restartKeys names the top-level keys of kontra.yaml that differ between a and b, services: aside.
func restartKeys(a, b *config.Config) []string {
	va, vb := reflect.ValueOf(*a), reflect.ValueOf(*b)
	t := va.Type()
	var out []string
	for i := range t.NumField() {
		f := t.Field(i)
		tag, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if tag == "" || tag == "-" || tag == "services" {
			continue
		}
		if !reflect.DeepEqual(va.Field(i).Interface(), vb.Field(i).Interface()) {
			out = append(out, tag)
		}
	}
	return out
}

func (r *Replica) hupLoop(ctx context.Context) error {
	sig := r.opts.hupCh
	if sig == nil {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGHUP)
		defer signal.Stop(ch)
		sig = ch
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-sig:
			r.log.Info("SIGHUP: reloading services")
			rctx, cancel := context.WithTimeout(ctx, time.Minute)
			_ = r.Reload(rctx)
			cancel()
		}
	}
}

// register inserts the replica's gate_replicas row (before any worker polls, spec/tunnel.md §11).
func (r *Replica) register(ctx context.Context) error {
	_, err := r.pool.Exec(ctx, `insert into kontra.gate_replicas (replica_id, version) values ($1, $2)`, r.id, version.Get())
	if err != nil {
		return fmt.Errorf("gate: register replica %s: %w", r.id, err)
	}
	return nil
}

// beat upserts last_seen_at. A row other replicas reaped (this one was silent: a pause, a partition) comes
// back, draining_at included. After deregister it does nothing: the drain deletes the row while the
// heartbeat loop still runs, and a beat must not bring a stopped replica's row back.
func (r *Replica) beat(ctx context.Context) error {
	r.regMu.Lock()
	defer r.regMu.Unlock()
	if r.deregistered {
		return nil
	}
	_, err := r.pool.Exec(ctx, `insert into kontra.gate_replicas (replica_id, version, draining_at)
		values ($1, $2, case when $3 then now() end)
		on conflict (replica_id) do update set last_seen_at = now(),
			draining_at = coalesce(gate_replicas.draining_at, excluded.draining_at)`, r.id, version.Get(), r.draining.Load())
	return err
}

// reap deletes the rows of replicas silent for more than ReplicaSilence (their tunnels rows cascade) and
// closes the MCP sessions pinned to them, tunnel_lost (spec/tunnel.md §11). It returns the reaped ids.
func (r *Replica) reap(ctx context.Context) ([]string, error) {
	rows, err := r.pool.Query(ctx, `with gone as (
			delete from kontra.gate_replicas
			 where last_seen_at < now() - $2::bigint * interval '1 microsecond' and replica_id <> $1
			returning replica_id
		), closed as (
			update kontra.mcp_sessions set closed_at = now(), close_reason = 'tunnel_lost'
			 where closed_at is null and replica_id in (select replica_id from gone)
		)
		select replica_id from gone`, r.id, r.opts.silence.Microseconds())
	if err != nil {
		return nil, err
	}
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (r *Replica) markDraining(ctx context.Context) error {
	_, err := r.pool.Exec(ctx, `update kontra.gate_replicas set draining_at = coalesce(draining_at, now()), last_seen_at = now()
		 where replica_id = $1`, r.id)
	if err != nil {
		return err
	}
	return r.beat(ctx) // a row reaped meanwhile comes back, draining
}

// deregister deletes the replica's row, last in the drain, and ends its heartbeat: it waits for a beat in
// flight, and no later beat inserts the row again. A delete that fails leaves the row to other replicas,
// which reap it after ReplicaSilence.
func (r *Replica) deregister(ctx context.Context) error {
	r.regMu.Lock()
	defer r.regMu.Unlock()
	r.deregistered = true
	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := r.pool.Exec(dctx, `delete from kontra.gate_replicas where replica_id = $1`, r.id)
	return err
}

// replicaLoop is the replica registry's loop (spec/tunnel.md §11): every beatEvery, upsert this replica's
// last_seen_at and reap the replicas silent for more than ReplicaSilence. It keeps beating during the drain.
func (r *Replica) replicaLoop(ctx context.Context) error {
	t := time.NewTicker(r.opts.beatEvery)
	defer t.Stop()
	for {
		bctx, cancel := context.WithTimeout(ctx, r.opts.beatEvery)
		if err := r.beat(bctx); err != nil && ctx.Err() == nil {
			r.log.Warn("gate_replicas heartbeat failed", "error", err)
		}
		if gone, err := r.reap(bctx); err != nil && ctx.Err() == nil {
			r.log.Warn("reaping silent replicas failed", "error", err)
		} else if len(gone) > 0 {
			r.log.Info("reaped replicas silent for more than "+r.opts.silence.String(), "replicas", gone)
		}
		cancel()
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// checkHealth pings Postgres and Temporal once and records the answers for Ready.
func (r *Replica) checkHealth(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		hctx, cancel := context.WithTimeout(ctx, HealthTimeout)
		defer cancel()
		err := r.pool.Ping(hctx)
		if r.pgUp.Swap(err == nil) != (err == nil) {
			r.log.Warn("postgres reachability changed", "up", err == nil, "error", err)
		}
	}()
	go func() {
		defer wg.Done()
		hctx, cancel := context.WithTimeout(ctx, HealthTimeout)
		defer cancel()
		_, err := r.temporal.CheckHealth(hctx, &client.CheckHealthRequest{})
		if r.temporalUp.Swap(err == nil) != (err == nil) {
			r.log.Warn("temporal reachability changed", "up", err == nil, "error", err)
		}
	}()
	wg.Wait()
}

func (r *Replica) healthLoop(ctx context.Context) error {
	t := time.NewTicker(r.opts.healthEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			r.checkHealth(ctx)
		}
	}
}

// serveMetrics is GET /metrics on the metrics listener (spec §2.1), in the Prometheus text format. The gate
// has no metrics registry yet (WP-03's note); these are the replica's own gauges.
func (r *Replica) serveMetrics(w http.ResponseWriter, _ *http.Request) {
	b := func(v bool) int {
		if v {
			return 1
		}
		return 0
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	gauge := func(name, help string, v any) {
		_, _ = io.WriteString(w, fmt.Sprintf("# HELP %s %s\n# TYPE %s gauge\n%s{replica=%q} %v\n", name, help, name, name, r.id, v))
	}
	gauge("kontra_gate_up", "1 while the replica runs.", 1)
	gauge("kontra_gate_ready", "1 while /healthz answers 200.", b(r.Ready()))
	gauge("kontra_gate_draining", "1 once the replica has begun its drain.", b(r.draining.Load()))
	gauge("kontra_gate_dispatches_in_flight", "Dispatches running on this replica.", r.InFlight())
	gauge("kontra_gate_services", "Enabled services in the registry.", r.services.Count())
	gauge("kontra_gate_tunnel_sessions", "Tunnel sessions this replica holds.", r.tunnels.Count())
	writePatchRefusals(w, r.id, &r.refusals)
}

// writePatchRefusals writes kontra_gate_operator_patches_refused_total, the operator patches this replica's
// apply_metadata and submit refused and dropped, by source and refusal class (spec §13.3: "a metric and an
// audit row"), so a hook or control plane that keeps sending refused patches can raise an alert.
func writePatchRefusals(w io.Writer, replica string, p *lifecycle.PatchRefusals) {
	const name = "kontra_gate_operator_patches_refused_total"
	_, _ = fmt.Fprintf(w, "# HELP %s Operator patches refused and dropped (spec §13.3), by source and refusal.\n# TYPE %s counter\n", name, name)
	for _, c := range p.Counts() {
		_, _ = fmt.Fprintf(w, "%s{replica=%q,source=%q,refusal=%q} %d\n", name, replica, c.Source, c.Refusal, c.N)
	}
}

// countingDispatch counts the dispatches running on this replica (metrics, the drain's log).
type countingDispatch struct {
	lifecycle.DispatchActivities
	n atomic.Int64
}

func (c *countingDispatch) Dispatch(ctx context.Context, in lifecycle.DispatchInput) (lifecycle.DispatchResult, error) {
	c.n.Add(1)
	defer c.n.Add(-1)
	return c.DispatchActivities.Dispatch(ctx, in)
}
