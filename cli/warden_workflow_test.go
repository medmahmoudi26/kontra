// warden_workflow_test.go — the diff that makes a blocked watcher possible, and the two properties
// of the workflow that do not need a server.
//
// THE COST OF THIS SLICE IS MEASURED SOMEWHERE ELSE, AND ON PURPOSE. Every number in
// warden_workflow.go's header — nothing at rest, single digits per change — is a claim about what
// TEMPORAL WRITES, and a claim about what Temporal writes cannot be tested against a fake that
// decides what Temporal writes. warden_workflow_live_test.go dials the server on this box, drives
// each of the four decisions, and reads the history back. What is here is everything that is true of
// the code without a server: which observations become a decision, which deliberately do not, and
// what a decision looks like once it is a sentence.
//
// THE CONTROL COMES FIRST, as everywhere in this package: a test that asserts "no decision was
// produced" passes identically against a watchpoint that can never produce one, so every such test
// first produces one.
package main

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
)

// --- fixtures ------------------------------------------------------------------------------------

// specsFor builds a desired list of the given ids. Only the identity matters to the watchpoint,
// which is the point: it diffs what was ASKED FOR against what is THERE and reads nothing else out
// of a spec.
func specsFor(ids ...string) []workerSpec {
	out := make([]workerSpec, 0, len(ids))
	for _, id := range ids {
		name, version, _ := strings.Cut(id, "@")
		out = append(out, workerSpec{Name: name, Version: version})
	}
	return out
}

// wholeFor builds the runtime's answer: every id present with both halves.
func wholeFor(ids ...string) []workerHandle {
	out := make([]workerHandle, 0, len(ids))
	for _, id := range ids {
		name, version, _ := strings.Cut(id, "@")
		out = append(out, workerHandle{
			Driver: "process", Name: name, Version: version,
			Halves: []workerHalf{{Part: partActor, Ref: "1"}, {Part: partHandler, Ref: "2"}},
		})
	}
	return out
}

// halfFor is one Worker with only the named half — the state serve.go calls "worse than a dead one".
func halfFor(id string, part workerPart) workerHandle {
	name, version, _ := strings.Cut(id, "@")
	return workerHandle{
		Driver: "process", Name: name, Version: version,
		Halves: []workerHalf{{Part: part, Ref: "1"}},
	}
}

// drain takes every decision that is queued right now, without blocking. `next` is the blocking
// read the activity uses; this is what a test uses to ask "and what else?".
func drain(p *wardenWatchpoint) []wardenDecision {
	var out []wardenDecision
	for {
		select {
		case d := <-p.decisions:
			out = append(out, d)
		default:
			return out
		}
	}
}

func kindsOf(ds []wardenDecision) []string {
	out := make([]string, 0, len(ds))
	for _, d := range ds {
		out = append(out, string(d.Kind)+"/"+d.Subject)
	}
	return out
}

func testWatchpoint(t *testing.T) *wardenWatchpoint {
	t.Helper()
	return newWardenWatchpoint("wdn-testtesttest0", time.Now)
}

// --- what a quiet Machine costs -------------------------------------------------------------------

// NOTHING AT REST IS THE WHOLE SLICE, and this is the version of it that needs no server. A reconcile
// loop that turns every five seconds forever must produce ZERO decisions while nothing changes,
// because every decision downstream of here is six Temporal events; a watchpoint that emitted one
// "still fine" per turn would be the ~14,400 events/hour ADR 0037 refuses, arriving by a different
// road.
func TestAQuietMachineDecidesNothing(t *testing.T) {
	p := testWatchpoint(t)
	desired, actual := specsFor("nscheck@0.1.0", "subfinder@0.2.0"), wholeFor("nscheck@0.1.0", "subfinder@0.2.0")

	// CONTROL: the first turn DOES decide something, so "nothing was produced" below is a statement
	// about the diff and not about a watchpoint that cannot speak.
	p.saw(desired, actual)
	if got := drain(p); len(got) != 1 {
		t.Fatalf("the first turn produced %d decisions, want exactly 1: %v", len(got), kindsOf(got))
	}

	for i := 0; i < 200; i++ {
		p.saw(desired, actual)
	}
	if got := drain(p); len(got) != 0 {
		t.Errorf("200 identical turns produced %d decisions, want 0: %v", len(got), kindsOf(got))
	}

	// …and `next` BLOCKS rather than returning a zero value, which is what a workflow waits on.
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if d, err := p.next(ctx); err == nil {
		t.Errorf("next returned %v on a quiet Machine; it must block until something happens", d)
	}
}

// A WARDEN THAT STARTS SAYS ONE THING, NOT ONE PER WORKER. `systemctl restart kontra-warden` is a
// routine act — the unit is `Restart=always` — and a Warden whose first turn reported every Worker
// it found as freshly started would make the quietest event in a Fleet's life the loudest thing in
// its history.
func TestAWardensFirstTurnIsOneDecision(t *testing.T) {
	p := testWatchpoint(t)
	ids := []string{"a@1", "b@1", "c@1", "d@1", "e@1"}
	p.saw(specsFor(ids...), wholeFor(ids...))

	got := drain(p)
	if len(got) != 1 {
		t.Fatalf("the first turn produced %d decisions, want 1: %v", len(got), kindsOf(got))
	}
	if got[0].Kind != decisionAssignment {
		t.Errorf("the first decision is %q, want %q", got[0].Kind, decisionAssignment)
	}
	for _, id := range ids {
		if !strings.Contains(got[0].Detail, id) {
			t.Errorf("the first decision does not name %s: %q", id, got[0].Detail)
		}
	}
}

// --- the four decisions ---------------------------------------------------------------------------

func TestAWorkerBecomingWholeIsAStart(t *testing.T) {
	p := testWatchpoint(t)
	desired := specsFor("nscheck@0.1.0")
	p.saw(desired, nil)
	drain(p)

	// CONTROL: half a pair is not a start. The Worker is present in the runtime and it is still not
	// running, which is exactly the state driver.go says must be representable.
	p.saw(desired, []workerHandle{halfFor("nscheck@0.1.0", partActor)})
	if got := drain(p); len(got) != 0 {
		t.Fatalf("half a pair was reported as %v; only a WHOLE pair is a Worker", kindsOf(got))
	}

	p.saw(desired, wholeFor("nscheck@0.1.0"))
	got := drain(p)
	if len(got) != 1 || got[0].Kind != decisionWorkerStarted || got[0].Subject != "nscheck@0.1.0" {
		t.Fatalf("a pair becoming whole produced %v, want one %q for nscheck@0.1.0",
			kindsOf(got), decisionWorkerStarted)
	}
}

func TestAWorkerThatLosesAHalfIsAnExit(t *testing.T) {
	p := testWatchpoint(t)
	desired := specsFor("nscheck@0.1.0")
	p.saw(desired, wholeFor("nscheck@0.1.0"))
	drain(p)

	p.saw(desired, []workerHandle{halfFor("nscheck@0.1.0", partHandler)})
	got := drain(p)
	if len(got) != 1 || got[0].Kind != decisionWorkerExited {
		t.Fatalf("losing the actor half produced %v, want one %q", kindsOf(got), decisionWorkerExited)
	}
	// WHICH HALF SURVIVED IS THE OPERATOR'S NEXT QUESTION, and it is the difference between a pair
	// that vanished and one still holding a Temporal lease with nothing behind it.
	if !strings.Contains(got[0].Detail, "handler") {
		t.Errorf("the exit does not say which half is left: %q", got[0].Detail)
	}

	p.saw(desired, nil)
	got = drain(p)
	if len(got) != 0 {
		t.Errorf("a Worker that was ALREADY not whole exited a second time: %v", kindsOf(got))
	}
}

// A SCALE-DOWN IS NOT A CRASH, and telling them apart is the reason the watchpoint is handed the
// desired state as well as the runtime's answer. `place()` is idempotent desired state (ADR 0037), so
// removing a Worker is an ordinary operation that a Fleet does whenever a Run finishes with it; a
// watcher that recorded every one of them as `worker exited` would make the wrong tone the common
// case and teach an operator to ignore the row.
func TestAWorkerDroppedFromTheAssignmentIsNotAnExit(t *testing.T) {
	p := testWatchpoint(t)
	p.saw(specsFor("nscheck@0.1.0", "subfinder@0.2.0"), wholeFor("nscheck@0.1.0", "subfinder@0.2.0"))
	drain(p)

	// The control plane asked for one fewer, and the Warden stopped it.
	p.saw(specsFor("nscheck@0.1.0"), wholeFor("nscheck@0.1.0"))
	got := drain(p)
	if len(got) != 1 {
		t.Fatalf("dropping a Worker from the assignment produced %d decisions, want 1: %v",
			len(got), kindsOf(got))
	}
	if got[0].Kind != decisionAssignment {
		t.Fatalf("dropping a Worker read as %q, want %q", got[0].Kind, decisionAssignment)
	}
	if !strings.Contains(got[0].Detail, "-subfinder@0.2.0") {
		t.Errorf("the assignment change does not name what left: %q", got[0].Detail)
	}

	// CONTROL: the same Worker disappearing while it is STILL WANTED is an exit, so the rule above
	// is a distinction and not a blanket silence.
	p.saw(specsFor("nscheck@0.1.0"), nil)
	got = drain(p)
	if len(got) != 1 || got[0].Kind != decisionWorkerExited {
		t.Errorf("a wanted Worker vanishing produced %v, want one %q", kindsOf(got), decisionWorkerExited)
	}
}

// EXITS ARE REPORTED BEFORE STARTS, mirroring `reconcile`'s own order (which stops before it starts,
// because a **Machine** is finite). A turn in which one Worker died and another came up must read in
// that order, or a Fleet's history says a Machine briefly ran both.
func TestAnExitIsReportedBeforeAStartInTheSameTurn(t *testing.T) {
	p := testWatchpoint(t)
	desired := specsFor("a@1", "b@1")
	p.saw(desired, wholeFor("a@1"))
	drain(p)

	p.saw(desired, wholeFor("b@1"))
	got := kindsOf(drain(p))
	want := []string{
		string(decisionWorkerExited) + "/a@1",
		string(decisionWorkerStarted) + "/b@1",
	}
	if len(got) != len(want) {
		t.Fatalf("one exit and one start in a turn produced %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("one exit and one start in a turn produced %v, want %v", got, want)
		}
	}
}

// A VERSION ROLLED FORWARD IS ONE FACT, NOT THREE, and the first version of this file got it wrong in
// a way its own sibling test caught: `nscheck@0.1.0` → `nscheck@0.2.0` looked like an assignment
// change, an exit and a start. The exit is the wrong word — the control plane asked for that Worker
// to go, `assignment changed` already names both sides of the swap, and `worker exited` carries a
// `wrong` tone that would make every routine roll in a Fleet render as a crash. The rule that makes
// this right is the same one that makes a scale-down quiet: a Worker that is gone AND no longer
// wanted did not exit, it was retired.
func TestARolledVersionIsNotAnExit(t *testing.T) {
	p := testWatchpoint(t)
	p.saw(specsFor("nscheck@0.1.0"), wholeFor("nscheck@0.1.0"))
	drain(p)

	p.saw(specsFor("nscheck@0.2.0"), wholeFor("nscheck@0.2.0"))
	got := drain(p)
	if len(got) != 2 {
		t.Fatalf("a rolled version produced %v, want an assignment change and a start", kindsOf(got))
	}
	if got[0].Kind != decisionAssignment || got[1].Kind != decisionWorkerStarted {
		t.Fatalf("a rolled version produced %v, want %q then %q",
			kindsOf(got), decisionAssignment, decisionWorkerStarted)
	}
	if !strings.Contains(got[0].Detail, "+nscheck@0.2.0") || !strings.Contains(got[0].Detail, "-nscheck@0.1.0") {
		t.Errorf("the one line about the roll does not name both sides of it: %q", got[0].Detail)
	}
}

// --- what a Warden nobody is listening to does with its decisions ---------------------------------

// A GAP IN A MACHINE'S HISTORY IS VISIBLE IN THAT HISTORY. A Warden whose control plane is
// unreachable keeps reconciling (warden.go's first rule about the Controller) and its decisions have
// nowhere to go; the buffer is bounded, because an unbounded one is a Machine that runs out of memory
// rather than one that loses a line. What must not happen is that the loss is silent — an operator
// reading a Fleet's history has no other way to know the account is incomplete.
func TestDecisionsLostWhileNobodyWasListeningAreCountedIntoTheNextOne(t *testing.T) {
	p := testWatchpoint(t)
	p.saw(specsFor(), nil)

	// Overfill: each turn flips one Worker in and out, so every turn is a real change.
	for i := 0; i < wardenWatchpointBuffer*2; i++ {
		if i%2 == 0 {
			p.saw(specsFor("x@1"), wholeFor("x@1"))
		} else {
			p.saw(specsFor("x@1"), nil)
		}
	}
	got := drain(p)
	if len(got) != wardenWatchpointBuffer {
		t.Fatalf("the buffer held %d decisions, want its stated bound of %d",
			len(got), wardenWatchpointBuffer)
	}

	// The count arrives on the NEXT decision that gets through, which is the only place a reader of
	// the **Lease** workflow could ever see it.
	p.saw(specsFor("y@1"), wholeFor("y@1"))
	next := drain(p)
	if len(next) == 0 {
		t.Fatal("nothing came through after the overflow")
	}
	if !strings.Contains(next[0].Detail, "were dropped") {
		t.Errorf("the decision after an overflow does not mention the loss: %q", next[0].Detail)
	}
}

// --- the sentence that reaches the Transcript ------------------------------------------------------

// THE SUMMARY IS THE ONLY THING A SURFACE EVER SEES. `backend/src/vocabulary.ts` reads it back; the
// activity's RESULT is a payload and ADR 0007 is that payloads are never decoded. So the grammar has
// to survive the two things that break a wire format: a field that is too long, and a field that is
// empty.
func TestADecisionsSummaryKeepsItsGrammarWhateverTheDetailIs(t *testing.T) {
	long := wardenDecision{
		Kind:    decisionWorkerExited,
		Subject: "nscheck@0.1.0",
		Detail:  strings.Repeat("a Machine with a very long story to tell. ", 40),
	}
	s := long.summary()
	if len(s) > wardenSummaryMax {
		t.Errorf("a long detail produced %d bytes, over the %d-byte budget", len(s), wardenSummaryMax)
	}
	if !strings.HasSuffix(s, "…") {
		t.Errorf("a shortened summary does not carry the ellipsis that says it was shortened: %q", s)
	}
	// THE KIND AND THE SUBJECT SURVIVE TRUNCATION, because they are what a reader names the row
	// from. A budget that ate the kind would leave a row saying only that something happened.
	fields := strings.Split(s, wardenSummarySep)
	if len(fields) < 3 || fields[0] != wardenSummaryPrefix ||
		fields[1] != string(decisionWorkerExited) || fields[2] != "nscheck@0.1.0" {
		t.Errorf("a shortened summary lost its grammar: %q", s)
	}

	bare := wardenDecision{Kind: decisionWatching, Subject: "wdn-1"}
	if got, want := bare.summary(), wardenSummaryPrefix+" · watching · wdn-1"; got != want {
		t.Errorf("a detail-free decision reads %q, want %q", got, want)
	}
}

// --- how a Machine learns where its lifecycle is recorded --------------------------------------------

// THE CONTROL PLANE ARRIVES AT ENROLMENT, AND IS NOT CONFIGURED ON THE MACHINE. Every other Temporal
// address in this CLI comes from `KONTRA_ADDRESS`; a **Warden** runs under a systemd unit on a
// Machine nobody logs into, and warden.go already states what a second place to configure something
// costs — "a unit and an EnvironmentFile that disagree is a Warden that enrolled into one directory
// and serves from another". So the Controller says it once, in the answer to the only question a
// Machine ever asks about itself.
//
// EMPTY IS ASSERTED TOO, and it is the half that matters more: a Machine whose Controller has no
// control plane must record NOTHING rather than fall back to whatever `temporalAddress()` resolves to
// in the enrolling process — which on the single box is `127.0.0.1:7233` and on a real Fleet is that
// Machine's own loopback.
//
// SLICE 08 SPLIT THE ADDRESS FROM THE NAMESPACE, and this test was rewritten to say so. The ADDRESS
// is a property of the CONTROLLER (`--temporal`) and is empty on an airgapped one. The NAMESPACE is a
// property of the TENANT the enrolment token was minted for, so it exists whether or not there is a
// Temporal to dial — an airgapped Fleet still has tenants, and a certificate with no namespace in it
// is the unscoped credential this slice removes. What must stay empty on an airgapped Controller is
// the address, because an address nobody configured is worse than none: it resolves.
func TestEnrolmentCarriesTheControlPlaneOntoTheMachine(t *testing.T) {
	c := newController(t)
	c.ca.temporal = "10.124.0.2:7233"

	state := t.TempDir()
	id, err := enrol(context.Background(), state, c.srv.URL, c.tokenFor(t, "acme"))
	if err != nil {
		t.Fatalf("enrolling: %v", err)
	}
	if id.Record.Temporal != "10.124.0.2:7233" || id.Record.Namespace != "acme" {
		t.Fatalf("enrolment recorded temporal=%q namespace=%q, want the Controller's own answer",
			id.Record.Temporal, id.Record.Namespace)
	}
	// …AND THE NAMESPACE CAME OUT OF THE CERTIFICATE, not out of the JSON beside it. Without this the
	// assertion above passes against a Controller that answers `"namespace":"acme"` and signs an
	// unscoped certificate — which is the exact drift `enrol`'s cross-check exists to catch.
	if id.scope.Namespace != "acme" || id.scope.WardenID != id.Record.WardenID {
		t.Errorf("the credential's own scope is %s, and the record says %s/%s",
			id.scope, id.Record.WardenID, id.Record.Namespace)
	}

	// …AND IT PERSISTS, because `kontra warden serve` reads it off disk in a different process from
	// the one that enrolled.
	again, err := loadIdentity(state)
	if err != nil {
		t.Fatalf("re-reading the identity: %v", err)
	}
	if again.Record.Temporal != id.Record.Temporal || again.Record.Namespace != id.Record.Namespace {
		t.Errorf("the control plane did not survive a restart: %q/%q then %q/%q",
			id.Record.Temporal, id.Record.Namespace, again.Record.Temporal, again.Record.Namespace)
	}
	if got := wardenWorkflowID(again.Record.WardenID); got == wardenWorkflowIDPrefix {
		t.Error("the enrolled Machine has no id to name its watcher with")
	}

	// CONTROL: a Controller with no control plane produces a Machine that records nothing and says so.
	airgapped := newController(t)
	bare, err := enrol(context.Background(), t.TempDir(), airgapped.srv.URL, airgapped.tokenFor(t, "globex"))
	if err != nil {
		t.Fatalf("enrolling against a Controller with no control plane: %v", err)
	}
	if bare.Record.Temporal != "" {
		t.Errorf("a Controller with no control plane enrolled a Machine pointed at %q — an "+
			"address nobody configured is worse than none, because it resolves", bare.Record.Temporal)
	}
	// …and it is still SCOPED, because a tenant is not a property of the control plane. An airgapped
	// Machine holds a credential for exactly one namespace; there is simply nowhere to present it yet.
	if bare.scope.Namespace != "globex" {
		t.Errorf("an airgapped Machine's credential is scoped to %q, want globex — a certificate with "+
			"no tenant in it is the unscoped credential slice 08 removes", bare.scope.Namespace)
	}
	// …AND IT STILL REFUSES TO ATTACH, which is the property the empty address is FOR. Without this,
	// "Temporal is empty" is a field nobody read.
	if _, err := wardenAttach(context.Background(), bare, "process", newWardenWatchpoint(bare.Record.WardenID, nil), nil); err == nil {
		t.Error("an airgapped Machine attached to a control plane it was never given")
	} else if !errorsIs(err, errNoControlPlane) {
		t.Errorf("an airgapped Machine failed to attach for the wrong reason: %v", err)
	}
}

// --- the loop is actually wired to the watcher ------------------------------------------------------

// THE ONE LINE SLICE 04 ADDED TO `reconcile`, ASSERTED THROUGH THE REAL LOOP. Everything above drives
// the watchpoint directly, which is the right way to test a diff and the wrong way to find out
// whether anything calls it: deleting `w.watch.saw(desired, actual)` from warden.go leaves every
// other test in this file and every measurement in warden_workflow_live_test.go green, and a Fleet
// with no history at all. Found by mutation; this is the test that was missing.
//
// It drives the process driver against real processes, like warden_test.go, for the same reason —
// the observation being reported is `list()`'s answer, and a fake would report a map.
// THE WARDEN IS THE ONE `serve` BUILDS, not one this test assembled. A `wardenServe` that forgot the
// watchpoint would give a Fleet no history whatsoever and leave every other test here green, because
// every other test sets the field itself — the second half of the same mutation that found the
// missing call below. `newServeWarden` exists so that there is one seam holding both.
func TestTheReconcileLoopReportsItsTurnsToTheWatcher(t *testing.T) {
	w := testWarden(t, "wwatch")
	served := newServeWarden(&wardenIdentity{Record: wardenRecord{WardenID: "wdn-wired"}}, w.driver, w.interval)
	if served.watch == nil {
		t.Fatal("`kontra warden serve` builds a Warden with nothing watching it, so nothing this " +
			"Machine ever does would be recorded anywhere")
	}
	w.watch = served.watch
	desired := []workerSpec{spec(t, "wwatch-a", "1.0.0")}

	// Turn one: nothing is running yet, so the loop starts it. The watchpoint sees the ASSIGNMENT
	// this turn and the Worker on a later one, because a Worker started during a turn is not whole
	// until the runtime says it is.
	turn(t, w, desired, "the Worker", func(hs []workerHandle) bool {
		h, ok := handleFor(hs, "wwatch-a")
		return ok && h.whole()
	})
	got := drain(w.watch)
	if len(got) != 1 || got[0].Kind != decisionAssignment {
		t.Fatalf("the first reconcile turn told the watcher %v, want one %q",
			kindsOf(got), decisionAssignment)
	}

	// Turn two: the pair is up, and THIS is the turn the watcher learns it.
	w.reconcile(context.Background(), desired)
	got = drain(w.watch)
	if len(got) != 1 || got[0].Kind != decisionWorkerStarted || got[0].Subject != "wwatch-a@1.0.0" {
		t.Fatalf("the turn that found the Worker whole told the watcher %v, want one %q for "+
			"wwatch-a@1.0.0", kindsOf(got), decisionWorkerStarted)
	}

	// …and a turn on which nothing changed tells it nothing, which is the property the whole slice
	// rests on, asserted here through the loop rather than through the diff.
	w.reconcile(context.Background(), desired)
	w.reconcile(context.Background(), desired)
	if got = drain(w.watch); len(got) != 0 {
		t.Errorf("two quiet reconcile turns told the watcher %v, want nothing", kindsOf(got))
	}
}

// A WARDEN'S NAMES ARE DERIVED FROM ITS OWN ID, and they are written out here rather than composed
// from the functions under test — which is the whole content of the assertion. A test that said
// `wardenWorkflowID(id) == wardenWorkflowID(id)` passes against a version that returns one shared
// name for every Machine in the Fleet, and every Warden would then attach to one workflow and fight
// over it. Found by mutation, against exactly that.
func TestAWardensNamesAreDerivedFromItsOwnId(t *testing.T) {
	if got, want := wardenWorkflowID("wdn-abc123"), "kontra-warden/wdn-abc123"; got != want {
		t.Errorf("wardenWorkflowID = %q, want %q", got, want)
	}
	if got, want := wardenMachineQueue("wdn-abc123"), "warden-wdn-abc123"; got != want {
		t.Errorf("wardenMachineQueue = %q, want %q", got, want)
	}
	if wardenWorkflowID("wdn-a") == wardenWorkflowID("wdn-b") {
		t.Error("two Machines share one watcher; the id must be derived from the Machine's own key")
	}
	if wardenMachineQueue("wdn-a") == wardenMachineQueue("wdn-b") {
		t.Error("two Machines share one queue; each Machine polls only its own")
	}
	// AND THE WATCHER DOES NOT RUN ON THE MACHINE IT WATCHES. This is the whole reason "Machine
	// unreachable" is recordable: a workflow polled by the Machine it is about cannot report that
	// the Machine stopped polling.
	if wardenPlaneQueue == wardenMachineQueue("wdn-a") {
		t.Error("the watcher runs on the queue it is watching")
	}
}

// --- the workflow, without a server ----------------------------------------------------------------

// THE BACKOFF IS AN ACTIVITY OPTION AND NOT A SLEEP, and this is the arithmetic that makes that
// affordable. A dark Machine costs one observation per wait; at a fixed one-minute wait that is 360
// events an hour of a workflow repeating itself, which reaches Temporal's history limit in under a
// week. Doubling to the ceiling makes a Machine that is gone cost ~6 events/hour — and it costs
// nothing at all while it is not firing, which is what a `workflow.Sleep` could not say.
func TestAnUnreachableMachineIsAskedForLessOftenRatherThanSleptOn(t *testing.T) {
	in := wardenWorkflowInput{UnreachableMin: time.Minute, UnreachableMax: time.Hour}
	want := []time.Duration{
		time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute,
		16 * time.Minute, 32 * time.Minute, time.Hour, time.Hour, time.Hour,
	}
	for i, w := range want {
		if got := in.unreachableAfter(i); got != w {
			t.Errorf("miss %d waits %s, want %s", i, got, w)
		}
	}
	// A ceiling below the floor is an operator's typo, not a licence to wait forever.
	odd := wardenWorkflowInput{UnreachableMin: time.Minute, UnreachableMax: time.Second}
	if got := odd.unreachableAfter(9); got != time.Minute {
		t.Errorf("a ceiling under the floor waits %s, want the floor (%s)", got, time.Minute)
	}
	// And the defaults are what production gets, unstated by a caller.
	if got := (wardenWorkflowInput{}).unreachableAfter(0); got != wardenUnreachableMinDefault {
		t.Errorf("the default first wait is %s, want %s", got, wardenUnreachableMinDefault)
	}
	if got := (wardenWorkflowInput{}).unreachableAfter(40); got != wardenUnreachableMaxDefault {
		t.Errorf("the default ceiling is %s, want %s", got, wardenUnreachableMaxDefault)
	}
}

// A WATCH THAT FAILS IS SURVIVED, AND A WATCH THAT ONLY EVER FAILS IS NOT.
//
// Both halves are the same defect seen from two sides. `worker.Stop()` races its own in-flight watch,
// so an ordinary `systemctl stop kontra-warden` produces `ActivityTaskFailed` whenever the completion
// loses — measured on this box, from the same code that produced a clean completion a run earlier.
// A workflow that ended on that lost the thing watching that Machine for ever, on a coin flip.
//
// The old branch was guarding something real, though: a watch that fails INSTANTLY and for ever costs
// six events a round trip with nothing to throttle it, because `ScheduleToStartTimeout` only delays a
// watch nobody takes. So the recovery is unbounded in TIME and bounded in CONSECUTIVE FAILURES, and
// this test drives both sides of that line.
func TestAFailedWatchIsSurvivedButAWatchThatOnlyFailsIsNot(t *testing.T) {
	watchThatFails := func(n *int32) func(context.Context) (wardenDecision, error) {
		return func(context.Context) (wardenDecision, error) {
			atomic.AddInt32(n, 1)
			return wardenDecision{}, errors.New("the watch blew up")
		}
	}

	// ONE FAILURE IS ABSORBED: the watcher re-arms, and the next watch answers.
	t.Run("recovers", func(t *testing.T) {
		var suite testsuite.WorkflowTestSuite
		env := suite.NewTestWorkflowEnvironment()
		env.SetTestTimeout(30 * time.Second)
		var calls int32
		hold := make(chan struct{})
		t.Cleanup(func() { close(hold) })
		env.RegisterActivityWithOptions(func(ctx context.Context) (wardenDecision, error) {
			if atomic.AddInt32(&calls, 1) == 1 {
				return wardenDecision{}, errors.New("the watch blew up once")
			}
			<-hold
			return wardenDecision{}, nil
		}, activity.RegisterOptions{Name: wardenWatchActivityName})
		env.RegisterDelayedCallback(func() { env.SignalWorkflow(wardenRetireSignal, nil) }, 2*time.Second)

		env.ExecuteWorkflow(wardenWorkflow, wardenWorkflowInput{
			WardenID: "wdn-flaky0000000", Queue: "q",
			UnreachableMin: time.Hour, UnreachableMax: time.Hour, Heartbeat: time.Minute,
		})
		if err := env.GetWorkflowError(); err != nil {
			t.Fatalf("one failed watch ended the watcher with %v — a Warden restarting is the most "+
				"ordinary event in a Fleet's life and must not do that", err)
		}
		if n := atomic.LoadInt32(&calls); n < 2 {
			t.Errorf("the watcher armed %d watches; it did not re-arm after the failure", n)
		}
	})

	// A WATCH THAT ONLY EVER FAILS STOPS, at the stated threshold and not one round trip later.
	t.Run("gives up", func(t *testing.T) {
		var suite testsuite.WorkflowTestSuite
		env := suite.NewTestWorkflowEnvironment()
		env.SetTestTimeout(30 * time.Second)
		var calls int32
		env.RegisterActivityWithOptions(watchThatFails(&calls),
			activity.RegisterOptions{Name: wardenWatchActivityName})

		env.ExecuteWorkflow(wardenWorkflow, wardenWorkflowInput{
			WardenID: "wdn-broken000000", Queue: "q",
			UnreachableMin: time.Hour, UnreachableMax: time.Hour, Heartbeat: time.Minute,
		})
		if !env.IsWorkflowCompleted() {
			t.Fatal("a watch that only ever fails left the watcher running")
		}
		if env.GetWorkflowError() == nil {
			t.Error("a watcher that gave up reported success; the reason it stopped has to be in the " +
				"history, because nothing else will ever say it")
		}
		if n := atomic.LoadInt32(&calls); int(n) != wardenFailuresBeforeStopping {
			t.Errorf("the watcher armed %d watches before stopping, want exactly %d — an unbounded "+
				"retry here is six Temporal events per round trip until the history limit",
				n, wardenFailuresBeforeStopping)
		}
	})
}

// RETIRING IS THE ONLY WAY THIS WORKFLOW ENDS ON PURPOSE, and it exists for a hazard slice 03
// reported and did not fix: a certificate lives one year with no renewal, re-enrolment mints a new
// key, and a Warden's id is derived from its key — so the remedy for an expiring Machine produces a
// NEW Warden and leaves the old one's watcher arming watches on a queue nobody will poll again.
func TestTheWatcherStopsWhenTheMachineIsRetired(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.SetTestTimeout(30 * time.Second)

	// A watch that never answers — which is the state a retired Machine's watcher is in forever.
	hold := make(chan struct{})
	t.Cleanup(func() { close(hold) })
	env.RegisterActivityWithOptions(func(ctx context.Context) (wardenDecision, error) {
		<-hold
		return wardenDecision{}, nil
	}, activity.RegisterOptions{Name: wardenWatchActivityName})
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(wardenRetireSignal, nil)
	}, time.Second)

	env.ExecuteWorkflow(wardenWorkflow, wardenWorkflowInput{
		WardenID: "wdn-retired0000", Queue: "warden-wdn-retired0000",
		UnreachableMin: time.Hour, UnreachableMax: time.Hour, Heartbeat: time.Minute,
	})
	if !env.IsWorkflowCompleted() {
		t.Fatal("the retire signal did not end the watcher")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Errorf("a retired watcher ended with %v, want a clean completion — retiring is an "+
			"operator's act, not a failure", err)
	}
}
