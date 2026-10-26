//go:build unix

package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const signalHelperEnv = "KONTRA_GATE_SIGNAL_HELPER"

// The helper process of TestSecondSignalStopsAtOnce: it catches signals as serve does, reports the drain
// cause, then waits out a drain longer than the test.
func TestDrainSignalHelper(t *testing.T) {
	if os.Getenv(signalHelperEnv) != "1" {
		t.Skip("the helper process of TestSecondSignalStopsAtOnce")
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	defer drainOnSignal(slog.New(slog.NewTextHandler(os.Stdout, nil)), cancel)()
	fmt.Println("ready")
	<-ctx.Done()
	fmt.Println("cause:", context.Cause(ctx))
	time.Sleep(time.Minute)
	fmt.Println("drain finished")
}

// The first SIGTERM drains (the context is canceled with the drain cause); a second one, during the
// drain, ends the process at once instead of being swallowed until drain_timeout.
func TestSecondSignalStopsAtOnce(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestDrainSignalHelper$", "-test.count=1")
	cmd.Env = append(os.Environ(), signalHelperEnv+"=1")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	var waitErr error
	lines := make(chan string, 64)
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
		waitErr = cmd.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
			t.Error("the helper process did not exit after SIGKILL")
		}
	})
	expect := func(prefix string) {
		t.Helper()
		deadline := time.After(10 * time.Second)
		for {
			select {
			case l, ok := <-lines:
				if !ok {
					t.Fatalf("the helper exited before printing %q", prefix)
				}
				if strings.HasPrefix(l, prefix) {
					return
				}
			case <-deadline:
				t.Fatalf("no %q from the helper within 10 s", prefix)
			}
		}
	}
	expect("ready")
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	expect("cause: drain: received terminated")
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
		var ee *exec.ExitError
		if !errors.As(waitErr, &ee) {
			t.Fatalf("helper exit = %v, want killed by SIGTERM", waitErr)
		}
		if ws, ok := ee.Sys().(syscall.WaitStatus); !ok || !ws.Signaled() || ws.Signal() != syscall.SIGTERM {
			t.Errorf("helper exit = %v, want killed by SIGTERM", waitErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a second SIGTERM during the drain did not stop the process within 5 s")
	}
}

// A SIGHUP while `kontra-gate serve` is still starting (gate.New: the migration check, up to 30 s of
// Temporal dial, the services sync) does not end the process, which is the signal's default action: an
// operator or a service manager that reloads right after a (re)start must not stop the gate. Here the start
// waits on a Postgres that accepts the connection and never answers.
func TestSIGHUPDuringStartDoesNotStopTheGate(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan struct{}, 16)
	var conns []net.Conn
	var connsMu sync.Mutex
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			connsMu.Lock()
			conns = append(conns, c)
			connsMu.Unlock()
			accepted <- struct{}{}
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		connsMu.Lock()
		for _, c := range conns {
			c.Close()
		}
		connsMu.Unlock()
	})
	dsn := "postgres://kontra@" + ln.Addr().String() + "/kontra?sslmode=disable&connect_timeout=60"
	cfg := writeConfig(t, dsn, "listen: "+freeAddr(t)+"\ntemporal: { address: \"127.0.0.1:1\", namespace: kontra }\nmetrics: { listen: \"127.0.0.1:0\" }\nauth: { api_keys: true }\n")

	cmd := exec.Command(os.Args[0], "-test.run=^TestServeHelper$", "-test.count=1")
	cmd.Env = append(os.Environ(), serveHelperEnv+"="+cfg, "KONTRA_GATE_SERVE_LISTEN="+freeAddr(t))
	var out lockedBuffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	var waitErr error
	go func() { waitErr = cmd.Wait(); close(exited) }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
			t.Error("the serve process did not exit after SIGKILL")
		}
		if t.Failed() {
			t.Logf("serve output:\n%s", out.String())
		}
	})
	select {
	case <-accepted: // serve is in gate.New, connecting to Postgres
	case <-exited:
		t.Fatalf("serve exited before it connected to Postgres: %v", waitErr)
	case <-time.After(30 * time.Second):
		t.Fatal("serve did not connect to Postgres within 30 s")
	}
	if err := cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
		t.Fatalf("a SIGHUP during the start ended serve: %v", waitErr)
	case <-time.After(time.Second):
	}
	// SIGTERM still stops the start (the context New dials with is canceled).
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
		var ee *exec.ExitError
		if errors.As(waitErr, &ee) {
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				t.Fatalf("serve was killed by %v, want it to return", ws.Signal())
			}
		}
		if !strings.Contains(out.String(), "serve returned:") {
			t.Fatalf("serve did not return: %v", waitErr)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("serve did not stop within 30 s of SIGTERM during its start")
	}
}
