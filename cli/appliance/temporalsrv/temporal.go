// Package appliance holds the parts of the control plane that run INSIDE the kontra binary
// instead of beside it in a container (ADR 0031).
//
// This file is the first of them: a Temporal server started as a library. `go.temporal.io/server`
// IS the thing the `temporalio/temporal` image runs — `temporal server start-dev` is that library
// plus a `main` — so embedding it removes a container without changing what the server is. Every
// client keeps talking gRPC to an address; the only thing that changes is who owns the process.
//
// WHAT THIS DELIBERATELY REPRODUCES, and why it is a copy rather than an import: the setup below
// mirrors `temporalio/cli`'s `internal/devserver` at CLI 1.8.0 — the exact code the compose
// container ran — because that package is `internal/` and cannot be imported. The equivalent
// inside the server module, `temporaltest/internal`, is internal too. So the choice is a copy of
// ~150 lines or a dependency on the whole CLI (cobra, the UI server, the news-feed fetch). The
// copy is small, it is pinned to a version we can read, and it drops the two pieces we do not
// want: the Web UI (issue 16, opt-in) and the ephemeral in-memory database.
//
// WHY THE SERVER IS A 1.32 PRE-RELEASE AND NOT 1.31.2, WHICH IS WHAT THE CONTAINER RAN. It is
// not a choice, it is the module graph. `go.temporal.io/server` v1.31.2 is built against
// `go.temporal.io/api` v1.62.8; api v1.63 added `CountNexusOperationExecutions` to
// `WorkflowServiceClient`, which 1.31.2's generated wrappers do not implement, so it does not
// COMPILE against api ≥ 1.63. And this workspace is already past that line in two places that
// have nothing to do with the appliance: the SDK this CLI uses (v1.46.0) requires api v1.63.0,
// and sdk/go pins api v1.63.4 outright. One process links one copy of api, so the embedded
// server has to be a build made against the same one. v1.32.0-162.1 is the first published
// version that is. When 1.32.0 final ships, move to it.
package temporalsrv

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/server/chasm/lib/activity"
	"go.temporal.io/server/common/authorization"
	"go.temporal.io/server/common/cluster"
	"go.temporal.io/server/common/config"
	"go.temporal.io/server/common/dynamicconfig"
	"go.temporal.io/server/common/log"
	"go.temporal.io/server/common/membership/static"
	"go.temporal.io/server/common/metrics"
	sqliteplugin "go.temporal.io/server/common/persistence/sql/sqlplugin/sqlite"
	"go.temporal.io/server/common/primitives"
	"go.temporal.io/server/components/nexusoperations"
	sqliteschema "go.temporal.io/server/schema/sqlite"
	"go.temporal.io/server/temporal"

	"github.com/google/uuid"
)

// DefaultNamespace is the namespace every kontra client resolves when KONTRA_NAMESPACE is unset,
// so it is the one the embedded server must pre-create. A worker that polls a namespace the
// server has never heard of fails on its first poll, not at startup.
const DefaultNamespace = "default"

// DefaultPort is Temporal's gRPC port, and it is deliberately the SAME number the compose service
// published. `KONTRA_ADDRESS` defaults to `localhost:7233` in three SDKs and in this CLI; an
// embedded server on a different port would be a working control plane that every existing client
// fails to find.
const DefaultPort = 7233

// Options configures the embedded server. The zero value is not useful — Start
// fills the defaults that have one and rejects the rest.
type Options struct {
	// DataDir is the directory the server owns. It holds `temporal.db` and nothing else today.
	// Required: an appliance with no data directory is the ephemeral dev server, which is the one
	// behaviour this slice exists to not have.
	DataDir string

	// BindIP is the address the frontend listens on. Empty means 127.0.0.1 (ADR 0031 §3: the
	// appliance binds loopback, so there is no remote caller and therefore no browser credential
	// question to answer). The internal services always bind this same address on free ports.
	BindIP string

	// Port is the frontend gRPC port; 0 means DefaultPort.
	Port int

	// MetricsPort serves Prometheus at /metrics; 0 means a system-chosen free port. Compose
	// pinned 9091 — carried as an option rather than a constant because a pinned port that is
	// taken is a startup failure, and metrics are not worth one.
	MetricsPort int

	// Namespaces are pre-created on first boot. DefaultNamespace is always included.
	Namespaces []string

	// LogLevel is the server's own log level: "debug", "info", "warn", "error". Empty means
	// "warn", which is what compose passed — the server is noisy at info and most of it is about
	// its own internals rather than about your run.
	LogLevel string
}

// Server is a started embedded server. It is returned already serving: Address() answers
// a real listener, so a caller can hand that string to a client without polling for readiness.
type Server struct {
	server  temporal.Server
	address string
	dbPath  string
	metrics string
}

// Address is host:port for the frontend — what goes in KONTRA_ADDRESS.
func (s *Server) Address() string { return s.address }

// DatabasePath is the SQLite file this server persists to. Named so an operator can find the
// thing to back up, and so a test can assert it survived a restart.
func (s *Server) DatabasePath() string { return s.dbPath }

// MetricsAddress is host:port for the Prometheus handler at /metrics.
func (s *Server) MetricsAddress() string { return s.metrics }

// Stop shuts the server down. Safe to call once; the server itself is not re-startable.
func (s *Server) Stop() { s.server.Stop() }

// Start boots the embedded server and returns once it is serving.
func Start(opts Options) (*Server, error) {
	if opts.DataDir == "" {
		return nil, errors.New("appliance: DataDir is required (the embedded server owns its own data directory)")
	}
	if opts.BindIP == "" {
		opts.BindIP = "127.0.0.1"
	}
	if net.ParseIP(opts.BindIP) == nil {
		// The server wants an IP, not a name: "localhost" reaches the config as a literal and the
		// listener never binds. The CLI translates that one word for the same reason.
		return nil, fmt.Errorf("appliance: bind address %q is not an IP (use 127.0.0.1, not a hostname)", opts.BindIP)
	}
	if opts.Port == 0 {
		opts.Port = DefaultPort
	}
	if opts.LogLevel == "" {
		opts.LogLevel = "warn"
	}

	// THE PORT CHECK IS THE ERROR MESSAGE. ADR 0031 leaves "what `kontra up` does on a machine
	// that also runs the compose stack" open, and the visible half of that question is this port.
	// Answered here as a refusal that names the collision, because the alternative — the server
	// failing somewhere inside fx with a wrapped `bind: address already in use` — is how an
	// operator concludes the appliance is broken when in fact it is the second one.
	if err := checkPortFree(opts.BindIP, opts.Port); err != nil {
		return nil, fmt.Errorf("appliance: %s:%d is already listening — another Temporal (the compose `temporal` service?) has it: %w",
			opts.BindIP, opts.Port, err)
	}
	if opts.MetricsPort == 0 {
		p, err := freePort(opts.BindIP)
		if err != nil {
			return nil, fmt.Errorf("appliance: %w", err)
		}
		opts.MetricsPort = p
	}

	if err := os.MkdirAll(opts.DataDir, 0o755); err != nil {
		return nil, fmt.Errorf("appliance: data directory %s: %w", opts.DataDir, err)
	}
	dbPath := filepath.Join(opts.DataDir, "temporal.db")

	cfg, err := serverConfig(opts, dbPath)
	if err != nil {
		return nil, err
	}

	logger := log.NewZapLogger(log.BuildZapLogger(log.Config{Stdout: true, Level: opts.LogLevel}))
	authorizer, err := authorization.GetAuthorizerFromConfig(&cfg.Global.Authorization)
	if err != nil {
		return nil, fmt.Errorf("appliance: authorizer: %w", err)
	}
	claimMapper, err := authorization.GetClaimMapperFromConfig(&cfg.Global.Authorization, logger)
	if err != nil {
		return nil, fmt.Errorf("appliance: claim mapper: %w", err)
	}

	srv, err := temporal.NewServer(
		temporal.WithConfig(cfg),
		temporal.ForServices(temporal.DefaultServices),
		// Static hosts rather than ringpop discovery: all four services are in THIS process, so
		// there is nothing to discover and a membership gossip round is a startup delay paid for
		// no information.
		temporal.WithStaticHosts(map[primitives.ServiceName]static.Hosts{
			primitives.FrontendService: static.SingleLocalHost(hostPort(opts.BindIP, cfg.Services[string(primitives.FrontendService)].RPC.GRPCPort)),
			primitives.MatchingService: static.SingleLocalHost(hostPort(opts.BindIP, cfg.Services[string(primitives.MatchingService)].RPC.GRPCPort)),
			primitives.HistoryService:  static.SingleLocalHost(hostPort(opts.BindIP, cfg.Services[string(primitives.HistoryService)].RPC.GRPCPort)),
			primitives.WorkerService:   static.SingleLocalHost(hostPort(opts.BindIP, cfg.Services[string(primitives.WorkerService)].RPC.GRPCPort)),
		}),
		temporal.WithLogger(logger),
		temporal.WithAuthorizer(authorizer),
		temporal.WithClaimMapper(func(*config.Config) authorization.ClaimMapper { return claimMapper }),
		temporal.WithDynamicConfigClient(DynamicConfig()),
	)
	if err != nil {
		return nil, fmt.Errorf("appliance: building the embedded Temporal server: %w", err)
	}
	if err := srv.Start(); err != nil {
		return nil, fmt.Errorf("appliance: starting the embedded Temporal server: %w", err)
	}
	return &Server{
		server:  srv,
		address: hostPort(opts.BindIP, opts.Port),
		dbPath:  dbPath,
		metrics: hostPort(opts.BindIP, opts.MetricsPort),
	}, nil
}

// DynamicConfig is the server's dynamic configuration, and it is EXPORTED so a test can assert
// the three load-bearing values without starting a server.
//
// Every key here is spelled as a typed setting's `.Key()` rather than as a string. That is the
// whole point of the shape: a string key that the server does not register is not an error, it is
// a silent fallback to the default. Today's compose command passes
// `--dynamic-config-value limit.numPendingNexusOperations=2000`, and NO SUCH KEY is registered by
// the server — see the Nexus entry below. Bound to the setting variable, a rename in a future
// server release fails the build instead of quietly restoring a limit somebody measured.
func DynamicConfig() dynamicconfig.StaticClient {
	return dynamicconfig.StaticClient{
		// ---- the load-bearing three (carried from the compose `temporal` service) ----

		// 16 MiB, against defaults of 2 MiB (error) and 512 KiB (warn). NOT AN ARBITRARY
		// ROUNDING UP: a large root-seed set rides INLINE in the interpreter's workflow argument,
		// because the streaming cursor only shards INLINE roots — an offloaded ($ref) payload
		// runs as one un-sharded node. So the seed set has to fit in one event blob, and at the
		// dev default the run is rejected at admission rather than degraded.
		dynamicconfig.BlobSizeLimitError.Key(): 16 * 1024 * 1024,
		// Warn is raised to the SAME number on purpose, not to a fraction of it. Left at 512 KiB
		// it fires on every single dispatch of a normal-sized seed set, and a warning that is
		// always on is a log nobody reads.
		dynamicconfig.BlobSizeLimitWarn.Key(): 16 * 1024 * 1024,

		// The pending-Nexus-operation cap, raised from a default of 30. Each in-flight node
		// operation is one pending Nexus operation on the interpreter workflow, and
		// KONTRA_MAX_INFLIGHT_NODES defaults to 64 — already over the default cap. Exceeding it
		// does not fail loudly: the workflow task is rejected and retries forever while every
		// dashboard still reads "running", which is the failure mode docs/wiki/Deployment.md
		// warns about.
		//
		// THE KEY IS NOT THE ONE COMPOSE PASSES. Compose says `limit.numPendingNexusOperations`,
		// which the server does not register anywhere — checked against both 1.31.2 (what the
		// container ran) and the version pinned here — so that value has been a no-op and the
		// live cap has been the default 30, below the 64 the API hands out. The registered
		// setting is `component.nexusoperations.limit.operation.concurrency`. The 2000 is
		// carried; the spelling is corrected.
		nexusoperations.MaxConcurrentOperations.Key(): 2000,

		// ---- what `temporal server start-dev` sets for itself ----
		//
		// These are not kontra's choices; they are the dev server's, and dropping them would
		// change behaviour the whole repo has been developed against. Reproduced from
		// temporalio/cli 1.8.0 `internal/temporalcli/commands.server.go:defaultDynamicConfigValues`
		// and `internal/devserver/server.go:buildServerOptions`.

		// Search attributes visible the instant they are created. The orchestrator registers
		// KontraTenant / KontraRunId / KontraActor / KontraTag at startup and upserts them on the
		// very next workflow; with the cache in play that upsert fails on an attribute the
		// frontend has not noticed yet.
		dynamicconfig.ForceSearchAttributesCacheRefreshOnRead.Key(): true,
		// Nexus endpoints visible the instant they are created, for the same reason: registering
		// an Actor creates its endpoint and a dispatch can follow in the next second. Worth
		// noting for whoever reads the compose file's history — this key is registered in the
		// version pinned here but was NOT in 1.31.2, which is what the compose stack ran, so on
		// that server the CLI's own default was as dead as kontra's Nexus limit was.
		dynamicconfig.ForceNexusEndpointRefreshOnRead.Key(): true,
		// Both caches above are bypassed, so those reads go to persistence. These two raise the
		// QPS ceilings to match; they keep the ratio the packaged defaults have.
		dynamicconfig.FrontendPersistenceMaxQPS.Key(): 10000,
		dynamicconfig.HistoryPersistenceMaxQPS.Key():  45000,
		// Host-level mutable state cache, and the visibility RPS the dev server raises with it.
		dynamicconfig.HistoryCacheHostLevelMaxSize.Key():                 8096,
		dynamicconfig.FrontendMaxNamespaceVisibilityRPSPerInstance.Key(): 100,
		// CHASM and the CHASM activity component. On by default from server 1.32; until then the
		// dev server turns them on, so a server without these lines is a DIFFERENT server from
		// the one the repo's workflows have always run against.
		dynamicconfig.EnableChasm.Key(): true,
		activity.Enabled.Key():          true,
	}
}

// serverConfig builds the static server configuration: SQLite persistence in the appliance's own
// data directory, four services in one process, no archival, no cross-cluster anything.
func serverConfig(opts Options, dbPath string) (*config.Config, error) {
	var cfg config.Config

	cfg.Global.Membership.MaxJoinDuration = 30 * time.Second
	cfg.Global.Membership.BroadcastAddress = opts.BindIP
	cfg.Global.Metrics = &metrics.Config{
		Prometheus: &metrics.PrometheusConfig{
			ListenAddress: hostPort(opts.BindIP, opts.MetricsPort),
			HandlerPath:   "/metrics",
		},
	}

	// PERSISTENT SQLITE, NOT THE DEV SERVER'S IN-MEMORY DEFAULT. The whole recovery story leans
	// on Temporal history surviving a control-plane restart: a run is resumed from its history,
	// `kontra runs` reads Visibility for runs that finished days ago, and the Monitor's answer to
	// "what happened" is history. An ephemeral server makes every restart a data loss that looks
	// like a run that never existed. `mode=rwc` creates the file on first boot.
	//
	// This is also the entry that deletes `temporal-data-owner` from compose. That one-shot
	// existed to chown a Docker named volume to uid 1000, because a root-owned volume made SQLite
	// report `unable to open database file: out of memory (14)` on a machine with 7.9 GB free.
	// A file the appliance creates in its own data directory, as the user who ran `kontra up`,
	// has no uid to mismatch.
	sqlConf := config.SQL{
		PluginName:        sqliteplugin.PluginName,
		DatabaseName:      dbPath,
		ConnectAttributes: map[string]string{"mode": "rwc"},
	}
	cfg.Persistence = config.Persistence{
		DefaultStore:     sqliteplugin.PluginName,
		VisibilityStore:  sqliteplugin.PluginName,
		NumHistoryShards: 1,
		DataStores:       map[string]config.DataStore{sqliteplugin.PluginName: {SQL: &sqlConf}},
	}

	// Schema migration runs once, when the file is not there yet. On every later boot the file
	// exists and this is skipped — which is what makes a restart a restart rather than a reset.
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		if err := sqliteschema.SetupSchema(&sqlConf); err != nil {
			return nil, fmt.Errorf("appliance: setting up the schema in %s: %w", dbPath, err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("appliance: %s: %w", dbPath, err)
	}

	httpPort, err := freePort(opts.BindIP)
	if err != nil {
		return nil, fmt.Errorf("appliance: %w", err)
	}
	cfg.ClusterMetadata = &cluster.Config{
		EnableGlobalNamespace:    false,
		FailoverVersionIncrement: 10,
		MasterClusterName:        "active",
		CurrentClusterName:       "active",
		ClusterInformation: map[string]cluster.ClusterInformation{
			"active": {
				Enabled:                true,
				InitialFailoverVersion: 1,
				RPCAddress:             hostPort(opts.BindIP, opts.Port),
				HTTPAddress:            hostPort(opts.BindIP, httpPort),
				// A fresh id per boot, exactly as `start-dev --db-filename` does: cluster
				// metadata is persisted on first boot and the stored row wins afterwards.
				ClusterID: uuid.NewString(),
			},
		},
	}
	cfg.DCRedirectionPolicy.Policy = "noop"

	frontend := config.Service{RPC: config.RPC{GRPCPort: opts.Port, BindOnIP: opts.BindIP, HTTPPort: httpPort}}
	internal := func() (config.Service, error) {
		p, err := freePort(opts.BindIP)
		if err != nil {
			return config.Service{}, err
		}
		return config.Service{RPC: config.RPC{GRPCPort: p, BindOnIP: opts.BindIP}}, nil
	}
	history, err := internal()
	if err != nil {
		return nil, fmt.Errorf("appliance: %w", err)
	}
	matching, err := internal()
	if err != nil {
		return nil, fmt.Errorf("appliance: %w", err)
	}
	worker, err := internal()
	if err != nil {
		return nil, fmt.Errorf("appliance: %w", err)
	}
	cfg.Services = map[string]config.Service{
		"frontend": frontend,
		"history":  history,
		"matching": matching,
		"worker":   worker,
	}

	cfg.Archival.History.State = "disabled"
	cfg.Archival.Visibility.State = "disabled"
	cfg.NamespaceDefaults.Archival.History.State = "disabled"
	cfg.NamespaceDefaults.Archival.Visibility.State = "disabled"
	// Without PublicClient the server panics with "Client must be created with client.Dial()".
	cfg.PublicClient.HostPort = hostPort(opts.BindIP, opts.Port)

	// Namespace creation is idempotent against an existing database, so this is safe on a
	// restart and is what makes the FIRST boot usable without a `temporal operator` call.
	wanted := append([]string{DefaultNamespace}, opts.Namespaces...)
	seen := map[string]bool{}
	var namespaces []*sqliteschema.NamespaceConfig
	for _, ns := range wanted {
		if ns == "" || seen[ns] {
			continue
		}
		seen[ns] = true
		nsCfg, err := sqliteschema.NewNamespaceConfig(cfg.ClusterMetadata.CurrentClusterName, ns, false, kontraSearchAttributes())
		if err != nil {
			return nil, fmt.Errorf("appliance: namespace %q: %w", ns, err)
		}
		namespaces = append(namespaces, nsCfg)
	}
	if err := sqliteschema.CreateNamespaces(&sqlConf, namespaces...); err != nil {
		return nil, fmt.Errorf("appliance: creating namespaces: %w", err)
	}

	return &cfg, nil
}

// kontraSearchAttributes are the four custom search attributes kontra's own workflows STAMP, and
// a namespace without them cannot run a dispatch at all.
//
// MEASURED, and the failure is the expensive kind. The Go handler's backing workflow
// (`kontra.v1.ActorService.Run`) upserts `KontraRunId` and `KontraActor`; against a namespace that
// has no mapping for them, every workflow task fails with
//
//	BadSearchAttributes: Namespace default has no mapping defined for search attribute KontraRunId
//
// and Temporal RETRIES the task forever. So the actor never runs, nothing is dropped, nothing
// errors, and the caller sees a schedule-to-close timeout minutes later naming a Nexus operation
// — three layers away from a namespace setting. The handler's log is the only place the real
// sentence appears, and on a worker container that log is inside the container.
//
// THEY USED TO BE REGISTERED BY THE ORCHESTRATOR AND ONLY BY IT (`control/orchestrator/src/visibility.ts`,
// called from `temporalClient.ts` at boot), which was fine while the orchestrator was the only way
// anything reached Temporal. It is not fine now: `kontra up --orchestrator=none` describes itself
// as "serving its five embedded services only", and what it actually served was a control plane on
// which no dispatch could ever succeed. A namespace's search attributes are a property of the
// NAMESPACE, and this process is the one that creates it.
//
// Registering here does not replace the orchestrator's call and must not: an appliance pointed at
// someone else's Temporal still needs that one, and adding an attribute that already exists is a
// no-op on both paths. The names are the contract — they are spelled identically in
// `control/orchestrator/src/visibility.ts` and in `handler/`, and a fifth one added there needs a line
// here.
func kontraSearchAttributes() map[string]enumspb.IndexedValueType {
	return map[string]enumspb.IndexedValueType{
		"KontraTenant": enumspb.INDEXED_VALUE_TYPE_KEYWORD,
		"KontraRunId":  enumspb.INDEXED_VALUE_TYPE_KEYWORD,
		"KontraActor":  enumspb.INDEXED_VALUE_TYPE_KEYWORD,
		"KontraTag":    enumspb.INDEXED_VALUE_TYPE_KEYWORD,
	}
}

func hostPort(ip string, port int) string { return net.JoinHostPort(ip, fmt.Sprint(port)) }

// checkPortFree reports whether we could bind ip:port right now.
func checkPortFree(ip string, port int) error {
	l, err := net.Listen("tcp", hostPort(ip, port))
	if err != nil {
		return err
	}
	return l.Close()
}

// freePort asks the OS for a port and then makes sure the OS will not hand the SAME one out again
// a moment later.
//
// The dial-and-close dance is not superstition and it is not ours: it is temporalio/cli's, copied
// with its reason. On Linux, ephemeral ports are randomised and a port released by bind(:0) can be
// re-allocated to the next bind(:0) within seconds — and this function is called four times in a
// row, for four services that must not collide. Connecting and closing from the LISTENER's side
// parks the port in TIME_WAIT, which stops bind(:0) choosing it again while still allowing an
// explicit bind (Go sets SO_REUSEADDR). macOS and Windows allocate sequentially and have a much
// smaller range, so there the trick would only hasten exhaustion.
func freePort(ip string) (int, error) {
	l, err := net.Listen("tcp", hostPort(ip, 0))
	if err != nil {
		return 0, fmt.Errorf("no free port on %s: %w", ip, err)
	}
	defer l.Close()
	port := l.Addr().(*net.TCPAddr).Port
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		return port, nil
	}
	conn, err := net.Dial("tcp", hostPort(ip, port))
	if err != nil {
		return 0, fmt.Errorf("no free port on %s: %w", ip, err)
	}
	defer conn.Close()
	accepted, err := l.Accept()
	if err != nil {
		return 0, fmt.Errorf("no free port on %s: %w", ip, err)
	}
	// Closed from the SERVER side — that is the half that creates the TIME_WAIT.
	accepted.Close()
	return port, nil
}
