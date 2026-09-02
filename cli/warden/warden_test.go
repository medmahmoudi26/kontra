// warden_test.go — the reconcile loop, and the four properties that are not this slice's to revise.
//
// EVERY TEST HERE DRIVES A REAL DRIVER AGAINST REAL PROCESSES. There is no fake `workerDriver`,
// because the properties being asserted are all properties of what the loop does with what the
// RUNTIME says — and a fake would answer from a map, which is precisely the implementation
// driver.go's header says is the bug. The one thing wrapped is the driver's `list`, narrowed to the
// test's own Worker names, and it is wrapped for a reason worth stating: reconcile STOPS everything
// it is not asked for, so an unscoped test would reach across the process table and kill the Workers
// of every other test file on this box. `scoped` filters the answer; it does not invent one, and
// TestScopedListStillReadsTheRealProcessTable proves the difference.
//
// ═══ THE CONTROLS COME FIRST ═══
//
// driver_podman_test.go's socket test is the model: "the socket answers a dial from the HOST → the
// target is real" before "the socket refuses a dial from inside → the claim". The same discipline
// applies to every negative assertion here — that the Warden did not orphan a half, did not open a
// listener, did not die with a Worker — because all three pass identically against a test that never
// created the hazard.
package warden

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/medmahmoudi26/kontra/cli/internal/trustpolicy"
)

// --- fixtures --------------------------------------------------------------------------------------

// scoped narrows `list` to one test's Workers. See this file's header for why it exists and what it
// deliberately does not do.
type scoped struct {
	workerDriver
	prefix string

	// starts counts calls, per Worker, and DELEGATES every one of them. It exists because the loop's
	// own restart counter is not a record of how many times a Worker was started: `settled` forgives
	// it once the Worker has been whole for long enough, which is correct behaviour and makes it the
	// wrong thing to assert against. Found by driving the clock forward — the counter read 0 while
	// the log showed three starts.
	mu     *sync.Mutex
	starts map[string]int
}

func (s scoped) Start(ctx context.Context, spec Spec) (workerHandle, error) {
	s.mu.Lock()
	s.starts[spec.Name+"@"+spec.Version]++
	s.mu.Unlock()
	return s.workerDriver.Start(ctx, spec)
}

// startsOf is how many times the Warden asked this driver to start one Worker.
func (w *warden) startsOf(id string) int {
	s := w.driver.(scoped)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.starts[id]
}

func (s scoped) list(ctx context.Context) ([]workerHandle, error) {
	all, err := s.workerDriver.list(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]workerHandle, 0, len(all))
	for _, h := range all {
		if strings.HasPrefix(h.Name, s.prefix) {
			out = append(out, h)
		}
	}
	return out, nil
}

// testWarden builds a Warden over the process driver, scoped to `prefix`, with a clock the test owns.
//
// `id` is nil and `http` is nil on purpose: every test in this file calls `reconcile` with a desired
// state directly, so nothing here dials a Controller. The two tests that DO need one build it
// themselves against a real CA (warden_identity_test.go's server).
func testWarden(t *testing.T, prefix string) *warden {
	t.Helper()
	requireProcTable(t)
	drv := &ProcessDriver{out: io.Discard, err: io.Discard}
	w := &warden{
		driver:   scoped{workerDriver: drv, prefix: prefix, mu: &sync.Mutex{}, starts: map[string]int{}},
		interval: 10 * time.Millisecond,
		out:      &testLog{t: t},
		now:      time.Now,
	}
	t.Cleanup(func() {
		hs, err := w.driver.list(context.Background())
		if err != nil {
			return
		}
		for _, h := range hs {
			_ = w.driver.Stop(context.Background(), h, 0)
		}
	})
	return w
}

// testLog puts the loop's own output in the test log, so a failure carries the decisions that led to
// it rather than only the assertion that caught it.
type testLog struct{ t *testing.T }

func (l *testLog) Write(p []byte) (int, error) {
	l.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// sleeper is a half that stays up. `sleep` rather than a compiled fixture because the process
// driver's unit is a process and this file is about the loop above it.
func sleeper(t *testing.T) ProcSpec {
	t.Helper()
	return ProcSpec{Dir: t.TempDir(), Argv: []string{"sleep", "600"}, Env: os.Environ()}
}

func spec(t *testing.T, name, version string) Spec {
	t.Helper()
	return Spec{Name: name, Version: version, Actor: sleeper(t), Handler: sleeper(t)}
}

// eventually polls until `want` holds, then returns what the driver last reported. Polling because a
// process entering and leaving the process table is not synchronous with the call that caused it; the
// assertions are on WHAT is found, never on how long it took.
func eventually(t *testing.T, w *warden, what string, want func([]workerHandle) bool) []workerHandle {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var last []workerHandle
	for time.Now().Before(deadline) {
		hs, err := w.driver.list(context.Background())
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		last = hs
		if want(hs) {
			return hs
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("never became true: %s\n  the driver last reported: %v", what, last)
	return nil
}

func ids(hs []workerHandle) []string {
	out := make([]string, 0, len(hs))
	for _, h := range hs {
		out = append(out, h.id())
	}
	sort.Strings(out)
	return out
}

func handleFor(hs []workerHandle, name string) (workerHandle, bool) {
	for _, h := range hs {
		if h.Name == name {
			return h, true
		}
	}
	return workerHandle{}, false
}

// turn runs one reconcile and waits for the runtime to catch up with it.
func turn(t *testing.T, w *warden, desired []Spec, what string, want func([]workerHandle) bool) []workerHandle {
	t.Helper()
	w.reconcile(context.Background(), desired)
	return eventually(t, w, what, want)
}

// --- the scoping is real ------------------------------------------------------------------------

// THE WRAPPER MUST NOT BE A FAKE, and this is what says so. `scoped` narrows the real driver's answer
// by name; if it invented one, every test in this file would be asserting against a map — the exact
// implementation driver.go's header calls "the single most common bug in this class of software".
// So: a process this driver never started, wearing the label, must reach the Warden through it.
func TestScopedListStillReadsTheRealProcessTable(t *testing.T) {
	w := testWarden(t, "wscope")
	ghost := startLabelled(t, "wscope-outside", "9.9.9", partActor, "sleep", "600")

	hs := eventually(t, w, "a Worker the driver never started", func(hs []workerHandle) bool {
		_, ok := handleFor(hs, "wscope-outside")
		return ok
	})
	h, _ := handleFor(hs, "wscope-outside")
	half, _ := h.half(partActor)
	if half.Ref != strconv.Itoa(ghost.Process.Pid) {
		t.Errorf("the handle names pid %s; the process nothing here started is %d", half.Ref, ghost.Process.Pid)
	}
	// …and the narrowing is real too, or the stop-everything-not-wanted tests below would be
	// operating on the whole box.
	other := startLabelled(t, "notwscope", "9.9.9", partActor, "sleep", "600")
	_ = other
	hs, err := w.driver.list(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if _, ok := handleFor(hs, "notwscope"); ok {
		t.Errorf("the scope let a Worker outside it through: %v", ids(hs))
	}
}

// --- adoption -------------------------------------------------------------------------------------

// A WORKER ALREADY RUNNING IS ADOPTED, NOT RESTARTED. This is the property that makes a rebooted or
// re-executed Warden safe: it has no memory of what it started, so the only thing that can tell it a
// Worker is up is `list()`, and a loop that got this wrong would start a SECOND poller on a queue
// that already has one — which identity.go describes as a worker that "can take its continue_as_new
// and run different code".
func TestWardenAdoptsAWorkerItDidNotStart(t *testing.T) {
	w := testWarden(t, "wadopt")
	ghost := startLabelled(t, "wadopt-a", "1.0.0", partActor, "sleep", "600")
	ghost2 := startLabelled(t, "wadopt-a", "1.0.0", partHandler, "sleep", "600")

	// CONTROL: the pair is whole BEFORE reconcile runs, so "it was not restarted" is a claim about
	// the loop and not about a fixture that never came up.
	before := eventually(t, w, "the out-of-band pair, whole", func(hs []workerHandle) bool {
		h, ok := handleFor(hs, "wadopt-a")
		return ok && h.whole()
	})
	if len(before) != 1 {
		t.Fatalf("the fixture left %d Workers, want 1: %v", len(before), ids(before))
	}

	after := turn(t, w, []Spec{spec(t, "wadopt-a", "1.0.0"), spec(t, "wadopt-b", "2.0.0")},
		"the adopted Worker and the started one", func(hs []workerHandle) bool {
			a, aok := handleFor(hs, "wadopt-a")
			b, bok := handleFor(hs, "wadopt-b")
			return aok && a.whole() && bok && b.whole()
		})

	a, _ := handleFor(after, "wadopt-a")
	actor, _ := a.half(partActor)
	handler, _ := a.half(partHandler)
	if actor.Ref != strconv.Itoa(ghost.Process.Pid) || handler.Ref != strconv.Itoa(ghost2.Process.Pid) {
		t.Errorf("the adopted Worker was restarted: it is now %s/%s, it was %d/%d",
			actor.Ref, handler.Ref, ghost.Process.Pid, ghost2.Process.Pid)
	}
	if len(after) != 2 {
		t.Errorf("reconcile left %d Workers, want exactly the 2 asked for: %v", len(after), ids(after))
	}

	// AND NO RIVAL WAS STARTED BESIDE IT — which the handle above CANNOT say, and this was found by
	// mutating the loop rather than by reading it. `driver_process.go:list` keeps the LOWER pid when
	// two roots claim one half ("Two roots for one half means two rival processes on one queue. The
	// LOWER pid wins so the answer is stable across calls"), so a Warden that started a second pair on
	// top of the adopted one returns a handle naming the ORIGINAL pids and every assertion above still
	// passes. The duplicate is only visible where the seam declined to resolve it: the process table.
	//
	// That is also the failure this whole property exists to prevent — two pollers on
	// `wadopt-a-1.0.0`, one of which "can take its continue_as_new and run different code"
	// (identity.go).
	for _, part := range workerParts {
		if n := labelledCount(t, workerLabel("wadopt-a", "1.0.0", part)); n != 1 {
			t.Errorf("%d processes wear the %s half's label; the Worker was adopted, so there must be "+
				"exactly the 1 the fixture started", n, part)
		}
	}
}

// labelledCount is how many processes wear one exact KONTRA_WORKER label. It reads the same process
// table `list` does and does NOT dedupe, which is the whole reason it exists — see the caller.
func labelledCount(t *testing.T, label string) int {
	t.Helper()
	procs, err := labelledProcs()
	if err != nil {
		t.Fatalf("labelledProcs: %v", err)
	}
	n := 0
	for _, p := range procs {
		if p.label == label {
			n++
		}
	}
	return n
}

// --- the orphan this loop must not leave behind ----------------------------------------------------

// THE `ppid=1` HANDLER, COLLECTED. Slice 01 found sixteen of these on this box — handlers with no
// actor half, up to six days old, still polling. They arrive when a parent is killed and the process
// it started is reparented to init: `go run .` runs a compiled binary, `exec.CommandContext` SIGKILLs
// only `go run`, and the binary carries on.
//
// The Warden cannot prevent every way one is made — an operator's Ctrl-C makes them too — so what it
// owes is that it COLLECTS one. That falls out of reading the runtime: an orphan is a labelled
// process, `list()` reports it, and a Worker that is not in the assignment is stopped.
//
// THE CONTROLS ARE THE INTERESTING PART. This test passes vacuously if the fixture's shell simply
// exited without leaving anything, so both facts are asserted first: the orphan exists, and it has
// been reparented away from the shell that made it.
func TestWardenCollectsAnOrphanedHalfWithNoActor(t *testing.T) {
	w := testWarden(t, "worphan")

	// `sleep 600 &` and NOT `& wait`: the shell forks and exits immediately, so the sleep is
	// reparented and keeps the label it inherited. That is the shape of the sixteen.
	shell := exec.Command("sh", "-c", "sleep 600 &")
	shell.Env = append(os.Environ(), workerLabelEnv("worphan-h", "0.1.0", partHandler))
	if err := shell.Run(); err != nil {
		t.Fatalf("the orphan fixture did not run: %v", err)
	}

	hs := eventually(t, w, "the orphaned handler", func(hs []workerHandle) bool {
		_, ok := handleFor(hs, "worphan-h")
		return ok
	})
	h, _ := handleFor(hs, "worphan-h")
	orphan, _ := h.half(partHandler)
	pid, err := strconv.Atoi(orphan.Ref)
	if err != nil {
		t.Fatalf("the orphan's ref is not a pid: %q", orphan.Ref)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })

	// CONTROL 1: it is a real, running process.
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("the orphan (pid %d) is not running, so nothing here is being collected: %v", pid, err)
	}
	// CONTROL 2: it was reparented. If the shell were still its parent this would be an ordinary
	// child and the ppid=1 shape would be untested.
	if ppid := ppidOf(t, pid); ppid == shell.Process.Pid {
		t.Fatalf("the orphan's parent is still the shell (%d), so it was never orphaned", ppid)
	}
	// CONTROL 3: it is HALF a Worker — a handler with no actor, which is the state that is worse
	// than dead (serve.go) and the one the sixteen were in.
	if h.whole() {
		t.Fatalf("the fixture left a whole pair, so this is not the orphan shape: %v", h.Halves)
	}

	// THE CLAIM. It is not in the assignment, so it goes.
	turn(t, w, nil, "the orphan collected", func(hs []workerHandle) bool {
		_, ok := handleFor(hs, "worphan-h")
		return !ok
	})
	if syscall.Kill(pid, 0) == nil {
		t.Errorf("pid %d survived the reconcile that stopped it — the Warden stopped a handle it "+
			"remembered, not the one `list` reported", pid)
	}
}

// ppidOf reads a process's parent from /proc. Field 4 of `stat`, and it is read from the END of the
// line rather than by splitting: field 2 is the executable name in parentheses and MAY CONTAIN
// SPACES, which is how naive parsers of this file get the wrong number.
func ppidOf(t *testing.T, pid int) int {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		t.Fatalf("reading /proc/%d/stat: %v", pid, err)
	}
	close := strings.LastIndex(string(body), ")")
	if close < 0 {
		t.Fatalf("/proc/%d/stat is not in the shape this expects: %q", pid, body)
	}
	fields := strings.Fields(string(body)[close+1:])
	if len(fields) < 2 {
		t.Fatalf("/proc/%d/stat has no ppid: %q", pid, body)
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		t.Fatalf("/proc/%d/stat ppid is not a number: %q", pid, fields[1])
	}
	return ppid
}

// AND A START MUST NOT BE CANCELLABLE BY THE LOOP SHUTTING DOWN, which is the other half of the same
// bug. `exec.CommandContext` kills what it started when its context is cancelled, so a `start` running
// under the loop's context turns `systemctl stop kontra-warden` into a SIGKILL delivered to `go run`
// mid-compile — leaving exactly the orphan above.
//
// THE CONTROL IS THE WHOLE TEST. "The Worker survived a cancel" passes identically against a driver
// that ignores contexts entirely, so the same driver is first shown to kill a pair when its context
// IS the one that is cancelled.
func TestWardenStartDoesNotDieWithTheLoopsContext(t *testing.T) {
	w := testWarden(t, "wdetach")
	drv := w.driver.(scoped).workerDriver.(*ProcessDriver)

	// CONTROL: this driver really does own what it starts through the context it is given.
	cctx, ccancel := context.WithCancel(context.Background())
	control := spec(t, "wdetach-control", "0.0.1")
	if _, err := drv.Start(cctx, control); err != nil {
		t.Fatalf("start: %v", err)
	}
	eventually(t, w, "the control pair", func(hs []workerHandle) bool {
		h, ok := handleFor(hs, "wdetach-control")
		return ok && h.whole()
	})
	ccancel()
	eventually(t, w, "the control pair killed by its own cancelled context", func(hs []workerHandle) bool {
		_, ok := handleFor(hs, "wdetach-control")
		return !ok
	})

	// THE CLAIM: a Worker the WARDEN started outlives the Warden's own context. `reconcile` is given
	// a cancellable context exactly as `run` gives it one, and cancelling it must change nothing.
	_, cancel := context.WithCancel(context.Background())
	turn(t, w, []Spec{spec(t, "wdetach-worker", "0.0.1")}, "the Warden's pair",
		func(hs []workerHandle) bool {
			h, ok := handleFor(hs, "wdetach-worker")
			return ok && h.whole()
		})
	cancel()

	// Long enough that the control above had already died by this point.
	time.Sleep(500 * time.Millisecond)
	hs, err := w.driver.list(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	h, ok := handleFor(hs, "wdetach-worker")
	if !ok || !h.whole() {
		t.Fatalf("cancelling the loop's context killed a Worker the Warden started (%v) — a half killed "+
			"mid-launch is how a handler ends up orphaned with no actor", hs)
	}
}

// --- stopping what is not wanted -------------------------------------------------------------------

// DESIRED STATE, NOT COMMANDS. Nothing told the Warden to stop this Worker; it stopped because it is
// not in the answer any more. That is what makes a reboot cost no replay: there was never a message.
func TestWardenStopsAWorkerThatLeavesTheAssignment(t *testing.T) {
	w := testWarden(t, "wleave")
	turn(t, w, []Spec{spec(t, "wleave-a", "1.0.0"), spec(t, "wleave-b", "1.0.0")},
		"both Workers", func(hs []workerHandle) bool { return len(hs) == 2 })

	turn(t, w, []Spec{spec(t, "wleave-a", "1.0.0")},
		"only the one still in the assignment", func(hs []workerHandle) bool {
			_, a := handleFor(hs, "wleave-a")
			_, b := handleFor(hs, "wleave-b")
			return a && !b
		})
}

// HALF A WORKER IS NOT A WORKER. driver.go makes the PAIR the unit and serve.go says why: "A
// half-dead worker is worse than a dead one: it keeps its Temporal lease and units time out one by
// one." So a pair with one half gone is stopped and started, not left as it is and not repaired by
// restarting the surviving half into a rival.
func TestWardenRestartsAWorkerThatLostAHalf(t *testing.T) {
	w := testWarden(t, "whalf")

	// THE CLOCK IS THE TEST'S, and it has to be. A repair is a RESTART, so it goes through the same
	// backoff as any other, and the first one is `wardenBackoffMin` after the start — which is longer
	// than these turns take on an idle box and shorter than they take on a busy one. Written against
	// the wall clock this passed on the machine it was written on and failed inside the full suite,
	// which is a flake with a plausible story rather than a bug. Driving `now` makes the assertion
	// about the transition and not about how fast the box was.
	now := time.Now()
	w.now = func() time.Time { return now }

	s := spec(t, "whalf-a", "1.0.0")
	hs := turn(t, w, []Spec{s}, "the whole pair", func(hs []workerHandle) bool {
		h, ok := handleFor(hs, "whalf-a")
		return ok && h.whole()
	})
	h, _ := handleFor(hs, "whalf-a")
	first, _ := h.half(partHandler)

	// KILLED BEHIND THE WARDEN'S BACK, which is what a segfault, an OOM kill and an operator's `kill`
	// all look like from here.
	actor, _ := h.half(partActor)
	pid, _ := strconv.Atoi(actor.Ref)
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatalf("killing the actor half out of band: %v", err)
	}
	// CONTROL: the driver really is reporting a half-dead pair, or the branch under test is never
	// reached and this passes for the wrong reason.
	eventually(t, w, "the pair with its actor gone", func(hs []workerHandle) bool {
		h, ok := handleFor(hs, "whalf-a")
		return ok && !h.whole()
	})

	// ONE TRANSITION PER TURN: this turn stops the remains, the next starts a fresh pair.
	turn(t, w, []Spec{s}, "the half-dead pair stopped", func(hs []workerHandle) bool {
		_, ok := handleFor(hs, "whalf-a")
		return !ok
	})
	now = now.Add(wardenBackoffMax) // past the restart backoff, which is not what this test is about
	hs = turn(t, w, []Spec{s}, "a whole pair again", func(hs []workerHandle) bool {
		h, ok := handleFor(hs, "whalf-a")
		return ok && h.whole()
	})
	h, _ = handleFor(hs, "whalf-a")
	again, _ := h.half(partHandler)
	if again.Ref == first.Ref {
		t.Errorf("the surviving handler (pid %s) was left running and the pair rebuilt around it — "+
			"the pair is the unit, so both halves have to be new", first.Ref)
	}
}

// --- the crash that must not be contagious -----------------------------------------------------------

// THE WARDEN SURVIVES A WORKER THAT CRASHES HARD, and goes on to reconcile a healthy one.
//
// "The loop did not die" is a claim that passes against a test where nothing crashed, so this asserts
// in this order:
//
//	the fixture really raises SIGSEGV                       → the crash is real
//	the crashing Worker is started more than once            → the loop kept turning through it
//	a DIFFERENT, healthy Worker reconciles afterwards        → the loop is still doing its job
//
// The third is the one with teeth: a `reconcile` that returned on a driver error, or a `run` that
// propagated one, would leave the healthy Worker unstarted while every other assertion still held.
func TestWardenSurvivesAWorkerThatSegfaults(t *testing.T) {
	w := testWarden(t, "wsegv")

	crash := []string{"sh", "-c", "kill -SEGV $$"}

	// CONTROL: this argv really dies of SIGSEGV. Without it the test is about a process that exited,
	// which is a much weaker thing and is already covered.
	probe := exec.Command(crash[0], crash[1:]...)
	_ = probe.Run()
	ws, ok := probe.ProcessState.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() || ws.Signal() != syscall.SIGSEGV {
		t.Fatalf("the fixture did not segfault (%v), so nothing here tests a hard crash", probe.ProcessState)
	}

	crasher := Spec{
		Name: "wsegv-crasher", Version: "0.0.1",
		Actor:   ProcSpec{Dir: t.TempDir(), Argv: crash, Env: os.Environ()},
		Handler: sleeper(t),
	}
	healthy := spec(t, "wsegv-healthy", "0.0.1")

	// Turn the loop by hand rather than through `run`, so a failure points at a turn rather than at a
	// timeout — and step the WARDEN's clock past the restart backoff on each turn, because the
	// backoff is a real property of this loop (a crash-looping Worker is rate-limited) and it is not
	// the property under test here. Against the wall clock this asserted "restarted more than once"
	// inside a 600ms window with a one-second minimum backoff, which passed only when the box was
	// slow enough — a flake that took the full suite to find.
	now := time.Now()
	w.now = func() time.Time { return now }
	for i := 0; i < 12; i++ {
		w.reconcile(context.Background(), []Spec{crasher, healthy})
		time.Sleep(50 * time.Millisecond)
		now = now.Add(wardenBackoffMax)
	}

	if n := w.startsOf("wsegv-crasher@0.0.1"); n < 2 {
		t.Errorf("the crashing Worker was started %d time(s); the loop is supposed to keep restarting it", n)
	}
	// THE ASSERTION WITH TEETH.
	hs := eventually(t, w, "the healthy Worker, reconciled despite its neighbour crashing",
		func(hs []workerHandle) bool {
			h, ok := handleFor(hs, "wsegv-healthy")
			return ok && h.whole()
		})
	if _, ok := handleFor(hs, "wsegv-healthy"); !ok {
		t.Fatalf("the healthy Worker never started: %v", ids(hs))
	}
	// …and the Warden is a different process from everything it started, which is the structural
	// half of "it runs no actor code". A loop that ran a Worker in-process could not say this.
	me := strconv.Itoa(os.Getpid())
	for _, h := range hs {
		for _, half := range h.Halves {
			if half.Ref == me {
				t.Errorf("%s's %s half IS this process — the Warden is running actor code", h.id(), half.Part)
			}
		}
	}
}

// A CRASH LOOP IS RATE-LIMITED, or a bad image spins the Machine's CPU restarting it. The backoff is
// this Warden's rate limit on its OWN actions and nothing reads it to answer "is this Worker up" —
// see go. Driven through the injected clock so the test asserts the schedule rather than
// waiting it out.
func TestWardenBacksOffAWorkerItKeepsRestarting(t *testing.T) {
	w := testWarden(t, "wback")
	now := time.Now()
	w.now = func() time.Time { return now }

	dead := Spec{
		Name: "wback-a", Version: "0.0.1",
		// A binary that does not exist: `start` fails immediately and nothing is left running, so
		// every turn takes the "missing" branch.
		Actor:   ProcSpec{Dir: t.TempDir(), Argv: []string{"/nonexistent/kontra-no-such-binary"}, Env: os.Environ()},
		Handler: sleeper(t),
	}

	w.reconcile(context.Background(), []Spec{dead})
	if got := w.restart("wback-a@0.0.1").n; got != 1 {
		t.Fatalf("the first turn started it %d times, want 1", got)
	}
	// CONTROL: with the clock frozen, a second turn must NOT start it — if it did, there is no
	// backoff and every assertion below would pass against a loop that just retries as fast as it can.
	w.reconcile(context.Background(), []Spec{dead})
	if got := w.restart("wback-a@0.0.1").n; got != 1 {
		t.Fatalf("the frozen clock did not hold the restart back: n=%d", got)
	}
	// …and once the delay has passed, it does.
	now = now.Add(wardenBackoffMin + time.Millisecond)
	w.reconcile(context.Background(), []Spec{dead})
	if got := w.restart("wback-a@0.0.1").n; got != 2 {
		t.Fatalf("after the first delay it started %d times, want 2", got)
	}
	// The delay doubles, so the SAME step forward is no longer enough.
	now = now.Add(wardenBackoffMin + time.Millisecond)
	w.reconcile(context.Background(), []Spec{dead})
	if got := w.restart("wback-a@0.0.1").n; got != 2 {
		t.Errorf("the delay did not grow: after %s it started again (n=%d)", wardenBackoffMin, got)
	}
	// …and it is capped, or a Worker that has been down all night is never retried again.
	r := w.restart("wback-a@0.0.1")
	r.n = 40
	r.at = now
	if d := w.backoff(r); d > wardenBackoffMax {
		t.Errorf("the backoff at restart %d is %s, past the %s cap", r.n, d, wardenBackoffMax)
	}
}

// --- what survives a Warden, and what does not ---------------------------------------------------------

// A KILLED WARDEN RESTARTS AND RE-RECONCILES, WITH NOTHING CARRIED OVER. The "restart" here is a
// second `warden` value that shares no field with the first — no handles, no restart counters, no
// record of what it started — because that is what a process that was SIGKILLed and brought back by
// systemd actually has.
//
// Two things must then be true, and they are opposite: the Worker that is still wanted is ADOPTED
// (not restarted), and the one that is not is COLLECTED (even though this Warden never started it).
func TestWardenReconcilesAfterARestartWithNothingCarriedOver(t *testing.T) {
	first := testWarden(t, "wboot")
	keep, drop := spec(t, "wboot-keep", "1.0.0"), spec(t, "wboot-drop", "1.0.0")
	hs := turn(t, first, []Spec{keep, drop}, "both Workers", func(hs []workerHandle) bool {
		a, aok := handleFor(hs, "wboot-keep")
		b, bok := handleFor(hs, "wboot-drop")
		return aok && a.whole() && bok && b.whole()
	})
	before, _ := handleFor(hs, "wboot-keep")
	beforeActor, _ := before.half(partActor)

	// THE REBOOT. A fresh value, and the assertion that it really is one: a Warden that had somehow
	// inherited the first's memory would make everything below prove nothing.
	second := testWarden(t, "wboot")
	if len(second.restarts) != 0 || second.hasDesired {
		t.Fatalf("the second Warden was born holding state: restarts=%v hasDesired=%v", second.restarts, second.hasDesired)
	}

	after := turn(t, second, []Spec{keep}, "the adopted Worker, alone", func(hs []workerHandle) bool {
		a, aok := handleFor(hs, "wboot-keep")
		_, bok := handleFor(hs, "wboot-drop")
		return aok && a.whole() && !bok
	})
	a, _ := handleFor(after, "wboot-keep")
	afterActor, _ := a.half(partActor)
	if afterActor.Ref != beforeActor.Ref {
		t.Errorf("the surviving Worker was restarted by the new Warden: pid %s became %s",
			beforeActor.Ref, afterActor.Ref)
	}
}

// THE STATE DIRECTORY HOLDS AN IDENTITY AND NOTHING ELSE. driver.go refuses a local database of what
// is running one level down ("a pidfile, a JSON file under a state directory, or a map in the driver
// would all be the local database this seam refuses"); a Warden that wrote a `workers.json` beside
// its key would have re-introduced it one directory up. So the contents are asserted, by name.
func TestWardenWritesNoRecordOfWhatIsRunning(t *testing.T) {
	w := testWarden(t, "wstate")
	dir := t.TempDir()
	w.id = &wardenIdentity{Dir: dir}

	// CONTROL: the directory is real and writable, so "nothing appeared in it" is not "nothing could
	// have".
	if err := os.WriteFile(filepath.Join(dir, "canary"), []byte("x"), 0o600); err != nil {
		t.Fatalf("the state directory is not writable, so this test proves nothing: %v", err)
	}

	turn(t, w, []Spec{spec(t, "wstate-a", "1.0.0")}, "the Worker", func(hs []workerHandle) bool {
		h, ok := handleFor(hs, "wstate-a")
		return ok && h.whole()
	})
	turn(t, w, nil, "no Workers", func(hs []workerHandle) bool { return len(hs) == 0 })

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() == "canary" {
			continue
		}
		t.Errorf("the Warden wrote %q into its state directory; the runtime is what says what is "+
			"running, and a file here is the local database driver.go refuses", e.Name())
	}
}

// --- an unreachable Controller -----------------------------------------------------------------------

// A CONTROLLER THAT STOPS ANSWERING MUST NOT TEAR THE MACHINE DOWN. The desired state did not change;
// only the ability to ask did. A loop that reconciled against an empty answer would turn every
// network blip, every orchestrator restart and every rolling upgrade into a Fleet-wide outage.
//
// CONTROL FIRST: the same Warden, with the same broken fetch, must ALSO refuse to start anything when
// it has never had an assignment — otherwise "it held the last one" is indistinguishable from "it
// ignores the fetch entirely".
func TestWardenHoldsTheLastAssignmentWhenTheControllerGoesAway(t *testing.T) {
	w := testWarden(t, "wheld")
	w.id = &wardenIdentity{Record: wardenRecord{Controller: "https://127.0.0.1:1"}} // nothing listens there
	w.http = &http.Client{Timeout: 200 * time.Millisecond}

	// CONTROL: never had an assignment ⇒ holds nothing, starts nothing.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	_ = w.run(ctx)
	cancel()
	if hs, _ := w.driver.list(context.Background()); len(hs) != 0 {
		t.Fatalf("a Warden with no assignment started something: %v", ids(hs))
	}

	// Now give it one, the way a successful fetch would have.
	w.desired, w.hasDesired = []Spec{spec(t, "wheld-a", "1.0.0")}, true
	ctx, cancel = context.WithTimeout(context.Background(), 3*time.Second)
	go func() { _ = w.run(ctx) }()
	eventually(t, w, "the Worker started from the held assignment", func(hs []workerHandle) bool {
		h, ok := handleFor(hs, "wheld-a")
		return ok && h.whole()
	})
	// …and it is STILL there after several more failed fetches.
	time.Sleep(500 * time.Millisecond)
	hs, err := w.driver.list(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if h, ok := handleFor(hs, "wheld-a"); !ok || !h.whole() {
		t.Errorf("the Worker was torn down because the Controller was unreachable: %v", hs)
	}
	cancel()
}

// --- outbound only --------------------------------------------------------------------------------

// A MACHINE OPENS NO LISTENING SOCKET. ADR 0037's whole topology rests on it — "Every arrow is
// dialled by the Machine. No inbound ports; NAT and private networks work unchanged" — and it is the
// property most likely to be lost by accident, because slice 05 has to get pane frames off the
// Machine and a listener is the obvious way to do it.
//
// THE DETECTOR IS ASSERTED BEFORE THE CLAIM IS, which is this repo's rule for negative tests: a
// listener is opened in this process and the detector must find it, then it is closed and the
// detector must lose it. Without those two lines, "the Warden opened none" passes against a detector
// that finds nothing at all.
func TestWardenOpensNoListeningSocket(t *testing.T) {
	w := testWarden(t, "wlisten")
	if _, err := os.Stat("/proc/net/tcp"); err != nil {
		t.Skipf("no /proc/net/tcp here, so nothing can be observed about listeners: %v", err)
	}

	// CONTROL 1: the detector finds a listener this process really has.
	base := len(ownListeners(t))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("cannot open a control listener: %v", err)
	}
	if n := len(ownListeners(t)); n <= base {
		_ = ln.Close()
		t.Fatalf("the detector did not see a listener this process just opened (%d then %d) — "+
			"every assertion below would be vacuous", base, n)
	}
	// CONTROL 2: …and loses it again, so it is reporting the present rather than accumulating.
	_ = ln.Close()
	deadline := time.Now().Add(2 * time.Second)
	for len(ownListeners(t)) > base && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := len(ownListeners(t)); n > base {
		t.Fatalf("the detector still reports %d listeners after the control was closed (was %d)", n, base)
	}

	// THE CLAIM: a full turn of the loop adds none — SAMPLED WHILE IT IS RUNNING, not once after it
	// has stopped. Found by mutation: a listener opened at the top of `run` under a `defer Close()`
	// is invisible to a sample taken after `run` returns, and that is EXACTLY the shape slice 05
	// would add for pane frames — open for the Warden's lifetime, closed on shutdown. A test that
	// only looked afterwards would have waved it through.
	w.id = &wardenIdentity{Record: wardenRecord{Controller: "https://127.0.0.1:1"}}
	w.http = &http.Client{Timeout: 200 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = w.run(ctx); close(done) }()

	worst, deadline := base, time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if n := len(ownListeners(t)); n > worst {
			worst = n
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done

	// …and a reconcile that actually starts a Worker adds none either.
	turn(t, w, []Spec{spec(t, "wlisten-a", "1.0.0")}, "the Worker", func(hs []workerHandle) bool {
		h, ok := handleFor(hs, "wlisten-a")
		return ok && h.whole()
	})
	if n := len(ownListeners(t)); n > worst {
		worst = n
	}
	if worst > base {
		t.Errorf("the Warden opened %d listening socket(s); a Machine accepts no inbound connection "+
			"(ADR 0037). If a later slice needs one, that is a decision to argue for, not to add", worst-base)
	}
}

// ownListeners is every listening socket THIS PROCESS holds: the intersection of the kernel's
// listening set (/proc/net/tcp*, /proc/net/unix) with this process's own socket inodes
// (/proc/self/fd). Both halves are needed — the first alone would report the whole machine's
// listeners and the second alone cannot tell a listening socket from a dialled one.
func ownListeners(t *testing.T) []uint64 {
	t.Helper()
	listening := map[uint64]bool{}
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		body, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(body), "\n")[1:] {
			fields := strings.Fields(line)
			// st is field 3 and `0A` is TCP_LISTEN; the inode is field 9.
			if len(fields) < 10 || fields[3] != "0A" {
				continue
			}
			if n, err := strconv.ParseUint(fields[9], 10, 64); err == nil {
				listening[n] = true
			}
		}
	}
	if body, err := os.ReadFile("/proc/net/unix"); err == nil {
		for _, line := range strings.Split(string(body), "\n")[1:] {
			fields := strings.Fields(line)
			// St is field 5 and `01` is SS_UNCONNECTED, which for a stream socket in this file means
			// listening; the inode is field 6.
			if len(fields) < 7 || fields[5] != "01" {
				continue
			}
			if n, err := strconv.ParseUint(fields[6], 10, 64); err == nil {
				listening[n] = true
			}
		}
	}
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatalf("cannot read this process's file descriptors: %v", err)
	}
	var out []uint64
	for _, e := range entries {
		target, err := os.Readlink(filepath.Join("/proc/self/fd", e.Name()))
		if err != nil {
			continue
		}
		inode := strings.TrimSuffix(strings.TrimPrefix(target, "socket:["), "]")
		if inode == target {
			continue
		}
		n, err := strconv.ParseUint(inode, 10, 64)
		if err == nil && listening[n] {
			out = append(out, n)
		}
	}
	return out
}

// --- the assignment's shape ------------------------------------------------------------------------

// AN ASSIGNMENT NAMES AN ARTIFACT AND AN ENVIRONMENT, and the environment it produces must be
// DETERMINISTIC. Go randomises map iteration on purpose, so an unsorted render would make two
// identical assignments produce two different `podman run` argvs — invisible until something compares
// them, and then invisible again because the difference is only ordering.
func TestAnAssignmentRendersOneEnvironmentEveryTime(t *testing.T) {
	a := assignedWorker{
		Name: "nscheck", Version: "0.1.0",
		Image: "localhost:5000/nscheck@sha256:" + strings.Repeat("a", 64),
		Env:   map[string]string{"B": "2", "A": "1", "C": "3", "D": "4", "E": "5"},
		Actor: assignedHalf{Env: map[string]string{"PYTHONPATH": "/opt", "A": "overridden"}},
	}
	first := a.spec("acme")
	for i := 0; i < 20; i++ {
		got := a.spec("acme")
		if strings.Join(got.Actor.Env, "\x00") != strings.Join(first.Actor.Env, "\x00") {
			t.Fatalf("two renders of one assignment differ:\n  %v\n  %v", first.Actor.Env, got.Actor.Env)
		}
	}
	// The half's own value wins over the Worker's, or a per-half PYTHONPATH could never override a
	// shared one.
	if !contains(first.Actor.Env, "A=overridden") || contains(first.Actor.Env, "A=1") {
		t.Errorf("the half's environment did not override the Worker's: %v", first.Actor.Env)
	}
	// CONTROL: the Worker's own values are still there, so the override did not replace the map.
	if !contains(first.Actor.Env, "B=2") {
		t.Errorf("the Worker's environment was lost: %v", first.Actor.Env)
	}
	if first.Image != a.Image {
		t.Errorf("the Artifact was not carried through: %q", first.Image)
	}
	// The HANDLER half gets the Worker's environment and NOT the actor's — a handler that inherited
	// the actor's PYTHONPATH would be a leak between two processes that ADR 0036 keeps apart.
	if contains(first.Handler.Env, "PYTHONPATH=/opt") {
		t.Errorf("the actor's own environment reached the handler: %v", first.Handler.Env)
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// --- the unit that supervises it ------------------------------------------------------------------

// THE SYSTEMD UNIT IS THE OTHER HALF OF "A WORKER CRASH MUST NOT TAKE THE WARDEN WITH IT", and two of
// its lines exist only to overrule a DEFAULT that would do exactly that:
//
//	OOMPolicy=  defaults to `stop` (via DefaultOOMPolicy=), so when the kernel OOM-kills any process
//	            in this unit's cgroup — which, under the `process` driver, includes every Worker's
//	            halves — systemd stops the UNIT. "An actor used too much memory" becomes "the Warden
//	            is gone and nothing will restart it".
//	KillMode=   defaults to `control-group`, so `systemctl restart kontra-warden` SIGKILLs every
//	            Worker on the Machine and the Fleet's work restarts with its supervisor.
//
// Neither is observable from a test that only reads the unit back — what is observable is whether the
// decision is still written down, in the section where systemd reads it. So the assertions are on the
// parsed `[Service]` section rather than on the file, and `serviceSection` drops comments, so a line
// that survives only inside the explanatory comment above it does not count.
func TestTheWardenUnitOverridesTheDefaultsThatWouldKillIt(t *testing.T) {
	const state, driver = "/srv/kontra/warden-state", "process"
	unit := wardenUnit(state, driver, trustpolicy.Options{})
	svc := serviceSection(t, unit, "Service")

	// CONTROL: the parser really did drop the comments, or "it is in [Service]" is just "it is in the
	// file" and the two lines could be sitting in the prose above them.
	for _, line := range svc {
		if strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			t.Fatalf("the section parser kept a comment (%q), so every assertion below is weaker than it looks", line)
		}
	}
	if len(svc) == 0 {
		t.Fatal("the unit has no [Service] section at all")
	}

	for _, want := range []string{"OOMPolicy=continue", "KillMode=process", "Restart=always", "Type=simple"} {
		if !contains(svc, want) {
			t.Errorf("[Service] does not carry %q — see this test's header for what its default does instead", want)
		}
	}
	if !contains(serviceSection(t, unit, "Install"), "WantedBy=multi-user.target") {
		t.Error("the unit is not enabled at boot, so a rebooted Machine holds no Workers until somebody logs in")
	}

	// THE STATE DIRECTORY AND THE DRIVER ARE BAKED IN, and a NON-DEFAULT pair is used here on purpose:
	// a unit that hardcoded `/var/lib/kontra/warden` would pass against the default and produce, in
	// the field, a Warden that enrolled into one directory and served from another — which fails as
	// "not enrolled" with the identity sitting right there.
	var exec string
	for _, line := range svc {
		if strings.HasPrefix(line, "ExecStart=") {
			exec = line
		}
	}
	if exec == "" {
		t.Fatal("the unit has no ExecStart")
	}
	for _, want := range []string{"warden serve", "--state " + state, "--driver " + driver} {
		if !strings.Contains(exec, want) {
			t.Errorf("ExecStart is %q; it does not carry %q", exec, want)
		}
	}

	// AND IT CARRIES NO CREDENTIAL. `/etc/systemd/system` is world-readable and a unit ends up in
	// `systemctl cat` and in journald; the enrolment token was spent by `join` before this was
	// written, and the identity it bought lives in the state directory at 0600. The unit names the
	// directory and nothing that is in it.
	for _, never := range []string{enrolTokenPrefix + ".", "--token", "PRIVATE KEY", "Environment="} {
		if strings.Contains(unit, never) {
			t.Errorf("the unit carries %q; it is world-readable and this is not the place for one", never)
		}
	}
}

// THE TRUST POLICY HAS TO SURVIVE INTO THE UNIT, and the reason is the same one the state directory
// has: every trust flag defaults to an environment variable, and systemd starts a unit with an
// almost-empty environment. A unit that did not carry the policy would hand the installed Warden an
// EMPTY ALLOWLIST on every boot however the operator's shell was configured — and an empty allowlist
// refuses every placement, so the Machine would enrol, attach, report health and place nothing.
//
// The absence half is asserted too: an unset flag must not appear as `--trust-key ""`, because a unit
// whose ExecStart lists five empty flags does not read as the decision that was made.
func TestTheWardenUnitCarriesTheTrustPolicyItWasJoinedWith(t *testing.T) {
	unit := wardenUnit("/srv/kontra/warden-state", "podman", trustpolicy.Options{
		Registries: "ghcr.io/acme,localhost:5000",
		Unsigned:   "localhost:5000",
		Identity:   "https://github.com/acme/actors/.github/workflows/release.yml@refs/heads/main",
		Issuer:     "https://token.actions.githubusercontent.com",
	})
	var exec string
	for _, line := range serviceSection(t, unit, "Service") {
		if strings.HasPrefix(line, "ExecStart=") {
			exec = line
		}
	}
	if exec == "" {
		t.Fatal("the unit has no ExecStart")
	}
	for _, want := range []string{
		`--trust-registries "ghcr.io/acme,localhost:5000"`,
		`--trust-unsigned "localhost:5000"`,
		`--trust-issuer "https://token.actions.githubusercontent.com"`,
		"--trust-identity",
	} {
		if !strings.Contains(exec, want) {
			t.Errorf("ExecStart does not carry %q, so this Machine's Warden boots with a different policy "+
				"than it was joined with:\n%s", want, exec)
		}
	}
	if strings.Contains(exec, "--trust-key") {
		t.Errorf("ExecStart names --trust-key, which was never set:\n%s", exec)
	}
}

// serviceSection returns one systemd section's directives, without comments or blank lines.
func serviceSection(t *testing.T, unit, name string) []string {
	t.Helper()
	var out []string
	in := false
	for _, raw := range strings.Split(unit, "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			in = line == "["+name+"]"
			continue
		}
		if !in || line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		out = append(out, line)
	}
	return out
}

// --- an assignment the Controller got wrong ------------------------------------------------------

// A MALFORMED ASSIGNMENT MUST NOT BE A WARDEN CRASH LOOP. This is "the Warden runs no actor code"
// extended to the other thing that arrives from outside: the assignment itself. Two entries here are
// not merely wrong, they are actively worse than wrong —
//
//	a version holding the label separator  → the Worker starts, `list` cannot see it under the id
//	                                         the loop asked for, and it is started AGAIN every turn
//	a half with no argv, under `process`   → `driver_process.go` indexes `p.Argv[0]`, which panics;
//	                                         systemd restarts the Warden, which fetches the same
//	                                         assignment and panics again
//
// — and both are reachable from a JSON file the Machine did not write.
//
// THE CONTROLS ARE THE FIRST TWO ASSERTIONS. `a/b` must still be accepted, because
// `shared/conformance/queues.json` carries it as an actor name that must keep working and a refusal aimed at
// the separator rather than at the round trip would break it; and a well-formed neighbour must still
// start, or "it did not crash" would be indistinguishable from "it did nothing".
func TestWardenRefusesAnAssignmentEntryThatWouldCrashOrDuplicate(t *testing.T) {
	w := testWarden(t, "wbad")

	// CONTROL 1: the adversarial NAME from the corpus is fine — a slash in the name round-trips.
	if err := w.wellFormed(spec(t, "wbad/inner", "1.0.0")); err != nil {
		t.Fatalf("a slash in the NAME was refused, which would stop serving an actor the corpus "+
			"requires: %v", err)
	}
	// CONTROL 2: an ordinary spec is fine, so the refusals below are about what changed.
	if err := w.wellFormed(spec(t, "wbad-ok", "1.0.0")); err != nil {
		t.Fatalf("a well-formed spec was refused: %v", err)
	}

	for _, tc := range []struct {
		what string
		spec Spec
	}{
		{"a version holding the label separator", spec(t, "wbad-v", "1/2")},
		{"an empty name", Spec{Name: "", Version: "1.0.0", Actor: sleeper(t), Handler: sleeper(t)}},
		{"an empty version", Spec{Name: "wbad-e", Version: "", Actor: sleeper(t), Handler: sleeper(t)}},
		{"a half with no command", Spec{
			Name: "wbad-noargv", Version: "1.0.0",
			Image:   "localhost:5000/x@sha256:" + strings.Repeat("a", 64),
			Actor:   ProcSpec{Dir: t.TempDir(), Env: os.Environ()},
			Handler: sleeper(t),
		}},
	} {
		if err := w.wellFormed(tc.spec); err == nil {
			t.Errorf("%s was accepted", tc.what)
		}
	}

	// AND THE LOOP SURVIVES ONE. A turn holding a spec that would panic the driver must reconcile the
	// rest of the assignment — this is the assertion that fails as a panic rather than as a
	// comparison if the guard is not there at all.
	crasher := Spec{
		Name: "wbad-noargv", Version: "1.0.0",
		Actor:   ProcSpec{Dir: t.TempDir(), Env: os.Environ()},
		Handler: sleeper(t),
	}
	turn(t, w, []Spec{crasher, spec(t, "wbad-healthy", "1.0.0")},
		"the healthy neighbour of an unrunnable entry", func(hs []workerHandle) bool {
			h, ok := handleFor(hs, "wbad-healthy")
			return ok && h.whole()
		})
	hs, err := w.driver.list(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := handleFor(hs, "wbad-noargv"); ok {
		t.Errorf("the unrunnable entry was started anyway: %v", ids(hs))
	}
}
