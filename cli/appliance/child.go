// child.go — supervising the one process the appliance does not contain (ADR 0031 §1, issue 14).
//
// Everything else in this package is a library the binary links: Temporal, the object store, the
// key-value store, the codec, the registry. The orchestrator is carried, not rewritten (ADR 0031
// §1, locked decision 1), so it is a Node process — and a Node process is a thing that has to be
// started, watched, read and stopped. That is what this file is.
//
// WHY A FILE RATHER THAN `exec.Command(...).Start()`. A fire-and-forget spawn gets three things
// wrong, and each of them has a name in this repo's own history:
//
//   - ITS OUTPUT DISAPPEARS. A child whose stdout goes nowhere is the tmux pane that held the only
//     copy of `dial tcp: lookup redis`: the process was up, every surface said healthy, and the
//     one fact that explained the failure existed in a place nobody was looking. The child's
//     stdout and stderr belong in the binary's log stream, line by line, tagged with whose they
//     are.
//
//   - ITS DEATH IS SILENT. `kontra up` that keeps running after its control plane exited is worse
//     than one that stops: the ports are still bound, the banner still says where everything is,
//     and the API answers nothing. A crash is reported WITH ITS EXIT STATUS and ends the command.
//
//   - IT OUTLIVES US. The orchestrator holds the DuckLake catalog lock (`data/catalogLock.ts`),
//     a Temporal connection and port 8088. An orphan keeps all three, and the catalog lock is the
//     nasty one: the next `kontra up` is refused by a lock whose holder is a process nobody
//     started and nobody can see. Every child is put in its own process GROUP and the group is
//     what gets signalled, so a grandchild cannot survive its parent either.
//
// THE SHUTDOWN LADDER IS TERM, THEN WAIT, THEN KILL, THEN CHECK. The orchestrator installs
// SIGTERM and SIGINT handlers precisely so it can release the catalog lock on the way out, so a
// SIGTERM it is given time to answer is the difference between a clean restart and a
// stale-lock recovery on every boot. The KILL is the backstop for a child that does not answer,
// and the CHECK after it is what makes "no orphaned process" a statement rather than a hope.
package appliance

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
)

// DefaultStopTimeout is how long a child gets to answer SIGTERM before it is killed.
//
// Ten seconds because the orchestrator's own exit path is short — release the lock, exit — but it
// runs after Fastify's close and after the Temporal worker's shutdown, and a worker mid-poll can
// take a couple of seconds to unwind. Long enough that a healthy stop is never escalated; short
// enough that Ctrl-C stays a thing a human does interactively.
const DefaultStopTimeout = 10 * time.Second

// ChildOptions describes the process to supervise.
type ChildOptions struct {
	// Name is the noun in every message about this child, and the tag on every line of its
	// output. "orchestrator", not a path: an operator reading a log wants to know which of our
	// things is talking.
	Name string

	// Path is the program, absolute. Args are the arguments AFTER it (argv[1:]).
	Path string
	Args []string

	// Dir is the child's working directory. Required: a Node process resolves `require` from
	// its own file's location but reads relative paths from CWD, so leaving it as whatever
	// directory the operator typed the command in is how a data file lands somewhere nobody
	// expects.
	Dir string

	// Env is the child's WHOLE environment. Not merged with the parent's here — see
	// orchestratorEnv in the cli package, which decides what is inherited and what is a fact
	// about this appliance and must override.
	Env []string

	// Log is where the child's stdout and stderr are written, tagged. nil discards, which is
	// only ever right in a test.
	Log io.Writer

	// StopTimeout overrides DefaultStopTimeout.
	StopTimeout time.Duration
}

// Child is a running supervised process.
type Child struct {
	name    string
	cmd     *exec.Cmd
	pid     int
	started time.Time

	// done is CLOSED when the child has exited and its output has been drained; exit holds the
	// classified reason, readable by any number of callers afterwards. A channel carrying the
	// error would deliver it to exactly one of them, and a supervisor whose shutdown path
	// happens to read first is one that swallows the crash it was supposed to report.
	done chan struct{}
	exit error

	mu       sync.Mutex
	stopping bool // set before we signal, so a requested stop is not reported as a crash
}

// StartChild starts the process and returns once it is running.
//
// THE THREAD IS LOCKED FOR THE CHILD'S WHOLE LIFE, and that is not incidental. On Linux the
// child asks the kernel to signal it when its parent THREAD dies (see child_linux.go), which is
// what stops a `kill -9` of this binary from leaving a Node process holding the catalog lock. A
// Go runtime thread that exited early would deliver that signal to a perfectly healthy child, so
// the goroutine that starts it locks its thread and holds it until Wait returns.
func StartChild(opts ChildOptions) (*Child, error) {
	if opts.Name == "" {
		opts.Name = "child"
	}
	if opts.Path == "" {
		return nil, errors.New("appliance: a supervised child needs a program to run")
	}
	if opts.Dir == "" {
		return nil, fmt.Errorf("appliance: %s needs a working directory", opts.Name)
	}

	cmd := exec.Command(opts.Path, opts.Args...)
	cmd.Dir = opts.Dir
	cmd.Env = opts.Env
	cmd.SysProcAttr = childProcAttr()

	// ONE TAGGED STREAM, TWO SOURCES. stdout and stderr are interleaved into the binary's log
	// exactly as they arrive, because the orchestrator writes its own structured lines to stdout
	// and Node writes stack traces to stderr, and a crash is the moment where seeing the last
	// ordinary line beside the trace is the whole point.
	tagged := newTagWriter(opts.Log, opts.Name)
	cmd.Stdout = tagged
	cmd.Stderr = tagged

	// WAITDELAY IS WHAT STOPS `Wait` HANGING ON A GRANDCHILD. With an io.Writer for Stdout, Wait
	// blocks until the pipe closes — and the pipe stays open as long as ANY process holds it,
	// including one the child forked and abandoned. Without this, a process we successfully
	// killed could still leave the supervisor waiting forever on a descendant that inherited its
	// output. Wait then returns ErrWaitDelay, which classify() reads as the exit it really was.
	cmd.WaitDelay = 5 * time.Second

	c := &Child{name: opts.Name, cmd: cmd, done: make(chan struct{})}

	launched := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		if err := cmd.Start(); err != nil {
			launched <- fmt.Errorf("start %s (%s): %w", opts.Name, opts.Path, err)
			return
		}
		c.pid = cmd.Process.Pid
		c.started = time.Now()
		launched <- nil

		err := cmd.Wait()
		_ = tagged.flush()
		c.mu.Lock()
		stopping := c.stopping
		c.mu.Unlock()
		c.exit = c.classify(err, stopping)
		close(c.done)
	}()

	if err := <-launched; err != nil {
		return nil, err
	}
	return c, nil
}

// PID is the child's process id, which is also its process GROUP id — see childProcAttr.
func (c *Child) PID() int { return c.pid }

// Done closes when the child has exited and everything it wrote has been logged. Err is the
// reason, and it is nil only for a stop we asked for.
//
// A CLEAN EXIT IS STILL AN ERROR, and that is deliberate. `kontra up` supervising a control
// plane that returned 0 has nothing left to do; a command that keeps running with its ports
// bound and its API gone is the hollow failure this slice exists to not ship.
func (c *Child) Done() <-chan struct{} { return c.done }

// Err is why the child ended, or nil while it is still running.
func (c *Child) Err() error {
	select {
	case <-c.done:
		return c.exit
	default:
		return nil
	}
}

// Stop shuts the child down and returns once nothing of it is left running.
//
// It is safe to call after the child has already exited — a crash and a Ctrl-C arriving at the
// same moment is an ordinary race, not an error — and it is safe to call twice.
func (c *Child) Stop(timeout time.Duration) error {
	if timeout <= 0 {
		timeout = DefaultStopTimeout
	}

	c.mu.Lock()
	if c.stopping {
		c.mu.Unlock()
		return nil
	}
	c.stopping = true
	c.mu.Unlock()

	// Already gone? Then there is nothing to signal, and signalling a pid that has been reaped is
	// how a supervisor kills a process id that now belongs to something else.
	select {
	case <-c.done:
		return nil
	default:
	}

	// THE GROUP, NOT THE PROCESS. `node` is one PID today, and the reason to signal the group
	// anyway is that it has not always been: the Dashboard streamer was a forked child until ADR
	// 0031 §4, and a `serve` started from the Workflows page spawns through tmux. A supervisor
	// that only knows about its direct child is one refactor away from leaving orphans again.
	if err := signalGroup(c.pid, syscall.SIGTERM); err != nil && !isGone(err) {
		return fmt.Errorf("signal %s (pid %d): %w", c.name, c.pid, err)
	}

	escalated := false
	select {
	case <-c.done:
		// It answered. Whether the GROUP is empty is still a separate question — see below.
	case <-time.After(timeout):
		escalated = true
		if err := signalGroup(c.pid, syscall.SIGKILL); err != nil && !isGone(err) {
			return fmt.Errorf("kill %s (pid %d): %w", c.name, c.pid, err)
		}
		select {
		case <-c.done:
		case <-time.After(5 * time.Second):
			return fmt.Errorf("%s (pid %d) did not exit within %s of SIGTERM and is still there after SIGKILL", c.name, c.pid, timeout)
		}
	}

	// THE CHECK THAT MAKES "NO ORPHANS" A STATEMENT. The child is reaped; anything it started is
	// in the same group, got the same signal, and is dying on its own schedule. So this WAITS
	// first and escalates second — measured, on this box: a shell's backgrounded `sleep` is still
	// in the group for a few milliseconds after the shell itself has been waited for, and
	// reporting that as an orphan would fire the honest message ("something outlived us") on
	// every ordinary shutdown until nobody read it any more.
	if !waitGroupEmpty(c.pid, timeout) {
		_ = signalGroup(c.pid, syscall.SIGKILL)
		if !waitGroupEmpty(c.pid, 2*time.Second) {
			return fmt.Errorf("%s exited but left processes in its group (pgid %d) that did not answer SIGKILL", c.name, c.pid)
		}
		return fmt.Errorf("%s exited leaving processes behind in its group (pgid %d); they ignored SIGTERM and were killed", c.name, c.pid)
	}
	if escalated {
		return fmt.Errorf("%s (pid %d) ignored SIGTERM for %s and was killed", c.name, c.pid, timeout)
	}
	return nil
}

// waitGroupEmpty polls until nothing is left in the process group, or the deadline passes.
//
// POLLING, BECAUSE THERE IS NOTHING TO WAIT ON. `wait` reaches our own children and a grandchild
// is not one; the kernel offers no "tell me when this group is empty". Signal 0 against the group
// is the question that can be asked, and 20 ms is fast enough that an ordinary shutdown does not
// feel it and slow enough that a long wait is not a spin.
func waitGroupEmpty(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if !groupAlive(pid) {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// classify turns Wait's error into a sentence about this child.
//
// THE STATUS IS THE MESSAGE. "the orchestrator failed" is a line that sends the reader to the
// logs to find out what actually happened; "exited with status 1" and "killed by SIGKILL" are
// two different investigations, and the second one usually ends at the OOM killer.
func (c *Child) classify(err error, stopping bool) error {
	ran := time.Since(c.started).Round(time.Millisecond)

	var ee *exec.ExitError
	switch {
	case err == nil:
		if stopping {
			return nil
		}
		// EXIT 0 IS NOT SUCCESS FOR A SUPERVISED SERVICE. Nothing asked it to stop, and what it
		// was doing — serving the API, polling three queues — is not work that finishes.
		return fmt.Errorf("the %s exited cleanly after %s without being asked to; "+
			"the appliance has no control plane left to supervise", c.name, ran)

	case errors.As(err, &ee):
		status, ok := ee.Sys().(syscall.WaitStatus)
		switch {
		case ok && status.Signaled():
			sig := status.Signal()
			if stopping {
				// We asked. 130/143 are what the orchestrator's own handlers exit with; a raw
				// signal here means it did not get to run them.
				return nil
			}
			hint := ""
			if sig == syscall.SIGKILL {
				hint = "; an unrequested SIGKILL is usually the kernel's OOM killer — check `dmesg -T | tail`"
			}
			// BOTH THE NUMBER AND THE NAME. Go's Signal.String() answers "killed" and
			// "terminated", which read as prose rather than as the signals they are: "the
			// orchestrator was killed by killed" is a sentence nobody can search for.
			return fmt.Errorf("the %s was killed by signal %d (%s) after %s%s", c.name, int(sig), sig, ran, hint)
		case stopping:
			// A stop we asked for: the orchestrator exits 130 on SIGINT and 143 on SIGTERM after
			// releasing the catalog lock, which is the GOOD path and must not read as a crash.
			return nil
		default:
			return fmt.Errorf("the %s exited with status %d after %s", c.name, ee.ExitCode(), ran)
		}

	case errors.Is(err, exec.ErrWaitDelay):
		// The process is gone; something that inherited its output was not. Not a crash, and
		// worth saying once rather than never.
		if stopping {
			return nil
		}
		return fmt.Errorf("the %s exited and left its output stream open (a process it started is still running)", c.name)

	default:
		return fmt.Errorf("the %s could not be waited for: %w", c.name, err)
	}
}

// --- the tagged log stream ------------------------------------------------------------------

// tagWriter turns a child's byte stream into whole lines with a prefix.
//
// LINE-ORIENTED, NOT CHUNK-ORIENTED. exec.Cmd hands over whatever came out of one read, which
// splits log lines at arbitrary offsets; prefixing those directly produces a log where a tag
// lands in the middle of a JSON object. Partial lines are buffered until their newline.
//
// AND THE BUFFER HAS A CEILING. A child that writes a megabyte with no newline — a stack trace
// with an embedded blob, a `console.log` of a payload — must not become unbounded memory in the
// supervisor. Past the ceiling the buffer is flushed as its own line, which is a cosmetic loss
// and never a lost byte.
type tagWriter struct {
	mu     sync.Mutex
	w      io.Writer
	prefix string
	buf    []byte
}

const tagWriterCeiling = 256 << 10

func newTagWriter(w io.Writer, name string) *tagWriter {
	return &tagWriter{w: w, prefix: name + " | "}
}

func (t *tagWriter) Write(p []byte) (int, error) {
	if t.w == nil {
		return len(p), nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	for {
		i := indexByte(t.buf, '\n')
		if i < 0 {
			break
		}
		line := t.buf[:i]
		t.buf = t.buf[i+1:]
		t.emit(strings.TrimSuffix(string(line), "\r"))
	}
	if len(t.buf) >= tagWriterCeiling {
		t.emit(string(t.buf))
		t.buf = t.buf[:0]
	}
	return len(p), nil
}

// flush emits whatever the child left without a trailing newline. Called once its output stream
// is closed — a process that dies mid-line still wrote that line, and it is usually the one that
// says why.
func (t *tagWriter) flush() error {
	if t.w == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.buf) > 0 {
		t.emit(string(t.buf))
		t.buf = t.buf[:0]
	}
	return nil
}

func (t *tagWriter) emit(line string) {
	_, _ = io.WriteString(t.w, t.prefix+line+"\n")
}

func indexByte(b []byte, c byte) int {
	return bytes.IndexByte(b, c)
}
