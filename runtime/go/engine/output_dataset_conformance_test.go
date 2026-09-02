package engine

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/medmahmoudi26/kontra/sdk/go/core"
)

// The GO ARM of the output-Dataset author-surface contract (shared/conformance/output_dataset.json).
//
// A per-SDK check against its own docs is not the same as the two SDKs agreeing, and this repo has
// been bitten by exactly that (a field reached the proto and three consumers but not the Go
// emitter, every suite green). So the granularities ADR 0028 §1 promises — one record per Unit, N
// per Unit, one per three, one for the whole Batch, none at all — are pinned in a FIXTURE the
// Python peer asserts too (tests/test_output_dataset_conformance.py), not just in each side's own
// tests. Each side selects the body named by `case` and drives it through its real engine, then
// compares the run's `results` to the fixture's `expected`.
//
// Inline mode (no object store), so results are the pushed records themselves rather than $refs —
// which is what makes the two sides' envelopes directly comparable.

type outputDatasetFixture struct {
	Cases []struct {
		Case     string           `json:"case"`
		Units    []any            `json:"units"`
		Expected []map[string]any `json:"expected"`
	} `json:"cases"`
}

// bodyFor returns the Method body the named case describes. The bodies are the contract's Go side:
// the same behaviour tests/test_output_dataset_conformance.py writes in Python, so a drift in
// either shows up as a fixture mismatch rather than as two green suites that disagree.
func bodyFor(t *testing.T, name string) core.MethodFunc {
	t.Helper()
	switch name {
	case "one-per-unit":
		return func(_ *core.Session, b *core.Batch, ds *core.Dataset) error {
			for unit := range b.All() {
				ds.Push(map[string]any{"u": unit.Value})
			}
			return b.Err()
		}
	case "one-per-three":
		return func(_ *core.Session, b *core.Batch, ds *core.Dataset) error {
			for unit := range b.All() {
				if unit.Index%3 == 0 {
					ds.Push(map[string]any{"i": unit.Index})
				}
			}
			return b.Err()
		}
	case "n-per-unit":
		return func(_ *core.Session, b *core.Batch, ds *core.Dataset) error {
			for unit := range b.All() {
				n := int(unit.Value.(float64)) // a JSON number arrives as float64
				for k := 0; k < n; k++ {
					ds.Push(map[string]any{"seed": n, "k": k})
				}
			}
			return b.Err()
		}
	case "none-for-a-unit":
		return func(_ *core.Session, b *core.Batch, ds *core.Dataset) error {
			for unit := range b.All() {
				if unit.Value != "skip" {
					ds.Push(map[string]any{"u": unit.Value})
				}
			}
			return b.Err()
		}
	case "whole-batch":
		return func(_ *core.Session, b *core.Batch, ds *core.Dataset) error {
			n := 0
			for range b.All() {
				n++
			}
			ds.Push(map[string]any{"total": n}, core.Key("total")) // no current Unit -> keyed tail
			return b.Err()
		}
	default:
		t.Fatalf("no Go body for conformance case %q — the fixture and this switch disagree", name)
		return nil
	}
}

func TestOutputDatasetSurfaceMatchesTheFixture(t *testing.T) {
	// ../../../shared/conformance/output_dataset.json — engine -> go -> runtime -> <repo root>.
	raw, err := os.ReadFile("../../../shared/conformance/output_dataset.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var fx outputDatasetFixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	if len(fx.Cases) == 0 {
		t.Fatal("fixture is empty — a vacuously passing conformance test is worse than none")
	}

	for _, c := range fx.Cases {
		t.Run(c.Case, func(t *testing.T) {
			a, _ := actorWith(t, methodRegistry("m", bodyFor(t, c.Case)))
			resp, err := a.RunBatch(context.Background(), RunBatchReq{
				Method: "m", Units: c.Units, RunID: "r", NodeID: "n"})
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if !resp.Done {
				t.Errorf("%s: batch not done — every Unit is accounted for", c.Case)
			}
			// Compare through JSON so an int pushed on this side and a float parsed from the
			// fixture read as one value, and map keys sort the same on both.
			if got, want := jsonify(t, resp.Results), jsonify(t, toAnySlice(c.Expected)); got != want {
				t.Errorf("%s results\n got %s\nwant %s", c.Case, got, want)
			}
		})
	}
}

func jsonify(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func toAnySlice(ms []map[string]any) []any {
	out := make([]any, len(ms))
	for i, m := range ms {
		out[i] = m
	}
	return out
}
