package warden

import (
	"encoding/json"
	"math"
	"os"
	"strings"
	"testing"
	"time"
)

// The READER's arm of shared/conformance/workerhealth.json.
//
// Three arms, not two, and this is the odd one. The corpus's usual shape is N implementations of one
// computation; here two actor hosts WRITE the two series (`tests/test_worker_health_conformance.py`,
// `runtime/go/engine/workerhealth_conformance_test.go`) and this binary READS them and divides one by
// the other on a Machine. All three can be individually correct and collectively broken: a host that
// renamed `_failures_total` to `_failure_total` renders a valid exposition and passes its own tests,
// and the only symptom is a Warden dividing by an absent series on every Worker of that language —
// visible as a `loads` chip stuck on `unknown`, which is exactly what an uninstrumented Worker looks
// like.
//
// So this file asserts BOTH halves of the reader's contract: the names it looks for, and the
// arithmetic it does with them.

type healthVerdictCase struct {
	Why    string `json:"why"`
	Before struct {
		Loads    float64 `json:"loads"`
		Failures float64 `json:"failures"`
	} `json:"before"`
	After struct {
		Loads    float64 `json:"loads"`
		Failures float64 `json:"failures"`
	} `json:"after"`
	Seconds float64 `json:"seconds"`
	Expect  string  `json:"expect"`
	Reason  string  `json:"reason"`
	Ratio   float64 `json:"ratio"`
}

type workerHealthCorpus struct {
	Metrics struct {
		Loads    string   `json:"loads"`
		Failures string   `json:"failures"`
		Labels   []string `json:"labels"`
	} `json:"metrics"`
	Thresholds struct {
		Ratio          float64 `json:"ratio"`
		MinLoads       float64 `json:"min_loads"`
		MinSpanSeconds float64 `json:"min_span_seconds"`
		WindowSeconds  float64 `json:"window_seconds"`
	} `json:"thresholds"`
	Verdicts struct {
		Cases []healthVerdictCase `json:"cases"`
	} `json:"verdicts"`
	Exposition struct {
		Sample string `json:"sample"`
		Expect struct {
			Loads    float64 `json:"loads"`
			Failures float64 `json:"failures"`
		} `json:"expect"`
	} `json:"exposition"`
}

func loadWorkerHealthCorpus(t *testing.T) workerHealthCorpus {
	t.Helper()
	raw, err := os.ReadFile("../../shared/conformance/workerhealth.json")
	if err != nil {
		t.Fatalf("reading the corpus: %v", err)
	}
	var doc workerHealthCorpus
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parsing the corpus: %v", err)
	}
	return doc
}

// A corpus that shrank to its easy half passes every judge, including one that returns `healthy`
// unconditionally. So the interesting rows are asserted PRESENT before anything is asserted about
// them — shared/conformance/README.md rule 3, and the discipline five vacuous guards in this program have
// earned.
func TestWorkerHealthCorpusStillCarriesTheCasesThatMatter(t *testing.T) {
	doc := loadWorkerHealthCorpus(t)
	if len(doc.Verdicts.Cases) < 8 {
		t.Fatalf("the corpus has shrunk to %d verdict cases", len(doc.Verdicts.Cases))
	}
	want := map[string]bool{"sick": false, "healthy": false, "cannot-tell": false}
	reasons := map[string]bool{"span": false, "loads": false, "reset": false}
	for _, c := range doc.Verdicts.Cases {
		want[c.Expect] = true
		if c.Reason != "" {
			reasons[c.Reason] = true
		}
	}
	for verdict, present := range want {
		if !present {
			t.Errorf("the corpus no longer exercises the %q verdict", verdict)
		}
	}
	for reason, present := range reasons {
		if !present {
			t.Errorf("the corpus no longer exercises the %q cannot-tell", reason)
		}
	}

	// THE CONTROL PAIR. The recorded instance and its control must share a denominator and differ
	// only in the numerator — otherwise an implementation that ignored the failure count could pass
	// both, which is the "corpus row where two implementations cannot differ" this program has
	// already found once.
	var sick, healthy *healthVerdictCase
	for i := range doc.Verdicts.Cases {
		c := &doc.Verdicts.Cases[i]
		if c.Seconds != 300 || c.After.Loads-c.Before.Loads != 82 {
			continue
		}
		if c.Expect == "sick" {
			sick = c
		}
		if c.Expect == "healthy" {
			healthy = c
		}
	}
	if sick == nil || healthy == nil {
		t.Fatal("the corpus no longer carries the 82-load pair: one sick, one healthy, same denominator")
	}
	if sick.After.Failures-sick.Before.Failures != 81 {
		t.Errorf("the recorded instance is 81 of 82; the corpus says %g of 82",
			sick.After.Failures-sick.Before.Failures)
	}
	if healthy.After.Failures-healthy.Before.Failures != 0 {
		t.Errorf("the control must fail none of its 82 loads; the corpus says %g",
			healthy.After.Failures-healthy.Before.Failures)
	}
}

// The two series names, and the three thresholds. Pinned separately from the cases that use them:
// a corpus regenerated from one implementation would agree with itself about every case and still
// have moved the contract.
func TestTheWardenLooksForTheSeriesTheCorpusNames(t *testing.T) {
	doc := loadWorkerHealthCorpus(t)
	if MetricLoadsTotal != doc.Metrics.Loads {
		t.Errorf("the Warden reads %q; the corpus names %q", MetricLoadsTotal, doc.Metrics.Loads)
	}
	if MetricLoadFailTotal != doc.Metrics.Failures {
		t.Errorf("the Warden reads %q; the corpus names %q", MetricLoadFailTotal, doc.Metrics.Failures)
	}
	if WardenHealthRatio != doc.Thresholds.Ratio {
		t.Errorf("ratio threshold %v; corpus %v", WardenHealthRatio, doc.Thresholds.Ratio)
	}
	if WardenHealthMinLoads != doc.Thresholds.MinLoads {
		t.Errorf("min loads %v; corpus %v", float64(WardenHealthMinLoads), doc.Thresholds.MinLoads)
	}
	if WardenHealthMinSpan.Seconds() != doc.Thresholds.MinSpanSeconds {
		t.Errorf("min span %v; corpus %vs", WardenHealthMinSpan, doc.Thresholds.MinSpanSeconds)
	}
	if WardenHealthWindow.Seconds() != doc.Thresholds.WindowSeconds {
		t.Errorf("window %v; corpus %vs", WardenHealthWindow, doc.Thresholds.WindowSeconds)
	}
}

func TestJudgeAgreesWithTheCorpus(t *testing.T) {
	doc := loadWorkerHealthCorpus(t)
	base := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	for _, c := range doc.Verdicts.Cases {
		t.Run(c.Why[:min(len(c.Why), 48)], func(t *testing.T) {
			got := JudgeSamples([]LoadSample{
				{at: base, LoadCounters: LoadCounters{loads: c.Before.Loads, failures: c.Before.Failures}},
				{at: base.Add(time.Duration(c.Seconds) * time.Second),
					LoadCounters: LoadCounters{loads: c.After.Loads, failures: c.After.Failures}},
			})
			if string(got.Verdict) != c.Expect {
				t.Fatalf("verdict %q, corpus says %q (reason %q, detail %q)",
					got.Verdict, c.Expect, got.Reason, got.Detail)
			}
			if c.Reason != "" && got.Reason != c.Reason {
				t.Errorf("cannot-tell reason %q, corpus says %q", got.Reason, c.Reason)
			}
			if c.Expect != "cannot-tell" && math.Abs(got.Ratio-c.Ratio) > 1e-12 {
				t.Errorf("ratio %v, corpus says %v", got.Ratio, c.Ratio)
			}
		})
	}
}

// The reader's half of the contract that neither host can test: a whole exposition, parsed.
func TestTheExpositionInTheCorpusParsesToItsTwoNumbers(t *testing.T) {
	doc := loadWorkerHealthCorpus(t)
	// The control first: a sample that does not mention the two series must be REFUSED, or this
	// assertion proves nothing about the parse — an implementation returning a zeroed struct would
	// otherwise pass every "healthy" case downstream.
	if _, err := ParseLoadCounters(strings.NewReader("kontra_batches_total{actor=\"x\"} 7\n")); err == nil {
		t.Fatal("an exposition with neither counter must be an error, not a zero reading")
	}
	got, err := ParseLoadCounters(strings.NewReader(doc.Exposition.Sample))
	if err != nil {
		t.Fatalf("the corpus's exposition did not parse: %v", err)
	}
	if got.loads != doc.Exposition.Expect.Loads || got.failures != doc.Exposition.Expect.Failures {
		t.Fatalf("parsed %+v, corpus expects loads=%v failures=%v",
			got, doc.Exposition.Expect.Loads, doc.Exposition.Expect.Failures)
	}
}
