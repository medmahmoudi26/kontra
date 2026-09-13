// The Batch an author ranges over, and the Dataset a Method pushes to (ADR 0023 §2, §3, §23;
// ADR 0028).
//
//	func crawl(s *kontra.Session, b *kontra.Batch, ds *kontra.Dataset) error {
//		for unit := range b.All() {
//			ds.Push(fetch(unit.Str("url")))
//		}
//		return b.Err()
//	}
//
// Two save points are tracked here and ADR 0028 §1 is the reason they are kept apart:
//
//	INPUT POSITION says a Unit is FINISHED. The iterator is framework-owned, so asking for the next
//	Unit is what tells the framework the last one is done and can be committed. An author who takes
//	the whole Batch instead (Units(), to run them concurrently) has no position to commit by, so
//	nothing commits until the Method returns — the honest cost of owning the concurrency, and the
//	only thing it costs.
//
//	OUTPUT is a Dataset, not a Unit. ds.Push(x) names no Unit; provenance the author cares about
//	goes INSIDE the record (ADR 0028 §1). The framework needs the record->Unit link only for resume
//	("everything pushed so far is durable, and I am past unit 7"), which the input cursor already
//	answers — so the link is gone from the author's surface.
//
// A push made INSIDE the loop is committed against the Unit the iterator is currently handing out —
// batch.current — which is byte-for-byte what the retired per-Unit emit did, only the author no
// longer spells the Unit, and the per-Unit commit map (ADR 0023 §17) makes it exactly-once with no
// key and no new burden.
//
// AN OUT-OF-LOOP PUSH IS IDENTIFIED BY AN EXPLICIT KEY, NOT BY ITS POSITION. A push made while no
// Unit is current — before the loop, after it, or from a goroutine spawned under Units() — belongs
// to no Unit's commit, so it collects on the Batch TAIL and folds into the call's results at Method
// exit (still durable at push time, so a concurrent crawler streams as it goes). The driver
// re-invokes the body from the top with the remaining Units after isolating one (ADR 0023 §13), and
// the inst's fields survive that in-memory re-invoke while author locals reset — so the same
// out-of-loop push RE-RUNS with the same or different bytes, and the framework must reconcile the
// re-runs. It CANNOT infer whether the author "re-pushed a changed version of the same record" or
// "deliberately pushed a different record" — both are the same control-flow position with different
// bytes. Three mechanisms that inferred identity from content or position each traded one failure
// for another (truncate-and-re-execute LOST a self-guarded push; accumulate-and-content-dedup
// DUPLICATED a content-varying push; (group, ordinal) first-wins DUPLICATED *and* LOST on a prefix
// shift). So identity is TOLD, the principle ADR 0023 §18 settled for this class of problem (Restate's
// ctx.run(key, …), Lambda's batchItemFailures by itemIdentifier): name the durable thing.
//
//	ds.Push(summary, kontra.Key("batch-summary"))
//
// Each keyed tail push is reconciled BY THAT KEY, FIRST-WRITE-WINS across re-invokes: a key already
// written in an earlier entry is SKIPPED BEFORE the durable write, so results and the store agree
// with no orphan blob; a key new to this call is written and folded. Position stops mattering — a
// push that appears only on the re-invoke carries a key nobody has written yet and is kept, while a
// push that precedes it carries a key already written and is skipped. A within-entry duplicate under
// two distinct author keys is kept (the author named two things). An OUT-OF-LOOP PUSH WITH NO KEY
// PANICS at the call site (converted to a whole-call error by the driver's recover), the loud peer
// of Python raising MissingPushKey — a generated key would be a fourth guess, wrong the same way.
//
// Pure sequencing, no runtime import: the durable half is three calls on the Sink the host passes
// in — Enter when a Unit is handed out, Record per push, Commit when a Unit is finished. This file
// is the peer of Python's actorkit/batch.py, and deliberately reads like it.
package core

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"iter"
	"sync"
)

// tailBase keeps a tail record's synthetic index far above any real Unit index, so the two never
// share a shard prefix in the blob layout — output is offset-addressed, not Unit-addressed
// (ADR 0028 §1). Peer of Python's _TAIL_BASE.
const tailBase = 1_000_000

// tailSpan is the synthetic-index span a tail key hashes into, above tailBase. Wide enough that two
// distinct keys colliding on one synthetic index is negligible; even then their records differ in
// content sha and so land on different blobs. Peer of Python's _TAIL_SPAN.
const tailSpan = 1_000_000_000

// tailIndex is the synthetic Unit index a tail record commits under — a pure function of its KEY,
// so the SAME out-of-loop push lands on the SAME blob key across an isolation re-invoke or a
// genuine-death retry, making a content-deterministic re-push an idempotent overwrite instead of an
// orphan (ADR 0028 §1, ADR 0015). Distinct keys land on distinct indices, so two keys carrying
// identical bytes never collide onto one blob. Peer of Python's _tail_index.
func tailIndex(key string) int {
	sum := sha256.Sum256([]byte(key))
	h := binary.BigEndian.Uint64(append([]byte{0, 0}, sum[:6]...))
	return tailBase + int(h%tailSpan)
}

// PushOption configures one Push. The only option today is Key, which names the durable identity of
// an out-of-loop push (ADR 0028) — an in-loop push needs none, the current Unit is its identity.
type PushOption func(*pushOpts)

type pushOpts struct {
	key   string
	keyed bool
}

// Key names the durable identity of a push made with no current Unit (before/after the loop, or from
// a goroutine under Units()). The framework reconciles that push across an isolation re-invoke by
// this key, first-write-wins — never by its control-flow position (ADR 0028). An out-of-loop push
// without a Key panics; an in-loop push ignores it (the current Unit is the identity).
func Key(k string) PushOption { return func(o *pushOpts) { o.key = k; o.keyed = true } }

// Sink is the framework's durable half of the author's loop, supplied by the host.
type Sink interface {
	// Enter is called when a Unit is handed to the author.
	Enter(u *Unit)
	// Record makes one pushed record durable NOW and returns what stands in for it in the
	// commit (a ref when an object store is configured, the record itself otherwise).
	Record(u *Unit, rec any) (any, error)
	// Commit is called when a Unit is finished — the durable done-marker a retry reads to skip it.
	Commit(u *Unit) error
}

// Item is one input Unit as the host hands it in: its position in the Batch and its payload.
// The index is the host's, not the slice's, so a resumed Batch keeps the original positions of
// the Units that are left.
type Item struct {
	Index int
	Value any
}

// Unit is one indivisible piece of work — the grain of retry and of commit (ADR 0023 §17).
type Unit struct {
	// Value is the author's payload — whatever JSON the caller put in the Batch. Usually a
	// map[string]any; Str reads a field off one.
	Value any
	// Index is this Unit's position in the Batch, and the index half of its commit key
	// (ADR 0023 §17).
	Index int

	sink  Sink
	state UnitState

	// mu guards out. A push attributed to this Unit appends here, and Out reads it; the author
	// may run their Units in real goroutines (which ADR 0023 §18 exists to allow), so a plain
	// slice would tear. ADR 0023 §23 names this as the one thing Go must add that Python does not.
	mu  sync.Mutex
	out []any
}

// appendOut records one committed value against this Unit, under its own lock. Called by
// Batch.push with the Batch lock also held; nothing takes u.mu then b.mu, so the nesting is safe.
func (u *Unit) appendOut(v any) {
	u.mu.Lock()
	u.out = append(u.out, v)
	u.mu.Unlock()
}

// Out is what was pushed while this Unit was the iterator's current Unit, as committed (refs, or
// the records themselves when no object store is configured). Framework-owned: the host reads it
// to write the commit. The author never names this Unit — the framework attributes a push to
// whichever Unit the iterator is handing out (ADR 0028 §1).
func (u *Unit) Out() []any {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]any(nil), u.out...)
}

// Str reads a string field off a Unit whose payload is a JSON object — the common case, and the
// difference between a one-line loop body and three lines of type assertion. Absent field, wrong
// type or a non-object payload all give "".
func (u *Unit) Str(field string) string {
	m, ok := u.Value.(map[string]any)
	if !ok {
		return ""
	}
	s, _ := m[field].(string)
	return s
}

// Into decodes this Unit's payload into `target`, a pointer to your own struct — the typed peer
// of Str, and of Python's `unit.value` when a Method declares `takes=`.
//
//	var t Target
//	if err := unit.Into(&t); err != nil { continue }   // this Unit's payload is malformed
//	ds.Push(Page{Host: t.Host})
//
// Go cannot give Value a declared type the way Python can: methods take no type parameters
// (golang/go#77273 is accepted and unscheduled), so a typed `Value` would have to live on the
// receiver and make Batch generic — which would then have to be generic through the whole host.
// A decode call is the honest shape, and it is what encoding/json users already reach for.
//
// An error here means THIS Unit's payload does not fit; returning it from the Method isolates
// that Unit and leaves the rest of the Batch alone (ADR 0023 §13).
func (u *Unit) Into(target any) error {
	b, err := json.Marshal(u.Value)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, target)
}

// State is this Unit's durable resume scratch (see UnitState). Outside a hosted Unit it is a
// safe no-op, so Method bodies unit-test without a host.
func (u *Unit) State() UnitState {
	if u.state == nil {
		return noopUnitState{}
	}
	return u.state
}

// BindState attaches this Unit's durable scratch. The host calls it from Enter; authors never do.
func (u *Unit) BindState(s UnitState) { u.state = s }

// Dataset is where a Method pushes its output (ADR 0028 §2) — the third parameter, and the Go peer
// of Python's kontra.batch.Dataset. From inside a Method the destination is indistinguishable
// whether the caller named it or not, which is the asymmetry ADR 0028 exists to remove.
//
//	func ask(s *kontra.Session, b *kontra.Batch, ds *kontra.Dataset) error {
//		for unit := range b.All() {
//			ds.Push(Verdict{Domain: unit.Str("domain"), OK: ok})
//		}
//		return b.Err()
//	}
//
// Push names nothing and RETURNS nothing: a write failure surfaces at b.Err() or at Method exit,
// the Kafka-producer model where you check the flush and not every send (ADR 0028 §3).
//
// Constructed with no Batch it is a plain COLLECTOR, which is the whole testability argument
// (ADR 0028 §rejected/bare-callable): a Method body is unit-tested by handing it a collecting
// Dataset and reading Records(), with no host, no queue and no object store. The framework hands
// the real one, backed by the Batch, so the same body streams to the lake in production.
type Dataset struct {
	// batch routes each push to the Unit the iterator is on (or the tail). nil -> a collector: the
	// unit-test path, where records land in `collected` and Records() reads them back.
	batch *Batch

	mu        sync.Mutex
	collected []any
}

// NewDataset builds the output Dataset the framework hands a Method, backed by the Batch so a push
// commits against the Unit the iterator is on. The host owns this; authors never call it.
func NewDataset(b *Batch) *Dataset { return &Dataset{batch: b} }

// CollectingDataset builds a Dataset with no backend — a plain collector for testing a Method body
// with no host, no queue and no object store. Read Records() for what the body pushed, in push
// order. Peer of Python's kontra.testing.collecting_dataset.
func CollectingDataset() *Dataset { return &Dataset{} }

// Push appends one record to the output. It names no Unit and returns nothing: a write failure is
// held and surfaces at b.Err() or at Method exit (ADR 0028 §3), never at the call.
//
// An in-loop push takes no option — the Unit the iterator is on is its identity. A push made with no
// current Unit (before/after the loop, or from a goroutine under Units()) REQUIRES kontra.Key(...):
// the framework reconciles it across an isolation re-invoke by that key, first-write-wins, and an
// out-of-loop push without one panics at the call site (ADR 0028).
//
// ORDERING, which an author fanning Units across goroutines needs to be able to reason about
// (ADR 0023 §23). Pushes are serialized, so no record is lost or torn under concurrency. A record
// pushed while a Unit is the iterator's current Unit attributes to that Unit and appears in push
// order within it; Units assemble into the result in input-index order. A keyed record pushed with
// no current Unit folds into the result after the per-Unit output, in first-write order of its key.
func (d *Dataset) Push(record any, opts ...PushOption) {
	if d.batch == nil {
		d.mu.Lock()
		d.collected = append(d.collected, record)
		d.mu.Unlock()
		return
	}
	var o pushOpts
	for _, opt := range opts {
		opt(&o)
	}
	d.batch.push(record, o.key, o.keyed)
}

// Records is what a collector Dataset was handed, in push order — the unit-test read side. Empty
// on a framework-backed Dataset, whose output lives on the Batch instead.
func (d *Dataset) Records() []any {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]any(nil), d.collected...)
}

// Batch is the set of Units handed to one Method call, as the iterator the author ranges over.
type Batch struct {
	sink Sink

	mu      sync.Mutex
	pending []Item        // not yet handed out, popped from the end -> input order
	held    map[int]*Unit // handed out, not yet committed
	handed  []*Unit       // every Unit handed out, in order — Emitted reads it
	current *Unit         // the Unit the iterator is on — who a push and a raise are blamed on
	err     error         // the first framework-side (commit) failure

	// tailSlots holds records pushed with no current Unit (before/after the loop, or a goroutine
	// under Units()), keyed by the AUTHOR'S explicit key. They belong to no Unit's commit (ADR 0023
	// §17), so they fold into results at Method exit — made durable at push time like any other
	// record. Keyed rather than accumulated, FIRST-WRITE-WINS across an isolation re-invoke (where
	// inst fields persist): a re-push finds its key filled and is dropped, so an inst-guarded push
	// that ran only on entry 1 survives, a content-varying one keeps its first value with no orphan
	// blob, and a push that appears only on the re-invoke carries a fresh key and is kept. tailOrder
	// records first-write order (map iteration is unordered), which is the fold order. NOT reset
	// across entries — that retention is the mechanism.
	tailSlots map[string]any
	tailOrder []string
	pushErr   error
}

// NewBatch builds the Batch a Method receives. The host owns this; authors never call it.
func NewBatch(sink Sink, todo []Item) *Batch {
	pending := make([]Item, len(todo))
	for i, it := range todo {
		pending[len(todo)-1-i] = it // reversed, so popping from the end yields input order
	}
	return &Batch{sink: sink, pending: pending, held: map[int]*Unit{}, tailSlots: map[string]any{}}
}

// push makes one pushed record durable NOW, attributed to the Unit the iterator is handing out
// (ADR 0028 §1: durability unchanged) — or, when no Unit is current, to the Batch tail under the
// author's explicit key. Serialized under b.mu so concurrent pushes neither tear nor lose a record.
// A store failure is HELD, not returned: push stays fire-and-forget and the failure surfaces at the
// next iterator boundary (All) and at Method exit (Settle), BEFORE the Unit it struck can commit —
// else a retry would skip that Unit and lose the record for good (ADR 0028 §consequence 5).
//
// An out-of-loop push with no key PANICS: it is an author error the framework refuses rather than a
// fourth guess at identity (ADR 0028). The driver's recover turns it into a whole-call error, the
// loud peer of Python raising MissingPushKey.
func (b *Batch) push(rec any, key string, keyed bool) {
	b.mu.Lock()
	unit := b.current
	if unit != nil {
		defer b.mu.Unlock()
		if b.sink == nil { // an unhosted Batch (a plain Method test that bound a Dataset to it)
			unit.appendOut(rec)
			return
		}
		committed, err := b.sink.Record(unit, rec)
		if err != nil {
			b.setPushErr(err)
			return
		}
		unit.appendOut(committed)
		return
	}
	if !keyed {
		b.mu.Unlock() // release before panicking so a recover cannot resume holding the lock
		panic("push() with no current Unit needs an explicit key: " +
			`ds.Push(record, kontra.Key("batch-summary")). A push made outside the loop belongs ` +
			"to no Unit's commit, so the framework reconciles it across an isolation re-invoke by " +
			"that key (ADR 0028) — it cannot infer identity from where you are in control flow.")
	}
	defer b.mu.Unlock()
	// No current Unit: the tail, under the author's explicit key. FIRST-WRITE-WINS: a key already
	// written in a prior entry is retained and this re-push is dropped BEFORE the durable write — so
	// an inst-guarded push kept from entry 1 is not overwritten, and a content-varying push writes no
	// second blob (results and store agree, no orphan). A key the author has not used yet is written
	// and folded, whichever entry it first appears on.
	if _, ok := b.tailSlots[key]; ok {
		return
	}
	if b.sink == nil {
		b.tailSlots[key] = rec
		b.tailOrder = append(b.tailOrder, key)
		return
	}
	committed, err := b.sink.Record(&Unit{Index: tailIndex(key)}, rec)
	if err != nil {
		b.setPushErr(err)
		return
	}
	b.tailSlots[key] = committed
	b.tailOrder = append(b.tailOrder, key)
}

// orderedTail is the tail records in fold order — first-write order of each key, which is push order
// for the common single-threaded case. Caller holds b.mu.
func (b *Batch) orderedTail() []any {
	out := make([]any, 0, len(b.tailOrder))
	for _, k := range b.tailOrder {
		out = append(out, b.tailSlots[k])
	}
	return out
}

// setPushErr keeps the FIRST failed push. Caller holds b.mu.
func (b *Batch) setPushErr(err error) {
	if b.pushErr == nil {
		b.pushErr = err
	}
}

// hasPushErr reports whether a push has failed. Read at each iterator boundary so a failed write
// stops the loop BEFORE the Unit it struck commits.
func (b *Batch) hasPushErr() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.pushErr != nil
}

// Tail is the records pushed with no current Unit, in push order, as committed — folded into the
// call's results after the per-Unit outputs. Framework-owned: the host reads it to assemble the
// response envelope (ADR 0028 §1).
func (b *Batch) Tail() []any {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.orderedTail()
}

// All is the author's loop: `for unit := range b.All()`. Asking for the next Unit commits the
// last one, which is what makes a death mid-Batch resume at the first Unit the author had not
// finished. Breaking out early is fine — the Units taken and not iterated past are committed by
// Settle on the way out.
//
// A commit failure stops the iteration and is reported by Err, the ordinary Go iterator idiom:
// range-over-func cannot return an error, and swallowing a failed durable write would report a
// Unit as finished when nothing recorded it.
func (b *Batch) All() iter.Seq[*Unit] {
	return func(yield func(*Unit) bool) {
		for {
			// A failed push surfaces HERE, before the Unit it struck commits (ADR 0028
			// §consequence 5): a committed Unit is skipped on retry, so committing one whose
			// durable write failed would lose the record for good. Err reports it to the driver.
			if b.hasPushErr() {
				return
			}
			if err := b.finishCurrent(); err != nil {
				b.setErr(err)
				return
			}
			u := b.handOut()
			if u == nil {
				return
			}
			b.setCurrent(u) // POSITION — only the iterator has one; Units() deliberately does not
			if !yield(u) {
				return // the author broke out (or raised); current stays set, to blame or settle
			}
		}
	}
}

// Units hands over every remaining Unit at once, for an author who runs them concurrently
// themselves.
//
// Taking the Batch this way gives up per-Unit commit: with no position there is nothing to commit
// by, so the whole Batch commits when the Method returns. Pushes made from the spawned goroutines
// land in the Batch tail (there is no current Unit) and fold into results at Method exit, in push
// order.
func (b *Batch) Units() []*Unit {
	b.mu.Lock()
	n := len(b.pending)
	b.mu.Unlock()
	out := make([]*Unit, 0, n)
	for i := 0; i < n; i++ {
		if u := b.handOut(); u != nil {
			out = append(out, u)
		}
	}
	return out
}

// Pending is how many Units have not been handed to the author — the remainder a re-invoked
// Method receives (ADR 0023 §13).
func (b *Batch) Pending() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.pending)
}

// Emitted is everything this Batch has produced: each Unit's own output in UNIT order — not
// completion order, so an author who fans their Units out across goroutines still gets a
// deterministic answer — then the tail (records pushed with no current Unit) in push order. It is
// the assertion surface for testing a Method whose Dataset was bound to this Batch; the host reads
// each Unit's own Out() and Tail() instead, because it commits per Unit.
func (b *Batch) Emitted() []any {
	b.mu.Lock()
	handed := append([]*Unit(nil), b.handed...)
	tail := b.orderedTail()
	b.mu.Unlock()
	sortUnits(handed)
	var out []any
	for _, u := range handed {
		out = append(out, u.Out()...)
	}
	return append(out, tail...)
}

// Err is the first framework-side failure during iteration: a durable commit that did not land, or
// a push whose durable write failed (ADR 0028 §3). It is NOT the author's error, which the Method
// returns in the ordinary way. The driver checks it after every Method entry, which is how a
// fire-and-forget push failure fails the whole call.
func (b *Batch) Err() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return b.err
	}
	return b.pushErr
}

// Settle commits every Unit the author took and did not iterate past — the tail of a loop that
// broke early, and the whole Batch when the author held it.
func (b *Batch) Settle() error {
	b.mu.Lock()
	// Method exit is the last checkpoint: a push that failed after the final commit (or from a
	// spawned goroutine) has nowhere later to surface, so it must surface before anything held
	// commits and the call reports success — else a committed Unit hides a lost record.
	if b.pushErr != nil {
		perr := b.pushErr
		b.mu.Unlock()
		return perr
	}
	b.current = nil
	held := make([]*Unit, 0, len(b.held))
	for _, u := range b.held {
		held = append(held, u)
	}
	b.mu.Unlock()

	// Committed in index order so a partial batch's durable writes are ordered like its input.
	sortUnits(held)
	for _, u := range held {
		if err := b.commit(u); err != nil {
			return err
		}
	}
	return nil
}

// Blame is the Unit the iterator was on when the Method returned an error (ADR 0023 §13), taken
// out of the committable set so the host can record it as a failure instead. Every other Unit the
// author took is committed on the way out: what they pushed is already durable, and re-running
// them would be a second execution of finished work.
//
// nil when the author held the Batch rather than ranging over it — there is no position to blame,
// and inventing one would attribute a failure to whichever Unit was pulled last.
func (b *Batch) Blame() (*Unit, error) {
	b.mu.Lock()
	u := b.current
	b.current = nil
	if u != nil {
		delete(b.held, u.Index)
	}
	b.mu.Unlock()
	if u == nil {
		return nil, nil
	}
	return u, b.Settle()
}

// finishCurrent commits the Unit the author has just finished with (nil on the first call).
func (b *Batch) finishCurrent() error {
	b.mu.Lock()
	done := b.current
	b.current = nil
	b.mu.Unlock()
	if done == nil {
		return nil
	}
	return b.commit(done)
}

// handOut takes the next pending Unit, registers it as held and tells the sink. nil when the
// Batch is exhausted.
func (b *Batch) handOut() *Unit {
	b.mu.Lock()
	if len(b.pending) == 0 {
		b.mu.Unlock()
		return nil
	}
	// Both the loop (All) and Units() route through here. Only All sets a position (setCurrent);
	// Units() hands out without one, so a push from a spawned goroutine has no current Unit and must
	// carry a key.
	it := b.pending[len(b.pending)-1]
	b.pending = b.pending[:len(b.pending)-1]
	u := &Unit{Value: it.Value, Index: it.Index, sink: b.sink}
	b.held[it.Index] = u
	b.handed = append(b.handed, u)
	b.mu.Unlock()
	if b.sink != nil {
		b.sink.Enter(u)
	}
	return u
}

// commit writes a Unit's durable done-marker exactly once (a Unit already dropped from held has
// been committed or blamed, and must not be committed twice).
func (b *Batch) commit(u *Unit) error {
	b.mu.Lock()
	_, ok := b.held[u.Index]
	delete(b.held, u.Index)
	b.mu.Unlock()
	if !ok || b.sink == nil {
		return nil
	}
	return b.sink.Commit(u)
}

// setCurrent records where the iterator is. Only All() calls it: position is what says a Unit is
// finished and who a failure or a push belongs to, and an author who took the whole Batch has none.
func (b *Batch) setCurrent(u *Unit) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.current = u
}

func (b *Batch) setErr(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err == nil {
		b.err = err
	}
}

// sortUnits orders units by index (insertion sort — a settled tail is tiny).
func sortUnits(us []*Unit) {
	for i := 1; i < len(us); i++ {
		for j := i; j > 0 && us[j].Index < us[j-1].Index; j-- {
			us[j], us[j-1] = us[j-1], us[j]
		}
	}
}
