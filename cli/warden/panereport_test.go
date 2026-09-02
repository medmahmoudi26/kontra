package warden

// panereport_test.go — the telemetry wire, and the negative that is the whole reason it exists.
//
// The negative — "none of this touches workflow history" — is the one most at risk of being a
// vacuous guard, because it passes against a file that does nothing at all. So it is asserted with a
// POSITIVE CONTROL in the same test: the sweep must FIND Temporal in the files that legitimately use
// it before it is allowed to conclude anything from not finding it here.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/medmahmoudi26/kontra/cli/internal/tmux"
)

// --- NOTHING RIDES WORKFLOW HISTORY --------------------------------------------------------------

// ADR 0037: "Telemetry does not ride workflow history… A 200×50 terminal frame through Temporal
// history is the shape this repo already measured as 86% of a workflow's events, for a value that is
// stale a second later."
//
// This is a source sweep and it is honest about being one: it cannot prove a runtime property, it
// proves that the two files that carry telemetry cannot reach Temporal at all, which is the
// structural version of the same claim. The CONTROL is what makes it worth running — a sweep that
// found nothing anywhere would pass on a typo in the pattern, which is the first vacuous guard this
// program found (a walk that visited zero files).
func TestTelemetryCannotReachTemporal(t *testing.T) {
	const temporalImport = "go.temporal.io/"

	// THE CONTROL, FIRST. Some file in this package must import Temporal, or the pattern is wrong.
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the package: %v", err)
	}
	var importers []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		raw, err := os.ReadFile(e.Name())
		if err != nil {
			t.Fatalf("reading %s: %v", e.Name(), err)
		}
		if strings.Contains(string(raw), temporalImport) {
			importers = append(importers, e.Name())
		}
	}
	if len(importers) == 0 {
		t.Fatalf("no file in this package imports %q, so this sweep proves nothing about the ones "+
			"that do not — fix the pattern before trusting the assertion below", temporalImport)
	}

	// …and now the claim.
	for _, f := range []string{"panereport.go", "sickworker.go"} {
		for _, importer := range importers {
			if importer == f {
				t.Errorf("%s imports %s — telemetry must not reach workflow history (ADR 0037)", f, temporalImport)
			}
		}
	}
}

// The report is DIALLED BY THE MACHINE, which is what makes NAT and a customer's firewall work. A
// reporter that listened would be an inbound port on a fleet Machine, and `warden_test.go` already
// asserts the loop opens none — this is the same property for the half that talks the most.
func TestTheReporterOpensNoListener(t *testing.T) {
	before := listeningPorts(t)
	w := testWarden(t, "wlisten")
	w.now = time.Now
	r := &PaneReporter{w: w, url: "https://controller.invalid/api/panels/report", interval: time.Hour,
		hostname:  func() string { return "kf-test-01" },
		telemetry: func() PaneTelemetry { return PaneTelemetry{} },
		post: func(context.Context, string, []byte) error {
			return errors.New("the Controller is not reachable in a test, which is the point")
		}}
	// One turn, and a failing one: the interesting case is a reporter that could not deliver, since
	// that is when a naive implementation starts buffering or retrying on a socket of its own.
	_ = r.Once(context.Background())

	after := listeningPorts(t)
	for port := range after {
		if !before[port] {
			t.Errorf("the reporter left something listening on %s; a Machine accepts no inbound "+
				"connection (ADR 0037)", port)
		}
	}
}

// listeningPorts is warden_test.go's /proc/net/tcp read, narrowed to what this file needs. Duplicated
// rather than exported from there: warden_test.go belongs to slice 04's agent this week, and a helper
// shared across that boundary is a merge conflict in a file neither of us is editing for the same
// reason.
func listeningPorts(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, path := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(raw), "\n")[1:] {
			f := strings.Fields(line)
			// st == 0A is TCP_LISTEN.
			if len(f) > 3 && f[3] == "0A" {
				out[f[1]] = true
			}
		}
	}
	return out
}

// --- what a report says ---------------------------------------------------------------------------

// The whole path, against a real HTTP server: a real Worker in a real tmux session, its real
// scrollback read through the real driver, rendered into panes and POSTed as JSON.
//
// tmux, because the `process` driver's `logs()` refuses without it — "a foreground `process` Worker
// writes to the terminal that started it and nothing keeps a copy". That refusal is a case this
// tests too, below.
func TestAReportCarriesThePanesAndTheHealthVerdict(t *testing.T) {
	requireProcTable(t)
	if !tmux.Available() {
		t.Skip("tmux is not installed")
	}
	const name, version = "wreport", "0.1.0"
	session := tmux.Session(name, version)
	t.Cleanup(func() { _ = exec.Command("tmux", "kill-session", "-t", session).Run() })
	if tmux.HasSession(session) {
		t.Fatalf("tmux session %s already exists; this test would adopt somebody else's", session)
	}

	drv := &ProcessDriver{out: io.Discard, err: io.Discard, tmux: true}
	c := newClock()
	w := &warden{driver: drv, out: &testLog{t: t}, now: c.now, interval: time.Hour}
	w.health = NewWorkerHealth(c.now)
	counters := &fakeCounters{}
	counters.set(400, 12)
	w.health.scrape = counters.scrape
	t.Cleanup(func() {
		hs, err := drv.list(context.Background())
		if err != nil {
			return
		}
		for _, h := range hs {
			if h.Name == name {
				_ = drv.Stop(context.Background(), h, 0)
			}
		}
	})

	spec := Spec{
		Name: name, Version: version,
		Actor: ProcSpec{Dir: t.TempDir(), Env: append(os.Environ(), metricsAddrEnv+"=127.0.0.1:19110"),
			Argv: []string{"sh", "-c", "echo 'actor is up and polling'; sleep 600"}},
		Handler: ProcSpec{Dir: t.TempDir(), Env: os.Environ(),
			Argv: []string{"sh", "-c", "echo 'handler is up and polling'; sleep 600"}},
	}
	h, err := drv.Start(context.Background(), spec)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	// The Worker's output has to have reached the pane, or the frame assertions below would pass
	// against a reporter that returned empty strings.
	waitFor(t, func() bool {
		rc, err := drv.logs(context.Background(), h)
		if err != nil {
			return false
		}
		defer rc.Close()
		body, _ := io.ReadAll(rc)
		return strings.Contains(string(body), "actor is up and polling")
	}, "the actor half's output to reach its pane")

	// Two readings five minutes apart, the second one terrible: the report has to carry the
	// verdict, not recompute it.
	_ = w.Judge(context.Background(), spec, h)
	c.advance(5 * time.Minute)
	counters.set(482, 93)
	if v := w.Judge(context.Background(), spec, h); v.Verdict != HealthSick {
		t.Fatalf("setting up: expected a sick verdict, got %q (%s)", v.Verdict, v.Detail)
	}

	var got PaneReport
	var contentType string
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		contentType = r.Header.Get("content-type")
		if r.URL.Path != WardenReportPath {
			rw.WriteHeader(http.StatusNotFound)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("the report is not readable JSON: %v", err)
		}
		rw.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	r := &PaneReporter{w: w, url: srv.URL + WardenReportPath, interval: time.Hour,
		hostname:  func() string { return "kf-report-01" },
		telemetry: func() PaneTelemetry { cpu := 0.42; return PaneTelemetry{CPU: &cpu} }}
	w.http = srv.Client()
	if err := r.Once(context.Background()); err != nil {
		t.Fatalf("posting the report: %v", err)
	}

	if contentType != "application/json" {
		t.Errorf("content-type = %q", contentType)
	}
	if got.Machine != "kf-report-01" || got.Driver != "process" {
		t.Errorf("report identifies the Machine as %q on the %q driver", got.Machine, got.Driver)
	}
	if got.Telemetry.CPU == nil || *got.Telemetry.CPU != 0.42 {
		t.Errorf("telemetry did not travel: %+v", got.Telemetry)
	}
	var reported *PaneWorker
	for i := range got.Workers {
		if got.Workers[i].Name == name {
			reported = &got.Workers[i]
		}
	}
	if reported == nil {
		t.Fatalf("the report carries no Worker called %s: %+v", name, got.Workers)
	}
	if reported.Health.Verdict != string(HealthSick) || reported.Health.Failures != 81 || reported.Health.Loads != 82 {
		t.Errorf("the verdict did not travel: %+v", reported.Health)
	}
	if reported.Health.Detail == "" {
		t.Error("a sick verdict crossed the wire with no sentence an operator could act on")
	}
	frames := map[string]string{}
	for _, p := range reported.Panes {
		frames[p.Window] = p.Frame
		if p.Cols != tmux.PaneCols || p.Rows != tmux.PaneRows {
			t.Errorf("pane %s is %dx%d; the geometry is %dx%d", p.Window, p.Cols, p.Rows, tmux.PaneCols, tmux.PaneRows)
		}
	}
	if !strings.Contains(frames["actor"], "actor is up and polling") {
		t.Errorf("the actor pane's frame does not carry what the actor printed: %q", frames["actor"])
	}
	if strings.Contains(frames["actor"], "handler is up and polling") {
		t.Errorf("the actor pane's frame carries the HANDLER's output; the sections did not split: %q",
			frames["actor"])
	}
}

// A runtime that kept no log produces NO panes rather than two empty ones. driver.go's rule: "an
// empty log and a log that was never kept are different answers and only one of them means the
// Worker printed nothing."
func TestARuntimeThatKeptNoLogsReportsNoPanes(t *testing.T) {
	requireProcTable(t)
	drv := &ProcessDriver{out: io.Discard, err: io.Discard} // no tmux: a foreground pair
	w := &warden{driver: drv, out: &testLog{t: t}, now: time.Now, interval: time.Hour}
	r := &PaneReporter{w: w, hostname: func() string { return "x" },
		telemetry: func() PaneTelemetry { return PaneTelemetry{} }}

	h := workerHandle{Driver: "process", Name: "wnolog", Version: "0.1.0",
		Halves: []workerHalf{{Part: partActor, Ref: "1"}}}
	// The control: this driver really does refuse, or the assertion below is about nothing.
	if _, err := drv.logs(context.Background(), h); !errors.Is(err, errNoRetainedLogs) {
		t.Fatalf("expected the foreground process driver to refuse; got %v", err)
	}
	if panes := r.PanesFor(context.Background(), h); len(panes) != 0 {
		t.Fatalf("a Worker with no retained log reported %d pane(s)", len(panes))
	}
}

// --- the frame is a screen -------------------------------------------------------------------------

func TestAFrameIsTheLastScreenful(t *testing.T) {
	var b strings.Builder
	for i := 0; i < tmux.PaneRows*3; i++ {
		b.WriteString("line ")
		b.WriteString(strings.Repeat("x", 300))
		b.WriteString("\n")
	}
	frame := FrameOf(b.String())
	lines := strings.Split(frame, "\n")
	if len(lines) != tmux.PaneRows {
		t.Fatalf("frame is %d lines; a screen is %d", len(lines), tmux.PaneRows)
	}
	for _, l := range lines {
		if got := len([]rune(l)); got != tmux.PaneCols {
			t.Fatalf("a line is %d columns wide; the pane is %d", got, tmux.PaneCols)
		}
	}
}

// TRUNCATED BY RUNE, NOT BY BYTE. `shared/conformance/queues.json` carries `café` as an adversarial actor
// name on purpose, and a Worker's output is arbitrary UTF-8; half a rune is a replacement character
// on the wall and an escaping problem on the way there.
func TestAFrameIsCutOnRuneBoundaries(t *testing.T) {
	line := strings.Repeat("é", tmux.PaneCols+40)
	frame := FrameOf(line)
	if got := len([]rune(frame)); got != tmux.PaneCols {
		t.Fatalf("cut to %d runes, want %d", got, tmux.PaneCols)
	}
	if !utf8Valid(frame) {
		t.Fatal("the frame is not valid UTF-8, so a rune was cut in half")
	}
}

func utf8Valid(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}

// The banner both drivers write is matched as a WHOLE LINE, at both ends of a section.
//
// A Worker's output is the one part of this stream nobody here wrote, so it is the part that gets to
// try. The property is bounded and stated rather than overclaimed: a line that CONTAINS a banner
// cannot start or end a section, which covers every ordinary log line ("the actor prints: ==
// handler =="). A Worker that prints a line that IS exactly `== handler ==`, alone, is
// indistinguishable from the driver's own banner — that ambiguity is in the drivers' format (a plain
// `fmt.Fprintf(&buf, "== %s ==\n", part)` with no delimiter a payload cannot contain) and this file
// cannot resolve it from the stream. The cost is a truncated pane, not a wrong verdict.
func TestAWorkerCannotForgeThePaneBanner(t *testing.T) {
	combined := "== actor ==\nreal actor output\n== handler ==\nreal handler output\n"
	body, ok := LogSection(combined, partActor)
	if !ok || strings.TrimSpace(body) != "real actor output" {
		t.Fatalf("actor section = %q (ok=%v)", body, ok)
	}
	body, ok = LogSection(combined, partHandler)
	if !ok || strings.TrimSpace(body) != "real handler output" {
		t.Fatalf("handler section = %q (ok=%v)", body, ok)
	}

	// A line that CONTAINS the section's own banner must not START it — otherwise the real banner
	// that follows lands inside the body and the first line of the pane is `== actor ==`.
	late := "the actor prints: == actor ==\n== actor ==\nreal actor output\n"
	body, ok = LogSection(late, partActor)
	if !ok {
		t.Fatal("no actor section")
	}
	if strings.TrimSpace(body) != "real actor output" {
		t.Errorf("a line merely CONTAINING the banner started the section, so the pane is not the "+
			"actor's output: %q", body)
	}

	// …and must not END it.
	forged := "== actor ==\nthe actor prints: == handler ==\nstill the actor\n== handler ==\nthe handler\n"
	body, ok = LogSection(forged, partActor)
	if !ok {
		t.Fatal("no actor section")
	}
	if !strings.Contains(body, "still the actor") {
		t.Errorf("a line CONTAINING the banner ended the actor's section: %q", body)
	}
	if strings.Contains(body, "the handler\n") {
		t.Errorf("the handler's output leaked into the actor's pane: %q", body)
	}
}

// --- where a report goes ---------------------------------------------------------------------------

// The default is the origin the Machine enrolled with — the one address it has already proved it can
// reach and already holds a certificate for. There is deliberately no discovery.
func TestAReportGoesToTheEnrolmentOriginUnlessToldOtherwise(t *testing.T) {
	w := &warden{id: &wardenIdentity{Record: wardenRecord{Controller: "https://controller.example:8443/"}}}
	t.Setenv(wardenReportURLEnv, "")
	if got := ReportURLFor(w); got != "https://controller.example:8443"+WardenReportPath {
		t.Errorf("default report url = %q", got)
	}
	t.Setenv(wardenReportURLEnv, "https://elsewhere.example/ingest")
	if got := ReportURLFor(w); got != "https://elsewhere.example/ingest" {
		t.Errorf("overridden report url = %q", got)
	}
}

// A reporter with nowhere to report does NOTHING — no goroutine spinning, no log line every three
// seconds for the life of the Machine.
func TestAReporterWithNoUrlIsSilent(t *testing.T) {
	var mu sync.Mutex
	lines := 0
	w := &warden{now: time.Now, out: writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		lines++
		mu.Unlock()
		return len(p), nil
	})}
	r := &PaneReporter{w: w, url: "", interval: time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	r.Run(ctx)
	mu.Lock()
	defer mu.Unlock()
	if lines != 0 {
		t.Fatalf("a reporter with no URL wrote %d log line(s)", lines)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// --- the Machine's own numbers ----------------------------------------------------------------------

// A FIELD THAT COULD NOT BE MEASURED IS ABSENT, NOT ZERO. `panels/types.ts`: "`unknown` is a value,
// never a shrug." A Machine whose /proc could not be read is not a Machine that is idle, and 0% CPU
// on the wall would be the friendliest possible lie.
func TestUnreadableTelemetryIsAbsentRatherThanZero(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope")
	if _, ok := ReadLoad1(missing); ok {
		t.Error("load1 claimed a reading from a file that does not exist")
	}
	if _, ok := ReadMemoryUsed(missing); ok {
		t.Error("memory claimed a reading from a file that does not exist")
	}
	// …and the JSON omits it, which is what the reader on the other side actually sees.
	body, err := json.Marshal(PaneReport{Telemetry: PaneTelemetry{}})
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"cpu"`, `"memory"`, `"load1"`} {
		if strings.Contains(string(body), field) {
			t.Errorf("an unmeasured %s reached the wire: %s", field, body)
		}
	}
}

// iowait counts as IDLE. A Machine blocked on an object-store read is not busy, and counting it as
// work is what makes a Fleet doing large reads look permanently pinned.
func TestCpuCountsIowaitAsIdle(t *testing.T) {
	dir := t.TempDir()
	stat := filepath.Join(dir, "stat")
	// user nice system idle iowait irq softirq steal
	if err := os.WriteFile(stat, []byte("cpu  10 0 10 60 20 0 0 0\ncpu0 1 0 1 6 2 0 0 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	total, idle, ok := ReadCPUJiffies(stat)
	if !ok {
		t.Fatal("the aggregate cpu line did not parse")
	}
	if total != 100 {
		t.Errorf("total = %v, want 100", total)
	}
	if idle != 80 {
		t.Errorf("idle = %v, want 80 (60 idle + 20 iowait)", idle)
	}
}

// MemAvailable, not MemFree. Linux spends every spare page on cache, so MemFree on a healthy Machine
// is near zero and a gauge built on it is a permanent alarm.
func TestMemoryUsesAvailableAndNotFree(t *testing.T) {
	meminfo := filepath.Join(t.TempDir(), "meminfo")
	// A Machine with almost no MemFree and plenty of MemAvailable: the ordinary healthy state.
	body := "MemTotal:       1000 kB\nMemFree:           8 kB\nMemAvailable:    750 kB\nBuffers: 12 kB\n"
	if err := os.WriteFile(meminfo, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	used, ok := ReadMemoryUsed(meminfo)
	if !ok {
		t.Fatal("meminfo did not parse")
	}
	if used < 0.24 || used > 0.26 {
		t.Fatalf("used = %v; MemAvailable says 25%%, MemFree would say 99.2%%", used)
	}
}

// waitFor polls until `want` holds. Polling because a process writing to a tmux pane is not
// synchronous with the call that started it.
func waitFor(t *testing.T, want func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if want() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("never became true: %s", what)
}
