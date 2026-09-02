package engine

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func resetMetrics() {
	metricsMu.Lock()
	isolated = map[string]float64{}
	reloads, batches = 0, 0
	loads, loadFails = 0, 0
	metricsMu.Unlock()
}

// A healthy worker must still EMIT the series. If it is absent, `rate()` returns nothing and a
// dashboard renders it identically to "no data" — which is the exact ambiguity this metric
// exists to remove.
func TestZeroSeriesIsEmittedWhenNothingIsolated(t *testing.T) {
	resetMetrics()
	w := httptest.NewRecorder()
	writeMetrics(w, "cachebuster", "0.6.0")
	body := w.Body.String()
	want := `kontra_isolated_units_total{actor="cachebuster",version="0.6.0",category="exhausted"} 0`
	if !strings.Contains(body, want) {
		t.Fatalf("healthy worker must emit an explicit zero series.\nwant line: %s\ngot:\n%s", want, body)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Fatalf("wrong exposition content-type: %q", ct)
	}
}

func TestIsolationIsCountedPerCategory(t *testing.T) {
	resetMetrics()
	countIsolated("exhausted")
	countIsolated("exhausted")
	countIsolated("bad-unit")
	countIsolated("") // must not produce an empty label value
	w := httptest.NewRecorder()
	writeMetrics(w, "cachebuster", "0.6.0")
	body := w.Body.String()
	for _, want := range []string{
		`kontra_isolated_units_total{actor="cachebuster",version="0.6.0",category="exhausted"} 2`,
		`kontra_isolated_units_total{actor="cachebuster",version="0.6.0",category="bad-unit"} 1`,
		`kontra_isolated_units_total{actor="cachebuster",version="0.6.0",category="unknown"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s\ngot:\n%s", want, body)
		}
	}
}

// kf-m9 failed 81 of 82 resource loads while its peers sat at 0. That ratio is the signature of
// a silently sick worker, and it was only ever found by grepping logs by hand.
func TestReloadCounterCarriesTheSickWorkerSignature(t *testing.T) {
	resetMetrics()
	for i := 0; i < 81; i++ {
		countReload()
	}
	for i := 0; i < 82; i++ {
		countBatch()
	}
	w := httptest.NewRecorder()
	writeMetrics(w, "cachebuster", "0.6.0")
	body := w.Body.String()
	if !strings.Contains(body, `kontra_resource_reloads_total{actor="cachebuster",version="0.6.0"} 81`) {
		t.Errorf("reload counter wrong:\n%s", body)
	}
	if !strings.Contains(body, `kontra_batches_total{actor="cachebuster",version="0.6.0"} 82`) {
		t.Errorf("batch counter wrong:\n%s", body)
	}
}

// Label values are attacker-adjacent (a category can come from an error string); an unescaped
// quote would emit a corrupt exposition that breaks the whole scrape, not just one series.
func TestLabelValuesAreEscaped(t *testing.T) {
	resetMetrics()
	countIsolated(`we"ird` + "\n" + `\x`)
	w := httptest.NewRecorder()
	writeMetrics(w, `ac"tor`, "1.0")
	body := w.Body.String()
	if strings.Contains(body, "\n\\x") || strings.Count(body, `category="`) != 1 {
		t.Fatalf("label escaping is broken:\n%s", body)
	}
	if !strings.Contains(body, `we\"ird\n\\x`) {
		t.Fatalf("expected escaped label value, got:\n%s", body)
	}
	if !strings.Contains(body, `actor="ac\"tor"`) {
		t.Fatalf("expected escaped actor label, got:\n%s", body)
	}
}
