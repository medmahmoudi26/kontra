package main

// driver.go — the seam between "a Worker belongs on this Machine" and whatever actually runs it.
//
// ADR 0036 collapses the Target axis to one: actor code runs in a container everywhere it runs, and
// the machine Target *survives as a DRIVER, not as a Target*. `process` — code executed directly,
// no runtime required — is what `kontra serve --actor <dir>` has always done and is how the single
// box and the airgapped install keep working. ADR 0037 gives that seam its consumer: the **Warden**
// reconciles the **Workers** on its **Machine** against what the control plane asked for, and a
// reconcile loop needs exactly these four verbs and no more.
//
// The four are Start / Stop / List / Logs. Nothing else belongs here — not "restart" (that is Stop
// then Start, and a driver that owned the policy would be a second place to disagree about health
// with the thing ADR 0037 says judges it), and not "wait" (a reconcile loop learns that a Worker
// died by LISTING, which is the same read it makes for every other reason; see the rule below).
//
// ═══ THE UNIT IS THE PAIR, NOT THE PROCESS ═══
//
// A **Worker** here is an actor AND the handler bound to it, held together by one handle. That is a
// deliberate choice against the cheaper one — a driver whose unit is a single process — and the
// reasons are:
//
//  1. ADR 0036 makes the binding structural, not conventional: "the handler stays one process per
//     actor version", because a handler serving several actors would put several tenants'
//     credentials in one process. `handler/main.go` reads KONTRA_ACTOR_NAME/VERSION from its
//     environment and registers ONE worker on ONE queue. There is no such thing as half a Worker to
//     schedule, so a seam whose unit is a process makes an unschedulable thing expressible and
//     leaves every driver to re-derive the 1:1 rule.
//
//  2. serve.go's own header states the failure this prevents: "Whichever half dies takes the other
//     with it. A half-dead worker is worse than a dead one: it keeps its Temporal lease and units
//     time out one by one." If the unit were a process, a reconcile loop could restart the handler
//     and leave the actor, which is precisely how a half-dead pair is MANUFACTURED.
//
//  3. The runtimes already agree. `--tmux` puts both halves in ONE session tagged
//     `actor:<name>:<version>` (tmux.go, and `panels/tmux.ts:actorSessionTag` on the other side of
//     the language boundary) — the tag names the pair, and `tmux kill-session` stops the pair.
//     Slice 02's podman driver has the same shape available in a pod. A per-process unit would have
//     to invent a grouping that both runtimes already have.
//
// What the pair-as-unit does NOT do is pretend a pair is atomic. A `workerHandle` carries the halves
// the runtime actually holds, so ONE half is representable — because it is a real state a Machine
// gets into, and a seam that could not report it would make the Warden blind to the one failure
// serve.go's header calls out.
//
// ═══ List() READS THE RUNTIME, NEVER A LOCAL DATABASE ═══
//
// A driver's memory of what it started is a CACHE. The runtime is the truth. This is the single most
// common bug in this class of software and it is invisible until the day it matters: the driver's
// map still holds the Worker it started, so reconcile is satisfied, while the process died an hour
// ago and the queue has had no poller since. It fails in the other direction too — a Worker started
// by the PREVIOUS Warden, or by an operator's own `kontra serve`, is invisible to a driver that only
// remembers its own, so reconcile starts a second one on the same queue.
//
// So `list` is a QUERY against the thing that is actually running, and every driver owes the same
// two properties, which driver_test.go pins:
//
//   - it reports a Worker this driver did not start, and
//   - it forgets one that died, without being told.
//
// For `podman` that is `podman ps --filter label=…`. For `process` it is the operating system's own
// process table, filtered by a label that lives in the process's environment — see
// driver_process.go, which explains why the environment is the process driver's label.

import (
	"context"
	"io"
	"strings"
	"time"
)

// workerPart names one half of a Worker. The two spellings are the tmux WINDOW names serve.go
// already uses and the ones `kontra panels list` prints, so a pane id and a driver handle name the
// same half with the same word.
type workerPart string

const (
	partActor   workerPart = "actor"
	partHandler workerPart = "handler"
)

// workerParts is the pair, in the order they are started: the actor first, because the handler is
// the half that owns the shared queue and a queue with a poller but no activity worker is the
// half-dead state that reads as healthy.
var workerParts = []workerPart{partActor, partHandler}

// procSpec is one half of a Worker as a driver is asked to run it.
//
// Env is the WHOLE environment and not a delta, matching what serve.go builds today — `derive()`
// there returns os.Environ() plus the actor's own variables, and the two halves get different ones
// (PYTHONPATH for the actor, GOWORK=off for the handler), which is why this is per-half and not per
// Worker.
type procSpec struct {
	Dir  string
	Argv []string
	Env  []string
}

// workerSpec is ONE Worker: the actor, the handler bound to it, and the identity both derive their
// queues from. `sharedQueue(Name, Version)` is what the handler polls and `<that>-sessions` is what
// the actor polls; the driver does not derive either, it only has to keep them together.
type workerSpec struct {
	Name    string
	Version string
	Actor   procSpec
	Handler procSpec

	// Image is the **Artifact** the two halves run FROM, and it is a driver-specific requirement
	// rather than a universal one: ADR 0036 collapses the Target axis to a container, and `process`
	// is the driver that reaches the same code without one. `podman` refuses a spec without it;
	// `process` never reads it, because a checkout is what it has instead.
	//
	// DIGEST-PINNED OR NOTHING — `<repo>@sha256:<64 hex>`, never a tag. See driver_podman.go, which
	// enforces it, for why a tag is not a weaker version of this but a different thing entirely.
	Image string
}

// workerHalf is one half AS THE RUNTIME HOLDS IT. Ref is driver-scoped and opaque to everything
// above this seam: the process driver's is a pid in decimal, podman's will be a container id. It is
// a string rather than a typed field per driver so that a handle can cross the seam unchanged.
type workerHalf struct {
	Part workerPart
	Ref  string
}

// workerHandle is a Worker as a driver can currently see it.
//
// Halves holds only what the runtime actually has, so a pair with one half gone comes back with one
// entry — see `whole()`. A handle is therefore a SNAPSHOT and not a token: it is safe to pass to
// stop/logs, and it is stale the moment it is returned, which is why nothing here caches one.
type workerHandle struct {
	Driver  string
	Name    string
	Version string
	Halves  []workerHalf
}

// id is how one Worker is named in output and in errors — the same `<name>@<version>` serve.go and
// `kontra workers list` already print.
func (h workerHandle) id() string { return h.Name + "@" + h.Version }

// half answers whether the runtime is holding one named half of this Worker.
func (h workerHandle) half(p workerPart) (workerHalf, bool) {
	for _, w := range h.Halves {
		if w.Part == p {
			return w, true
		}
	}
	return workerHalf{}, false
}

// whole reports whether BOTH halves are there. Anything else is the half-dead worker serve.go's
// header calls "worse than a dead one", and it is a question the Warden asks on every reconcile.
func (h workerHandle) whole() bool {
	_, a := h.half(partActor)
	_, b := h.half(partHandler)
	return a && b
}

// workerDriver is what puts a Worker on a Machine. `process` is the implementation this slice ships;
// `podman` arrives beside it in slice 02 and neither may see the other.
type workerDriver interface {
	// driverName is how this driver is spelled in output and in errors: "process", then "podman".
	// It goes on every handle so a mixed report says which runtime answered.
	driverName() string

	// start runs a Worker and returns it as the runtime now holds it.
	//
	// It is NOT idempotent and does not check whether the Worker is already running, because the
	// check that would be worth making is `list` — the read that is true whoever started the other
	// one. ADR 0037's `place()` is the idempotent verb and it is a caller of this, not this.
	start(ctx context.Context, spec workerSpec) (workerHandle, error)

	// stop ends both halves. drain is how long the Worker gets to leave on its own before it is
	// killed; a drain of 0 means kill now, which is what a caller that has already decided the
	// Worker is finished passes.
	stop(ctx context.Context, h workerHandle, drain time.Duration) error

	// list is every Worker this driver's runtime is holding, INCLUDING ones it did not start. See
	// this file's header: the memory is a cache, the runtime is the truth.
	list(ctx context.Context) ([]workerHandle, error)

	// logs is what the runtime retained for this Worker. A driver whose runtime retains nothing
	// says so rather than returning an empty stream — an empty log and a log that was never kept
	// are different answers and only one of them means "the Worker printed nothing".
	logs(ctx context.Context, h workerHandle) (io.ReadCloser, error)
}

// --- the label ----------------------------------------------------------------------------------

// workerLabelVar is the environment variable that marks a process as one half of a Worker, and it is
// the process driver's equivalent of `podman run --label`.
//
// THE ENVIRONMENT IS THE ONLY LABEL A BARE PROCESS HAS, and it happens to have exactly the property
// the List rule needs: it is written by the kernel at execve and is not the driver's to edit
// afterwards, it survives the driver exiting, and it vanishes when the process does. A pidfile, a
// JSON file under a state directory, or a map in the driver would all be the local database this
// seam refuses.
//
// It is one variable and not three because the value is a LABEL — a thing to match on — and three
// variables would let a process carry half of one. KONTRA_ACTOR_NAME and KONTRA_ACTOR_VERSION are
// already on both halves and were the tempting reuse; they are not enough, because they are equal on
// the actor and the handler and because every containerised Worker on this host carries them too
// (their processes are visible in the host's process table), and the `process` driver must not claim
// a Worker `podman` is holding.
const workerLabelVar = "KONTRA_WORKER"

// workerLabelSep separates the three fields of a label.
//
// NOTHING FORBIDS IT IN A NAME, and assuming otherwise was this file's one real bug.
// `shared/conformance/queues.json` carries an actor called `a/b` on purpose and states the rule the whole
// corpus is built to prove: "A QUEUE NAME IS NOT SANITISED" — `my actor` and `café` reach Temporal
// verbatim, and so does a slash. So the separator is not a constraint on the name, and the parse
// below reads the fields from the RIGHT rather than pretending it is one.
const workerLabelSep = "/"

// workerLabel is the value of workerLabelVar for one half: `<name>/<version>/<part>`, e.g.
// `beacon/0.2.0/actor`.
func workerLabel(name, version string, part workerPart) string {
	return name + workerLabelSep + version + workerLabelSep + string(part)
}

// workerLabelEnv is the whole KEY=VALUE entry, which is what goes into a process's environment and
// into `tmux new-session -e`.
func workerLabelEnv(name, version string, part workerPart) string {
	return workerLabelVar + "=" + workerLabel(name, version, part)
}

// parseWorkerLabel reads a label back.
//
// FROM THE RIGHT, AND THAT IS THE WHOLE POINT. A left-to-right `Split` into exactly three fields
// refuses `a/b/0.1.0/actor`, and refusing there is not a safe failure: `start` would launch the pair
// perfectly and `list` would never see it again, so an actor whose name holds a slash becomes an
// invisible Worker — the exact shape this seam exists to prevent, arriving through the seam itself.
// The part and the version are fixed-shape and the NAME is the free string, so taking the last two
// separators lets the name keep whatever it holds.
//
// STILL STRICT, because this is the boundary where an arbitrary process on the machine gets to claim
// it is one of ours: a part this file knows, and neither other field empty. A lenient parse here is
// how a stranger's `KONTRA_WORKER=x` becomes a Worker the Warden then tries to stop.
//
// WHAT IT CANNOT REPRESENT is a VERSION containing a separator — `n/1/2/actor` reads back as name
// `n/1`, version `2`. That is stated rather than guarded because the corpus carries a slashed name
// and no slashed version, and a guard would have to be a refusal at `start`, which would stop
// serving an actor that serves today.
func parseWorkerLabel(v string) (name, version string, part workerPart, ok bool) {
	cut := strings.LastIndex(v, workerLabelSep)
	if cut < 0 {
		return "", "", "", false
	}
	rest, p := v[:cut], workerPart(v[cut+1:])
	if cut = strings.LastIndex(rest, workerLabelSep); cut < 0 {
		return "", "", "", false
	}
	name, version = rest[:cut], rest[cut+1:]
	if name == "" || version == "" {
		return "", "", "", false
	}
	for _, known := range workerParts {
		if p == known {
			return name, version, p, true
		}
	}
	return "", "", "", false
}
