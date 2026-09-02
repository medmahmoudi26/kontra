package engine

import (
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
)

// Prometheus exposition for the one number this platform could not previously report:
// how many units a run silently threw away.
//
// WHY THIS EXISTS. When a unit error is classified as a dead resource the batch is aborted and,
// after maxUnitReloads, the unit is ISOLATED — dropped so one poison unit cannot fail a whole
// run. That is the right behaviour. The problem is that it was invisible: `RunBatchResp.Failures`
// carried the count, but nothing surfaced it, so a node that isolated every one of its units
// reported `completed` with zero output and looked exactly like a node that scanned everything
// and legitimately found nothing.
//
// It cost real work. A 15,814-target run reported success in ~7 minutes (impossible at the
// configured rate limit) because each node held ~1,500 units and mass-isolated. Separately,
// ~1,400 crawl seeds vanished inside nodes that reported `completed` with healthy-looking blob
// counts — the count was its own alibi. Both were found by hand, hours later.
//
// Deliberately NOT labelled by run id or node id. Every run would mint a fresh series and
// the TSDB index would grow without bound across runs; the actor+version+category triple is
// stable, and correlating to a run is what the timestamp is for.
//
// No prometheus/client_golang dependency: a counter's exposition format is four lines, and
// adding a library here would push a transitive dep into every Go actor's go.mod.

var (
	metricsMu sync.Mutex
	isolated  = map[string]float64{} // category -> count
	reloads   float64                // resource reloads (a dead resource forced a rebuild)
	batches   float64
	loads     float64 // Load attempts
	loadFails float64 // …of which raised
)

// countLoad records one Load attempt and whether it returned. Peer of `count_load` in
// internals/metrics.py; the NAMES it renders are pinned by conformance/workerhealth.json.
//
// ONE FUNCTION, BOTH COUNTERS, and that is the lesson from the two beside it. `countBatch` has had
// no production caller since it was written, so `rate(reloads)/rate(batches)` — the expression the
// fleet dashboard plots and `backend/src/panels/metrics.ts` refuses to trust — divides by a
// permanent zero. Two functions is how one of them gets wired and the other does not; a single call
// that always advances the denominator cannot be half-wired.
//
// This pair is the round-3 signature stated in the units it was actually reported in: kf-m9 failed
// **81 of 82 resource loads** in five minutes with its process alive and its poller live. Nothing
// counted the 82, so that sentence could not be asked of any system — it was found by hand.
//
// The WINDOW is not here. The **Warden** scrapes this listener on its own turn and differences two
// samples (ADR 0037), so "in the last five minutes" is a fact about the Warden's clock rather than a
// hope about whatever a Machine's log driver happens to retain.
func countLoad(ok bool) {
	metricsMu.Lock()
	loads++
	if !ok {
		loadFails++
	}
	metricsMu.Unlock()
}

// countIsolated records one permanently-dropped unit.
func countIsolated(category string) {
	if category == "" {
		category = "unknown"
	}
	metricsMu.Lock()
	isolated[category]++
	metricsMu.Unlock()
}

// countReload records that a resource was declared dead and rebuilt. A worker whose reload
// count climbs while its peers sit at zero is the "silently sick worker" signature — two
// workers behaved that way during a run, each eating roughly an eighth of a sweep.
func countReload() {
	metricsMu.Lock()
	reloads++
	metricsMu.Unlock()
}

func countBatch() {
	metricsMu.Lock()
	batches++
	metricsMu.Unlock()
}

func escapeLabel(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(s)
}

// writeMetrics renders the Prometheus text exposition format (version 0.0.4).
func writeMetrics(w http.ResponseWriter, actor, version string) {
	metricsMu.Lock()
	snapIso := make(map[string]float64, len(isolated))
	for k, v := range isolated {
		snapIso[k] = v
	}
	snapReloads, snapBatches := reloads, batches
	snapLoads, snapLoadFails := loads, loadFails
	metricsMu.Unlock()

	base := fmt.Sprintf(`actor="%s",version="%s"`, escapeLabel(actor), escapeLabel(version))
	var b strings.Builder

	b.WriteString("# HELP kontra_isolated_units_total Units permanently dropped after exhausting reloads. NON-ZERO MEANS THE RUN LOST WORK.\n")
	b.WriteString("# TYPE kontra_isolated_units_total counter\n")
	if len(snapIso) == 0 {
		// Emit an explicit zero so the series always exists. Otherwise a healthy worker has no
		// series at all and `rate()` on it returns nothing, which reads identically to "no data"
		// on a dashboard — the same ambiguity this metric exists to remove.
		fmt.Fprintf(&b, "kontra_isolated_units_total{%s,category=\"exhausted\"} 0\n", base)
	} else {
		cats := make([]string, 0, len(snapIso))
		for k := range snapIso {
			cats = append(cats, k)
		}
		sort.Strings(cats)
		for _, c := range cats {
			fmt.Fprintf(&b, "kontra_isolated_units_total{%s,category=\"%s\"} %g\n", base, escapeLabel(c), snapIso[c])
		}
	}

	b.WriteString("# HELP kontra_resource_reloads_total Times the actor resource was declared dead and rebuilt.\n")
	b.WriteString("# TYPE kontra_resource_reloads_total counter\n")
	fmt.Fprintf(&b, "kontra_resource_reloads_total{%s} %g\n", base, snapReloads)

	b.WriteString("# HELP kontra_batches_total Unit batches executed by this worker.\n")
	b.WriteString("# TYPE kontra_batches_total counter\n")
	fmt.Fprintf(&b, "kontra_batches_total{%s} %g\n", base, snapBatches)

	// The sick-worker ratio's two halves, byte-identical to the Python host's rendering. The names
	// are pinned by conformance/workerhealth.json rather than by these two literals: a
	// `_failures_total` here against a `_failure_total` there would leave the Warden dividing by an
	// absent series on every Go Worker in a Fleet, and the only symptom would be a chip that says
	// `unknown` forever while both files read correctly on their own.
	b.WriteString("# HELP kontra_resource_loads_total @actor.load attempts. The DENOMINATOR of the sick-worker ratio.\n")
	b.WriteString("# TYPE kontra_resource_loads_total counter\n")
	fmt.Fprintf(&b, "kontra_resource_loads_total{%s} %g\n", base, snapLoads)

	b.WriteString("# HELP kontra_resource_load_failures_total @actor.load attempts that raised. 81 of 82 in five minutes is the round-3 signature.\n")
	b.WriteString("# TYPE kontra_resource_load_failures_total counter\n")
	fmt.Fprintf(&b, "kontra_resource_load_failures_total{%s} %g\n", base, snapLoadFails)

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(b.String()))
}

// ServeMetrics starts the metrics listener on its own port, separate from anything else the
// enabling it cannot interfere with the sidecar's actor callbacks. Bound to loopback by default:
// a fleet droplet must accept no inbound connections, so a local agent scrapes 127.0.0.1 and
// pushes outward. Set KONTRA_METRICS_ADDR to override.
// ServeMetrics is called by the host at boot. Exported because the host lives in its own package
// (runtime/go/temporalhost).
func ServeMetrics(actor, version string) {
	addr := os.Getenv("KONTRA_METRICS_ADDR")
	if addr == "" {
		addr = "127.0.0.1:9110"
	}
	if strings.EqualFold(addr, "off") {
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) { writeMetrics(w, actor, version) })
	go func() {
		// A metrics listener must never take the actor down: log and continue.
		if err := http.ListenAndServe(addr, mux); err != nil {
			fmt.Fprintf(os.Stderr, "[actorkit] metrics listener on %s stopped: %v\n", addr, err)
		}
	}()
}
