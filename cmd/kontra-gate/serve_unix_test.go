//go:build unix

package main

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"kontra.sh/kontra/internal/pgtest"
	"kontra.sh/kontra/internal/temporaltest"
)

const serveHelperEnv = "KONTRA_GATE_SERVE_HELPER"

// TestMain stops the Temporal development server the serve test started.
func TestMain(m *testing.M) {
	code := m.Run()
	temporaltest.Stop()
	os.Exit(code)
}

// The helper process of TestServeReloadsOnSIGHUPAndDrainsOnSIGTERM: `kontra-gate serve` as main runs it.
func TestServeHelper(t *testing.T) {
	if os.Getenv(serveHelperEnv) == "" {
		t.Skip("the helper process of TestServeReloadsOnSIGHUPAndDrainsOnSIGTERM")
	}
	err := run([]string{"serve", "-f", os.Getenv(serveHelperEnv), "--listen", os.Getenv("KONTRA_GATE_SERVE_LISTEN")}, os.Stdout, os.Stderr)
	fmt.Println("serve returned:", err)
	if err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

// `kontra-gate serve` as a process (spec §13.5, §14.3): on a migrated database with a key minted by
// `enroll --api-key`, it serves the service; SIGHUP applies a changed allow: list from the same file (the
// key is then the unknown-service 404, and admitted again after a second change); SIGTERM drains and the
// process exits 0, its gate_replicas row gone.
func TestServeReloadsOnSIGHUPAndDrainsOnSIGTERM(t *testing.T) {
	dsn := pgtest.Database(t)
	temporal := temporaltest.Address(t)
	listen := freeAddr(t)
	svc := func(allow string) string {
		return fmt.Sprintf("listen: %s\ntemporal: { address: %s, namespace: kontra }\nmetrics: { listen: \"127.0.0.1:0\" }\nauth: { api_keys: true }\ndrain_timeout: 30s\n"+
			"services:\n  hello:\n    kind: a2a\n    backend: { tunnel: {} }\n    allow: [%q]\n", listen, temporal, allow)
	}
	cfg := writeConfig(t, dsn, svc("org:default"))
	if _, err := runCmd(t, "migrate", "-f", cfg); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := runSplit(t, "enroll", "-f", cfg, "--api-key", "ci")
	if err != nil {
		t.Fatal(err)
	}
	key := strings.TrimSpace(stdout)

	cmd := exec.Command(os.Args[0], "-test.run=^TestServeHelper$", "-test.count=1")
	cmd.Env = append(os.Environ(), serveHelperEnv+"="+cfg, "KONTRA_GATE_SERVE_LISTEN="+listen)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr lockedBuffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	var lines []string
	var mu sync.Mutex
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			mu.Lock()
			lines = append(lines, sc.Text())
			mu.Unlock()
		}
		exited <- cmd.Wait()
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
		}
		if t.Failed() {
			mu.Lock()
			t.Logf("serve stdout:\n%s\nstderr:\n%s", strings.Join(lines, "\n"), stderr.String())
			mu.Unlock()
		}
	})

	base := "http://" + listen
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for !ok() {
			if time.Now().After(deadline) {
				t.Fatalf("%s: not within 30 s", what)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	status := func(path, token string) int {
		req, _ := http.NewRequest(http.MethodGet, base+path, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	// The card route of a tunnel service the key may call: 503 (no card in M1), not the unknown-service 404.
	waitFor("serving /healthz 200", func() bool { return status("/healthz", "") == http.StatusOK })
	if code := status("/agents/hello", key); code != http.StatusServiceUnavailable {
		t.Fatalf("the card of an admitted service: %d", code)
	}

	write := func(allow string) {
		t.Helper()
		if err := os.WriteFile(cfg, []byte("public_url: http://localhost:8080\npostgres: \""+dsn+"\"\n"+svc(allow)), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := cmd.Process.Signal(syscall.SIGHUP); err != nil {
			t.Fatal(err)
		}
	}
	write("key:kk_nobody_*")
	waitFor("SIGHUP: the key is no longer admitted", func() bool { return status("/agents/hello", key) == http.StatusNotFound })
	write("org:default")
	waitFor("SIGHUP: the key is admitted again", func() bool { return status("/agents/hello", key) == http.StatusServiceUnavailable })

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("serve after SIGTERM: %v (want exit 0)", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("serve did not exit within 60 s of SIGTERM")
	}
	mu.Lock()
	joined := strings.Join(lines, "\n")
	mu.Unlock()
	if !strings.Contains(joined, "serve returned: <nil>") {
		t.Fatalf("serve's own result: %s", joined)
	}
	for _, want := range []string{`"msg":"draining"`, `"msg":"drained"`, `"msg":"SIGHUP: services applied"`} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("serve's log has no %s", want)
		}
	}
	pool := pgtest.Pool(t, dsn)
	var n int
	if err := pool.QueryRow(context.Background(), `select count(*) from kontra.gate_replicas`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("gate_replicas after the drain: %d %v", n, err)
	}
}

type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
