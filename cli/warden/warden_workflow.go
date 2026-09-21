package warden

// warden_workflow.go — the **Warden** as a Temporal Worker that BLOCKS, and the four decisions that
// are worth a durable event.
//
// ADR 0037: "The Warden is a Temporal Worker, and it BLOCKS. One per **Machine**, waiting on a
// condition rather than a timer, so history records decisions — assignment changed, **Worker**
// started, **Worker** exited non-zero, **Machine** unreachable — at roughly five events per real
// change and nothing at rest. This buys durable fleet lifecycle and puts it in the Transcript and
// the Event log for free."
//
// ═══ WHY A TICK WAS NEVER AN OPTION ═══
//
// Measured previously in this repo and recorded in 0037: a WORKFLOW that turns on a timer costs ~20
// events per tick. warden.go's loop turns every five seconds, which as a workflow would be ~14,400
// events per hour PER MACHINE and continue-as-new every few hours — for a hundred Machines, a
// control plane whose entire history is Wardens saying nothing happened. The same loop against a
// socket costs one `podman ps`, which is why warden.go keeps it and this file does not repeat it.
//
// So the split is: THE MACHINE OBSERVES, THE WORKFLOW RECORDS. warden.go's reconcile loop runs
// exactly as it did and hands each turn's observation to `wardenWatchpoint.saw`, which DIFFS it
// against the previous turn and produces a decision only when something actually changed. The
// workflow is blocked in `Selector.Select` the entire time in between, and a blocked workflow writes
// nothing.
//
// MEASURED on this checkout against the live Temporal at `config.TemporalAddress()`, by
// warden_workflow_live_test.go, which prints every number it asserts:
//
//	at rest ..................... 0 events over a 30s window — 120 reconcile turns and ~30
//	                              heartbeats — which is 0 events/hour, and 0 for any window
//	assignment changed .......... 6 events
//	Worker started .............. 6 events
//	Worker exited ............... 6 events
//	the Warden itself stopping .. 6 events
//	Machine died holding the
//	  watch (heartbeat timeout) . 6 events
//	Machine never took the watch
//	  (schedule-to-start) ....... 5 events, and the NEXT observation is scheduled further out
//	                              (see `unreachableAfter`), so a Machine that stays dark settles
//	                              at ~5 events/hour rather than ~5 a minute
//
// Six, not five, and the shape is worth knowing because it is the same six every time:
//
//	ActivityTaskStarted                           written LAZILY, at close, so an open watch costs
//	                                              nothing however long it is open
//	ActivityTaskCompleted / …TimedOut             what the watch said, or that it went quiet
//	WorkflowTask ×3                               one task, in which the workflow reads it
//	ActivityTaskScheduled                         …and arms the next watch, in that same task
//
// Five is that list without the `ActivityTaskStarted` that never happened — which is also the proof
// of what it is reporting: no worker took it. Nothing else can appear, because there is nothing else
// in the loop.
//
// ═══ THE ASSIGNMENT DOES NOT TRAVEL THROUGH HISTORY, AND THAT IS A REVERSAL ═══
//
// warden_ca.go says of `GET /warden/assignment`: "ADR 0037 puts the desired state in a blocked
// Temporal workflow and slice 04 moves it there — at which point this endpoint and its directory are
// deleted." That was slice 03's forecast and it is not what this slice does. The endpoint stays and
// the Warden still fetches its assignment over its own mTLS connection; what moved into the workflow
// is the RECORD that the assignment changed, not the assignment.
//
// The reason is arithmetic. Pushing desired state through the workflow means the activity's INPUT
// carries it, which means a change has to cancel the watch in flight and start another one — and
// each of those events is one Temporal writes unconditionally:
//
//	WorkflowExecutionSignaled, WorkflowTask ×3, ActivityTaskCancelRequested, ActivityTaskScheduled,
//	ActivityTaskStarted, ActivityTaskCanceled, WorkflowTask ×3        = 11 events
//
// against the 6 measured below for the Machine noticing the same change on its own next turn and
// reporting it. That eleven is COUNTED, not measured — the design it belongs to was not built — and
// the count is credible for the same reason the six is: this loop's history has no event in it that
// is not one of the five in that list. Eleven is not "roughly five", and the extra five buy a
// delivery guarantee the Machine already has: the fetch is idempotent desired state read on every
// turn, so a Warden that was off for a week converges on the same answer (warden.go, rule 2). ADR
// 0037's sentence is "so history records decisions" — history is the LEDGER, and a ledger that is
// also the transport pays for the traffic twice.
//
// The cancellation is also a hazard on its own terms. warden.go's `start` runs under a context that
// can never be cancelled, because a SIGKILL between `go run`'s compile and its exec orphans a
// handler; a workflow that cancels the watch activity mid-reconcile is a second source of exactly
// that cancellation. This design has no cancellation anywhere: every watch ends because the Machine
// decided it should.
//
// ═══ WHAT DETECTS A MACHINE THAT IS GONE ═══
//
// The one decision the Machine cannot report is its own absence, and it is why there is an activity
// at all rather than the Machine signalling the workflow directly (which would cost 4 events, not 6).
// The watch runs on the MACHINE'S OWN task queue, so:
//
//   - `ScheduleToStartTimeout` fires when nothing on that queue takes the work — the Warden is not
//     polling, which is the Machine being off, partitioned, or never having come up.
//   - `HeartbeatTimeout` fires when it took the work and then went quiet — the Machine died holding
//     it.
//
// Both are recorded BY THE CONTROL PLANE'S worker, which is the whole reason the workflow runs on
// `wardenPlaneQueue` and not on the Machine's. A watcher hosted by the thing it is watching cannot
// report that the thing it is watching has stopped answering: the workflow task would sit unpolled
// beside the activity task and nothing would be written at all.
//
// A HEARTBEAT IS NOT A TICK. Heartbeats are recorded in Temporal's mutable state and never appended
// to history — the live test asserts exactly that by watching a Machine heartbeat for thirty seconds
// and finding zero new events. The heartbeat interval is ordinary Go on the Machine, in the activity,
// which is the same place warden.go's five seconds already lives.
//
// ═══ NO TIMER, AND IT IS ASSERTED RATHER THAN INTENDED ═══
//
// There is no `workflow.Sleep`, no `workflow.NewTimer`, no `AwaitWithTimeout` and no
// `Selector.AddDefault` below. The backoff on an unreachable Machine is expressed as the NEXT
// watch's `ScheduleToStartTimeout` — an activity option, which costs nothing until it fires, rather
// than a sleep, which costs a `TimerStarted` and a `TimerFired` and a workflow task to notice each.
// `TestTheWatcherStartsNoTimer` reads the finished history back and fails on any `Timer` event,
// which is a claim about what ran rather than about what the source says.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	enumspb "go.temporal.io/api/enums/v1"
)

// --- the names both sides of the protocol know --------------------------------------------------

const (
	// wardenPlaneQueue is where a **Warden**'s workflow RUNS, and it is deliberately not the
	// Machine's own queue. See this file's header: a watcher polled by the Machine it watches cannot
	// record that the Machine stopped polling.
	wardenPlaneQueue = "kontra-wardens"

	// wardenWorkflowType is how the workflow is registered and how it appears in the Workflows page.
	// `stackWorkflow`'s spelling, for the same reason `transcript.ts` pins that one: a surface reads
	// the type off the log, and a rename that nobody pinned draws the wrong kind of turn.
	wardenWorkflowType = "wardenWorkflow"

	// wardenWatchActivityName is the ONE activity a **Machine** serves. It is called by name so the
	// control plane's worker never needs the implementation — which is right, because the
	// implementation reads that Machine's runtime and exists nowhere else.
	wardenWatchActivityName = "wardenWatch"

	// wardenRetireSignal ends a **Warden**'s workflow. See `wardenCARetire` for the one thing it is
	// for: a **Machine** whose identity changed, which is what re-enrolment does and what a
	// certificate expiring forces.
	wardenRetireSignal = "kontra.warden.retire"

	wardenWorkflowIDPrefix   = "kontra-warden/"
	wardenMachineQueuePrefix = "warden-"
)

// wardenWorkflowID and wardenMachineQueue are DERIVED FROM THE WARDEN'S ID, which is itself derived
// from the Machine's own key (`wardenIDFor`). Nothing allocates either, so a Warden that restarts
// re-attaches to its own workflow and its own queue with nothing to look up.
//
// ═══ NEITHER OF THESE NAMES IS A BOUNDARY, AND SLICE 08 IS WHY THAT MATTERS ═══
//
// Both are unguessable — 64 bits of a hash of a key nobody else has — and unguessable is not the same
// as unauthorised. In ONE namespace:
//
//   - anything holding a Temporal client may poll `warden-wdn-<hex>` and take a watch armed for that
//     Machine, and
//   - `wardenRetire` is a SIGNAL BY WORKFLOW ID, which needs no queue and no poll at all — so knowing
//     a Warden id is enough to end another Machine's watcher, and the Controller prints that id on
//     every enrolment and again in `kontra warden ca list`.
//
// Renaming either would fix neither. What fixes both is that two tenants' Machines are in two
// namespaces, which is the only authorisation boundary Temporal has (ADR 0036 §7). All three names
// below stay exactly as they are — they are namespace-RELATIVE, which is the same reason
// `shared/conformance/queues.json` needed no change for this slice.
func wardenWorkflowID(wardenID string) string   { return wardenWorkflowIDPrefix + wardenID }
func wardenMachineQueue(wardenID string) string { return wardenMachineQueuePrefix + wardenID }

// --- the tuning ---------------------------------------------------------------------------------

const (
	// wardenHeartbeatDefault is how often the watch tells Temporal the Machine is still there, and
	// therefore how quickly a Machine that dies mid-watch is noticed. It writes NOTHING to history —
	// heartbeats live in mutable state — so this is a cost against a socket, not against a ledger.
	wardenHeartbeatDefault = 30 * time.Second

	// wardenUnreachableMinDefault is how long the control plane waits for SOMETHING on this
	// Machine's queue to take the watch before it calls the Machine unreachable. A minute rather
	// than a few seconds because a Warden restarting (`Restart=always`, `RestartSec=5`) and a
	// Machine rebooting are both normal, and neither should write an event.
	wardenUnreachableMinDefault = 1 * time.Minute

	// wardenUnreachableMaxDefault bounds the same wait once a Machine has missed repeatedly.
	//
	// THIS IS THE BACKOFF, AND IT IS AN ACTIVITY OPTION RATHER THAN A SLEEP. A dark Machine costs
	// six events per observation; at a fixed one-minute wait that is 360 events/hour of a workflow
	// saying the same thing, which reaches Temporal's history limit in under a week and forces the
	// continue-as-new this whole design exists to avoid. Doubling to an hour makes a Machine that is
	// gone cost ~6 events/hour, and it costs nothing while it is not firing.
	wardenUnreachableMaxDefault = 1 * time.Hour

	// wardenWatchStopGrace is how long `worker.Stop()` waits for the in-flight watch to report before
	// it tears the poller down.
	//
	// THE SDK'S DEFAULT IS ZERO, AND ZERO IS A COIN FLIP. Stopping cancels the watch and then cancels
	// the worker's background context; the watch returns a decision immediately (see
	// `wardenWatchpoint.watch`), but whether its completion RPC wins that race decides whether the
	// server records `ActivityTaskCompleted` or `ActivityTaskFailed` — and both were observed on this
	// box. The workflow handles either, and this is what makes the clean one the common one. It costs
	// a stopping Warden nothing in practice, because the watch has nothing to do but return.
	wardenWatchStopGrace = 5 * time.Second

	// wardenFailuresBeforeStopping is how many watches may end in an ERROR, consecutively, before the
	// watcher gives up rather than re-arming. See the `default:` branch of `wardenWorkflow` for the
	// two failures this number is between: an ordinary shutdown race, which recovers in one, and an
	// activity that fails instantly for ever, which would otherwise cost six events a round trip until
	// it hit Temporal's history limit. Five is enough to absorb the first and cheap enough to pay for
	// the second once.
	wardenFailuresBeforeStopping = 5

	// wardenWatchLifetime is the watch activity's `StartToCloseTimeout`.
	//
	// TEMPORAL REQUIRES ONE OF THE TWO CLOSE TIMEOUTS AND THIS WATCH HAS NO NATURAL END. A Machine
	// that nothing happens to is supposed to hold one watch open for as long as it lives, so any
	// value that could plausibly be reached would be a decision recorded about nothing — an
	// "unreachable" for a Machine that is fine. Liveness is the heartbeat's job, and this is set past
	// the life of any Machine so that it is never the thing that fires.
	wardenWatchLifetime = 10 * 365 * 24 * time.Hour
)

// --- what a decision is -------------------------------------------------------------------------

// wardenDecisionKind is one of the four things ADR 0037 says are worth a durable event, plus the
// opening line of a Warden's workflow.
//
// THE LIST IS CLOSED ON PURPOSE. Every candidate fifth — a reconcile turn that changed nothing, a
// backoff that elapsed, a `list` that took 200ms — is a fact that is stale a second later, which is
// the class of thing 0037 sends down the pane-snapshot path instead (slice 05). If a value would be
// wrong by the time somebody read it, it does not belong in a ledger.
type wardenDecisionKind string

const (
	// decisionWatching is the state a Warden's workflow opens in, before its Machine has said
	// anything. It is a kind rather than an empty summary so that the first event of a Machine's
	// life reads as a sentence.
	decisionWatching wardenDecisionKind = "watching"

	// decisionAssignment is "the control plane asked for something different". Reported by the
	// MACHINE, from its own fetch, because that is the moment the change becomes true of this
	// Machine — see this file's header on why the assignment itself does not travel through history.
	decisionAssignment wardenDecisionKind = "assignment changed"

	decisionWorkerStarted wardenDecisionKind = "worker started"

	// decisionWorkerExited is ADR 0037's "Worker exited non-zero", NARROWED, and the narrowing is
	// stated rather than papered over: the `workerDriver` seam reads the runtime with `list()` and
	// has no verb that returns an exit status (driver.go deliberately has four verbs and `wait` is
	// not one of them). What this Warden can witness is that a Worker it was asked to run is no
	// longer whole and nothing asked it to stop, which is the fact an operator acts on. The exit
	// CODE is not available here and is not invented.
	decisionWorkerExited wardenDecisionKind = "worker exited"

	// decisionUnreachable is the one decision the Machine cannot report about itself. It is produced
	// by the workflow, from Temporal's own activity timeouts. See this file's header.
	decisionUnreachable wardenDecisionKind = "machine unreachable"
)

// wardenDecision is one thing that happened on a **Machine**, as the control plane records it.
//
// SMALL, AND NOT A TELEMETRY FRAME. There is no CPU, no memory, no load ratio and no pane content
// here, because those go over the snapshot path (`/api/panels/terminals`) where being replaced every
// few seconds is the point. ADR 0037: "A 200×50 terminal frame through Temporal history is the shape
// this repo already measured as 86% of a workflow's events, for a value that is stale a second
// later."
type wardenDecision struct {
	Kind wardenDecisionKind `json:"kind"`
	// Subject is what the decision is ABOUT: `<name>@<version>` for a Worker, the Warden's own id
	// for anything about the Machine.
	Subject string `json:"subject,omitempty"`
	Detail  string `json:"detail,omitempty"`
	// At is the MACHINE's clock, carried for the operator and never used to order anything. Temporal
	// timestamps the event; a Machine's clock is a claim, and a claim is not what a ledger sorts by.
	At time.Time `json:"at,omitempty"`
}

// The Summary grammar, and the one cross-language literal in this slice.
//
// A DECISION REACHES THE TRANSCRIPT AS A TEMPORAL USER-METADATA SUMMARY, ≤200 bytes, set on the
// event that arms the NEXT watch. `control/orchestrator/src/vocabulary.ts` reads it back and names it, which is
// how a **Machine**'s lifecycle appears in an operator's account of a run with no component
// anywhere knowing what a Warden is. The prefix and the five kind spellings are written out on both
// sides — the house rule for a literal two languages share — and `vocabulary.test.ts` reads THIS
// FILE'S BYTES to pin them, so a rename here is a red test rather than a Machine that silently drops
// out of the Transcript.
const (
	wardenSummaryPrefix = "kontra.machine"
	// wardenSummarySep is `transcript.ts`'s own `SUMMARY_SEP`, so a Summary and a reduced log's
	// `detail` line split the same way.
	wardenSummarySep = " · "
	// wardenSummaryMax is Temporal's practical budget for user metadata and the same 200 bytes
	// `dispatch_summary` spends. A field this has to shorten carries the ellipsis, which is what
	// makes `transcript.ts:METHOD_NAME` unable to mistake a truncated field for a Method name.
	wardenSummaryMax = 200
)

// summary renders a decision as the one line that reaches the Transcript.
//
// THE FIELDS ARE IN COST ORDER, like `dispatch_summary`: the kind first, because it is the word an
// operator scans for; then what it is about; then why. A detail that would push the line past the
// budget is shortened, never dropped, because a truncated reason still names the Worker.
func (d wardenDecision) summary() string {
	fields := []string{wardenSummaryPrefix, string(d.Kind)}
	if d.Subject != "" {
		fields = append(fields, d.Subject)
	}
	line := strings.Join(fields, wardenSummarySep)
	if d.Detail == "" {
		return line
	}
	full := line + wardenSummarySep + d.Detail
	if len(full) <= wardenSummaryMax {
		return full
	}
	room := wardenSummaryMax - len(line) - len(wardenSummarySep) - len("…")
	if room <= 0 {
		return line
	}
	return line + wardenSummarySep + d.Detail[:room] + "…"
}

// --- the Machine's side: turning observations into decisions ------------------------------------

// wardenWatchpoint is where warden.go's reconcile loop and this slice's watch activity meet.
//
// IT DIFFS; IT DOES NOT POLL. `saw` is called once per reconcile turn — five seconds apart in
// production — with the two things that turn already read: what the control plane asked for, and
// what the runtime is actually holding. Two identical turns produce nothing at all, which is what
// makes a workflow that blocks on this cost zero at rest. Everything a decision says is a DIFFERENCE
// between two consecutive turns.
//
// THE QUEUE IS BOUNDED AND A DROP IS COUNTED. An unbounded queue behind a watcher nobody is draining
// is a Machine that runs out of memory rather than one that loses a line, and a silent drop is worse
// than both. So decisions go into a fixed buffer, an overrun increments a counter, and the next
// decision that gets through carries the count — which is the only shape where the failure is
// visible from the ledger it damaged.
type wardenWatchpoint struct {
	// id is the Warden this speaks for, used as the Subject of every decision about the Machine
	// rather than about one Worker.
	id string

	decisions chan wardenDecision
	now       func() time.Time

	mu sync.Mutex
	// want is the previous turn's DESIRED ids, and whole is the previous turn's OBSERVED whole ones.
	// Both are the previous turn only: nothing here accumulates, because a watchpoint that
	// remembered further back would be a second opinion about what is running, which driver.go's
	// header calls "the single most common bug in this class of software".
	want    map[string]bool
	whole   map[string]bool
	started bool
	dropped int
}

// wardenWatchpointBuffer is how many decisions may be pending before one is lost. Sixty-four is two
// orders of magnitude more than a reconcile turn can produce (one assignment change plus one line
// per Worker on the Machine) and costs nothing to hold.
const wardenWatchpointBuffer = 64

func newWardenWatchpoint(id string, now func() time.Time) *wardenWatchpoint {
	if now == nil {
		now = time.Now
	}
	return &wardenWatchpoint{
		id:        id,
		decisions: make(chan wardenDecision, wardenWatchpointBuffer),
		now:       now,
		want:      map[string]bool{},
		whole:     map[string]bool{},
	}
}

// saw is one reconcile turn's observation. NIL-SAFE, because a **Warden** with no control plane —
// an airgapped install, every test in warden_test.go — runs the same loop with nothing watching it,
// and a loop whose behaviour depended on whether anybody was listening would be a different loop
// under test than in production.
func (p *wardenWatchpoint) saw(desired []Spec, actual []workerHandle) {
	if p == nil {
		return
	}
	want := make(map[string]bool, len(desired))
	for _, s := range desired {
		want[s.Name+"@"+s.Version] = true
	}
	whole := make(map[string]bool, len(actual))
	for _, h := range actual {
		if h.whole() {
			whole[h.id()] = true
		}
	}

	p.mu.Lock()
	first := !p.started
	prevWant, prevWhole := p.want, p.whole
	p.want, p.whole, p.started = want, whole, true
	p.mu.Unlock()

	// THE FIRST TURN IS ONE DECISION, NOT A FLOOD. A Warden that has just started sees every desired
	// Worker as "new" and every running one as "just started", and reporting all of that would make
	// a routine restart the noisiest thing in a Fleet's history. So the first turn says what this
	// Machine was asked to hold, once, and everything after it is a difference.
	if first {
		p.emit(wardenDecision{
			Kind:    decisionAssignment,
			Subject: p.id,
			Detail:  firstAssignmentDetail(want),
		})
		return
	}

	if added, removed, changed := diffIDs(prevWant, want); changed {
		p.emit(wardenDecision{
			Kind:    decisionAssignment,
			Subject: p.id,
			Detail:  assignmentDetail(added, removed),
		})
	}

	// EXITS BEFORE STARTS, mirroring `reconcile`'s own order, so a Worker that was replaced reads as
	// gone-then-back rather than as two arrivals with a disappearance between them.
	for _, id := range sortedIDs(prevWhole) {
		// A WORKER THIS MACHINE WAS TOLD TO STOP IS NOT A WORKER THAT EXITED. Dropping out of the
		// assignment is the control plane's act and it is already recorded as `assignment changed`;
		// counting it again here would make every scale-down look like a crash.
		if whole[id] || !want[id] {
			continue
		}
		p.emit(wardenDecision{Kind: decisionWorkerExited, Subject: id, Detail: wardenExitDetail(actual, id)})
	}
	for _, id := range sortedIDs(whole) {
		if prevWhole[id] {
			continue
		}
		p.emit(wardenDecision{Kind: decisionWorkerStarted, Subject: id})
	}
}

// emit queues one decision, stamping the Machine's clock and folding in anything that was lost.
func (p *wardenWatchpoint) emit(d wardenDecision) {
	d.At = p.now().UTC()
	p.mu.Lock()
	if lost := p.dropped; lost > 0 {
		p.dropped = 0
		p.mu.Unlock()
		d.Detail = strings.TrimSpace(d.Detail + fmt.Sprintf(" (%d earlier decisions were dropped: "+
			"nothing was watching this Machine and the buffer filled)", lost))
	} else {
		p.mu.Unlock()
	}
	select {
	case p.decisions <- d:
	default:
		p.mu.Lock()
		// +1 for the one that could not be queued, and +1 more for whatever this decision was
		// already carrying, so the count never understates.
		p.dropped++
		p.mu.Unlock()
	}
}

// next BLOCKS until this Machine has decided something. THE WHOLE POINT: there is no poll here and
// no deadline, so a Machine with nothing to say costs one parked goroutine.
func (p *wardenWatchpoint) next(ctx context.Context) (wardenDecision, error) {
	select {
	case d := <-p.decisions:
		return d, nil
	case <-ctx.Done():
		return wardenDecision{}, ctx.Err()
	}
}

func firstAssignmentDetail(want map[string]bool) string {
	ids := sortedIDs(want)
	if len(ids) == 0 {
		return "this Machine was asked to hold nothing"
	}
	return fmt.Sprintf("%d Worker(s): %s", len(ids), strings.Join(ids, ", "))
}

func assignmentDetail(added, removed []string) string {
	var parts []string
	for _, id := range added {
		parts = append(parts, "+"+id)
	}
	for _, id := range removed {
		parts = append(parts, "-"+id)
	}
	if len(parts) == 0 {
		// Unreachable through `diffIDs`, which only reports `changed` when one of the two lists is
		// non-empty. Written anyway, because a decision whose detail is the empty string reads as a
		// bug in the reader rather than in the writer.
		return "the assignment changed"
	}
	return strings.Join(parts, " ")
}

// wardenExitDetail says WHICH way a Worker stopped being whole, which is the difference between a
// pair that vanished and the half-dead pair serve.go calls "worse than a dead one".
func wardenExitDetail(actual []workerHandle, id string) string {
	for _, h := range actual {
		if h.id() == id {
			return "only its " + halvesOf(h) + " half is running"
		}
	}
	return "both halves are gone"
}

func diffIDs(before, after map[string]bool) (added, removed []string, changed bool) {
	for _, id := range sortedIDs(after) {
		if !before[id] {
			added = append(added, id)
		}
	}
	for _, id := range sortedIDs(before) {
		if !after[id] {
			removed = append(removed, id)
		}
	}
	return added, removed, len(added) > 0 || len(removed) > 0
}

func sortedIDs(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		if v {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// --- the activity: the Machine's half of the block ----------------------------------------------

// watch is the whole of what a **Machine** serves to Temporal.
//
// IT RETURNS ONE DECISION AND THEN ENDS. A watch that reported several would need a batching rule
// and a cursor; a watch that never ended would need cancellation to deliver anything, which is the
// hazard this design does not have. One decision per activity means one decision per six events, and
// the arithmetic an operator does over a history is a division.
//
// THE HEARTBEAT IS THE ONLY CLOCK IN THIS FILE, and it is ordinary Go on the Machine. It writes
// nothing to history — Temporal keeps heartbeats in mutable state — and it exists so that a Machine
// that dies holding this watch is noticed rather than being confused with one that has simply had
// nothing to say for a week.
//
// ═══ A WARDEN SHUTTING DOWN IS A DECISION, NOT AN ERROR ═══
//
// This function returned `ctx.Err()` on cancellation, which is the obvious way to write it and was
// wrong in the way that matters most. `worker.Stop()` — which is what `systemctl restart
// kontra-warden` and Ctrl-C both reach — CANCELS the activity's context, so the watch failed with
// `context canceled`; that is neither a timeout nor a Temporal cancellation, so it fell into
// `wardenWorkflow`'s "an error nobody designed for" branch and FAILED THE WATCHER. A routine Warden
// restart permanently ended the thing watching that Machine, and the Machine came back with nothing
// recording it. Caught by TestAMachineThatStopsAnsweringIsRecorded, whose control had just proved the
// Machine was answering.
//
// So a cancelled watch RETURNS, with the truest thing there is to say: this Machine has stopped
// watching. The control plane records it, arms the next watch on a queue nobody is polling, and — if
// the Warden really is gone rather than restarting — the schedule-to-start timeout says so a minute
// later, on its own.
//
// THAT WAS HALF THE FIX, AND THE TEST WENT ON TO FIND THE OTHER HALF BY FLAKING. Returning promptly
// is not the same as being HEARD: `worker.Stop()` cancels the watch and then tears the poller down,
// so the completion RPC is in a race with the shutdown and the server records `ActivityTaskFailed`
// when it loses. Both outcomes were observed on this box from the same code. `wardenWatchStopGrace`
// makes the clean one common and `wardenWorkflow`'s `default:` branch makes the other one survivable;
// neither alone was enough, and a design that needed the race to go one way was not a design.
func (p *wardenWatchpoint) watch(ctx context.Context) (wardenDecision, error) {
	beat := wardenHeartbeatDefault / 3
	if info := activity.GetInfo(ctx); info.HeartbeatTimeout > 0 {
		// A THIRD OF THE TIMEOUT, so one lost heartbeat is not a dead Machine. The timeout is
		// whatever the workflow asked for, which is how the live test makes thirty seconds two.
		beat = info.HeartbeatTimeout / 3
	}
	if beat <= 0 {
		beat = time.Second
	}
	ticker := time.NewTicker(beat)
	defer ticker.Stop()
	for {
		select {
		case d := <-p.decisions:
			return d, nil
		case <-ticker.C:
			// The details are empty on purpose. A heartbeat payload is a place telemetry would
			// arrive by accident, and ADR 0037 puts telemetry on the snapshot path.
			activity.RecordHeartbeat(ctx)
		case <-ctx.Done():
			// A DECISION AND A NIL ERROR. See this function's header for the outage the obvious
			// `return ctx.Err()` produced. A decision that is already queued is preferred to the
			// shutdown note, because a Worker that died a moment before its Warden was stopped is a
			// fact worth more than the stop.
			select {
			case d := <-p.decisions:
				return d, nil
			default:
			}
			return wardenDecision{
				Kind:    decisionUnreachable,
				Subject: p.id,
				Detail:  "this Machine's Warden stopped watching (" + ctx.Err().Error() + ")",
				At:      p.now().UTC(),
			}, nil
		}
	}
}

// --- the workflow: the block itself -------------------------------------------------------------

// wardenWorkflowInput is what the **Machine** tells its own workflow when it attaches.
//
// IT NAMES NO WORKER. The desired state is not here and never arrives here — see this file's header.
// What a workflow needs to know is which queue reaches this Machine and what to call it.
type wardenWorkflowInput struct {
	WardenID string `json:"wardenId"`
	// Queue is the Machine's own task queue. Carried rather than derived so that a Warden and its
	// workflow cannot disagree about it after a rename: the Machine says which queue it is polling.
	Queue    string `json:"queue"`
	Hostname string `json:"hostname,omitempty"`
	Driver   string `json:"driver,omitempty"`

	// The three tunables ride on the INPUT rather than being read from a package variable, because a
	// workflow that read a mutable global would replay differently after a redeploy — and because
	// the measurement test has to be able to make a minute two seconds without changing what
	// production does.
	Heartbeat      time.Duration `json:"heartbeat,omitempty"`
	UnreachableMin time.Duration `json:"unreachableMin,omitempty"`
	UnreachableMax time.Duration `json:"unreachableMax,omitempty"`

	// ═══ WHAT CROSSES A CONTINUE-AS-NEW BOUNDARY (kontra#10) ═══
	//
	// This workflow watches a Machine for as long as the Machine exists, and Temporal TERMINATES an
	// execution at 51,200 history events. A dark Machine writes five events per watch — measured —
	// which is a death date about 14 months out. What dies is the watcher on a live Machine, with no
	// signal, no metric and no degraded mode: the server simply ends it.
	//
	// CARRIED, NOT REBUILT. `Last` is what the next watch's Summary says, so losing it would make the
	// first row after a handover read "attached from …" as though the Machine had just enrolled —
	// a lie about a Machine that may have been dark for a month. `Misses` drives the backoff, so
	// losing it would drop a Machine that had backed off to an hour straight back to a minute and
	// undo the whole reason the backoff exists.
	Last   *wardenDecision `json:"last,omitempty"`
	Misses int             `json:"misses,omitempty"`
	// Failures is deliberately NOT carried. It counts CONSECUTIVE errors toward
	// wardenFailuresBeforeStopping, and a handover is not an error — resetting it is the correct
	// reading of "consecutive", and carrying it would let a chain accumulate toward a stop across
	// boundaries that had nothing to do with each other.
}

func (in wardenWorkflowInput) heartbeat() time.Duration {
	if in.Heartbeat > 0 {
		return in.Heartbeat
	}
	return wardenHeartbeatDefault
}

// unreachableAfter is how long the control plane waits for this Machine to take the next watch, and
// it doubles for each consecutive miss. See `wardenUnreachableMaxDefault` for why the backoff is
// here rather than in a sleep.
func (in wardenWorkflowInput) unreachableAfter(misses int) time.Duration {
	floor, ceiling := in.UnreachableMin, in.UnreachableMax
	if floor <= 0 {
		floor = wardenUnreachableMinDefault
	}
	if ceiling <= 0 {
		ceiling = wardenUnreachableMaxDefault
	}
	// A CEILING UNDER THE FLOOR IS A TYPO, NOT A LICENCE TO WAIT FOR EVER. Taking the larger would
	// make one mistyped flag turn every Machine in a Fleet unwatchable for an hour at a time.
	if ceiling < floor {
		ceiling = floor
	}
	d := floor
	for i := 0; i < misses; i++ {
		d *= 2
		if d >= ceiling || d <= 0 {
			return ceiling
		}
	}
	return d
}

// wardenWorkflow is the watcher. It BLOCKS.
//
// The entire body is: arm one watch on the Machine's queue, block until it answers or until somebody
// retires this Machine, record what it said, arm the next one. There is no timer, no poll, no
// condition variable that is checked on a schedule, and no state that grows.
//
// THE SUMMARY DESCRIBES THE PREVIOUS DECISION, AND THAT IS DELIBERATE. Temporal's user metadata is
// set when a command is ISSUED, so the only event this workflow can attach a sentence to is the one
// that schedules the next watch — an activity's RESULT is a payload, and ADR 0007 is that payloads
// are never decoded by a surface. So the event that reads "worker exited · nscheck@0.1.0" is the
// watch that was armed microseconds after that exit was reported, and the row's duration is the quiet
// that followed it. The alternative was a second activity per decision purely to carry the sentence,
// which doubles the cost of every real change to carry no new fact.
func wardenWorkflow(ctx workflow.Context, in wardenWorkflowInput) error {
	log := workflow.GetLogger(ctx)
	retire := workflow.GetSignalChannel(ctx, wardenRetireSignal)

	// SEEDED FROM THE HANDOVER when there was one, so the first row of a continued leg says what the
	// Machine was actually doing rather than announcing a fresh attach.
	last := wardenDecision{
		Kind:    decisionWatching,
		Subject: in.WardenID,
		Detail:  "attached from " + wardenHostLabel(in),
	}
	if in.Last != nil {
		last = *in.Last
	}
	// misses is consecutive TIMEOUTS and drives the backoff; failures is consecutive ERRORS and is the
	// only thing that can stop this workflow on its own. Two counters because they mean two different
	// things: a Machine that is quiet is expected and cheap, and a watch that keeps erroring is a bug
	// that must not be allowed to fill a history.
	misses, failures := in.Misses, 0

	for {
		// ═══ HAND OVER WHEN THE SERVER SAYS SO ═══
		//
		// GetContinueAsNewSuggested is set on EVERY workflow task once history passes 4,096 events,
		// and nothing in this repository has ever read it. It is free — it is already on the info
		// object — and it is the only warning before the 51,200 ceiling. A dark Machine trips the
		// suggestion at ~34 days and the ceiling at ~14 months.
		//
		// AT THE TOP OF THE LOOP, BEFORE THE WATCH IS ARMED, which is the one place in this loop
		// where nothing is in flight: no activity to abandon, no decision half-read. A handover
		// below the Select would race a watch that had already been scheduled.
		if workflow.GetInfo(ctx).GetContinueAsNewSuggested() {
			log.Info("warden handing over", "wardenId", in.WardenID, "misses", misses)
			next := in
			next.Last = &last
			next.Misses = misses
			return workflow.NewContinueAsNewError(ctx, wardenWorkflowType, next)
		}

		opts := workflow.ActivityOptions{
			TaskQueue: in.Queue,
			// NOBODY TOOK IT = THE MACHINE IS NOT THERE. This is the timeout that turns "the Warden
			// is not polling" into an event, and it is the only reason this workflow does not run on
			// the Machine's own queue.
			ScheduleToStartTimeout: in.unreachableAfter(misses),
			StartToCloseTimeout:    wardenWatchLifetime,
			HeartbeatTimeout:       in.heartbeat(),
			// ONE ATTEMPT. Temporal's own retry would be the right answer for a flaky activity and
			// is the wrong one here: a retried heartbeat timeout never reaches the workflow, so the
			// Machine going quiet would be handled silently by the SDK and never recorded. The
			// retry that matters is the next watch, which this loop arms itself.
			RetryPolicy: &temporal.RetryPolicy{MaximumAttempts: 1},
			Summary:     last.summary(),
		}
		f := workflow.ExecuteActivity(workflow.WithActivityOptions(ctx, opts), wardenWatchActivityName)

		var got wardenDecision
		var err error
		retired := false
		sel := workflow.NewSelector(ctx)
		sel.AddFuture(f, func(fu workflow.Future) { err = fu.Get(ctx, &got) })
		sel.AddReceive(retire, func(c workflow.ReceiveChannel, _ bool) {
			c.Receive(ctx, nil)
			retired = true
		})

		// ═══ THE BLOCK ═══ Nothing below this line runs, and nothing is written to history, until
		// the Machine has something to say or somebody retires it.
		sel.Select(ctx)

		if retired {
			log.Info("warden retired", "wardenId", in.WardenID)
			return nil
		}
		switch {
		case err == nil:
			misses, failures = 0, 0
			last = got
		case temporal.IsCanceledError(err):
			// The workflow itself is being cancelled — the control plane's act, not the Machine's.
			return err
		case temporal.IsTimeoutError(err):
			misses, failures = misses+1, 0
			last = wardenDecision{
				Kind:    decisionUnreachable,
				Subject: in.WardenID,
				Detail:  wardenUnreachableDetail(err, misses, in.unreachableAfter(misses)),
			}
		default:
			// ═══ A WATCH THAT FAILED IS A MACHINE THAT STOPPED WATCHING, NOT A REASON TO STOP ═══
			//
			// This branch returned an error — "an error nobody designed for stops, it does not spin" —
			// on the argument that `watch` can only fail on cancellation, so anything else is a bug.
			// The argument was wrong about how ORDINARY this case is. `worker.Stop()` cancels the
			// in-flight watch and then tears the worker down; whether the watch's completion wins that
			// race decides whether the server records `ActivityTaskCompleted` or `ActivityTaskFailed`,
			// and BOTH happen. `WorkerStopTimeout` below makes the clean one the common one and cannot
			// make it certain. So `systemctl stop kontra-warden` had a coin-flip chance of ending the
			// thing watching that Machine for ever — the same outage `wardenWatchpoint.watch`'s header
			// describes, arriving through the other half of the same race.
			//
			// THE HOT LOOP THE OLD BRANCH WAS GUARDING IS REAL, AND IT IS COUNTED INSTEAD. A watch that
			// fails INSTANTLY and for ever — a panic in the activity, say — would cost six events per
			// round trip with nothing to throttle it, because `ScheduleToStartTimeout` only delays a
			// watch nobody takes. So consecutive failures are counted and the workflow stops at the
			// threshold, naming the error; anything that recovers, including this shutdown race,
			// resets the count long before it.
			misses, failures = misses+1, failures+1
			if failures >= wardenFailuresBeforeStopping {
				return fmt.Errorf("the watch on %s has failed %d times in a row and this watcher is "+
					"stopping rather than re-arming into a loop; `kontra warden serve` on that Machine "+
					"will start a fresh one: %w", in.WardenID, failures, err)
			}
			last = wardenDecision{
				Kind:    decisionUnreachable,
				Subject: in.WardenID,
				Detail: fmt.Sprintf("this Machine's watch ended with an error (%v) — failure %d of %d "+
					"before this watcher stops", err, failures, wardenFailuresBeforeStopping),
			}
		}
		log.Info("machine decision", "wardenId", in.WardenID, "kind", string(last.Kind),
			"subject", last.Subject, "detail", last.Detail)
	}
}

func wardenHostLabel(in wardenWorkflowInput) string {
	host := in.Hostname
	if host == "" {
		host = "an unnamed Machine"
	}
	if in.Driver != "" {
		return host + " via " + in.Driver
	}
	return host
}

// wardenUnreachableDetail says which of the two silences this was, because they send an operator to
// two different places: a Machine that never took the work is off or partitioned, and one that took
// it and stopped heartbeating died holding it.
func wardenUnreachableDetail(err error, misses int, next time.Duration) string {
	var to *temporal.TimeoutError
	what := "the watch timed out"
	if ok := asTimeout(err, &to); ok {
		switch to.TimeoutType() {
		case enumspb.TIMEOUT_TYPE_SCHEDULE_TO_START:
			what = "nothing on this Machine's queue took the watch"
		case enumspb.TIMEOUT_TYPE_HEARTBEAT:
			what = "this Machine took the watch and stopped heartbeating"
		case enumspb.TIMEOUT_TYPE_START_TO_CLOSE, enumspb.TIMEOUT_TYPE_SCHEDULE_TO_CLOSE:
			what = "the watch outlived its close timeout, which should be unreachable"
		}
	}
	return fmt.Sprintf("%s (miss %d; next watch waits %s)", what, misses, next.Truncate(time.Second))
}

// asTimeout is `errors.As` for the SDK's timeout error. A helper rather than an inline `errors.As`
// because the workflow body is the thing a reader is checking for a timer and every line that is not
// about blocking is a line in the way.
func asTimeout(err error, out **temporal.TimeoutError) bool {
	for e := err; e != nil; {
		if t, ok := e.(*temporal.TimeoutError); ok {
			*out = t
			return true
		}
		u, ok := e.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		e = u.Unwrap()
	}
	return false
}

// --- wiring: the Machine attaches, the Controller executes ---------------------------------------

// wardenPlane is a **Warden**'s attachment to the control plane, held for the life of
// `kontra warden serve`.
type wardenPlane struct {
	client client.Client
	worker worker.Worker
	watch  *wardenWatchpoint
	// WorkflowID is what was started or re-used, printed so an operator can go and read it.
	WorkflowID string
}

func (p *wardenPlane) close() {
	if p == nil {
		return
	}
	if p.worker != nil {
		// STOPPING THE WORKER DOES NOT STOP THE WORKERS. This ends the Temporal poller only; the
		// **Workers** on this Machine are containers and processes the driver holds, and warden.go's
		// systemd unit (`KillMode=process`) is explicit that a Warden coming and going must not take
		// them with it.
		p.worker.Stop()
	}
	if p.client != nil {
		p.client.Close()
	}
}

// wardenAttach connects a **Warden** to the control plane that records its lifecycle.
//
// EVERY ARROW IS DIALLED BY THE MACHINE, still. A Temporal worker is a long poll outbound, so this
// adds no listener and needs no inbound port — the property warden.go asserts against
// `/proc/net/tcp` is unchanged, and `TestAttachingToTheControlPlaneOpensNoListener` says so with the
// plane attached.
func wardenAttach(ctx context.Context, id *wardenIdentity, driverName string, watch *wardenWatchpoint, out io.Writer) (*wardenPlane, error) {
	if id.Record.Temporal == "" {
		return nil, errNoControlPlane
	}
	if watch == nil {
		return nil, fmt.Errorf("a Warden cannot attach without a watchpoint to report from")
	}
	// THE NAMESPACE IS NOT CHOSEN HERE, AND THERE IS NO FALLBACK TO `config.DefaultNamespace` ANY MORE.
	// It comes out of this Machine's certificate (`temporalOptions`), which is the only thing in this
	// slice that authorises anything — a fallback to a shared default would put a tenant whose
	// credential was somehow unreadable into the one namespace every other tenant can also reach,
	// which is the collision ADR 0036 §6 describes, arriving through an error path.
	opts, err := id.temporalOptions()
	if err != nil {
		return nil, err
	}
	addr, ns := opts.HostPort, opts.Namespace
	// DialContext, not Dial: `wardenServeAttach` retries this for the life of the process, and a dial
	// that cannot be interrupted would keep a Ctrl-C'd Warden alive until it timed out.
	c, err := client.DialContext(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("this Machine's enrolment names %s (namespace %s) as its control plane "+
			"and it is not answering: %w", addr, ns, err)
	}
	host, _ := os.Hostname()
	queue := wardenMachineQueue(id.Record.WardenID)

	wk := worker.New(c, queue, worker.Options{
		// ONE WATCH AT A TIME, plus room for the one that is being replaced. A Machine serves
		// exactly one activity and holding several would mean several readers on one decision
		// queue, which is how a decision is delivered to a watch that has already been superseded.
		MaxConcurrentActivityExecutionSize: 2,
		// NO WORKFLOWS ARE SERVED HERE. A Machine executes no workflow code — its own watcher runs
		// on the control plane precisely so that a Machine that stops answering is still watched.
		DisableWorkflowWorker: true,
		// …AND A STOPPING WARDEN SAYS SO BEFORE IT GOES. See `wardenWatchStopGrace`.
		WorkerStopTimeout: wardenWatchStopGrace,
	})
	wk.RegisterActivityWithOptions(watch.watch, activity.RegisterOptions{Name: wardenWatchActivityName})
	if err := wk.Start(); err != nil {
		c.Close()
		return nil, fmt.Errorf("this Machine could not start polling %s: %w", queue, err)
	}

	in := wardenWorkflowInput{
		WardenID: id.Record.WardenID,
		Queue:    queue,
		Hostname: host,
		Driver:   driverName,
	}
	run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        wardenWorkflowID(id.Record.WardenID),
		TaskQueue: wardenPlaneQueue,
		// USE_EXISTING, BECAUSE A WARDEN RESTARTING IS NOT A NEW MACHINE. The id is derived from the
		// Machine's key, so `systemctl restart kontra-warden` re-attaches to the SAME workflow and
		// the Machine's history stays one story. Anything else would start a second watcher on a
		// queue that already has one — the same failure `list()` exists to prevent, one level up.
		WorkflowIDConflictPolicy: enumspb.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING,
		// WHOSE MACHINE THIS IS, stamped at start — the Go half of the control plane's
		// `tenantAttributes` (control/orchestrator/src/visibility.ts). A Tenant IS a Temporal
		// namespace (ADR 0036 §7), so this records what a tenant already is rather than inventing a
		// second notion of one.
		//
		// AT START, WHICH IS THE ONLY AFFORDABLE MOMENT. Attributes on start ride inside
		// WorkflowExecutionStarted and write no extra event; an UpsertTypedSearchAttributes inside
		// this workflow would write one per call, into the history that the event-log audit measured
		// walking toward Temporal's 51,200-event ceiling at five events per dark watch.
		//
		// AND IT DOES NOT RETROFIT, WHICH IS WHY IT IS NOT THE WHOLE FIX. USE_EXISTING above means a
		// `systemctl restart kontra-warden` RE-ATTACHES to the execution already running — so a
		// Machine placed before this line never acquires the stamp, and a Warden's execution does
		// not close on its own. Those are read correctly anyway because `temporalClient.ts` derives
		// the tenant from the namespace it queried when the attribute is absent. This line is for
		// Machines placed from here on; that derivation is for the ones already out there.
		TypedSearchAttributes: temporal.NewSearchAttributes(
			temporal.NewSearchAttributeKeyKeyword("KontraTenant").ValueSet(ns),
		),
	}, wardenWorkflowType, in)
	if err != nil {
		wk.Stop()
		c.Close()
		return nil, fmt.Errorf("this Machine could not attach its watcher to %s: %w", addr, err)
	}
	if out != nil {
		fmt.Fprintf(out, "attached to %s (namespace %s)\n  watcher  %s\n  queue    %s\n"+
			"  the watcher runs on %s — it records this Machine's lifecycle only while a Controller "+
			"is serving that queue.\n",
			addr, ns, run.GetID(), queue, wardenPlaneQueue)
	}
	return &wardenPlane{client: c, worker: wk, watch: watch, WorkflowID: run.GetID()}, nil
}

// errNoControlPlane is "this Machine enrolled before its Controller had one", matched with errors.Is
// by `wardenServeAttach` so that the ONE case that is not a failure — and must not be retried for
// ever — reads differently from every case that is.
var errNoControlPlane = errors.New("this Machine's enrolment names no control plane")

// wardenPlaneWorker is the CONTROLLER's half: the worker that actually executes every Machine's
// watcher.
//
// ONE WORKER FOR THE WHOLE FLEET. A blocked workflow occupies no worker slot — it is not resident
// between workflow tasks — so the number of Machines a single plane worker can watch is bounded by
// how often they DECIDE something, not by how many there are. That is the whole economic argument
// for blocking rather than ticking, stated as a capacity claim: at six events per change, a Fleet
// that changes once a minute per Machine is the same load as one workflow doing a little work.
func wardenPlaneWorker(c client.Client) worker.Worker {
	return wardenPlaneWorkerOn(c, wardenPlaneQueue)
}

// wardenPlaneWorkerOn is the same worker on a named queue. The queue is a parameter for exactly one
// caller — warden_workflow_live_test.go, which measures this workflow's event cost against the live
// Temporal on this box and must not do so on the queue a real Fleet's Wardens are watched from.
func wardenPlaneWorkerOn(c client.Client, queue string) worker.Worker {
	wk := worker.New(c, queue, worker.Options{})
	wk.RegisterWorkflowWithOptions(wardenWorkflow, workflow.RegisterOptions{Name: wardenWorkflowType})
	return wk
}

// --- one plane worker per tenant ------------------------------------------------------------------

// wardenPlaneSet is the Controller executing every tenant's watchers — ONE WORKER PER NAMESPACE.
//
// ═══ WHY IT CANNOT BE ONE WORKER ═══
//
// `wardenPlaneQueue` is `kontra-wardens` in EVERY namespace, because a queue name is
// namespace-relative and this slice changed none of them (`shared/conformance/queues.json`). A Temporal
// client is bound to one namespace at dial time, so "poll kontra-wardens" is a different queue in
// each tenant and one worker reaches exactly one of them. A Controller that kept slice 04's single
// worker would answer every enrolment successfully and execute the watchers of one tenant, leaving
// every other tenant's Machines arming watches nobody takes — which reads, in the Transcript, as a
// Fleet where nothing has happened. That is the failure shape this repo has now found in five
// surfaces, so it is a set rather than a worker.
//
// ═══ LAZY, BECAUSE A TENANT IS ONBOARDED WITHOUT A RESTART ═══
//
// `ca serve` starts a worker for every tenant already on the roster, and `ensure` starts one the
// first time an enrolment names a tenant that was minted after the Controller came up. Idempotent by
// namespace, so a hundred Machines of one tenant enrolling produce one worker.
//
// NIL-SAFE, because `ca token`, `ca list` and every test open a CA with no control plane at all and
// must not acquire a Temporal client by touching a field.
type wardenPlaneSet struct {
	ctx  context.Context
	addr string
	out  io.Writer

	mu      sync.Mutex
	workers map[string]func()
}

// wardenPlaneStart stands one tenant's plane worker up and returns how to stop it. A FUNC VAR SO
// TESTS CAN HOLD THE SEAM: what has to be asserted is WHICH namespaces acquired a worker and that a
// second Machine of one tenant does not acquire a second, and the answer to both is a list of strings
// rather than a Temporal connection. Every test in this package restores it; production never touches
// it.
var wardenPlaneStart = func(ctx context.Context, addr, namespace string) (func(), error) {
	c, err := client.DialContext(ctx, client.Options{HostPort: addr, Namespace: namespace})
	if err != nil {
		return nil, fmt.Errorf("temporal at %s (namespace %s): %w", addr, namespace, err)
	}
	wk := wardenPlaneWorker(c)
	if err := wk.Start(); err != nil {
		c.Close()
		return nil, fmt.Errorf("polling %s in namespace %s: %w", wardenPlaneQueue, namespace, err)
	}
	return func() { wk.Stop(); c.Close() }, nil
}

func newWardenPlaneSet(ctx context.Context, addr string, out io.Writer) *wardenPlaneSet {
	if strings.TrimSpace(addr) == "" {
		return nil
	}
	return &wardenPlaneSet{ctx: ctx, addr: addr, out: out, workers: map[string]func(){}}
}

// ensure starts this tenant's plane worker if it is not already running.
//
// A FAILURE HERE IS A SENTENCE, NOT AN ERROR RETURNED TO THE MACHINE. The enrolment succeeded; the
// Machine has its identity and will reconcile. What is missing is the ledger, and telling the Machine
// its enrolment failed would make it spend another token for a problem on the Controller.
func (s *wardenPlaneSet) ensure(namespace string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.workers[namespace]; ok {
		return
	}
	stop, err := wardenPlaneStart(s.ctx, s.addr, namespace)
	if err != nil {
		s.logf("  lifecycle NOT recorded for tenant %s: %v\n"+
			"    Machines of that tenant will still reconcile; nothing will write their history.\n",
			namespace, err)
		return
	}
	s.workers[namespace] = stop
	s.logf("  watchers executed here for tenant %s, on %s (%s, namespace %s)\n",
		namespace, wardenPlaneQueue, s.addr, namespace)
}

func (s *wardenPlaneSet) logf(format string, args ...any) {
	if s == nil || s.out == nil {
		return
	}
	fmt.Fprintf(s.out, format, args...)
}

func (s *wardenPlaneSet) close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for ns, stop := range s.workers {
		stop()
		delete(s.workers, ns)
	}
}

// namespaces is what this Controller is currently executing watchers for. For `kontra warden ca list`
// and for the tests, which must be able to assert that a tenant acquired a worker rather than that a
// function was called.
func (s *wardenPlaneSet) namespaces() []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.workers))
	for ns := range s.workers {
		out = append(out, ns)
	}
	sort.Strings(out)
	return out
}

// --- retiring a Machine --------------------------------------------------------------------------

// wardenRetire ends one **Machine**'s watcher.
//
// THE HAZARD THIS EXISTS FOR IS THE CERTIFICATE. warden_ca.go: a Warden's certificate lives one year
// and there is no renewal, so the remedy is re-enrolment — which generates a NEW KEY, and the
// Warden's id is derived from its key (`wardenIDFor`). A re-enrolled Machine is therefore a new
// Warden with a new queue and a new workflow, and the OLD workflow keeps arming watches on a queue
// nobody will ever poll again. It backs off to one observation an hour (`unreachableAfter`) so the
// cost is bounded, but it never ends on its own and it will report a live Machine as unreachable
// forever. This is the command that closes it, and the gap is named here rather than discovered.
func wardenRetire(ctx context.Context, c client.Client, wardenID string) error {
	return c.SignalWorkflow(ctx, wardenWorkflowID(wardenID), "", wardenRetireSignal, nil)
}
