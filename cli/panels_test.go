// panels_test.go — the streamer is faked with httptest; no test reaches a real one, and no test
// needs a Fleet (there is none in this environment). Most assertions read the RENDERED TABLE
// rather than the structs behind it, because the table is the contract with the operator: a
// signal that stops being its own column, or an `unknown` that starts looking like `ok`, is a
// silent regression that a struct-level assertion would not notice.
package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/medmahmoudi26/kontra/cli/internal/cliio"
)

// The Fleet this suite renders, deliberately UNSORTED and deliberately mixed:
//
//	kf-bust-01   an ad-hoc session an operator made by hand, no Artifact placed → actor ''
//	kf-crawl-01  one healthy Terminal, and one whose session was killed under it
//	kf-crawl-02  one Terminal the streamer cannot see AT ALL (every signal unknown), and one that
//	             is perfectly reachable while its host fails 81 of 82 loads — the round-3 shape
//	             that ADR 0020's four-signal rule exists for
const panelsFixture = `{"terminals":[
  {"id":"fleet:kf-crawl-02/kontra-webcrawl/handler","machine":"kf-crawl-02","host":"10.0.0.2","publicIp":"203.0.113.2",
   "tag":"crawl","fleet":"webcrawl","actor":"webcrawl","version":"0.1.0","window":"handler",
   "health":{"reachable":"ok","session":"present","poller":"live","loads":"failing",
             "detail":"nscheck@0.1.0: 81 of its 82 resource loads failed in 5m0s — the process is alive"},
   "lastSnapshotAt":%d},
  {"id":"fleet:kf-crawl-02/kontra-webcrawl/actor","machine":"kf-crawl-02","host":"10.0.0.2","publicIp":"203.0.113.2",
   "tag":"crawl","fleet":"webcrawl","actor":"webcrawl","version":"0.1.0","window":"actor",
   "health":{"reachable":"unknown","session":"unknown","poller":"unknown","loads":"unknown",
             "detail":"ssh: connect to host 10.0.0.2 port 22: Connection timed out"}},
  {"id":"fleet:kf-crawl-01/kontra-webcrawl/actor","machine":"kf-crawl-01","host":"10.0.0.1","publicIp":"203.0.113.1",
   "tag":"crawl","fleet":"webcrawl","actor":"webcrawl","version":"0.1.0","window":"actor",
   "health":{"reachable":"ok","session":"present","poller":"live","loads":"ok"},
   "lastSnapshotAt":%d},
  {"id":"fleet:kf-crawl-01/kontra-webcrawl/handler","machine":"kf-crawl-01","host":"10.0.0.1","publicIp":"203.0.113.1",
   "tag":"crawl","fleet":"webcrawl","actor":"webcrawl","version":"0.1.0","window":"handler",
   "health":{"reachable":"ok","session":"absent","poller":"none","loads":"ok",
             "detail":"no session kontra-webcrawl on kf-crawl-01 — converge it from the Dashboard"}},
  {"id":"fleet:kf-bust-01/ad-hoc/shell","machine":"kf-bust-01","host":"10.0.0.9","publicIp":"203.0.113.9",
   "tag":"bust","fleet":"","actor":"","version":"","window":"shell",
   "health":{"reachable":"ok","session":"no-tmux","poller":"unknown","loads":"unknown",
             "detail":"tmux is not installed on kf-bust-01"}}
]}`

func panelsBody() string {
	ago := time.Now().Add(-2 * time.Second).UnixMilli()
	return fmt.Sprintf(panelsFixture, ago, ago)
}

// panelStreamer fakes the one route `kontra panels list` reads and records what the CLI sent —
// the bearer especially: a command that quietly asked unauthenticated would still pass every
// rendering assertion below.
type panelStreamer struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []string
	auth string
}

func newPanelStreamer(t *testing.T, respond http.HandlerFunc) *panelStreamer {
	t.Helper()
	ps := &panelStreamer{}
	ps.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ps.mu.Lock()
		ps.reqs = append(ps.reqs, r.RequestURI)
		ps.auth = r.Header.Get("Authorization")
		ps.mu.Unlock()
		respond(w, r)
	}))
	t.Cleanup(ps.Close)
	return ps
}

func (ps *panelStreamer) sent() ([]string, string) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return append([]string(nil), ps.reqs...), ps.auth
}

func okTerminals(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, body)
	}
}

// listPanels runs `kontra panels list` against a fake streamer with stdout captured. The token is
// set explicitly in every test: panelToken() otherwise walks up to the checkout's .env and then
// asks Docker, so an ambient value would decide what these tests prove.
func listPanels(t *testing.T, base string, args ...string) (string, error) {
	t.Helper()
	t.Setenv("KONTRA_PANEL_TOKEN", "panel-tok")
	var buf bytes.Buffer
	defer swap[io.Writer](&cliio.Stdout, &buf)()
	defer swap[io.Writer](&cliio.Stderr, io.Discard)()
	err := cmdPanels(append([]string{"list", "--url", base}, args...))
	return buf.String(), err
}

// panelRows returns the rendered Terminal lines (detail lines and chrome excluded), each
// whitespace-split into cells: id, run, actor, the four signals, then the snapshot age.
func panelRows(out string) [][]string {
	var rows [][]string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "fleet:") {
			rows = append(rows, strings.Fields(line))
		}
	}
	return rows
}

func panelRow(t *testing.T, out, id string) []string {
	t.Helper()
	for _, r := range panelRows(out) {
		if r[0] == id {
			return r
		}
	}
	t.Fatalf("no row for %s in:\n%s", id, out)
	return nil
}

// --- the table ---

func TestPanelsListRendersFourIndependentSignals(t *testing.T) {
	srv := newPanelStreamer(t, okTerminals(panelsBody()))
	out, err := listPanels(t, srv.URL)
	if err != nil {
		t.Fatalf("panels list: %v\n%s", err, out)
	}

	reqs, auth := srv.sent()
	if len(reqs) != 1 || reqs[0] != "/api/panels/terminals" {
		t.Errorf("wrong requests: %v", reqs)
	}
	if auth != "Bearer panel-tok" {
		t.Errorf("KONTRA_PANEL_TOKEN must be sent as the bearer, got %q", auth)
	}

	// Four columns, one per signal. Collapsing them into a single light is the failure mode ADR
	// 0020 was written against, so the header is asserted literally.
	for _, col := range []string{"REACHABLE", "SESSION", "POLLER", "LOADS"} {
		if !strings.Contains(out, col) {
			t.Errorf("column %s missing — the four signals must never be collapsed:\n%s", col, out)
		}
	}
	healthy := panelRow(t, out, "fleet:kf-crawl-01/kontra-webcrawl/actor")
	if got := healthy[3:7]; got[0] != "ok" || got[1] != "present" || got[2] != "live" || got[3] != "ok" {
		t.Errorf("healthy signals rendered %v, want [ok present live ok]", got)
	}
	// A perfectly reachable Terminal whose host is failing loads: three signals ok, one FAILING.
	// If this row ever reads as healthy, the round-3 incident is invisible again.
	loadsBad := panelRow(t, out, "fleet:kf-crawl-02/kontra-webcrawl/handler")
	if got := loadsBad[3:7]; got[0] != "ok" || got[1] != "present" || got[2] != "live" || got[3] != "FAILING" {
		t.Errorf("failing loads under three ok signals rendered %v, want [ok present live FAILING]", got)
	}
	// The placement, and its absence: an ad-hoc session on an unplaced Machine is a real Terminal.
	if got := panelRow(t, out, "fleet:kf-crawl-01/kontra-webcrawl/actor")[2]; got != "webcrawl@0.1.0" {
		t.Errorf("actor cell = %q, want webcrawl@0.1.0", got)
	}
	adhoc := panelRow(t, out, "fleet:kf-bust-01/ad-hoc/shell")
	if adhoc[1] != "-" || adhoc[2] != "-" {
		t.Errorf("an unplaced Machine's Terminal must show -, not an empty cell: %v", adhoc)
	}
	if adhoc[4] != "NO-TMUX" {
		t.Errorf("session=no-tmux rendered %q, want NO-TMUX", adhoc[4])
	}
	// A snapshot age, and "-" for a Terminal that has never been painted — not "0s ago".
	if got := healthy[7]; got != "2s" {
		t.Errorf("snapshot age = %q, want \"2s ago\"", strings.Join(healthy[7:], " "))
	}
	if got := panelRow(t, out, "fleet:kf-crawl-02/kontra-webcrawl/actor")[7]; got != "-" {
		t.Errorf("a never-snapshotted Terminal must render -, got %q", got)
	}
}

func TestPanelsListUnknownIsNotHealthy(t *testing.T) {
	// The whole point of the three-valued signals: "the streamer could not tell" must not look
	// like "fine" — in colour OR piped, which is how a list command is usually read.
	srv := newPanelStreamer(t, okTerminals(panelsBody()))
	out, err := listPanels(t, srv.URL)
	if err != nil {
		t.Fatalf("panels list: %v\n%s", err, out)
	}
	blind := panelRow(t, out, "fleet:kf-crawl-02/kontra-webcrawl/actor")
	for i, cell := range blind[3:7] {
		if cell != "?" {
			t.Errorf("unknown signal %d rendered %q, want ?", i, cell)
		}
		for _, healthy := range []string{"ok", "present", "live"} {
			if cell == healthy {
				t.Errorf("unknown rendered as the healthy word %q", healthy)
			}
		}
		if strings.TrimSpace(cell) == "" {
			t.Error("unknown rendered as an empty cell — an operator reads a blank as nothing wrong")
		}
	}
	// …and unknown is distinct from failing too: three states, three renderings.
	if signalText("unknown") == signalText("fail") || signalText("unknown") == signalText("ok") {
		t.Fatal("unknown must render distinctly from both ok and fail")
	}
	// The summary must not fold unknowns into the failure counts.
	if !strings.Contains(out, "UNKNOWN") {
		t.Errorf("the summary must say how many signals are UNKNOWN:\n%s", out)
	}
}

func TestPanelsListPrintsTheStreamersDetailSentence(t *testing.T) {
	// The detail is the only thing that says WHY, and each failing signal has its own. Summarising
	// it away would leave an operator with a red column and nowhere to go.
	srv := newPanelStreamer(t, okTerminals(panelsBody()))
	out, err := listPanels(t, srv.URL)
	if err != nil {
		t.Fatalf("panels list: %v\n%s", err, out)
	}
	for _, want := range []string{
		// The Warden's own sentence, verbatim from `sickworker.go`. It arrives through
		// `panels/warden.ts`, prefixed with the Worker it is about — a Machine may hold several.
		"81 of its 82 resource loads failed",
		"Connection timed out",
		"no session kontra-webcrawl on kf-crawl-01",
		"tmux is not installed on kf-bust-01",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("detail sentence %q was dropped:\n%s", want, out)
		}
	}
	// It has to land under ITS row — a detail attributed to the wrong Terminal is worse than none.
	lines := strings.Split(out, "\n")
	for i, line := range lines {
		if strings.Contains(line, "fleet:kf-bust-01/ad-hoc/shell") {
			if i+1 >= len(lines) || !strings.Contains(lines[i+1], "tmux is not installed") {
				t.Errorf("the detail must follow its own row, got:\n%s", strings.Join(lines[i:min(i+3, len(lines))], "\n"))
			}
		}
	}
}

// cellOffsets returns the character offset of every cell on a rendered line; cells are separated
// by two or more spaces, which is what tabwriter pads with here.
func cellOffsets(line string) []int {
	var offs []int
	inCell := false
	for i := 0; i < len(line); i++ {
		switch {
		case line[i] != ' ' && !inCell:
			offs, inCell = append(offs, i), true
		case line[i] == ' ' && inCell && i+1 < len(line) && line[i+1] == ' ':
			inCell = false
		}
	}
	return offs
}

func TestPanelsTableStaysAlignedThroughTheDetailLines(t *testing.T) {
	// Regression, found by looking at the output rather than at an assertion: the `↳` lines carry
	// no tab, and text/tabwriter ENDS A COLUMN BLOCK at the first tab-free line. Written into the
	// writer, they re-aligned each group of rows between two details independently, and the
	// columns stepped left and right down the page. The table is therefore laid out on its own and
	// the details are interleaved afterwards — which only a positional assertion can pin.
	srv := newPanelStreamer(t, okTerminals(panelsBody()))
	out, err := listPanels(t, srv.URL)
	if err != nil {
		t.Fatalf("panels list: %v\n%s", err, out)
	}
	var header []int
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "REACHABLE") {
			header = cellOffsets(line)
			break
		}
	}
	if len(header) != 8 {
		t.Fatalf("expected 8 header cells, got %d in:\n%s", len(header), out)
	}
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "fleet:") {
			continue
		}
		got := cellOffsets(line)
		if len(got) != len(header) {
			t.Errorf("row has %d cells, header has %d:\n%s", len(got), len(header), line)
			continue
		}
		for i := range header {
			if got[i] != header[i] {
				t.Errorf("column %d starts at %d in the header and %d in a row — the table is not aligned:\n%s",
					i, header[i], got[i], out)
				break
			}
		}
	}
}

func TestPanelsNeverReplaysEscapeSequencesFromTheStreamer(t *testing.T) {
	// Every string in a row is remote text: run/actor come from Pulumi stack outputs and the
	// detail is built from a Machine's stderr. Printing it verbatim lets a crafted value repaint
	// the operator's terminal (or hide a row), so control characters are flattened.
	srv := newPanelStreamer(t, okTerminals(`{"terminals":[
	  {"id":"fleet:kf-crawl-01/kontra-webcrawl/actor","machine":"kf-crawl-01","fleet":"\u001b[2Jwebcrawl",
	   "actor":"webcrawl","version":"0.1.0","window":"actor",
	   "health":{"reachable":"ok","session":"present","poller":"live","loads":"failing",
	             "detail":"vm says: 81 of 82 failed\nand\u001b[31m the ratio is still climbing"}}]}`))
	out, err := listPanels(t, srv.URL)
	if err != nil {
		t.Fatalf("panels list: %v\n%s", err, out)
	}
	if strings.Contains(out, "\x1b") {
		t.Errorf("an escape sequence from the streamer reached the terminal:\n%q", out)
	}
	// Flattened, not dropped: the words still have to arrive, on both lines of the detail.
	for _, want := range []string{"webcrawl", "81 of 82 failed", "the ratio is still climbing"} {
		if !strings.Contains(out, want) {
			t.Errorf("stripping control characters must not eat the text — %q missing:\n%s", want, out)
		}
	}
	if n := strings.Count(out, "↳"); n != 2 {
		t.Errorf("a two-line detail must render as two indented lines, got %d:\n%s", n, out)
	}
}

func TestPanelsListSortsByMachineThenWindow(t *testing.T) {
	// Deterministic order is not cosmetic: this output gets diffed between invocations, and map
	// iteration order would show phantom moves on an unchanged Fleet.
	srv := newPanelStreamer(t, okTerminals(panelsBody()))
	out, err := listPanels(t, srv.URL)
	if err != nil {
		t.Fatalf("panels list: %v\n%s", err, out)
	}
	var got []string
	for _, r := range panelRows(out) {
		got = append(got, r[0])
	}
	want := []string{
		"fleet:kf-bust-01/ad-hoc/shell",
		"fleet:kf-crawl-01/kontra-webcrawl/actor",
		"fleet:kf-crawl-01/kontra-webcrawl/handler",
		"fleet:kf-crawl-02/kontra-webcrawl/actor",
		"fleet:kf-crawl-02/kontra-webcrawl/handler",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("order:\n got  %v\n want %v", got, want)
	}
	// The fixture is shuffled on purpose — if it were already sorted this test would prove nothing.
	if strings.Index(panelsFixture, "kf-bust-01") < strings.Index(panelsFixture, "kf-crawl-02") {
		t.Fatal("fixture is no longer shuffled: the sort assertion above is vacuous")
	}
}

func TestPanelsListEmptyIsAnAnswerNotAFailure(t *testing.T) {
	// A 200 with no Terminals is the streamer having LOOKED. It is the one case where "none" is
	// honest, and it points at the command that changes it.
	srv := newPanelStreamer(t, okTerminals(`{"terminals":[]}`))
	out, err := listPanels(t, srv.URL)
	if err != nil {
		t.Fatalf("an empty Fleet is not an error: %v", err)
	}
	if !strings.Contains(out, "none") || !strings.Contains(out, "kontra fleet up") {
		t.Errorf("an empty list must say so and name what changes it:\n%s", out)
	}
}

// --- failing to look is never an empty list ---

// panelsFailure runs the command expecting an error, and asserts the two things every failure
// path here owes: nothing that reads as an inventory on stdout, and never the word an empty Fleet
// would print. `cmdWorkers` refuses to print "(none)" on a Temporal dial failure for the same
// reason — "no Terminals" and "cannot see the Terminals" send an operator to opposite places.
func panelsFailure(t *testing.T, base string) string {
	t.Helper()
	out, err := listPanels(t, base)
	if err == nil {
		t.Fatalf("want an error, got output:\n%s", out)
	}
	if strings.Contains(out, "TERMINAL") || strings.Contains(out, "none") {
		t.Errorf("a failed lookup printed an inventory:\n%s", out)
	}
	if strings.Contains(err.Error(), "none") {
		t.Errorf("a failed lookup must not read as an empty Fleet: %v", err)
	}
	return err.Error()
}

func TestPanelsUnreachableStreamerBlamesTheChildProcess(t *testing.T) {
	srv := newPanelStreamer(t, okTerminals(panelsBody()))
	base := srv.URL
	srv.Close() // nothing is listening now: a connection refused on a loopback port

	msg := panelsFailure(t, base)
	for _, want := range []string{"panels streamer", "connection refused", "orchestrator-infra", "panels/health"} {
		if !strings.Contains(msg, want) {
			t.Errorf("transport error must mention %q, got: %s", want, msg)
		}
	}
	// api.go's transport message names the ORCHESTRATOR and points at `kontra infra status`, which
	// says nothing about a forked child on another port. This pins the trim in panelTransportCause:
	// if that wording changes, this fails rather than the CLI quietly misattributing the fault.
	if strings.Contains(msg, "is it up?") || strings.Contains(msg, "cannot reach the orchestrator") {
		t.Errorf("the orchestrator's transport prose leaked into a panels error: %s", msg)
	}
}

func TestPanels401BlamesTheLocalToken(t *testing.T) {
	srv := newPanelStreamer(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
	msg := panelsFailure(t, srv.URL)
	for _, want := range []string{"401", "KONTRA_PANEL_TOKEN", "does not\n  match", ".env"} {
		if !strings.Contains(msg, want) {
			t.Errorf("401 must name the LOCAL token mismatch (%q), got: %s", want, msg)
		}
	}
}

func TestPanels503BlamesTheStreamersOwnEnvironment(t *testing.T) {
	// 503 is a DEPLOYMENT fact, not a local one: checkBearer fails closed when the streamer has no
	// KONTRA_PANEL_TOKEN, and saying so saves an hour spent re-checking the workstation.
	srv := newPanelStreamer(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "panels disabled", http.StatusServiceUnavailable)
	})
	msg := panelsFailure(t, srv.URL)
	for _, want := range []string{"503", "KONTRA_PANEL_TOKEN", "ITS environment", "fails closed", "orchestrator-infra"} {
		if !strings.Contains(msg, want) {
			t.Errorf("503 must point at the STREAMER's env (%q), got: %s", want, msg)
		}
	}
}

func TestPanels404SaysTheRouteIsMissing(t *testing.T) {
	srv := newPanelStreamer(t, func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	})
	msg := panelsFailure(t, srv.URL)
	for _, want := range []string{"404", "/api/panels/terminals"} {
		if !strings.Contains(msg, want) {
			t.Errorf("404 must name the missing route (%q), got: %s", want, msg)
		}
	}
}

func TestPanelsMalformedJSONIsNotAnEmptyFleet(t *testing.T) {
	// The realistic case: something OTHER than the streamer is on that port (a proxy's login page,
	// the orchestrator API). Decoding nothing must not present as seeing nothing.
	for name, body := range map[string]string{
		"not json":    "<html><body>login</body></html>",
		"wrong shape": `{"terminals":"soon"}`,
		"truncated":   `{"terminals":[{"id":"fleet:kf-crawl-01/s/actor"`,
	} {
		t.Run(name, func(t *testing.T) {
			srv := newPanelStreamer(t, okTerminals(body))
			msg := panelsFailure(t, srv.URL)
			if !strings.Contains(msg, "not a terminals list") {
				t.Errorf("a bad body must be reported as a bad body, got: %s", msg)
			}
			if strings.Contains(msg, "cannot reach") {
				t.Errorf("a body problem must not be reported as unreachable: %s", msg)
			}
			if !strings.Contains(msg, "KONTRA_PANEL_PORT") {
				t.Errorf("say what to check (the port), got: %s", msg)
			}
		})
	}
}

func TestPanelsWithoutATokenMakesNoRequest(t *testing.T) {
	srv := newPanelStreamer(t, okTerminals(panelsBody()))
	t.Setenv("KONTRA_PANEL_TOKEN", "")
	// panelToken() DISCOVERS the token from the checkout's .env and then from the running stack, so
	// this has to run somewhere without either — otherwise the repo's own .env (or a live
	// orchestrator) satisfies the very thing the test asserts is absent. Same trap explore_test.go
	// documents; t.Setenv only sets, it does not isolate.
	t.Chdir(t.TempDir())
	t.Setenv("PATH", "")

	var buf bytes.Buffer
	defer swap[io.Writer](&cliio.Stdout, &buf)()
	defer swap[io.Writer](&cliio.Stderr, io.Discard)()
	err := cmdPanels([]string{"list", "--url", srv.URL})
	if err == nil {
		t.Fatal("a missing token must be an error, not a silent unauthenticated attempt")
	}
	for _, want := range []string{"KONTRA_PANEL_TOKEN", "KONTRA_STATE_TOKEN"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must name %s (and why it is not the state token), got: %v", want, err)
		}
	}
	if reqs, _ := srv.sent(); len(reqs) != 0 {
		t.Errorf("nothing may be sent without a token, got %v", reqs)
	}
}

// --- read-only ---

func TestPanelsHasNoWriteSubcommands(t *testing.T) {
	// `attach` is the first thing anyone reaches for, and its absence is a decision: no code path
	// may write to a session's channel, because `tmux attach -r` was measured NOT to be a boundary.
	for _, sub := range []string{"attach", "kill", "converge"} {
		err := cmdPanels([]string{sub, "fleet:kf-crawl-01/kontra-webcrawl/actor"})
		if err == nil {
			t.Fatalf("`kontra panels %s` must not exist", sub)
		}
		for _, want := range []string{"READ-ONLY", "list"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("refusing %q should say %q, got: %v", sub, want, err)
			}
		}
	}
	if err := cmdPanels(nil); err == nil || !strings.Contains(err.Error(), "usage:") {
		t.Errorf("bare `kontra panels` should print usage, got: %v", err)
	}
}

// --- units ---

func TestHealthWordsCoverTheContractAndDefaultToUnknown(t *testing.T) {
	// Every value CONTRACT.md defines, with the severity it must carry. An UNRECOGNISED value —
	// a newer streamer's vocabulary, or a field it omitted — has to land on unknown: rendering it
	// as healthy is how a signal silently stops working.
	for value, want := range map[string]severity{
		"ok": sevOK, "present": sevOK, "live": sevOK,
		"fail": sevBad, "absent": sevBad, "no-tmux": sevBad, "none": sevBad, "failing": sevBad,
		"unknown": sevUnknown, "": sevUnknown, "degraded-ish": sevUnknown,
	} {
		if got := severityOf(value); got != want {
			t.Errorf("severityOf(%q) = %v, want %v", value, got, want)
		}
	}
	// An unfamiliar value is shown verbatim (so it can be reported) but marked, not blessed.
	if got := signalText("degraded-ish"); got != "?degraded-ish" {
		t.Errorf("signalText(unfamiliar) = %q, want ?degraded-ish", got)
	}
	if got := signalText(""); got != "?" {
		t.Errorf("signalText(\"\") = %q, want ?", got)
	}
	// Failing values are SHOUTED and healthy ones are not, so severity survives a pipe.
	for _, bad := range []string{"fail", "absent", "no-tmux", "none", "failing"} {
		if got := signalText(bad); got != strings.ToUpper(got) {
			t.Errorf("signalText(%q) = %q — a failing signal must be uppercase", bad, got)
		}
	}
}

func TestPanelsSummaryCountsUnknownsApartFromFailures(t *testing.T) {
	ts := []terminal{
		{Health: terminalHealth{Reachable: "fail", Session: "unknown", Poller: "unknown", Loads: "unknown"}},
		{Health: terminalHealth{Reachable: "ok", Session: "absent", Poller: "none", Loads: "ok"}},
	}
	got := panelsSummary(ts)
	for _, want := range []string{"1 unreachable", "1 without a session", "1 with no live poller", "3 signal(s) UNKNOWN"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary missing %q: %s", want, got)
		}
	}
	// Adding unknowns to the failures would report 7 problems where there are 3 and 3 blind spots.
	if strings.Contains(got, "4 unreachable") || strings.Contains(got, "failing loads") {
		t.Errorf("unknowns were folded into the failure counts: %s", got)
	}
	if got := panelsSummary([]terminal{{Health: terminalHealth{Reachable: "ok", Session: "present", Poller: "live", Loads: "ok"}}}); got != "all 4 signals ok" {
		t.Errorf("a clean Fleet summary = %q", got)
	}
}

func TestPanelURLPrefersTheExplicitURLThenThePort(t *testing.T) {
	// KONTRA_PANEL_PORT is the knob the streamer itself reads; honouring it here keeps one fact in
	// one place for the common case of "same host, moved port".
	t.Setenv("KONTRA_PANEL_URL", "")
	t.Setenv("KONTRA_PANEL_PORT", "")
	if got := panelURL(); got != "http://localhost:8090" {
		t.Errorf("default = %q", got)
	}
	t.Setenv("KONTRA_PANEL_PORT", "9999")
	if got := panelURL(); got != "http://localhost:9999" {
		t.Errorf("with KONTRA_PANEL_PORT = %q", got)
	}
	t.Setenv("KONTRA_PANEL_URL", "https://panels.example")
	if got := panelURL(); got != "https://panels.example" {
		t.Errorf("KONTRA_PANEL_URL must win: %q", got)
	}
}
