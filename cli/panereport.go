package main

// panereport.go — what a **Machine** says about itself, and the one wire it says it on.
//
// ADR 0037: "Telemetry does not ride workflow history. CPU, memory, load ratios and tmux frames go
// over the existing pane-snapshot path. A 200×50 terminal frame through Temporal history is the
// shape this repo already measured as 86% of a workflow's events, for a value that is stale a second
// later."
//
// ═══ NOTHING HERE TOUCHES TEMPORAL, AND THAT IS THE POINT ═══
//
// This file imports no Temporal package, starts no workflow, signals nothing and records nothing.
// It is one HTTPS POST per interval, dialled BY the Machine — the same direction as every other
// arrow in 0037's diagram, which is what makes NAT, a private VPC and a customer's firewall work
// unchanged. `panereport_test.go` asserts the negative directly rather than trusting this paragraph.
//
// The measurement behind the refusal is this repo's own: a blob cursor's poll loop was 86% of a
// workflow's history at ~1,736 events/hour, and a workflow publishing 16k refs wrote 11.74 MB into
// history because a publish→subscribe writes the data twice. A pane frame at this file's geometry is
// up to ~5 KB of text per Worker, every three seconds. Through history that is a Machine that cannot
// run for a day; over this wire it is a POST that is thrown away when the next one arrives.
//
// ═══ WHY logs() IS THE RIGHT SOURCE HERE AND THE WRONG ONE NEXT DOOR ═══
//
// sickworker.go refuses `driver.logs()` as health evidence because it carries no clock, and a
// five-minute window cannot be derived from an untimestamped scrollback of unknown depth. A PANE has
// no such requirement: "the last screenful of what this Worker printed" is exactly what `logs()`
// can answer, and staleness is carried by the report's own timestamp rather than assumed. Same call,
// two questions, and only one of them needs a window.
//
// ═══ THE FRAME IS A RECTANGLE OF KNOWN SIZE, AND IT IS tmux.go's SIZE ═══
//
// TWO GEOMETRIES ALREADY EXIST IN THIS REPO AND ONLY ONE OF THEM IS ARGUED FOR.
// `control/orchestrator/src/panels/converge.ts` pins a fleet tmux session at 200×50, "what makes a snapshot a
// rectangle of known size instead of whatever the last client happened to be". `cli/tmux.go` then
// measured the consequence and chose differently for the panes it creates: a tile scales type so the
// whole pane fits (`fitSourceWidth` in TerminalTile.tsx), and "200 columns in a default ~794px tile
// lands at about 6.6px, which is technically unwrapped and practically unreadable", against ~11px at
// 120.
//
// So this reuses `paneCols`/`paneRows` from tmux.go rather than introducing a third constant. A
// Warden-sourced fleet pane is then the same rectangle as a `kontra serve --tmux` pane, drawn by the
// same tile at the same type size — and there is exactly one place to change if that number is ever
// wrong again.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// wardenReportPath is where a report lands. Same shape as the assignment fetch: the Controller
// derives WHICH Machine this is from the certificate the handshake proved, never from a field in the
// body, so a Warden cannot file a report about another Machine even by trying.
const wardenReportPath = "/api/panels/report"

// wardenReportURLEnv points a Warden at a Controller that does not serve the panels ingest on its
// enrolment origin. Read here rather than added as a flag on `warden serve`, so that the whole of
// this feature is one file plus two lines in the loop.
const wardenReportURLEnv = "KONTRA_WARDEN_REPORT_URL"

const (
	// reportInterval matches the streamer's own snapshot cadence (DEFAULT_SNAPSHOT_MS = 3000). A
	// Machine that reported faster would be filling a buffer the Monitor overwrites unread.
	reportInterval = 3 * time.Second

	// reportTimeout bounds one POST. Shorter than the interval, so a Controller that has stopped
	// accepting reports cannot make this goroutine fall behind its own clock.
	reportTimeout = 2 * time.Second
)

// --- the wire ------------------------------------------------------------------------------------

// paneReport is one Machine's whole statement about itself. Pinned by
// `shared/conformance/workerhealth.json`, because the reader is TypeScript
// (`control/orchestrator/src/panels/warden.ts`) and a field renamed on one side of that boundary has no failure
// mode louder than a pane that never appears.
type paneReport struct {
	Warden  string `json:"warden"`
	Machine string `json:"machine"`
	Driver  string `json:"driver"`
	// At is the MACHINE's clock, in RFC3339. The Controller does not trust it for freshness — it
	// stamps its own arrival time — and carries it so a skewed Machine is visible as a skew rather
	// than as a pane that is mysteriously always stale.
	At        string        `json:"at"`
	Telemetry paneTelemetry `json:"telemetry"`
	Workers   []paneWorker  `json:"workers"`
}

// paneTelemetry is the **Machine**, not a Worker. Every field is a pointer so that "not measured on
// this platform" is a different value from zero — /proc is Linux's, and a Warden built for anything
// else reports the fields it has rather than a machine that is idle.
type paneTelemetry struct {
	// CPU is the fraction of one interval spent NOT idle, across all cores. 0..1.
	CPU *float64 `json:"cpu,omitempty"`
	// Memory is used/total. 0..1.
	Memory *float64 `json:"memory,omitempty"`
	// Load1 is the kernel's own one-minute run-queue average. Reported beside CPU rather than
	// instead of it: they answer different questions, and a Machine at 100% CPU with a load of 1 is
	// working while the same CPU at a load of 40 is thrashing.
	Load1 *float64 `json:"load1,omitempty"`
}

type paneWorker struct {
	ID      string     `json:"id"`
	Name    string     `json:"name"`
	Version string     `json:"version"`
	Whole   bool       `json:"whole"`
	Halves  []string   `json:"halves"`
	Health  paneHealth `json:"health"`
	Panes   []pane     `json:"panes"`
}

// paneHealth is a `workerVerdict` on the wire. Verdict and Reason are the machine-readable pair the
// corpus pins; Detail is the sentence an operator acts on. All three travel, for the reason
// `control/orchestrator/src/panels/types.ts` gives about its own health block: a verdict whose only explanation
// is prose is one nothing can assert on, and a verdict with no prose is one nobody can act on.
type paneHealth struct {
	Verdict     string  `json:"verdict"`
	Reason      string  `json:"reason"`
	Detail      string  `json:"detail,omitempty"`
	Ratio       float64 `json:"ratio"`
	Loads       float64 `json:"loads"`
	Failures    float64 `json:"failures"`
	SpanSeconds float64 `json:"spanSeconds"`
}

// pane is one window's screen: the same `<actor>` / `<handler>` window names the tmux path uses, so
// a Terminal id built from a Warden report and one built from `list-panes` name the same thing.
type pane struct {
	Window string `json:"window"`
	Cols   int    `json:"cols"`
	Rows   int    `json:"rows"`
	Frame  string `json:"frame"`
}

// --- the reporter ---------------------------------------------------------------------------------

type paneReporter struct {
	w        *warden
	url      string
	interval time.Duration

	// post is the seam a test replaces. Real callers leave it nil and get the Warden's own mTLS
	// client, which is the same one the assignment is fetched with — one identity, one trust store,
	// one place a certificate is checked.
	post func(ctx context.Context, url string, body []byte) error

	// hostname and readers are fields so a test can drive telemetry without a /proc.
	hostname  func() string
	telemetry func() paneTelemetry
}

func newPaneReporter(w *warden) *paneReporter {
	return &paneReporter{
		w:         w,
		url:       reportURLFor(w),
		interval:  reportInterval,
		hostname:  func() string { h, _ := os.Hostname(); return h },
		telemetry: readTelemetry,
	}
}

// reportURLFor decides where reports go.
//
// THE ENROLMENT ORIGIN IS THE DEFAULT, because it is the one address this Machine has already
// proved it can reach and already holds a certificate for. An installation that serves the panels
// ingest somewhere else says so through the environment; there is no discovery, because a Machine
// guessing at a Controller's topology is how a Fleet ends up reporting into a port nobody is
// listening on and looking exactly like a Fleet with no telemetry.
func reportURLFor(w *warden) string {
	if v := strings.TrimSpace(os.Getenv(wardenReportURLEnv)); v != "" {
		return v
	}
	if w == nil || w.id == nil {
		return ""
	}
	return strings.TrimRight(w.id.Record.Controller, "/") + wardenReportPath
}

// run posts a report every interval until ctx is done.
//
// A FAILED POST IS A LOG LINE AND NOTHING ELSE. Telemetry is the one thing on a Machine that may be
// lost without consequence — the next report replaces it — and a reporter that retried, buffered or
// returned would be trading the property this whole file exists for: that watching a Fleet cannot
// cost it work.
func (r *paneReporter) run(ctx context.Context) {
	if r == nil || r.url == "" {
		return
	}
	for {
		if err := r.once(ctx); err != nil && ctx.Err() == nil {
			r.w.logf("could not report this Machine's panes to %s: %v", r.url, err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(r.interval):
		}
	}
}

func (r *paneReporter) once(ctx context.Context) error {
	report, err := r.snapshot(ctx)
	if err != nil {
		return err
	}
	body, err := json.Marshal(report)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, reportTimeout)
	defer cancel()
	if r.post != nil {
		return r.post(ctx, r.url, body)
	}
	return r.httpPost(ctx, r.url, body)
}

// snapshot builds one report from what the runtime currently holds.
//
// EVERYTHING COMES FROM `list()`, which is driver.go's rule one level up: a report assembled from
// what this Warden remembers starting would show a Worker that died an hour ago, which is the exact
// failure the pane path exists to make visible.
func (r *paneReporter) snapshot(ctx context.Context) (paneReport, error) {
	handles, err := r.w.driver.list(ctx)
	if err != nil {
		return paneReport{}, fmt.Errorf("cannot read what is running: %w", err)
	}
	report := paneReport{
		Machine:   r.hostname(),
		Driver:    r.w.driver.driverName(),
		At:        r.w.now().UTC().Format(time.RFC3339),
		Telemetry: r.telemetry(),
		Workers:   make([]paneWorker, 0, len(handles)),
	}
	if r.w.id != nil {
		report.Warden = r.w.id.Record.WardenID
	}

	live := make(map[string]bool, len(handles))
	for _, h := range handles {
		live[h.id()] = true
		pw := paneWorker{
			ID: h.id(), Name: h.Name, Version: h.Version,
			Whole:  h.whole(),
			Halves: make([]string, 0, len(h.Halves)),
			Health: healthOnWire(r.w.verdictFor(h.id())),
			Panes:  r.panesFor(ctx, h),
		}
		for _, part := range workerParts {
			if _, ok := h.half(part); ok {
				pw.Halves = append(pw.Halves, string(part))
			}
		}
		report.Workers = append(report.Workers, pw)
	}
	// The one place that sees the whole live set, so it is the one that prunes the health readings
	// of Workers this Machine no longer holds. A Fleet that reassigns a Machine through many actors
	// must not accumulate a five-minute window per actor it has ever run.
	if r.w.health != nil {
		r.w.health.retain(live)
	}
	return report, nil
}

// verdictFor is the last thing the reconcile loop concluded about a Worker. READ, NEVER MEASURED:
// the reporter must not scrape, because a second scraper on a different cadence would produce a
// second window and the two would disagree about the same Worker in the same minute.
func (w *warden) verdictFor(id string) workerVerdict {
	if w.health == nil {
		return workerVerdict{
			Verdict: healthCannotTell,
			Reason:  "disabled",
			Detail:  "this Warden has no health judge, so nothing here decides a Worker is sick",
		}
	}
	if v, ok := w.health.verdict(id); ok {
		return v
	}
	return workerVerdict{
		Verdict: healthCannotTell,
		Reason:  "samples",
		Detail:  "this Warden has not judged " + id + " yet — its first reading is on the next reconcile turn",
	}
}

func healthOnWire(v workerVerdict) paneHealth {
	return paneHealth{
		Verdict:     string(v.Verdict),
		Reason:      v.Reason,
		Detail:      v.Detail,
		Ratio:       v.Ratio,
		Loads:       v.Loads,
		Failures:    v.Failures,
		SpanSeconds: v.Span.Seconds(),
	}
}

// panesFor renders one Worker's two windows.
//
// ONE `logs()` CALL FOR THE PAIR, because that is what the seam offers: both drivers return the two
// halves in one stream, separated by the `== actor ==` / `== handler ==` banner they both write. A
// driver that retained nothing produces no panes at all rather than two empty ones — driver.go's own
// rule, that "an empty log and a log that was never kept are different answers".
func (r *paneReporter) panesFor(ctx context.Context, h workerHandle) []pane {
	rc, err := r.w.driver.logs(ctx, h)
	if err != nil {
		return nil
	}
	defer rc.Close()
	// Bounded: a Worker that printed a gigabyte must not become this process's memory profile, and
	// only the tail is ever rendered. 1 MiB is twenty screens at this geometry.
	raw, err := io.ReadAll(io.LimitReader(rc, 1<<20))
	if err != nil {
		return nil
	}
	out := make([]pane, 0, len(workerParts))
	for _, part := range workerParts {
		body, ok := logSection(string(raw), part)
		if !ok {
			continue
		}
		out = append(out, pane{Window: string(part), Cols: paneCols, Rows: paneRows, Frame: frameOf(body)})
	}
	return out
}

// logSection pulls one half's output out of the combined stream both drivers produce.
//
// The banner is `== actor ==` on its own line, written by driver_process.go and driver_podman.go
// with the same `fmt.Fprintf(&buf, "== %s ==\n", part)`. Matching on the whole line rather than on
// the substring is what stops a Worker that PRINTS `== handler ==` from splitting its own pane.
func logSection(all string, part workerPart) (string, bool) {
	banner := "== " + string(part) + " =="
	lines := strings.Split(all, "\n")
	start := -1
	for i, line := range lines {
		if strings.TrimRight(line, "\r") == banner {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return "", false
	}
	end := len(lines)
	for i := start; i < len(lines); i++ {
		if trimmed := strings.TrimRight(lines[i], "\r"); strings.HasPrefix(trimmed, "== ") &&
			strings.HasSuffix(trimmed, " ==") {
			end = i
			break
		}
	}
	return strings.Join(lines[start:end], "\n"), true
}

// frameOf is the last `paneRows` lines, each cut to `paneCols` — a screen, not a log.
//
// TRUNCATED BY RUNE AND NOT BY BYTE. A Worker's output is arbitrary UTF-8 (this repo carries `café`
// as an adversarial actor name on purpose), and cutting a multi-byte rune in half produces invalid
// JSON escapes on the way out and a replacement character on the wall.
func frameOf(body string) string {
	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	if len(lines) > paneRows {
		lines = lines[len(lines)-paneRows:]
	}
	for i, line := range lines {
		line = strings.TrimRight(line, "\r")
		if r := []rune(line); len(r) > paneCols {
			line = string(r[:paneCols])
		}
		lines[i] = line
	}
	return strings.Join(lines, "\n")
}

func (r *paneReporter) httpPost(ctx context.Context, url string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	client := r.w.http
	if client == nil {
		client = &http.Client{Timeout: reportTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	// The body is drained rather than ignored: an undrained response body keeps the connection out
	// of the client's keep-alive pool, so a Warden reporting every three seconds would open a fresh
	// TLS handshake per report for the life of the Machine.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return nil
}

// --- the Machine's own numbers --------------------------------------------------------------------

// readTelemetry reads what /proc will tell us.
//
// EVERY FIELD IS OPTIONAL AND A MISSING ONE IS ABSENT, NOT ZERO. `control/orchestrator/src/panels/types.ts`
// states the rule this follows: "`unknown` is a value, never a shrug" — a Machine whose /proc could
// not be read is not a Machine that is idle, and a tile rendering 0% CPU for it would be the
// friendliest possible lie.
func readTelemetry() paneTelemetry {
	var t paneTelemetry
	if v, ok := readLoad1("/proc/loadavg"); ok {
		t.Load1 = &v
	}
	if v, ok := readMemoryUsed("/proc/meminfo"); ok {
		t.Memory = &v
	}
	if v, ok := sampleCPU(); ok {
		t.CPU = &v
	}
	return t
}

// cpuPrev is the previous /proc/stat reading. CPU is a RATE and the file is a set of counters, so
// one reading cannot answer the question — the first call after boot legitimately reports nothing.
var cpuPrev struct{ total, idle float64 }

func sampleCPU() (float64, bool) {
	total, idle, ok := readCPUJiffies("/proc/stat")
	if !ok {
		return 0, false
	}
	prevTotal, prevIdle := cpuPrev.total, cpuPrev.idle
	cpuPrev.total, cpuPrev.idle = total, idle
	dt := total - prevTotal
	if prevTotal == 0 || dt <= 0 {
		return 0, false
	}
	busy := dt - (idle - prevIdle)
	if busy < 0 {
		busy = 0
	}
	return busy / dt, true
}

// readCPUJiffies returns the aggregate `cpu` line's total and idle counters.
//
// IDLE IS FIELDS 4 AND 5 TOGETHER — `idle` and `iowait`. A Machine blocked on a disk is not busy,
// and counting iowait as work is what makes a Fleet doing large object-store reads look pinned.
func readCPUJiffies(path string) (total, idle float64, ok bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, false
	}
	for _, line := range strings.Split(string(raw), "\n") {
		f := strings.Fields(line)
		if len(f) < 6 || f[0] != "cpu" {
			continue
		}
		for i, field := range f[1:] {
			v, err := strconv.ParseFloat(field, 64)
			if err != nil {
				return 0, 0, false
			}
			total += v
			if i == 3 || i == 4 {
				idle += v
			}
		}
		return total, idle, true
	}
	return 0, 0, false
}

// readMemoryUsed is (MemTotal - MemAvailable) / MemTotal.
//
// MemAvailable AND NOT MemFree, which is the difference between a number an operator can act on and
// one that alarms them every time: Linux spends every spare page on cache, so MemFree on a healthy
// Machine is near zero and always has been.
func readMemoryUsed(path string) (float64, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	var total, available float64
	for _, line := range strings.Split(string(raw), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		v, err := strconv.ParseFloat(f[1], 64)
		if err != nil {
			continue
		}
		switch f[0] {
		case "MemTotal:":
			total = v
		case "MemAvailable:":
			available = v
		}
	}
	if total <= 0 || available <= 0 {
		return 0, false
	}
	used := (total - available) / total
	if used < 0 {
		used = 0
	}
	return used, true
}

func readLoad1(path string) (float64, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	f := strings.Fields(string(raw))
	if len(f) == 0 {
		return 0, false
	}
	v, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return 0, false
	}
	return v, true
}
