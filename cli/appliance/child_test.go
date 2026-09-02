package appliance

import (
	"bytes"
	"errors"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// logBuffer is a writer the supervisor can hold while a test reads it. The child's output is
// written from exec's own copy goroutine, so a bare bytes.Buffer is a data race and `-race` says
// so.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func startShell(t *testing.T, log *logBuffer, script string) *Child {
	t.Helper()
	c, err := StartChild(ChildOptions{
		Name: "orchestrator",
		Path: "/bin/sh",
		Args: []string{"-c", script},
		Dir:  t.TempDir(),
		Env:  []string{"PATH=/usr/bin:/bin"},
		Log:  log,
	})
	if err != nil {
		t.Fatalf("StartChild: %v", err)
	}
	t.Cleanup(func() { _ = c.Stop(2 * time.Second) })
	return c
}

func waitFor(t *testing.T, c *Child, d time.Duration) {
	t.Helper()
	select {
	case <-c.Done():
	case <-time.After(d):
		t.Fatalf("the child did not exit within %s", d)
	}
}

// A supervised child's output is the binary's output. The failure this prevents is the one that
// cost this repo a run: a process that was up, healthy on every surface, and spinning on a
// DNS name — with the only copy of that fact inside a stream nobody was reading.
func TestChildOutputReachesTheLogStream(t *testing.T) {
	var log logBuffer
	c := startShell(t, &log, "echo hello from stdout; echo and from stderr >&2")
	waitFor(t, c, 10*time.Second)

	out := log.String()
	for _, want := range []string{"orchestrator | hello from stdout", "orchestrator | and from stderr"} {
		if !strings.Contains(out, want) {
			t.Errorf("the log does not contain %q; it has:\n%s", want, out)
		}
	}
}

// A child that dies mid-line still wrote that line, and it is usually the one that says why.
func TestChildOutputWithoutATrailingNewlineIsNotLost(t *testing.T) {
	var log logBuffer
	c := startShell(t, &log, "printf 'died mid-sentence'")
	waitFor(t, c, 10*time.Second)

	if !strings.Contains(log.String(), "orchestrator | died mid-sentence") {
		t.Errorf("the last partial line was dropped; log was:\n%s", log.String())
	}
}

// A crash is reported WITH ITS STATUS. "the orchestrator failed" sends the reader to the logs;
// "exited with status 3" is a fact they can act on.
func TestChildCrashCarriesItsExitStatus(t *testing.T) {
	var log logBuffer
	c := startShell(t, &log, "echo about to fail; exit 3")
	waitFor(t, c, 10*time.Second)

	err := c.Err()
	if err == nil {
		t.Fatal("a child that exited 3 reported no error")
	}
	if !strings.Contains(err.Error(), "status 3") {
		t.Errorf("the report does not name the exit status: %v", err)
	}
	if !strings.Contains(err.Error(), "orchestrator") {
		t.Errorf("the report does not name which child died: %v", err)
	}
}

// Signalled and exited are different investigations, and an unrequested SIGKILL is usually the
// OOM killer — which is a thing to check `dmesg` for, not a thing to debug in the orchestrator.
func TestChildKilledBySignalSaysWhichSignal(t *testing.T) {
	var log logBuffer
	c := startShell(t, &log, "kill -9 $$")
	waitFor(t, c, 10*time.Second)

	err := c.Err()
	if err == nil {
		t.Fatal("a child killed by SIGKILL reported no error")
	}
	if !strings.Contains(err.Error(), "signal 9") {
		t.Errorf("the report does not name the signal: %v", err)
	}
	if !strings.Contains(err.Error(), "OOM") {
		t.Errorf("an unrequested SIGKILL should point at the OOM killer: %v", err)
	}
}

// EXIT 0 IS NOT SUCCESS for a control plane nobody asked to stop. `kontra up` still holding its
// ports with no API behind them is the hollow failure this slice exists to not ship.
func TestChildCleanExitIsStillReported(t *testing.T) {
	var log logBuffer
	c := startShell(t, &log, "exit 0")
	waitFor(t, c, 10*time.Second)

	err := c.Err()
	if err == nil {
		t.Fatal("a control plane that exited on its own reported nothing")
	}
	if !strings.Contains(err.Error(), "exited cleanly") {
		t.Errorf("the report does not say what happened: %v", err)
	}
}

// The exit is readable by every caller, not delivered to whichever one happened to read first.
// `kontra up` selects on Done() and then calls Stop() in the same breath; a channel carrying the
// error would let the shutdown path swallow the crash it was supposed to print.
func TestChildExitIsReadableTwice(t *testing.T) {
	var log logBuffer
	c := startShell(t, &log, "exit 7")
	waitFor(t, c, 10*time.Second)

	first, second := c.Err(), c.Err()
	if first == nil || second == nil || first.Error() != second.Error() {
		t.Fatalf("the exit was not readable twice: %v then %v", first, second)
	}
	// And a Stop after the fact is a no-op rather than a signal to a reaped pid — which, by
	// then, may belong to something else entirely.
	if err := c.Stop(time.Second); err != nil {
		t.Errorf("Stop after exit: %v", err)
	}
}

// SHUTTING DOWN LEAVES NOTHING BEHIND. The orchestrator holds the DuckLake catalog lock, and A10
// made a second start refuse while it is held — so an orphan is not an untidy process, it is the
// next `kontra up` failing on a lock nobody holds.
func TestStopLeavesNoOrphanInTheProcessGroup(t *testing.T) {
	var log logBuffer
	// A grandchild the supervisor never learns about, exactly like a forked streamer or a tmux
	// client: only the GROUP reaches it.
	c := startShell(t, &log, "sleep 120 & echo grandchild=$!; sleep 120")

	var grandchild int
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, after, ok := strings.Cut(log.String(), "grandchild="); ok {
			line, _, _ := strings.Cut(after, "\n")
			grandchild, _ = strconv.Atoi(strings.TrimSpace(line))
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if grandchild == 0 {
		t.Fatalf("the child never reported its grandchild; log:\n%s", log.String())
	}

	if err := c.Stop(5 * time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if c.Err() != nil {
		t.Errorf("a stop we asked for must not be reported as a crash: %v", c.Err())
	}
	if groupAlive(c.PID()) {
		t.Error("the process group is still alive after Stop")
	}
	if alive(grandchild) {
		t.Errorf("the grandchild (pid %d) outlived the shutdown", grandchild)
	}
}

// A child that ignores SIGTERM is escalated, and the escalation is REPORTED. A supervisor that
// silently kills what it could not stop teaches nobody that their shutdown path is broken.
func TestStopEscalatesToKillAndSaysSo(t *testing.T) {
	var log logBuffer
	c := startShell(t, &log, "trap '' TERM; echo ignoring; while true; do sleep 0.2; done")

	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(log.String(), "ignoring") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}

	err := c.Stop(500 * time.Millisecond)
	if err == nil {
		t.Fatal("a child that ignored SIGTERM was killed silently")
	}
	if !strings.Contains(err.Error(), "SIGTERM") {
		t.Errorf("the report does not say what was ignored: %v", err)
	}
	if groupAlive(c.PID()) {
		t.Error("the process group survived the escalation")
	}
}

// The tag writer must not become unbounded memory when a child writes without newlines — a
// stack trace with an embedded payload is not a reason for the supervisor to grow.
func TestTagWriterFlushesPastItsCeiling(t *testing.T) {
	var log logBuffer
	w := newTagWriter(&log, "orchestrator")
	if _, err := w.Write(bytes.Repeat([]byte("x"), tagWriterCeiling+10)); err != nil {
		t.Fatal(err)
	}
	if got := len(log.String()); got == 0 {
		t.Fatal("nothing was emitted for a line past the ceiling")
	}
	if len(w.buf) != 0 {
		t.Errorf("the buffer kept %d bytes past the ceiling", len(w.buf))
	}
}

// A child needs somewhere to run. Node resolves `require` from its own file but reads relative
// paths from CWD, so an unset working directory is a data file landing wherever the operator
// happened to type the command.
func TestStartChildRefusesWithoutAWorkingDirectory(t *testing.T) {
	_, err := StartChild(ChildOptions{Name: "orchestrator", Path: "/bin/sh", Args: []string{"-c", "true"}})
	if err == nil {
		t.Fatal("a child with no working directory was started")
	}
	if !strings.Contains(err.Error(), "working directory") {
		t.Errorf("the refusal does not name what is missing: %v", err)
	}
}

// A program that is not there fails AT START, with the path in the message, rather than becoming
// a supervised child that immediately vanishes.
func TestStartChildNamesAProgramItCannotRun(t *testing.T) {
	missing := t.TempDir() + "/not-a-program"
	_, err := StartChild(ChildOptions{Name: "orchestrator", Path: missing, Dir: t.TempDir()})
	if err == nil {
		t.Fatal("a missing program was accepted")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("the failure does not name the program: %v", err)
	}
}

// alive asks the kernel about one process, the way catalogLock.ts does about its holder.
func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
