// Package temporaltest gives integration tests a Temporal server with the gate's namespace.
//
// KONTRA_TEST_TEMPORAL names a running server (host:port) whose namespace "kontra" exists; the tests use
// it as it is. Otherwise the package starts one development server per test binary from the pinned CLI
// (KONTRA_TEMPORAL_CLI, else `temporal` on PATH, which must report Server 1.32; spec §0 pins the server)
// on free loopback ports, in memory, and stops it when the binary's TestMain calls Stop (or when the test
// process dies: the server runs with a parent-death signal). The server is handed out once a workflow has run
// on it end to end (warm). At most one runs at a time, which is what
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

	enums "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
	tlog "go.temporal.io/sdk/log"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
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
	exited  <-chan struct{} // closed once started has exited (its Wait has returned)
	address string
)

// errExited is a development server that exited before it was ready.
var errExited = errors.New("the development server exited at start")

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
	// A server that exits while it starts (its own port choice colliding with another process, say) is
	// started again on fresh ports, three times at most, as test/e2e's harness does.
	var err error
	for attempt := 1; attempt <= 3; attempt++ {
		var (
			a    string
			cmd  *exec.Cmd
			done <-chan struct{}
		)
		if a, cmd, done, err = start(); err == nil {
			address, started, exited = a, cmd, done
			return address
		}
		if !errors.Is(err, errExited) {
			break
		}
	}
	t.Fatalf("temporaltest: %v", err)
	return ""
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
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		_ = started.Process.Kill()
		<-exited
	}
	started, exited, address = nil, nil, ""
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

// freePorts returns n distinct free loopback ports, reserved for the minute that follows, as test/e2e's
// freePorts does. Every probe listener stays open until all n are chosen, and each port is left in
// TIME_WAIT by a connection its listening side closes first: for a minute the kernel then gives it to no
// other bind(:0), such as the development server's own choice of ports for its history, matching and worker
// services, while the server's explicit bind still succeeds. A port closed plainly could come back to the
// server that way: its worker failed to bind the frontend's port and the server exited (CI, 3 Nov).
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

func start() (string, *exec.Cmd, <-chan struct{}, error) {
	path, err := cli()
	if err != nil {
		return "", nil, nil, err
	}
	ports, err := freePorts(3)
	if err != nil {
		return "", nil, nil, err
	}
	addr := "127.0.0.1:" + strconv.Itoa(ports[0])
	cmd := exec.Command(path, "server", "start-dev", "--headless", "--ip", "127.0.0.1",
		"--port", strconv.Itoa(ports[0]), "--http-port", strconv.Itoa(ports[1]), "--metrics-port", strconv.Itoa(ports[2]),
		"--namespace", Namespace, "--log-level", "error")
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = io.Discard, &stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL, Setpgid: true}
	if err := cmd.Start(); err != nil {
		return "", nil, nil, fmt.Errorf("start %s: %w", path, err)
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for {
		err := ready(ctx, addr)
		if err == nil {
			if err = warm(ctx, addr); err == nil {
				return addr, cmd, done, nil
			}
		}
		select {
		case <-done:
			// stderr is complete once Wait has returned.
			return "", nil, nil, fmt.Errorf("%w (%s): %v: %s", errExited, addr, err, stderr.String())
		case <-ctx.Done():
			_ = cmd.Process.Kill()
			<-done
			return "", nil, nil, fmt.Errorf("the development server on %s did not become ready: %v: %s", addr, err, stderr.String())
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// warmName is the warm-up workflow's type, task queue and id, and its Update's name: nothing a test uses.
const warmName = "temporaltest-warmup"

// warm runs one workflow end to end on the server: started with an Update (UpdateWithStart, as the lifecycle
// starts a task), its Update completed and the workflow closed, through a worker of its own. The frontend's health
// and the namespace (ready) say only that the frontend has joined the cluster: history may not own its shards yet
// and matching may not serve a task queue, and a worker whose first polls fail backs off (the SDK's poll retry
// grows from 200 ms to 10 s), so a test's first Update could spend lifecycle.AcceptWait (10 s) on the server's own
// start, which a CI runner did once (TestStreamsOverTheRealStack, "the task's lifecycle did not answer in time").
// After a workflow has run, a test's workers poll a server that serves.
func warm(ctx context.Context, addr string) error {
	c, err := client.DialContext(ctx, client.Options{HostPort: addr, Namespace: Namespace, Logger: quiet()})
	if err != nil {
		return err
	}
	defer c.Close()
	w := worker.New(c, warmName, worker.Options{})
	w.RegisterWorkflowWithOptions(func(ctx workflow.Context) error {
		updated := false
		if err := workflow.SetUpdateHandler(ctx, warmName, func(workflow.Context) error {
			updated = true
			return nil
		}); err != nil {
			return err
		}
		return workflow.Await(ctx, func() bool { return updated })
	}, workflow.RegisterOptions{Name: warmName})
	if err := w.Start(); err != nil {
		return err
	}
	defer w.Stop()
	op := c.NewWithStartWorkflowOperation(client.StartWorkflowOptions{
		ID: warmName, TaskQueue: warmName, WorkflowIDConflictPolicy: enums.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING,
	}, warmName)
	h, err := c.UpdateWithStartWorkflow(ctx, client.UpdateWithStartWorkflowOptions{
		StartWorkflowOperation: op,
		UpdateOptions:          client.UpdateWorkflowOptions{UpdateName: warmName, UpdateID: warmName, WaitForStage: client.WorkflowUpdateStageCompleted},
	})
	if err != nil {
		return fmt.Errorf("warm-up update: %w", err)
	}
	if err := h.Get(ctx, nil); err != nil {
		return fmt.Errorf("warm-up update: %w", err)
	}
	if err := c.GetWorkflow(ctx, warmName, "").Get(ctx, nil); err != nil {
		return fmt.Errorf("warm-up workflow: %w", err)
	}
	return nil
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
