package main

// sickworker_test.go — the case the Watchdog existed for, driven through the real reconcile loop
// against real processes.
//
// ═══ THE CONTROLS COME FIRST, EVERY TIME ═══
//
// warden_test.go's header states the rule and driver_podman_test.go's socket test is the model:
// prove the hazard exists before asserting it was handled. Every negative in this file passes
// identically against a Warden that judges nothing, so each one is preceded by the positive that
// makes it mean something:
//
//   - "a sick Worker is stopped" is worthless unless a HEALTHY Worker at the same volume is left
//     alone by the same code on the same run. Both are asserted, from one fixture, differing only in
//     how many of the 82 loads failed.
//   - "a Worker with no evidence is not restarted" is worthless unless something else in the same
//     test IS restarted. The sick case is that control.
//   - "the scrape actually happened" is worthless if the counters could have come from anywhere, so
//     the scrape seam counts its calls and the assertions read them.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- fixtures --------------------------------------------------------------------------------------

// fakeCounters is a Worker's :9110, under a test's control. It counts scrapes, because "the judge
// concluded X" and "the judge concluded X from a reading it actually took" are different claims and
// only the second one is worth making.
type fakeCounters struct {
	mu       sync.Mutex
	loads    float64
	failures float64
	err      error
	scrapes  int
	addrs    []string
}

func (f *fakeCounters) scrape(_ context.Context, addr string) (loadCounters, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scrapes++
	f.addrs = append(f.addrs, addr)
	if f.err != nil {
		return loadCounters{}, f.err
	}
	return loadCounters{loads: f.loads, failures: f.failures}, nil
}

func (f *fakeCounters) set(loads, failures float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loads, f.failures = loads, failures
}

func (f *fakeCounters) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.scrapes
}

// clock is a time a test moves by hand. The window this judge divides by is five minutes and the
// span floor is one; a test that slept for either would be a test nobody runs.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock {
	return &clock{t: time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)}
}
func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// healthyWarden is warden_test.go's `testWarden` with a judge attached and its scrape faked. The
// address always resolves, because the `process` driver deliberately refuses to guess one (see
// sickworker.go) and every Worker here is given the variable that answers it.
func healthyWarden(t *testing.T, prefix string) (*warden, *fakeCounters, *clock) {
	t.Helper()
	w := testWarden(t, prefix)
	c := newClock()
	w.now = c.now
	counters := &fakeCounters{}
	w.health = newWorkerHealth(c.now)
	w.health.scrape = counters.scrape
	return w, counters, c
}

// scrapable is a Worker whose actor half declares where its counters are, which is what
// `metricsAddrFor` reads first and what a real assignment would carry for a `process` Worker.
func scrapable(t *testing.T, name, version string) workerSpec {
	t.Helper()
	s := spec(t, name, version)
	s.Actor.Env = append(s.Actor.Env, metricsAddrEnv+"=127.0.0.1:19110")
	return s
}

// --- THE CASE THE WATCHDOG EXISTED FOR ---------------------------------------------------------

// A Worker whose halves are both up, whose poller is live, and which is failing 81 of its 82
// resource loads, is STOPPED — and the loop starts it again on the next turn.
//
// THE RATIO IS THE RECORDED ONE. `machine.ts`'s watchdog header: "During the round-3 run one
// Machine failed 81 of 82 resource loads and another 19 of 20 while their peers sat at zero, each
// silently isolating about an eighth of a sweep — and the runs still reported `completed`. Nothing
// off the shelf catches it: the process never dies, so `Restart=` never fires, and a healthcheck
// only marks."
//
// AND THE CONTROL IS IN THE SAME TEST. A second Worker on the same Machine, judged by the same
// Warden on the same turns, attempts the same 82 loads and fails none of them. If the sick one were
// stopped by anything other than its ratio — a turn boundary, the scrape seam, the clock — the
// healthy one would go with it.
func TestASickButNotDeadWorkerIsCaughtAndAHealthyOneIsNot(t *testing.T) {
	w, sickCounters, c := healthyWarden(t, "wsick")
	healthyCounters := &fakeCounters{}

	sick := scrapable(t, "wsick-ill", "0.1.0")
	well := scrapable(t, "wsick-well", "0.1.0")
	// One scrape seam, two Workers: dispatch on the id the address carries, so each Worker's
	// readings are genuinely its own.
	well.Actor.Env = append(well.Actor.Env, metricsAddrEnv+"=127.0.0.1:19111")
	w.health.scrape = func(ctx context.Context, addr string) (loadCounters, error) {
		if strings.HasSuffix(addr, ":19111") {
			return healthyCounters.scrape(ctx, addr)
		}
		return sickCounters.scrape(ctx, addr)
	}

	desired := []workerSpec{sick, well}
	bothWhole := func(hs []workerHandle) bool {
		a, okA := handleFor(hs, "wsick-ill")
		b, okB := handleFor(hs, "wsick-well")
		return okA && okB && a.whole() && b.whole()
	}

	// Turn 1 STARTS them and judges nothing, which is a property rather than an accident: the judge
	// hangs off the `running && whole()` branch, so a Worker is only ever read once the runtime has
	// confirmed both its halves. Turn 2 is the first reading.
	sickCounters.set(400, 12)
	healthyCounters.set(400, 12)
	turn(t, w, desired, "both Workers whole", bothWhole)
	if sickCounters.calls() != 0 {
		t.Fatal("a Worker was scraped on the turn that started it, before the runtime confirmed it was whole")
	}
	turn(t, w, desired, "both Workers still whole", bothWhole)

	// THE CONTROL FOR THE CONTROL: one reading is not a window, and the judge must say so rather
	// than defaulting to either answer.
	if v, _ := w.health.verdict("wsick-ill@0.1.0"); v.Verdict != healthCannotTell || v.Reason != "samples" {
		t.Fatalf("one reading must be `cannot tell (samples)`; got %q/%q", v.Verdict, v.Reason)
	}
	if sickCounters.calls() == 0 {
		t.Fatal("nothing was scraped, so every assertion below would be about a judge with no input")
	}

	// Five minutes pass. The sick Worker fails 81 of its next 82 loads; the healthy one fails none.
	c.advance(5 * time.Minute)
	sickCounters.set(482, 93)
	healthyCounters.set(482, 12)

	// Turn 2: the sick one is judged and stopped; the healthy one is judged and left running.
	turn(t, w, desired, "the sick Worker is gone and the healthy one is not", func(hs []workerHandle) bool {
		_, stillSick := handleFor(hs, "wsick-ill")
		b, okB := handleFor(hs, "wsick-well")
		return !stillSick && okB && b.whole()
	})

	sickV, _ := w.health.verdict("wsick-ill@0.1.0")
	if sickV.Verdict != healthSick {
		t.Fatalf("81 of 82 loads failed in five minutes and the verdict was %q (%s)", sickV.Verdict, sickV.Detail)
	}
	wellV, _ := w.health.verdict("wsick-well@0.1.0")
	if wellV.Verdict != healthHealthy {
		t.Fatalf("the control Worker attempted the same 82 loads and failed none; verdict %q (%s)",
			wellV.Verdict, wellV.Detail)
	}

	// AND THE STOP IS A RESTART, not a removal. The Worker is still in the assignment, so the next
	// turn starts it — which is the whole reason `sick` is expressed as a stop and left to the
	// loop's own one-transition-per-turn rule.
	before := w.startsOf("wsick-ill@0.1.0")
	turn(t, w, desired, "the sick Worker is running again", func(hs []workerHandle) bool {
		h, ok := handleFor(hs, "wsick-ill")
		return ok && h.whole()
	})
	if after := w.startsOf("wsick-ill@0.1.0"); after <= before {
		t.Fatalf("the Warden stopped a sick Worker and did not start it again (starts %d -> %d)", before, after)
	}
	if got := w.startsOf("wsick-well@0.1.0"); got != 1 {
		t.Errorf("the healthy Worker was restarted %d time(s); it should never have been touched", got)
	}
}

// The readings of a stopped Worker are dropped, so its replacement is judged on its own numbers.
//
// Without this the Warden restarts a Worker, the replacement's counters start at zero, and the
// window straddles two lives — reported honestly by the `reset` branch, but for a full five minutes
// during which nothing about the new Worker can be learned.
func TestASickWorkersReadingsGoWithIt(t *testing.T) {
	w, counters, c := healthyWarden(t, "wforget")
	s := scrapable(t, "wforget-a", "0.1.0")
	desired := []workerSpec{s}

	counters.set(400, 12)
	whole := func(hs []workerHandle) bool {
		h, ok := handleFor(hs, "wforget-a")
		return ok && h.whole()
	}
	turn(t, w, desired, "up", whole)
	turn(t, w, desired, "first reading taken", whole)
	c.advance(5 * time.Minute)
	counters.set(482, 93)
	turn(t, w, desired, "stopped for being sick", func(hs []workerHandle) bool {
		_, ok := handleFor(hs, "wforget-a")
		return !ok
	})

	// The control: it really was judged sick, or "the readings were dropped" is trivially true.
	if v, _ := w.health.verdict("wforget-a@0.1.0"); v.Verdict != healthSick {
		t.Fatalf("expected the Worker to be judged sick first; got %q", v.Verdict)
	}
	if got := len(w.health.samples["wforget-a@0.1.0"]); got != 0 {
		t.Fatalf("a sick Worker's %d reading(s) survived the stop", got)
	}
}

// --- `cannot tell` never restarts anything ------------------------------------------------------

// A Worker whose counters cannot be read is left alone, and says why.
//
// THE CONTROL IS THE SECOND HALF OF THE TEST: the same Warden, the same turn count, a Worker whose
// counters CAN be read and are terrible, is stopped. So "nothing was stopped" is a fact about the
// evidence and not about a Warden that never stops anything.
func TestAnUnreadableWorkerIsNotRestartedButASickOneIs(t *testing.T) {
	w, _, c := healthyWarden(t, "wblind")
	blind := scrapable(t, "wblind-quiet", "0.1.0")
	loud := scrapable(t, "wblind-loud", "0.1.0")
	loud.Actor.Env = append(loud.Actor.Env, metricsAddrEnv+"=127.0.0.1:19111")

	loudCounters := &fakeCounters{}
	w.health.scrape = func(ctx context.Context, addr string) (loadCounters, error) {
		if strings.HasSuffix(addr, ":19111") {
			return loudCounters.scrape(ctx, addr)
		}
		return loadCounters{}, errors.New("connection refused")
	}
	desired := []workerSpec{blind, loud}

	loudCounters.set(400, 12)
	bothWhole := func(hs []workerHandle) bool {
		a, okA := handleFor(hs, "wblind-quiet")
		b, okB := handleFor(hs, "wblind-loud")
		return okA && okB && a.whole() && b.whole()
	}
	turn(t, w, desired, "both up", bothWhole)
	turn(t, w, desired, "first readings taken", bothWhole)
	c.advance(5 * time.Minute)
	loudCounters.set(482, 93)
	turn(t, w, desired, "the readable-and-sick one is stopped", func(hs []workerHandle) bool {
		_, gone := handleFor(hs, "wblind-loud")
		a, stillThere := handleFor(hs, "wblind-quiet")
		return !gone && stillThere && a.whole()
	})

	v, _ := w.health.verdict("wblind-quiet@0.1.0")
	if v.Verdict != healthCannotTell || v.Reason != "scrape" {
		t.Fatalf("an unreadable Worker must be `cannot tell (scrape)`; got %q/%q", v.Verdict, v.Reason)
	}
	// The sentence has to name the thing an operator can act on. The Go actor host's loopback bind
	// is the measured blind spot this verdict will most often mean.
	for _, want := range []string{"127.0.0.1", metricsAddrEnv} {
		if !strings.Contains(v.Detail, want) {
			t.Errorf("the `scrape` sentence does not mention %q: %s", want, v.Detail)
		}
	}
	if w.startsOf("wblind-quiet@0.1.0") != 1 {
		t.Errorf("a Worker nobody could measure was restarted")
	}
}

// A Warden with no judge — which is every Warden built before this slice, and every one built by a
// test that does not ask for one — reconciles exactly as it did, and stops nothing for health.
func TestAWardenWithNoJudgeStopsNothingForHealth(t *testing.T) {
	w := testWarden(t, "wnojudge")
	if w.health != nil {
		t.Fatal("testWarden must not acquire a judge by accident; the rest of warden_test.go depends on it")
	}
	s := spec(t, "wnojudge-a", "0.1.0")
	desired := []workerSpec{s}
	turn(t, w, desired, "up", func(hs []workerHandle) bool {
		h, ok := handleFor(hs, "wnojudge-a")
		return ok && h.whole()
	})
	turn(t, w, desired, "still up after a second turn", func(hs []workerHandle) bool {
		h, ok := handleFor(hs, "wnojudge-a")
		return ok && h.whole()
	})
	if v := w.verdictFor("wnojudge-a@0.1.0"); v.Verdict != healthCannotTell || v.Reason != "disabled" {
		t.Fatalf("a Warden with no judge must report `cannot tell (disabled)`; got %q/%q", v.Verdict, v.Reason)
	}
}

// --- where the counters are -----------------------------------------------------------------------

func TestTheSpecsMetricsAddressWinsOverTheDrivers(t *testing.T) {
	drv := &processDriver{out: io.Discard, err: io.Discard}
	h := workerHandle{Driver: "process", Name: "a", Version: "1"}

	// The control: with nothing in the spec, the `process` driver refuses rather than guessing, and
	// the refusal names the variable that fixes it.
	if _, err := metricsAddrFor(context.Background(), drv, workerSpec{}, h); err == nil {
		t.Fatal("the process driver must refuse to invent a per-Worker metrics address")
	} else if !strings.Contains(err.Error(), metricsAddrEnv) {
		t.Errorf("the refusal must name %s: %v", metricsAddrEnv, err)
	}

	spec := workerSpec{Actor: procSpec{Env: []string{metricsAddrEnv + "=10.88.0.7:9110"}}}
	got, err := metricsAddrFor(context.Background(), drv, spec, h)
	if err != nil || got != "10.88.0.7:9110" {
		t.Fatalf("address = %q, %v; want the spec's own", got, err)
	}
}

// A LISTEN address is not a DIAL address, and the Python actor host's default is the wildcard.
func TestAWildcardBindIsDialledOnLoopback(t *testing.T) {
	for _, tc := range []struct{ bind, want string }{
		{"0.0.0.0:9110", "127.0.0.1:9110"},
		{":9110", "127.0.0.1:9110"},
		{"[::]:9110", "127.0.0.1:9110"},
		{"10.88.0.7:9110", "10.88.0.7:9110"},
	} {
		if got := dialableAddr(tc.bind); got != tc.want {
			t.Errorf("dialableAddr(%q) = %q, want %q", tc.bind, got, tc.want)
		}
	}
}

// `KONTRA_METRICS_ADDR=off` is a real setting both hosts honour — it turns the listener off. A judge
// that scraped anyway would report `scrape` forever about a Worker that is doing exactly what it was
// told, so the refusal says which.
func TestMetricsOffIsItsOwnRefusal(t *testing.T) {
	drv := &processDriver{out: io.Discard, err: io.Discard}
	spec := workerSpec{Actor: procSpec{Env: []string{metricsAddrEnv + "=off"}}}
	_, err := metricsAddrFor(context.Background(), drv, spec, workerHandle{Name: "a", Version: "1"})
	if err == nil || !strings.Contains(err.Error(), "off") {
		t.Fatalf("an `off` listener must be refused by name; got %v", err)
	}
}

// --- the scrape, against a real listener ----------------------------------------------------------

// The parser is driven by the corpus; this drives the whole HTTP path against a server that speaks
// exactly what an actor host speaks.
func TestScrapeReadsARealExposition(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		fmt.Fprintf(w, "# TYPE %s counter\n%s{actor=\"a\",version=\"1\"} 482\n", metricLoadsTotal, metricLoadsTotal)
		fmt.Fprintf(w, "# TYPE %s counter\n%s{actor=\"a\",version=\"1\"} 93\n", metricLoadFailTotal, metricLoadFailTotal)
	}))
	defer srv.Close()

	got, err := scrapeLoadCounters(context.Background(), srv.Client(), strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("scrape: %v", err)
	}
	if got.loads != 482 || got.failures != 93 {
		t.Fatalf("scraped %+v, want loads=482 failures=93", got)
	}
}

// A listener that answers, but is not an actor host, is `errNoLoadSeries` and not a zero reading.
func TestAListenerWithNoLoadSeriesIsRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "# TYPE go_goroutines gauge\ngo_goroutines 12\n")
	}))
	defer srv.Close()
	_, err := scrapeLoadCounters(context.Background(), srv.Client(), strings.TrimPrefix(srv.URL, "http://"))
	if !errors.Is(err, errNoLoadSeries) {
		t.Fatalf("a listener with no load counters must be refused by sentinel; got %v", err)
	}
}

// --- the window is a window -----------------------------------------------------------------------

// Readings older than the window stop being part of it, and the one immediately older is kept as its
// left edge. Without that last part the span collapses every time a sample ages out and the verdict
// flickers between a real ratio and `span` on the sampling cadence.
func TestReadingsOlderThanTheWindowAreDroppedButTheEdgeIsKept(t *testing.T) {
	base := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	samples := []loadSample{
		{at: base},
		{at: base.Add(1 * time.Minute)},
		{at: base.Add(6 * time.Minute)},
		{at: base.Add(7 * time.Minute)},
	}
	// A cutoff of base+2m: the first two are older, and exactly one of them survives as the edge.
	got := pruneSamples(samples, base.Add(2*time.Minute))
	if len(got) != 3 || !got[0].at.Equal(base.Add(1*time.Minute)) {
		t.Fatalf("pruned to %d sample(s) starting %v; want 3 starting %v",
			len(got), got[0].at, base.Add(1*time.Minute))
	}
}
