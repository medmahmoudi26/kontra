// Package temporaltest gives integration tests a Temporal server with the gate's namespace.
//
// KONTRA_TEST_TEMPORAL names a running server (host:port) whose namespace "kontra" exists; the tests use
// it as it is. Otherwise the package starts one development server per test binary from the pinned CLI
// (KONTRA_TEMPORAL_CLI, else `temporal` on PATH, which must report Server 1.32; spec §0 pins the server)
// on free loopback ports, in memory, and stops it when the binary's TestMain calls Stop (or when the test
// process dies: the server runs with a parent-death signal). At most one runs at a time, which is what
// `go test -p 1` gives. Under -short every caller is skipped; without -short a missing CLI fails the test
// rather than skipping it, so a run that tested nothing cannot read as a pass.
package temporaltest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"go.temporal.io/sdk/client"
	tlog "go.temporal.io/sdk/log"
)

// Env names an existing server, host:port; CLIEnv the temporal CLI to start a development server with.
const (
	Env    = "KONTRA_TEST_TEMPORAL"
	CLIEnv = "KONTRA_TEMPORAL_CLI"
)

// Namespace is the gate's namespace (temporal.namespace's default, spec §14.1).
const Namespace = "kontra"

// ServerVersion is what the CLI must report: the pinned server line (spec §0: 1.32.x).
const ServerVersion = "Server 1.32"

var (
	mu      sync.Mutex
	started *exec.Cmd
	address string
)

// Address returns the server's host:port, starting the development server on first use. It skips under
// -short and fails the test when no server can be had.
func Address(t testing.TB) string {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test: needs a Temporal server; skipped under -short")
	}
	if a := os.Getenv(Env); a != "" {
		return a
	}
	mu.Lock()
	defer mu.Unlock()
	if address != "" {
		return address
	}
	a, cmd, err := start()
	if err != nil {
		t.Fatalf("temporaltest: %v", err)
	}
	address, started = a, cmd
	return address
}

// Client dials the server's kontra namespace; it is closed when the test ends.
func Client(t testing.TB) client.Client {
	t.Helper()
	addr := Address(t)
	c, err := client.Dial(client.Options{HostPort: addr, Namespace: Namespace, Logger: quiet()})
	if err != nil {
		t.Fatalf("temporaltest: dial %s: %v", addr, err)
	}
	t.Cleanup(c.Close)
	return c
}

func quiet() tlog.Logger {
	return tlog.NewStructuredLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// Stop stops the development server this binary started, if any. Call it from TestMain after m.Run.
func Stop() {
	mu.Lock()
	defer mu.Unlock()
	if started == nil {
		return
	}
	_ = started.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		_ = started.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = started.Process.Kill()
		<-done
	}
	started, address = nil, ""
}

func cli() (string, error) {
	path := os.Getenv(CLIEnv)
	if path == "" {
		p, err := exec.LookPath("temporal")
		if err != nil {
			return "", fmt.Errorf("no temporal CLI: set %s to the pinned CLI (1.9.1, %s)", CLIEnv, ServerVersion)
		}
		path = p
	}
	out, err := exec.Command(path, "--version").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s --version: %v", path, err)
	}
	if !strings.Contains(string(out), ServerVersion) {
		return "", fmt.Errorf("%s is %q, not the pinned %s: set %s", path, strings.TrimSpace(string(out)), ServerVersion, CLIEnv)
	}
	return path, nil
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func start() (string, *exec.Cmd, error) {
	path, err := cli()
	if err != nil {
		return "", nil, err
	}
	var ports [3]int
	for i := range ports {
		if ports[i], err = freePort(); err != nil {
			return "", nil, err
		}
	}
	addr := "127.0.0.1:" + strconv.Itoa(ports[0])
	cmd := exec.Command(path, "server", "start-dev", "--headless", "--ip", "127.0.0.1",
		"--port", strconv.Itoa(ports[0]), "--http-port", strconv.Itoa(ports[1]), "--metrics-port", strconv.Itoa(ports[2]),
		"--namespace", Namespace, "--log-level", "error")
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = io.Discard, &stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL, Setpgid: true}
	if err := cmd.Start(); err != nil {
		return "", nil, fmt.Errorf("start %s: %w", path, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for {
		if err := ready(ctx, addr); err == nil {
			return addr, cmd, nil
		}
		select {
		case <-ctx.Done():
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return "", nil, fmt.Errorf("the development server on %s did not become ready: %s", addr, stderr.String())
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// ready reports whether the server answers in the kontra namespace.
func ready(ctx context.Context, addr string) error {
	dctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	c, err := client.DialContext(dctx, client.Options{HostPort: addr, Namespace: Namespace, Logger: quiet()})
	if err != nil {
		return err
	}
	defer c.Close()
	if _, err := c.CheckHealth(dctx, &client.CheckHealthRequest{}); err != nil {
		return err
	}
	ns, err := client.NewNamespaceClient(client.Options{HostPort: addr, Logger: quiet()})
	if err != nil {
		return err
	}
	defer ns.Close()
	if _, err := ns.Describe(dctx, Namespace); err != nil {
		return errors.Join(errors.New("namespace not ready"), err)
	}
	return nil
}
