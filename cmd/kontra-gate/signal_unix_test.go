//go:build unix

package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
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
