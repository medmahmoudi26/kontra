package warden

// warden.go — the **Warden**: one process per **Machine**, and the only thing kontra installs there.
//
// `infra/CONTEXT.md` (as ADR 0037 leaves it): "The one process kontra installs on a **Machine**. It
// reconciles which **Workers** run there against what the control plane asked for, judges their
// health, and streams the **Machine's** panes. It runs no actor code itself."
//
// This slice is the loop and the identity. Not here, and not to be smuggled in: the Warden as a
// blocked Temporal Worker (04), telemetry and panes (05), namespace-scoped credentials (08), Leases
// (09). What is here is the shape those four attach to.
//
// ═══ FOUR THINGS THAT ARE NOT NEGOTIABLE, AND WHERE EACH ONE LIVES ═══
//
//  1. THE WARDEN RUNS NO ACTOR CODE. Nothing in this file loads, links or evaluates anything an
//     actor author wrote. Every **Worker** is another process or another container, reached only
//     through the four verbs of `workerDriver`, and every error one of them produces is a value this
//     loop logs and continues from. warden_test.go proves it with a **Worker** that raises SIGSEGV:
//     the loop restarts it and goes on to reconcile a second, healthy one. The systemd unit carries
//     the other half of the same property — see `wardenUnit`, and in particular `OOMPolicy`, which
//     defaults to a setting that stops the Warden when a Worker is OOM-killed.
//
//  2. IT RECONCILES DESIRED STATE, NOT COMMANDS. There is no start message and no stop message. The
//     Controller answers with the LIST of Workers that should be running; anything else running is
//     stopped and anything missing is started. A Warden that was killed, or a **Machine** that was
//     rebooted, asks the same question and gets the same answer as one that was not — nothing is
//     replayed because nothing was ever sent.
//
//  3. WHAT IS ACTUALLY RUNNING COMES FROM `list()`, NEVER FROM A FILE. driver.go calls a driver's
//     memory of what it started "the single most common bug in this class of software", and this
//     loop inherits the rule one level up: it keeps no record of the Workers it started, so a Worker
//     an operator started by hand, or one left behind by the PREVIOUS Warden on this Machine, is
//     seen and reconciled rather than duplicated. The state directory holds an identity and nothing
//     else, and warden_test.go asserts that after a reconcile has run.
//
//  4. IT MAY RESTART A WORKER; IT MAY NOT REPLACE A MACHINE. ADR 0037 settles that half of the
//     repair model: "Destroying and re-provisioning is the control plane's act, because it costs
//     money and because a **Machine** that judges itself unfit is not the thing to trust with the
//     decision." There is therefore no provider call, no cloud credential and no teardown verb in
//     this file, and a Warden that decided it was sick has exactly one thing it can do about it,
//     which is say so.
//
// ═══ OUTBOUND ONLY ═══
//
// A **Machine** accepts no inbound connection. Every arrow is dialled by it (ADR 0037's diagram),
// which is what makes NAT, a private VPC and a customer's firewall work unchanged, and it is why the
// assignment is FETCHED rather than pushed. This file opens no listener; warden_test.go asserts that
// against `/proc/net/tcp`, with a positive control so the assertion is not vacuously true.
//
// ═══ THE ORPHAN THIS LOOP MUST NOT CREATE ═══
//
// Slice 01 found sixteen handler processes on this box with `ppid=1`, up to six days old, still
// polling live queues with no actor half. The cause is two facts meeting: `go run .` COMPILES AND
// THEN RUNS A SECOND BINARY, and `exec.CommandContext` SIGKILLs only the process it started. Kill
// `go run` and its child survives, reparented to init, holding a Temporal lease nobody is watching.
//
// Two rules here, and each one is tested:
//
//   - `start` RUNS UNDER A CONTEXT THAT CAN NEVER BE CANCELLED, and it takes no context argument so
//     that it cannot be given one. Cancelling the loop — Ctrl-C, `systemctl stop` — must not SIGKILL
//     a half mid-launch, because that is precisely the moment `go run` is between compiling and
//     exec'ing. Every other verb takes the loop's context, because a slow `list` or a slow `stop`
//     SHOULD be interruptible. `start` is different in kind and `w.start`'s own header says why: a
//     driver's start context is the WORKER'S LIFETIME, not the call's timeout.
//   - EVERY STOP TARGETS THE HANDLE `list()` REPORTED, never one remembered from `start`. After
//     `go run` exits, the pid it returned names a dead process and the one still serving the queue is
//     its child; the process driver's `list` already reports that child as the half's root, so a stop
//     driven by a listed handle reaches it and a stop driven by a remembered one does not.
//
// The second rule is not a special case for `go run`; it falls straight out of rule 3 above. That is
// the argument for the rule: a loop that reads the runtime cannot orphan a process it cannot see.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/medmahmoudi26/kontra/cli/internal/cliio"
	"github.com/medmahmoudi26/kontra/cli/internal/cliutil"
	"github.com/medmahmoudi26/kontra/cli/internal/config"
	"github.com/medmahmoudi26/kontra/cli/internal/trustpolicy"
	"go.temporal.io/sdk/client"
)

// --- where a Warden keeps itself ------------------------------------------------------------------

// wardenStateDefault is where an installed Warden lives. `/var/lib/` because this is a system daemon
// with a persistent identity, which is exactly what that directory is for — not `KONTRA_HOME`, which
// is a human's installation of the CLI and is per-user.
const wardenStateDefault = "/var/lib/kontra/warden"

// wardenStateEnv lets a test, a container or a second Warden on one box say otherwise.
const wardenStateEnv = "KONTRA_WARDEN_STATE"

func wardenState(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if v := os.Getenv(wardenStateEnv); v != "" {
		return v
	}
	return wardenStateDefault
}

// wardenCAEnv and wardenCADir are the CONTROLLER's side. Under `cliutil.KontraRoot()` because the Fleet CA
// belongs to an installation of kontra rather than to a system daemon, and because the Controller is
// where `.kontra/` already holds this installation's secrets.
const wardenCAEnv = "KONTRA_WARDEN_CA"

func wardenCADir(flagValue string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	if v := os.Getenv(wardenCAEnv); v != "" {
		return v, nil
	}
	root, err := cliutil.KontraRoot()
	if err != nil {
		return "", fmt.Errorf("no place to keep the Fleet CA (set %s): %w", wardenCAEnv, err)
	}
	return filepath.Join(root, "warden-ca"), nil
}

// --- the loop's tuning ----------------------------------------------------------------------------

const (
	// wardenInterval is how often the loop turns. Five seconds is the reconcile latency an operator
	// feels after a `place()`, and it costs one HTTPS GET and one `podman ps` — a scale this loop
	// pays with nothing, unlike the ~20 Temporal events per tick that ADR 0037 measures and refuses
	// for the SAME loop once it is a workflow (slice 04). The two are not in tension: what is cheap
	// against a socket is expensive against a durable history.
	wardenInterval = 5 * time.Second

	// wardenDrain is how long a Worker gets to leave on its own. serve.go's value, for the same
	// reason: a handler that is killed mid-Batch holds its Temporal lease until it times out.
	wardenDrain = 10 * time.Second

	// wardenBackoffMin/Max bound how fast a crash-looping Worker is restarted.
	wardenBackoffMin = 1 * time.Second
	wardenBackoffMax = 5 * time.Minute

	// wardenStable is how long a Worker must be whole before its backoff is forgiven. Without it a
	// Worker that survives one turn and dies on the next resets to a one-second retry forever, which
	// is a crash loop with extra steps.
	wardenStable = 2 * time.Minute
)

// --- the Warden -----------------------------------------------------------------------------------

// warden is the reconcile loop and everything it needs.
//
// `now` and `out` are fields so warden_test.go can drive the backoff without sleeping and read what
// the loop decided. `desired` is the ONLY thing carried between turns and it is the DESIRED state,
// never the actual — see `run` for why holding it is what makes an unreachable Controller
// non-destructive, and why it is deliberately not persisted.
type warden struct {
	id     *wardenIdentity
	driver workerDriver
	http   *http.Client

	interval time.Duration
	out      io.Writer
	now      func() time.Time

	desired    []Spec
	hasDesired bool

	// desiredEgress is the assignment's half of the egress policy, carried beside `desired` and for the
	// same reason: it is DESIRED state, it is held across a failed fetch, and it is not persisted. See
	// warden_egress.go for the floor it cannot switch off.
	desiredEgress egressPolicy

	// egress installs that policy in the HOST's nftables, where the workload cannot reach it. Nil on a
	// Warden with no host to enforce on — the `process` driver, which has no container and therefore no
	// traffic to attach a rule to, and every test in this package that builds a Warden by hand.
	egress *machineEgress

	// watch is where slice 04's BLOCKED watcher reads this loop's decisions, and it is nil on a
	// **Warden** with no control plane — an airgapped install, and every test in warden_test.go.
	// warden_workflow.go holds the whole of it; what this loop owes it is one call per turn, and
	// every method on it is nil-safe so that a loop nobody is watching runs exactly as it did.
	watch *wardenWatchpoint

	// restarts is a rate limit on THIS WARDEN'S OWN ACTIONS, and it is not a record of what is
	// running — nothing is read out of it to answer "is this Worker up". It dies with the process,
	// which is right: a Machine that has just rebooted should retry immediately, not serve out a
	// backoff earned by a previous life.
	restarts map[string]*wardenRestart

	// health judges a Worker that is running and not working — the duty ADR 0037 retires the
	// Watchdog INTO this process to take. Nil-able: a Warden with no judge reconciles exactly as it
	// did before, which is what every test written before slice 05 asserts. See sickworker.go, and
	// in particular why its evidence is not `logs()`.
	health *WorkerHealth

	// THE OUTBOUND SNAPSHOT IS GONE WITH THE MONITOR. A `*PaneReporter` sat here and POSTed this
	// Machine's telemetry and a screenful per Worker, once per interval, to the only thing that ever
	// read it: the wall. `health` above is untouched — it is the reconcile loop's evidence
	// (sickworker.go), not a report — and a droplet's CPU and memory still reach VictoriaMetrics
	// through vmagent, which was always the durable path. A docker Fleet has no vmagent, so a local
	// Warden's Machine telemetry is now unreported; that is a gap to close with an agent, not by
	// reviving a POST whose only consumer was a terminal.
}

type wardenRestart struct {
	n          int
	at         time.Time
	wholeSince time.Time
}

func (w *warden) logf(format string, args ...any) {
	if w.out == nil {
		return
	}
	fmt.Fprintf(w.out, "%s "+format+"\n", append([]any{w.now().UTC().Format(time.RFC3339)}, args...)...)
}

// run is the loop. It returns only when ctx is done.
//
// THE ASSIGNMENT IS HELD ACROSS A FAILED FETCH, and this is the design decision in the loop. A
// Controller that is briefly unreachable — a restart, a network partition, a rolling upgrade — must
// not cause a **Machine** to tear its Workers down: the desired state did not change, only the
// ability to ask did, and a loop that reconciled against an empty answer would turn every blip into
// a Fleet-wide outage.
//
// AND IT IS NOT PERSISTED, which is the other half of the same decision. A Warden that restarts with
// an unreachable Controller holds NOTHING, because a desired state read off a disk is a memory of
// what somebody wanted once, and starting a stranger's code on the strength of it is the one thing
// a Machine should not do while it cannot ask.
//
// THE EGRESS POLICY IS INSTALLED WHETHER OR NOT THERE IS AN ASSIGNMENT, and that is the third half of
// the same decision. A Warden that waited for one would leave a Machine unprotected for exactly as long
// as its Controller was unreachable — which is the moment a Worker left behind by the PREVIOUS Warden is
// still running and nobody is watching. So a Machine that has never been told anything still gets the
// floor and the private default-deny, with an EMPTY policy; the assignment only ever carves holes.
func (w *warden) run(ctx context.Context) error {
	for {
		if got, err := w.fetchAssignment(ctx); err != nil {
			if ctx.Err() != nil {
				w.stopDockerWorkers()
				return nil
			}
			if w.hasDesired {
				w.logf("the Controller did not answer (%v) — holding the last assignment, %d Worker(s)", err, len(w.desired))
			} else {
				w.logf("the Controller did not answer (%v) — and this Warden has never had an assignment, "+
					"so it is holding nothing", err)
			}
		} else {
			w.desired, w.hasDesired = w.workersOf(got), true
			w.desiredEgress = got.Egress
		}
		if w.hasDesired {
			// `reconcile` applies the policy itself, before it starts anything.
			w.reconcile(ctx, w.desired)
		} else {
			// NO ASSIGNMENT IS STILL A TURN THE POLICY IS INSTALLED ON, and it RETRIES: an `nft` that
			// was missing on the first turn must not leave the Machine unprotected forever because
			// nothing ever called this again.
			w.applyEgress(ctx)
		}
		select {
		case <-ctx.Done():
			w.stopDockerWorkers()
			return nil
		case <-time.After(w.interval):
		}
	}
}

// stopDockerWorkers tears down sibling containers this Warden started. Docker Workers are not
// child processes: Pulumi destroying the Warden container would otherwise leave them polling.
// process and podman keep the systemd leak-on-restart contract (KillMode=process).
func (w *warden) stopDockerWorkers() {
	if w.driver == nil || w.driver.driverName() != "docker" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), wardenDrain+5*time.Second)
	defer cancel()
	hs, err := w.driver.list(ctx)
	if err != nil {
		w.logf("could not list Workers on shutdown: %v", err)
		return
	}
	for _, h := range hs {
		w.logf("stopping %s (this Warden is stopping)", h.id())
		if err := w.driver.Stop(ctx, h, 0); err != nil {
			w.logf("could not stop %s: %v", h.id(), err)
		}
	}
}

// assignment asks the Controller what should be running here.
//
// ONE OUTBOUND GET, AUTHENTICATED BY THE CERTIFICATE ENROLMENT PRODUCED. There is no id in the
// request: the Controller derives it from the key the handshake proved this Machine holds
// (`wardenIDFor`), so a Warden cannot ask for another Warden's assignment even by trying.
func (w *warden) assignment(ctx context.Context) ([]Spec, error) {
	a, err := w.fetchAssignment(ctx)
	if err != nil {
		return nil, err
	}
	return w.workersOf(a), nil
}

// fetchAssignment is the GET and the decode. Split out of `assignment` because the loop needs BOTH
// halves of the answer — the Workers and the egress policy — and a second fetch for the second half
// would be a second answer, from a Controller that may have changed its mind between the two.
func (w *warden) fetchAssignment(ctx context.Context) (wardenAssignment, error) {
	if raw := strings.TrimSpace(os.Getenv("KONTRA_WARDEN_ASSIGNMENT")); raw != "" {
		var a wardenAssignment
		if err := json.Unmarshal([]byte(raw), &a); err != nil {
			return wardenAssignment{}, fmt.Errorf("KONTRA_WARDEN_ASSIGNMENT is not readable: %w", err)
		}
		return a, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(w.id.Record.Controller, "/")+wardenAssignmentPath, nil)
	if err != nil {
		return wardenAssignment{}, err
	}
	resp, err := w.http.Do(req)
	if err != nil {
		return wardenAssignment{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return wardenAssignment{}, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var a wardenAssignment
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&a); err != nil {
		return wardenAssignment{}, fmt.Errorf("the Controller's assignment is not readable: %w", err)
	}
	return a, nil
}

// workersOf is the assignment's Workers, in this Machine's own namespace.
func (w *warden) workersOf(a wardenAssignment) []Spec {
	out := make([]Spec, 0, len(a.Workers))
	for _, aw := range a.Workers {
		// THE NAMESPACE THIS MACHINE'S CREDENTIAL NAMES, PUT ON EVERY WORKER IT STARTS. A Warden
		// scoped to one namespace whose Workers are not is not scoped at all — the Workers are the
		// processes that poll. See `assignedWorker.derivedEnv`.
		out = append(out, aw.spec(w.id.scope.Namespace))
	}
	return out
}

// applyEgress installs the Machine's egress policy, and says so when it cannot.
//
// A FAILURE HERE IS NOT AN EXIT AND IS NOT A TEARDOWN. It is the same shape as every other failure in
// this loop: a log line and a fact the next turn acts on. What it costs is that `reconcile` starts no
// Worker while `enforced()` is false — see there for why that is the only safe direction.
func (w *warden) applyEgress(ctx context.Context) {
	if w.egress == nil {
		return
	}
	if err := w.egress.apply(ctx, w.desiredEgress); err != nil {
		w.logf("could not install this Machine's egress policy: %v", err)
	}
}

// reconcile is ONE TURN: read what is running, stop what should not be, start what should.
//
// AT MOST ONE TRANSITION PER WORKER PER TURN. A half-dead pair is STOPPED here and started on the
// NEXT turn rather than stopped-and-started in one, and that is deliberate: a state machine that only
// moves one step per observation is one whose every step was justified by something it actually saw.
// The alternative — stop, then immediately start — acts on the assumption that the stop took, which
// is the assumption `list()` exists to remove. The cost is one interval of extra downtime on a
// repair; the interval is five seconds.
//
// STOPS RUN BEFORE STARTS, because a **Machine** is finite. Slice 11 packs several Workers onto one,
// and a turn that started the new before releasing the old would need capacity for both.
func (w *warden) reconcile(ctx context.Context, desired []Spec) {
	// THE POLICY IS RE-APPLIED BEFORE ANYTHING IS STARTED, EVERY TURN. It is desired state exactly like
	// the Worker list, and the same argument applies: re-deriving it from what the Controller currently
	// says is what makes an operator's edit take effect, and what puts the table back if somebody
	// deleted it by hand. The apply is one atomic `nft -f`, so there is no instant in the middle of it
	// where a Machine is unprotected.
	w.applyEgress(ctx)
	actual, err := w.driver.list(ctx)
	if err != nil {
		// AN UNREADABLE RUNTIME STOPS NOTHING. "podman is not answering" and "there are no Workers"
		// are the same empty list to a loop that guessed, and guessing wrong here tears down a
		// healthy Machine.
		w.logf("cannot read what is running (%v) — nothing was changed this turn", err)
		return
	}
	// THE ONE LINE SLICE 04 ADDED TO THIS LOOP. It hands the turn's two facts — what was asked for
	// and what is actually there — to the blocked watcher, which DIFFS them against the previous
	// turn and records an event only when they differ. Two identical turns produce nothing, which is
	// what makes a watcher blocked on it cost zero at rest; see warden_workflow.go for the numbers.
	// Placed BEFORE anything is stopped or started, so a decision is about what this Warden found
	// rather than about what it then did.
	w.watch.saw(desired, actual)
	have := make(map[string]workerHandle, len(actual))
	for _, h := range actual {
		have[h.id()] = h
	}
	want := make(map[string]Spec, len(desired))
	order := make([]string, 0, len(desired))
	for _, s := range desired {
		id := s.Name + "@" + s.Version
		if _, dup := want[id]; dup {
			// TWO ENTRIES FOR ONE WORKER IS AN ASSIGNMENT BUG AND IT IS SAID OUT LOUD. Silently
			// taking the last one would make a Fleet run something nobody can find in the file.
			w.logf("the assignment names %s twice; using the first", id)
			continue
		}
		want[id] = s
		order = append(order, id)
	}

	for _, id := range sortedKeysOf(have) {
		if _, ok := want[id]; ok {
			continue
		}
		w.Stop(ctx, have[id], "it is not in the assignment")
	}

	for _, id := range order {
		h, running := have[id]
		switch {
		case running && h.whole():
			w.settled(id)
			// A WHOLE WORKER IS NOT NECESSARILY A WORKING ONE, and this is the case the Watchdog
			// existed for: the round-3 Machine that failed 81 of 82 resource loads with both halves
			// up and its poller live, while the run reported `completed`. `whole()` cannot see it —
			// nothing about a process table can — so the judgement is a separate reading, and it is
			// the Warden's because the Warden is what can act on it (ADR 0037).
			//
			// SICK IS A STOP, NOT A RESTART, which keeps this inside the rule the rest of this
			// function obeys: at most one transition per Worker per turn. The Worker is still in the
			// assignment, so the next turn starts it, through the same backoff a crash goes through
			// — a Worker that is sick every five minutes is restarted with the same diminishing
			// patience as one that dies every five minutes, and for the same reason.
			if v := w.Judge(ctx, want[id], h); v.Sick() {
				w.Stop(ctx, h, v.Detail)
			}
		case running:
			// THE PAIR IS THE UNIT (driver.go), so half of one is not a Worker that is half up, it is
			// a Worker that is down and holding a queue. serve.go: "A half-dead worker is worse than
			// a dead one: it keeps its Temporal lease and units time out one by one."
			w.Stop(ctx, h, "only its "+halvesOf(h)+" half is running, and the pair is the unit")
		default:
			// ═══ NOTHING STARTS UNTIL THE EGRESS POLICY IS IN THE KERNEL ═══
			//
			// This is the one place the loop refuses to do its job, and it is the direction that fails
			// safe. A Worker started before the policy is installed is a stranger's code with the
			// Machine's whole network reachable — the VPC, every other Machine on it, and the cloud
			// metadata service that hands out this Machine's own credentials. Measured on this
			// Controller with no policy installed: a container with `--cap-drop ALL` reached
			// 169.254.169.254 and the Machine's unauthenticated Redis on the first try.
			//
			// IT DOES NOT STOP WHAT IS ALREADY RUNNING, and the asymmetry is deliberate. This file never
			// removes its rules, so a Warden that installed a policy and later lost the ability to
			// re-install one is still enforcing the last one it managed; tearing the Machine down on a
			// transient `nft` failure would turn a re-apply blip into a Fleet outage, which is the
			// same mistake as reconciling against an unreachable Controller's empty answer.
			if w.egress != nil && !w.egress.enforced() {
				w.logf("not starting %s: %v, and starting a Worker before it is would put a stranger's "+
					"code on this Machine's network with nothing bounding it", id, errEgressUnenforceable)
				continue
			}
			w.start(want[id])
		}
	}
}

// halvesOf names which half survived, for the log line that says why a Worker is being stopped.
func halvesOf(h workerHandle) string {
	names := make([]string, 0, len(h.Halves))
	for _, part := range workerParts {
		if _, ok := h.half(part); ok {
			names = append(names, string(part))
		}
	}
	if len(names) == 0 {
		return "no"
	}
	return strings.Join(names, "+")
}

// sortedKeysOf makes a turn's order deterministic. Go randomises map iteration on purpose, and a
// reconcile that stopped Workers in a different order every turn would make two identical runs
// produce two different logs — which is the difference between a log you can diff and one you cannot.
func sortedKeysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// stop ends a Worker. THE HANDLE IS THE ONE `list` RETURNED — see this file's header on the orphan
// this rule prevents.
func (w *warden) Stop(ctx context.Context, h workerHandle, why string) {
	w.logf("stopping %s (%s)", h.id(), why)
	if err := w.driver.Stop(ctx, h, wardenDrain); err != nil {
		// A FAILED STOP IS NOT FATAL AND IS NOT RETRIED HERE. The next turn lists again; if the
		// Worker is still there it is still not in the assignment and this runs again, which is a
		// retry driven by an observation rather than by a counter.
		w.logf("could not stop %s: %v", h.id(), err)
	}
}

// start runs a Worker, unless this Warden has been restarting it too fast.
//
// ═══ IT TAKES NO CONTEXT, AND THAT IS THE POINT ═══
//
// A `workerDriver`'s `start` context is NOT a call timeout. driver_process.go states what it actually
// is: "THE ctx OWNS THE FOREGROUND CHILDREN. `exec.CommandContext` kills them when it is cancelled."
// So a context handed to `start` is the WORKER'S LIFETIME under the `process` driver, and any
// cancellation of it — the loop shutting down, a `defer cancel()`, a deadline — kills the pair.
//
// This function had a `context.WithTimeout(context.WithoutCancel(ctx), 15*time.Minute)` and a
// `defer cancel()`, which is the obvious way to write it and is wrong twice over. The `defer cancel()`
// killed every Worker the instant `start` returned — caught by
// TestWardenOpensNoListeningSocket, which is a test about something else entirely and only
// noticed because it happened to poll `list` after the killer goroutine had run rather than
// before. And even without the defer, a fifteen-minute deadline would SIGKILL a perfectly healthy
// Worker fifteen minutes into its life.
//
// So the context is `context.Background()` and this function does not accept one, because a
// signature that cannot be passed a context cannot be given a cancellable one by the next reader.
//
// WHAT THAT COSTS is that a wedged `podman run` blocks the turn, and it is the right trade rather
// than a hole: ADR 0036 says a cold Machine pulls a ~900 MB image before it can work, so a deadline
// here is either shorter than a legal pull or long enough to protect nothing. A Warden blocked in
// `start` is alive, is supervised, tears nothing down, and resumes reconciling when the call returns.
func (w *warden) start(spec Spec) {
	id := spec.Name + "@" + spec.Version
	// THE ASSIGNMENT IS THE ONE INPUT NOBODY ON THIS MACHINE TYPED, so it is checked before a driver
	// is handed it — see `wellFormed` for the two failures that are worse than "it did not run".
	// Refused BEFORE the backoff, because a refusal is not an attempt and counting it as one would
	// make a malformed assignment go quiet after a few minutes.
	if err := w.wellFormed(spec); err != nil {
		w.logf("refusing to start %s: %v", id, err)
		return
	}
	r := w.restart(id)
	if wait := w.backoff(r); wait > 0 {
		w.logf("%s is missing; not restarting for another %s (restart %d)", id, wait.Truncate(time.Second), r.n)
		return
	}
	r.n++
	r.at = w.now()
	r.wholeSince = time.Time{}
	w.logf("starting %s (restart %d)", id, r.n)

	// THE UNCANCELLABLE CONTEXT — the first of the two orphan rules in this file's header, and see
	// this function's own header for what a cancellable one does instead. SIGKILL delivered between
	// `go run`'s compile and its exec is exactly how a handler ends up with `ppid=1` and no actor
	// half; a context that can never be cancelled is the only shape that cannot deliver one.
	if _, err := w.driver.Start(context.Background(), spec); err != nil {
		// A WORKER THAT WILL NOT START IS A LOG LINE, NOT AN EXIT. An unpullable image, an
		// unrepresentable name (driver_podman.go), a Machine with no subuid allocation — every one
		// of those is a fact about one Worker, and a Warden that returned on any of them would take
		// every OTHER Worker on the Machine down with it.
		w.logf("could not start %s: %v", id, err)
	}
}

// wellFormed refuses an assignment entry that would not survive contact with a driver.
//
// THE CONTROL PLANE IS THE ONE PLACE A SPEC COMES FROM THAT NOBODY TYPED. `kontra serve` builds its
// Spec in the same process, from a manifest it just read; a **Warden**'s comes over the wire
// from a Controller across an organisational boundary (ADR 0037). So this is the boundary where a
// spec stops being trusted, and both refusals below are for failures that were reachable from a JSON
// file and are not reachable from serve.go.
//
//  1. A LABEL THAT DOES NOT ROUND-TRIP IS AN INVISIBLE WORKER, AND AN INVISIBLE WORKER IS AN
//     INFINITE LOOP. driver.go states the gap and declines to guard it: "WHAT IT CANNOT REPRESENT is
//     a VERSION containing a separator — `n/1/2/actor` reads back as name `n/1`, version `2`. That is
//     stated rather than guarded because … a guard would have to be a refusal at `start`, which would
//     stop serving an actor that serves today." That reasoning is right for `kontra serve` and does
//     not transfer here: nothing is serving yet, and the consequence is not cosmetic. Such a Worker
//     STARTS, is then absent from `list()` under the id the loop asked for, and is started AGAIN
//     every five seconds — a Machine filling with rival pollers on one queue, which is the exact
//     failure the seam exists to prevent.
//
//     The check is the round trip itself rather than a character class, because the round trip is the
//     question that matters and a character class would be a second, drifting copy of the parse. It
//     also passes `a/b`, which `shared/conformance/queues.json` carries as an adversarial actor name that
//     must keep working — a slash in the NAME round-trips; a slash in the VERSION does not.
//
//  2. A HALF WITH NO COMMAND IS A PANIC IN THE PROCESS DRIVER. `driver_process.go:start` indexes
//     `p.Argv[0]` unconditionally, which is safe for its only previous caller and is an index out of
//     range for an assignment that omitted the argv — a crash of the Warden, restarted by systemd,
//     fetching the same assignment and crashing again. Under `podman` an empty argv is legitimate and
//     means "the image's entrypoint is the spec" (driver_podman.go), which is why this refusal is the
//     one place in the loop that asks which driver it is holding.
func (w *warden) wellFormed(spec Spec) error {
	name, version, part, ok := parseWorkerLabel(workerLabel(spec.Name, spec.Version, partActor))
	if !ok || name != spec.Name || version != spec.Version || part != partActor {
		return fmt.Errorf("%q@%q cannot be labelled and read back, so `list` would never report it "+
			"and this Warden would start another one every turn — a version may not contain %q, and "+
			"neither field may be empty (driver.go, parseWorkerLabel)",
			spec.Name, spec.Version, workerLabelSep)
	}
	if w.driver.driverName() != "process" {
		return nil
	}
	for _, h := range []struct {
		part workerPart
		p    ProcSpec
	}{{partActor, spec.Actor}, {partHandler, spec.Handler}} {
		if len(h.p.Argv) == 0 {
			return fmt.Errorf("its %s half has no command, and the `process` driver has no image to "+
				"take an entrypoint from — an assignment for `process` states both halves' argv "+
				"(under `podman` an empty argv means the image's own entrypoint)", h.part)
		}
	}
	return nil
}

func (w *warden) restart(id string) *wardenRestart {
	if w.restarts == nil {
		w.restarts = map[string]*wardenRestart{}
	}
	if r, ok := w.restarts[id]; ok {
		return r
	}
	r := &wardenRestart{}
	w.restarts[id] = r
	return r
}

// backoff is how long is left before this Worker may be started again. Exponential from one second,
// capped at five minutes — the cap because an unpullable image should be retried forever (the
// registry may come back) but not more often than it costs to ask.
func (w *warden) backoff(r *wardenRestart) time.Duration {
	if r.n == 0 {
		return 0
	}
	delay := wardenBackoffMin << (r.n - 1)
	if delay > wardenBackoffMax || delay <= 0 {
		delay = wardenBackoffMax
	}
	if elapsed := w.now().Sub(r.at); elapsed < delay {
		return delay - elapsed
	}
	return 0
}

// settled forgives a Worker's restart count once it has been WHOLE for long enough to have earned it.
// Measured from the first turn it was seen whole, not from the start, because a Worker that takes a
// minute to boot has not been up for a minute.
func (w *warden) settled(id string) {
	r := w.restart(id)
	if r.n == 0 {
		return
	}
	if r.wholeSince.IsZero() {
		r.wholeSince = w.now()
		return
	}
	if w.now().Sub(r.wholeSince) >= wardenStable {
		r.n = 0
		r.at = time.Time{}
	}
}

// --- the command ------------------------------------------------------------------------------------

func Command(args []string) error {
	if len(args) == 0 {
		wardenUsage()
		return errors.New("warden: expected a subcommand")
	}
	switch args[0] {
	case "join":
		return wardenJoin(args[1:])
	case "serve":
		return wardenServe(args[1:])
	case "status":
		return wardenStatus(args[1:])
	case "ca":
		return wardenCACmd(args[1:])
	case "help", "-h", "--help":
		wardenUsage()
		return nil
	default:
		wardenUsage()
		return fmt.Errorf("unknown warden subcommand %q", args[0])
	}
}

func wardenUsage() {
	fmt.Fprint(cliio.Stdout, `kontra warden — the one process kontra installs on a Machine (ADR 0037)

ON A MACHINE
  kontra warden join --controller https://<controller>:8443 --token <kw1....>
               # exchange a one-time token for an mTLS identity that persists, then install
               # and start kontra-warden.service. The token carries the Fleet CA's fingerprint,
               # so the secret is never sent to a Controller that cannot prove it holds that CA.
               [--state <dir>]        # default `+wardenStateDefault+`
               [--no-systemd]         # enrol only; print the unit instead of installing it
  kontra warden serve [--state <dir>] [--driver podman|docker|process] [--local] [--interval 5s]
               # the reconcile loop: fetch the assignment, compare it to what the runtime
               # actually holds, start and stop the difference. Outbound only; no listener.
               # Also attaches this Machine's BLOCKED watcher to the control plane its
               # enrolment named, so its lifecycle lands in the Transcript.
  kontra warden status [--state <dir>]
               # who this Machine is, when its certificate expires, what is running

ON THE CONTROLLER
  kontra warden ca serve [--dir <dir>] [--listen :8443] [--san <host-or-ip>]...
               # enrolment and assignment for this Fleet. Creates the CA on first use.
               # Also EXECUTES every TENANT's Machines' watchers, on the `+wardenPlaneQueue+`
               # queue IN THAT TENANT'S NAMESPACE — one worker per tenant, because a queue
               # name is namespace-relative and a client is bound to one namespace.
               [--temporal <addr>]    # the address MACHINES reach Temporal on, handed to each
                                      # one at enrolment. Empty records no lifecycle.
               [--temporal-tls]       # that Temporal requires client certificates, so each
                                      # Machine offers the identity this CA issued it
               [--no-plane]
  kontra warden ca token  [--dir <dir>] [--ttl 1h] [--tenant <name>]
               # mint ONE one-time enrolment token, FOR ONE TENANT. A tenant IS a Temporal
               # namespace (ADR 0036) and it is fixed here, at the one moment a human knows
               # which customer the Machine belongs to — nothing the Machine sends can
               # change it. Default: `+wardenDefaultTenant()+`
  kontra warden ca list   [--dir <dir>]                # the CA, its tenants, and every token
  kontra warden ca retire <warden-id> [--tenant <name>]
                                                       # stop watching one Machine (see
                                                       # warden_workflow.go: a re-enrolled
                                                       # Machine is a NEW Warden, and the old
                                                       # one's watcher outlives it)

ASSIGNMENTS ARE PER TENANT
  <dir>/assignments/<tenant>/default.json      every Machine of that tenant
  <dir>/assignments/<tenant>/<warden-id>.json  one Machine, overriding the above

  An assignment also carries this Machine's EGRESS POLICY, which the Warden installs as an
  nftables ruleset in the Machine's own network namespace — outside every container, where a
  Worker cannot reach it (cli/warden/warden_egress.go):

    { "workers": [...],
      "egress": { "allow": ["10.20.0.0/16"], "deny": ["203.0.113.4"] } }

  RFC1918 is DENIED unless "allow" names it; the public internet is allowed unless "deny"
  names it; the cloud metadata service (169.254.0.0/16) is denied and "allow" cannot say
  otherwise. This Machine's Controller, its Temporal and its resolvers are allowed without
  being written down. Entries are addresses or CIDRs, and a malformed one refuses the whole
  policy — after which this Warden starts no Worker at all.
`)
}

// --- join ---------------------------------------------------------------------------------------

func wardenJoin(args []string) error {
	fs := flag.NewFlagSet("warden join", flag.ContinueOnError)
	controller := fs.String("controller", "", "the Controller's enrolment URL, https://<host>:8443")
	token := fs.String("token", "", "a one-time enrolment token (`kontra warden ca token`)")
	state := fs.String("state", "", "where to keep this Machine's identity (default "+wardenStateDefault+")")
	noSystemd := fs.Bool("no-systemd", false, "enrol only; print the unit instead of installing it")
	unitDir := fs.String("unit-dir", "/etc/systemd/system", "where the unit is written")
	driver := fs.String("driver", "podman", "which runtime the installed unit reconciles through: podman|process")
	trust := wardenTrustFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *controller == "" || *token == "" {
		return errors.New("warden join needs --controller and --token")
	}
	// THE POLICY IS JUDGED BEFORE THE TOKEN IS SPENT. An enrolment token is one-time (`wardenCAToken`),
	// so a `join` that enrols and THEN discovers `--trust-identity` has no `--trust-issuer` has cost
	// the operator a token to learn about a typo — and the message four lines below says in capitals
	// not to re-run `join`. Nothing about the network changes this verdict, so nothing about it waits
	// for the network.
	policy, err := trustpolicy.Load(trust.value())
	if err != nil {
		return err
	}
	dir := wardenState(*state)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("cannot create %s (a Warden's identity lives there; run as root, or pass --state / set %s): %w",
			dir, wardenStateEnv, err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	id, err := enrol(ctx, dir, *controller, *token)
	if err != nil {
		return err
	}
	fmt.Fprintf(cliio.Stdout, "enrolled as %s with %s\n  identity: %s\n  expires:  %s\n",
		id.Record.WardenID, id.Record.Controller, wardenIdentityDir(dir), id.Record.NotAfter.Format(time.RFC3339))

	// WHAT THIS MACHINE WILL BE ALLOWED TO RUN, AT THE MOMENT IT BECOMES A MACHINE. An empty policy is
	// legal and enrols fine; it also places nothing, and an operator who reads that here fixes it now
	// rather than in an hour, from a Fleet that looks idle.
	for _, line := range policy.Describe() {
		fmt.Fprintf(cliio.Stdout, "  trust:    %s\n", line)
	}
	if len(policy.Rules) == 0 && *driver != "process" {
		fmt.Fprintf(cliio.Stdout, "  NOTE:     this Machine's registry allowlist is empty, so its Warden will refuse "+
			"every placement.\n            Re-running `warden join` is NOT the fix and would spend another "+
			"token — edit\n            ExecStart in the unit below, or pass --trust-registries "+
			"<host[:port][/path]>.\n")
	}

	unit := wardenUnit(dir, *driver, trust.value())
	if *noSystemd {
		fmt.Fprintf(cliio.Stdout, "\n--no-systemd: nothing is supervising this Warden. The unit it would have "+
			"written to %s/%s is:\n\n%s\n", *unitDir, wardenUnitName, unit)
		return nil
	}
	// A FAILED INSTALL IS NOT A FAILED ENROLMENT. The identity is already on disk and re-running
	// `join` would spend a second token for nothing, so this reports and returns success with the
	// unit printed — the operator has everything they need to finish by hand.
	if err := installWardenUnit(*unitDir, unit); err != nil {
		fmt.Fprintf(cliio.Stdout, "\nenrolment succeeded; installing the systemd unit did not (%v).\n"+
			"  Do NOT re-run `join` — that spends another token and the identity is already in %s.\n"+
			"  Write this to %s/%s and `systemctl enable --now %s`:\n\n%s\n",
			err, dir, *unitDir, wardenUnitName, wardenUnitName, unit)
		return nil
	}
	fmt.Fprintf(cliio.Stdout, "\ninstalled and started %s\n", wardenUnitName)
	return nil
}

// --- the unit -------------------------------------------------------------------------------------

const wardenUnitName = "kontra-warden.service"

// wardenUnit renders the systemd unit that supervises a **Warden**.
//
// Three directives here are not defaults and each one exists because of a specific way a Worker could
// otherwise take the Warden with it:
//
//	OOMPolicy=continue   systemd's DEFAULT is `stop`, which means that when the kernel OOM-kills ANY
//	                     process in this unit's cgroup, systemd stops the UNIT. Under the `process`
//	                     driver a Worker's halves are in that cgroup, so the default turns "an actor
//	                     used too much memory" into "the Warden is gone and nothing will restart it"
//	                     — the exact failure ADR 0037 says must not be possible.
//
//	KillMode=process     the default (`control-group`) SIGKILLs everything in the cgroup on stop, so
//	                     `systemctl restart kontra-warden` would kill every Worker on the Machine and
//	                     the Fleet's work would restart with the supervisor. systemd's manual advises
//	                     against this setting in general and it is correct to in general: it leaks
//	                     processes. Here the leak IS the design — a Warden that comes back adopts
//	                     what is running, because `list()` reads the runtime, so the processes it
//	                     leaves are found rather than lost.
//
//	Restart=always       a Warden that exits for any reason comes back and re-reconciles. Nothing is
//	                     replayed, so there is no state to be lost by dying.
//
// The `--state` and `--driver` are baked in rather than read from an environment file: a unit and an
// EnvironmentFile that disagree is a Warden that enrolled into one directory and serves from another,
// and it fails as "not enrolled" while the identity is right there.
//
// THE TRUST POLICY IS BAKED IN FOR THE SAME REASON, AND ITS ABSENCE WOULD BE WORSE. `warden serve`
// defaults every trust flag to an environment variable, and systemd starts a unit with an almost
// empty environment — so a unit that did not carry the policy would hand the Warden an empty
// allowlist on every boot, however the operator had configured their shell. The policy would silently
// change meaning between `kontra warden serve` typed by hand and the same command under systemd,
// which is exactly the disagreement this paragraph already refuses for `--state`.
//
// EMPTY VALUES ARE OMITTED, not written as `--trust-key ""`, so the ExecStart line reads as the
// decision that was made. Each value is systemd-quoted because a key path may contain a space.
func wardenUnit(state, driver string, trust trustpolicy.Options) string {
	exe, err := os.Executable()
	if err != nil || exe == "" {
		exe = "/usr/local/bin/kontra"
	}
	flags := ""
	for _, kv := range [][2]string{
		{"trust-registries", trust.Registries},
		{"trust-unsigned", trust.Unsigned},
		{"trust-key", trust.Key},
		{"trust-identity", trust.Identity},
		{"trust-issuer", trust.Issuer},
	} {
		if v := strings.TrimSpace(kv[1]); v != "" {
			flags += fmt.Sprintf(" --%s %q", kv[0], v)
		}
	}
	return `[Unit]
Description=kontra Warden (reconciles this Machine's Workers)
Documentation=https://github.com/medmahmoudi26/kontra/blob/main/docs/adr/0037-the-warden-and-the-fleet-as-capacity.md
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=` + exe + ` warden serve --state ` + state + ` --driver ` + driver + flags + `
Restart=always
RestartSec=5

# The Warden runs no actor code, and these two keep that true when a Worker misbehaves.
# See cli/warden/warden.go:wardenUnit for what each default would have done instead.
OOMPolicy=continue
KillMode=process

[Install]
WantedBy=multi-user.target
`
}

// installWardenUnit writes the unit and starts it. Every step is reported by its own error, because
// "systemctl is not here" and "this is not root" send an operator to two different places.
func installWardenUnit(dir, unit string) error {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return errors.New("systemctl is not on this Machine, so nothing here can supervise a Warden")
	}
	path := filepath.Join(dir, wardenUnitName)
	if err := os.WriteFile(path, []byte(unit), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if out, err := exec.Command("systemctl", "daemon-reload").CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %v: %s", err, strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command("systemctl", "enable", "--now", wardenUnitName).CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl enable --now %s: %v: %s", wardenUnitName, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// --- serve ------------------------------------------------------------------------------------------

// wardenTrustFlags declares the five inputs to `cli/internal/trustpolicy/trustpolicy.go` on a flag set, each defaulting to
// its environment variable.
//
// A FUNCTION AND NOT A LITERAL, for `buildFlagSet`'s reason: `cli/fleet_documented_flags_test.go`
// found three flags this repo told people to type that had been gone for months, so a flag set a test
// can enumerate is worth more than a list written beside one.
//
// BOTH CHANNELS, BECAUSE THERE ARE TWO CALLERS AND THEY ARE NOT THE SAME PERSON. An installed Warden
// is a systemd unit and its configuration is `Environment=`; an operator debugging a Machine at 2am
// types flags. A control that could only be set one way would be set neither.
type wardenTrustFlagSet struct {
	registries, unsigned, key, identity, issuer *string
}

func wardenTrustFlags(fs *flag.FlagSet) wardenTrustFlagSet {
	return wardenTrustFlagSet{
		registries: fs.String("trust-registries", cliutil.EnvOr(trustpolicy.RegistriesEnv, ""),
			"registries this Machine may pull Workers from: `host[:port][/path]`, comma-separated. Empty means none"),
		unsigned: fs.String("trust-unsigned", cliutil.EnvOr(trustpolicy.UnsignedEnv, ""),
			"registries from the list above whose images are accepted UNSIGNED (per registry; there is no global off)"),
		key: fs.String("trust-key", cliutil.EnvOr(trustpolicy.KeyEnv, ""),
			"cosign public key every Artifact must be signed by"),
		identity: fs.String("trust-identity", cliutil.EnvOr(trustpolicy.IdentityEnv, ""),
			"keyless: the certificate identity (SAN) allowed to sign, e.g. a CI workflow's URL"),
		issuer: fs.String("trust-issuer", cliutil.EnvOr(trustpolicy.IssuerEnv, ""),
			"keyless: the OIDC issuer that must have vouched for that identity — without it, anyone can obtain one"),
	}
}

func (f wardenTrustFlagSet) value() trustpolicy.Options {
	return trustpolicy.Options{
		Registries: *f.registries, Unsigned: *f.unsigned,
		Key: *f.key, Identity: *f.identity, Issuer: *f.issuer,
	}
}

func wardenServe(args []string) error {
	fs := flag.NewFlagSet("warden serve", flag.ContinueOnError)
	state := fs.String("state", "", "where this Machine's identity lives (default "+wardenStateDefault+")")
	driverName := fs.String("driver", "podman", "which runtime holds the Workers: podman|docker|process")
	interval := fs.Duration("interval", wardenInterval, "how often the loop turns")
	local := fs.Bool("local", false, "mint a self-signed identity (dockerFleet only; not production enrolment)")
	trust := wardenTrustFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	policy, err := trustpolicy.Load(trust.value())
	if err != nil {
		return err
	}
	dir := wardenState(*state)
	var id *wardenIdentity
	if *local {
		id, err = bootstrapLocalIdentity(dir, "", "")
	} else {
		id, err = loadIdentity(dir)
	}
	if err != nil {
		if errors.Is(err, errNotEnrolled) {
			return fmt.Errorf("%w — this Machine has no identity yet:\n"+
				"  kontra warden join --controller https://<controller>:%d --token <kw1....>", err, wardenCADefaultPort)
		}
		return err
	}
	drv, err := wardenDriver(context.Background(), *driverName, policy)
	if err != nil {
		return err
	}
	w := newServeWarden(id, drv, *interval)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	w.logf("warden %s reconciling through the %s driver, against %s every %s",
		id.Record.WardenID, drv.driverName(), id.Record.Controller, *interval)

	// WHAT THIS MACHINE WILL RUN, PRINTED EVERY TIME, INCLUDING WHEN THE ANSWER IS "NOTHING".
	//
	// A trust policy that is not visible is a trust policy nobody can check, and the state this most
	// needs to be legible in is the unconfigured one: an allowlist of nothing admits nothing, so a
	// Warden with no policy places no Workers — which, in a log that did not say so, looks exactly
	// like a Fleet nobody has placed anything on. That indistinguishability is the failure this repo
	// has now found in four surfaces, and it costs two lines to avoid.
	for _, line := range policy.Describe() {
		w.logf("trust: %s", line)
	}
	if drv.driverName() == "process" {
		// SAID OUT LOUD RATHER THAN IMPLIED BY AN EMPTY LINE. `process` runs actor code from source and
		// pulls no image, so neither gate above applies to it at all — that is a true and materially
		// weaker posture, and an operator is entitled to read it rather than infer it from a flag.
		w.logf("trust: the `process` driver runs actor code from source and pulls nothing, so no " +
			"registry allowlist and no signature applies to this Machine")
	}
	go wardenServeAttach(ctx, w, id, drv.driverName())
	return w.run(ctx)
}

// newServeWarden builds the **Warden** `serve` runs.
//
// A FUNCTION RATHER THAN A LITERAL INSIDE `wardenServe`, for one reason worth stating: the watchpoint
// is the field that makes a Machine's lifecycle exist, and a `wardenServe` that forgot to set it
// would leave a Fleet with NO HISTORY AT ALL while every test in this package stayed green — the
// tests build their own Wardens. Found by mutation. This is the one seam a test can hold, and
// warden_workflow_test.go holds it by reconciling through a Warden built here and reading the
// decisions back out.
//
// THE WATCHPOINT IS CREATED WHETHER OR NOT A CONTROL PLANE CAN BE REACHED. `reconcile` reads it on
// every turn, and setting it later from the attach goroutine would be a data race on the loop's hot
// path. Attaching is what retries; observing is not.
func newServeWarden(id *wardenIdentity, drv workerDriver, interval time.Duration) *warden {
	w := &warden{
		id:       id,
		driver:   drv,
		http:     id.client(),
		interval: interval,
		out:      cliio.Stdout,
		now:      time.Now,
		watch:    newWardenWatchpoint(id.Record.WardenID, time.Now),
	}
	// THE JUDGE AND THE REPORTER LIVE HERE FOR THE SAME REASON THE WATCHPOINT DOES, and NOT in the
	// `warden` literal. Every test in this package builds its own `&warden{...}`, and none of them
	// should acquire a thing that scrapes a metrics port or a thing that dials a Controller by
	// accident — a reconcile test asks what the loop does, not what the network says. Setting them
	// in `wardenServe` gave that same property, and cost the one this constructor exists to buy:
	// a `serve` that forgot a field stayed green across the whole suite.
	w.health = NewWorkerHealth(w.now)
	// THE EGRESS POLICY IS BUILT ONLY FOR `podman`, AND THAT IS NOT A CONVENIENCE. warden_egress.go:
	// the `process` driver has no container, no pod network namespace and therefore no traffic to
	// attach a rule to that is not simply the Machine's own — a policy there would govern the Warden,
	// the operator's SSH session and the package manager alongside the actor. ADR 0036 scopes that
	// driver to "the single box and the airgapped install", where the Controller and the operator are
	// the same party. Set HERE and not in `wardenServe` for the reason this constructor exists: a
	// `serve` that forgot the field would leave a Fleet with no egress policy at all while every test
	// in this package stayed green.
	if drv.driverName() == "podman" {
		w.egress = newMachineEgress(id.Record, cliio.Stdout)
	}
	return w
}

// wardenServeAttach keeps a **Warden** attached to the control plane, and keeps reconciling when it
// cannot be.
//
// A CONTROL PLANE THAT WILL NOT ANSWER IS NOT A REASON TO STOP RUNNING WORKERS. warden.go's whole
// first rule about the Controller applies here unchanged: a restart, a partition or a rolling
// upgrade must not change what is running on a **Machine**. So this retries in the background, with
// the same backoff bounds the loop uses on a Worker, and the reconcile loop never waits on it.
//
// WHAT IS LOST WHILE IT IS DETACHED IS SAID OUT LOUD. Decisions queue in the watchpoint's fixed
// buffer while nothing is draining it; an overrun is counted and folded into the next decision that
// gets through (`wardenWatchpoint.emit`), so a gap in a Machine's history is visible in the history
// rather than being a silence indistinguishable from calm.
func wardenServeAttach(ctx context.Context, w *warden, id *wardenIdentity, driverName string) {
	delay := wardenBackoffMin
	for {
		plane, err := wardenAttach(ctx, id, driverName, w.watch, cliio.Stdout)
		switch {
		case err == nil:
			<-ctx.Done()
			plane.close()
			return
		case errors.Is(err, errNoControlPlane):
			// THE ONE FAILURE HERE THAT IS NOT ONE. A Machine enrolled against a Controller with no
			// Temporal address is the airgapped case, and it reconciles exactly as it did in slice
			// 03 — but it says so, once, on the line an operator is already reading. A Fleet whose
			// history is silently not being recorded looks exactly like a Fleet in which nothing has
			// happened, which is the failure this repo has now found in four different surfaces.
			w.logf("this Machine's enrolment names no control plane, so its lifecycle is not being " +
				"recorded anywhere — re-enrol against a Controller started with `kontra warden ca " +
				"serve --temporal <address reachable from this Machine>` to change that")
			return
		}
		if ctx.Err() != nil {
			return
		}
		w.logf("not attached to the control plane (%v) — still reconciling; retrying in %s",
			err, delay.Truncate(time.Second))
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		if delay *= 2; delay > wardenBackoffMax {
			delay = wardenBackoffMax
		}
	}
}

// wardenDriver picks the runtime.
//
// PODMAN IS THE DEFAULT AND `process` IS AN EXPLICIT CHOICE, not a fallback. ADR 0036: there is ONE
// Target and it is a container, and `process` survives as the driver for "the single box and the
// airgapped install". A Warden that quietly degraded to `process` when podman was missing would run a
// stranger's code directly on a Machine, which is the reversal 0036 exists to prevent — arriving
// through a default nobody chose.
//
// THE TRUST POLICY IS PASSED IN AND IS NOT READ HERE, and the `process` case is why it is not simply
// stored on the driver interface: `process` runs an actor FROM SOURCE and pulls nothing, so there is
// no registry to allowlist and no Artifact to have been signed. Handing it a policy would suggest one
// was being enforced. What that costs is stated where it is decided — `wardenServe` prints the policy
// on startup and says plainly that `--driver process` is outside it.
func wardenDriver(ctx context.Context, name string, trust trustpolicy.Policy) (workerDriver, error) {
	switch name {
	case "podman":
		d, err := newPodmanDriver(ctx, trust)
		if err != nil {
			return nil, fmt.Errorf("this Machine cannot run containers, and a Warden will not fall back to "+
				"running a stranger's code directly (ADR 0036: one Target, and it is a container). "+
				"Install podman, or pass `--driver process` if this is the single-box or airgapped case: %w", err)
		}
		return d, nil
	case "docker":
		d, err := newDockerDriver(ctx, trust)
		if err != nil {
			return nil, err
		}
		return d, nil
	case "process":
		return &ProcessDriver{out: cliio.Stdout, err: os.Stderr}, nil
	default:
		return nil, fmt.Errorf("--driver %q: there are three, `podman`, `docker` and `process`", name)
	}
}

// --- status -----------------------------------------------------------------------------------------

func wardenStatus(args []string) error {
	fs := flag.NewFlagSet("warden status", flag.ContinueOnError)
	state := fs.String("state", "", "where this Machine's identity lives (default "+wardenStateDefault+")")
	driverName := fs.String("driver", "podman", "which runtime to ask what is running: podman|process")
	trust := wardenTrustFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	dir := wardenState(*state)
	id, err := loadIdentity(dir)
	if err != nil {
		return err
	}
	fmt.Fprintf(cliio.Stdout, "warden     %s\ncontroller %s\nCA         %s\nenrolled   %s\n",
		id.Record.WardenID, id.Record.Controller, id.Record.CAFingerprint, id.Record.EnrolledAt.Format(time.RFC3339))

	// THE TENANT, READ OUT OF THE CERTIFICATE. Printed with the URI it came from rather than as a bare
	// word, because the whole claim of this slice is that the namespace is IN THE CREDENTIAL — and an
	// operator who cannot see the difference between that and a line in a JSON file has no way to
	// check it. `loadIdentity` has already refused if `warden.json` disagreed.
	fmt.Fprintf(cliio.Stdout, "tenant     %s (namespace; from this Machine's certificate)\n           %s\n",
		id.scope.Namespace, id.scope)

	// WHERE THIS MACHINE'S LIFECYCLE IS WRITTEN, or that it is not written anywhere. The absence is
	// printed as a sentence rather than as a blank field, because a Fleet whose history is silently
	// not being recorded looks exactly like a Fleet in which nothing has happened.
	if id.Record.Temporal == "" {
		fmt.Fprintf(cliio.Stdout, "watcher    none — this Machine's enrolment named no control plane, so "+
			"nothing records what happens here\n")
	} else {
		fmt.Fprintf(cliio.Stdout, "watcher    %s\n           on %s (namespace %s), from queue %s\n",
			wardenWorkflowID(id.Record.WardenID), id.Record.Temporal, id.scope.Namespace,
			wardenMachineQueue(id.Record.WardenID))
		// WHICH OF THE TWO TENANCIES THIS IS, SAID PLAINLY. With mTLS, the SERVER holds the boundary:
		// the certificate names one namespace and Temporal gives this Machine that one. Without it,
		// kontra chose the namespace and nothing on the wire stops a Machine's own code asking for
		// another — which is a true and materially weaker property, and an operator is entitled to
		// know which one they have rather than to infer it from a flag they did not type.
		if id.Record.TemporalTLS {
			fmt.Fprintf(cliio.Stdout, "           this Machine offers the certificate above to Temporal, so the "+
				"namespace is enforced by the server\n")
		} else {
			fmt.Fprintf(cliio.Stdout, "           this Machine dials Temporal WITHOUT a client certificate — the "+
				"namespace above is the only one kontra will use, but the server is not checking. "+
				"`kontra warden ca serve --temporal-tls` on a Temporal that requires client "+
				"certificates is what makes it a boundary the server holds.\n")
		}
	}

	// THE EXPIRY IS PRINTED AS A COUNTDOWN because there is no renewal in this slice (see
	// warden_ca.go:wardenCertValidity). A date is something an operator reads past; "expires in 12
	// days" is something they act on.
	left := time.Until(id.Record.NotAfter)
	days := int(left.Hours() / 24)
	switch {
	case left <= 0:
		fmt.Fprintf(cliio.Stdout, "expires    %s — EXPIRED. Re-run `kontra warden join` with a fresh token.\n",
			id.Record.NotAfter.Format(time.RFC3339))
	case days < 30:
		fmt.Fprintf(cliio.Stdout, "expires    in %d days (%s) — there is no automatic renewal; re-enrol before then.\n",
			days, id.Record.NotAfter.Format(time.RFC3339))
	default:
		fmt.Fprintf(cliio.Stdout, "expires    in %d days (%s)\n", days, id.Record.NotAfter.Format(time.RFC3339))
	}

	// WHAT THIS MACHINE TRUSTS IS PART OF ITS STATUS. `warden status` is the one command an operator
	// runs on a Machine that is not doing what they expect, and "nothing is placeable because nothing
	// is allowlisted" is the answer they most need and would otherwise have to infer from silence.
	//
	// A BAD POLICY IS PRINTED, NOT RETURNED. A diagnostic that refuses to run because the thing it
	// diagnoses is broken is the least useful moment for it to be strict; `warden serve` is where the
	// same error stops the process.
	policy, perr := trustpolicy.Load(trust.value())
	if perr != nil {
		fmt.Fprintf(cliio.Stdout, "\ntrust      MISCONFIGURED, so this Machine would start no Workers:\n           %v\n", perr)
	} else {
		lines := policy.Describe()
		fmt.Fprintf(cliio.Stdout, "\ntrust      %s\n           %s\n", lines[0], lines[1])
	}

	drv, err := wardenDriver(context.Background(), *driverName, policy)
	if err != nil {
		fmt.Fprintf(cliio.Stdout, "\nworkers    unknown: %v\n", err)
		return nil
	}
	hs, err := drv.list(context.Background())
	if err != nil {
		fmt.Fprintf(cliio.Stdout, "\nworkers    unknown: %v\n", err)
		return nil
	}
	if len(hs) == 0 {
		fmt.Fprintf(cliio.Stdout, "\nworkers    none (%s)\n", drv.driverName())
		return nil
	}
	fmt.Fprintf(cliio.Stdout, "\nworkers    (%s)\n", drv.driverName())
	for _, h := range hs {
		health := "whole"
		if !h.whole() {
			health = halvesOf(h) + " only"
		}
		fmt.Fprintf(cliio.Stdout, "  %-32s %s\n", h.id(), health)
	}
	return nil
}

// --- the Controller's side ----------------------------------------------------------------------------

func wardenCACmd(args []string) error {
	if len(args) == 0 {
		wardenUsage()
		return errors.New("warden ca: expected serve, token or list")
	}
	switch args[0] {
	case "serve":
		return wardenCAServe(args[1:])
	case "token":
		return wardenCAToken(args[1:])
	case "list":
		return wardenCAList(args[1:])
	case "retire":
		return wardenCARetire(args[1:])
	default:
		wardenUsage()
		return fmt.Errorf("unknown `warden ca` subcommand %q", args[0])
	}
}

// wardenCARetire ends one **Machine**'s watcher.
//
// THE ONE THING THAT CLOSES A WARDEN'S WORKFLOW, and warden_workflow.go's `wardenRetire` states the
// hazard it exists for: a **Warden**'s id is derived from its key, a certificate lives one year with
// no renewal, and re-enrolment therefore produces a NEW Warden — leaving the previous one's watcher
// arming watches on a queue nobody will poll again. It backs off to one observation an hour, so a
// forgotten one is cheap rather than free; this is how it is made free.
//
// IT DOES NOT DESTROY ANYTHING. ADR 0037: "The Warden may restart a Worker. It may not replace a
// Machine." Retiring is the control plane declining to WATCH a Machine; nothing here touches the
// Machine, its Workers or its identity, and a Machine that is still running will keep reconciling
// against its assignment with nobody recording it.
//
// ═══ IT NEEDS THE TENANT, AND THAT IS SLICE 08'S POINT IN ONE COMMAND ═══
//
// This signals a workflow BY ID. A signal needs no queue and no poll, so in a shared namespace
// knowing a Warden id — which the Controller prints on every enrolment — would be enough to end
// another tenant's watcher. Nothing about the id can fix that; the two Machines being in two
// namespaces is what fixes it, and this command therefore has to say WHICH namespace it means. A
// `--tenant` that names the wrong one finds no workflow and changes nothing, which is the right
// failure.
func wardenCARetire(args []string) error {
	fs := flag.NewFlagSet("warden ca retire", flag.ContinueOnError)
	temporalFlag := fs.String("temporal", config.TemporalAddress(), "the Temporal address the watcher runs in")
	nsFlag := fs.String("tenant", wardenDefaultTenant(),
		"which tenant's Machine this is — a tenant IS its Temporal namespace, and a watcher exists in exactly one")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("warden ca retire takes exactly one Warden id (`kontra warden status` prints it)")
	}
	if err := validWardenNamespace(*nsFlag); err != nil {
		return err
	}
	wardenID := fs.Arg(0)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	c, err := client.DialContext(ctx, client.Options{HostPort: *temporalFlag, Namespace: *nsFlag})
	if err != nil {
		return fmt.Errorf("temporal at %s: %w", *temporalFlag, err)
	}
	defer c.Close()
	if err := wardenRetire(ctx, c, wardenID); err != nil {
		return fmt.Errorf("retiring %s (%s, namespace %s): %w", wardenID, wardenWorkflowID(wardenID), *nsFlag, err)
	}
	fmt.Fprintf(cliio.Stdout, "retired %s in namespace %s — %s will stop watching it\n",
		wardenID, *nsFlag, wardenWorkflowID(wardenID))
	return nil
}

// sanList collects repeated --san flags.
type sanList []string

func (s *sanList) String() string { return strings.Join(*s, ",") }
func (s *sanList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func wardenCAServe(args []string) error {
	fs := flag.NewFlagSet("warden ca serve", flag.ContinueOnError)
	dirFlag := fs.String("dir", "", "where the Fleet CA lives (default <kontra home>/warden-ca)")
	listen := fs.String("listen", fmt.Sprintf(":%d", wardenCADefaultPort), "address to serve enrolment on")
	// THE ADDRESS A MACHINE WILL DIAL, NOT THE ONE THIS PROCESS USES, and it defaults to the latter
	// for the same reason `--san` defaults to `localSANs()`: on the single box they are the same
	// string, and on a real Fleet they are not. `127.0.0.1:7233` handed to a Machine is an address
	// that resolves perfectly and reaches that Machine's own loopback, which is the failure mode
	// worth naming in a flag description.
	temporalFlag := fs.String("temporal", config.TemporalAddress(),
		"the Temporal address MACHINES will reach; empty means this Fleet records no lifecycle")
	// THERE IS NO `--namespace` ANY MORE, and its absence is the slice. A Controller-wide namespace
	// was one namespace for every Machine that ever enrolled here — the collision ADR 0036 §6
	// describes, where two customers shipping `nscheck@0.1.0` land on one task queue. The namespace is
	// now the TENANT, fixed per token at `ca token --tenant`, and this process serves as many of them
	// as its roster holds.
	tlsFlag := fs.Bool("temporal-tls", false,
		"that Temporal requires client certificates, so each Machine offers the identity this CA issued it")
	noPlane := fs.Bool("no-plane", false,
		"serve enrolment only; do not execute Machines' watchers here")
	var sans sanList
	fs.Var(&sans, "san", "a name or address Machines will use to reach this Controller (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dir, err := wardenCADir(*dirFlag)
	if err != nil {
		return err
	}
	ca, err := openWardenCA(dir, cliio.Stdout)
	if err != nil {
		return err
	}
	ca.temporal, ca.temporalTLS = strings.TrimSpace(*temporalFlag), *tlsFlag
	if len(sans) == 0 {
		sans = sanList(localSANs())
	}
	cfg, err := ca.tlsConfig(sans)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              *listen,
		Handler:           ca.handler(cliio.Stdout),
		TLSConfig:         cfg,
		ReadHeaderTimeout: 10 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	fmt.Fprintf(cliio.Stdout, "serving Warden enrolment on %s\n  CA %s\n  names in the certificate: %s\n"+
		"  assignments read from %s — ONE DIRECTORY PER TENANT, chosen from the caller's own\n"+
		"    certificate, so no Machine can be served another tenant's file\n",
		*listen, ca.pinString(), strings.Join(sans, ", "), filepath.Join(dir, caAssignDir, "<tenant>"))

	// THE CONTROLLER EXECUTES EVERY MACHINE'S WATCHER, and it is the same process because a Fleet
	// whose lifecycle is recorded by a second daemon nobody remembered to start is a Fleet with no
	// lifecycle. See warden_workflow.go: a blocked workflow holds no worker slot, so one worker per
	// TENANT watches all of that tenant's Machines and its cost is per DECISION rather than per
	// Machine.
	//
	// A CONTROL PLANE THAT IS NOT THERE IS A SENTENCE, NOT A FAILURE. Enrolment is the thing this
	// command exists for and it works with no Temporal at all; what is lost is the **Lease** workflow, and this
	// says so on the line where an operator is already reading.
	if err := servePlane(ctx, ca, *temporalFlag, *noPlane); err != nil {
		fmt.Fprintf(cliio.Stdout, "  lifecycle NOT recorded: %v\n"+
			"    Machines enrolling here will still reconcile; nothing will write their history.\n", err)
	}
	defer ca.plane.close()

	// ListenAndServeTLS with the certificate already in TLSConfig — the two empty strings are how
	// Go says "use what is configured" rather than "read these files".
	if err := srv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// servePlane attaches this Controller's plane set to the CA and starts one worker per tenant ALREADY
// ON THE ROSTER, or explains why it did not.
//
// It returns nil for the two cases that are choices rather than failures — `--no-plane`, and a
// Controller with no Temporal address — and prints which of the two it was, so that "the operator
// asked for this" and "this is broken" are two different sentences rather than one silence.
//
// THE ROSTER IS READ HERE AND ALSO ON EVERY ENROLMENT. A tenant minted while this process was down
// is picked up now; one minted while it is up is picked up by `wardenPlaneSet.ensure` from the enrol
// handler. Both, because either alone leaves a real Fleet with a tenant nobody is watching, and a
// tenant nobody is watching is indistinguishable from a tenant to whom nothing has happened.
func servePlane(ctx context.Context, ca *wardenCA, addr string, disabled bool) error {
	if disabled {
		fmt.Fprintf(cliio.Stdout, "  --no-plane: Machines' watchers are NOT executed here\n")
		return nil
	}
	if strings.TrimSpace(addr) == "" {
		fmt.Fprintf(cliio.Stdout, "  --temporal is empty: Machines enrolling here record no lifecycle\n")
		return nil
	}
	ca.plane = newWardenPlaneSet(ctx, addr, cliio.Stdout)
	tenants, err := ca.readTenants()
	if err != nil {
		return err
	}
	if len(tenants) == 0 {
		fmt.Fprintf(cliio.Stdout, "  no tenants yet — `kontra warden ca token --tenant <name>` creates one, and "+
			"its watchers start when its first Machine enrols\n")
		return nil
	}
	for _, name := range sortedKeysOf(tenants) {
		ca.plane.ensure(name)
	}
	return nil
}

// wardenCAToken mints one enrolment token, for one TENANT.
//
// THE TENANT IS CHOSEN HERE AND NOWHERE ELSE, and this command is the reason that is possible: it is
// the one step of enrolment a human performs, at the one moment they know which customer the Machine
// they are about to provision belongs to. Everything after it — the CSR, the certificate, the
// namespace, the assignment directory — follows from the token, so there is no later point at which
// a Machine could be asked, or could volunteer, which tenant it is in. See warden_tenant.go.
func wardenCAToken(args []string) error {
	fs := flag.NewFlagSet("warden ca token", flag.ContinueOnError)
	dirFlag := fs.String("dir", "", "where the Fleet CA lives (default <kontra home>/warden-ca)")
	ttl := fs.Duration("ttl", time.Hour, "how long this token may be spent within")
	tenant := fs.String("tenant", wardenDefaultTenant(),
		"which tenant the Machine belongs to — a tenant IS its Temporal namespace")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dir, err := wardenCADir(*dirFlag)
	if err != nil {
		return err
	}
	ca, err := openWardenCA(dir, cliio.Stdout)
	if err != nil {
		return err
	}
	tok, err := ca.mintToken(*ttl, strings.TrimSpace(*tenant))
	if err != nil {
		return err
	}
	// PRINTED ONCE AND STORED AS A HASH. Saying so here is not decoration: the alternative is an
	// operator who loses the paste and goes looking for the file that has it.
	fmt.Fprintf(cliio.Stdout, "%s\n\n"+
		"  single use, expires in %s, for tenant %s (namespace %s). This is the only time it is shown\n"+
		"  — the Controller keeps only its hash. Whoever spends this token gets a Machine in THAT\n"+
		"  namespace and no other. On the Machine:\n\n"+
		"    kontra warden join --controller https://<this controller>:%d --token %s\n",
		tok, *ttl, *tenant, *tenant, wardenCADefaultPort, tok)
	return nil
}

func wardenCAList(args []string) error {
	fs := flag.NewFlagSet("warden ca list", flag.ContinueOnError)
	dirFlag := fs.String("dir", "", "where the Fleet CA lives (default <kontra home>/warden-ca)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dir, err := wardenCADir(*dirFlag)
	if err != nil {
		return err
	}
	ca, err := openWardenCA(dir, cliio.Stdout)
	if err != nil {
		return err
	}
	fmt.Fprintf(cliio.Stdout, "Fleet CA    %s\nfingerprint %s\nexpires     %s\n\n",
		dir, ca.pinString(), ca.cert.NotAfter.Format(time.RFC3339))

	// THE TENANTS FIRST, because they are what a Controller IS now — the set of namespaces it issues
	// credentials for and executes watchers in. A token is one act; a tenant is the boundary.
	tenants, err := ca.readTenants()
	if err != nil {
		return err
	}
	if len(tenants) == 0 {
		fmt.Fprintf(cliio.Stdout, "no tenants — `kontra warden ca token --tenant <name>` creates one. A tenant IS\n"+
			"a Temporal namespace (ADR 0036), so its Machines cannot address another tenant's queues.\n\n")
	} else {
		fmt.Fprintf(cliio.Stdout, "tenants (each one is a Temporal namespace)\n")
		for _, name := range sortedKeysOf(tenants) {
			fmt.Fprintf(cliio.Stdout, "  %-24s since %s   assignments: %s\n", name,
				tenants[name].CreatedAt.Format(time.RFC3339),
				filepath.Join(dir, caAssignDir, name))
		}
		fmt.Fprintln(cliio.Stdout)
	}

	tokens, err := ca.readTokens()
	if err != nil {
		return err
	}
	if len(tokens) == 0 {
		fmt.Fprintln(cliio.Stdout, "no enrolment tokens have been minted")
		return nil
	}
	for _, h := range sortedKeysOf(tokens) {
		t := tokens[h]
		// THE TENANT IS ON EVERY LINE. A token is a grant of one namespace to whoever holds it, and a
		// list that showed only "unspent, expires 15:04" would be a list of grants with the grant left
		// out — which is the one thing an operator hunting a mis-provisioned Machine needs to see.
		where := "tenant " + t.Tenant
		if t.Tenant == "" {
			where = "NO TENANT — minted before namespace-per-tenant; it will be refused, mint a fresh one"
		}
		switch {
		case t.SpentAt != nil:
			fmt.Fprintf(cliio.Stdout, "%s…  %s, spent %s by %s\n", h[:12], where, t.SpentAt.Format(time.RFC3339), t.SpentBy)
		case time.Now().After(t.ExpiresAt):
			fmt.Fprintf(cliio.Stdout, "%s…  %s, expired %s, unspent\n", h[:12], where, t.ExpiresAt.Format(time.RFC3339))
		default:
			fmt.Fprintf(cliio.Stdout, "%s…  %s, unspent, expires %s\n", h[:12], where, t.ExpiresAt.Format(time.RFC3339))
		}
	}
	return nil
}
