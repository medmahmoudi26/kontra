// gocanary — the Go half of the streaming canary, and the only thing that proves the Go host
// publishes at all.
//
// WHY A SECOND CANARY RATHER THAN TRUSTING THE FIRST. `canary` (Python) and this one exercise two
// DIFFERENT publishers: `runtime/python/internals/engine.py` binds a contextvar and awaits a
// coroutine, `runtime/go/temporalhost/stream.go` binds a func on the Session and hands the record
// to a buffering client. They share a topic convention and a wire format and nothing else. A green
// Python canary says nothing about whether a Go actor streams — which is exactly the gap the
// campaign would have run into, because every actor it dispatches is Go.
//
//	kontra.Streams(TickProgress{})   the TYPE, declared by the actor that owns it
//	  -> kontra.Stream(s, rec)       one record, on topic `gocanary/tick`
//	  -> the run's Workflow Stream   -> GET /api/runs/<run>/progress-stream -> the console
//
// IT SENDS NO NETWORK TRAFFIC, for the same reason the Python one does not: a canary whose failure
// mode is "the target was slow" tells you nothing about the thing under test.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	kontra "github.com/medmahmoudi26/kontra/sdk/go"
	// The actor runtime, imported for its side effect: it registers the Temporal actor host
	// behind a.Serve(). The SDK declares the seam and never imports across it — runtime/ ->
	// sdk/ is one-way — so this line is what puts a host in the binary, and without it this
	// actor builds, runs, serves nothing and streams nothing.
	_ "github.com/medmahmoudi26/kontra/runtime/go"
)

// Tick is one unit of pretend work.
//
// `label` rides along so the record can name what it is on, exactly as a crawler names a URL and a
// scanner names a host — the field an operator actually reads.
type Tick struct {
	Label   string  `json:"label"`
	Seconds float64 `json:"seconds"`
}

// Beat is what one unit produced. Deliberately boring: the OUTPUT is not what this actor is for.
//
// THE FIELD IS `worker`, NOT `node`, AND THAT IS NOT A STYLE CHOICE. The framework stamps its own
// provenance columns onto every pushed record, and `node` is one of them. An output type that
// declares it collides at INSERT time, in the materializer, long after the Method returned:
//
//	Binder Error: Duplicate column name "node" in INSERT
//
// which publishBatch then retried eight times while the run sat at RUNNING with no clue in the
// actor's log. Found by the Python canary; the collision is in the materializer, so it is
// language-independent and this one would hit it identically.
type Beat struct {
	Label  string  `json:"label"`
	Slept  float64 `json:"slept"`
	Worker string  `json:"worker"`
}

// TickProgress is WHAT THIS METHOD SHOWS WHILE IT RUNS — declared here, in the actor that owns it.
//
// A workflow author never reads this file; they read the catalog, where this arrives beside the
// Method's `input` and `output` as the operation's `stream` schema. That is what lets a console
// draw typed, labelled fields for a run whose actor it has never heard of.
//
// FIELD ORDER IS THE DISPLAY ORDER. `schema.Of` reflects into an ORDERED property map and the
// console's `orderedFields` honours it, so these three are read top to bottom as declared rather
// than alphabetically — which would put `done` first and lead with the least interesting number.
//
// The vocabulary is THIS actor's. A crawler would say `url` and `contexts`, a registry monitor
// `repo` and `layers`. Nothing here is a framework word except `done`, which the pane draws as a
// bar rather than as a labelled value.
// A `jsonschema_description` TAG, NOT A `//` COMMENT — the same asymmetry `Does(...)` exists for.
// A Go comment is not in the binary, so a worker cannot carry it; the console's pane renders a
// field's `description` from the schema, and without one an operator gets a value with a bare
// label and no way to learn what it means short of opening this file. The separate tag rather
// than `jsonschema:"description=…"` because that form is comma-split, so the first comma in a
// sentence silently truncates it into a second, meaningless option.
type TickProgress struct {
	Label string  `json:"label" jsonschema_description:"The unit currently being worked. A crawler would put its URL here and a scanner its host — this is the field an operator actually reads."`
	Done  int     `json:"done" jsonschema_description:"Units this Session has finished. The pane draws it as a bar against the Batch's size rather than as a labelled value."`
	Slept float64 `json:"slept" jsonschema_description:"Seconds spent on THIS unit. A stall is a number that stops climbing, which reads differently from a worker that has gone quiet."`
}

// Params are the run-wide dials, declared so the console renders a form rather than a raw JSON box.
type Params struct {
	// FailOn is a label to raise on, so unit isolation can be exercised deliberately.
	FailOn string `json:"fail_on"`
	// BeatEvery is how often the sleep loop wakes, which bounds how promptly a cancel is noticed.
	BeatEvery float64 `json:"beat_every"`
}

// doneKey is where the per-Session unit count lives.
//
// ON THE SESSION, NOT IN A LOCAL, AND THE SDK SAYS WHY: "A METHOD IS ENTERED SEVERAL TIMES PER
// BATCH — once per isolated Unit failure. Locals reset between entries; the Session does not."
// A `done := 0` at the top of tick() would reset to zero the moment `fail_on` isolated a Unit, so
// the one run that exercises isolation is the one where the counter silently restarts — a progress
// number that goes backwards mid-run, in the exact scenario the canary exists to test.
const doneKey = "done"

func main() {
	a := kontra.New()
	a.Params(Params{})

	// Nothing to open. Declared anyway so the canary exercises the load/close lifecycle the real
	// actors use — a canary that skipped it would pass on a runtime where Load is broken.
	a.Load(func(s *kontra.Session) error {
		s.Set(doneKey, 0)
		return nil
	})

	// THE ACTOR OVERALL: is the Session still usable? Nothing else.
	//
	// There is no resource to lose here, so it can only say "yes" — which is the shape every
	// healthcheck should have. Progress is NOT this hook's job: it is per Method, it is typed, and
	// it goes through kontra.Stream. They used to be one function, and the cost of that was a
	// crawler whose entire operator-facing signal was `{"contexts": 2}`.
	//
	// nil, nil is ALIVE-with-nothing-to-say. The host reads a non-nil error (or the bool false) as
	// dead and reloads; a nil payload is not death and must not read as one.
	a.Healthcheck(func(s *kontra.Session) (any, error) { return nil, nil })

	a.Close(func(s *kontra.Session) error { return nil })

	a.Method("tick", tick,
		kontra.Takes(Tick{}),
		kontra.Emits(Beat{}),
		kontra.Streams(TickProgress{}),
		kontra.Does("Sleep once per unit, naming the unit while it sleeps — the Go streaming canary"))

	a.Serve()
}

// tick sleeps once per unit, naming the unit while it sleeps.
//
// THE AUTHOR OWNS THE LOOP (ADR 0023 §18) and the framework commits each Unit as `b.All()` moves
// past it — which is exactly why this is a loop over units rather than one long activity: a worker
// killed halfway resumes at the unit it reached, and proving that is half the point of running it.
//
// `b.All()`, NOT `b.Units()`. The Go peer of the trap the Python canary hit with `batch.units`:
// Units() hands out the whole slice at once, which is right for a Method that wants to run them
// concurrently (webcrawl does) and wrong here — ranging All() is what commits a Unit as the loop
// passes it and what gives Push a Unit to attribute the record to.
func tick(s *kontra.Session, b *kontra.Batch, ds *kontra.Dataset) error {
	p := paramsOf(s)
	node := os.Getenv("KONTRA_NODE")
	if node == "" {
		node = "local"
	}

	for unit := range b.All() {
		var t Tick
		if err := unit.Into(&t); err != nil {
			// THIS Unit's payload does not fit. Returning isolates it and leaves the rest of the
			// Batch alone (ADR 0023 §13) — which is the behaviour, not a workaround for it.
			return err
		}

		done, _ := s.Get(doneKey)
		doneN, _ := done.(int)

		// NAMED BEFORE THE SLEEP, not after. A stall is when a reader needs the label, and a
		// record that names what just FINISHED says nothing during the wait.
		kontra.Stream(s, TickProgress{Label: t.Label, Done: doneN, Slept: 0})

		if matchesLabel(t.Label, p.FailOn) {
			return fmt.Errorf("gocanary asked to fail on %q", t.Label)
		}

		slept := 0.0
		for slept < t.Seconds {
			step := p.BeatEvery
			if rem := t.Seconds - slept; step > rem {
				step = rem
			}
			time.Sleep(time.Duration(step * float64(time.Second)))
			slept += step
		}

		doneN++
		s.Set(doneKey, doneN)
		// AFTER, TOO — the pair is what makes a stall legible: `slept` stops climbing while
		// `label` stays put, which reads differently from a worker that has simply gone quiet.
		kontra.Stream(s, TickProgress{Label: t.Label, Done: doneN, Slept: round3(slept)})
		ds.Push(Beat{Label: t.Label, Slept: round3(slept), Worker: node})
	}
	return b.Err()
}

func round3(f float64) float64 { return float64(int(f*1000+0.5)) / 1000 }

// matchesLabel reports whether `want` names this unit — the whole label, or its last segment.
//
// THE LAST SEGMENT, BECAUSE THE WHOLE LABEL IS UNGUESSABLE. The workflow prefixes every label with
// the run id (`canary-1789943883/go/unit-3`) to stop two runs with identical arguments hashing to
// the same Batch and replaying instead of working. An exact comparison against `fail_on` therefore
// could never match anything a person could type BEFORE the run existed — and `fail_on` is set on
// the launch form, which is exactly then. The form's own example is `"unit-3"`, so the documented
// value silently did nothing and the isolation path the canary exists to exercise never ran.
//
// SPLIT ON THE SEPARATOR RATHER THAN A BARE SUFFIX: `strings.HasSuffix` would let `fail_on: "3"`
// match `unit-13` and `unit-23` as well, which turns one deliberate failure into three.
func matchesLabel(label, want string) bool {
	if want == "" {
		return false
	}
	if label == want {
		return true
	}
	return strings.HasSuffix(label, "/"+want)
}

// paramsOf decodes the caller's params map into the declared type, defaults first.
//
// THE ZERO VALUE AND "UNSET" ARE DIFFERENT ANSWERS, which is why the defaults are seeded BEFORE
// the unmarshal rather than patched after it: `beat_every: 0` from a caller who means it and an
// absent key both arrive as 0.0, and only the seeded struct tells them apart — the absent key
// leaves 0.25 standing, the explicit 0 overwrites it. (An explicit 0 then spins the sleep loop,
// which is the caller's business and not something to silently correct.)
//
// A json round trip rather than three type assertions, matching desync's paramsOf: the schema the
// console renders a form from is reflected from these same struct tags, so decoding through them
// is the only way the form and the body cannot disagree.
func paramsOf(s *kontra.Session) Params {
	p := Params{BeatEvery: 0.25}
	raw, err := json.Marshal(s.Params)
	if err != nil {
		return p
	}
	// A malformed params blob leaves the defaults standing rather than failing the Batch: params
	// are run-wide config, and a canary that refuses to run because one dial was mistyped is less
	// useful than one that runs with the documented default and streams what it did.
	_ = json.Unmarshal(raw, &p)
	return p
}
