// driver_test.go — the driver seam, and the one property every driver behind it owes.
//
// THE PROPERTY IS THAT `list` READS THE RUNTIME. driver.go's header calls it the single most common
// bug in this class of software and it is invisible from inside: a driver that answers from its own
// memory of what it started passes every test written about starting and stopping, and fails on the
// day something else started a Worker or a Worker died without asking. So the two tests that matter
// here both go AROUND the driver — one starts a process it never told the driver about, the other
// kills one behind its back — because a test that only ever asks the driver about its own actions
// cannot tell the two implementations apart.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// requireProcTable skips where the process driver cannot enumerate — see
// driver_proctable_other.go. On Linux, which is what a **Machine** and every CI runner is, it
// never skips.
func requireProcTable(t *testing.T) {
	t.Helper()
	if _, err := labelledProcs(); err != nil {
		t.Skipf("no process table to read here: %v", err)
	}
}

// waitForWorker polls `list` for one Worker by name. Polling because a process appearing in and
// disappearing from the process table is not synchronous with the call that caused it; the
// assertions are on WHAT is found, never on how long it took.
func waitForWorker(t *testing.T, d *processDriver, name, version string, want func(workerHandle) bool, what string) workerHandle {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var last []workerHandle
	for time.Now().Before(deadline) {
		hs, err := d.list(context.Background())
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		last = hs
		for _, h := range hs {
			if h.Name == name && h.Version == version && want(h) {
				return h
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("list never reported %s: %s\n  it reported: %v", name, what, last)
	return workerHandle{}
}

// waitForNoWorker is the other half, and it is a SEPARATE function rather than a predicate because
// "eventually absent" and "eventually matching" fail differently: this one has to keep looking until
// the deadline and then say what was still there.
func waitForNoWorker(t *testing.T, d *processDriver, name string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		hs, err := d.list(context.Background())
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		still := false
		for _, h := range hs {
			if h.Name == name {
				still = true
			}
		}
		if !still {
			return
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("list still reports %s after it is gone: %v", name, hs)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// startLabelled starts a process the driver knows nothing about, wearing a Worker's label. This is
// how a Worker gets onto a Machine without this driver putting it there: the Warden that was running
// before this one restarted, or an operator's own `kontra serve`.
//
// ITS OWN PROCESS GROUP, AND THE GROUP IS WHAT GETS KILLED. Measured while writing this file: the
// forked fixture below leaves a `sleep` behind when only its shell is killed, init adopts it, and it
// is STILL LABELLED — so the next run of the suite found a second root for the same Worker and the
// pid assertion failed against a process from the previous run. The leak is the test's, but the
// behaviour it exposed is the driver's and is correct: an orphaned descendant becomes a root,
// because it is still serving.
func startLabelled(t *testing.T, name, version string, part workerPart, argv ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), workerLabelEnv(name, version, part))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the out-of-band process: %v", err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	return cmd
}

// A WORKER THIS DRIVER DID NOT START IS STILL A WORKER ON THIS MACHINE. A driver that answers from
// its own memory reports nothing here and reconcile then starts a SECOND Worker on a queue that
// already has one — two rival pollers on `<name>-<version>`, which identity.go describes as a worker
// that "can take its continue_as_new and run different code".
func TestListReportsAWorkerThisDriverDidNotStart(t *testing.T) {
	requireProcTable(t)
	d := &processDriver{out: io.Discard, err: io.Discard}

	ghost := startLabelled(t, "listghost", fixtureVersion("9.9.9"), partActor, "sleep", "600")

	h := waitForWorker(t, d, "listghost", fixtureVersion("9.9.9"),
		func(h workerHandle) bool { _, ok := h.half(partActor); return ok },
		"a Worker started outside the driver, with its actor half")

	half, _ := h.half(partActor)
	if half.Ref != strconv.Itoa(ghost.Process.Pid) {
		t.Errorf("the handle names pid %s; the process the driver never started is %d", half.Ref, ghost.Process.Pid)
	}
	if h.Driver != "process" {
		t.Errorf("handle carries driver %q, want process", h.Driver)
	}
	// ONE HALF IS A REAL STATE and the handle has to be able to say so — this is the "half-dead
	// worker" serve.go calls worse than a dead one, and a seam that could only report pairs would
	// hide it.
	if h.whole() {
		t.Errorf("a Worker with only an actor reported as whole: %v", h)
	}
}

// AND ONE THAT DIED IS GONE, WITH NOTHING TO INVALIDATE. The driver started this pair itself, so a
// memory-backed `list` would keep reporting it forever — the failure that leaves reconcile satisfied
// while the queue has had no poller for an hour.
func TestListForgetsAWorkerThatDied(t *testing.T) {
	requireProcTable(t)
	d := &processDriver{out: io.Discard, err: io.Discard}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sleeper := procSpec{Dir: t.TempDir(), Argv: []string{"sleep", "600"}, Env: os.Environ()}
	h, err := d.start(ctx, workerSpec{Name: "listdies", Version: fixtureVersion("0.0.1"), Actor: sleeper, Handler: sleeper})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	waitForWorker(t, d, "listdies", fixtureVersion("0.0.1"), workerHandle.whole, "the pair it just started")

	// KILLED BEHIND THE DRIVER'S BACK, which is what a segfault, an OOM kill and an operator's
	// `kill` all look like from here.
	actor, _ := h.half(partActor)
	pid, _ := strconv.Atoi(actor.Ref)
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatalf("killing the actor half out of band: %v", err)
	}
	got := waitForWorker(t, d, "listdies", fixtureVersion("0.0.1"),
		func(h workerHandle) bool { _, ok := h.half(partActor); return !ok },
		"the pair with its actor gone")
	if _, ok := got.half(partHandler); !ok {
		t.Errorf("the surviving handler half was dropped too: %v", got)
	}

	handler, _ := h.half(partHandler)
	pid, _ = strconv.Atoi(handler.Ref)
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatalf("killing the handler half out of band: %v", err)
	}
	waitForNoWorker(t, d, "listdies")
}

// A HALF IS A PROCESS TREE, NOT A PROCESS. The handler is `go run .`, which compiles and then execs
// a second binary carrying the same inherited environment — so the scan finds two labelled processes
// for one half, and a driver that reported both would tell the Warden a Worker it started once is
// running twice.
func TestListReportsOneHalfPerProcessTree(t *testing.T) {
	requireProcTable(t)
	d := &processDriver{out: io.Discard, err: io.Discard}

	// `sh -c 'sleep 600 & wait'` forks rather than execs, so the shell survives beside its child and
	// both carry the label — the same two-processes-one-half shape `go run` produces.
	parent := startLabelled(t, "listtree", fixtureVersion("0.0.2"), partHandler, "sh", "-c", "sleep 600 & wait")

	h := waitForWorker(t, d, "listtree", fixtureVersion("0.0.2"),
		func(h workerHandle) bool { _, ok := h.half(partHandler); return ok },
		"the forked half")

	// THE FIXTURE HAS TO ACTUALLY FORK or this test proves nothing: a shell that execs its argument
	// leaves one process, and then a driver with no dedupe at all would pass.
	procs, err := labelledProcs()
	if err != nil {
		t.Fatalf("labelledProcs: %v", err)
	}
	// `fixtureVersion` AND NOT THE BARE STRING. The fixture is started under a pid-suffixed version
	// (so two concurrent test processes cannot claim one another's Workers) and this lookup was left
	// on the literal — so it matched nothing, counted zero, and the guard below fired. The guard was
	// right and its input was wrong, which is the EASY direction: it failed loudly rather than
	// passing vacuously, which is the whole reason that guard counts before it asserts.
	//
	// (Introduced and fixed on the same day, twice, independently — once here by the slice that hit
	// it and once on the mainline. Both landed; this is the one with the reason written down.)
	label := workerLabel("listtree", fixtureVersion("0.0.2"), partHandler)
	n := 0
	for _, p := range procs {
		if p.label == label {
			n++
		}
	}
	if n < 2 {
		t.Fatalf("the fixture left %d labelled process(es), so the dedupe is untested — it must fork", n)
	}

	if len(h.Halves) != 1 {
		t.Fatalf("%d labelled processes became %d halves: %v", n, len(h.Halves), h.Halves)
	}
	half := h.Halves[0]
	if half.Ref != strconv.Itoa(parent.Process.Pid) {
		t.Errorf("the half names pid %s; the ROOT of the tree is %d", half.Ref, parent.Process.Pid)
	}
}

// AN ACTOR WHOSE NAME HOLDS THE LABEL'S SEPARATOR IS STILL A WORKER, and the parse is the only thing
// standing between it and invisibility. `shared/conformance/queues.json` carries `a/b` as an adversarial
// actor name and pins that every queue derivation passes it through verbatim, so this is a Worker
// that starts, serves and polls perfectly — and a `list` that split the label left-to-right would
// never report it again. An invisible Worker arriving THROUGH the seam built to prevent invisible
// Workers is the failure worth a test of its own.
func TestListSeesAnActorWhoseNameHoldsTheSeparator(t *testing.T) {
	requireProcTable(t)
	d := &processDriver{out: io.Discard, err: io.Discard}

	ghost := startLabelled(t, "listslash/inner", fixtureVersion("1.0.0"), partHandler, "sleep", "600")

	h := waitForWorker(t, d, "listslash/inner", fixtureVersion("1.0.0"),
		func(h workerHandle) bool { _, ok := h.half(partHandler); return ok },
		"a Worker whose name contains a slash")
	// AGAINST THE VERSION THE FIXTURE ACTUALLY USED, which is pid-suffixed. Comparing to the bare
	// `"1.0.0"` made this fail for the one reason it was written to rule out — it read as "the name
	// ate the version" when the parse was perfect and the expectation was stale. Same pre-existing
	// miss as the label above.
	if want := fixtureVersion("1.0.0"); h.Version != want {
		t.Errorf("the name ate the version: name=%q version=%q, want %q", h.Name, h.Version, want)
	}
	if h.Name != "listslash/inner" {
		t.Errorf("the slash was eaten: name=%q", h.Name)
	}
	half, _ := h.half(partHandler)
	if half.Ref != strconv.Itoa(ghost.Process.Pid) {
		t.Errorf("the half names pid %s, want %d", half.Ref, ghost.Process.Pid)
	}
}

// STOP ENDS BOTH HALVES, and `list` is what says so — asking the driver whether it thinks it stopped
// something is the question that always answers yes.
func TestStopEndsBothHalvesOfThePair(t *testing.T) {
	requireProcTable(t)
	d := &processDriver{out: io.Discard, err: io.Discard}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sleeper := procSpec{Dir: t.TempDir(), Argv: []string{"sleep", "600"}, Env: os.Environ()}
	h, err := d.start(ctx, workerSpec{Name: "stoppair", Version: fixtureVersion("0.0.3"), Actor: sleeper, Handler: sleeper})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	waitForWorker(t, d, "stoppair", fixtureVersion("0.0.3"), workerHandle.whole, "the pair it just started")

	if err := d.stop(context.Background(), h, 2*time.Second); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitForNoWorker(t, d, "stoppair")
}

// THE LABEL IS THE ONE PLACE AN ARBITRARY PROCESS GETS TO CLAIM IT IS OURS, so the parse is strict —
// a lenient one turns a stranger's `KONTRA_WORKER=x` into a Worker the Warden then tries to stop.
func TestWorkerLabelRoundTripsAndRefusesEverythingElse(t *testing.T) {
	// THE NAMES ARE THE CORPUS'S, not invented here. shared/conformance/queues.json states that "A QUEUE
	// NAME IS NOT SANITISED" and carries these to prove it — `a/b` is the one that matters, because
	// it holds the label's own separator and a left-to-right parse refuses it.
	for _, id := range []struct{ name, version string }{
		{"nscheck", "0.1.0"},
		{"a/b", "1.0.0"},
		{"my actor", "0.1.0"},
		{"café", "0.1.0"},
		{"http-fuzzer", "0.1.0"},
		{"crawl4ai", "0.5.0-rc1"},
		{"probe", "shared"},
	} {
		for _, part := range workerParts {
			name, version, got, ok := parseWorkerLabel(workerLabel(id.name, id.version, part))
			if !ok || name != id.name || version != id.version || got != part {
				t.Errorf("round trip of %q@%q/%s lost something: %q %q %q %v",
					id.name, id.version, part, name, version, got, ok)
			}
		}
	}
	for _, bad := range []string{
		"",                     // an empty variable
		"nscheck",              // a name on its own
		"nscheck/0.1.0",        // no part
		"nscheck/0.1.0/actor/", // a trailing separator is a fourth, empty field
		"nscheck/0.1.0/worker", // a part this file does not know
		"/0.1.0/actor",         // no name
		"nscheck//actor",       // no version
	} {
		if _, _, _, ok := parseWorkerLabel(bad); ok {
			t.Errorf("parseWorkerLabel(%q) accepted a label it should refuse", bad)
		}
	}
}

// A FOREGROUND WORKER HAS NO LOG, AND SAYING SO IS THE ANSWER. Its output went to the writers the
// driver was given and the kernel kept no copy; an empty stream would claim the Worker printed
// nothing, which is a different fact.
func TestLogsRefusesWhenTheRuntimeKeptNothing(t *testing.T) {
	requireProcTable(t)
	d := &processDriver{out: io.Discard, err: io.Discard}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sleeper := procSpec{Dir: t.TempDir(), Argv: []string{"sleep", "600"}, Env: os.Environ()}
	h, err := d.start(ctx, workerSpec{Name: "nologs", Version: "0.0.4", Actor: sleeper, Handler: sleeper})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = d.stop(context.Background(), h, 0) }()

	rc, err := d.logs(context.Background(), h)
	if err == nil {
		_ = rc.Close()
		t.Fatal("logs returned a stream for a foreground Worker, which retains nothing")
	}
	if !errors.Is(err, errNoRetainedLogs) {
		t.Errorf("the refusal must be recognisable, got: %v", err)
	}
	if !strings.Contains(err.Error(), "--tmux") {
		t.Errorf("the refusal must say where a log WOULD be kept, got: %v", err)
	}
}

// A HALF HELD IN TMUX OUTLIVES ITS OWN DEATH, and the process table alone gets this wrong. `tmuxHold`
// keeps the window open after the command exits, with the exit status on screen, because "a window
// that disappears is the worst possible report of a crash-on-boot" — so the wrapping shell is still
// running and still carries the label. A `list` that stopped at the process table would report a
// Worker that crashed on boot as healthy, forever, which is the same lie as reading from memory.
func TestListForgetsAHalfThatDiedInsideItsTmuxWindow(t *testing.T) {
	requireProcTable(t)
	if !tmuxAvailable() {
		t.Skip("tmux is not installed")
	}
	d := &processDriver{out: io.Discard, err: io.Discard, tmux: true}
	name, version := "tmuxcrash", fixtureVersion("0.0.6")
	session := tmuxSession(name, version)
	t.Cleanup(func() { _ = exec.Command("tmux", "kill-session", "-t", session).Run() })
	if tmuxHasSession(session) {
		t.Fatalf("tmux session %s is already there; this test would adopt somebody else's", session)
	}

	h, err := d.start(context.Background(), workerSpec{
		Name:    name,
		Version: version,
		// The actor crashes on boot the way a missing dependency does; the handler keeps serving.
		Actor:   procSpec{Dir: t.TempDir(), Argv: []string{"sh", "-c", "exit 3"}, Env: os.Environ()},
		Handler: procSpec{Dir: t.TempDir(), Argv: []string{"sh", "-c", "sleep 600"}, Env: os.Environ()},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	got := waitForWorker(t, d, name, version,
		func(h workerHandle) bool { _, ok := h.half(partActor); return !ok },
		"the pair with its crashed actor dropped")
	if _, ok := got.half(partHandler); !ok {
		t.Errorf("the handler half was dropped along with the crashed actor: %v", got)
	}

	// THE WINDOW IS STILL THERE, which is the whole reason this is hard: the shell holding it is
	// alive and labelled. If it were gone, this test would pass against a driver with no pane check
	// at all.
	if !tmuxHasSession(session) {
		t.Fatal("the session closed itself, so nothing here tested the pane check")
	}
	half, _ := h.half(partActor)
	if syscall.Kill(atoi(t, half.Ref), 0) != nil {
		t.Fatalf("the actor pane's shell (pid %s) is gone, so nothing here tested the pane check", half.Ref)
	}

	// …and its output is still readable, which is what the window is being kept open FOR.
	rc, err := d.logs(context.Background(), h)
	if err != nil {
		t.Fatalf("logs on a Worker whose half crashed: %v", err)
	}
	body, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !strings.Contains(string(body), "[exited 3]") {
		t.Errorf("the crashed half's exit status is not in the log:\n%s", body)
	}
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatalf("not a pid: %q", s)
	}
	return n
}

// THE TMUX PATH IS THE SAME DRIVER, and this pins the two things that only hold if the label reaches
// the pane: `list` finds a Worker held in a session, and `logs` reads what it printed.
//
// It is also the only proof that `tmux new-session -e` carries the label at all — a pane inherits
// the tmux SERVER's environment, so a variable set on the client reaches it not at all (tmux.go).
func TestTmuxHeldWorkerIsListedAndItsScrollbackIsTheLog(t *testing.T) {
	requireProcTable(t)
	if !tmuxAvailable() {
		t.Skip("tmux is not installed")
	}
	d := &processDriver{out: io.Discard, err: io.Discard, tmux: true}
	name, version := "tmuxheld", fixtureVersion("0.0.5")
	session := tmuxSession(name, version)
	t.Cleanup(func() { _ = exec.Command("tmux", "kill-session", "-t", session).Run() })
	if tmuxHasSession(session) {
		t.Fatalf("tmux session %s is already there; this test would adopt somebody else's", session)
	}

	marker := "slice01-" + strconv.Itoa(os.Getpid())
	say := func(who string) procSpec {
		return procSpec{Dir: t.TempDir(), Argv: []string{"sh", "-c", "echo " + marker + "-" + who + "; sleep 600"}, Env: os.Environ()}
	}
	h, err := d.start(context.Background(), workerSpec{Name: name, Version: version, Actor: say("actor"), Handler: say("handler")})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if len(h.Halves) != 2 {
		t.Errorf("start read back %d pane pids, want 2: %v", len(h.Halves), h.Halves)
	}

	waitForWorker(t, d, name, version, workerHandle.whole, "a pair held in a tmux session")

	var body string
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		rc, err := d.logs(context.Background(), h)
		if err != nil {
			t.Fatalf("logs: %v", err)
		}
		b, _ := io.ReadAll(rc)
		_ = rc.Close()
		body = string(b)
		if strings.Contains(body, marker+"-actor") && strings.Contains(body, marker+"-handler") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, want := range []string{"== actor ==", marker + "-actor", "== handler ==", marker + "-handler"} {
		if !strings.Contains(body, want) {
			t.Errorf("the log is missing %q:\n%s", want, body)
		}
	}

	// AND STOP KILLS THE SESSION, not the pane's shell: `tmuxHold` runs the command under
	// `sh -c`, so signalling the pid the handle carries would leave the command running with
	// nothing watching it.
	if err := d.stop(context.Background(), h, 2*time.Second); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if tmuxHasSession(session) {
		t.Errorf("tmux session %s survived stop", session)
	}
	waitForNoWorker(t, d, name)
}

// fixtureVersion makes a fixture's identity unique to THIS test process.
//
// BOTH DRIVERS REPORT THE WHOLE BOX, and that is deliberate: `list()` reads the runtime — /proc for
// `process`, `podman ps` for podman — because driver.go's third rule is that what is running is
// never read from a file. A fresh Warden on a rebooted Machine has to see the Workers its
// predecessor started, and it can only do that if nothing about the name is private to one process.
//
// The cost lands here. Two concurrent `cli.test` runs used to start the same `<name>/<version>` pair
// and see each other: podman refused with `pod already exists`, and the process driver did something
// worse — it matched, silently, and a test asserted against another run's Worker. Making the VERSION
// carry the pid gives each run its own pair while leaving the DERIVATION untouched, so nothing about
// production naming moves.
//
// The pid goes in the version rather than the name so `parseWorkerLabel` is unaffected: it parses
// from the right and treats the name as the free string, and a suffixed version is still one field.
func fixtureVersion(v string) string { return fmt.Sprintf("%s-p%d", v, os.Getpid()) }
