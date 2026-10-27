// Package e2e holds the gate's end-to-end tests (spec/relay.md §16) and their harness.
//
// Each test starts one environment and removes it when it ends, never in parallel: a database (a fresh
// one on the server $KONTRA_TEST_PG names, else a postgres:17-alpine container on a loopback port),
// migrated by `kontra-gate migrate`; one Temporal development server from the pinned CLI
// ($KONTRA_TEMPORAL_CLI, which must report Server 1.32; spec §0), in memory, namespace kontra, with
// frontend.WorkerCommandsEnabled and system.enableCancelActivityWorkerCommand on (spec §14.2); the slowagent
// upstreams in the test process; and two kontra-gate replicas as subprocesses of the binary built once from
// ./cmd/kontra-gate, sharing one kontra.yaml and given their own --listen and --metrics-listen (with
// Options.PostgresProxy, replica A reads a copy whose postgres: is a proxy the test can cut). public_url is
// the Front, a TCP proxy in the test process that sends each new connection to the replica the test routes
// it to (replica A by default), so a node's hello names public_url as its origin whichever replica its
// tunnel lands on (spec/tunnel.md §4). Two API keys with distinct buyers are minted with `kontra-gate enroll
// --api-key`. The buyer is a minimal SSE client that keeps every `id:` line, because a2a-go's client
// ignores them; unary calls use a2a-go's own client.
//
// Tunnel services (Options.Tunnel, backend tunnel) are hosted by nodes: `kontra connect` subprocesses of the
// binary built once from ./cmd/kontra (node.go), each with its own HOME, dialing the Front. Two in-process
// doubles stand beside them: a FakeNode (fakenode.go) that completes the handshake with a node's key and
// ignores the control stream, and a FakeGate (fakegate.go), the gate's tunnel handler with a fixed admission
// and no Postgres or Temporal, which drives crafted streams at a real `kontra connect`.
//
// Everything the harness starts it removes, including on failure; every process it starts (replicas,
// Temporal, nodes, the helloworld seller) carries KONTRA_E2E_RUN in its environment, and every file it writes
// lives under TempRoot, so `make test-e2e` can prove none outlived the run.
//
// The pinned CLI is temporal 1.9.1 (Server 1.32.0); its linux amd64 binary's sha256 is
// 712217694cac9f786a72d4b6c876cd15f3dcf1bf31434d9fed6af25242a327a0, which CI checks before it sets
// KONTRA_TEMPORAL_CLI. The `temporal` on a developer's PATH is often another version, so the harness never
// falls back to it.
//
// M1's tests (D80): relay_resume (TestRelayResume), replica_kill direct (TestReplicaKillDirect, and its
// SIGSTOP variant TestReplicaStallDirect, D77), cancel_forwarded (TestCancelForwarded), push_delivery
// (TestPushDelivery), ssrf_webhook (TestSSRFWebhook), card_compat (TestCardCompat), cross_buyer
// (TestCrossBuyer) and version_conflict (TestVersionConflict), plus follow-up turns through the real stack
// (TestFollowUp; TestFollowUpTemporalUnreachable, an Update Temporal does not accept; TestFollowUpLateRead,
// a2a-go's read failing after Write accepted the turn) and the failed push config set again
// (TestPushFailedRestart). M2's (PRD §14 M2 row; spec/tunnel.md §16, spec/relay.md §16): tunnel_kill
// (TestTunnelKill), input_required_sticky (TestInputRequiredSticky), cancel_forwarded through the tunnel
// (TestCancelForwardedTunnel), key_scope (TestKeyScope), sidecar_not_a_proxy (TestSidecarNotAProxy),
// replica_kill's tunnel variant (TestReplicaKillTunnel), the helloworld seller through the tunnel
// (TestHelloworldTunnel) and the make-before-break drain (TestTunnelDrain), plus the rotation after a leak
// at the binary level (TestNodeRotateAfterLeak, D125), a welcome lost after the gate committed
// (TestTunnelLostWelcome, D124, D139), a stream refused while the revocation view is stale
// (TestTunnelStaleRevocationView) and a task placed before a tunnel service's first card
// (TestTunnelCardPending, D143). Each test that ends a task
// exports its TaskLifecycle history and replays it against the current workflow; with
// KONTRA_RECORD_HISTORIES=1 it also writes it to internal/lifecycle/testdata/histories, never over an
// existing file.
//
// M3's (D171, D172, D189; m3_test.go lists them): the six gate tests sealed_no_plaintext, sealed_replay,
// sealed_unsealed_part, mcp_session_cross_node, mcp_tool_denied and mcp_task_resume, and card_passthrough,
// mcp_batch_refused, mcp_meta_spoof, mcp_session_revoked, mcp_case_smuggling, a2a_case_smuggling, stdio_roots,
// local_rebinding and the hiring row, mcp_modern_task, mcp_era_fallback, mcp_peer_hop and a sealed part through the
// bucket. For them both replicas share one signing_key (D166), log at debug level, and get a peer listener and a
// gofakes3 bucket when a test asks (Options.Peer, Options.Blobs); nodes host MCP services and hire; the stock npm
// servers and every other server the harness starts listen on 127.0.0.1 only, which each test asserts at its end
// (D185, CheckLoopback).
package e2e

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.temporal.io/sdk/client"
	tlog "go.temporal.io/sdk/log"

	"kontra.sh/kontra/internal/lifecycle"
	"kontra.sh/kontra/internal/store"
	"kontra.sh/kontra/test/e2e/slowagent"
)

// Environment variables the harness reads or sets.
const (
	// TemporalCLIEnv names the pinned temporal CLI (1.9.1, Server 1.32.0). Required: the temporal on PATH
	// is not the pin on every machine, and an unpinned server may lack the worker-command settings.
	TemporalCLIEnv = "KONTRA_TEMPORAL_CLI"
	// PostgresEnv names a Postgres server the harness creates (and drops) one database on per test; unset,
	// the harness runs a container per test.
	PostgresEnv = "KONTRA_TEST_PG"
	// RunEnv is set on every process the harness starts, to the run's id.
	RunEnv = "KONTRA_E2E_RUN"
	// SessionEnv, set by scripts/test_e2e.sh, is passed on to every process the harness starts and labels
	// its containers, so the script can tell this run's leftovers from anything else on the machine.
	SessionEnv = "KONTRA_E2E_SESSION"
	// RecordEnv, when 1, writes the TaskLifecycle histories the tests export into
	// internal/lifecycle/testdata/histories, never over an existing file (WP-05: a recording is never
	// re-recorded over once a change is gated).
	RecordEnv = "KONTRA_RECORD_HISTORIES"
)

// TemporalServerVersion is what the CLI must report (spec §0 pins the server at 1.32.x).
const TemporalServerVersion = "Server 1.32"

// Namespace is the gate's Temporal namespace.
const Namespace = "kontra"

// Options configure a harness.
type Options struct {
	PostgresImage string // the container when KONTRA_TEST_PG is unset; default postgres:17-alpine
	TemporalCLI   string // default: $KONTRA_TEMPORAL_CLI
	// Upstream is the service `slow`: a direct a2a backend at a slowagent with allow_private (D67).
	Upstream slowagent.Config
	// Services adds more direct a2a services, each with its own slowagent (card_compat's 0.2.5 seller).
	Services map[string]slowagent.Config
	// PushAllowPrivate is push.allow_private (spec §10.3): loopback http:// webhooks. Off by default, as in
	// the gate.
	PushAllowPrivate bool
	// TemporalProxy routes the replicas' Temporal connection through a TCP proxy the test can cut or
	// blackhole (Harness.TemporalProxy), to make Temporal unreachable for them, or make it hang, while the
	// test still reads it.
	TemporalProxy bool
	// PostgresProxy routes replica A's Postgres connections (its pool and its LISTEN connection) through a
	// TCP proxy the test can cut (Harness.PostgresProxy); replica B and the test's own probes connect
	// directly. Replica A then reads its own copy of kontra.yaml, which differs only in postgres:.
	PostgresProxy bool
	// Retries is services.slow.task.retries (default 2, spec §6.4), and every other service's unless it
	// sets its own.
	Retries *int
	// Tunnel declares tunnel services: backend tunnel, allow org:default, hosted by the nodes enrolled for
	// them (StartNode). A name that is also `slow` or in Services keeps its slowagent (Upstreams[name]) for a
	// node to serve, but the gate's configuration names no URL for it: the gate reaches it only through a
	// tunnel. Without nodes, a tunnel service has no card and no candidate session.
	Tunnel map[string]TunnelService
	// DrainTimeout is drain_timeout (default 10 s, spec §14.3).
	DrainTimeout time.Duration

	// M3 (WP-09). MCP declares the MCP services (kind mcp, spec §11): a direct mcp: {url} backend with
	// allow_private, or, without a URL, a tunnel service hosted by the nodes enrolled for it.
	MCP map[string]MCPService
	// Blobs gives the gate its claim-check bucket (blobs:, spec §10.4, D167): a gofakes3 server in the test
	// process on 127.0.0.1 (bucket.go), whose credentials only the replicas' environment carries.
	Blobs bool
	// Peer gives each replica a peer listener on a loopback port of its own (--peer-listen) and both the shared
	// secret in KONTRA_PEER_SECRET (spec §11.2, D163, D179).
	Peer bool
	// SellerKeys is services.<name>.seller_keys of a direct A2A service (spec §3): a JWKS document, inline.
	SellerKeys map[string]string
}

// TunnelService is one tunnel service of Options.Tunnel.
type TunnelService struct {
	Retries *int // task.retries; nil: Options.Retries
	// Sealed is the service's sealed: value as YAML ("true", "{required: false}"); "" leaves it unsealed
	// (ext-sealed.md §1).
	Sealed string
	// Listed makes the service's cards public (spec §2.1, §12.2): kontra seal reads the card without a
	// credential (ext-sealed.md §5.1).
	Listed bool
}

// MCPService is one service of Options.MCP.
type MCPService struct {
	URL   string   // a direct backend's endpoint URL; "" is a tunnel service
	Allow []string // allow: (default org:default)
	// ToolsAllow and ToolsDeny are tools: {allow, deny} (spec §11.3); a nil ToolsAllow writes no allow.
	ToolsAllow []string
	ToolsDeny  []string
	Tasks      string // tasks: durable (default), passthrough or off
	Sealed     string // sealed: as YAML, as for TunnelService
	// SessionIdle is session_idle_s (0: the default, 3600).
	SessionIdle int
	Retries     *int // task.retries; nil: Options.Retries
}

// PeerSecretEnv is the variable the replicas read the peer hop's secret from (peer.secret_env).
const PeerSecretEnv = "KONTRA_PEER_SECRET"

// tunnelled reports whether name is one of Options.Tunnel.
func (o Options) tunnelled(name string) bool {
	_, ok := o.Tunnel[name]
	return ok
}

// Harness is one running test environment.
type Harness struct {
	PostgresURL string
	Temporal    string // host:port of the dev server, as the test dials it
	Gates       [2]*Gate
	Upstream    *slowagent.Agent            // the service slow
	Upstreams   map[string]*slowagent.Agent // every service's upstream, slow included
	APIKey      string                      // buyer A's key
	APIKeyB     string                      // buyer B's key: another buyer of the same tenant
	Pool        *pgxpool.Pool               // probes
	TC          client.Client               // the test's own Temporal client (histories)
	// TemporalProxy is set with Options.TemporalProxy: the address the replicas dial.
	TemporalProxy *Proxy
	// PostgresProxy is set with Options.PostgresProxy: the address replica A dials for Postgres.
	PostgresProxy *Proxy
	// Front is public_url: new connections go to the replica it is routed to (replica A at start).
	Front *Front
	Dir   string // config, logs
	// Bucket is the gate's bucket with Options.Blobs.
	Bucket *Bucket
	// PeerSecret is the peer hop's shared secret with Options.Peer.
	PeerSecret string
	// SigningKey is the PEM file both replicas sign cards with (signing_key, D166): one key, one JWKS.
	SigningKey string

	run         string
	opts        Options
	ext         *External // the gate Attach attached to; nil for one Start started
	bin         string
	cli         string
	configPath  string
	configPaths [2]string // each replica's kontra.yaml: configPath, or replica A's copy with PostgresProxy
	pg          *postgres
	temporal    *exec.Cmd
	tempLog     *os.File
	closeOnce   sync.Once
	closeErr    error

	mu      sync.Mutex
	nodes   []*Node            // every `kontra connect` started, killed at Close
	cleanup []func() error     // what tests added (sellers, fake doubles), run at Close, last first
	agents  []*slowagent.Agent // slowagents started for a test (Agent), closed at Close
	pgids   []int              // process groups of the stock servers the harness started (CheckLoopback)
}

// Gate is one kontra-gate replica subprocess.
type Gate struct {
	Name      string
	URL       string // http://127.0.0.1:<port>
	Addr      string // 127.0.0.1:<port>, what the Front dials
	ReplicaID string // what the process registered in gate_replicas
	LogPath   string // its JSON log, at debug level (D171's marker scans read it)
	PeerAddr  string // 127.0.0.1:<port> of its peer listener with Options.Peer, else ""
	cmd       *exec.Cmd
	done      chan struct{}
	waitErr   error
	logFile   *os.File
}

var (
	rootOnce sync.Once
	rootDir  string
	rootErr  error

	buildOnce sync.Once
	gateBin   string
	buildErr  error
)

// TempRoot is the one directory every file the harness writes lives under: the built gate, and per test
// the shared kontra.yaml (which holds the Postgres URL and its password), the replicas' logs and
// Temporal's. Under scripts/test_e2e.sh it is $TMPDIR/kontra-e2e-<session> (SessionEnv), which the script
// removes after the run and reports as a leftover when a test binary that died (a go test -timeout panic,
// a crash) could not; otherwise a fresh directory per test binary. TestMain removes it after the run.
func TempRoot() (string, error) {
	rootOnce.Do(func() {
		if s := os.Getenv(SessionEnv); s != "" {
			rootDir = filepath.Join(os.TempDir(), "kontra-e2e-"+strings.Map(func(r rune) rune {
				switch {
				case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
					return r
				}
				return '_'
			}, s))
			rootErr = os.MkdirAll(rootDir, 0o700)
			return
		}
		rootDir, rootErr = os.MkdirTemp("", "kontra-e2e-")
	})
	return rootDir, rootErr
}

// gateBinary builds ./cmd/kontra-gate once per test binary, under TempRoot.
func gateBinary() (string, error) {
	buildOnce.Do(func() {
		root, err := TempRoot()
		if err != nil {
			buildErr = err
			return
		}
		dir, err := os.MkdirTemp(root, "bin-")
		if err != nil {
			buildErr = err
			return
		}
		gateBin = filepath.Join(dir, "kontra-gate")
		// The kontra_e2e tag lets the gate resolve PrivateHost from KONTRA_E2E_HOSTS (internal/gate's
		// e2e_hosts.go); a release is built without it.
		cmd := exec.Command("go", "build", "-tags", "kontra_e2e", "-o", gateBin, "kontra.sh/kontra/cmd/kontra-gate")
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("e2e: build kontra-gate: %v\n%s", err, out)
		}
	})
	return gateBin, buildErr
}

// removeRoot deletes TempRoot (the built binary, and anything a test left); TestMain calls it after the run.
func removeRoot() {
	if rootDir != "" {
		_ = os.RemoveAll(rootDir)
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// freePorts returns n distinct free loopback ports, reserved for the minute that follows. Every probe
// listener stays open until all n are chosen: a probe closed before the next one binds can hand the same
// port out twice (a replica's --listen and --metrics-listen once collided that way). And each port is left
// in TIME_WAIT by a connection its listening side closes first: for a minute the kernel then gives it to no
// other bind(:0), such as the Temporal development server's own choice of ports for its internal services
// (a collision there once panicked its start), while an explicit bind still succeeds, Go setting
// SO_REUSEADDR on every listener. The temporal CLI's devserver.GetFreePort reserves its ports the same way.
func freePorts(n int) ([]int, error) {
	ports := make([]int, 0, n)
	for i := 0; i < n; i++ {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		defer l.Close()
		c, err := net.Dial("tcp", l.Addr().String())
		if err != nil {
			return nil, err
		}
		defer c.Close() // after the accepted side: the listening side closes first
		a, err := l.Accept()
		if err != nil {
			return nil, err
		}
		a.Close()
		ports = append(ports, l.Addr().(*net.TCPAddr).Port)
	}
	return ports, nil
}

// Start brings the environment up. On an error, everything already started is removed.
func Start(ctx context.Context, o Options) (h *Harness, err error) {
	h = &Harness{run: randomHex(6), opts: o, Upstreams: map[string]*slowagent.Agent{}}
	defer func() {
		if err != nil {
			if cerr := h.Close(); cerr != nil {
				err = errors.Join(err, cerr)
			}
			h = nil
		}
	}()
	root, err := TempRoot()
	if err != nil {
		return h, err
	}
	if h.Dir, err = os.MkdirTemp(root, "run-"+h.run+"-"); err != nil {
		return h, err
	}
	if h.cli, err = temporalCLI(o.TemporalCLI); err != nil {
		return h, err
	}
	if h.bin, err = gateBinary(); err != nil {
		return h, err
	}
	if h.pg, err = startPostgres(ctx, o.PostgresImage, h.run); err != nil {
		return h, err
	}
	h.PostgresURL = h.pg.url
	if out, err := h.gateCommand(ctx, "migrate", "--postgres", h.PostgresURL, "up"); err != nil {
		return h, fmt.Errorf("e2e: kontra-gate migrate: %w\n%s", err, out)
	}
	if h.Pool, err = pgxpool.New(ctx, h.PostgresURL); err != nil {
		return h, err
	}
	if err := h.startTemporal(ctx); err != nil {
		return h, err
	}
	temporalAddr := h.Temporal
	if o.TemporalProxy {
		if h.TemporalProxy, err = NewProxy(h.Temporal); err != nil {
			return h, err
		}
		temporalAddr = h.TemporalProxy.Addr
	}
	if h.TC, err = client.Dial(client.Options{HostPort: h.Temporal, Namespace: Namespace, Logger: quietTemporal()}); err != nil {
		return h, err
	}

	services := map[string]slowagent.Config{"slow": o.Upstream}
	for name, c := range o.Services {
		services[name] = c
	}
	for name, c := range services {
		a := slowagent.New(c)
		if _, err := a.Start(context.Background()); err != nil {
			return h, err
		}
		h.Upstreams[name] = a
	}
	h.Upstream = h.Upstreams["slow"]

	nports := 4
	if o.Peer {
		nports = 6 // and each replica's peer listener
		h.PeerSecret = randomHex(32)
	}
	ports, err := freePorts(nports)
	if err != nil {
		return h, err
	}
	if h.Front, err = NewFront(); err != nil {
		return h, err
	}
	if h.SigningKey, err = writeSigningKey(h.Dir); err != nil {
		return h, err
	}
	if o.Blobs {
		if h.Bucket, err = StartBucket(); err != nil {
			return h, err
		}
	}
	if err := h.writeConfig(temporalAddr, ports[0]); err != nil {
		return h, err
	}
	h.configPaths = [2]string{h.configPath, h.configPath}
	if o.PostgresProxy {
		u, err := url.Parse(h.PostgresURL)
		if err != nil {
			return h, err
		}
		if h.PostgresProxy, err = NewProxy(u.Host); err != nil {
			return h, err
		}
		u.Host = h.PostgresProxy.Addr
		if h.configPaths[0], err = h.writeConfigCopy("kontra-A.yaml", u.String()); err != nil {
			return h, err
		}
	}
	if h.APIKey, err = h.enroll(ctx, "e2e-a"); err != nil {
		return h, err
	}
	if h.APIKeyB, err = h.enroll(ctx, "e2e-b"); err != nil {
		return h, err
	}
	for i, name := range []string{"A", "B"} {
		peerPort := 0
		if o.Peer {
			peerPort = ports[4+i]
		}
		if h.Gates[i], err = h.startGate(name, h.configPaths[i], ports[2*i], ports[2*i+1], peerPort); err != nil {
			return h, err
		}
	}
	h.Front.Route(h.Gates[0])
	// A tunnel service has no card before a node connects, and an MCP service none at all: readiness waits on
	// the direct A2A ones only.
	direct := slices.DeleteFunc(slices.Sorted(mapKeys(services)), o.tunnelled)
	for _, g := range h.Gates {
		if err := h.waitReady(ctx, g, direct); err != nil {
			return h, err
		}
		// The id the replica logged is the one it registered: GateByReplica maps attempts.replica_id with it.
		if _, err := h.ReplicaID(ctx, g); err != nil {
			return h, err
		}
	}
	if h.Gates[0].ReplicaID == h.Gates[1].ReplicaID {
		return h, fmt.Errorf("e2e: both replicas logged the replica id %s", h.Gates[0].ReplicaID)
	}
	return h, nil
}

func mapKeys[V any](m map[string]V) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

func quietTemporal() tlog.Logger {
	return tlog.NewStructuredLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// temporalCLI returns the pinned CLI: $KONTRA_TEMPORAL_CLI (or the option), checked to report Server 1.32.
func temporalCLI(path string) (string, error) {
	if path == "" {
		path = os.Getenv(TemporalCLIEnv)
	}
	if path == "" {
		return "", fmt.Errorf("e2e: set %s to the pinned temporal CLI (1.9.1, %s)", TemporalCLIEnv, TemporalServerVersion)
	}
	out, err := exec.Command(path, "--version").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("e2e: %s --version: %v", path, err)
	}
	if !strings.Contains(string(out), TemporalServerVersion) {
		return "", fmt.Errorf("e2e: %s is %q, not the pinned %s", path, strings.TrimSpace(string(out)), TemporalServerVersion)
	}
	return path, nil
}

// procAttr starts a child in its own process group, killed if the test process dies.
func procAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}

// PrivateHost is a name every gate of the harness resolves to PrivateAddr when it checks a push URL
// (KONTRA_E2E_HOSTS, honoured by the kontra_e2e build only): ssrf_webhook's "a name resolving to a private
// address" (spec §16, D80), on any machine. .invalid never resolves anywhere else.
const (
	PrivateHost = "webhook.private.e2e.invalid"
	PrivateAddr = "10.0.0.1"
)

func (h *Harness) env() []string {
	base := os.Environ()
	if h.ext != nil && h.ext.Env != nil {
		base = slices.Clone(h.ext.Env)
	}
	return append(base, RunEnv+"="+h.run, "KONTRA_E2E_HOSTS="+PrivateHost+"="+PrivateAddr)
}

// gateEnv is a replica's environment: env, and the secrets only the gate reads (the bucket's credentials and the
// peer hop's secret), which no node and no stock server ever gets (D185).
func (h *Harness) gateEnv() []string {
	env := h.env()
	if h.Bucket != nil {
		env = append(env, h.Bucket.Env()...)
	}
	if h.PeerSecret != "" {
		env = append(env, PeerSecretEnv+"="+h.PeerSecret)
	}
	return env
}

// Run is the run's id: the value of KONTRA_E2E_RUN on every process the harness starts.
func (h *Harness) Run() string { return h.run }

// gateCommand runs a short kontra-gate command and returns its combined output.
func (h *Harness) gateCommand(ctx context.Context, args ...string) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if h.ext != nil {
		return h.ext.Gate(cctx, args...)
	}
	cmd := exec.CommandContext(cctx, h.bin, args...)
	cmd.Env = h.gateEnv()
	cmd.SysProcAttr = procAttr()
	return cmd.CombinedOutput()
}

// enroll mints an API key with `kontra-gate enroll --api-key <name>`: a new key, and with it a new buyer.
func (h *Harness) enroll(ctx context.Context, name string) (string, error) {
	out, err := h.gateStdout(ctx, "enroll", "-f", h.configPath, "--api-key", name)
	if err != nil {
		return "", err
	}
	key := strings.TrimSpace(string(out))
	if !strings.HasPrefix(key, "kk_") {
		return "", fmt.Errorf("e2e: enroll printed %q, not a key", key)
	}
	return key, nil
}

// gateStdout runs a short kontra-gate command and returns its stdout; its stderr is in the error. An attached
// harness runs it where the gate runs (External.Gate).
func (h *Harness) gateStdout(ctx context.Context, args ...string) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if h.ext != nil {
		return h.ext.Gate(cctx, args...)
	}
	cmd := exec.CommandContext(cctx, h.bin, args...)
	cmd.Env = h.gateEnv()
	cmd.SysProcAttr = procAttr()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("e2e: kontra-gate %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return stdout.Bytes(), nil
}

// writeConfig writes the one kontra.yaml both replicas share; each gets its own listeners by flag, so
// listen: is replica A's. public_url is the Front's: the origin of every served card's interfaces and the
// one a node's hello must name (spec/tunnel.md §4). The tunnel's channel binding is none: the replicas do
// not terminate TLS, and the Front and the nodes are on loopback (spec/tunnel.md §4, D69). Both replicas sign
// cards with one key (signing_key, D166), so a card served by either verifies against the one JWKS; with
// Options.Blobs the bucket is blobs:, and with Options.Peer each replica's peer listener is set by flag.
func (h *Harness) writeConfig(temporalAddr string, listenPort int) error {
	retries := 2
	if h.opts.Retries != nil {
		retries = *h.opts.Retries
	}
	drain := 10 * time.Second
	if h.opts.DrainTimeout > 0 {
		drain = h.opts.DrainTimeout
	}
	var b strings.Builder
	fmt.Fprintf(&b, `# kontra.yaml of an e2e run: both replicas share it; each sets --listen and --metrics-listen.
listen: 127.0.0.1:%d
public_url: %s
postgres: %q
temporal: { address: %q, namespace: %s }
metrics: { listen: "127.0.0.1:0" }
auth: { api_keys: true }
drain_timeout: %s
push: { allow_private: %t }
tunnel: { channel_binding: none }
signing_key: %q
`, listenPort, h.Front.URL, h.PostgresURL, temporalAddr, Namespace, drain, h.opts.PushAllowPrivate, "file:"+h.SigningKey)
	if h.Bucket != nil {
		fmt.Fprintf(&b, "blobs: %q\n", h.Bucket.URL)
	}
	if h.opts.Peer {
		// Each replica's own --peer-listen replaces this one, which only has to be valid.
		fmt.Fprintf(&b, "peer: { listen: \"127.0.0.1:1\", secret_env: %s }\n", PeerSecretEnv)
	}
	b.WriteString("services:\n")
	names := slices.Collect(mapKeys(h.Upstreams))
	for name := range h.opts.Tunnel {
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	for _, name := range names {
		if ts, ok := h.opts.Tunnel[name]; ok {
			r := retries
			if ts.Retries != nil {
				r = *ts.Retries
			}
			fmt.Fprintf(&b, `  %s:
    kind: a2a
    backend: { tunnel: {} }
    allow: ["org:default"]
    task: { timeout: 10m, retries: %d }
`, name, r)
			if ts.Sealed != "" {
				fmt.Fprintf(&b, "    sealed: %s\n", ts.Sealed)
			}
			if ts.Listed {
				b.WriteString("    listed: true\n")
			}
			continue
		}
		fmt.Fprintf(&b, `  %s:
    kind: a2a
    backend: { a2a: { url: %q, allow_private: true } }
    allow: ["org:default"]
    task: { timeout: 10m, retries: %d }
`, name, h.Upstreams[name].URL(), retries)
		if keys, ok := h.opts.SellerKeys[name]; ok {
			fmt.Fprintf(&b, "    seller_keys: %s\n", keys)
		}
	}
	for _, name := range slices.Sorted(mapKeys(h.opts.MCP)) {
		m := h.opts.MCP[name]
		fmt.Fprintf(&b, "  %s:\n    kind: mcp\n", name)
		if m.URL != "" {
			fmt.Fprintf(&b, "    backend: { mcp: { url: %q, allow_private: true } }\n", m.URL)
		} else {
			b.WriteString("    backend: { tunnel: {} }\n")
		}
		allow := m.Allow
		if allow == nil {
			allow = []string{"org:default"}
		}
		fmt.Fprintf(&b, "    allow: %s\n", yamlList(allow))
		if m.ToolsAllow != nil || m.ToolsDeny != nil {
			var parts []string
			if m.ToolsAllow != nil {
				parts = append(parts, "allow: "+yamlList(m.ToolsAllow))
			}
			if m.ToolsDeny != nil {
				parts = append(parts, "deny: "+yamlList(m.ToolsDeny))
			}
			fmt.Fprintf(&b, "    tools: { %s }\n", strings.Join(parts, ", "))
		}
		if m.Tasks != "" {
			fmt.Fprintf(&b, "    tasks: %s\n", m.Tasks)
		}
		if m.Sealed != "" {
			fmt.Fprintf(&b, "    sealed: %s\n", m.Sealed)
		}
		if m.SessionIdle > 0 {
			fmt.Fprintf(&b, "    session_idle_s: %d\n", m.SessionIdle)
		}
		r := retries
		if m.Retries != nil {
			r = *m.Retries
		}
		fmt.Fprintf(&b, "    task: { timeout: 10m, retries: %d }\n", r)
	}
	h.configPath = filepath.Join(h.Dir, "kontra.yaml")
	return os.WriteFile(h.configPath, []byte(b.String()), 0o600)
}

// yamlList writes a flow sequence of quoted strings.
func yamlList(xs []string) string {
	q := make([]string, len(xs))
	for i, x := range xs {
		q[i] = strconv.Quote(x)
	}
	return "[" + strings.Join(q, ", ") + "]"
}

// writeSigningKey writes signing_key's PEM file under dir, 0600 (jws.LoadKeySet refuses a file others may read):
// one ECDSA P-256 key in PKCS #8, the key both replicas sign with.
func writeSigningKey(dir string) (string, error) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", err
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "signing.pem")
	return path, os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600)
}

// writeConfigCopy writes a copy of the shared kontra.yaml named name whose postgres: is postgresURL.
func (h *Harness) writeConfigCopy(name, postgresURL string) (string, error) {
	b, err := os.ReadFile(h.configPath)
	if err != nil {
		return "", err
	}
	from, to := fmt.Sprintf("postgres: %q\n", h.PostgresURL), fmt.Sprintf("postgres: %q\n", postgresURL)
	if !bytes.Contains(b, []byte(from)) {
		return "", fmt.Errorf("e2e: %s has no %q line", h.configPath, strings.TrimSpace(from))
	}
	path := filepath.Join(h.Dir, name)
	return path, os.WriteFile(path, bytes.Replace(b, []byte(from), []byte(to), 1), 0o600)
}

// startGate starts one replica from the configuration file configPath, logging at debug level (D171), with its
// peer listener on peerPort when it is not 0, and waits for its "replica ready to serve" line.
func (h *Harness) startGate(name, configPath string, port, metricsPort, peerPort int) (*Gate, error) {
	addr := "127.0.0.1:" + strconv.Itoa(port)
	g := &Gate{Name: name, URL: "http://" + addr, Addr: addr, LogPath: filepath.Join(h.Dir, "gate-"+name+".log"),
		done: make(chan struct{})}
	f, err := os.Create(g.LogPath)
	if err != nil {
		return nil, err
	}
	g.logFile = f
	args := []string{"serve", "-f", configPath, "--log-level", "debug",
		"--listen", "127.0.0.1:" + strconv.Itoa(port), "--metrics-listen", "127.0.0.1:" + strconv.Itoa(metricsPort)}
	if peerPort != 0 {
		g.PeerAddr = "127.0.0.1:" + strconv.Itoa(peerPort)
		args = append(args, "--peer-listen", g.PeerAddr)
	}
	g.cmd = exec.Command(h.bin, args...)
	g.cmd.Env = h.gateEnv()
	g.cmd.Stdout, g.cmd.Stderr = f, f
	g.cmd.SysProcAttr = procAttr()
	if err := g.cmd.Start(); err != nil {
		f.Close()
		return nil, fmt.Errorf("e2e: start gate %s: %w", name, err)
	}
	go func() {
		g.waitErr = g.cmd.Wait()
		close(g.done)
	}()
	deadline := time.Now().Add(90 * time.Second)
	for g.ReplicaID == "" {
		if time.Now().After(deadline) {
			return g, fmt.Errorf("e2e: gate %s did not log 'replica ready to serve' within 90 s:\n%s", name, tail(g.LogPath, 40))
		}
		select {
		case <-g.done:
			return g, fmt.Errorf("e2e: gate %s exited at start (%v):\n%s", name, g.waitErr, tail(g.LogPath, 40))
		case <-time.After(100 * time.Millisecond):
		}
		g.ReplicaID = readyReplica(g.LogPath)
	}
	return g, nil
}

// readyReplica finds the replica id in the gate's "replica ready to serve" log line (JSON).
func readyReplica(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		if !bytes.Contains(sc.Bytes(), []byte(`"replica ready to serve"`)) {
			continue
		}
		var line struct {
			Replica string `json:"replica"`
		}
		if json.Unmarshal(sc.Bytes(), &line) == nil {
			return line.Replica
		}
	}
	return ""
}

// waitReady waits until the gate answers /healthz 200 and serves every service's card (its keeper has
// fetched the upstream card).
func (h *Harness) waitReady(ctx context.Context, g *Gate, services []string) error {
	deadline := time.Now().Add(60 * time.Second)
	for {
		ok := status(ctx, http.MethodGet, g.URL+"/healthz", "") == http.StatusOK
		for _, s := range services {
			ok = ok && status(ctx, http.MethodGet, g.URL+"/agents/"+s+"/.well-known/agent-card.json", h.APIKey) == http.StatusOK
		}
		if ok {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("e2e: gate %s not ready (healthz and cards) within 60 s:\n%s", g.Name, tail(g.LogPath, 40))
		}
		select {
		case <-g.done:
			return fmt.Errorf("e2e: gate %s exited: %v\n%s", g.Name, g.waitErr, tail(g.LogPath, 40))
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func status(ctx context.Context, method, u, key string) int {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, method, u, nil)
	if err != nil {
		return 0
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

// tail returns the last n lines of a file.
func tail(path string, n int) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return "(" + err.Error() + ")"
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// LogTail returns the last lines of every replica's log, every node's and the Temporal server's, for a
// failed test.
func (h *Harness) LogTail(n int) string {
	var b strings.Builder
	for _, g := range h.Gates {
		if g != nil {
			fmt.Fprintf(&b, "--- gate %s (replica %s) ---\n%s\n", g.Name, g.ReplicaID, tail(g.LogPath, n))
		}
	}
	h.mu.Lock()
	nodes := slices.Clone(h.nodes)
	h.mu.Unlock()
	for _, nd := range nodes {
		fmt.Fprintf(&b, "--- node %s (key %s) ---\n%s\n%s\n", nd.Name, nd.KeyID, tail(nd.OutPath, n/4), tail(nd.LogPath, n))
	}
	if h.tempLog != nil {
		fmt.Fprintf(&b, "--- temporal ---\n%s\n", tail(h.tempLog.Name(), n))
	}
	return b.String()
}

// Close stops and removes everything Start started: the replicas (SIGKILL, also when stopped), the
// upstreams, the Temporal server and the proxy, the database or the container, and the directory.
func (h *Harness) Close() error {
	h.closeOnce.Do(func() {
		var errs []error
		// The nodes first: a node outliving its gate only redials.
		h.mu.Lock()
		nodes, cleanup, agents := slices.Clone(h.nodes), slices.Clone(h.cleanup), slices.Clone(h.agents)
		h.mu.Unlock()
		for _, nd := range nodes {
			if err := nd.Kill(); err != nil {
				errs = append(errs, err)
			}
		}
		for i := len(cleanup) - 1; i >= 0; i-- {
			if err := cleanup[i](); err != nil {
				errs = append(errs, err)
			}
		}
		for _, a := range agents {
			a.Close()
		}
		if h.Front != nil {
			h.Front.Close()
		}
		for _, g := range h.Gates {
			if g != nil {
				if err := g.Kill(); err != nil {
					errs = append(errs, err)
				}
				g.logFile.Close()
			}
		}
		for _, a := range h.Upstreams {
			a.Close()
		}
		if h.Bucket != nil {
			h.Bucket.Close()
		}
		if h.TC != nil {
			h.TC.Close()
		}
		if h.TemporalProxy != nil {
			h.TemporalProxy.Close()
		}
		if h.PostgresProxy != nil {
			h.PostgresProxy.Close()
		}
		if h.temporal != nil {
			if err := stopProcess(h.temporal, 10*time.Second); err != nil {
				errs = append(errs, fmt.Errorf("e2e: stop temporal: %w", err))
			}
			h.tempLog.Close()
		}
		if h.Pool != nil {
			h.Pool.Close()
		}
		if h.pg != nil {
			if err := h.pg.remove(); err != nil {
				errs = append(errs, err)
			}
		}
		if h.Dir != "" {
			_ = os.RemoveAll(h.Dir)
		}
		h.closeErr = errors.Join(errs...)
	})
	return h.closeErr
}

// stopProcess sends SIGTERM to the process group, then SIGKILL after grace.
func stopProcess(cmd *exec.Cmd, grace time.Duration) error {
	if cmd.Process == nil {
		return nil
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	select {
	case <-done:
		return nil
	case <-time.After(grace):
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	select {
	case <-done:
		return nil
	case <-time.After(10 * time.Second):
		return fmt.Errorf("pid %d did not exit after SIGKILL", cmd.Process.Pid)
	}
}

// Kill sends SIGKILL to the replica (no drain), as replica_kill needs, and waits until it is gone. A
// stopped replica is killed too.
func (g *Gate) Kill() error {
	if g.cmd == nil || g.cmd.Process == nil {
		return nil
	}
	select {
	case <-g.done:
		return nil
	default:
	}
	_ = syscall.Kill(-g.cmd.Process.Pid, syscall.SIGKILL)
	select {
	case <-g.done:
		return nil
	case <-time.After(10 * time.Second):
		return fmt.Errorf("e2e: gate %s (pid %d) did not exit after SIGKILL", g.Name, g.cmd.Process.Pid)
	}
}

// Term sends SIGTERM: the replica drains (spec §14.3, D76) and exits.
func (g *Gate) Term() error { return g.cmd.Process.Signal(syscall.SIGTERM) }

// Wait waits up to within for the replica's process to exit and returns how it exited.
func (g *Gate) Wait(within time.Duration) error {
	select {
	case <-g.done:
		return g.waitErr
	case <-time.After(within):
		return fmt.Errorf("e2e: gate %s still running after %v", g.Name, within)
	}
}

// Stop sends SIGSTOP: the replica's process stays alive but runs nothing (a GC pause or partition, D77).
func (g *Gate) Stop() error { return g.cmd.Process.Signal(syscall.SIGSTOP) }

// Continue sends SIGCONT to a stopped replica.
func (g *Gate) Continue() error { return g.cmd.Process.Signal(syscall.SIGCONT) }

// Stopped reports whether the replica's process is stopped (state T in /proc/<pid>/stat).
func (g *Gate) Stopped() bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(g.cmd.Process.Pid) + "/stat")
	if err != nil {
		return false
	}
	// pid (comm) state ...: the state follows the last ')'.
	i := bytes.LastIndexByte(b, ')')
	return i >= 0 && i+2 < len(b) && b[i+2] == 'T'
}

// Exited reports whether the replica's process has ended.
func (g *Gate) Exited() bool {
	select {
	case <-g.done:
		return true
	default:
		return false
	}
}

// Svc is the gate's {svc} URL of a service (single tenant: /agents/{name}).
func (g *Gate) Svc(name string) string { return g.URL + "/agents/" + name }

// Other returns the replica that is not g; nil when g is neither (a nil g included), so a test never
// mistakes an unknown replica for one of the two.
func (h *Harness) Other(g *Gate) *Gate {
	switch {
	case g == nil:
		return nil
	case g == h.Gates[0]:
		return h.Gates[1]
	case g == h.Gates[1]:
		return h.Gates[0]
	}
	return nil
}

// GateByReplica returns the replica whose id is id, nil if none.
func (h *Harness) GateByReplica(id string) *Gate {
	for _, g := range h.Gates {
		if g != nil && g.ReplicaID == id {
			return g
		}
	}
	return nil
}

// --- Postgres ---

// DatabasePrefix starts the name of every database the harness creates on $KONTRA_TEST_PG: kontra_e2e_,
// then this session's id (scripts/test_e2e.sh) when there is one, so the script can drop what a crashed
// test binary left behind without touching another run's.
func DatabasePrefix() string {
	p := "kontra_e2e_"
	if s := os.Getenv(SessionEnv); s != "" {
		p += strings.Map(func(r rune) rune {
			switch {
			case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
				return r
			case r >= 'A' && r <= 'Z':
				return r + ('a' - 'A')
			}
			return '_'
		}, s) + "_"
	}
	return p
}

type postgres struct {
	url    string
	remove func() error
}

// startPostgres creates a fresh database on $KONTRA_TEST_PG, or runs a container when it is unset.
func startPostgres(ctx context.Context, image, run string) (*postgres, error) {
	if server := os.Getenv(PostgresEnv); server != "" {
		return createDatabase(ctx, server, DatabasePrefix()+run)
	}
	if image == "" {
		image = "postgres:17-alpine"
	}
	name := "kontra-e2e-pg-" + run
	out, err := exec.CommandContext(ctx, "docker", "run", "-d", "--rm", "--name", name, "--label", "kontra-e2e="+run,
		"--label", "kontra-e2e-session="+os.Getenv(SessionEnv),
		"-e", "POSTGRES_USER=kontra", "-e", "POSTGRES_PASSWORD=kontra", "-e", "POSTGRES_DB=kontra",
		"-p", "127.0.0.1::5432", image).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("e2e: docker run %s (or set %s): %v\n%s", image, PostgresEnv, err, out)
	}
	pg := &postgres{remove: func() error {
		if out, err := exec.Command("docker", "rm", "-f", name).CombinedOutput(); err != nil {
			return fmt.Errorf("e2e: docker rm -f %s: %v\n%s", name, err, out)
		}
		return nil
	}}
	portOut, err := exec.CommandContext(ctx, "docker", "port", name, "5432/tcp").Output()
	if err != nil {
		return pg, fmt.Errorf("e2e: docker port %s: %w", name, err)
	}
	line := strings.TrimSpace(strings.Split(string(portOut), "\n")[0])
	port := line[strings.LastIndex(line, ":")+1:]
	pg.url = "postgres://kontra:kontra@127.0.0.1:" + port + "/kontra?sslmode=disable"
	// The image restarts once during its init: wait for a query through the published port, twice apart.
	deadline := time.Now().Add(60 * time.Second)
	ok := 0
	for ok < 2 {
		if time.Now().After(deadline) {
			return pg, fmt.Errorf("e2e: the postgres container did not answer within 60 s")
		}
		if c, err := pgx.Connect(ctx, pg.url); err == nil {
			if _, err := c.Exec(ctx, "select 1"); err == nil {
				ok++
			} else {
				ok = 0
			}
			c.Close(ctx)
		} else {
			ok = 0
		}
		time.Sleep(500 * time.Millisecond)
	}
	return pg, nil
}

func createDatabase(ctx context.Context, server, name string) (*postgres, error) {
	u, err := url.Parse(server)
	if err != nil {
		return nil, fmt.Errorf("e2e: %s: %w", PostgresEnv, err)
	}
	admin, err := pgx.Connect(ctx, server)
	if err != nil {
		return nil, fmt.Errorf("e2e: connect to %s: %w", PostgresEnv, err)
	}
	defer admin.Close(context.Background())
	if _, err := admin.Exec(ctx, "create database "+pgx.Identifier{name}.Sanitize()); err != nil {
		return nil, fmt.Errorf("e2e: create database %s: %w", name, err)
	}
	u.Path = "/" + name
	return &postgres{url: u.String(), remove: func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		c, err := pgx.Connect(ctx, server)
		if err != nil {
			return fmt.Errorf("e2e: drop database %s: %w", name, err)
		}
		defer c.Close(context.Background())
		if _, err := c.Exec(ctx, "drop database if exists "+pgx.Identifier{name}.Sanitize()+" with (force)"); err != nil {
			return fmt.Errorf("e2e: drop database %s: %w", name, err)
		}
		return nil
	}}, nil
}

// --- Temporal ---

// startTemporal starts the pinned development server, in memory, on free loopback ports, with the
// settings spec §14.2 asks of the gate's server. A server that exits while it starts (its own port choice
// colliding with another process, say) is started again on fresh ports, three times at most; its panic
// line is in the error.
func (h *Harness) startTemporal(ctx context.Context) error {
	var err error
	for attempt := 1; attempt <= 3; attempt++ {
		if err = h.startTemporalOnce(ctx, attempt); !errors.Is(err, errTemporalExited) {
			return err
		}
	}
	return err
}

// errTemporalExited is a development server that exited before it was ready.
var errTemporalExited = errors.New("e2e: the Temporal development server exited at start")

func (h *Harness) startTemporalOnce(ctx context.Context, attempt int) error {
	ports, err := freePorts(3)
	if err != nil {
		return err
	}
	addr := "127.0.0.1:" + strconv.Itoa(ports[0])
	name := "temporal.log"
	if attempt > 1 {
		name = fmt.Sprintf("temporal-%d.log", attempt)
	}
	f, err := os.Create(filepath.Join(h.Dir, name))
	if err != nil {
		return err
	}
	cmd := exec.Command(h.cli, "server", "start-dev", "--headless", "--ip", "127.0.0.1",
		"--port", strconv.Itoa(ports[0]), "--http-port", strconv.Itoa(ports[1]), "--metrics-port", strconv.Itoa(ports[2]),
		"--namespace", Namespace, "--log-level", "warn",
		"--dynamic-config-value", "frontend.WorkerCommandsEnabled=true",
		"--dynamic-config-value", "system.enableCancelActivityWorkerCommand=true")
	cmd.Env = h.env()
	cmd.Stdout, cmd.Stderr = f, f
	cmd.SysProcAttr = procAttr()
	if err := cmd.Start(); err != nil {
		f.Close()
		return fmt.Errorf("e2e: start %s: %w", h.cli, err)
	}
	h.temporal, h.tempLog = cmd, f
	rctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	for {
		if err := temporalReady(rctx, addr); err == nil {
			h.Temporal = addr
			return nil
		}
		if exited(cmd.Process.Pid) {
			// Nothing has waited on it, so it is a zombie: reap it, and let Close find nothing to stop.
			_ = stopProcess(cmd, time.Second)
			h.temporal, h.tempLog = nil, nil
			f.Close()
			return fmt.Errorf("%w (attempt %d, %s): %s\n%s", errTemporalExited, attempt, addr,
				panicLine(f.Name()), tail(f.Name(), 40))
		}
		select {
		case <-rctx.Done():
			return fmt.Errorf("e2e: the Temporal development server on %s did not become ready:\n%s", addr, tail(f.Name(), 40))
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// panicLine is the first "panic:" line of a log, "" when there is none.
func panicLine(path string) string {
	b, _ := os.ReadFile(path)
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "panic:") {
			return l
		}
	}
	return ""
}

// exited reports whether the child pid has exited and not been waited for (a zombie, state Z).
func exited(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return true
	}
	i := bytes.LastIndexByte(b, ')')
	return i >= 0 && i+2 < len(b) && b[i+2] == 'Z'
}

func temporalReady(ctx context.Context, addr string) error {
	dctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	c, err := client.DialContext(dctx, client.Options{HostPort: addr, Namespace: Namespace, Logger: quietTemporal()})
	if err != nil {
		return err
	}
	defer c.Close()
	if _, err := c.CheckHealth(dctx, &client.CheckHealthRequest{}); err != nil {
		return err
	}
	ns, err := client.NewNamespaceClient(client.Options{HostPort: addr, Logger: quietTemporal()})
	if err != nil {
		return err
	}
	defer ns.Close()
	_, err = ns.Describe(dctx, Namespace)
	return err
}

// WorkflowHistory exports a task's TaskLifecycle history with `temporal workflow show -o json`.
func (h *Harness) WorkflowHistory(ctx context.Context, taskID a2a.TaskID) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, h.cli, "workflow", "show", "-o", "json", "--address", h.Temporal,
		"--namespace", Namespace, "--workflow-id", lifecycle.WorkflowID(taskID))
	cmd.Env = h.env()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("e2e: temporal workflow show %s: %v: %s", lifecycle.WorkflowID(taskID), err, stderr.String())
	}
	return out, nil
}

// WaitWorkflowClosed waits until the task's workflow has completed.
func (h *Harness) WaitWorkflowClosed(ctx context.Context, taskID a2a.TaskID, within time.Duration) error {
	deadline := time.Now().Add(within)
	for {
		d, err := h.TC.DescribeWorkflowExecution(ctx, lifecycle.WorkflowID(taskID), "")
		if err == nil && d.WorkflowExecutionInfo.GetCloseTime() != nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("e2e: workflow of %s still open after %v (%v)", taskID, within, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// --- Postgres probes ---

// Log is the store's log over the harness's database (Log.SnapshotAt, the fold of spec §9).
func (h *Harness) Log() *store.Log { return store.NewLog(h.Pool) }

// ReplicaID checks the replica id the process logged at start ("replica ready to serve") against
// gate_replicas, where the process registered it, and returns it. Start calls it for both replicas.
func (h *Harness) ReplicaID(ctx context.Context, g *Gate) (string, error) {
	if g.ReplicaID == "" {
		return "", fmt.Errorf("e2e: gate %s logged no replica id", g.Name)
	}
	var n int
	if err := h.Pool.QueryRow(ctx, `select count(*) from kontra.gate_replicas where replica_id = $1`, g.ReplicaID).Scan(&n); err != nil {
		return "", err
	}
	if n != 1 {
		return "", fmt.Errorf("e2e: replica %s of gate %s is not in gate_replicas", g.ReplicaID, g.Name)
	}
	return g.ReplicaID, nil
}

// Attempt is one attempts row.
type Attempt struct {
	N                 int
	Backend           string
	TunnelSession     string // "" for a direct attempt and a no_session one
	KeyID             string
	ReplicaID         string
	DispatchID        *uuid.UUID
	UpstreamTaskID    string
	UpstreamContextID string
	FirstUpstreamIDAt *time.Time
	EndedAt           *time.Time
	Outcome           string
}

// Attempts reads the task's attempts rows, by n.
func (h *Harness) Attempts(ctx context.Context, taskID a2a.TaskID) ([]Attempt, error) {
	rows, err := h.Pool.Query(ctx, `select n, backend, coalesce(tunnel_session::text, ''), coalesce(key_id, ''),
		coalesce(replica_id, ''), dispatch_id, coalesce(upstream_task_id, ''),
		coalesce(upstream_context_id, ''), first_upstream_id_at, ended_at, coalesce(outcome, '')
		from kontra.attempts where task_id = $1 order by n`, string(taskID))
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Attempt, error) {
		var a Attempt
		err := r.Scan(&a.N, &a.Backend, &a.TunnelSession, &a.KeyID, &a.ReplicaID, &a.DispatchID, &a.UpstreamTaskID,
			&a.UpstreamContextID, &a.FirstUpstreamIDAt, &a.EndedAt, &a.Outcome)
		return a, err
	})
}

// AttemptReplica reads attempts.replica_id of the task's latest attempt: the replica holding the dispatch.
func (h *Harness) AttemptReplica(ctx context.Context, taskID a2a.TaskID) (replicaID string, n int, err error) {
	err = h.Pool.QueryRow(ctx, `select coalesce(replica_id, ''), n from kontra.attempts where task_id = $1 order by n desc limit 1`,
		string(taskID)).Scan(&replicaID, &n)
	return replicaID, n, err
}

// Row is one task_events row.
type Row struct {
	Seq     int64
	Kind    string
	Payload json.RawMessage
}

// Rows reads the task's log rows in seq order.
func (h *Harness) Rows(ctx context.Context, taskID a2a.TaskID) ([]Row, error) {
	rows, err := h.Pool.Query(ctx, `select seq, kind, coalesce(payload::text, 'null') from kontra.task_events where task_id = $1 order by seq`, string(taskID))
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Row, error) {
		var (
			x Row
			p string
		)
		err := r.Scan(&x.Seq, &x.Kind, &p)
		x.Payload = json.RawMessage(p)
		return x, err
	})
}

// CarriedSeqs reads the seqs of the task's carried rows (task, status, artifact) from task_events: what a
// complete stream must have delivered, each once, in order.
func (h *Harness) CarriedSeqs(ctx context.Context, taskID a2a.TaskID) ([]int64, error) {
	rows, err := h.Pool.Query(ctx, `select seq from kontra.task_events where task_id = $1 and kind in ('task', 'status', 'artifact') order by seq`, string(taskID))
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int64])
}

// WorkItem is one work_items row.
type WorkItem struct {
	ID        uuid.UUID
	Payload   string // the sanitized request, as text
	Consumed  bool   // consumed_at is set: a turn's first dispatch took it (spec §14.1, D66)
	Delivered bool   // delivered_at is set: the upstream first answered it (D81)
}

// WorkItems reads the task's work items in the order they were created.
func (h *Harness) WorkItems(ctx context.Context, taskID a2a.TaskID) ([]WorkItem, error) {
	rows, err := h.Pool.Query(ctx, `select id, payload::text, consumed_at is not null, delivered_at is not null
		from kontra.work_items where task_id = $1 order by created_at, id`, string(taskID))
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (WorkItem, error) {
		var w WorkItem
		err := r.Scan(&w.ID, &w.Payload, &w.Consumed, &w.Delivered)
		return w, err
	})
}

// WorkItemOf returns the task's work item that carries the message messageID, nil when none does.
func (h *Harness) WorkItemOf(ctx context.Context, taskID a2a.TaskID, messageID string) (*WorkItem, error) {
	ws, err := h.WorkItems(ctx, taskID)
	if err != nil {
		return nil, err
	}
	for i := range ws {
		if strings.Contains(ws[i].Payload, `"messageId":"`+messageID+`"`) {
			return &ws[i], nil
		}
	}
	return nil, nil
}

// TaskState reads tasks.state.
func (h *Harness) TaskState(ctx context.Context, taskID a2a.TaskID) (string, error) {
	var s string
	err := h.Pool.QueryRow(ctx, `select state from kontra.tasks where id = $1`, string(taskID)).Scan(&s)
	return s, err
}

// --- a TCP proxy ---

// proxyMode is what a Proxy does with connections.
type proxyMode int

const (
	forwarding  proxyMode = iota
	cutting               // close the open connections, reset new ones at once (nothing listens)
	blackholing           // close the open connections, accept new ones and never answer (a hung peer)
)

// Proxy forwards TCP connections to a target. Cut closes every open connection and resets new ones at
// once (the peer is unreachable: a dial fails fast); Blackhole closes every open connection and then
// accepts new ones but never forwards a byte nor closes them (the peer hangs: a call waits for its own
// deadline); Restore forwards new connections again, closing the held ones.
type Proxy struct {
	Addr   string
	target string
	l      net.Listener

	mu    sync.Mutex
	mode  proxyMode
	conns map[net.Conn]struct{} // forwarded connections, both ends, and held ones
}

// NewProxy listens on a loopback port and forwards to target.
func NewProxy(target string) (*Proxy, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p := &Proxy{Addr: l.Addr().String(), target: target, l: l, conns: map[net.Conn]struct{}{}}
	go p.serve()
	return p, nil
}

func (p *Proxy) serve() {
	for {
		c, err := p.l.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		mode := p.mode
		if mode == blackholing {
			p.conns[c] = struct{}{} // held until Restore or Close
		}
		p.mu.Unlock()
		switch mode {
		case cutting:
			if tc, ok := c.(*net.TCPConn); ok {
				_ = tc.SetLinger(0)
			}
			c.Close()
		case forwarding:
			go p.forward(c)
		}
	}
}

func (p *Proxy) forward(c net.Conn) {
	u, err := net.DialTimeout("tcp", p.target, 5*time.Second)
	if err != nil {
		c.Close()
		return
	}
	p.mu.Lock()
	if p.mode != forwarding {
		p.mu.Unlock()
		c.Close()
		u.Close()
		return
	}
	p.conns[c], p.conns[u] = struct{}{}, struct{}{}
	p.mu.Unlock()
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(u, c); done <- struct{}{} }()
	go func() { _, _ = io.Copy(c, u); done <- struct{}{} }()
	<-done
	c.Close()
	u.Close()
	p.mu.Lock()
	delete(p.conns, c)
	delete(p.conns, u)
	p.mu.Unlock()
}

// set switches the mode and closes every connection open now.
func (p *Proxy) set(m proxyMode) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.mode = m
	for c := range p.conns {
		c.Close()
		delete(p.conns, c)
	}
}

// Cut closes every connection and refuses new ones until Restore.
func (p *Proxy) Cut() { p.set(cutting) }

// Blackhole closes every connection, then accepts new ones and holds them silent until Restore.
func (p *Proxy) Blackhole() { p.set(blackholing) }

// Restore forwards new connections again; the connections a blackhole held are closed.
func (p *Proxy) Restore() { p.set(forwarding) }

// Close stops the proxy and its connections.
func (p *Proxy) Close() {
	p.Cut()
	p.l.Close()
}
