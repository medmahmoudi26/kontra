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
// its index (ADR 0023 §17). That is stable across a retry by construction, distinct across
// Batches, and it survives a reopened scope — a hash does not know its scope died. Keyed on
// position alone, a second Batch under one Session replays the first Batch's outputs by index and
// never runs the author at all: full, plausible, wrong.
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
// Load, and the commit map skips every committed Unit.
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

// maxUnitReloads: a Unit that forces this many resource reloads is ISOLATED as a failure instead
// of reloading forever, so one poison Unit (a Unit that kills the resource every run) can't sink
// the batch by exhausting the handler's activity retries (ADR 0023 §21, peer of Python's
// _MAX_UNIT_RELOADS). This is framework-internal machinery beside the commit map, NOT an author
// state tier, and NOT a failure budget — nothing fails a Batch on the framework's judgement (§14).
const maxUnitReloads = 2

// stateTTL: committed Unit keys self-expire this long after the run so Redis doesn't grow
// unbounded (one key set per run_id, never otherwise deleted); comfortably past the handler's
// retry budget. Applied by runtime/go/statekv on every write (one EXPIRE on the actor's hash) —
// peer of STATE_TTL_S in statekv.py.
const stateTTL = 24 * time.Hour

// ckptSuffix is the shared cross-SDK suffix for a Unit's resume scratch (ADR 0015). Both SDKs
// hang it off the Unit's commit slot, so the full key is `{batch}-u{i}-ckpt` in each
// (see statekey_congruence_test.go).
const ckptSuffix = "-ckpt"

// errInfo is the {type, message} of an isolated unit (wire.PerUnitFailure.error shape).
type errInfo struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// commit is one durable Unit result in the actor's state hash. A non-nil Error records an
// isolated Unit (with Category "terminal" or "exhausted"); Out holds what was pushed while the Unit
// was current otherwise. The presence of the key (Contains) is the exactly-once marker across a retry.
type commit struct {
	Out      []any    `json:"out"`
	Error    *errInfo `json:"error,omitempty"`
	Category string   `json:"category,omitempty"`
}

// RunBatchReq is one batch: the input Units, the run-wide params, and which Method to run.
type RunBatchReq struct {
	// ActorID keys the live instance and its state hash. The workflow derives it (idempotency
	// key, else run/node joined) so a retry lands on the SAME instance and the commit map skips
	// already-done Units.
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

// StateStore is the durable per-actor state the engine commits through. The production
// implementation is runtime/go/statekv over Redis; tests inject an in-memory fake.
//
// Narrow on purpose: this is everything the engine calls. It is an interface because the seam is
// real rather than hypothetical — the production store and the test fake have both existed from
// the start, and the store has been swapped once already.
type StateStore interface {
	Contains(ctx context.Context, field string) (bool, error)
	Get(ctx context.Context, field string, out any) error
	SetWithTTL(ctx context.Context, field string, val any, ttl time.Duration) error
	Remove(ctx context.Context, field string) error
	Save(ctx context.Context) error
	// Touch slides the whole actor's TTL forward in ONE call. It is the method the hash layout
	// bought: the store this replaced had none, so the host re-wrote every live session key at
	// each turn boundary just to refresh a clock.
	Touch(ctx context.Context) error
	// Drop clears the whole actor's state. Used when a new batch owner supersedes the previous
	// one, so one Session's per-batch state never leaks into the next.
	Drop(ctx context.Context) error
}

// KontraActor is one live actor instance, keyed by actor id. The host holds at most one per id
// and carries the loaded session across the retries of one batch — the Go peer of Session in
// engine.py.
type KontraActor struct {
	actorID string
	state   StateStore // the commit map and the per-Unit scratch
	inst    *core.Session
	opens   int
	global  *globalstore.Store // cross-session global_state, built once per instance
	kv      *rediskv.EtagKV    // global_state's backing client, closed on Close
	// beat is set by the host; called once per Unit outcome. nil outside a hosted run, which is
	// what makes the engine testable without a Temporal activity context.
	beat func(done, total, isolated int)
}

// SetHeartbeat installs the liveness/progress callback. Called by the host per activity
// execution, because RecordHeartbeat resolves against whichever execution is running.
func (a *KontraActor) SetHeartbeat(f func(done, total, isolated int)) { a.beat = f }

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

// unitSlot is where Unit i of batch bid commits, and the prefix its scratch hangs off.
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

// RunBatch runs one Batch through the named Method. Committed Units are skipped (exactly-once
// across a handler retry); a dead resource (healthcheck says dead, or a Method returns
// SessionLost) nulls the instance and is returned so the handler retries the same actor id; any
// other failure on a LIVE resource isolates the Unit the author was on.
func (a *KontraActor) RunBatch(ctx context.Context, req RunBatchReq) (*RunBatchResp, error) {
	// The DENOMINATOR of the sick-worker signature. `countReload` below is meaningless without
	// it — two reloads in a thousand batches and two in two are the same number otherwise. It was
	// defined and never called, which left the fleet dashboard dividing by a constant zero.
	countBatch()

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

	if a.inst == nil {
		if err := a.open(req.Params); err != nil {
			return nil, err
		}
	} else {
		a.inst.Params = req.Params
	}

	r := &batchRun{
		a: a, ctx: ctx, sm: a.state, bid: bid,
		slots: map[int][]any{}, failSlots: map[int]map[string]any{},
		total: len(req.Units),
		run:   req.RunID, node: req.NodeID, rdate: req.RunDate,
	}

	// Optional background progress beat: periodically log the healthcheck's returned progress
	// (peer of Python's _progress_beat). It only observes — a failing Method is what surfaces
	// death + reload — and is cancelled when RunBatch returns.
	if reg.HealthcheckFn != nil {
		beatCtx, stopBeat := context.WithCancel(ctx)
		defer stopBeat()
		go a.progressBeat(beatCtx)
	}

	// Slide live keys' TTLs forward
	// forward — the batch-boundary half of the sliding TTL. Done before the author's loop starts,
	// so the in-place bind and the renewal are uncontended.
	r.renewTTLs()
	// Bind cross-session global_state (its Redis client is opened lazily on first use).
	a.inst.BindGlobalState(boundGlobalState{ctx: ctx, s: a.globalStore()})

	// The commit map below is per BATCH; the state hash it lives in is per ACTOR ID. A KEYED
	// dispatch points many batches at ONE id, so per-batch state from a superseded owner is
	// dropped here rather than in Close — the handler's Close is best-effort, so a batch whose
	// close never landed would otherwise poison the next one. Tier 3/4 live in another store and
	// are untouched, which is the whole point of keying.
	r.claimOwner()

	// Replay commits from a PRIOR ATTEMPT of this Batch; what is left is this attempt's work. The
	// commit map is why a retry after a mid-batch death is not a re-run, and keying it by the
	// Batch's content hash is why a SECOND Batch under this owner cannot read the first's slots.
	var todo []core.Item
	for i, u := range req.Units {
		var prev commit
		if ok, err := r.sm.Contains(ctx, r.slot(i)); err == nil && ok {
			if err := r.sm.Get(ctx, r.slot(i), &prev); err == nil {
				if prev.Error != nil {
					r.failSlots[i] = failureRecord(u, prev.Error, prev.Category)
				} else {
					r.slots[i] = prev.Out
				}
				continue
			}
		}
		todo = append(todo, core.Item{Index: i, Value: u})
	}

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
				// return the error: the handler's activity retries this same actor id and the
				// commit map skips the finished Units. Reload = the retry.
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

// batchRun is one Batch in flight: the commit map for this Batch, the durable sink the author's
// Units push and commit through, and the failure policy.
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

// Commit writes the Unit's durable done-marker, drops its scratch, and beats. This is the marker
// a retry reads to skip the Unit, so nothing after this line may re-run it.
func (r *batchRun) Commit(u *core.Unit) error {
	out := u.Out()
	r.mu.Lock()
	if err := saveState(r.ctx, r.sm, r.slot(u.Index), commit{Out: out}); err != nil {
		r.mu.Unlock()
		return err
	}
	r.clearScratch(r.slot(u.Index))
	r.slots[u.Index] = out
	done, isolated := len(r.slots), len(r.failSlots)
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
// the commit so the skip-replay path re-emits it verbatim.
func (r *batchRun) fail(u *core.Unit, e error, category string) error {
	ei := &errInfo{Type: core.ErrorTypeName(e), Message: e.Error()}
	slot := r.slot(u.Index)
	r.mu.Lock()
	if err := saveState(r.ctx, r.sm, slot, commit{Error: ei, Category: category}); err != nil {
		r.mu.Unlock()
		return err
	}
	r.clearScratch(slot) // an isolated Unit never resumes -> drop its scratch
	r.failSlots[u.Index] = failureRecord(u.Value, ei, category)
	done, isolated := len(r.slots), len(r.failSlots)
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

// claimOwner drops per-batch state left by a superseded owner (see RunBatch).
func (r *batchRun) claimOwner() {
	owner := r.run + "/" + r.node
	var prev string
	r.mu.Lock()
	defer r.mu.Unlock()
	has := false
	if ok, err := r.sm.Contains(r.ctx, "batch-owner"); err == nil && ok {
		if err := r.sm.Get(r.ctx, "batch-owner", &prev); err == nil {
			has = true
		}
	}
	if has && prev == owner {
		return
	}
	if has {
		log.Printf("[%s] batch %s supersedes %s: clearing per-session state", r.a.ID(), owner, prev)
		_ = r.sm.Drop(r.ctx)
	}
	_ = saveState(r.ctx, r.sm, "batch-owner", owner) // after the drop, which clears the hash
}

// renewTTLs slides the actor's whole state TTL forward at the batch boundary. ONE call: the state
// is a single hash and EXPIRE applies to all of it. Best-effort — a store hiccup never fails a
// run, since the write-time TTL is the backstop.
func (r *batchRun) renewTTLs() {
	r.mu.Lock()
	defer r.mu.Unlock()
	_ = r.sm.Touch(r.ctx)
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

// progressBeat periodically probes the healthcheck and logs the returned progress, until ctx is
// cancelled (peer of Python's _progress_beat). It never triggers reload — a failing Method does
// that — it only surfaces liveness/progress for observability.
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
			}
		}
	}
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
