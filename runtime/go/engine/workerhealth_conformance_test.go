package engine

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/medmahmoudi26/kontra/sdk/go/core"
)

// The GO HOST's arm of conformance/workerhealth.json — one of two WRITERS of the two series the
// Warden divides.
//
// WHY THIS IS NOT `metrics_test.go`'s job. That file already has a test named
// `TestReloadCounterCarriesTheSickWorkerSignature` which drives `countReload()` and `countBatch()`
// 81 and 82 times and asserts the rendered numbers. It is green, it has been green since it was
// written, and it proves nothing about a Fleet: neither counter has a production caller in either
// host, so the ratio it names cannot be computed from a running Worker. It is a test of the
// exposition format wearing an incident's clothes.
//
// The difference here is that the corpus is shared. A rename in this file breaks
// `cli/sickworker_conformance_test.go` and `tests/test_worker_health_conformance.py` in the same
// run, which is the only arrangement that catches the failure that has no other symptom: this host
// renders `_failure_total`, the Python host renders `_failures_total`, both files read correctly,
// and the Warden divides by an absent series on every Go Worker in the Fleet.

type workerHealthNames struct {
	Metrics struct {
		Loads    string   `json:"loads"`
		Failures string   `json:"failures"`
		Labels   []string `json:"labels"`
	} `json:"metrics"`
}

func loadWorkerHealthNames(t *testing.T) workerHealthNames {
	t.Helper()
	raw, err := os.ReadFile("../../../conformance/workerhealth.json")
	if err != nil {
		t.Fatalf("reading the corpus: %v", err)
	}
	var doc workerHealthNames
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parsing the corpus: %v", err)
	}
	if doc.Metrics.Loads == "" || doc.Metrics.Failures == "" {
		t.Fatal("the corpus no longer names both counters")
	}
	return doc
}

// The names, and the labels, as this host renders them.
func TestGoHostRendersTheSeriesTheCorpusNames(t *testing.T) {
	doc := loadWorkerHealthNames(t)
	resetMetrics()
	countLoad(true)
	countLoad(false)
	countLoad(false)

	w := httptest.NewRecorder()
	writeMetrics(w, "webcrawl", "0.2.0")
	body := w.Body.String()

	for _, want := range []string{
		doc.Metrics.Loads + `{actor="webcrawl",version="0.2.0"} 3`,
		doc.Metrics.Failures + `{actor="webcrawl",version="0.2.0"} 2`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s\ngot:\n%s", want, body)
		}
	}
	for _, label := range doc.Metrics.Labels {
		if !strings.Contains(body, label+`="`) {
			t.Errorf("the corpus requires the %q label; the exposition has none", label)
		}
	}
}

// A healthy Worker must still EMIT both series at zero.
//
// metrics.go already argues this for kontra_isolated_units_total — "a series that is absent when
// the value is zero renders identically to no data" — and here the consequence is sharper than a
// dashboard reading oddly: a Warden that finds no `..._loads_total` reports `cannot tell`, so a
// Worker that has genuinely attempted no loads would be indistinguishable from an actor host too
// old to have the counter. Both are `cannot tell`, but only one of them clears the moment work
// arrives, and an operator needs to know which they are looking at.
func TestGoHostEmitsBothLoadCountersAtZero(t *testing.T) {
	resetMetrics()
	w := httptest.NewRecorder()
	writeMetrics(w, "webcrawl", "0.2.0")
	body := w.Body.String()
	for _, want := range []string{
		`kontra_resource_loads_total{actor="webcrawl",version="0.2.0"} 0`,
		`kontra_resource_load_failures_total{actor="webcrawl",version="0.2.0"} 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("a Worker that has loaded nothing must still emit %s\ngot:\n%s", want, body)
		}
	}
}

// THE DENOMINATOR CANNOT BE HALF-WIRED, which is the whole reason `countLoad` takes a bool instead
// of there being two functions. `countBatch()` is the cautionary case: it exists, it is exercised by
// metrics_test.go, and nothing in production calls it — so the fleet dashboard divides by a permanent
// zero. A failure must advance BOTH counters, and this is the assertion that says so.
func TestAFailedLoadAdvancesBothCounters(t *testing.T) {
	resetMetrics()
	countLoad(false)
	metricsMu.Lock()
	gotLoads, gotFails := loads, loadFails
	metricsMu.Unlock()
	if gotLoads != 1 || gotFails != 1 {
		t.Fatalf("a failed load must count as a load AND as a failure; got loads=%v failures=%v",
			gotLoads, gotFails)
	}
	resetMetrics()
	countLoad(true)
	metricsMu.Lock()
	gotLoads, gotFails = loads, loadFails
	metricsMu.Unlock()
	if gotLoads != 1 || gotFails != 0 {
		t.Fatalf("a load that returned must not count as a failure; got loads=%v failures=%v",
			gotLoads, gotFails)
	}
}

// THE COUNTER IS WIRED, WHICH IS THE ONE THING ITS THREE NEIGHBOURS ARE NOT.
//
// `kontra_batches_total` is defined, rendered, and exercised by metrics_test.go, and no production
// path increments it. `kontra_resource_reloads_total` has a caller here and none in the Python host.
// Both are green in their own tests and both are useless on a Fleet. The only assertion that can
// tell those apart from a working counter is one that drives the ENGINE and then reads the counter —
// never `countLoad` directly — so this test calls neither.
func TestTheEngineIsWhatAdvancesTheLoadCounters(t *testing.T) {
	body := core.MethodFunc(func(_ *core.Session, b *core.Batch, ds *core.Dataset) error {
		for unit := range b.All() {
			ds.Push(map[string]any{"seen": unit.Value})
		}
		return nil
	})

	// A load that returns: the denominator moves, the numerator does not.
	resetMetrics()
	ok := &core.Registry{Name: "t"}
	ok.LoadFn = func(s *core.Session) error { s.Set("resource", "browser"); return nil }
	ok.AddMethod("m", body)
	a, _ := actorWith(t, ok)
	if _, err := a.RunBatch(context.Background(), RunBatchReq{
		Method: "m", Units: []any{"x"}, RunID: "r1", NodeID: "n1"}); err != nil {
		t.Fatalf("a healthy batch: %v", err)
	}
	metricsMu.Lock()
	healthyLoads, healthyFails := loads, loadFails
	metricsMu.Unlock()
	if healthyLoads < 1 {
		t.Fatalf("RunBatch opened the resource and %s did not move (loads=%v)", metricLoadsName, healthyLoads)
	}
	if healthyFails != 0 {
		t.Fatalf("a load that returned must not be counted as a failure; failures=%v", healthyFails)
	}

	// A load that raises: BOTH move, and they move together.
	resetMetrics()
	bad := &core.Registry{Name: "t"}
	bad.LoadFn = func(*core.Session) error { return errors.New("the resource is unreachable") }
	bad.AddMethod("m", body)
	a2, _ := actorWith(t, bad)
	if _, err := a2.RunBatch(context.Background(), RunBatchReq{
		Method: "m", Units: []any{"x"}, RunID: "r2", NodeID: "n1"}); err == nil {
		t.Fatal("a batch whose load raises must not report success")
	}
	metricsMu.Lock()
	sickLoads, sickFails := loads, loadFails
	metricsMu.Unlock()
	if sickLoads < 1 || sickFails != sickLoads {
		t.Fatalf("every failed load must advance both counters; loads=%v failures=%v", sickLoads, sickFails)
	}
}

// The name the wiring test prints, kept beside the exposition rather than re-typed into an error
// string that could drift out from under the corpus.
const metricLoadsName = "kontra_resource_loads_total"
