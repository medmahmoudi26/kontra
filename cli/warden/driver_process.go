package warden

// driver_process.go — `process`: the driver that needs no container runtime.
//
// It is not new behaviour. `kontra serve --actor <dir> --mode local` has always started an actor and
// a handler as two children of this process;
// this file is that code with an interface around it, so slice 02 can put `podman` beside it without
// either driver knowing the other exists. ADR 0036: "`process` — code executed directly, no runtime
// required — is how the single box and the airgapped install still work."
//
// ═══ WHAT "THE RUNTIME" IS FOR A BARE PROCESS ═══
//
// podman has a daemon that remembers containers and labels. A process has neither, so `list` reads
// the only registry the operating system keeps: its own process table, and it matches on
// KONTRA_WORKER in the process's ENVIRONMENT (see driver.go for why the environment is the label).
//
// On Linux that is `/proc/<pid>/environ`, which is the kernel's copy of the environment as of
// execve. Three consequences, all of them the point:
//
//   - A Worker started by SOMEBODY ELSE is found. An operator's own `kontra serve`, or the Warden
//     that was running before this one restarted, is a process with the label, and `list` cannot
//     tell the difference — which is exactly the property that stops reconcile starting a rival
//     Worker on a queue that already has one.
//   - A Worker that DIED is gone, with nothing to invalidate. A zombie's environ region is already
//     torn down, so `/proc/<pid>/environ` reads empty and an unreaped child is not mistaken for a
//     running Worker either.
//   - DESCENDANTS INHERIT THE LABEL, so the scan finds more processes than there are halves. The
//     handler is `go run .`, which compiles and then EXECS a second binary that carries the same
//     environment — two labelled processes, one half. `list` keeps only the ROOTS: a process whose
//     parent carries the same label is the same half seen twice. When `go run` exits and hands its
//     child to init, that child becomes the root, which is right — it is what is serving the queue.
//
// `list` is therefore Linux-only, and driver_proctable_other.go says so by name rather than
// returning an empty slice. A **Machine** is Linux and so is every container ADR 0036 puts a Worker
// in; `kontra serve` on a developer's mac calls `start` and `stop`, which are portable. An empty
// list on a machine full of Workers is the failure this seam exists to prevent, so it is not
// something to fall back to.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/medmahmoudi26/kontra/cli/internal/cliutil"
)

// ProcessDriver runs a Worker's two halves directly on this machine.
// NewProcessDriver is how anything outside this package builds one — `kontra serve --mode local`
// is the only caller, and it wants the CLI's own streams so a foreground Worker interleaves into
// the terminal the operator typed in.
//
// A CONSTRUCTOR RATHER THAN EXPORTED FIELDS. `out` and `err` are how this driver HOLDS a Worker,
// which is nobody else's business; what a caller has an opinion about is where the output goes.
// Exporting the fields would have published the mechanism.
//
// IT TOOK A THIRD ARGUMENT AND IT WAS `tmux`. The driver could hold the pair in a detached session
// instead of as children of this process, which is how `kontra serve --tmux` and the Serve button
// both worked. That is `--mode dev` now — a container, which answers the same four questions
// (is it alive, what is running, what did it print, how do I stop it) without a terminal
// multiplexer in the middle, and which ADR 0036 asks for anyway. This driver is the FOREGROUND one
// again, exactly as its name says.
func NewProcessDriver(out, err io.Writer) *ProcessDriver {
	return &ProcessDriver{out: out, err: err}
}

type ProcessDriver struct {
	// out and err are where a half's stdout and stderr go. `kontra serve` passes the CLI's own
	// streams, which is what makes `--mode local` an interleaved log in the terminal you typed it in.
	out io.Writer
	err io.Writer

	// kids is the driver's memory of the foreground pairs IT started, and it is a CACHE with exactly
	// two uses: reaping (a child of this process becomes a zombie unless someone Waits for it) and
	// reporting an exit STATUS, which the process table cannot give. `list` never reads it. See
	// driver.go's header for why that separation is the whole point of this file.
	mu   sync.Mutex
	kids map[string]*foregroundPair
}

// foregroundPair is one started pair's Wait bookkeeping. exit carries the first message about a half
// that ended; wg is done when both halves have been reaped.
type foregroundPair struct {
	exit chan error
	wg   sync.WaitGroup
}

// The seam is real or it is ceremony, and this is what makes the compiler say which.
var _ workerDriver = (*ProcessDriver)(nil)

func (d *ProcessDriver) driverName() string { return "process" }

// errNoRetainedLogs is what `logs` answers when the runtime kept nothing. It is a sentinel because
// "there is no log" and "the log is empty" are different answers and a caller has to be able to tell
// them apart — an empty stream would say the Worker printed nothing, which is a claim this driver
// cannot make.
var errNoRetainedLogs = errors.New("no retained logs")

// start runs both halves and returns the Worker as the runtime now holds it.
//
// THE ctx OWNS THE FOREGROUND CHILDREN. `exec.CommandContext` kills them when it is cancelled, which
// is `kontra serve`'s Ctrl-C today: the signal handler cancels, both halves are killed, and the
// command returns. That is preserved deliberately — a driver that ran its children under a
// background context would leave a worker behind every time the CLI exited.
func (d *ProcessDriver) Start(ctx context.Context, spec Spec) (workerHandle, error) {
	h := workerHandle{Driver: d.driverName(), Name: spec.Name, Version: spec.Version}
	halves := map[workerPart]ProcSpec{partActor: spec.Actor, partHandler: spec.Handler}

	pair := &foregroundPair{exit: make(chan error, len(workerParts))}
	started := map[workerPart]*exec.Cmd{}
	for _, part := range workerParts {
		p := halves[part]
		cmd := exec.CommandContext(ctx, p.Argv[0], p.Argv[1:]...)
		cmd.Dir = p.Dir
		cmd.Stdout, cmd.Stderr = d.out, d.err
		cmd.Env = d.labelled(spec, part, p.Env)
		if err := cmd.Start(); err != nil {
			// The messages are serve.go's, verbatim: "starting the actor" and "go run ./handler"
			// are what an operator has seen from this command for as long as it has existed, and a
			// refactor is not a reason to retire an error string.
			for _, c := range started {
				_ = c.Process.Kill()
			}
			if part == partActor {
				return workerHandle{}, fmt.Errorf("starting the actor: %w", err)
			}
			return workerHandle{}, fmt.Errorf("go run ./handler: %w", err)
		}
		started[part] = cmd
		h.Halves = append(h.Halves, workerHalf{Part: part, Ref: strconv.Itoa(cmd.Process.Pid)})

		// One Wait per child, started here and nowhere else: two goroutines calling Wait on one
		// exec.Cmd is a data race, and this is the only place that can promise there is exactly one.
		pair.wg.Add(1)
		go func(part workerPart, cmd *exec.Cmd) {
			defer pair.wg.Done()
			// `%w` of a nil error is how serve.go has always written this — a half that exits ZERO
			// still ends the command, because a Worker that stopped serving is not a success even
			// when its exit code says so.
			pair.exit <- fmt.Errorf("%s exited: %w", part, cmd.Wait())
		}(part, cmd)
	}

	d.mu.Lock()
	if d.kids == nil {
		d.kids = map[string]*foregroundPair{}
	}
	d.kids[h.id()] = pair
	d.mu.Unlock()
	return h, nil
}

// labelled is the half's environment plus the one variable that makes it findable — see driver.go.
// cliutil.Derive(), not append(), for the reason serve_test.go pins: two appends onto one slice with spare
// capacity write the same index, and the environment being derived here is already the product of
// one such derivation.
func (d *ProcessDriver) labelled(spec Spec, part workerPart, env []string) []string {
	return cliutil.Derive(env, workerLabelEnv(spec.Name, spec.Version, part))
}

// exited is the channel a foreground half's end arrives on. It exists on the CONCRETE driver and not
// on workerDriver because nothing above the seam needs it: a Warden learns that a Worker died by
// listing, which is the read that is true whoever started it (driver.go). `kontra serve --mode local`
// is the one caller that is also the PARENT, so it is the one caller that can be told directly and
// can report the exit status.
//
// nil for a Worker this driver did not start, which is the honest answer: this process cannot be
// told about the exit of a process it is not the parent of.
func (d *ProcessDriver) Exited(h workerHandle) <-chan error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if pair, ok := d.kids[h.id()]; ok {
		return pair.exit
	}
	return nil
}

// stop ends both halves: drain to leave on their own, then SIGKILL.
//
// A drain of 0 means kill now, which is what a caller that has already watched a half exit passes —
// there is nothing left to be graceful with.
func (d *ProcessDriver) Stop(ctx context.Context, h workerHandle, drain time.Duration) error {
	pids := handlePids(h)
	if drain > 0 {
		for _, pid := range pids {
			_ = syscall.Kill(pid, syscall.SIGTERM)
		}
		d.waitGone(h, pids, drain)
	}
	for _, pid := range pids {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	return nil
}

// waitGone blocks until both halves are gone or drain elapses.
//
// TWO WAYS TO ASK, because there are two kinds of half and only one of them is ours. A child of this
// process is reaped by the goroutine `start` left behind, so its WaitGroup is exact and a poll would
// be wrong — an unreaped child answers signal 0 as though it were alive, forever. A half found by
// `list` belongs to someone else's process table entry, so signal 0 is the only probe available and
// is accurate for it.
func (d *ProcessDriver) waitGone(h workerHandle, pids []int, drain time.Duration) {
	d.mu.Lock()
	pair := d.kids[h.id()]
	d.mu.Unlock()

	if pair != nil {
		reaped := make(chan struct{})
		go func() { pair.wg.Wait(); close(reaped) }()
		select {
		case <-reaped:
		case <-time.After(drain):
		}
		return
	}
	deadline := time.Now().Add(drain)
	for time.Now().Before(deadline) {
		alive := false
		for _, pid := range pids {
			if syscall.Kill(pid, 0) == nil {
				alive = true
			}
		}
		if !alive {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// list is every Worker the process table is holding — see this file's header for what that means and
// why the driver's own memory is not consulted.
func (d *ProcessDriver) list(ctx context.Context) ([]workerHandle, error) {
	procs, err := labelledProcs()
	if err != nil {
		return nil, err
	}
	label := make(map[int]string, len(procs))
	for _, p := range procs {
		label[p.pid] = p.label
	}

	type key struct{ name, version string }
	found := map[key]map[workerPart]int{}
	for _, p := range procs {
		// A process whose PARENT carries the same label is the same half seen twice — `go run .` and
		// the binary it execs, or a pane's `sh -c` and the command inside it.
		if parent, ok := label[p.ppid]; ok && parent == p.label {
			continue
		}
		// THERE WAS A THIRD FILTER HERE AND IT WAS TMUX'S. A pane wrapped its command in a shell
		// that stayed alive after the command returned — so a crash-on-boot was readable — which
		// meant the wrapper was still carrying the label and would be reported as a running Worker
		// forever. `@kontra_exit`, written by that same wrapper, was how `list` learned to forget it.
		// Nothing wraps anything now: a half is the process, and a process that exits leaves the
		// table.
		name, version, part, ok := parseWorkerLabel(p.label)
		if !ok {
			continue
		}
		k := key{name, version}
		if found[k] == nil {
			found[k] = map[workerPart]int{}
		}
		// Two roots for one half means two rival processes on one queue. The LOWER pid wins so the
		// answer is stable across calls; the rivalry itself is not this seam's to resolve, and a
		// caller that cares can see it in the process table the same way this did.
		if cur, seen := found[k][part]; !seen || p.pid < cur {
			found[k][part] = p.pid
		}
	}

	out := make([]workerHandle, 0, len(found))
	for k, halves := range found {
		h := workerHandle{Driver: d.driverName(), Name: k.name, Version: k.version}
		for _, part := range workerParts {
			if pid, ok := halves[part]; ok {
				h.Halves = append(h.Halves, workerHalf{Part: part, Ref: strconv.Itoa(pid)})
			}
		}
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id() < out[j].id() })
	return out, nil
}

// logs is a REFUSAL, always, and that is the honest answer for this runtime.
//
// A `process` Worker's output went to the writers `start` was given — the terminal the operator
// typed in — and the kernel retained nothing. Returning an empty stream would claim the Worker
// printed nothing, which is a different fact and the one thing driver.go says a driver may not do:
// "an empty log and a log that was never kept are different answers".
//
// THERE USED TO BE AN ANSWER HERE AND IT WAS TMUX'S. A `--tmux` pair kept its scrollback in a
// session that outlived the process on purpose, and this read it back with `capture-pane`. That is
// `--mode dev` now: a container keeps its stdout, `docker logs` reads it, and logship ships it to
// VictoriaLogs by the `KONTRA_WORKER` label — searchable after the thing that wrote it is gone,
// which a pane never was.
func (d *ProcessDriver) logs(_ context.Context, h workerHandle) (io.ReadCloser, error) {
	return nil, fmt.Errorf("%s: %w — a foreground `process` Worker writes to the terminal that started "+
		"it and nothing keeps a copy. `--mode dev` runs the same pair in containers, whose output is "+
		"`kontra logs` and the Logs surface", h.id(), errNoRetainedLogs)
}

// handlePids is the halves of a handle as pids. A ref that is not a pid is skipped rather than
// guessed at: `workerHalf.Ref` is driver-scoped and a handle from another driver would carry a
// container id there.
func handlePids(h workerHandle) []int {
	out := make([]int, 0, len(h.Halves))
	for _, w := range h.Halves {
		if pid, err := strconv.Atoi(w.Ref); err == nil && pid > 0 {
			out = append(out, pid)
		}
	}
	return out
}
