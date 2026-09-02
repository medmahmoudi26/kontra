package warden

// driver_process.go — `process`: the driver that needs no container runtime.
//
// It is not new behaviour. `kontra serve --actor <dir> --mode local` has always started an actor and
// a handler either as two children of this process or as two windows of one detached tmux session;
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
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/medmahmoudi26/kontra/cli/internal/cliutil"
	"github.com/medmahmoudi26/kontra/cli/internal/tmux"
)

// ProcessDriver runs a Worker's two halves directly on this machine.
// NewProcessDriver is how anything outside this package builds one — `kontra serve --mode local`
// is the only caller, and it wants the CLI's own streams so a foreground Worker interleaves into
// the terminal the operator typed in.
//
// A CONSTRUCTOR RATHER THAN EXPORTED FIELDS. `out`, `err` and `tmux` are how this driver HOLDS a
// Worker, which is nobody else's business; what a caller has an opinion about is where the output
// goes and whether the pair is detached. Exporting the fields would have published the mechanism.
func NewProcessDriver(out, err io.Writer, tmux bool) *ProcessDriver {
	return &ProcessDriver{out: out, err: err, tmux: tmux}
}

type ProcessDriver struct {
	// out and err are where a FOREGROUND half's stdout and stderr go. `kontra serve` passes the
	// CLI's own streams, which is what makes `--mode local` without `--tmux` an interleaved log in
	// the terminal you typed it in.
	out io.Writer
	err io.Writer

	// tmux holds the pair in a detached `<name>-<version>` session, one window per half, instead of
	// as children of this process. It is a property of how the driver HOLDS a Worker and not of the
	// Worker, which is why it is a field here and not on Spec — the same spec runs either way.
	tmux bool

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

	if d.tmux {
		session := tmux.Session(spec.Name, spec.Version)
		procs := make([]tmux.Proc, 0, len(workerParts))
		for _, part := range workerParts {
			p := halves[part]
			procs = append(procs, tmux.Proc{
				Window: string(part),
				Dir:    p.Dir,
				Env:    d.labelled(spec, part, p.Env),
				Argv:   p.Argv,
			})
		}
		if err := tmux.Start(session, tmux.KontraSessionTag(spec.Name, spec.Version), procs); err != nil {
			return workerHandle{}, err
		}
		h.Halves = tmuxPanePids(session)
		return h, nil
	}

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
	// THE TMUX SESSION IS WHAT OWNS A PANE'S PROCESS GROUP, so when the runtime holding this Worker
	// is one, killing the session is the correct stop and signalling the pids is not: each pane runs
	// `sh -c '…'` (tmux.Hold) and killing that shell orphans the command it is running.
	//
	// ONLY WHEN THE SESSION IS ACTUALLY HOLDING THIS HANDLE, which is a question for tmux and not for
	// the name. The session name is derived from the Worker's identity, so a FOREGROUND pair and a
	// `--tmux` one for the same actor@version cliutil.Derive the SAME name — and without this check, the
	// foreground pair crashing would kill somebody else's running session on the way out.
	if session := tmux.Session(h.Name, h.Version); tmux.Available() && tmuxHolds(session, h) {
		if out, err := exec.CommandContext(ctx, "tmux", "kill-session", "-t", session).CombinedOutput(); err != nil {
			return fmt.Errorf("tmux kill-session -t %s: %v: %s", session, err, strings.TrimSpace(string(out)))
		}
		// AND NOTHING AFTER IT. The session took the pane's whole process group with it, so the pids
		// on the handle are already gone — and signalling a pid that is gone is a signal to whatever
		// the kernel hands that number to next.
		return nil
	}

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
	finished := finishedPanes()

	type key struct{ name, version string }
	found := map[key]map[workerPart]int{}
	for _, p := range procs {
		// A process whose PARENT carries the same label is the same half seen twice — `go run .` and
		// the binary it execs, or a pane's `sh -c` and the command inside it.
		if parent, ok := label[p.ppid]; ok && parent == p.label {
			continue
		}
		// A HALF HELD IN TMUX OUTLIVES ITS OWN DEATH, and this is the one place the process table
		// alone gives the wrong answer. `tmux.Hold` wraps every pane's command so the window stays
		// open with the exit status on screen — a crash-on-boot is unreadable otherwise — so the
		// holding shell is still alive, still carries the label, and would be reported as a running
		// Worker forever. `@kontra_exit` is the pane option that same wrapper writes when the command
		// returns, and it is what makes "list forgets one that died" true in a session too.
		if finished[p.pid] {
			continue
		}
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

// logs is the tmux scrollback for both halves, or a refusal.
//
// THE PANE OUTLIVES THE PROCESS ON PURPOSE (tmux.Hold: "a window that disappears is the worst
// possible report of a crash-on-boot"), so this asks the SESSION for every window rather than asking
// the handle for its live halves — the half that is missing from the handle is usually the half
// whose output you came for.
//
// A FOREGROUND WORKER HAS NO LOG AND THIS SAYS SO. Its output went to the writers `start` was given
// and the kernel retained nothing; returning an empty stream would claim the Worker printed nothing.
// That asymmetry is a property of the runtime, not a gap in the driver: `podman logs` has a real
// answer here and this does not.
func (d *ProcessDriver) logs(ctx context.Context, h workerHandle) (io.ReadCloser, error) {
	session := tmux.Session(h.Name, h.Version)
	if !tmux.Available() || !tmuxHolds(session, h) {
		return nil, fmt.Errorf("%s: %w — a foreground `process` Worker writes to the terminal that started it "+
			"and nothing keeps a copy; `kontra serve --actor … --tmux` puts it in a session that does", h.id(), errNoRetainedLogs)
	}
	var buf bytes.Buffer
	for _, part := range workerParts {
		// `-S -` is the whole scrollback rather than the visible pane: the interesting line is
		// usually the first one, and a pane is 40 rows deep (tmux.go).
		out, err := exec.CommandContext(ctx, "tmux", "capture-pane", "-p", "-S", "-", "-t", session+":"+string(part)).Output()
		if err != nil {
			continue
		}
		fmt.Fprintf(&buf, "== %s ==\n", part)
		buf.Write(out)
	}
	return io.NopCloser(&buf), nil
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

// finishedPanes is every pane pid whose wrapped command has already exited, read from the
// `@kontra_exit` option `tmux.Hold` writes. Empty when there is no tmux server, no tmux at all, or no
// pane has finished — all three mean the same thing to the caller.
//
// The KEY is the pane's pid, which is the wrapping shell, and that is exactly the process the label
// scan reports as the half's root — so the two joins line up without either side knowing about the
// other's identity scheme.
func finishedPanes() map[int]bool {
	if !tmux.Available() {
		return nil
	}
	out, err := exec.Command("tmux", "list-panes", "-a", "-F", "#{pane_pid} #{"+tmux.KontraExitOption+"}").Output()
	if err != nil {
		return nil
	}
	done := map[int]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		if pid, err := strconv.Atoi(f[0]); err == nil {
			done[pid] = true
		}
	}
	return done
}

// tmuxHolds reports whether the named session is what is holding this handle's halves: every ref it
// carries is a pane pid of that session. See `stop` for why the name alone is not enough.
func tmuxHolds(session string, h workerHandle) bool {
	if len(h.Halves) == 0 || !tmux.HasSession(session) {
		return false
	}
	panes := map[string]bool{}
	for _, w := range tmuxPanePids(session) {
		panes[w.Ref] = true
	}
	for _, w := range h.Halves {
		if !panes[w.Ref] {
			return false
		}
	}
	return true
}

// tmuxPanePids reads back the pid of each pane tmux just created. A READ FROM THE RUNTIME rather
// than a value tmux.Start could have returned: the pane's process is tmux's child, not this one's, so
// tmux is the only thing that knows it.
//
// Best-effort. A handle with no refs is still listable and its halves are still signallable, so a
// tmux too old for this format is not worth failing a start over.
func tmuxPanePids(session string) []workerHalf {
	out, err := exec.Command("tmux", "list-panes", "-t", session, "-s",
		"-F", "#{window_name} #{pane_pid}").Output()
	if err != nil {
		return nil
	}
	byPart := map[workerPart]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 {
			byPart[workerPart(f[0])] = f[1]
		}
	}
	var halves []workerHalf
	for _, part := range workerParts {
		if ref, ok := byPart[part]; ok {
			halves = append(halves, workerHalf{Part: part, Ref: ref})
		}
	}
	return halves
}
