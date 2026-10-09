// Package engine runs a kontra author registry (package core) — the Go peer of Python's
// internals/engine.py. The author writes the ordinary kontra surface and this executes it, with
// no runtime-specific author code:
//
//	Load        -> a.open() on the first RunBatch AND on reload (fresh resource)
//	Method      -> ONE call per batch; the AUTHOR loops and this commits each Unit as the loop
//	               moves past it (ADR 0023 §2, §3, §23)
//	Healthcheck -> the primary resource-death signal: probed on any Method failure (parity
//	               with Python's @actor.healthcheck) + an optional background progress beat
//	Close       -> Close (the handler invokes it when the run completes)
//
// It is deliberately TRANSPORT-FREE. The host (runtime/go/temporalhost) and the state store
// (runtime/go/statekv) are both swappable behind narrow seams; everything below — the commit
// keying, the isolation policy, the reload rule — is independent of either, and a runtime swap
// has already been done once without touching a line of it.
//
// COMMIT BY IDENTITY, NOT BY POSITION. A committed Unit is keyed by the BATCH's content hash plus
// its index (ADR 0023 §17). That is stable across a retry by construction and distinct across
// Batches, which is what lets the checkpoint a retry is handed be checked against the batch it
// describes. Keyed on position alone, a second Batch under one Session replays the first Batch's
// outputs by index and never runs the author at all: full, plausible, wrong.
//
// ISOLATION AT THE ITERATOR BOUNDARY. The Batch knows which Unit the author is on, so a failure
// in the body is attributed to that Unit, recorded, and the Method RE-INVOKED with the remainder
// (§13) — cheap and equivalent, because everything pushed so far is already committed. So a
// Method is entered several times per Batch: author locals reset between entries, the Session does
// not. A Unit that keeps killing the resource is isolated after maxUnitReloads (§21), so one
// poison Unit can't sink the batch by looping reload-resume-die.
//
// Reload is owned by the /handler Temporal activity, not this actor. When a Method fails, the host
// probes @actor.healthcheck: if the probe reports the resource DEAD (non-nil error or a returned
// false) — OR the Method returned a core.SessionLostError — RunBatch nulls the instance and
// RETURNS the error, so the handler's activity retries the SAME actor id, the fresh turn re-runs
// Load, and every committed Unit is folded back rather than re-run.
//
// RESUME IS TEMPORAL'S (PRD D1, ADR 0060): within one activity execution, across its attempts. The
// previous attempt's last heartbeat carries a checkpoint saying WHICH Units finished, and each
// finished Unit's outcome is a commit object in the unit store written before that beat. The host
// hands the checkpoint in (ResumeFrom); nothing here reads a cache to decide what already ran.
//
// Method bodies must be IDEMPOTENT: a Unit can re-run after a host death that struck between the
// work and its commit (at-least-once across that window).
//
// Close is HANDLER-DRIVEN so the Python and Go engines behave identically: the workflow schedules
// a Close activity on every exit path, including a failed batch — which is exactly when a loaded
// resource must not leak, since a browser left open on a fleet Machine outlives the run.
package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"runtime/debug"
	"sync"
	"time"

	"github.com/medmahmoudi26/kontra/runtime/go/checkpoint"
	"github.com/medmahmoudi26/kontra/runtime/go/globalstore"
	"github.com/medmahmoudi26/kontra/runtime/go/rediskv"
	"github.com/medmahmoudi26/kontra/runtime/go/statekv"
	"github.com/medmahmoudi26/kontra/runtime/go/unitstore"
	"github.com/medmahmoudi26/kontra/sdk/go/core"
)

// Package-level config, set once at boot. Configure is called before the worker starts polling;
// each live session reads these.
var (
	reg *core.Registry
	// unitStore is the per-unit blob plane for pushed records (nil => no KONTRA_S3_ENDPOINT,
	// the no-S3 dev/test mode where pushed records are collected inline). One per host process.
	unitStore *unitstore.Store
)

// Configure binds the registry this host process serves and opens the unit blob store from the
// environment. Call once before the worker starts polling (Serve does this).
func Configure(r *core.Registry) {
	reg = r
	if us, err := unitstore.FromEnv(context.Background()); err != nil {
		log.Printf("[kontra] unit store init failed (streaming falls back to inline): %v", err)
	} else {
		unitStore = us
	}
}

// ConfigureWith binds the registry and an already-open unit store — Configure with the store handed
// in rather than read from the environment. It exists because the store is now on the RESUME path as
// well as the push path: a host-level test of a retry has to put the same store under two attempts,
// and the environment can only describe a real S3. nil is the no-S3 mode, exactly as Configure
// leaves it when KONTRA_S3_ENDPOINT is unset.
func ConfigureWith(r *core.Registry, us *unitstore.Store) {
	reg = r
	unitStore = us
}

// maxUnitReloads: a Unit that forces this many resource reloads is ISOLATED as a failure instead
// of reloading forever, so one poison Unit (a Unit that kills the resource every run) can't sink
// the batch by exhausting the handler's activity retries (ADR 0023 §21, peer of Python's
// _MAX_UNIT_RELOADS). This is framework-internal machinery beside the commit, NOT an author
// state tier, and NOT a failure budget — nothing fails a Batch on the framework's judgement (§14).
const maxUnitReloads = 2

// stateTTL: a Unit's scratch and reload counter self-expire this long after their last write so
// Redis doesn't grow unbounded; comfortably past the handler's retry budget, so an in-flight Unit's
// scratch is still there for the attempt that re-runs it. Applied by runtime/go/statekv on every
// write (one EXPIRE on the actor's hash) — peer of STATE_TTL_S in statekv.py.
const stateTTL = 24 * time.Hour

// ckptSuffix is the shared cross-SDK suffix for a Unit's resume scratch (ADR 0015). Both SDKs
// hang it off the Unit's commit slot, so the full key is `{batch}-u{i}-ckpt` in each
// (see statekey_congruence_test.go).
const ckptSuffix = "-ckpt"

// errInfo is the {type, message} of an isolated unit (wire.PerUnitFailure.error shape) — the same
// type the Unit's commit object carries, so a failure folded back on a retry is the failure that
// was recorded, field for field.
type errInfo = unitstore.CommitError

// CommitLostError is a Unit the previous attempt's checkpoint calls finished with no readable commit
// object.
//
// LOUD, AND NOT RETRIED. The checkpoint is beaten only AFTER the commit object is written, so a
// finished Unit with nothing at its key means the store lost it, or this worker is reading a
// different store than the attempt that wrote it. Neither improves on the next attempt — it reads the
// same checkpoint and the same store — and the two quiet alternatives are both wrong: folding the
// Unit as empty drops its rows, and re-running it hides a store that is losing data. The host maps it
// to a non-retryable ApplicationError of type "CommitLost", as Python's host does.
type CommitLostError struct{ Msg string }

func (e *CommitLostError) Error() string           { return e.Msg }
func (e *CommitLostError) KontraNonRetryable()     {}
func (e *CommitLostError) KontraErrorType() string { return "CommitLost" }

// RunBatchReq is one batch: the input Units, the run-wide params, and which Method to run.
type RunBatchReq struct {
	// ActorID keys the live instance and its state hash. The workflow derives it (idempotency
	// key, else run/node joined) so a retry lands on the SAME instance when this process still
	// holds it; which Units are already done comes from the heartbeat, not from the instance.
	ActorID string         `json:"actor_id"`
	Units   []any          `json:"units"`
	Params  map[string]any `json:"params"`
	// Method names which Method of the actor this dispatch calls (ADR 0023 §5), forwarded
	// verbatim from EntryInput. Resolved against the registry PER BATCH, which is what lets one
	// loaded Session serve several Methods. "" takes the sole Method, or refuses.
	Method string `json:"method"`
	// RunID/NodeID ride along from the handler so the host can key pushed-record blobs
	// (units/run={run}/dt={run_date}/actor={actor}/shard=…).
	RunID  string `json:"run_id"`
	NodeID string `json:"node_id"`
	// RunDate (YYYY-MM-DD) is the RUN's date, sent by the handler so every worker on a run agrees
	// on one dt partition even when the run crosses midnight. Empty from an older handler, in
	// which case the host falls back to its own clock.
	RunDate string `json:"run_date"`
}

// RunBatchResp is the batch result: the pushed records, the isolated failures, and how many
// times this instance has (re)loaded its resource.
//
// The field names are the envelope the handler stores and the orchestrator reads, so they must
// match the Python engine's return dict exactly — `failures` in particular is what
// runtime/handler/workflow.go counts to surface permanently dropped Units.
type RunBatchResp struct {
	// Done reports that every Unit is accounted for (committed or isolated).
	Done     bool             `json:"done"`
	Results  []any            `json:"results"`
	Failures []map[string]any `json:"failures"`
	Opens    int              `json:"opens"`
	// Machine names the host that ran this Batch — the provenance the handler copies onto the
	// result ref's meta and a published row carries into the lake. The engine never sets it;
	// the HOST does (runtime/go/temporalhost), because it is a fact about the process, not the
	// Units. `omitempty` so an envelope assembled outside a host (every engine test) says
	// nothing rather than claiming the empty Machine, which is how unrecorded stays unrecorded.
	Machine string `json:"machine,omitempty"`
}

// CloseResp is the handler-driven Close acknowledgement.
type CloseResp struct {
	Closed bool `json:"closed"`
}

// StateStore is the durable per-actor state the engine keeps for a Unit IN FLIGHT: its resume
// scratch (`unit_state`) and its reload counter. The production implementation is
// runtime/go/statekv over Redis; tests inject an in-memory fake.
//
// Narrow on purpose: this is everything the engine calls. It is an interface because the seam is
// real rather than hypothetical — the production store and the test fake have both existed from
// the start, and the store has been swapped once already.
//
// TOUCH AND DROP WENT WITH THE COMMIT MAP (ADR 0060). Touch slid the hash's TTL forward at each
// batch boundary so an idle actor's commit map survived between batches; Drop cleared it when a new
// batch owner superseded the last. A finished Unit is a commit object now and which Units finished
// rides the heartbeat, so neither had anything left to protect — every write here still slides the
// TTL itself.
type StateStore interface {
	Contains(ctx context.Context, field string) (bool, error)
	Get(ctx context.Context, field string, out any) error
	SetWithTTL(ctx context.Context, field string, val any, ttl time.Duration) error
	Remove(ctx context.Context, field string) error
	Save(ctx context.Context) error
}

// KontraActor is one live actor instance, keyed by actor id. The host holds at most one per id
// and carries the loaded session across the retries of one batch — the Go peer of Session in
// engine.py.
type KontraActor struct {
	actorID string
	state   StateStore // the per-Unit scratch and reload counters
	inst    *core.Session
	opens   int
	global  *globalstore.Store // cross-session global_state, built once per instance
	kv      *rediskv.EtagKV    // global_state's backing client, closed on Close
	// beat is set by the host; called once per Unit outcome. nil outside a hosted run, which is
	// what makes the engine testable without a Temporal activity context.
	beat func(done, total, isolated int)

	// The latest checkpoint, published by the runner and read by the host inside `beat`. Guarded
	// separately from the session lock: this is written on the commit path and read from the
	// heartbeat callback, and making them contend would put a heartbeat in front of a commit.
	ckMu sync.Mutex
	ck   checkpoint.Details
	// prior is the checkpoint the previous ATTEMPT of this activity last beat, handed in by the
	// host (ResumeFrom) and consumed by the next RunBatch. Under ckMu with `ck`, which it seeds.
	prior checkpoint.Details
	// progress is the sink for the author's @actor.healthcheck value, on the engine's own
	// ticker rather than per Unit. See SetProgress.
	progress func(any)
}

// SetHeartbeat installs the liveness/progress callback. Called by the host per activity
// execution, because RecordHeartbeat resolves against whichever execution is running.
func (a *KontraActor) SetHeartbeat(f func(done, total, isolated int)) { a.beat = f }

// SetProgress installs the sink for the author's @actor.healthcheck value.
//
// SEPARATE FROM SetHeartbeat because the two answer different questions and fire on different
// clocks. The heartbeat is LIVENESS and fires once per COMMITTED UNIT — which for a sweep unit
// (one host against 3,625 techniques) is tens of minutes apart, long enough that Temporal's
// heartbeat timeout has already killed the attempt. This one fires on the engine's own 2-second
// ticker regardless of unit boundaries, so a long unit still says what it is doing.
//
// It is also why this is not folded into `beat`: widening that signature would make every
// liveness beat carry a healthcheck probe, and the probe calls into the author's code.
func (a *KontraActor) SetProgress(f func(any)) { a.progress = f }

// ResolvedMethodName is the Method a dispatch MEANS, as a name — "" when the actor declares none
// or the dispatch is ambiguous.
//
// It exists so the host can name this batch's topic without duplicating `ResolveMethod`'s rules.
// `req.Method` is NOT that name: it is empty for a sole-Method actor dispatched without one, and
// a topic built from it would be `<actor>/` for exactly the simplest actor anybody writes.
// Errors are swallowed to "" rather than returned — a topic is an observability concern and must
// not be able to fail a dispatch that the resolver is about to reject on its own terms anyway.
func (a *KontraActor) ResolvedMethodName(wireName string) string {
	m, err := reg.ResolveMethod(wireName)
	if err != nil || m == nil {
		return ""
	}
	return m.Name
}

// publishCheckpoint stores the batch's latest checkpoint for the host to ship with the next beat.
//
// WHY A FIELD AND NOT A WIDER CALLBACK. SetHeartbeat's signature is exported and has callers outside
// this repository, so widening it would break them for a payload they do not send. The runner
// publishes here immediately BEFORE it beats, so what the host reads inside the callback is this
// batch's current state rather than a lagging copy.
func (a *KontraActor) publishCheckpoint(d checkpoint.Details) {
	a.ckMu.Lock()
	a.ck = d
	a.ckMu.Unlock()
}

// ResumeFrom hands the engine the checkpoint the previous ATTEMPT of this activity last beat — the
// `checkpoint` field of its heartbeat details — for the next RunBatch to resume from. The zero value
// means a first attempt.
//
// IT IS ALSO PUBLISHED AT ONCE, AS IS, and that is the half that matters for a second death.
// Temporal keeps only the LAST heartbeat, and the host's keepalive beats on a timer from the moment
// RunBatch is entered — through Load, which can take as long as the author's resource takes to come
// up. A beat sent before this attempt has re-derived its own checkpoint would otherwise carry
// whatever this instance last published (zero on a fresh one, ANOTHER batch's on a reused keyed
// instance) and overwrite the record of what the previous attempt finished. Re-beating the prior
// unchanged costs nothing: it is exactly what Temporal already holds. RunBatch replaces it with this
// batch's own as soon as it has the batch id, which is before Load.
func (a *KontraActor) ResumeFrom(prior checkpoint.Details) {
	a.ckMu.Lock()
	a.prior, a.ck = prior, prior
	a.ckMu.Unlock()
}

// takePrior returns the handed-in checkpoint and forgets it, so a LATER batch on this instance — a
// different dispatch — is never resumed from a previous one's attempt.
func (a *KontraActor) takePrior() checkpoint.Details {
	a.ckMu.Lock()
	defer a.ckMu.Unlock()
	p := a.prior
	a.prior = checkpoint.Details{}
	return p
}

// Checkpoint is the latest published checkpoint — WHICH Units committed and which were isolated,
// which the three counters SetHeartbeat carries cannot express. The host puts it in the heartbeat
// details, where it lives in the activity's own history rather than in a cache that can evict it
// (ADR 0059).
//
// The zero value has V == 0, which every reader of the contract discards. That is correct for an
// actor that has not begun a batch: a checkpoint nobody wrote is not evidence about anything.
func (a *KontraActor) Checkpoint() checkpoint.Details {
	a.ckMu.Lock()
	defer a.ckMu.Unlock()
	return a.ck
}

// heartbeat reports progress, best-effort: a heartbeat that fails must never fail a Unit that
// already committed.
func (a *KontraActor) heartbeat(done, total, isolated int) {
	if a.beat == nil {
		return
	}
	defer func() { _ = recover() }()
	a.beat(done, total, isolated)
}

// New builds a session for one actor id. `state` may be nil, in which case it is opened from the
// environment; tests inject a fake.
func New(actorID string, state StateStore) *KontraActor {
	if state == nil {
		state = statekv.FromEnv(actorID)
	}
	return &KontraActor{actorID: actorID, state: state}
}

// ID is the actor id this instance serves.
func (a *KontraActor) ID() string { return a.actorID }

// BatchID is the Batch's CONTENT HASH — the first half of a committed Unit's key (ADR 0023 §17).
//
// Stable across a retry by construction, distinct across Batches, and it survives a reopened scope
// because a hash does not know its scope died. A sequence number would restart at zero on exactly
// the recovery path v2 makes routine.
//
// The Method's NAME is part of the content: two Methods handed the same Units are two Batches, and
// letting them share slots is precisely the replay §17 exists to stop. Params are in it for the
// same reason — same Units, different params is a different call. What follows from hashing rather
// than counting: an identical call, made twice on purpose, IS a retry and replays.
// THE ENCODER IS PART OF THE CONTRACT, NOT AN IMPLEMENTATION DETAIL. `json.Marshal` escapes `<`,
// `>` and `&` to `\u003c`, `\u003e` and `\u0026` — a browser-safety default that has no business
// deciding a content hash. Python's `json.dumps` escapes nothing of the sort and escapes non-ASCII
// instead, so the two SDKs hashed the SAME Batch to different keys the moment a Unit contained a
// query string. MEASURED 2026-08-27, method "probe", empty params:
//
//	https://acme.com/?a=1&b=2   py d8868b6200c19340   go 6250da42654ec10a
//	café                        py 4fc02e4dd607c61d   go d0de34ff30eafbc4
//	<script>                    py 7e16c1cbdab1e24b   go 6513f87dedc4a622
//	plain-ascii                 both 76457373b3bbfe8c
//
// Both sides now emit raw UTF-8 with no HTML escaping, which is why the last row above is the one
// that did not have to change: for a Unit of plain ASCII with no `<`, `>` or `&`, neither escape
// ever fired, so every hash this repo has ever committed for such a Batch is unchanged. Only the
// inputs where the two SDKs ALREADY disagreed hash differently now, and a Batch whose key the two
// halves could not agree on was not a key worth preserving.
//
// `SetEscapeHTML(false)` requires an Encoder rather than Marshal, and an Encoder appends a newline
// it is this function's job to drop.
func BatchID(method string, units []any, params map[string]any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	err := enc.Encode([]any{method, units, params})
	payload := bytes.TrimRight(buf.Bytes(), "\n")
	if err != nil {
		// Not reachable for wire data (it arrived as JSON); fall back to a printed form rather
		// than silently hashing nothing, which would collide every Batch onto one key.
		payload = []byte(fmt.Sprintf("%v|%v|%v", method, units, params))
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])[:16]
}

// unitSlot is Unit i of batch bid as a state-hash prefix: its scratch (`-ckpt`) and its reload
// counter (`-reloads`) hang off it. It used to be the commit's own key too; commits are objects now
// (unitstore.CommitKey), and the identity they share is the same content hash plus index.
func unitSlot(bid string, i int) string { return fmt.Sprintf("%s-u%d", bid, i) }

// open builds a fresh session and runs Load — on the first batch AND on every reload after a
// SessionLost (peer of _open in engine.py).
func (a *KontraActor) open(params map[string]any) error {
	inst := core.NewSession(params, a.ID())
	inst.Rebuilds = a.opens // prior opens on this activation: 0 on the first load
	inst.BindEmitDurable(unitStore != nil)
	a.inst = inst
	a.opens++
	if reg.LoadFn != nil {
		if err := reg.LoadFn(inst); err != nil {
			// Counted here rather than at the caller, because this is the only place that knows a
			// Load was ATTEMPTED. Peer of the `except BaseException` arm in engine.py:_open.
			countLoad(false)
			return err
		}
		// The denominator moves on both paths — see countLoad. An actor with no Load counts
		// nothing at all, and the Warden reports "cannot tell" rather than a ratio over zero.
		countLoad(true)
	}
	log.Printf("[%s] loaded resource (open #%d)", a.ID(), a.opens)
	return nil
}

// globalStore returns the actor's cross-session global_state store, built once. Scoped by actor
// NAME (reg.Name), not name+version, so a version bump deliberately SHARES the tier.
func (a *KontraActor) globalStore() *globalstore.Store {
	if a.global == nil {
		a.kv = rediskv.FromEnv()
		a.global = globalstore.New(a.kv, reg.Name)
	}
	return a.global
}

// boundGlobalState adapts globalstore.Store (ctx-taking) to the ctx-free core.GlobalState the
// Session exposes, capturing RunBatch's ctx (the author's Method signature carries none).
type boundGlobalState struct {
	ctx context.Context
	s   *globalstore.Store
}

func (b boundGlobalState) Get(key string, out any) (bool, error) { return b.s.Get(b.ctx, key, out) }
func (b boundGlobalState) Set(key string, value any) error       { return b.s.Set(b.ctx, key, value) }
func (b boundGlobalState) AddToSet(key string, member any) (bool, error) {
	return b.s.AddToSet(b.ctx, key, member)
}
func (b boundGlobalState) Incr(key string, by int) (int, error) { return b.s.Incr(b.ctx, key, by) }
func (b boundGlobalState) CompareAndSet(key string, expected, newVal any) (bool, error) {
	return b.s.CompareAndSet(b.ctx, key, expected, newVal)
}

// probe runs the author's @actor.healthcheck (if declared) to decide whether the resource is
// dead. It mirrors Python's _probe: (dead, progress). A non-nil error means DEAD; a returned
// `false` (bool) means DEAD too (Python back-compat); otherwise ALIVE and the returned value is
// progress (may be nil). No healthcheck declared -> (false, nil): the caller then falls back to
// the Method's own SessionLostError signal to decide reload.
func (a *KontraActor) probe() (dead bool, progress any) {
	if reg.HealthcheckFn == nil || a.inst == nil {
		return false, nil
	}
	res, err := reg.HealthcheckFn(a.inst)
	if err != nil {
		return true, nil
	}
	if b, ok := res.(bool); ok && !b {
		return true, nil
	}
	return false, res
}

// RunBatch runs one Batch through the named Method. Units the previous attempt finished (see
// ResumeFrom) are folded back from their commit objects rather than re-run; a dead resource
// (healthcheck says dead, or a Method returns SessionLost) nulls the instance and is returned so the
// handler retries the same actor id; any other failure on a LIVE resource isolates the Unit the
// author was on.
func (a *KontraActor) RunBatch(ctx context.Context, req RunBatchReq) (*RunBatchResp, error) {
	// The DENOMINATOR of the sick-worker signature. `countReload` below is meaningless without
	// it — two reloads in a thousand batches and two in two are the same number otherwise. It was
	// defined and never called, which left the fleet dashboard dividing by a constant zero.
	countBatch()

	// Taken first, so a dispatch that fails to resolve its Method below cannot leave the previous
	// attempt's checkpoint behind for some later batch on this instance to read.
	prior := a.takePrior()

	// Resolved here, not at boot: an actor declares many Methods and the dispatch names one, so
	// one loaded Session serves them all (ADR 0023 §9).
	method, err := reg.ResolveMethod(req.Method)
	if err != nil {
		return nil, err
	}
	name := ""
	if method != nil {
		name = method.Name
	}
	// The RESOLVED name goes into the hash, not the wire field: a sole Method dispatched once by
	// name and once without is one Method, and must hash to one Batch.
	bid := BatchID(name, req.Units, req.Params)

	r := &batchRun{
		a: a, ctx: ctx, sm: a.state, bid: bid,
		slots: map[int][]any{}, failSlots: map[int]map[string]any{},
		total: len(req.Units),
		run:   req.RunID, node: req.NodeID, rdate: req.RunDate,
	}
	// Where this batch's Units commit. Derived from the run, the dispatch and the batch's content
	// hash, so every attempt of one activity derives the same prefix — and resumePlan keeps the
	// previous attempt's if it differs. Empty without an object store, where nothing is written.
	if unitStore != nil {
		r.manifest = unitStore.CommitPrefix(req.RunID, req.NodeID, bid)
	}

	// RESUME, BEFORE THE RESOURCE LOADS. The fold reads the commit objects and fails loud, without
	// paying for a Load, when they cannot be read.
	//
	// WHAT THE KEEPALIVE CARRIES MEANWHILE is decided here, because it beats from the moment this
	// function is entered — through the fold and through Load — and Temporal keeps only the last
	// beat. With nothing to fold, this batch's own (empty) checkpoint is published at once, replacing
	// whatever the instance last published: a discarded prior, or ANOTHER batch's on a reused keyed
	// instance. With a fold, nothing is published until the slots are refilled: ResumeFrom already
	// published the prior as is — the very checkpoint the plan was built from — and a checkpoint
	// rebuilt from the still-empty slots would beat `done: []` over it for the length of the fold.
	//
	// THE REDIS COMMIT MAP, THE `batch-owner` GUARD AND THE TTL RENEWAL THAT WERE HERE ARE GONE. The
	// map was the resume record, keyed by actor id under a 24 h TTL, so it also resumed ACROSS
	// executions — a re-dispatch on the same idempotency key. That path no longer resumes; it re-runs
	// (ADR 0060 says what keeps that from duplicating rows). The guard stopped one owner's commit map
	// answering for another's, and the renewal kept the map alive between batches; with no commit
	// map in the hash, neither had anything left to do. What remains there is an in-flight Unit's
	// scratch and reload counter, keyed by the batch's content hash.
	plan, err := r.resumePlan(prior)
	if err != nil {
		return nil, err
	}
	if plan != nil {
		if err := r.fold(plan, req.Units); err != nil {
			return nil, err
		}
	}
	r.publish()

	if a.inst == nil {
		if err := a.open(req.Params); err != nil {
			return nil, err
		}
	} else {
		a.inst.Params = req.Params
	}

	// Optional background progress beat: periodically log the healthcheck's returned progress
	// (peer of Python's _progress_beat). It only observes — a failing Method is what surfaces
	// death + reload — and is cancelled when RunBatch returns.
	if reg.HealthcheckFn != nil {
		beatCtx, stopBeat := context.WithCancel(ctx)
		defer stopBeat()
		go a.progressBeat(beatCtx)
	}

	// Bind cross-session global_state (its Redis client is opened lazily on first use).
	a.inst.BindGlobalState(boundGlobalState{ctx: ctx, s: a.globalStore()})

	// What is left is this attempt's work: every Unit neither folded back as committed nor folded
	// back as isolated.
	var todo []core.Item
	for i, u := range req.Units {
		if _, done := r.slots[i]; done {
			continue
		}
		if _, isolated := r.failSlots[i]; isolated {
			continue
		}
		todo = append(todo, core.Item{Index: i, Value: u})
	}

	// THIS BATCH'S CHECKPOINT, BEFORE THIS ATTEMPT'S FIRST BEAT. Every beat now carries
	// `a.Checkpoint()` (the keepalive and the author's progress included), and until a Unit commits
	// that was whatever the instance last published: a keyed dispatch's PREVIOUS batch — its done
	// count shown on this batch's run page — or, on a fresh instance, the zero value, which a
	// keepalive then wrote over the real checkpoint of the attempt before. Published here, from the
	// replayed commits, so the first beat of an attempt already says what is committed. And the
	// counts seeded to match, so the beat reports the replayed Units as done.
	r.mu.Lock()
	ck, done, isolated := r.checkpoint(), len(r.slots), len(r.failSlots)
	r.mu.Unlock()
	a.publishCheckpoint(ck)
	a.heartbeat(done, r.total, isolated)

	countBatch()
	if method == nil {
		// No Method declared (a load-only actor): identity passthrough.
		for _, it := range todo {
			r.slots[it.Index] = []any{it.Value}
		}
	} else {
		// The output Dataset the Method pushes to (ADR 0028 §2). Bound to the Batch so a push
		// commits against the Unit the iterator is on; the caller does not yet name it (that is
		// slice 07), so it is the unnamed, chainable kind. Its tail — records pushed with no
		// current Unit — is folded into results below.
		b := core.NewBatch(r, todo)
		if err := r.drive(method.Fn, b, core.NewDataset(b)); err != nil {
			if isSessionLost(err) {
				// The resource is dead. Null the instance so the NEXT turn re-runs Load, then
				// return the error: the handler's activity retries this same actor id, and the
				// retry folds the finished Units back from the checkpoint. Reload = the retry.
				log.Printf("[%s] resource dead -> let the handler retry: %v", a.ID(), err)
				countReload()
				a.inst = nil
			}
			return nil, err
		}
		r.tail = b.Tail()
	}

	return r.response(), nil
}

// batchRun is one Batch in flight: which of its Units have finished and with what, the durable
// sink the author's Units push and commit through, and the failure policy.
//
// It implements core.Sink — Enter / Record / Commit are the three calls the Batch makes as the
// author's loop moves.
type batchRun struct {
	a     *KontraActor
	ctx   context.Context
	sm    StateStore
	bid   string
	total int

	run, node, rdate string
	// manifest is the prefix this batch's commit objects live under — what the checkpoint carries
	// as `manifest_ref`. "" without an object store, where nothing is written.
	manifest string

	// mu guards the state store and the two slot maps. The store is NOT assumed goroutine-safe
	// and the author may run their Units in real goroutines, so every store call serializes here.
	// The author's body runs UNLOCKED, so acquiring mu from a sink call never self-deadlocks.
	mu        sync.Mutex
	slots     map[int][]any
	failSlots map[int]map[string]any
	// tail is the records the Method pushed with no current Unit (before/after the loop, or from a
	// goroutine under Units()). They belong to no Unit's commit, so they fold into results after
	// the per-Unit output (ADR 0028 §1). Read once from the Batch after drive returns.
	tail []any
}

func (r *batchRun) slot(i int) string { return unitSlot(r.bid, i) }

// checkpoint renders this batch's progress in the cross-SDK encoding
// (shared/conformance/checkpoint.json). THE CALLER MUST HOLD r.mu: it reads both slot maps.
//
// Built from the maps rather than maintained beside them, as the Python peer does. Two structures
// tracking one fact drift, and the drift would be a checkpoint that disagrees with the commits it
// claims to describe.
func (r *batchRun) checkpoint() checkpoint.Details {
	c := checkpoint.New(r.bid)
	c.ManifestRef = r.manifest
	for i := range r.slots {
		c.Commit(i)
	}
	for i := range r.failSlots {
		c.Isolate(i)
	}
	return c.ToDetails()
}

// Enter binds the Unit's durable resume scratch. Called when a Unit is handed to the author.
func (r *batchRun) Enter(u *core.Unit) {
	u.BindState(boundUnitState{r: r, key: r.slot(u.Index) + ckptSuffix})
}

// Record makes one pushed record durable NOW and returns what stands in for it in the commit.
// With an object store configured that is a blob keyed by content sha under the Unit's prefix, so
// a downstream cursor sees records before the Unit finishes and a re-run overwrites idempotently.
// Without one the record rides inline and is durable when the Unit commits — which is what
// Session.EmitDurable tells the author.
//
// No lock of its own: the object store is goroutine-safe and nothing shared is touched. The Batch
// serializes pushes above this, so a tail record's synthetic Unit index (ADR 0028 §1) is stable
// for the duration of the write.
func (r *batchRun) Record(u *core.Unit, rec any) (any, error) {
	if unitStore == nil {
		return rec, nil
	}
	m, ok := rec.(map[string]any)
	if !ok {
		m = map[string]any{"value": rec} // the blob layout stores JSON objects
	}
	return unitStore.PutSubunit(r.ctx, r.rdate, r.run, r.node, u.Index, m)
}

// Commit writes the Unit's commit object, drops its scratch, and beats. The beat is what a retry
// reads to skip the Unit and the object is what it folds back, so nothing after this line may
// re-run it.
func (r *batchRun) Commit(u *core.Unit) error {
	out := u.Out()
	if err := r.putCommit(u.Index, unitstore.Commit{Out: out}); err != nil {
		return err
	}
	r.mu.Lock()
	r.clearScratch(r.slot(u.Index))
	r.slots[u.Index] = out
	done, isolated := len(r.slots), len(r.failSlots)
	// PUBLISHED UNDER r.mu: two Units committing at once would otherwise publish in either order,
	// and the older checkpoint landing last would un-commit a Unit in the heartbeat.
	r.a.publishCheckpoint(r.checkpoint())
	r.mu.Unlock()
	r.a.heartbeat(done, r.total, isolated)
	return nil
}

// drive hands the whole Batch and its output Dataset to the author's Method and lets them loop
// (ADR 0023 §2; ADR 0028 §2).
//
// A failure is attributed to the Unit the iterator was on, recorded, and the Method RE-INVOKED
// with the remainder — cheap and equivalent, because everything pushed before the failure is
// already committed. So a Method is entered several times per Batch: author locals reset between
// entries, the Session does not.
//
// A failure with no Unit to blame (the author held the Batch to run it concurrently) propagates:
// there is no position to attribute by, and guessing one would pin the failure on whichever Unit
// happened to be pulled last.
func (r *batchRun) drive(fn core.MethodFunc, b *core.Batch, ds *core.Dataset) error {
	for {
		// A push made with no current Unit rides the Batch tail, keyed by the author's explicit key
		// (ADR 0028). The tail is RETAINED across the isolation re-invokes below — not rebuilt per
		// entry — and reconciled first-write-wins by that key: a re-push of a key already written is
		// dropped before its durable write, so a self-guarded push kept from entry 1 survives, a
		// content-varying one keeps its first value, and a push that appears only on the re-invoke
		// carries a fresh key and is kept. Identity is TOLD by the key, never inferred from
		// control-flow position — the three position/content inference attempts each lost or
		// duplicated a push.
		err := call(fn, r.a.inst, b, ds)
		// A durable write that did not land — a commit, or a fire-and-forget push (ADR 0028 §3) —
		// is the framework's failure, not the author's, and fails the WHOLE call: a broken store
		// is systemic, not one bad Unit, and isolating a Unit whose push failed would commit it
		// empty and lose the record on retry. Checked first, and before Blame, so a push failure
		// that coincides with an author raise takes precedence (peer of Python raising
		// push_error first).
		if berr := b.Err(); berr != nil {
			return berr
		}
		if err == nil {
			return b.Settle()
		}
		unit, serr := b.Blame()
		if serr != nil {
			return serr
		}
		if unit == nil {
			return err
		}
		if cerr := r.classify(unit, err); cerr != nil {
			return cerr
		}
		if b.Pending() == 0 {
			return nil
		}
	}
}

// call invokes the author's Method, converting a panic into an ordinary error.
//
// Python's engine catches Exception, so parity requires this: an author who indexes a nil map
// would otherwise take down the worker process AND every other Session it holds, turning one bad
// Unit into a fleet-wide outage. The panic still reaches the log with its stack; what changes is
// that it is classified like any other failure and isolates one Unit.
func call(fn core.MethodFunc, s *core.Session, b *core.Batch, ds *core.Dataset) (err error) {
	defer func() {
		if p := recover(); p != nil {
			log.Printf("[kontra] panic in Method: %v\n%s", p, debug.Stack())
			err = fmt.Errorf("panic in Method: %v", p)
		}
	}()
	return fn(s, b, ds)
}

// classify records one failure against the Unit the iterator was on (ADR 0023 §13). It returns
// non-nil only when the whole Batch must abort: a dead resource (so the handler retries and Load
// re-runs) or a durable write that did not land.
func (r *batchRun) classify(u *core.Unit, e error) error {
	switch {
	case isSessionLost(e): // the author says the resource is gone
		return r.reloadOrIsolate(u, e)
	case isNonRetryable(e): // terminal: isolate without probing
		return r.fail(u, e, "terminal")
	default:
		if dead, _ := r.a.probe(); dead { // dead resource -> reload
			return r.reloadOrIsolate(u, e)
		}
		return r.fail(u, e, "exhausted") // live resource -> isolate the Unit
	}
}

// fail isolates one Unit as a durable failure. The record matches the wire contract
// (PerUnitFailure {unit, error:{type,message}, category}); the SAME structured error is stored in
// the Unit's commit object so a retry folds it back verbatim.
func (r *batchRun) fail(u *core.Unit, e error, category string) error {
	ei := &errInfo{Type: core.ErrorTypeName(e), Message: e.Error()}
	slot := r.slot(u.Index)
	if err := r.putCommit(u.Index, unitstore.Commit{Error: ei, Category: category}); err != nil {
		return err
	}
	r.mu.Lock()
	r.clearScratch(slot) // an isolated Unit never resumes -> drop its scratch
	r.failSlots[u.Index] = failureRecord(u.Value, ei, category)
	done, isolated := len(r.slots), len(r.failSlots)
	r.a.publishCheckpoint(r.checkpoint()) // under r.mu, for Commit's reason
	r.mu.Unlock()
	countIsolated(category) // the run just lost this Unit; make that observable
	r.a.heartbeat(done, r.total, isolated)
	return nil
}

// reloadOrIsolate handles a Unit that signalled resource death (returned SessionLost, or failed
// while the healthcheck reports the resource dead). It bumps a DURABLE per-Unit reload counter;
// once one Unit has forced maxUnitReloads reloads it is isolated as a failure instead of reloading
// again — so a single poison Unit can't sink the batch by exhausting the handler's activity
// retries (ADR 0023 §21). Otherwise it returns SessionLost so the handler reloads.
func (r *batchRun) reloadOrIsolate(u *core.Unit, e error) error {
	key := r.slot(u.Index) + "-reloads"
	r.mu.Lock()
	n := 0
	if ok, err := r.sm.Contains(r.ctx, key); err == nil && ok {
		_ = r.sm.Get(r.ctx, key, &n)
	}
	n++
	if n >= maxUnitReloads {
		r.mu.Unlock()
		return r.fail(u, e, "exhausted")
	}
	err := saveState(r.ctx, r.sm, key, n)
	r.mu.Unlock()
	if err != nil {
		return err
	}
	return &core.SessionLostError{Msg: e.Error()}
}

// putCommit writes Unit i's commit object — its output refs, or its isolation error.
//
// SYNCHRONOUS, AND BEFORE THE BEAT. Commit and fail beat only after this returns, so a checkpoint can
// never name a Unit whose outcome is not already in the store — which is what lets a reader treat a
// missing object as loss (CommitLostError) rather than as "not finished yet". Outside r.mu: the
// object store is goroutine-safe, and holding the lock across a PUT would serialise every Unit's
// commit behind the slowest one.
//
// A no-op without an object store. That mode has nowhere durable to put a finished Unit's output,
// so a retry re-runs the whole batch (resumePlan); refusing to run at all would break every no-S3
// dev loop for an optimisation, while re-running is the path the contract already calls safe —
// Method bodies tolerate replay.
func (r *batchRun) putCommit(i int, c unitstore.Commit) error {
	if r.manifest == "" {
		return nil
	}
	return unitStore.PutCommit(r.ctx, unitstore.CommitKey(r.manifest, i), r.bid, i, c)
}

// publish hands the host this batch's current checkpoint for the next beat to carry.
func (r *batchRun) publish() {
	r.mu.Lock()
	ck := r.checkpoint()
	r.mu.Unlock()
	r.a.publishCheckpoint(ck)
}

// foldPlan is what resumePlan found worth reading back: the prefix the previous attempt committed
// under, and the Units it finished.
type foldPlan struct {
	manifest string
	finished []int
}

// resumePlan decides what the previous attempt's checkpoint lets this one skip — nil to run every
// Unit. Peer of Python's _resume_plan, outcome for outcome.
//
// The checkpoint is honoured only if checkpoint.Accepted says so — v1, and THIS batch's content
// hash — which is the corpus's rule and ResumeFrom's, so none of the `discarded` rows in
// shared/conformance/checkpoint.json can be folded back here.
//
// THREE OUTCOMES WHEN IT IS HONOURED, AND ONLY ONE OF THEM IS QUIET:
//   - no manifest_ref: the attempt that committed had no object store, so the outputs it finished
//     were never durable anywhere. Every Unit runs again — safe, because a re-run is what the
//     contract already permits, and logged, because it is work redone.
//   - a manifest_ref and no store HERE: this worker cannot read what the batch already committed.
//     CommitLostError, because re-running would silently hide a skewed fleet.
//   - both: fold the finished Units back from their objects (fold).
//
// A manifest_ref that differs from the one this worker derives is KEPT, for writing as well as
// reading. It only differs when the two attempts disagree on the layout or on KONTRA_S3_PREFIX, and
// splitting one batch's commits across two prefixes would make the NEXT attempt's checkpoint point
// at only half of them.
func (r *batchRun) resumePlan(prior checkpoint.Details) (*foldPlan, error) {
	ck := checkpoint.Accepted(&prior, r.bid)
	if ck == nil {
		if prior.V != 0 || prior.BatchID != "" {
			log.Printf("[%s] previous attempt's checkpoint does not describe batch %s; running every unit", r.a.ID(), r.bid)
		}
		return nil, nil
	}
	if ck.ManifestRef != "" && unitStore != nil && ck.ManifestRef != r.manifest {
		log.Printf("[%s] batch %s committed under %s, not %s: keeping the previous attempt's prefix",
			r.a.ID(), r.bid, ck.ManifestRef, r.manifest)
		r.manifest = ck.ManifestRef
	}
	var finished []int
	for i := 0; i < r.total; i++ {
		if ck.Done.Has(i) || ck.Failed[i] {
			finished = append(finished, i)
		}
	}
	if len(finished) == 0 {
		return nil, nil
	}
	if ck.ManifestRef == "" {
		log.Printf("[%s] batch %s: %d unit(s) finished on an attempt with no object store, so their outputs were never durable — running every unit again",
			r.a.ID(), r.bid, len(finished))
		return nil, nil
	}
	if unitStore == nil {
		return nil, &CommitLostError{Msg: fmt.Sprintf(
			"batch %s: a previous attempt committed %d unit(s) under %q, and this actor has no object store configured (KONTRA_S3_ENDPOINT unset) to read them back from",
			r.bid, len(finished), ck.ManifestRef)}
	}
	return &foldPlan{manifest: ck.ManifestRef, finished: finished}, nil
}

// foldConcurrency bounds the commit-object reads a resume makes at once — the peer of Python's
// _INGEST_CONCURRENCY, for the same reason: each is an S3 GET, and an unbounded fan-out over a
// thousand-Unit batch would open a thousand at once.
const foldConcurrency = 16

// fold reads every finished Unit's commit object and folds it back into this batch's slots.
//
// WHAT THE OBJECT SAYS WINS OVER WHICH SET THE CHECKPOINT PUT THE UNIT IN. The checkpoint answers
// "is it finished"; the object answers "with what". They can disagree only when a Unit re-ran after
// its beat was lost (a beat is throttled and a hard kill drops it) and finished the other way the
// second time — and the object is the later write.
func (r *batchRun) fold(plan *foldPlan, units []any) error {
	type read struct {
		i   int
		c   unitstore.Commit
		err error
	}
	results := make([]read, len(plan.finished))
	sem := make(chan struct{}, foldConcurrency)
	var wg sync.WaitGroup
	for n, i := range plan.finished {
		wg.Add(1)
		sem <- struct{}{}
		go func(n, i int) {
			defer wg.Done()
			defer func() { <-sem }()
			results[n] = read{i: i}
			key := unitstore.CommitKey(plan.manifest, i)
			raw, err := unitStore.GetCommit(r.ctx, key)
			switch {
			case errors.Is(err, unitstore.ErrNotFound):
				results[n].err = &CommitLostError{Msg: fmt.Sprintf(
					"batch %s: unit %d is finished according to the previous attempt's checkpoint, but there is no commit object at %s",
					r.bid, i, key)}
			case err != nil:
				results[n].err = err // a read that failed for any other reason stays retryable
			default:
				c, derr := unitstore.DecodeCommit(raw, r.bid, i)
				if derr != nil {
					results[n].err = &CommitLostError{Msg: fmt.Sprintf(
						"batch %s: unit %d's commit object at %s cannot be folded back: %v", r.bid, i, key, derr)}
				}
				results[n].c = c
			}
		}(n, i)
	}
	wg.Wait()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, res := range results {
		if res.err != nil {
			return res.err
		}
	}
	for _, res := range results {
		if res.c.Error != nil {
			r.failSlots[res.i] = failureRecord(units[res.i], res.c.Error, res.c.Category)
		} else {
			r.slots[res.i] = res.c.Out
		}
	}
	log.Printf("[%s] resumed batch %s: %d of %d unit(s) folded back from %s",
		r.a.ID(), r.bid, len(plan.finished), len(units), plan.manifest)
	return nil
}

// clearScratch drops a committed/isolated Unit's resume scratch. Best-effort: the Unit never
// re-runs and the state TTL is the backstop. Caller holds r.mu.
func (r *batchRun) clearScratch(slot string) {
	if err := r.sm.Remove(r.ctx, slot+ckptSuffix); err == nil {
		_ = r.sm.Save(r.ctx)
	}
}

// response assembles the batch envelope in INPUT order (identity-keyed), never completion order.
func (r *batchRun) response() *RunBatchResp {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Empty, not nil: the envelope is JSON the handler and the orchestrator read, and a nil slice
	// marshals to `null` where the Python peer emits `[]`. One SDK returning null failures is the
	// kind of difference a reader discovers in production data.
	results := []any{}
	failures := []map[string]any{}
	for i := 0; i < r.total; i++ {
		results = append(results, r.slots[i]...)
		if f, ok := r.failSlots[i]; ok {
			failures = append(failures, f)
		}
	}
	// The tail after the per-Unit outputs: records pushed with no current Unit belong to no Unit's
	// commit, so they append after the assembled Units, in push order (ADR 0028 §1).
	results = append(results, r.tail...)
	return &RunBatchResp{
		Done:     len(r.slots)+len(r.failSlots) == r.total,
		Results:  results,
		Failures: failures,
		Opens:    r.a.opens,
	}
}

// progressBeat periodically probes the healthcheck and REPORTS the returned progress, until ctx
// is cancelled (peer of Python's _progress_beat). It never triggers reload — a failing Method
// does that — it only surfaces liveness/progress for observability.
//
// ── IT REPORTS TO TEMPORAL, NOT ONLY TO A LOG ───────────────────────────────────────────────────
//
// This used to be `log.Printf` and nothing else, which put the richest progress signal in the
// system — whatever the author's @actor.healthcheck chose to return — into a file inside a
// container, reachable only by someone who knew to `docker exec … tail /tmp/host.log`. For a
// Machine in a Fleet there is no such someone.
//
// Temporal already carries progress for a running activity: `RecordHeartbeat` details are
// returned by DescribeWorkflowExecution on the pending activity, so `temporal workflow describe`
// shows them live and any client can poll them without a log pipeline, a shipper, or a mounted
// volume. The unit-level beat (`done`/`total`/`isolated`) already went that way; the healthcheck
// map did not, and the healthcheck map is the one that says what the actor is actually doing.
//
// Both still happen. The log line stays because it is what `docker logs` shows on a local
// worker, and the heartbeat is added because it is what works everywhere else.
func (a *KontraActor) progressBeat(ctx context.Context) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			dead, progress := a.probe()
			if dead {
				return // a failing Method will surface it + return the error
			}
			if progress != nil {
				log.Printf("[%s] progress: %v", a.ID(), progress)
				a.reportProgress(progress)
			}
		}
	}
}

// reportProgress hands the healthcheck's value to the host's progress sink, best-effort.
//
// Best-effort for the same reason `heartbeat` is: an observability call must never be the thing
// that fails a Unit that already committed. A nil sink is the ordinary case outside a hosted run
// and is what keeps the engine testable without a Temporal activity context.
func (a *KontraActor) reportProgress(v any) {
	if a.progress == nil {
		return
	}
	defer func() { _ = recover() }()
	a.progress(v)
}

// Close is the handler-driven cleanup: run CloseFn once and null the instance. The workflow
// schedules it on every exit path, including a failed batch. Peer of close() in engine.py.
func (a *KontraActor) Close(ctx context.Context) (*CloseResp, error) {
	if reg.CloseFn != nil && a.inst != nil {
		_ = reg.CloseFn(a.inst) // best-effort, like the Python host
	}
	a.inst = nil
	if a.kv != nil {
		_ = a.kv.Close() // release the global_state Redis client if one was opened
	}
	return &CloseResp{Closed: true}, nil
}

// saveState persists one value under key with a TTL and flushes it — each commit survives host
// death (peer of Python _set). The CALLER must hold the batch mutex: the store is not assumed
// goroutine-safe, so every write is serialized while the author's Units run concurrently.
func saveState(ctx context.Context, sm StateStore, key string, val any) error {
	if err := sm.SetWithTTL(ctx, key, val, stateTTL); err != nil {
		return err
	}
	return sm.Save(ctx)
}

// boundUnitState is the per-Unit resume scratch bound by Enter (implements core.UnitState). ALL of
// a Unit's keys live in ONE blob at `{batch}-u{i}-ckpt` (next to the Unit's commit), so
// clear-on-commit drops the whole Unit's scratch by deleting that single key. Its ctx is
// RunBatch's (the author's Method signature carries none), and every store call serializes under
// the batch mutex; the author's body runs UNLOCKED, so acquiring it here never self-deadlocks and
// the read-modify-write within one locked op is atomic.
type boundUnitState struct {
	r   *batchRun
	key string
}

func (b boundUnitState) Get(key string, out any) (bool, error) {
	b.r.mu.Lock()
	defer b.r.mu.Unlock()
	blob, err := b.readBlob()
	if err != nil {
		return false, err
	}
	raw, ok := blob[key]
	if !ok {
		return false, nil
	}
	return true, json.Unmarshal(raw, out)
}

func (b boundUnitState) Set(key string, value any) error {
	b.r.mu.Lock()
	defer b.r.mu.Unlock()
	blob, err := b.readBlob()
	if err != nil {
		return err
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	blob[key] = raw
	return b.writeBlob(blob)
}

func (b boundUnitState) Delete(key string) error {
	b.r.mu.Lock()
	defer b.r.mu.Unlock()
	blob, err := b.readBlob()
	if err != nil {
		return err
	}
	if _, ok := blob[key]; !ok {
		return nil
	}
	delete(blob, key)
	return b.writeBlob(blob)
}

// readBlob loads the Unit's keyed scratch blob (empty if absent). Caller holds the batch mutex.
func (b boundUnitState) readBlob() (map[string]json.RawMessage, error) {
	blob := map[string]json.RawMessage{}
	ok, err := b.r.sm.Contains(b.r.ctx, b.key)
	if err != nil || !ok {
		return blob, err
	}
	if err := b.r.sm.Get(b.r.ctx, b.key, &blob); err != nil {
		return nil, err
	}
	if blob == nil {
		blob = map[string]json.RawMessage{}
	}
	return blob, nil
}

func (b boundUnitState) writeBlob(blob map[string]json.RawMessage) error {
	if err := b.r.sm.SetWithTTL(b.r.ctx, b.key, blob, stateTTL); err != nil {
		return err
	}
	return b.r.sm.Save(b.r.ctx)
}

// failureRecord is one wire PerUnitFailure map {unit, error:{type,message}, category}.
func failureRecord(unit any, e *errInfo, category string) map[string]any {
	return map[string]any{"unit": unit, "error": e, "category": category}
}

// isSessionLost reports whether err signals a dead session resource — either the built-in
// *core.SessionLostError or any error whose cross-language type name is "SessionLost".
func isSessionLost(err error) bool {
	var sl *core.SessionLostError
	return errors.As(err, &sl) || core.ErrorTypeName(err) == "SessionLost"
}

// isNonRetryable reports whether err is an author NonRetryable signal (peer of Python's
// except NonRetryableError): a terminal Unit isolated without a reload probe.
func isNonRetryable(err error) bool {
	var nr core.NonRetryable
	return errors.As(err, &nr)
}
