// warden_workflow_live_test.go — WHAT THE BLOCKED WATCHER ACTUALLY COSTS, measured against the
// Temporal on this box.
//
// ═══ WHY THIS CANNOT BE A UNIT TEST ═══
//
// Every claim in this slice is a claim about what TEMPORAL WRITES: nothing at rest, single digits per
// real change, no timer anywhere. `testsuite.TestWorkflowEnvironment` does not produce a history —
// it produces a simulation of one, and a simulation is not evidence about the thing whose event
// economy is the whole design. This repo has already found four guards that passed while measuring
// nothing, so the numbers in warden_workflow.go's header come from `GetWorkflowHistory` on a real
// server and the tests below print them as they assert them.
//
// SKIPPED, NOT FAILED, WHERE THERE IS NO SERVER. CI without a Temporal skips these and every other
// test in this package still runs; the skip message names the address it tried, so a skip is never
// mistaken for a pass.
//
// ═══ WHAT IS ISOLATED, AND HOW ═══
//
// Each test gets its own Warden id, and therefore its own workflow id, its own Machine queue and its
// own PLANE queue. The last one matters: `wardenPlaneQueue` is where a real Fleet's watchers run, and
// a test that polled it would execute — and terminate — some other agent's Machines. Nothing here
// touches a queue whose name it did not just mint.
package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/worker"
)

// --- reaching the server ---------------------------------------------------------------------------

func requireLiveTemporal(t *testing.T) client.Client {
	t.Helper()
	addr := temporalAddress()
	conn, err := net.DialTimeout("tcp", addr, 750*time.Millisecond)
	if err != nil {
		t.Skipf("no Temporal at %s, so this slice's event counts cannot be measured here: %v", addr, err)
	}
	_ = conn.Close()
	c, err := client.Dial(client.Options{HostPort: addr, Namespace: temporalNamespace()})
	if err != nil {
		t.Skipf("Temporal at %s answered a dial but not a client handshake: %v", addr, err)
	}
	t.Cleanup(c.Close)
	return c
}

// --- one Machine, watched, for the length of one test ------------------------------------------------

// liveWatch is a whole Warden protocol in one process: a control plane executing the watcher, a
// Machine serving the watch, and the watchpoint the test drives instead of a reconcile loop.
//
// THE WATCHPOINT IS THE REAL ONE. warden.go's loop calls `saw` once per turn with the two things it
// just read; these tests call the same method with the same shapes. What is not here is the process
// driver, because what is being measured is what the LEDGER costs, and the ledger cannot tell how the
// Machine learned a Worker was gone.
type liveWatch struct {
	c       client.Client
	watch   *wardenWatchpoint
	wid     string
	rid     string
	planeQ  string
	machQ   string
	plane   worker.Worker
	machine worker.Worker
	// mute makes this Machine hold the watch and stop heartbeating — see `startMachine`.
	mute atomic.Bool
}

func startLiveWatch(t *testing.T, in wardenWorkflowInput) *liveWatch {
	t.Helper()
	c := requireLiveTemporal(t)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	wardenID := "wdn-live" + suffix
	l := &liveWatch{
		c:      c,
		watch:  newWardenWatchpoint(wardenID, time.Now),
		wid:    wardenWorkflowID(wardenID),
		planeQ: "kontra-wardens-test-" + suffix,
		machQ:  wardenMachineQueue(wardenID),
	}

	l.plane = wardenPlaneWorkerOn(c, l.planeQ)
	if err := l.plane.Start(); err != nil {
		t.Fatalf("cannot start the control plane's worker: %v", err)
	}
	t.Cleanup(l.plane.Stop)
	l.startMachine(t)

	in.WardenID, in.Queue = wardenID, l.machQ
	if in.Hostname == "" {
		in.Hostname = "a test"
	}
	run, err := c.ExecuteWorkflow(context.Background(), client.StartWorkflowOptions{
		ID:                       l.wid,
		TaskQueue:                l.planeQ,
		WorkflowIDConflictPolicy: enumspb.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING,
	}, wardenWorkflowType, in)
	if err != nil {
		t.Fatalf("cannot start the watcher: %v", err)
	}
	l.rid = run.GetRunID()
	t.Cleanup(func() {
		// TERMINATED, NOT LEFT. A watcher blocks forever by design, so a test that merely stopped its
		// workers would leave a workflow on this box arming watches on a queue nobody polls — the
		// exact leak `kontra warden ca retire` exists for, produced by the test suite.
		_ = c.TerminateWorkflow(context.Background(), l.wid, l.rid, "test over")
	})
	return l
}

// startMachine brings this Machine's Temporal Worker up. Separate from `startLiveWatch` because the
// unreachability test has to take it away and put it back.
//
// THE ACTIVITY IS WRAPPED SO A MACHINE CAN DIE WITHOUT SAYING SO. `worker.Stop()` is a GRACEFUL stop
// — it cancels the watch, and the watch reports that it is going away — which is a **Warden**
// restarting and not a Machine dying. The other failure is a Machine that stops answering while
// holding the watch: power off, network gone, kernel panic. Nothing a worker API offers reproduces
// that, so `mute` does: the activity holds the task and never heartbeats, which is precisely what a
// dead Machine looks like from the server. Without it the heartbeat timeout is a branch nothing in
// this suite ever reaches — found by mutation, against a version whose retry policy would have
// swallowed it entirely.
func (l *liveWatch) startMachine(t *testing.T) {
	t.Helper()
	// THE SAME OPTIONS `wardenAttach` USES, and `WorkerStopTimeout` in particular: a test whose worker
	// stopped differently from production's would measure a shutdown this Fleet never performs.
	wk := worker.New(l.c, l.machQ, worker.Options{
		MaxConcurrentActivityExecutionSize: 2,
		DisableWorkflowWorker:              true,
		WorkerStopTimeout:                  wardenWatchStopGrace,
	})
	wk.RegisterActivityWithOptions(func(ctx context.Context) (wardenDecision, error) {
		if l.mute.Load() {
			<-ctx.Done()
			return wardenDecision{}, ctx.Err()
		}
		return l.watch.watch(ctx)
	}, activity.RegisterOptions{Name: wardenWatchActivityName})
	if err := wk.Start(); err != nil {
		t.Fatalf("cannot start this Machine's worker: %v", err)
	}
	l.machine = wk
	t.Cleanup(wk.Stop)
}

// events is every history event's type, in order.
func (l *liveWatch) events(t *testing.T) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	it := l.c.GetWorkflowHistory(ctx, l.wid, l.rid, false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	var out []string
	for it.HasNext() {
		ev, err := it.Next()
		if err != nil {
			t.Fatalf("reading %s's history: %v", l.wid, err)
		}
		out = append(out, strings.TrimPrefix(ev.GetEventType().String(), "EVENT_TYPE_"))
	}
	return out
}

// summaries is every Temporal user-metadata Summary in this history, in order — which is exactly what
// `backend/src/transcript.ts` reads and what an operator sees as a row's label.
func (l *liveWatch) summaries(t *testing.T) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	it := l.c.GetWorkflowHistory(ctx, l.wid, l.rid, false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	dc := converter.GetDefaultDataConverter()
	var out []string
	for it.HasNext() {
		ev, err := it.Next()
		if err != nil {
			t.Fatalf("reading %s's history: %v", l.wid, err)
		}
		meta := ev.GetUserMetadata()
		if meta == nil || meta.GetSummary() == nil {
			continue
		}
		var s string
		if err := dc.FromPayload(meta.GetSummary(), &s); err != nil {
			t.Fatalf("a Summary on event %d is not readable: %v", ev.GetEventId(), err)
		}
		out = append(out, s)
	}
	return out
}

// settle waits until the history has stopped growing, and returns it.
//
// QUIET IS THE SIGNAL, not a fixed sleep. A workflow that has finished reacting produces no further
// event, so "the same length three polls running" is the observation; a sleep long enough to be safe
// would make every measurement below cost seconds it did not need.
func (l *liveWatch) settle(t *testing.T) []string {
	t.Helper()
	deadline := time.Now().Add(25 * time.Second)
	var last []string
	same := 0
	for time.Now().Before(deadline) {
		now := l.events(t)
		if len(now) == len(last) {
			if same++; same >= 3 {
				return now
			}
		} else {
			same = 0
		}
		last = now
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("%s's history never stopped growing (%d events)", l.wid, len(last))
	return nil
}

// grow waits for the history to get longer than `from` and then settle, returning what was added.
func (l *liveWatch) grow(t *testing.T, from []string, what string) []string {
	t.Helper()
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		if len(l.events(t)) > len(from) {
			after := l.settle(t)
			return after[len(from):]
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("nothing was recorded for %s: the history is still %d events", what, len(from))
	return nil
}

// --- the two numbers this slice is judged on ---------------------------------------------------------

// NOTHING AT REST. The Machine's reconcile loop turns four times a second for thirty seconds and its
// watch heartbeats every second, and the workflow — blocked in `Selector.Select` the whole time —
// writes not one event.
//
// THE CONTROLS ARE BOTH DIRECTIONS. Before the quiet window, a real change is made and the history is
// shown to GROW, so "it did not grow" afterwards is a statement about the design rather than about a
// watcher that was never running. After it, another change is made and it grows again, so the thirty
// seconds are shown to have been a live watcher going quiet and not a dead one.
func TestAWatcherAtRestWritesNothing(t *testing.T) {
	const window = 30 * time.Second
	l := startLiveWatch(t, wardenWorkflowInput{
		// A three-second heartbeat means the watch beats every second (`watch` uses a third of the
		// timeout), so the quiet window below covers ~30 heartbeats. Production beats every ten.
		Heartbeat:      3 * time.Second,
		UnreachableMin: 5 * time.Minute,
		UnreachableMax: 5 * time.Minute,
	})
	desired, actual := specsFor("nscheck@0.1.0"), wholeFor("nscheck@0.1.0")

	opening := l.settle(t)
	// CONTROL 1: a real change is recorded, so the watcher is demonstrably alive and connected.
	l.watch.saw(desired, actual)
	before := append(opening, l.grow(t, opening, "the opening assignment")...)

	turns := 0
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(250 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				// The SAME two facts every turn, which is what a Machine on which nothing is
				// happening reports for as long as nothing happens.
				l.watch.saw(desired, actual)
				turns++
			}
		}
	}()
	time.Sleep(window)
	close(stop)
	<-done

	after := l.events(t)
	perHour := float64(len(after)-len(before)) * float64(time.Hour) / float64(window)
	t.Logf("AT REST: %d events over %s — %d reconcile turns, ~%d heartbeats — %.0f events/hour",
		len(after)-len(before), window, turns, int(window/time.Second), perHour)
	if len(after) != len(before) {
		t.Errorf("a blocked watcher wrote %d events in %s while nothing happened: %v\n"+
			"  ADR 0037's whole argument is that this number is zero. A watcher that TICKED at these "+
			"%d turns would be ~%d events at the ~20 events per tick this repo measured.",
			len(after)-len(before), window, after[len(before):], turns, turns*20)
	}

	// CONTROL 2: and it is still watching — the quiet was the design, not a dead worker.
	l.watch.saw(desired, nil)
	if added := l.grow(t, after, "a Worker exiting after the quiet window"); len(added) == 0 {
		t.Error("the watcher recorded nothing after the quiet window; the window proved nothing")
	}
}

// A REAL CHANGE COSTS SINGLE DIGITS, and the same single digits every time. The shape is printed
// beside the count because the composition is the argument: an activity closing, one workflow task to
// read it, and the next watch armed. There is nothing else in the loop, so there is nothing else that
// can be in the history.
func TestEachRealChangeCostsSingleDigits(t *testing.T) {
	l := startLiveWatch(t, wardenWorkflowInput{
		Heartbeat:      10 * time.Second,
		UnreachableMin: 5 * time.Minute,
		UnreachableMax: 5 * time.Minute,
	})
	desired := specsFor("nscheck@0.1.0")
	at := l.settle(t)
	t.Logf("OPENING: %d events — %v", len(at), at)

	for _, step := range []struct {
		what string
		do   func()
	}{
		{"assignment changed", func() { l.watch.saw(desired, nil) }},
		{"worker started", func() { l.watch.saw(desired, wholeFor("nscheck@0.1.0")) }},
		{"worker exited", func() { l.watch.saw(desired, nil) }},
	} {
		step.do()
		added := l.grow(t, at, step.what)
		t.Logf("PER CHANGE: %-20s %d events — %v", step.what, len(added), added)
		if len(added) > 9 {
			t.Errorf("%q cost %d events, and the bar is single digits: %v", step.what, len(added), added)
		}
		if len(added) < 4 {
			t.Errorf("%q cost only %d events (%v) — an activity closing and a workflow task to read "+
				"it cannot be fewer than four, so something was not actually recorded",
				step.what, len(added), added)
		}
		at = append(at, added...)
	}
}

// THE ONE DECISION A MACHINE CANNOT REPORT ABOUT ITSELF. Its worker stops polling; nothing on its
// queue takes the next watch; the CONTROL PLANE's worker — which is why the watcher does not run on
// the Machine — records it, and the wait before the next attempt doubles.
func TestAMachineThatStopsAnsweringIsRecorded(t *testing.T) {
	l := startLiveWatch(t, wardenWorkflowInput{
		Heartbeat:      10 * time.Second,
		UnreachableMin: 2 * time.Second,
		UnreachableMax: 8 * time.Second,
	})
	at := l.settle(t)

	// CONTROL: the Machine is answering right now, so the silence below is one this test caused.
	l.watch.saw(specsFor("nscheck@0.1.0"), nil)
	at = append(at, l.grow(t, at, "the opening assignment")...)

	// A WARDEN STOPPING IS THE FIRST HALF, and it is the half that used to end the watcher outright:
	// `worker.Stop()` cancels the in-flight watch, and a watch that returned `context canceled`
	// reached `wardenWorkflow`'s "an error nobody designed for" branch and failed the workflow. That
	// is what `systemctl restart kontra-warden` does. See `wardenWatchpoint.watch`.
	l.machine.Stop()
	added := l.grow(t, at, "the Machine's Warden stopping")
	t.Logf("WARDEN STOPPED: %d events — %v", len(added), added)
	if containsEvent(added, "WorkflowExecutionFailed") {
		t.Fatalf("stopping a Warden ENDED its watcher: %v — a restart is the most ordinary event in "+
			"a Fleet's life and it must not take the thing watching the Machine with it", added)
	}
	if got := lastOf(l.summaries(t)); !strings.Contains(got, string(decisionUnreachable)) {
		t.Errorf("a Warden that stopped watching reads as %q, want a %q", got, decisionUnreachable)
	}
	at = append(at, added...)

	// AND NOBODY TAKING THE NEXT WATCH IS THE SECOND. This is the one a Machine cannot report at all
	// — the power went off, the network went away — and it is Temporal's own schedule-to-start
	// timeout, recorded by the CONTROL PLANE's worker rather than by the Machine's.
	added = l.grow(t, at, "the Machine going quiet")
	t.Logf("UNREACHABLE: %d events — %v", len(added), added)
	if len(added) > 9 {
		t.Errorf("an unreachable Machine cost %d events: %v", len(added), added)
	}
	if !containsEvent(added, "ActivityTaskTimedOut") {
		t.Errorf("a Machine that took no watch produced %v, with no ActivityTaskTimedOut in it — "+
			"nothing else can tell a Machine that is gone from one with nothing to say", added)
	}
	sums := l.summaries(t)
	if len(sums) == 0 || !strings.Contains(sums[len(sums)-1], string(decisionUnreachable)) {
		t.Errorf("the last thing the Transcript would show is %q, and it does not say %q",
			lastOf(sums), decisionUnreachable)
	}

	// …AND IT RECOVERS. A Machine that comes back is watched again with nothing to reset, because
	// the watch is re-armed by the loop rather than by anything remembering it.
	at = append(at, added...)
	l.startMachine(t)
	l.watch.saw(specsFor("nscheck@0.1.0"), wholeFor("nscheck@0.1.0"))
	back := l.grow(t, at, "the Machine coming back")
	t.Logf("RECOVERY: %d events — %v", len(back), back)
	sums = l.summaries(t)
	if !strings.Contains(lastOf(sums), string(decisionWorkerStarted)) {
		t.Errorf("after the Machine came back the Transcript shows %q, want a %q",
			lastOf(sums), decisionWorkerStarted)
	}
}

// A MACHINE THAT DIES HOLDING THE WATCH — the other silence, and the one a graceful stop can never
// produce. The Machine keeps the activity and stops heartbeating; nothing on its side ever reports
// anything again, and it is Temporal's heartbeat timeout on the CONTROL PLANE that says so.
//
// THIS IS THE BRANCH A RETRY POLICY WOULD SWALLOW. A heartbeat timeout is retryable, so a watch with
// Temporal's default retry would be re-enqueued silently — no history event, no decision, and a
// Machine that is off the air reads as one with nothing to say. `MaximumAttempts: 1` is what makes it
// surface, and until this test existed nothing in the suite would have noticed it going away.
func TestAMachineThatDiesHoldingTheWatchIsRecorded(t *testing.T) {
	l := startLiveWatch(t, wardenWorkflowInput{
		Heartbeat:      3 * time.Second,
		UnreachableMin: 5 * time.Minute,
		UnreachableMax: 5 * time.Minute,
	})
	at := l.settle(t)

	// CONTROL: the Machine is heartbeating and reporting right now.
	l.watch.saw(specsFor("nscheck@0.1.0"), nil)
	at = append(at, l.grow(t, at, "the opening assignment")...)

	// Mute first, then let the LIVE watch finish: the mute takes effect on the next watch this
	// Machine picks up, which is the one it will then hold and never heartbeat on.
	l.mute.Store(true)
	l.watch.saw(specsFor("nscheck@0.1.0"), wholeFor("nscheck@0.1.0"))
	at = append(at, l.grow(t, at, "the last thing this Machine said")...)

	added := l.grow(t, at, "the Machine going silent while holding the watch")
	t.Logf("DIED HOLDING THE WATCH: %d events — %v", len(added), added)
	if !containsEvent(added, "ActivityTaskTimedOut") {
		t.Fatalf("a Machine that stopped heartbeating produced %v, with no ActivityTaskTimedOut in "+
			"it — that timeout is the only thing that can tell a dead Machine from a quiet one", added)
	}
	if len(added) > 9 {
		t.Errorf("a Machine dying cost %d events: %v", len(added), added)
	}
	if got := lastOf(l.summaries(t)); !strings.Contains(got, "stopped heartbeating") {
		t.Errorf("the Transcript says %q; it does not distinguish a Machine that took the watch and "+
			"died from one that never took it, and those send an operator to two different places", got)
	}
}

// NO TIMER ANYWHERE, ASSERTED AGAINST WHAT RAN. The source could be read for `workflow.Sleep`, and a
// reading of the source is not what this slice is judged on: `AwaitWithTimeout`, a Selector with a
// timer future and `workflow.Sleep` all produce the same two events, and this test fails on any of
// them however they were spelled.
func TestTheWatcherStartsNoTimer(t *testing.T) {
	l := startLiveWatch(t, wardenWorkflowInput{
		Heartbeat:      3 * time.Second,
		UnreachableMin: 2 * time.Second,
		UnreachableMax: 4 * time.Second,
	})
	at := l.settle(t)
	l.watch.saw(specsFor("nscheck@0.1.0"), nil)
	at = append(at, l.grow(t, at, "the opening assignment")...)
	// Drive the one path that would most plausibly want a timer — waiting before looking at an
	// unreachable Machine again — and let it happen twice. The first growth is the Warden's own stop
	// (see `wardenWatchpoint.watch`); the two after it are the misses.
	l.machine.Stop()
	at = append(at, l.grow(t, at, "the Warden stopping")...)
	at = append(at, l.grow(t, at, "the first miss")...)
	at = append(at, l.grow(t, at, "the second miss")...)

	for _, e := range at {
		if strings.Contains(e, "Timer") {
			t.Errorf("the watcher started a timer (%s) — the backoff on an unreachable Machine is the "+
				"next watch's ScheduleToStartTimeout, which costs nothing until it fires: %v", e, at)
			break
		}
	}
	t.Logf("NO TIMER: %d events across two misses, none of them a timer — %v", len(at), at)
}

// A MACHINE'S LIFECYCLE READS AS SENTENCES, and this is the half of "visible in the Transcript" that
// can be proved from Go. Each decision arrives as a Temporal user-metadata Summary — the same channel
// an author's `speak` line rides — so `backend/src/transcript.ts` folds it onto a turn and
// `backend/src/vocabulary.ts` names it, with no component knowing what a Warden is.
//
// The other half is over there: `vocabulary.test.ts` reads THIS PACKAGE'S BYTES for the five kind
// spellings, so a rename here fails a test in TypeScript rather than dropping a Fleet out of the
// account.
func TestAMachinesLifecycleReadsAsSentences(t *testing.T) {
	l := startLiveWatch(t, wardenWorkflowInput{
		Heartbeat:      10 * time.Second,
		UnreachableMin: 5 * time.Minute,
		UnreachableMax: 5 * time.Minute,
	})
	desired := specsFor("nscheck@0.1.0")
	at := l.settle(t)
	for _, do := range []func(){
		func() { l.watch.saw(desired, nil) },
		func() { l.watch.saw(desired, wholeFor("nscheck@0.1.0")) },
		func() { l.watch.saw(desired, nil) },
	} {
		do()
		at = append(at, l.grow(t, at, "a decision")...)
	}

	got := l.summaries(t)
	t.Logf("TRANSCRIPT:\n  %s", strings.Join(got, "\n  "))
	want := []string{
		string(decisionWatching),
		string(decisionAssignment),
		string(decisionWorkerStarted),
		string(decisionWorkerExited),
	}
	if len(got) != len(want) {
		t.Fatalf("the Transcript would show %d rows, want %d: %v", len(got), len(want), got)
	}
	for i, w := range want {
		if !strings.HasPrefix(got[i], wardenSummaryPrefix+wardenSummarySep+w) {
			t.Errorf("row %d reads %q, want it to open %q", i, got[i], wardenSummaryPrefix+wardenSummarySep+w)
		}
	}
}

// A WARDEN ATTACHING TO ITS CONTROL PLANE STILL OPENS NO LISTENER. `TestWardenOpensNoListeningSocket`
// makes this claim about the reconcile loop; a Temporal Worker is a LONG POLL OUTBOUND, and this is
// the assertion that says so rather than assuming it. ADR 0037: "Every arrow is dialled by the
// Machine. No inbound ports; NAT and private networks work unchanged."
func TestAttachingToTheControlPlaneOpensNoListener(t *testing.T) {
	if _, err := os.Stat("/proc/net/tcp"); err != nil {
		t.Skipf("no /proc/net/tcp here, so nothing can be observed about listeners: %v", err)
	}
	c := requireLiveTemporal(t)
	_ = c

	// CONTROL: the detector sees a listener this process really has, so the claim below is not
	// vacuous. Same two-way control as warden_test.go's version.
	base := len(ownListeners(t))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("cannot open a control listener: %v", err)
	}
	if n := len(ownListeners(t)); n <= base {
		_ = ln.Close()
		t.Fatalf("the detector did not see a listener this process just opened (%d then %d)", base, n)
	}
	_ = ln.Close()

	wardenID := fmt.Sprintf("wdn-attach%d", time.Now().UnixNano())
	// THE SCOPE IS SET AS WELL AS THE RECORD, because slice 08 made the certificate the source of a
	// Machine's namespace and `temporalOptions` reads it from there and nowhere else. This identity is
	// hand-built rather than enrolled — there is no CA in this test — so the field `loadIdentity` would
	// have filled in from a certificate is filled in here. It is set to the SAME namespace the record
	// names, which is the only pair `loadIdentity` would ever produce.
	id := &wardenIdentity{
		Record: wardenRecord{
			WardenID:  wardenID,
			Temporal:  temporalAddress(),
			Namespace: temporalNamespace(),
		},
		scope: wardenScope{Namespace: temporalNamespace(), WardenID: wardenID},
	}
	watch := newWardenWatchpoint(wardenID, time.Now)
	plane, err := wardenAttach(context.Background(), id, "process", watch, nil)
	if err != nil {
		t.Fatalf("attaching: %v", err)
	}
	t.Cleanup(func() {
		// The workflow is started on the REAL plane queue by `wardenAttach`, which is the point of
		// this test — so it is terminated rather than left for a Controller to pick up later.
		_ = plane.client.TerminateWorkflow(context.Background(), plane.WorkflowID, "", "test over")
		plane.close()
	})

	worst, deadline := base, time.Now().Add(2*time.Second)
	for time.Now().Before(deadline) {
		if n := len(ownListeners(t)); n > worst {
			worst = n
		}
		time.Sleep(20 * time.Millisecond)
	}
	if worst > base {
		t.Errorf("attaching to the control plane opened %d listening socket(s); a Machine accepts no "+
			"inbound connection (ADR 0037)", worst-base)
	}
	if plane.WorkflowID != wardenWorkflowID(wardenID) {
		t.Errorf("attached as %q, want %q — the id is derived from the Machine's key so that a Warden "+
			"restarting re-attaches to its own story", plane.WorkflowID, wardenWorkflowID(wardenID))
	}
}

func containsEvent(events []string, want string) bool {
	for _, e := range events {
		if e == want {
			return true
		}
	}
	return false
}

func lastOf(ss []string) string {
	if len(ss) == 0 {
		return ""
	}
	return ss[len(ss)-1]
}
