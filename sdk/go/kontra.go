// Package kontra is the Go actor SDK author surface — the parallel of Python's actorkit.
// An author writes a small main.go that registers a load -> method* -> close lifecycle and
// calls Serve(); a Go actor is RUN BY GO (`go run .` locally, the compiled binary in its
// image), never by a CLI — the same run-by-language boundary the Python seam draws.
//
// Go implements the CALLEE half here (ADR 0023 §22): Actors are written in Go. What Go gets is
// full parity on the author's loop (§23; ADR 0028): many named Methods, a Batch the author ranges
// over, and an output Dataset it pushes to.
//
//	func main() {
//		a := kontra.New()
//		a.Method("resolve", resolve)
//		a.Serve()
//	}
//
//	func resolve(s *kontra.Session, b *kontra.Batch, ds *kontra.Dataset) error {
//		for unit := range b.All() {
//			ips, err := net.LookupHost(unit.Str("host"))
//			if err != nil {
//				continue // this host has no A record; the others still finish
//			}
//			ds.Push(map[string]any{"host": unit.Str("host"), "ips": ips})
//		}
//		return b.Err()
//	}
package kontra

import "github.com/medmahmoudi26/kontra-local/sdk/go/core"

// Unit is one indivisible piece of work — the grain of retry and of commit. Its Value is the
// caller's payload; Str/Into read fields off it. An author no longer names a Unit to produce
// output — the framework attributes a push to the Unit the iterator is on, and provenance the
// author cares about goes inside the pushed record (ADR 0028 §1).
type Unit = core.Unit

// Batch is the set of Units handed to one Method call — the thing the author ranges over. Asking
// for the next Unit is what commits the last one, so a death mid-Batch resumes at the first Unit
// the author had not finished.
type Batch = core.Batch

// Dataset is the output a Method pushes to — the third parameter, and the peer of the input Batch:
// both are things the caller hands over (ADR 0028 §2, §3). ds.Push(x) appends one record, names no
// Unit, and returns nothing; a write failure surfaces at b.Err() or at Method exit, the
// Kafka-producer flush model. Every granularity is the same call with no flag — one record per
// Unit, N per Unit, one per three, one for the whole Batch, or none at all.
type Dataset = core.Dataset

// PushOption configures one ds.Push. The only option is Key.
type PushOption = core.PushOption

// Key names the durable identity of a push made with no current Unit — before or after the loop, or
// from a goroutine spawned under b.Units(). The framework reconciles that push across an isolation
// re-invoke by this key, first-write-wins, never by its control-flow position (ADR 0028):
//
//	ds.Push(summary, kontra.Key("batch-summary"))
//
// An in-loop push needs none (the Unit the iterator is on is its identity); an out-of-loop push
// without a Key panics, the loud error the author hits the first time the Method runs.
func Key(k string) PushOption { return core.Key(k) }

// Session is the shared, mutable state one actor instance carries across its lifecycle (the Go
// peer of Python's `self`): resources opened in Load stay visible to every Method, so `extract`
// sees the browser `crawl` opened. Reach it via Get/Set, which are mutex-guarded — a Method may
// run its Units in real goroutines.
type Session = core.Session

// UnitState is a Unit's durable resume scratch, reached via unit.State() — a crawl frontier or a
// cursor, not a result store; the host clears it when the Unit commits. See ADR 0015.
type UnitState = core.UnitState

// `SessionState` was a fourth tier and RETIRED with ADR 0023 §19: durable means keyed,
// in-memory means scoped. The Session's own fields are its memory, `object_state` is the key's,
// `global_state` is the name's — and the one path that loses the Session's fields is host death,
// which fails the scope, so nothing was left to read it.

// GlobalState is cross-session, actor-name-scoped durable state a Method reaches via
// s.GlobalState(), with atomic ops (AddToSet/Incr/CompareAndSet) so concurrent sessions never
// lose updates. See ADR 0015.
type GlobalState = core.GlobalState

// Actor is the registry one main.go builds: identity + lifecycle + typed I/O. Construct with
// New(), register Load / Method* / Close (and optionally IO for schemas), then Serve().
type Actor struct{ reg core.Registry }

// New returns an empty actor registry.
func New() *Actor { return &Actor{} }

// Load registers the once-per-session resource open (a browser, a client, a pool) — run once
// before any Method and visible to every Method of the session.
func (a *Actor) Load(fn func(*Session) error) { a.reg.LoadFn = fn }

// Method registers a named, individually dispatchable Method. It receives the whole Batch and the
// output Dataset, and YOU write the loop (ADR 0028 §2):
//
//	a.Method("crawl", func(s *kontra.Session, b *kontra.Batch, ds *kontra.Dataset) error {
//		for unit := range b.All() {
//			ds.Push(fetch(unit.Str("url")))
//		}
//		return b.Err()
//	})
//
// ds.Push(x) appends one record to the output — it names no Unit (put the provenance you care
// about INSIDE the record) and returns nothing (a write failure surfaces at b.Err() or at Method
// exit, the Kafka-producer model). The framework commits a Unit as your loop moves past it — so a
// death mid-Batch resumes at the first Unit you had not finished. Bodies must be IDEMPOTENT: a
// Unit re-runs if death strikes between the work and its commit.
//
// Because push names no Unit, every granularity is the same call with no flag: one record per
// Unit, N per Unit, one per three Units, one for the whole Batch, or none at all. Run Units
// concurrently by taking them yourself (b.Units()) and push from the spawned goroutines — a
// parameter can be called from anywhere. Concurrent pushes are safe; see Dataset for the ordering.
// A push made with NO current Unit — before the loop, after it drains, or from such a goroutine —
// needs an explicit key: ds.Push(summary, kontra.Key("batch-summary")). The framework reconciles it
// across an isolation re-invoke by that key, first-write-wins, never by its position; an unkeyed
// out-of-loop push panics (ADR 0028 §consequence 6).
//
// Declare as many Methods as the Actor has jobs; the caller names the one it wants, and an Actor
// with a single Method needs no name at the call site. Two Methods of one Actor share the loaded
// resource and the Session, which is the point. Declaration ORDER means nothing — it is not a
// pipeline, and an unnamed dispatch against several Methods is refused rather than resolved by
// order (ADR 0023 §16).
//
// A METHOD IS ENTERED SEVERAL TIMES PER BATCH — once per isolated Unit failure (ADR 0023 §13).
// Locals reset between entries; the Session does not. Anything you accumulate across Units
// belongs on the Session, not in a local.
//
// Declare what the Method takes and emits with Takes/Emits; the catalog registers one operation
// per Method from them:
//
//	a.Method("fetch", fetch, kontra.Takes(Target{}), kontra.Emits(Page{}))
func (a *Actor) Method(name string, fn func(*Session, *Batch, *Dataset) error, opts ...MethodOption) {
	a.reg.AddMethod(name, fn, opts...)
}

// MethodOption declares something about a Method at registration.
type MethodOption = core.MethodOption

// Takes declares what one Unit of this Method's Batch carries — a zero value of the type, from
// which the catalog reflects the operation's input schema.
//
// It is per-METHOD, not per-Actor: an Actor has many Methods with different signatures (ADR 0023
// §9), and one Actor-level pair would typecheck every node against whichever Method happened to be
// declared first. Undeclared leaves the schema unset; the Method still registers and stays
// dispatchable by name.
func Takes(v any) MethodOption { return core.Takes(v) }

// Emits declares what this Method pushes per record — a zero value of the type.
//
// It cannot be inferred from the body, because a Method pushes rather than returns (ADR 0028 §3):
// a Method returns only an error. So it is stated.
func Emits(v any) MethodOption { return core.Emits(v) }

// Does declares what this Method is FOR, in one sentence: `Does("Resolve each domain's NS set")`.
//
// The Go peer of a Python docstring, and it has to be a VALUE because a Go doc comment is not in
// the binary — a running worker cannot read it. So the sentence is typed once more, next to the
// comment it repeats. It is read on the Actors page and on a Scratch node beside the Method's
// name: one sentence, present tense, about what a CALLER gets.
func Does(s string) MethodOption { return core.Does(s) }

// Close registers cleanup, always run at the end (success or failure).
func (a *Actor) Close(fn func(*Session) error) { a.reg.CloseFn = fn }

// Healthcheck registers an optional resource-liveness probe (the Go peer of
// @actor.healthcheck). Convention: RETURN progress (any, may be nil) with a nil error
// while the resource is ALIVE; return a non-nil error (or the bool `false`) when it is
// DEAD. It is the primary death signal: after a Method failure the host probes the
// resource to decide reload (dead) vs isolate-the-Unit (alive).
func (a *Actor) Healthcheck(fn func(*Session) (any, error)) { a.reg.HealthcheckFn = fn }

// Params declares the run-wide config type for schema derivation (optional). It stays
// ACTOR-level, unlike Takes/Emits: params are run-wide config, not a per-Method signature.
func (a *Actor) Params(p any) { a.reg.ParamsType = p }

// NewSession builds an unhosted Session, for TESTING a Method without a worker. Get/Set work; the
// durable tiers are the same safe no-ops they are in any unhosted body. The host builds its own.
//
// It is exported because the alternative was that a Go actor's Methods could not be called at all
// outside Temporal: Session lives in an internal package, and a zero value nil-panics on Set
// because its mutex is unset. An author's loop is ordinary code and must be testable as such.
func NewSession(params map[string]any) *Session { return core.NewSession(params, "") }

// TestBatch builds an unhosted Batch over values, for TESTING a Method without a worker — the
// input the caller passes first. Ranging, Units() and Str all behave exactly as they do under the
// host; what is absent is the durable half, so nothing commits and nothing is written to the blob
// plane. Pair it with TestDataset for the output the caller passes second.
func TestBatch(values ...any) *Batch {
	todo := make([]core.Item, len(values))
	for i, v := range values {
		todo[i] = core.Item{Index: i, Value: v}
	}
	return core.NewBatch(nil, todo)
}

// TestDataset builds a collecting output Dataset, for TESTING a Method body with no host, no queue
// and no object store — the peer of Python's actorkit.testing.collecting_dataset. Hand it to a
// Method as its third argument and read ds.Records() for what the body pushed, in push order:
//
//	ds := kontra.TestDataset()
//	if err := ask(s, kontra.TestBatch(pair), ds); err != nil { t.Fatal(err) }
//	got := ds.Records()
//
// It is the forcing argument for a parameter over a bare emit callable: because the destination is
// something the caller hands over, a body is exercised by substituting it, and the loop stays
// ordinary code (ADR 0028 §3).
func TestDataset() *Dataset { return core.CollectingDataset() }

// NonRetryable wraps a message as a non-retryable error: the host isolates the Unit as
// "terminal" and does NOT retry it. The Go peer of raising actorkit's NonRetryableError.
func NonRetryable(msg string) error { return &core.NonRetryableError{Msg: msg} }

// SessionLost signals the session's shared resource is dead. Returning it ends the Session: the
// host nulls the instance and the handler's retry re-runs Load for a fresh resource, resuming
// from the commit map (ADR 0023 §20). The Go peer of raising actorkit's SessionLost.
func SessionLost(msg string) error { return &core.SessionLostError{Msg: msg} }
