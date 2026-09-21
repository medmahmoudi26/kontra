package main

import (
	"encoding/json"
	"strings"
	"testing"

	kontra "github.com/medmahmoudi26/kontra/sdk/go"
	"github.com/medmahmoudi26/kontra/sdk/go/schema"
)

// collect binds a sink to the Session the way the host does per Batch, and returns what the
// Method published. This is the ONLY way to test `kontra.Stream` without a worker: outside a
// hosted run the sink is nil and Stream is a deliberate no-op, so a body that never streamed and
// one that streamed perfectly look identical from a plain test.
func collect(s *kontra.Session) *[]TickProgress {
	var got []TickProgress
	s.BindStream(func(v any) {
		rec, ok := v.(TickProgress)
		if !ok {
			return
		}
		got = append(got, rec)
	})
	return &got
}

func fast() map[string]any { return map[string]any{"beat_every": 0.01} }

func TestTickStreamsBeforeAndAfterEachUnit(t *testing.T) {
	// THE PAIR IS THE POINT. One record per unit would leave a reader unable to tell a unit that
	// is MID-FLIGHT from one that has finished, which is the entire question during a stall.
	s := kontra.NewSession(fast())
	got := collect(s)
	s.Set(doneKey, 0)

	b := kontra.TestBatch(
		map[string]any{"label": "unit-1", "seconds": 0.02},
		map[string]any{"label": "unit-2", "seconds": 0.02},
	)
	if err := tick(s, b, kontra.TestDataset()); err != nil {
		t.Fatalf("tick: %v", err)
	}

	if len(*got) != 4 {
		t.Fatalf("want 4 records (2 units x before/after), got %d: %+v", len(*got), *got)
	}
	want := []struct {
		label string
		done  int
	}{{"unit-1", 0}, {"unit-1", 1}, {"unit-2", 1}, {"unit-2", 2}}
	for i, w := range want {
		if (*got)[i].Label != w.label || (*got)[i].Done != w.done {
			t.Errorf("record %d: want %s/done=%d, got %s/done=%d",
				i, w.label, w.done, (*got)[i].Label, (*got)[i].Done)
		}
	}
	// `slept` climbing from 0 is what distinguishes a working unit from a wedged one.
	if (*got)[0].Slept != 0 || (*got)[1].Slept <= 0 {
		t.Errorf("slept should start at 0 and be positive after: %+v", (*got)[:2])
	}
}

func TestDoneSurvivesAReEntry(t *testing.T) {
	// THE BUG THIS PINS. A Method is entered SEVERAL TIMES PER BATCH — once per isolated Unit
	// failure (ADR 0023 §13) — and locals reset between entries while the Session does not. A
	// `done := 0` local would restart the count on the re-entry, so the one run that exercises
	// isolation is the one where the progress number goes BACKWARDS mid-run. Two calls on one
	// Session is that re-entry.
	s := kontra.NewSession(fast())
	got := collect(s)
	s.Set(doneKey, 0)

	for _, label := range []string{"unit-1", "unit-2"} {
		b := kontra.TestBatch(map[string]any{"label": label, "seconds": 0.01})
		if err := tick(s, b, kontra.TestDataset()); err != nil {
			t.Fatalf("tick %s: %v", label, err)
		}
	}
	last := (*got)[len(*got)-1]
	if last.Done != 2 {
		t.Fatalf("done must carry across entries; want 2, got %d (all: %+v)", last.Done, *got)
	}
}

func TestFailOnMatchesTheLastSegment(t *testing.T) {
	// THE BUG THIS PINS. The workflow prefixes labels with the run id to defeat content-hash
	// replay, so the exact comparison this used could only match a string nobody can type before
	// the run exists — while the launch form documents `fail_on` with the example "unit-3".
	if !matchesLabel("canary-1789943883/go/unit-3", "unit-3") {
		t.Error("the form's own documented value must match")
	}
	if !matchesLabel("unit-3", "unit-3") {
		t.Error("a bare label must still match itself")
	}
	// A bare suffix would make "3" match unit-13 and unit-23 as well, turning one deliberate
	// failure into three — which is a different test from the one the operator asked for.
	if matchesLabel("canary-x/go/unit-13", "unit-3") {
		t.Error("unit-3 must not match unit-13")
	}
	if matchesLabel("canary-x/go/unit-3", "") {
		t.Error("an empty fail_on must fail nothing")
	}
}

func TestFailOnStreamsTheUnitBeforeRaising(t *testing.T) {
	// A unit that fails must still have NAMED itself first, or the pane's last word about a
	// failing run is the previous unit — which points a reader at the wrong one.
	s := kontra.NewSession(map[string]any{"fail_on": "unit-2", "beat_every": 0.01})
	got := collect(s)
	s.Set(doneKey, 0)

	b := kontra.TestBatch(
		map[string]any{"label": "unit-1", "seconds": 0.01},
		map[string]any{"label": "unit-2", "seconds": 0.01},
	)
	err := tick(s, b, kontra.TestDataset())
	if err == nil || !strings.Contains(err.Error(), "unit-2") {
		t.Fatalf("want a failure naming unit-2, got %v", err)
	}
	last := (*got)[len(*got)-1]
	if last.Label != "unit-2" || last.Slept != 0 {
		t.Fatalf("the failing unit must be the last thing streamed, mid-flight: %+v", last)
	}
}

func TestBeatDoesNotDeclareNode(t *testing.T) {
	// THE COLLISION THIS PINS. The materializer stamps its own provenance columns onto every
	// pushed record, `node` among them, so an output type declaring it dies at INSERT:
	//
	//	Binder Error: Duplicate column name "node" in INSERT
	//
	// long after the Method returned, with nothing in the actor's log. Cheap to assert, and the
	// assertion is what stops the next author "fixing" `worker` back to `node`.
	raw, err := json.Marshal(Beat{Label: "u", Slept: 1, Worker: "n1"})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if _, bad := m["node"]; bad {
		t.Fatalf("Beat must not declare `node` — it collides with the framework's own column: %s", raw)
	}
	if _, ok := m["worker"]; !ok {
		t.Fatalf("Beat lost `worker`: %s", raw)
	}
}

func TestStreamSchemaKeepsDeclarationOrder(t *testing.T) {
	// The console renders a topic's fields in the order the schema declares them, so this order
	// is a display decision and not an accident. Alphabetical would lead with `done`, which the
	// pane draws as a bar anyway — the labelled values would then open on the least interesting
	// number instead of on the unit's name.
	raw := schema.Of(TickProgress{})
	if len(raw) == 0 {
		t.Fatal("TickProgress reflected to no schema; the catalog would carry no `stream`")
	}
	// `properties` is an ordered object on the wire; reading the raw JSON is what preserves that,
	// since unmarshalling into a map would throw the order away — the very thing under test.
	s := string(raw)
	iLabel, iDone, iSlept := strings.Index(s, `"label"`), strings.Index(s, `"done"`), strings.Index(s, `"slept"`)
	if iLabel < 0 || iDone < 0 || iSlept < 0 {
		t.Fatalf("schema is missing a declared field: %s", s)
	}
	if !(iLabel < iDone && iDone < iSlept) {
		t.Fatalf("want label < done < slept as declared, got label=%d done=%d slept=%d in %s",
			iLabel, iDone, iSlept, s)
	}
}

func TestEveryStreamedFieldCarriesADescription(t *testing.T) {
	// A CONSOLE RENDERS `description` AND CANNOT INVENT ONE. Go comments are not in the binary, so
	// a field documented only by `// the unit being worked` reaches an operator as a bare label —
	// which is the exact complaint the launch form's inputs drew. The tag is the only form of the
	// sentence that survives compilation, so it is worth asserting it is there.
	var doc struct {
		Properties map[string]struct {
			Description string `json:"description"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(schema.Of(TickProgress{}), &doc); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"label", "done", "slept"} {
		p, ok := doc.Properties[f]
		if !ok {
			t.Fatalf("%s is not in the schema at all", f)
		}
		// A comma-split `jsonschema:"description=…"` tag truncates at the first comma, so a
		// suspiciously short sentence is the symptom of the wrong tag rather than a terse author.
		if len(p.Description) < 20 {
			t.Errorf("%s has no usable description (%q) — check the tag is jsonschema_description",
				f, p.Description)
		}
	}
}

func TestParamsDefaultWhenAbsentAndAreHonouredWhenSet(t *testing.T) {
	if got := paramsOf(kontra.NewSession(nil)); got.BeatEvery != 0.25 {
		t.Errorf("absent beat_every must leave the default standing, got %v", got.BeatEvery)
	}
	if got := paramsOf(kontra.NewSession(map[string]any{"beat_every": 0.5})); got.BeatEvery != 0.5 {
		t.Errorf("an explicit beat_every must win, got %v", got.BeatEvery)
	}
	if got := paramsOf(kontra.NewSession(map[string]any{"fail_on": "unit-3"})); got.FailOn != "unit-3" {
		t.Errorf("fail_on did not decode, got %q", got.FailOn)
	}
}
