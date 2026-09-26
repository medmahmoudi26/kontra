package warden

// sickworker.go — the **Warden** judging a Worker that is sick but not dead.
//
// ADR 0037 hands the Warden this duty and says why it is the Warden's: "The Watchdog retires into
// the Warden. The thing that decides a **Worker** is sick should be the thing that can restart it;
// two processes disagreeing about that was a bug waiting to be written."
//
// ═══ THE CHECK THIS REPLACES NEVER FIRED FOR ITS STATED REASON ═══
//
// `control/orchestrator/src/infra/programs/machine.ts` installed `watchdog.sh` on every **Machine**, on a
// five-minute systemd timer, and its header called it "the one that matters": the round-3 run,
// where one Machine failed **81 of 82 resource loads** and another 19 of 20 while their peers sat at
// zero, each silently eating about an eighth of a sweep, and the runs still reported `completed`.
// What it did was count the failure ratio in the actor host's journal:
//
//	fails=$(… | grep -ci 'unit failed\|SessionLost\|engine dead')
//	oks=$(…   | grep -ci 'unit ok\|committed')
//	total=$((fails + oks));  [ "$total" -ge 10 ] || exit 0
//
// Swept on 2026-08-30 with `grep -rn -a` over `runtime/`, `sdk/` and `handler/`: **`unit failed`,
// `unit ok` and `engine dead` occur nowhere in this repo**, and every occurrence of `committed` is a
// comment or a doc — no log line emits any of the five. So `oks` was permanently 0, `total` was
// `fails`, and the `MIN_UNITS=10` gate meant the timer could only ever fire on ten stray
// `SessionLost` tracebacks in fifteen minutes, at which point the ratio was 100% by construction.
// It could not count the shape it was written for, and it was the Machine's only health authority.
//
// This file is therefore not a like-for-like replacement. It is the first version of this check that
// can produce the number in its own name.
//
// ═══ WHY NOT logs(), WHICH IS THE OBVIOUS SOURCE ═══
//
// `driver.go`'s fourth verb looks like the containerised `journalctl` and is not, in exactly the
// property a five-minute window depends on. Measured:
//
//   - `process` (driver_process.go:334) returns `tmux capture-pane -p -S -` per half — a scrollback
//     bounded by LINES, at tmux's default 2000 because `cli/internal/tmux/tmux.go` sets no `history-limit` — with
//     no timestamps. Or it refuses outright with `errNoRetainedLogs` for a foreground Worker.
//   - `podman` (driver_podman.go:676) runs `podman logs <name>` with neither `--since` nor
//     `--timestamps`, over whatever `log_driver` the Machine happens to be configured with (measured
//     on the box this was written on: podman 4.3.1, `LogDriver: journald`).
//
// Neither carries a clock. A judge built on either computes over an unknown span and then reports
// confidently, which is worse than not checking: "1 of 1 failed, and that was the last thirty
// seconds" reads exactly like the round-3 signature, and "81 of 82, over six days" reads exactly
// like a healthy Worker having a bad week.
//
// ═══ SO THE EVIDENCE IS THE WORKER'S OWN COUNTERS, AND THE WARDEN OWNS THE WINDOW ═══
//
// Both actor hosts already serve a Prometheus exposition on :9110 (`runtime/python/internals/
// metrics.py`, `runtime/go/engine/metrics.go`), and this slice adds the one pair the ratio is
// actually stated in — `kontra_resource_loads_total` and `kontra_resource_load_failures_total`,
// incremented by a single call on both paths out of the resource open. Their NAMES are a contract
// with two independent writers and no registration step, so they live in
// `shared/conformance/workerhealth.json` rather than as string literals here (ADR 0035 rule two).
//
// The Warden scrapes that listener on its own turn and DIFFERENCES TWO SAMPLES. That is the whole
// reason this works where the journal grep could not: the window is a fact about the Warden's clock,
// not a hope about somebody's log retention.
//
// ═══ THREE-VALUED, AND `cannot tell` IS NEVER FOLDED INTO `healthy` ═══
//
// A health check that silently degrades to always-pass is worse than no check, because it is
// believed. Every way this can fail to reach an answer has its own verdict and its own sentence:
//
//	address   nothing on this Machine can say where the Worker's listener is
//	scrape    the listener did not answer, or served no such series
//	samples   fewer than two readings so far — no window exists yet
//	span      the two readings are closer together than WardenHealthMinSpan
//	loads     too few loads in the window to divide by
//	reset     the counters went backwards, so the Worker restarted between the readings
//
// None of them restarts anything, and all of them ride out to the pane's `loads` chip as `unknown`
// with the sentence attached — the same rule `control/orchestrator/src/panels/metrics.ts` argues for at length:
// "a green chip derived from a counter nothing increments would be worse than no chip".
//
// ═══ A MEASURED BLIND SPOT, RECORDED RATHER THAN PAPERED OVER ═══
//
// The Python host binds `0.0.0.0:9110` (metrics.py) and the Go host binds `127.0.0.1:9110`
// (runtime/go/engine/metrics.go:123). Under the `podman` driver a Worker's halves share a pod
// netns, so a loopback bind is invisible from the Machine — a Go Worker's counters cannot be
// scraped until something sets `KONTRA_METRICS_ADDR`. That is a `scrape` cannot-tell with that
// sentence, and it is deliberately NOT fixed here: which of the two binds is right is a decision
// with an exposure trade-off in both directions (`0.0.0.0` is fine inside a container netns and is
// a listener on the network for a bare `process` Worker), and it belongs to whoever owns the
// driver's environment rather than to telemetry.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// --- the contract with the actor hosts -------------------------------------------------------

// The two series names. Spelled here once, and asserted against
// `shared/conformance/workerhealth.json` by sickworker_conformance_test.go together with both hosts'
// renderings — three arms, because this file is the READER and the two hosts are the writers.
const (
	MetricLoadsTotal    = "kontra_resource_loads_total"
	MetricLoadFailTotal = "kontra_resource_load_failures_total"
)

// The port both hosts default to. Overridable per Worker through `KONTRA_METRICS_ADDR`, which is
// the same variable the hosts read, so a Worker and its Warden cannot be pointed at two places.
const (
	metricsPortDefault = 9110
	metricsAddrEnv     = "KONTRA_METRICS_ADDR"
)

// --- the tuning, pinned by the corpus ---------------------------------------------------------

const (
	// WardenHealthRatio is the failure ratio at which a Worker is restarted. `watchdog.sh`'s own
	// line, kept rather than the fleet dashboard's 0.5 — `panels/metrics.ts` states the asymmetry:
	// "a chip that only informs can afford to speak earlier than one that reboots something."
	WardenHealthRatio = 0.8

	// WardenHealthMinLoads is the smallest denominator worth dividing by. `watchdog.sh`'s
	// `MIN_UNITS`, now over a counter that actually moves.
	WardenHealthMinLoads = 10

	// WardenHealthMinSpan is the floor on how much time two readings must span. A Warden turns
	// every five seconds and a ratio over one turn's delta would restart a Worker for a single
	// failed load.
	WardenHealthMinSpan = 60 * time.Second

	// WardenHealthWindow is how far back readings are kept, and therefore the widest span a verdict
	// is ever computed over. Five minutes because that is the window the recorded instance is
	// stated in: 81 of 82 resource loads, in five minutes.
	WardenHealthWindow = 5 * time.Minute

	// wardenHealthScrapeTimeout bounds one scrape. Short, because this runs inside the reconcile
	// turn: a Worker whose listener has wedged must cost its own verdict, never the loop.
	wardenHealthScrapeTimeout = 2 * time.Second
)

// --- the verdict -------------------------------------------------------------------------------

type healthVerdict string

const (
	HealthSick       healthVerdict = "sick"
	HealthHealthy    healthVerdict = "healthy"
	healthCannotTell healthVerdict = "cannot-tell"
)

// workerVerdict is one Worker's health as the Warden currently reads it.
//
// Reason is machine-readable and Detail is the sentence a human acts on. Both travel: the reason is
// what the corpus pins and what a test can assert without matching prose, and the sentence is what
// reaches the operator through the pane report. Neither is derived from the other, because a
// verdict whose only explanation is a string is one nobody can assert on.
type workerVerdict struct {
	Verdict healthVerdict
	Reason  string
	Detail  string

	// Ratio is meaningful only when Verdict is sick or healthy. It is left at 0 otherwise, and
	// 0 is also a legitimate healthy ratio — which is exactly why `Verdict` is the field to read
	// and this one is for display.
	Ratio    float64
	Loads    float64
	Failures float64
	Span     time.Duration
}

func (v workerVerdict) Sick() bool { return v.Verdict == HealthSick }

// LoadCounters is one scrape's two numbers.
type LoadCounters struct {
	loads    float64
	failures float64
}

// LoadSample is one reading, with the Warden's own timestamp on it. The timestamp is the Warden's
// and not the host's on purpose: a clock skew between a Machine and a container is a real thing and
// the span this file divides by must be measured by one clock.
type LoadSample struct {
	at time.Time
	LoadCounters
}

// --- the judgement, which is pure --------------------------------------------------------------

// JudgeSamples turns a Worker's readings into a verdict.
//
// PURE, AND THE CORPUS DRIVES IT. Every branch below is a row in `shared/conformance/workerhealth.json`
// with a `why`, including the ones that refuse to answer — the corpus's own invariant is that
// `cannot-tell` is never folded into `healthy`, and the case that proves it is a quiet Worker whose
// counters did not move: an implementation that divides 0 by 0 into 0 reports that one as green.
//
// Samples must be in time order, oldest first.
func JudgeSamples(samples []LoadSample) workerVerdict {
	if len(samples) < 2 {
		return workerVerdict{
			Verdict: healthCannotTell,
			Reason:  "samples",
			Detail: fmt.Sprintf("only %d reading(s) of %s so far — a ratio needs two, and the Warden "+
				"takes one per turn", len(samples), MetricLoadsTotal),
		}
	}
	oldest, newest := samples[0], samples[len(samples)-1]
	span := newest.at.Sub(oldest.at)

	// THE COUNTERS WENT BACKWARDS. A counter only decreases by being reset, which for a Worker means
	// the process it lives in was replaced — so the two readings are of two different lives and the
	// delta between them is not a measurement of anything. Checked BEFORE the span and the
	// denominator, because a negative delta can satisfy both.
	if newest.loads < oldest.loads || newest.failures < oldest.failures {
		return workerVerdict{
			Verdict: healthCannotTell,
			Reason:  "reset",
			Detail: "its counters went backwards, so this Worker restarted between two readings and " +
				"the window straddles two lives of it — the next full window is the first one that measures anything",
			Span: span,
		}
	}

	if span < WardenHealthMinSpan {
		return workerVerdict{
			Verdict: healthCannotTell,
			Reason:  "span",
			Detail: fmt.Sprintf("its readings span %s, and a verdict needs %s — the recorded sick Worker "+
				"is 81 of 82 loads in five minutes, and the same ratio over one turn is four unlucky loads",
				span.Truncate(time.Second), WardenHealthMinSpan),
			Span: span,
		}
	}

	loads := newest.loads - oldest.loads
	failures := newest.failures - oldest.failures
	if loads < WardenHealthMinLoads {
		return workerVerdict{
			Verdict: healthCannotTell,
			Reason:  "loads",
			Detail: fmt.Sprintf("it attempted %g resource load(s) in %s, under the %d this check divides by "+
				"— nothing was measured, which is not the same as nothing being wrong",
				loads, span.Truncate(time.Second), WardenHealthMinLoads),
			Loads:    loads,
			Failures: failures,
			Span:     span,
		}
	}

	ratio := failures / loads
	v := workerVerdict{Ratio: ratio, Loads: loads, Failures: failures, Span: span}
	if ratio >= WardenHealthRatio {
		v.Verdict = HealthSick
		v.Reason = "ratio"
		v.Detail = fmt.Sprintf("%g of its %g resource loads failed in %s (%.0f%%) — the process is alive and "+
			"this is the round-3 signature, where the run reports completed while losing its work",
			failures, loads, span.Truncate(time.Second), ratio*100)
		return v
	}
	v.Verdict = HealthHealthy
	v.Reason = "ratio"
	return v
}

// --- where a Worker's listener is ----------------------------------------------------------------

// metricsAddresser is an OPTIONAL capability, and it is declared here rather than on `workerDriver`
// on purpose. driver.go is explicit that its seam is four verbs and that "nothing else belongs here
// — not `restart` … and not `wait`". Health is the Warden's job, not a driver's, and the only thing
// a driver has that the Warden cannot get for itself is the ADDRESS of a Worker it is holding. So
// the capability is asked for, not required: a driver that cannot answer produces an `address`
// cannot-tell with its own sentence, which is a real state and not a degradation.
type metricsAddresser interface {
	metricsAddress(ctx context.Context, h workerHandle) (string, error)
}

// metricsAddress for `podman`: the address of the pod the pair shares.
//
// MEASURED ON A LIVE podman 4.3.1: a container in a pod reports the pod's address through
// `NetworkSettings.IPAddress`, and that address is reachable from the Machine on an arbitrary port
// with NO published port at all — `curl http://10.88.0.142:9110/` answered 200 from the host. That
// is what makes this work without touching `runArgs`: publishing a port per Worker would need a host
// port allocator, and slice 11 packs several Workers onto one Machine.
//
// The ACTOR half is asked, because the actor host is what serves :9110; the handler has no metrics
// listener. Its container name outlives the container (driver_podman.go), so this resolves for a
// half that has just died — which is the reading most worth having.
func (d *podmanDriver) metricsAddress(ctx context.Context, h workerHandle) (string, error) {
	name := podmanContainer(h.Name, h.Version, partActor)
	out, err := exec.CommandContext(ctx, d.bin, "inspect", name,
		"--format", "{{.NetworkSettings.IPAddress}}").Output()
	if err != nil {
		return "", fmt.Errorf("podman inspect %s: %w", name, err)
	}
	ip := strings.TrimSpace(string(out))
	if ip == "" {
		return "", fmt.Errorf("podman is holding %s but reports no address for it, so its :%d cannot be "+
			"reached from this Machine", name, metricsPortDefault)
	}
	return fmt.Sprintf("%s:%d", ip, metricsPortDefault), nil
}

// metricsAddress for `process`: there is no answer, and saying so is the answer.
//
// A `process` Worker's halves run in the Machine's own network namespace, so every Worker on the
// Machine would serve :9110 on the same loopback and the first one to bind wins. There is no
// per-Worker address to resolve and no way to tell whose listener answered — which is a fact about
// running without a container, not a gap here. `KONTRA_METRICS_ADDR` in the Worker's own environment
// is the one thing that makes it answerable, and that is read from the spec (see `MetricsAddrFor`)
// rather than guessed at here.
func (d *ProcessDriver) metricsAddress(_ context.Context, h workerHandle) (string, error) {
	return "", fmt.Errorf("the `process` driver runs %s in the Machine's own network namespace, so every "+
		"Worker on it would serve :%d on the same loopback and there is no per-Worker address to scrape "+
		"— set %s in this Worker's environment to give it one", h.id(), metricsPortDefault, metricsAddrEnv)
}

// MetricsAddrFor is the address to scrape for one Worker: what the Worker was TOLD, or what the
// driver can find out.
//
// THE SPEC WINS, because the spec is what the actor host itself read. Both hosts take their bind
// address from `KONTRA_METRICS_ADDR`, so an assignment that sets it has already decided where the
// listener is; a driver-derived address that disagreed would be the Warden scraping a port nobody is
// serving and reporting `scrape` about a perfectly healthy Worker.
//
// A wildcard bind is rewritten to loopback for the DIAL. `0.0.0.0:9110` is where the host LISTENS
// and is not an address anything connects to; under `process` that is the same machine, and under
// `podman` the spec-supplied case is only reachable at all if something published it.
func MetricsAddrFor(ctx context.Context, drv workerDriver, spec Spec, h workerHandle) (string, error) {
	if addr := envValueLast(spec.Actor.Env, metricsAddrEnv); addr != "" {
		if strings.EqualFold(addr, "off") {
			return "", fmt.Errorf("this Worker's %s is `off`, so its actor host serves no counters and its "+
				"health cannot be judged from them", metricsAddrEnv)
		}
		return DialableAddr(addr), nil
	}
	if a, ok := drv.(metricsAddresser); ok {
		return a.metricsAddress(ctx, h)
	}
	return "", fmt.Errorf("the %s driver cannot say where %s's counters are served", drv.driverName(), h.id())
}

// envValueLast reads one KEY=VALUE out of a spec's environment. The LAST wins, which is what execve
// does with a duplicated key on Linux and therefore what the actor host will have read. (Named for
// that rule rather than plainly, because `scale_test.go` already has an `envValue` in this package
// which takes the FIRST — a different answer to the same question, and the two must not be confused
// by whoever reads this next.)
func envValueLast(env []string, key string) string {
	out := ""
	for _, e := range env {
		if rest, ok := strings.CutPrefix(e, key+"="); ok {
			out = rest
		}
	}
	return out
}

// DialableAddr turns a LISTEN address into one that can be dialled. `0.0.0.0` and `[::]` are
// wildcards a server binds; nothing connects to them.
func DialableAddr(addr string) string {
	for _, wildcard := range []string{"0.0.0.0:", "[::]:", ":::"} {
		if rest, ok := strings.CutPrefix(addr, wildcard); ok {
			return "127.0.0.1:" + rest
		}
	}
	if strings.HasPrefix(addr, ":") {
		return "127.0.0.1" + addr
	}
	return addr
}

// --- the scrape ---------------------------------------------------------------------------------

// errNoLoadSeries is what a scrape answers when the exposition parsed but carried neither counter.
// A sentinel because "this Worker serves no load counters" and "this Worker could not be reached"
// send an operator to two different places: the first is an actor host older than this slice, the
// second is a listener that is off or unreachable.
var errNoLoadSeries = errors.New("no load counters in the exposition")

// ScrapeLoadCounters reads one Worker's two counters.
//
// The whole exposition is summed per metric NAME across label sets. Both hosts emit exactly one
// series per counter today, but summing rather than taking the first is what keeps this correct if
// a host ever adds a label — and it is what the Prometheus data model means by a counter anyway.
func ScrapeLoadCounters(ctx context.Context, client *http.Client, addr string) (LoadCounters, error) {
	ctx, cancel := context.WithTimeout(ctx, wardenHealthScrapeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/metrics", nil)
	if err != nil {
		return LoadCounters{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return LoadCounters{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return LoadCounters{}, fmt.Errorf("%s answered %s", addr, resp.Status)
	}
	// Bounded: an exposition is a few hundred bytes and this reads a stranger's listener, which may
	// not be an actor host at all.
	return ParseLoadCounters(io.LimitReader(resp.Body, 1<<20))
}

// ParseLoadCounters reads the two counters out of a Prometheus text exposition.
//
// Deliberately NOT a general exposition parser. It matches a line whose metric name is one of the
// two, with or without a label set, and sums the values — which is the whole of what this file needs
// and is small enough to be obviously right. A dependency that parsed the format properly would be a
// dependency in the CLI's go.mod for four lines of text.
func ParseLoadCounters(r io.Reader) (LoadCounters, error) {
	var c LoadCounters
	var seen bool
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		// `# HELP kontra_resource_loads_total …` carries the name and no value.
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := expositionSample(line)
		if !ok {
			continue
		}
		switch name {
		case MetricLoadsTotal:
			c.loads += value
			seen = true
		case MetricLoadFailTotal:
			c.failures += value
			seen = true
		}
	}
	if err := sc.Err(); err != nil {
		return LoadCounters{}, err
	}
	if !seen {
		return LoadCounters{}, fmt.Errorf("%w (%s / %s) — this actor host predates them, or it is not an "+
			"actor host at all", errNoLoadSeries, MetricLoadsTotal, MetricLoadFailTotal)
	}
	return c, nil
}

// expositionSample splits one sample line into its metric name and value.
//
// The label set is DISCARDED rather than parsed, and the name is taken up to the first `{` — which
// is the format's own rule and needs no bracket matching, because a metric name may not contain one.
func expositionSample(line string) (string, float64, bool) {
	cut := strings.LastIndexByte(line, ' ')
	if cut < 0 {
		return "", 0, false
	}
	value, err := strconv.ParseFloat(strings.TrimSpace(line[cut+1:]), 64)
	if err != nil {
		return "", 0, false
	}
	head := strings.TrimSpace(line[:cut])
	if brace := strings.IndexByte(head, '{'); brace >= 0 {
		head = head[:brace]
	}
	if head == "" {
		return "", 0, false
	}
	return head, value, true
}

// --- the sampler ---------------------------------------------------------------------------------

// WorkerHealth is the Warden's memory of what each Worker's counters have been doing.
//
// IT HOLDS READINGS AND NOTHING ELSE. driver.go's rule — "a driver's memory of what it started is a
// CACHE, the runtime is the truth" — applies one level up and this obeys it: nothing here answers
// "is this Worker running", which is `list()`'s question, and a Worker that vanishes simply stops
// being sampled. The readings die with the process, which is right: a Machine that has just rebooted
// has no window and should say so rather than judging on a previous life's numbers.
type WorkerHealth struct {
	now    func() time.Time
	client *http.Client

	// samples is per Worker id, oldest first, pruned to WardenHealthWindow.
	samples map[string][]LoadSample
	// last is the verdict each Worker most recently got, so the pane report can carry it without
	// re-scraping and so a log line can be emitted only when it CHANGES.
	last map[string]workerVerdict

	// scrape is the seam tests replace. Real callers leave it nil and get the HTTP one.
	scrape func(ctx context.Context, addr string) (LoadCounters, error)
}

func NewWorkerHealth(now func() time.Time) *WorkerHealth {
	return &WorkerHealth{
		now:     now,
		client:  &http.Client{Timeout: wardenHealthScrapeTimeout},
		samples: map[string][]LoadSample{},
		last:    map[string]workerVerdict{},
	}
}

// observe takes one reading of one Worker and returns what it now believes.
//
// NEVER RETURNS AN ERROR, because every failure here is a verdict. An unreachable listener is not a
// problem for the reconcile loop to handle — it is this check saying it cannot tell, which is one of
// the three answers it has.
func (w *WorkerHealth) observe(ctx context.Context, drv workerDriver, spec Spec, h workerHandle) workerVerdict {
	id := h.id()
	addr, err := MetricsAddrFor(ctx, drv, spec, h)
	if err != nil {
		return w.remember(id, workerVerdict{
			Verdict: healthCannotTell,
			Reason:  "address",
			Detail:  err.Error(),
		})
	}
	counters, err := w.read(ctx, addr)
	if err != nil {
		// A FAILED SCRAPE DOES NOT DISCARD THE WINDOW. A listener that missed one turn — a GC pause,
		// a container mid-restart — must not reset a five-minute window to nothing, or a Worker that
		// blips every few minutes can never be judged at all. The readings stay and age out normally.
		return w.remember(id, workerVerdict{
			Verdict: healthCannotTell,
			Reason:  "scrape",
			Detail: fmt.Sprintf("could not read %s's counters at %s: %v — the listener is off, or it is "+
				"bound to loopback inside a container netns (the Go actor host binds 127.0.0.1:%d by "+
				"default; set %s to move it)", id, addr, err, metricsPortDefault, metricsAddrEnv),
		})
	}

	at := w.now()
	kept := append(w.samples[id], LoadSample{at: at, LoadCounters: counters})
	// Prune to the window, keeping the FIRST sample at or before the cutoff as the window's left
	// edge. Dropping everything older than the cutoff outright would leave a span that shrinks to
	// nothing every time a sample ages out, and the verdict would flicker between a real ratio and
	// `span` on the sampling cadence.
	kept = PruneSamples(kept, at.Add(-WardenHealthWindow))
	w.samples[id] = kept
	return w.remember(id, JudgeSamples(kept))
}

func (w *WorkerHealth) read(ctx context.Context, addr string) (LoadCounters, error) {
	if w.scrape != nil {
		return w.scrape(ctx, addr)
	}
	return ScrapeLoadCounters(ctx, w.client, addr)
}

func (w *WorkerHealth) remember(id string, v workerVerdict) workerVerdict {
	w.last[id] = v
	return v
}

// verdict is what this Warden last concluded about a Worker, for a reader that must not scrape.
func (w *WorkerHealth) Verdict(id string) (workerVerdict, bool) {
	v, ok := w.last[id]
	return v, ok
}

// forgetSamples drops a Worker's READINGS and keeps its verdict.
//
// CALLED WHEN A WORKER IS STOPPED FOR BEING SICK, and the asymmetry is the whole point. The readings
// have to go: the replacement starts its counters at zero, so a window straddling the restart is a
// negative delta — honestly reported by the `reset` branch, but for the five minutes it takes the old
// readings to age out, during which nothing about the new Worker can be learned. The VERDICT has to
// stay: it is the finding an operator came for, and a Warden that dropped it would restart a Worker
// and then report `cannot tell` about the Machine it just repaired.
func (w *WorkerHealth) forgetSamples(id string) {
	delete(w.samples, id)
}

// forget drops both, for a Worker this Machine no longer holds at all.
func (w *WorkerHealth) forget(id string) {
	delete(w.samples, id)
	delete(w.last, id)
}

// retain drops the readings of every Worker that is no longer on this Machine. A Fleet that
// reassigns a Machine through many actors must not accumulate a window per actor it has ever held.
func (w *WorkerHealth) retain(live map[string]bool) {
	for id := range w.samples {
		if !live[id] {
			w.forget(id)
		}
	}
	for id := range w.last {
		if !live[id] {
			w.forget(id)
		}
	}
}

// PruneSamples keeps the readings inside the window, plus the newest one older than it.
func PruneSamples(samples []LoadSample, cutoff time.Time) []LoadSample {
	keep := 0
	for i, s := range samples {
		if s.at.Before(cutoff) {
			keep = i
		}
	}
	if keep == 0 {
		return samples
	}
	return append([]LoadSample(nil), samples[keep:]...)
}

// --- what the reconcile loop calls ---------------------------------------------------------------

// judge is the Warden's one question about a Worker that is running: is it working?
//
// It lives here and not in warden.go so that the loop's own file carries one call and no policy —
// the same split driver.go makes between the four verbs and everything that decides when to use
// them. A Warden with no `health` (which is every Warden a test builds, and any built before this
// slice) answers `cannot tell` and stops nothing.
//
// THE READINGS ARE DROPPED WHEN A WORKER IS JUDGED SICK — the readings only, never the verdict. See
// `forgetSamples` for why those two go different ways.
func (w *warden) Judge(ctx context.Context, spec Spec, h workerHandle) workerVerdict {
	if w.health == nil {
		return workerVerdict{
			Verdict: healthCannotTell,
			Reason:  "disabled",
			Detail:  "this Warden has no health judge, so nothing here decides a Worker is sick",
		}
	}
	v := w.health.observe(ctx, w.driver, spec, h)
	if v.Sick() {
		w.health.forgetSamples(h.id())
	}
	return v
}

// VerdictFor is the last thing the reconcile loop concluded about a Worker. READ, NEVER MEASURED:
// a caller must not scrape, because a second scraper on a different cadence would produce a second
// window and the two would disagree about the same Worker in the same minute.
//
// IT LIVED IN `panereport.go` UNTIL THE MONITOR WENT, which is why it is a method on `warden` rather
// than on `WorkerHealth`: the reporter had a `*warden` and no judge of its own. The two "cannot
// tell" answers below are the ones that must never read as "well" — `healthCannotTell` with a reason
// is a Worker nothing has judged yet, not a Worker that passed.
func (w *warden) VerdictFor(id string) workerVerdict {
	if w.health == nil {
		return workerVerdict{
			Verdict: healthCannotTell,
			Reason:  "disabled",
			Detail:  "this Warden has no health judge, so nothing here decides a Worker is sick",
		}
	}
	if v, ok := w.health.Verdict(id); ok {
		return v
	}
	return workerVerdict{
		Verdict: healthCannotTell,
		Reason:  "samples",
		Detail:  "this Warden has not judged " + id + " yet — its first reading is on the next reconcile turn",
	}
}
