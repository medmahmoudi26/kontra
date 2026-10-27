package e2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"kontra.sh/kontra/internal/mcprelay/mcptest"
)

// The M3 tests' MCP upstreams (D171, D185): the stock servers @modelcontextprotocol/server-everything@1.0.0
// (streamableHttp) and @modelcontextprotocol/server-filesystem@1.0.0 (stdio), installed once per test binary from
// examples/mcp's package-locks, and the in-repository dual-era double (internal/mcprelay/mcptest), served in the
// test process on 127.0.0.1.
//
// D185: every server the harness starts listens on 127.0.0.1 only. server-everything@1.0.0 calls app.listen(port),
// which binds every interface, so it runs with NODE_OPTIONS=--require=testdata/loopback.cjs, a preload that makes
// every listen naming no host (a port alone, a backlog, no port at all, an empty host) bind 127.0.0.1
// (TestLoopbackPreloadBindsLoopback); CheckLoopback asserts the result. The npm servers get a minimal environment: PATH,
// HOME (a directory of the run's), LANG, NODE_OPTIONS, the run's marker variables and GZIP_ALLOWED_DOMAINS set to a
// name that matches nothing; never KONTRA_TEST_PG, AWS_* or KONTRA_PEER_SECRET. The services deny get-env and
// gzip-file-as-resource (EverythingDeny) unless a test needs them, and none does in M3.

// OfflineEnv, set to 1, skips (saying why) a test whose pinned npm package cannot be installed; otherwise a failed
// install fails the test (the M3 plan's acceptance).
const OfflineEnv = "KONTRA_E2E_OFFLINE"

// EverythingDeny are the server-everything@1.0.0 tools every service of it denies (D185): get-env returns the
// server's environment, gzip-file-as-resource fetches URLs.
var EverythingDeny = []string{"get-env", "gzip-file-as-resource"}

var (
	npmMu    sync.Mutex
	npmDirs  = map[string]string{}
	npmErrs  = map[string]error{}
	repoRoot = filepath.Join("..", "..")
)

// npmInstall installs the package-lock of src (a directory relative to the repository's root: examples/mcp/<name>,
// or test/e2e/testdata/<name>) once per test binary, under TempRoot, and returns the directory. npm runs with a
// minimal environment of its own and the run's marker variables (markerEnv), so that scripts/test_e2e.sh finds an
// install that outlived a test binary which died during it; its download cache is kept between runs in the user's
// cache directory, so a second run needs no network.
func npmInstall(src string) (string, error) {
	npmMu.Lock()
	defer npmMu.Unlock()
	if dir, ok := npmDirs[src]; ok {
		return dir, npmErrs[src]
	}
	dir, err := func() (string, error) {
		npm, err := exec.LookPath("npm")
		if err != nil {
			return "", fmt.Errorf("npm is not on PATH")
		}
		if _, err := exec.LookPath("node"); err != nil {
			return "", fmt.Errorf("node is not on PATH")
		}
		root, err := TempRoot()
		if err != nil {
			return "", err
		}
		dir, err := os.MkdirTemp(root, "npm-"+filepath.Base(src)+"-")
		if err != nil {
			return "", err
		}
		for _, f := range []string{"package.json", "package-lock.json"} {
			b, err := os.ReadFile(filepath.Join(repoRoot, src, f))
			if err != nil {
				return "", err
			}
			if err := os.WriteFile(filepath.Join(dir, f), b, 0o600); err != nil {
				return "", err
			}
		}
		cache := filepath.Join(dir, ".npm")
		if uc, err := os.UserCacheDir(); err == nil {
			cache = filepath.Join(uc, "kontra-e2e", "npm")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, npm, "ci", "--ignore-scripts", "--no-audit", "--no-fund", "--prefer-offline")
		cmd.Dir = dir
		cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "npm_config_cache=" + cache,
			"npm_config_update_notifier=false"}, markerEnv("npm-ci-"+randomHex(4))...)
		cmd.SysProcAttr = procAttr()
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("npm ci (%s): %v\n%s", src, err, lastLines(out, 8))
		}
		return dir, nil
	}()
	npmDirs[src], npmErrs[src] = dir, err
	return dir, err
}

// installed is npmInstall for a test: a failed install fails it, or skips it under KONTRA_E2E_OFFLINE=1.
func installed(t testingTB, src, pkg string) string {
	t.Helper()
	dir, err := npmInstall(src)
	if err != nil {
		if os.Getenv(OfflineEnv) == "1" {
			t.Skipf("%s: the pinned package cannot be installed here (%s=1): %v", pkg, OfflineEnv, err)
		}
		t.Fatalf("%s: the pinned package cannot be installed (set %s=1 to skip instead): %v", pkg, OfflineEnv, err)
	}
	return dir
}

// testingTB is the part of testing.TB the helpers here use.
type testingTB interface {
	Helper()
	Fatalf(format string, args ...any)
	Skipf(format string, args ...any)
	Logf(format string, args ...any)
}

func lastLines(b []byte, n int) string {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// FilesystemCommand is the --serve value that runs server-filesystem@1.0.0 over stdio with dir as its command-line
// directory: "stdio:'<node>' '<entry>' '<dir>'".
func (h *Harness) FilesystemCommand(t testingTB, dir string) string {
	t.Helper()
	pkgDir := installed(t, "examples/mcp/fs-stdio", "server-filesystem@1.0.0")
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	entry := filepath.Join(pkgDir, "node_modules", "@modelcontextprotocol", "server-filesystem", "dist", "index.js")
	if _, err := os.Stat(entry); err != nil {
		t.Fatalf("npm ci installed no server-filesystem: %v", err)
	}
	return "stdio:'" + nodeBin + "' '" + entry + "' '" + dir + "'"
}

// StockServer is a stock MCP server process the harness started.
type StockServer struct {
	URL     string // its endpoint
	Name    string
	LogPath string
	cmd     *exec.Cmd
	done    chan struct{}
}

// StartEverything runs server-everything@1.0.0 (streamableHttp) on a free loopback port, as
// examples/mcp/everything's run.sh does, and returns it once it listens; the harness stops its process group
// when it closes.
func (h *Harness) StartEverything(t testingTB, name string) *StockServer {
	t.Helper()
	return h.startEverything(t, name, "examples/mcp/everything", "server-everything@1.0.0")
}

// StartEverything2 runs server-everything@2.0.0 from testdata/everything-2's lock, which needs Node 24 (D171).
func (h *Harness) StartEverything2(t testingTB, name string) *StockServer {
	t.Helper()
	return h.startEverything(t, name, "test/e2e/testdata/everything-2", "server-everything@2.0.0")
}

func (h *Harness) startEverything(t testingTB, name, src, pkg string) *StockServer {
	t.Helper()
	pkgDir := installed(t, src, pkg)
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	preload, err := filepath.Abs(filepath.Join("testdata", "loopback.cjs"))
	if err != nil {
		t.Fatalf("%v", err)
	}
	ports, err := freePorts(1)
	if err != nil {
		t.Fatalf("%v", err)
	}
	home, err := os.MkdirTemp(h.Dir, "home-"+name+"-")
	if err != nil {
		t.Fatalf("%v", err)
	}
	s := &StockServer{Name: name, URL: fmt.Sprintf("http://127.0.0.1:%d/mcp", ports[0]),
		LogPath: filepath.Join(h.Dir, "stock-"+name+".log"), done: make(chan struct{})}
	logf, err := os.Create(s.LogPath)
	if err != nil {
		t.Fatalf("%v", err)
	}
	s.cmd = exec.Command(nodeBin, filepath.Join(pkgDir, "node_modules", "@modelcontextprotocol", "server-everything", "dist", "index.js"), "streamableHttp")
	s.cmd.Dir = home
	s.cmd.Env = h.stockEnv(home, "PORT="+strconv.Itoa(ports[0]), "NODE_OPTIONS=--require="+preload)
	s.cmd.Stdout, s.cmd.Stderr = logf, logf
	s.cmd.SysProcAttr = procAttr()
	if err := s.cmd.Start(); err != nil {
		logf.Close()
		t.Fatalf("start server-everything: %v", err)
	}
	go func() {
		_ = s.cmd.Wait()
		logf.Close()
		close(s.done)
	}()
	h.mu.Lock()
	h.pgids = append(h.pgids, s.cmd.Process.Pid)
	h.mu.Unlock()
	h.OnClose(s.Stop)
	deadline := time.Now().Add(60 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", ports[0]), time.Second)
		if err == nil {
			c.Close()
			break
		}
		select {
		case <-s.done:
			t.Fatalf("server-everything exited at start:\n%s", tail(s.LogPath, 20))
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("server-everything does not listen within 60 s:\n%s", tail(s.LogPath, 20))
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err := h.CheckLoopback(); err != nil {
		t.Fatalf("%v", err)
	}
	return s
}

// stockEnv is a stock server's whole environment (D185): PATH, HOME, LANG, the run's markers, GZIP_ALLOWED_DOMAINS,
// and extra.
func (h *Harness) stockEnv(home string, extra ...string) []string {
	env := append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "LANG=C.UTF-8", "GZIP_ALLOWED_DOMAINS=example.invalid"},
		markerEnv(h.run)...)
	return append(env, extra...)
}

// markerEnv are the marker variables scripts/test_e2e.sh finds a leftover by: RunEnv=run, and SessionEnv when the
// script set it.
func markerEnv(run string) []string {
	env := []string{RunEnv + "=" + run}
	if s := os.Getenv(SessionEnv); s != "" {
		env = append(env, SessionEnv+"="+s)
	}
	return env
}

// Stop kills the server's process group and waits for it.
func (s *StockServer) Stop() error {
	if s.cmd == nil || s.cmd.Process == nil {
		return nil
	}
	_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
	select {
	case <-s.done:
		return nil
	case <-time.After(10 * time.Second):
		return fmt.Errorf("e2e: %s (pid %d) did not exit after SIGKILL", s.Name, s.cmd.Process.Pid)
	}
}

// Double serves the dual-era Go double on 127.0.0.1 for the harness's life and returns its /mcp endpoint.
func (h *Harness) Double(d *mcptest.Server) string {
	srv := httptest.NewServer(d) // 127.0.0.1
	h.OnClose(func() error {
		srv.CloseClientConnections()
		srv.Close()
		return nil
	})
	return srv.URL + "/mcp"
}

// CheckLoopback fails if a process the harness started (a replica, a node, a node's stdio child, a stock server, the
// Temporal server) listens on any address but a loopback one (D185). It reads `ss -Hltnp`, and the processes are
// those whose environment carries this run's KONTRA_E2E_RUN, and the members of the stock servers' process groups.
func (h *Harness) CheckLoopback() error {
	out, err := exec.Command("ss", "-Hltnp").Output()
	if err != nil {
		return fmt.Errorf("e2e: ss -Hltnp: %w", err)
	}
	ours := h.processes()
	var bad []string
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 6 {
			continue
		}
		local := f[3]
		host, _, err := net.SplitHostPort(local)
		if err != nil {
			continue
		}
		if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil && ip.IsLoopback() {
			continue
		}
		for _, pid := range ssPids(strings.Join(f[5:], " ")) {
			if ours[pid] {
				bad = append(bad, fmt.Sprintf("pid %d listens on %s (%s)", pid, local, cmdline(pid)))
			}
		}
	}
	if len(bad) > 0 {
		return errors.New("e2e: a process the harness started listens off loopback (D185): " + strings.Join(bad, "; "))
	}
	return nil
}

// ssPids reads the pids of ss's users:(("node",pid=123,fd=4),…) column.
func ssPids(users string) []int {
	var out []int
	for {
		i := strings.Index(users, "pid=")
		if i < 0 {
			return out
		}
		users = users[i+4:]
		j := strings.IndexAny(users, ",)")
		if j < 0 {
			return out
		}
		if pid, err := strconv.Atoi(users[:j]); err == nil {
			out = append(out, pid)
		}
		users = users[j:]
	}
}

// processes are the pids of every process of this run: KONTRA_E2E_RUN=<run> in its environment, or in the process
// group of a stock server.
func (h *Harness) processes() map[int]bool {
	h.mu.Lock()
	groups := map[int]bool{}
	for _, g := range h.pgids {
		groups[g] = true
	}
	h.mu.Unlock()
	marker := []byte(RunEnv + "=" + h.run + "\x00")
	out := map[int]bool{}
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if env, err := os.ReadFile(filepath.Join("/proc", e.Name(), "environ")); err == nil && containsEnv(env, marker) {
			out[pid] = true
			continue
		}
		if pg, err := syscall.Getpgid(pid); err == nil && groups[pg] {
			out[pid] = true
		}
	}
	return out
}

// containsEnv reports whether a NUL-separated environment holds the entry marker (NUL-terminated).
func containsEnv(env, marker []byte) bool {
	for _, kv := range bytes.Split(env, []byte{0}) {
		if len(kv) > 0 && bytes.Equal(append(kv, 0), marker) {
			return true
		}
	}
	return false
}

// cmdline is a process's command line, for a report.
func cmdline(pid int) string {
	b, _ := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	s := strings.TrimSpace(strings.ReplaceAll(string(b), "\x00", " "))
	if len(s) > 120 {
		s = s[:120]
	}
	return s
}

// dualEraTool is a tool object for the double: name, an object input schema, and taskSupport when not "".
func dualEraTool(name, taskSupport string) []byte {
	if taskSupport == "" {
		return []byte(fmt.Sprintf(`{"name":%q,"inputSchema":{"type":"object"}}`, name))
	}
	return []byte(fmt.Sprintf(`{"name":%q,"inputSchema":{"type":"object"},"execution":{"taskSupport":%q}}`, name, taskSupport))
}
