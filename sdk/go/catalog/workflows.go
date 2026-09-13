package catalog

import (
	"fmt"
	"iter"
	"strconv"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// BareRef is the claim-check ref the wire trades in. Hand-written with SNAKE_CASE tags to match
// `runtime/handler/internal/wire`, NOT the generated protobuf type: the generated Nexus client lives in
// the handler module (unimportable here) and protojson would emit camelCase against a decoder
// expecting snake_case — the exact trap `handler/nexus.go` hand-wires its own operation to avoid.
type BareRef struct {
	Sha256 string            `json:"sha256"`
	Size   uint64            `json:"size"`
	Meta   map[string]string `json:"meta,omitempty"`
}

// EntryInput is the kontra.actor:run operation's input. Field-for-field the peer of
// `runtime/handler/internal/wire.EntryInput` and Python's `entry_input`.
type EntryInput struct {
	Units          []any          `json:"units"`
	InputRef       *BareRef       `json:"input_ref,omitempty"`
	ReturnRef      bool           `json:"return_ref"`
	RunID          string         `json:"run_id"`
	NodeID         string         `json:"node_id"`
	IdempotencyKey string         `json:"idempotency_key"`
	ExpectedDigest string         `json:"expected_digest"`
	Method         string         `json:"method"`
	SessionID      string         `json:"session_id"`
	Params         map[string]any `json:"params,omitempty"`
}

// Batch is a set of Units living in the object store, addressed by a ref — the currency.
//
//	A Dataset yields Batches. A Method call returns (results, dropped), both Batches.
//
// N, Isolated and Machine come off the ref's meta, so counting, "did anything get dropped" and
// "which Machine ran this" cost nothing: the Dropped half of a Call's answer (ADR 0028 §4) reports
// a drop count with no fetch on a clean Batch.
//
// Distinct from kontra.Batch, which is the CALLEE's view — the thing a Method's author ranges
// over. Same word, opposite ends of the call.
type Batch struct {
	Ref BareRef
	// N is how many records the Batch holds.
	N int
	// Isolated is how many Units the producing Method permanently dropped.
	Isolated int
	// Done is false when the producer returned before covering its input.
	Done bool
	// FailuresSha addresses the dropped Units, when there are any.
	FailuresSha string
	// Machine is the host that ran the Method that produced this Batch, stamped into the meta by
	// the handler and read here with no fetch. EMPTY MEANS UNRECORDED, which is a real answer: a
	// Batch paged out of a Dataset was produced by the lake and not by a Machine, and an actor
	// host older than this contract reports none. Peer of Python's `Batch.machine`; the writer
	// forwards it so a published row can say which Machine produced it.
	Machine string

	actor, version string
}

func batchFromRef(ref BareRef, actor, version string) *Batch {
	atoi := func(k string) int {
		n, _ := strconv.Atoi(ref.Meta[k])
		return n
	}
	return &Batch{
		Ref:         ref,
		N:           atoi("n"),
		Isolated:    atoi("isolated"),
		Done:        ref.Meta["done"] != "false",
		FailuresSha: ref.Meta["failures"],
		Machine:     ref.Meta["machine"],
		actor:       actor,
		version:     version,
	}
}

// Rows materializes this Batch's records INTO YOUR WORKFLOW. Explicit because it is the one
// thing the ref exists to avoid — use it at the end of a pipeline, on something small.
func (b *Batch) Rows(ctx workflow.Context) ([]any, error) {
	return fetchUnits(ctx, b.Ref, b.actor, b.version)
}

// Dropped is the drops this Batch carries, as a *Dropped. Call and Session.Call hand it back as a
// separate value; the iterating form (Iter) yields (results, error) — the two-value iter.Seq2 shape
// where ignoring the error is visible — so a caller that iterates reads the drops off the Batch
// through this, the peer of how Python's iterator yields (results, dropped).
func (b *Batch) Dropped() *Dropped { return &Dropped{batch: b} }

// Failures returns the Units the producing Method dropped. Empty without a fetch when none were.
func (b *Batch) Failures(ctx workflow.Context) ([]any, error) {
	if b.Isolated == 0 || b.FailuresSha == "" {
		return nil, nil
	}
	return fetchUnits(ctx, BareRef{Sha256: b.FailuresSha}, b.actor, b.version)
}

// Batches re-pages this Batch into Batches of at most size — the peer of Dataset.Batches, and
// the answer to "how big is what the last Method emitted?".
//
// A caller sizes its INPUT pages, but a Method's fan-out belongs to its author: a 1→50 Method
// turns a 200-unit page into 10,000 units, and handing that to the next Actor is one activity on
// one worker with one oversized blob. `b.N` is free, so guard with it and skip this entirely
// when a Method is 1:1.
func (b *Batch) Batches(ctx workflow.Context, size int) iter.Seq2[*Batch, error] {
	return func(yield func(*Batch, error) bool) {
		if b.N == 0 {
			return
		}
		if size <= 0 {
			yield(nil, fmt.Errorf("batch size must be positive, got %d", size))
			return
		}
		if b.N <= size {
			// ALREADY FITS — yield self and schedule nothing, so a caller can range over this
			// unconditionally instead of guarding with `if resolved.N > size`. A 1:1 Method
			// pays nothing for the loop.
			yield(b, nil)
			return
		}
		ao := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
			TaskQueue:           DatasetQueue,
			StartToCloseTimeout: 5 * time.Minute,
		})
		var out struct {
			Refs []BareRef `json:"refs"`
		}
		err := workflow.ExecuteActivity(ao, SplitBatchActivity,
			map[string]any{"sha256": b.Ref.Sha256, "size": size}).Get(ctx, &out)
		if err != nil {
			yield(nil, err)
			return
		}
		for _, r := range out.Refs {
			// The split refs are minted by the orchestrator, so their meta names no Machine —
			// but the ROWS in them are still the ones this Batch's Machine produced. Carried the
			// same way actor and version are: slicing a result does not change who produced it.
			chunk := batchFromRef(r, b.actor, b.version)
			chunk.Machine = b.Machine
			if !yield(chunk, nil) {
				return
			}
		}
	}
}

// SplitBatchActivity re-pages a Batch. Served by the orchestrator's dataset worker.
const SplitBatchActivity = "splitBatch"

func fetchUnits(ctx workflow.Context, ref BareRef, actor, version string) ([]any, error) {
	ao := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue:           SharedQueue(actor, version),
		StartToCloseTimeout: 5 * time.Minute,
	})
	var payload any
	if err := workflow.ExecuteActivity(ao, FetchBlobActivity, ref).Get(ctx, &payload); err != nil {
		return nil, err
	}
	switch v := payload.(type) {
	case []any:
		return v, nil
	case map[string]any: // a ref minted before the envelope split
		list, _ := v["results"].([]any)
		return list, nil
	default:
		return nil, nil
	}
}

// ActorHandle is a deployed Actor, addressed by (name, version). Cheap and stateless — build it
// wherever; nothing happens until you open a scope or dispatch.
type ActorHandle struct {
	Name, Version string
	endpoint      string
	key           string
	// How many Method calls this handle has dispatched, used only to keep two node ids apart
	// inside ONE workflow task — see {@link nextNodeSeq}. An ordinary field and not shared state:
	// a handle is created by workflow code, so it belongs to one execution and replays with it.
	dispatches int64
}

// nextNodeSeq is a node-id suffix that costs no history event.
//
// TWO DETERMINISTIC SOURCES, AND NEITHER IS A MARKER:
//
//	the history length   `GetInfo(ctx).GetCurrentHistoryLength()` is the same number at the same
//	                     point of a replay, and grows as commands are written — so two dispatches
//	                     separated by any command differ.
//	this handle's count   a plain field, incremented here. Workflow code re-executes from the top
//	                     on replay, so the Nth call through one handle is the Nth call again.
//
// THE PAIR IS WHAT CLOSES THE COMMON CASE. History length alone is identical for two dispatches
// issued inside ONE workflow task, which is exactly the collision `workflow.SideEffect(workflow.Now)`
// also failed to prevent — every call in one task saw the same instant. A loop over one handle is
// the shape that produces it, and the counter distinguishes those.
//
// WHAT IS STILL POSSIBLE, said plainly: two dispatches in one workflow task through two SEPARATE
// handles, at the same history length, collide. That is no worse than before this change and it is
// not a state this SDK can rule out without persisting something — which is the event this exists
// to stop paying. A caller who needs a guaranteed-distinct id passes `WithNodeID`.
func (h *ActorHandle) nextNodeSeq(ctx workflow.Context) string {
	h.dispatches++
	return strconv.FormatInt(int64(workflow.GetInfo(ctx).GetCurrentHistoryLength()), 36) +
		"-" + strconv.FormatInt(h.dispatches, 36)
}

// Actor returns a handle on a deployed Actor.
func Actor(name, version string) *ActorHandle {
	return &ActorHandle{Name: name, Version: version, endpoint: EndpointName(name, version)}
}

// Key binds a virtual-object KEY — `Actor("crawler","1").Key("acme.com")`. One Batch at a time
// per key, and `object_state` is scoped to it (ADR 0022). The Go peer of Python's handle[key].
func (h *ActorHandle) Key(k string) *ActorHandle {
	c := *h
	c.key = k
	return &c
}

// Call runs one Method over one Batch WITHOUT opening a Session — a single load/run/close on the
// actor's shared queue, which is what an Activity was (ADR 0023 §9). It returns the results, the
// drops and an error as three distinct values (ADR 0028 §4), the same shape Session.Call returns:
//
//	verdicts, dropped, err := ns.Call(ctx, "ask", batch)
//
// Opening a Session is no longer the price of a chainable result — a handle reaches a Method on its
// own, and `results` is a Batch that feeds the next Call directly. Open() only PINS successive
// calls to one process (see Session.Open); this one does not, so two Calls on a bare handle may
// land on two workers. `dropped` is a distinct type from `err` on purpose: a permanently dropped
// Unit is not the call failing, and Go's multiple returns would otherwise let a caller confuse
// them. `dropped.Len()` costs no fetch.
func (h *ActorHandle) Call(ctx workflow.Context, method string, units any, opts ...CallOption) (*Batch, *Dropped, error) {
	b, err := h.dispatch(ctx, method, "", units, opts...)
	if err != nil {
		return nil, nil, err
	}
	return b, &Dropped{batch: b}, nil
}

// Dropped is the Units a Call permanently dropped — the middle value of Call's three (ADR 0028 §4),
// and the Go peer of Python's Dropped. It is a DISTINCT TYPE from error so a caller cannot mistake
// a dropped Unit for the call failing: isolation is not an error and does not fail a workflow on
// the framework's judgement (ADR 0023 §14), so it is the caller who reads the count and decides.
//
// Len and Any cost NO FETCH — the isolation count rides on the producing Batch's ref meta, the same
// way results.N does. Rows pays one fetch and exists for one reason: to hand the lost Units back
// for a retry.
type Dropped struct{ batch *Batch }

// Len is how many Units were permanently dropped. No fetch — a sweep that dropped everything and
// one that found nothing are told apart by this, not by an empty result.
func (d *Dropped) Len() int {
	if d == nil || d.batch == nil {
		return 0
	}
	return d.batch.Isolated
}

// Any reports whether anything was dropped. No fetch.
func (d *Dropped) Any() bool { return d.Len() > 0 }

// Rows materializes the dropped Units — for handing back to a retry. Empty without a fetch when
// nothing was dropped.
func (d *Dropped) Rows(ctx workflow.Context) ([]any, error) {
	if d == nil || d.batch == nil {
		return nil, nil
	}
	return d.batch.Failures(ctx)
}

// Session is one activated Actor instance — the scope §4 makes explicit. Go has no `async with`;
// the idiom is the one Go already has:
//
//	s, err := crawler.Open(ctx)
//	if err != nil { return err }
//	defer s.Close(ctx)
type Session struct {
	handle *ActorHandle
	id     string
}

// Open activates the Actor and pins every call in this scope to that one process, by giving the
// Session its own task queue (ADR 0023 §6). Losing the host FAILS the scope rather than silently
// re-activating: an orphaned queue means calls sit until ScheduleToStart fires.
func (h *ActorHandle) Open(ctx workflow.Context) (*Session, error) {
	// Deterministic and replay-stable — never uuid.New(), which would produce a different id on
	// replay and address a queue nobody polls. It also becomes part of a task-queue name, so it
	// stays short.
	var sid string
	if err := workflow.SideEffect(ctx, func(workflow.Context) any {
		return workflow.GetInfo(ctx).WorkflowExecution.RunID[:8] +
			strconv.Itoa(int(workflow.Now(ctx).UnixNano()%1e6))
	}).Get(&sid); err != nil {
		return nil, err
	}
	keyed := ""
	if h.key != "" {
		keyed = "[" + h.key + "]"
	}
	ao := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue:           SessionsQueue(h.Name, h.Version),
		StartToCloseTimeout: 5 * time.Minute,
		// The same line Python's open writes, so one transcript reads both SDKs' scopes alike.
		Summary: fmt.Sprintf("open %s@%s%s %s", h.Name, h.Version, keyed, sid),
	})
	if err := workflow.ExecuteActivity(ao, OpenSessionActivity, map[string]any{
		"session_id": sid, "actor": h.Name, "version": h.Version, "key": h.key,
	}).Get(ctx, nil); err != nil {
		return nil, err
	}
	return &Session{handle: h, id: sid}, nil
}

// ID is the Session's identity — the suffix of its task queue.
func (s *Session) ID() string { return s.id }

// Close ends the scope. Deferred, so it runs on every exit path including a failure — which is
// exactly when a loaded resource must not leak.
func (s *Session) Close(ctx workflow.Context) {
	if s == nil || s.id == "" {
		return
	}
	// A disconnected context, so the close still runs when the workflow is already failing.
	dctx, cancel := workflow.NewDisconnectedContext(ctx)
	defer cancel()
	ao := workflow.WithActivityOptions(dctx, workflow.ActivityOptions{
		TaskQueue:              SessionQueue(s.handle.Name, s.handle.Version, s.id),
		StartToCloseTimeout:    30 * time.Second,
		ScheduleToStartTimeout: time.Minute,
		RetryPolicy:            &temporal.RetryPolicy{MaximumAttempts: 3},
		Summary:                fmt.Sprintf("close %s@%s %s", s.handle.Name, s.handle.Version, s.id),
	})
	_ = workflow.ExecuteActivity(ao, CloseSessionActivity,
		map[string]any{"session_id": s.id}).Get(dctx, nil)
	s.id = ""
}

// Call runs one Method of this Session over one Batch and returns the results, the drops and an
// error as three distinct values (ADR 0028 §4). Opening the Session pinned this and every other
// call in the scope to one process (see Open); the results Batch feeds the next Method directly.
//
// `units` is either a []any of values or a *Batch another Method returned; a *Batch travels BY
// REF and is never materialized here.
//
// The three values are three different claims. `results` is what committed; `dropped` is the Units
// the actor permanently dropped, which is NOT the call failing — nothing fails a Batch on the
// framework's judgement (§14), so a caller reads it and decides; `err` is the dispatch itself
// failing. Drops ride their own type so Go's multiple returns cannot let a caller mistake a dropped
// Unit for a broken call. `dropped.Len()` costs no fetch.
func (s *Session) Call(ctx workflow.Context, method string, units any, opts ...CallOption) (*Batch, *Dropped, error) {
	if s == nil || s.id == "" {
		return nil, nil, fmt.Errorf("%s.%s() outside its scope — Open() first (ADR 0023 §4)",
			s.handle.Name, method)
	}
	b, err := s.handle.dispatch(ctx, method, s.id, units, opts...)
	if err != nil {
		return nil, nil, err
	}
	return b, &Dropped{batch: b}, nil
}

// Iter runs one Method as an ITERATOR, yielding (results, error) per bounded chunk as each lands —
// the peer of Python's async-iterable Method call (ADR 0023 §8), for a caller that wants to ACT on
// results before the whole Batch finishes (branch on an early finding, feed a second Actor, stop
// early). Call is the awaiting form; this is the streaming one, and the Actor is identical between
// them.
//
//	for verdicts, err := range ns.Iter(ctx, "ask", pairs) {
//		if err != nil { return err }
//		rows, _ := verdicts.Rows(ctx)
//		if hit(rows) { break }        // the remaining chunks never dispatch
//	}
//
// TWO VALUES, results and error — the iter.Seq2 shape where ignoring the error is visible at the
// call site, and the honest peer of a raising `async for`. The drops ride the Batch: `verdicts.
// Dropped()` (or `verdicts.Isolated`) is where a caller reads them, because a Go iterator has only
// the two slots and the error has to be one of them.
//
// BREAKING is defined (ADR 0023 §8): each chunk is a COMPLETE Method call that finished before it
// was yielded, so leaving the range early simply never dispatches the chunks after it — nothing is
// cancelled and no Actor is left mid-Batch. The chunk width is BOUNDED at SafePageMax, because each
// yield is a workflow-visible event and per-record granularity over a 40,000-unit Batch would
// rebuild the history blow-up that once capped a run near 5,100 units.
//
// An Into option publishes every chunk into the named Dataset as it lands, so the caller streams to
// the lake and acts on each chunk at once.
func (h *ActorHandle) Iter(
	ctx workflow.Context, method string, units any, opts ...CallOption,
) iter.Seq2[*Batch, error] {
	return h.iter(ctx, method, "", units, opts...)
}

// Iter runs one Method of this Session as an iterator, pinned to the Session's process the same way
// Call is. See ActorHandle.Iter for the shape and the break/bound guarantees.
func (s *Session) Iter(
	ctx workflow.Context, method string, units any, opts ...CallOption,
) iter.Seq2[*Batch, error] {
	return func(yield func(*Batch, error) bool) {
		if s == nil || s.id == "" {
			yield(nil, fmt.Errorf("%s.%s() outside its scope — Open() first (ADR 0023 §4)",
				s.handle.Name, method))
			return
		}
		for b, err := range s.handle.iter(ctx, method, s.id, units, opts...) {
			if !yield(b, err) {
				return
			}
		}
	}
}

func (h *ActorHandle) iter(
	ctx workflow.Context, method, sessionID string, units any, opts ...CallOption,
) iter.Seq2[*Batch, error] {
	return func(yield func(*Batch, error) bool) {
		for chunk, err := range chunksOf(ctx, units) {
			if err != nil {
				yield(nil, err)
				return
			}
			b, derr := h.dispatch(ctx, method, sessionID, chunk, opts...)
			if !yield(b, derr) {
				return
			}
			if derr != nil {
				return
			}
		}
	}
}

// chunksOf splits a Method call's input into bounded pieces for the iterating form. A *Batch
// re-pages by ref through Batch.Batches (the splitBatch activity); a []any is sliced in place. The
// width is SafePageMax — the bound that keeps the iterator's workflow-event count to ceil(n/200)
// rather than one per record.
func chunksOf(ctx workflow.Context, units any) iter.Seq2[any, error] {
	return func(yield func(any, error) bool) {
		switch v := units.(type) {
		case *Batch:
			for sub, err := range v.Batches(ctx, SafePageMax) {
				if !yield(sub, err) {
					return
				}
				if err != nil {
					return
				}
			}
		case []any:
			for i := 0; i < len(v); i += SafePageMax {
				end := i + SafePageMax
				if end > len(v) {
					end = len(v)
				}
				if !yield(v[i:end], nil) {
					return
				}
			}
		case nil:
			// Nothing to iterate.
		default:
			yield(nil, fmt.Errorf("units must be []any or *Batch, got %T", units))
		}
	}
}

// CallOption tunes one dispatch.
type CallOption func(*callOpts)

type callOpts struct {
	params  map[string]any
	timeout time.Duration
	runID   string
	nodeID  string
	dest    OutputDest
}

// Params sets the run-wide config for this call.
func Params(p map[string]any) CallOption { return func(o *callOpts) { o.params = p } }

// Timeout bounds the whole dispatch.
func Timeout(d time.Duration) CallOption { return func(o *callOpts) { o.timeout = d } }

// Into names the output Dataset this call's results land in (ADR 0028 §2) — the congruent peer of
// Python's second positional argument, spelled as an option because Go has no optional positional.
// Pass a *DatasetWriter or a *DatasetHandle: the results are published into it as the call returns,
// so a caller's loop no longer holds the Batch and writes it. Omit it and the results stay a
// chainable Batch that materializes nothing.
func Into(dst OutputDest) CallOption { return func(o *callOpts) { o.dest = dst } }

func (h *ActorHandle) dispatch(
	ctx workflow.Context, method, sessionID string, units any, opts ...CallOption,
) (*Batch, error) {
	o := callOpts{timeout: 30 * time.Minute}
	for _, f := range opts {
		f(&o)
	}

	info := workflow.GetInfo(ctx)
	runID := o.runID
	if runID == "" {
		// The workflow ID, not the run id: it survives continue-as-new, so every attempt of one
		// logical run writes to one `units/run=…` partition.
		runID = info.WorkflowExecution.ID
	}
	nodeID := o.nodeID
	if nodeID == "" {
		// ── THE MARKER THAT RECORDED A VALUE THAT NEEDED NO RECORDING ────────────────────────
		//
		// This was `workflow.SideEffect(… workflow.Now(ctx) …)`, costing a `MarkerRecorded` event
		// on EVERY dispatch. `workflow.Now` is already replay-deterministic — that is its whole
		// contract — so the marker persisted something the SDK would have reproduced for free.
		//
		// AND IT DID NOT SOLVE THE COLLISION IT LOOKS LIKE IT IS GUARDING. Every call inside one
		// workflow task sees the SAME instant either way, marker or no marker, so two dispatches
		// in one task produced the same suffix before this change and would have kept doing so.
		// A counter is what actually makes them differ, and it is free: an ordinary variable in
		// workflow code is deterministic on replay because the workflow re-executes from the top.
		// Python's equivalent path already pays nothing (`workflow.uuid4()`).
		//
		// GATED, BECAUSE REMOVING A COMMAND IS A DETERMINISM CHANGE. An execution that is in
		// flight has a `MarkerRecorded` in its history at this point; replaying it against code
		// that no longer emits one is a non-determinism failure, not a smaller history. The gate
		// is the same mechanism `runtime/handler/workflow.go` uses for its search-attribute
		// upsert, and for the same reason.
		if workflow.GetVersion(ctx, "kontra-node-id", workflow.DefaultVersion, 1) >= 1 {
			nodeID = h.Name + "-" + h.nextNodeSeq(ctx)
		} else {
			var suffix string
			if err := workflow.SideEffect(ctx, func(workflow.Context) any {
				return strconv.FormatInt(workflow.Now(ctx).UnixNano(), 36)
			}).Get(&suffix); err != nil {
				return nil, err
			}
			nodeID = h.Name + "-" + suffix
		}
	}

	entry := EntryInput{
		Units:          []any{},
		ReturnRef:      true,
		RunID:          runID,
		NodeID:         nodeID,
		IdempotencyKey: h.key,
		Method:         method,
		SessionID:      sessionID,
		Params:         o.params,
	}
	n := 0
	switch v := units.(type) {
	case *Batch:
		// The seam: one Method's output becomes the next one's input without a row entering
		// this workflow. Type-switched rather than duck-typed — the failure mode of getting it
		// wrong is a large batch silently inlined into history.
		ref := v.Ref
		entry.InputRef = &ref
		n = v.N
	case []any:
		entry.Units = v
		n = len(v)
	case nil:
	default:
		return nil, fmt.Errorf("units must be []any or *Batch, got %T", units)
	}

	c := workflow.NewNexusClient(h.endpoint, ServiceName)
	fut := c.ExecuteOperation(ctx, RunOperation, entry, workflow.NexusOperationOptions{
		ScheduleToCloseTimeout: o.timeout,
		// METADATA, NOT PAYLOAD — the Method name is in `entry` too, but `entry` is a payload and
		// may be a claim-check ref, so this is the copy a reader can afford. Built by the same
		// rules as Python's, because `control/orchestrator/src/transcript.ts` recovers the Method from the
		// FIRST FIELD of this line: a Go dispatch with a format of its own put a Method name into
		// history that the transcript then reported as no Method at all.
		//
		// The count comes from the Batch when the units rode in by ref — `entry.Units` is empty on
		// that path, so reading it would label every chained dispatch "0 units".
		Summary: DispatchSummary(h.Name, h.Version, method, h.key, n),
	})
	var ref BareRef
	if err := fut.Get(ctx, &ref); err != nil {
		return nil, err
	}
	b := batchFromRef(ref, h.Name, h.Version)
	if o.dest != nil {
		// The caller named an output Dataset (ADR 0028 §2): publish the results into it as the
		// call returns, forwarding THIS Batch's own Machine so a multi-Machine run names each.
		if _, err := o.dest.publishInto(ctx, b); err != nil {
			return nil, err
		}
	}
	return b, nil
}
