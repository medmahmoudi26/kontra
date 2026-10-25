// Package e2e holds the gate's end-to-end tests (spec/relay.md §16) and their harness.
//
// Each test starts one environment and removes it when it ends, never in parallel: a database (a fresh
// one on the server $KONTRA_TEST_PG names, else a postgres:17-alpine container on a loopback port),
// migrated by `kontra-gate migrate`; one Temporal development server from the pinned CLI
// ($KONTRA_TEMPORAL_CLI, which must report Server 1.32; spec §0), in memory, namespace kontra, with
// frontend.WorkerCommandsEnabled and system.enableCancelActivityWorkerCommand on (spec §14.2); the slowagent
// upstreams in the test process; and two kontra-gate replicas as subprocesses of the binary built once from
// ./cmd/kontra-gate, sharing one kontra.yaml and given their own --listen and --metrics-listen. Two API keys
// with distinct buyers are minted with `kontra-gate enroll --api-key`. The buyer is a minimal SSE client
// that keeps every `id:` line, because a2a-go's client ignores them; unary calls use a2a-go's own client.
// Everything the harness starts it removes, including on failure; every process it starts carries
// KONTRA_E2E_RUN in its environment, so `make test-e2e` can prove none outlived the run.
//
// The pinned CLI is temporal 1.9.1 (Server 1.32.0); its linux amd64 binary's sha256 is
// 712217694cac9f786a72d4b6c876cd15f3dcf1bf31434d9fed6af25242a327a0, which CI checks before it sets
// KONTRA_TEMPORAL_CLI. The `temporal` on a developer's PATH is often another version, so the harness never
// falls back to it.
//
// The tests are M1's (D80): relay_resume (TestRelayResume), replica_kill direct (TestReplicaKillDirect,
// and its SIGSTOP variant TestReplicaStallDirect, D77), cancel_forwarded (TestCancelForwarded),
// push_delivery (TestPushDelivery), ssrf_webhook (TestSSRFWebhook), card_compat (TestCardCompat),
// cross_buyer (TestCrossBuyer) and version_conflict (TestVersionConflict), plus a follow-up turn through
// the real stack (TestFollowUp, TestFollowUpTemporalUnreachable). Each test that ends a task exports its
// TaskLifecycle history and replays it against the current workflow; with KONTRA_RECORD_HISTORIES=1 it
// also writes it to internal/lifecycle/testdata/histories, never over an existing file.
package e2e

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
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
	// TemporalProxy routes the replicas' Temporal connection through a TCP proxy the test can cut
	// (Harness.TemporalProxy), to make Temporal unreachable for them while the test still reads it.
	TemporalProxy bool
	// Retries is services.slow.task.retries (default 2, spec §6.4).
	Retries *int
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
	Dir           string // config, logs

	run        string
	opts       Options
	bin        string
	cli        string
	configPath string
	pg         *postgres
	temporal   *exec.Cmd
	tempLog    *os.File
	closeOnce  sync.Once
	closeErr   error
}

// Gate is one kontra-gate replica subprocess.
type Gate struct {
	Name      string
	URL       string // http://127.0.0.1:<port>
	ReplicaID string // what the process registered in gate_replicas
	LogPath   string
	cmd       *exec.Cmd
	done      chan struct{}
	waitErr   error
	logFile   *os.File
}

var (
	buildOnce sync.Once
	buildDir  string
	gateBin   string
	buildErr  error
)

// gateBinary builds ./cmd/kontra-gate once per test binary.
func gateBinary() (string, error) {
	buildOnce.Do(func() {
		buildDir, buildErr = os.MkdirTemp("", "kontra-e2e-bin-")
		if buildErr != nil {
			return
		}
		gateBin = filepath.Join(buildDir, "kontra-gate")
		cmd := exec.Command("go", "build", "-o", gateBin, "kontra.sh/kontra/cmd/kontra-gate")
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("e2e: build kontra-gate: %v\n%s", err, out)
		}
	})
	return gateBin, buildErr
}

// removeBuild deletes the built binary; TestMain calls it after the run.
func removeBuild() {
	if buildDir != "" {
		_ = os.RemoveAll(buildDir)
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
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
	if h.Dir, err = os.MkdirTemp("", "kontra-e2e-"+h.run+"-"); err != nil {
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

	var ports [4]int
	for i := range ports {
		if ports[i], err = freePort(); err != nil {
			return h, err
		}
	}
	if err := h.writeConfig(temporalAddr, ports[0]); err != nil {
		return h, err
	}
	if h.APIKey, err = h.enroll(ctx, "e2e-a"); err != nil {
		return h, err
	}
	if h.APIKeyB, err = h.enroll(ctx, "e2e-b"); err != nil {
		return h, err
	}
	for i, name := range []string{"A", "B"} {
		if h.Gates[i], err = h.startGate(name, ports[2*i], ports[2*i+1]); err != nil {
			return h, err
		}
	}
	for _, g := range h.Gates {
		if err := h.waitReady(ctx, g, slices.Sorted(mapKeys(services))); err != nil {
			return h, err
		}
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

func (h *Harness) env() []string {
	return append(os.Environ(), RunEnv+"="+h.run)
}

// gateCommand runs a short kontra-gate command and returns its combined output.
func (h *Harness) gateCommand(ctx context.Context, args ...string) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, h.bin, args...)
	cmd.Env = h.env()
	cmd.SysProcAttr = procAttr()
	return cmd.CombinedOutput()
}

// enroll mints an API key with `kontra-gate enroll --api-key <name>`: a new key, and with it a new buyer.
func (h *Harness) enroll(ctx context.Context, name string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, h.bin, "enroll", "-f", h.configPath, "--api-key", name)
	cmd.Env = h.env()
	cmd.SysProcAttr = procAttr()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("e2e: kontra-gate enroll --api-key %s: %v\n%s", name, err, stderr.String())
	}
	key := strings.TrimSpace(stdout.String())
	if !strings.HasPrefix(key, "kk_") {
		return "", fmt.Errorf("e2e: enroll printed %q, not a key", key)
	}
	return key, nil
}

// writeConfig writes the one kontra.yaml both replicas share; each gets its own listeners by flag.
func (h *Harness) writeConfig(temporalAddr string, publicPort int) error {
	retries := 2
	if h.opts.Retries != nil {
		retries = *h.opts.Retries
	}
	var b strings.Builder
	fmt.Fprintf(&b, `# kontra.yaml of an e2e run: both replicas share it; each sets --listen and --metrics-listen.
listen: 127.0.0.1:%d
public_url: http://127.0.0.1:%d
postgres: %q
temporal: { address: %q, namespace: %s }
metrics: { listen: "127.0.0.1:0" }
auth: { api_keys: true }
drain_timeout: 10s
push: { allow_private: %t }
services:
`, publicPort, publicPort, h.PostgresURL, temporalAddr, Namespace, h.opts.PushAllowPrivate)
	for _, name := range slices.Sorted(mapKeys(h.Upstreams)) {
		fmt.Fprintf(&b, `  %s:
    kind: a2a
    backend: { a2a: { url: %q, allow_private: true } }
    allow: ["org:default"]
    task: { timeout: 10m, retries: %d }
`, name, h.Upstreams[name].URL(), retries)
	}
	h.configPath = filepath.Join(h.Dir, "kontra.yaml")
	return os.WriteFile(h.configPath, []byte(b.String()), 0o600)
}

// startGate starts one replica and waits for its "replica ready to serve" line.
func (h *Harness) startGate(name string, port, metricsPort int) (*Gate, error) {
	g := &Gate{Name: name, URL: "http://127.0.0.1:" + strconv.Itoa(port), LogPath: filepath.Join(h.Dir, "gate-"+name+".log"),
		done: make(chan struct{})}
	f, err := os.Create(g.LogPath)
	if err != nil {
		return nil, err
	}
	g.logFile = f
	g.cmd = exec.Command(h.bin, "serve", "-f", h.configPath,
		"--listen", "127.0.0.1:"+strconv.Itoa(port), "--metrics-listen", "127.0.0.1:"+strconv.Itoa(metricsPort))
	g.cmd.Env = h.env()
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

// LogTail returns the last lines of every replica's log and the Temporal server's, for a failed test.
func (h *Harness) LogTail(n int) string {
	var b strings.Builder
	for _, g := range h.Gates {
		if g != nil {
			fmt.Fprintf(&b, "--- gate %s (replica %s) ---\n%s\n", g.Name, g.ReplicaID, tail(g.LogPath, n))
		}
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
		if h.TC != nil {
			h.TC.Close()
		}
		if h.TemporalProxy != nil {
			h.TemporalProxy.Close()
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

// Stop sends SIGSTOP: the replica's process stays alive but runs nothing (a GC pause or partition, D77).
func (g *Gate) Stop() error { return g.cmd.Process.Signal(syscall.SIGSTOP) }

// Continue sends SIGCONT to a stopped replica.
func (g *Gate) Continue() error { return g.cmd.Process.Signal(syscall.SIGCONT) }

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

// Other returns the replica that is not g.
func (h *Harness) Other(g *Gate) *Gate {
	if g == h.Gates[0] {
		return h.Gates[1]
	}
	return h.Gates[0]
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
// settings spec §14.2 asks of the gate's server.
func (h *Harness) startTemporal(ctx context.Context) error {
	var ports [3]int
	for i := range ports {
		p, err := freePort()
		if err != nil {
			return err
		}
		ports[i] = p
	}
	addr := "127.0.0.1:" + strconv.Itoa(ports[0])
	f, err := os.Create(filepath.Join(h.Dir, "temporal.log"))
	if err != nil {
		return err
	}
	h.tempLog = f
	cmd := exec.Command(h.cli, "server", "start-dev", "--headless", "--ip", "127.0.0.1",
		"--port", strconv.Itoa(ports[0]), "--http-port", strconv.Itoa(ports[1]), "--metrics-port", strconv.Itoa(ports[2]),
		"--namespace", Namespace, "--log-level", "warn",
		"--dynamic-config-value", "frontend.WorkerCommandsEnabled=true",
		"--dynamic-config-value", "system.enableCancelActivityWorkerCommand=true")
	cmd.Env = h.env()
	cmd.Stdout, cmd.Stderr = f, f
	cmd.SysProcAttr = procAttr()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("e2e: start %s: %w", h.cli, err)
	}
	h.temporal = cmd
	rctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	for {
		if err := temporalReady(rctx, addr); err == nil {
			h.Temporal = addr
			return nil
		}
		select {
		case <-rctx.Done():
			return fmt.Errorf("e2e: the Temporal development server on %s did not become ready:\n%s", addr, tail(f.Name(), 20))
		case <-time.After(250 * time.Millisecond):
		}
	}
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

// ReplicaID reads the replica id the process registered in gate_replicas: the id it logged at start,
// checked against the table.
func (h *Harness) ReplicaID(ctx context.Context, g *Gate) (string, error) {
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
	rows, err := h.Pool.Query(ctx, `select n, backend, coalesce(replica_id, ''), dispatch_id, coalesce(upstream_task_id, ''),
		coalesce(upstream_context_id, ''), first_upstream_id_at, ended_at, coalesce(outcome, '')
		from kontra.attempts where task_id = $1 order by n`, string(taskID))
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Attempt, error) {
		var a Attempt
		err := r.Scan(&a.N, &a.Backend, &a.ReplicaID, &a.DispatchID, &a.UpstreamTaskID, &a.UpstreamContextID,
			&a.FirstUpstreamIDAt, &a.EndedAt, &a.Outcome)
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

// TaskState reads tasks.state.
func (h *Harness) TaskState(ctx context.Context, taskID a2a.TaskID) (string, error) {
	var s string
	err := h.Pool.QueryRow(ctx, `select state from kontra.tasks where id = $1`, string(taskID)).Scan(&s)
	return s, err
}

// --- a TCP proxy ---

// Proxy forwards TCP connections to a target until it is cut: Cut closes every open connection and
// resets new ones at once, Restore forwards again.
type Proxy struct {
	Addr   string
	target string
	l      net.Listener

	mu    sync.Mutex
	cut   bool
	conns map[net.Conn]struct{}
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
		cut := p.cut
		p.mu.Unlock()
		if cut {
			if tc, ok := c.(*net.TCPConn); ok {
				_ = tc.SetLinger(0)
			}
			c.Close()
			continue
		}
		go p.forward(c)
	}
}

func (p *Proxy) forward(c net.Conn) {
	u, err := net.DialTimeout("tcp", p.target, 5*time.Second)
	if err != nil {
		c.Close()
		return
	}
	p.mu.Lock()
	if p.cut {
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

// Cut closes every forwarded connection and refuses new ones until Restore.
func (p *Proxy) Cut() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cut = true
	for c := range p.conns {
		c.Close()
	}
}

// Restore forwards new connections again.
func (p *Proxy) Restore() {
	p.mu.Lock()
	p.cut = false
	p.mu.Unlock()
}

// Close stops the proxy and its connections.
func (p *Proxy) Close() {
	p.Cut()
	p.l.Close()
}
