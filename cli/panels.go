// panels.go — `kontra panels list`: every Terminal the streamer can see, with their four health
// signals.
//
// A Terminal is a READ-ONLY view of one window of one tmux session, id
// `<mode>:<node>/<session>/<window>` (ADR 0020). The first segment is the EXECUTION MODE — the
// place that Worker runs, and therefore what a crashed tmux server there costs:
//
//	fleet:<machine>/…    a Machine, over SSH; its panes hold `journalctl -fu`
//	docker:<container>/… a worker container, over `docker exec`
//	local:<host>/…       this host's tmux server; its panes hold the REAL actor and handler
//
// This command is that inventory from a terminal — the same list the Dashboard paints in a
// browser — and nothing else: there is no `panels attach` and no `panels kill`, because the
// feature's whole boundary is that NO code path can put bytes into a session's channel
// (`tmux attach -r` was measured not to be a boundary: `run-shell` and `send-keys` both executed
// through a read-only client).
//
// The Terminals live on the STREAMER, a forked child of the orchestrator-infra container with
// its own port — not on the orchestrator API. That is why this is the one command whose base
// URL is KONTRA_PANEL_URL and whose bearer is KONTRA_PANEL_TOKEN, and why a failure here says
// nothing about `kontra infra status`.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

const (
	defaultPanelPort = "8090" // KONTRA_PANEL_PORT's default (.scratch/tmux-dashboard/CONTRACT.md)
	panelsListPath   = "/api/panels/terminals"
)

// panelURL is the streamer's base URL. KONTRA_PANEL_PORT is the knob the streamer itself reads,
// so an operator who moved the port has already set it — making them ALSO set a URL would be a
// second source of truth for one fact. KONTRA_PANEL_URL wins, for a streamer on another host.
func panelURL() string {
	if u := os.Getenv("KONTRA_PANEL_URL"); u != "" {
		return u
	}
	return "http://localhost:" + envOr("KONTRA_PANEL_PORT", defaultPanelPort)
}

// panelToken resolves the bearer the panel routes accept — KONTRA_PANEL_TOKEN and only that.
//
// Deliberately NOT stateToken(): KONTRA_STATE_TOKEN also authorises
// POST /api/infra/stacks/:fqn/:op, i.e. spending money, and ADR 0020 keeps the credential that
// reaches Terminals (and lives in a browser) narrower than that on purpose. The discovery ladder
// is stateToken's: the environment, then the checkout's .env, then whatever the running stack was
// started with. That last step inspects orchestrator-api's env while the streamer runs in
// orchestrator-infra, so it only finds the token when compose passes it to both — a miss there is
// a miss, never a wrong value.
func panelToken() string {
	if t := os.Getenv("KONTRA_PANEL_TOKEN"); t != "" {
		return t
	}
	if root, ok := findUp(".env"); ok {
		if t := dotEnv(filepath.Join(root, ".env"))["KONTRA_PANEL_TOKEN"]; t != "" {
			return t
		}
	}
	return envFromRunningStack("KONTRA_PANEL_TOKEN")
}

// errNoPanelToken fails BEFORE the request: the routes fail closed, so asking unauthenticated
// only turns a missing local secret into a 401 that reads like the wrong one.
var errNoPanelToken = errors.New(
	"no panel token — set KONTRA_PANEL_TOKEN to the same value the streamer runs with:\n" +
		"    export KONTRA_PANEL_TOKEN='…'   # it is in the checkout's .env\n" +
		"  the panel routes mint terminal streams over the Fleet's SSH key, so they are token-gated\n" +
		"  and serve nothing without one. This is NOT KONTRA_STATE_TOKEN: that token can also spend money.")

// --- wire types (.scratch/tmux-dashboard/CONTRACT.md's `Terminal`, field for field) ---

// terminalHealth is the four INDEPENDENT signals. They are separate fields here, separate
// columns in the table, and separate counters in the summary, because collapsing them is the
// bug this feature exists to fix: the round-3 incident had 81 of 82 resource loads failing on a
// Machine whose run reported `completed`, and `reachable`+`session`+`poller` all looked fine.
type terminalHealth struct {
	Reachable string `json:"reachable"` // ok | fail | unknown
	Session   string `json:"session"`   // present | absent | no-tmux | unknown
	Poller    string `json:"poller"`    // live | none | unknown  ('none' = registered, nothing polling)
	// ok | failing | unknown. From the **Warden's** own verdict where a Machine reports one
	// (sickworker.go), and from VictoriaMetrics otherwise. Every one of the Warden's six ways of
	// not knowing arrives here as `unknown` and never as `ok` — see panels/warden.ts.
	Loads  string `json:"loads"`
	Detail string `json:"detail,omitempty"`
}

type terminal struct {
	ID string `json:"id"` // <mode>:<node>/<session>/<window>
	// Mode is where the Worker runs: fleet | docker | local. ADDED by slice 6 and decoded
	// tolerantly ON PURPOSE — an older streamer sends no `mode` at all, and its ids still carry
	// one, so `modeName` falls back to the prefix rather than rendering a blank column. A field
	// that must be present to be read would have made a newer CLI unable to talk to a running
	// streamer, which is the opposite of what an additive wire is for.
	Mode     string `json:"mode,omitempty"`
	Machine  string `json:"machine"`
	Host     string `json:"host"`
	PublicIP string `json:"publicIp"`
	// WHICH FLEET — the stack name, `<actor>-<version>`, DERIVED from the Artifact it places.
	//
	// A DEAD COLUMN UNTIL 2026-08-30, and the two ways this struct was wrong are worth keeping:
	//
	//   * this key was `campaign` long after `backend/src/panels/types.ts` renamed it to `fleet`,
	//     so it decoded nothing and `dashIfEmpty` printed `-` for every Terminal on every Fleet —
	//     a blank that reads as "this Machine has no Fleet" rather than as a wire mismatch;
	//   * a `Role string `json:"role"`` sat below it. The streamer has never sent `role` (its key
	//     is `tag`), and no Go line ever read the field. It is gone.
	//
	// Nothing failed on either side, because each side's tests were built from that side's own
	// declaration: the TypeScript tests assert the TypeScript spelling, the Go tests built their
	// fixtures from this struct. ADR 0035 rule two — a contract with two writers gets a corpus, not
	// two independent string literals. `conformance/terminal.json` is that corpus and
	// `panels_conformance_test.go` is this side's arm of it.
	Fleet          string         `json:"fleet"`
	Actor          string         `json:"actor"`   // '' when the stack carries no placement
	Version        string         `json:"version"` // ''      "        "
	Window         string         `json:"window"`  // actor | handler
	Health         terminalHealth `json:"health"`
	LastSnapshotAt int64          `json:"lastSnapshotAt,omitempty"` // epoch ms
}

type terminalsResponse struct {
	Terminals []terminal `json:"terminals"`
}

// --- the command ---

func cmdPanels(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: kontra panels list [--url <streamer>]")
	}
	switch args[0] {
	case "list":
		return panelsList(args[1:])
	default:
		// Said out loud rather than left as "unknown subcommand": `attach` is the first thing a
		// person reaches for here, and its absence is a decision, not a gap.
		return fmt.Errorf("unknown `kontra panels %s` — `list` is the only subcommand.\n"+
			"  The Dashboard is READ-ONLY (ADR 0020): no `attach`, no `kill`, no input path at all,\n"+
			"  because no code path may write to a session's channel. Watch a Terminal in the browser,\n"+
			"  or `ssh` to the Machine yourself if you need to type.", args[0])
	}
}

func panelsList(args []string) error {
	fs := flag.NewFlagSet("panels list", flag.ContinueOnError)
	base := fs.String("url", panelURL(), "panels streamer base URL (KONTRA_PANEL_URL)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q — usage: kontra panels list [--url <streamer>]", fs.Arg(0))
	}
	token := panelToken()
	if token == "" {
		return errNoPanelToken
	}
	ts, err := fetchTerminals(*base, token)
	if err != nil {
		return err
	}
	sortTerminals(ts)
	return renderTerminals(stdout, *base, ts)
}

func fetchTerminals(base, token string) ([]terminal, error) {
	var out terminalsResponse
	if err := newAuthAPI(base, token).getJSON(panelsListPath, &out); err != nil {
		return nil, panelError(base, err)
	}
	return out.Terminals, nil
}

// panelError names WHOSE fault a failed lookup is. Same rule `cmdWorkers` follows when a Temporal
// dial fails: a lookup that FAILED must never reach the renderer, because "no Terminals" and
// "cannot see the Terminals" send an operator to opposite places — one to `fleet up`, the other to
// a token or a dead child process.
func panelError(base string, err error) error {
	var he *httpError
	if errors.As(err, &he) {
		switch he.status {
		case 401:
			return fmt.Errorf("the panels streamer rejected the token (401): the KONTRA_PANEL_TOKEN used here does not\n" +
				"  match the value the streamer runs with. Compare it with the checkout's .env (or\n" +
				"  `docker compose exec orchestrator-infra printenv KONTRA_PANEL_TOKEN`) — this is a LOCAL mismatch,\n" +
				"  not an empty Fleet.")
		case 503:
			return fmt.Errorf("the panels streamer has panels DISABLED (503): KONTRA_PANEL_TOKEN is unset in ITS environment.\n" +
				"  It fails closed by design (backend/src/auth.ts) and serves NOTHING rather than exposing the\n" +
				"  Fleet's terminals unauthenticated. Set KONTRA_PANEL_TOKEN for the orchestrator-infra service and\n" +
				"  restart it (`kontra infra up`), then set the same value here.")
		case 404:
			return fmt.Errorf("no %s on the streamer at %s (404) — that process predates the Dashboard, or something\n"+
				"  else is listening on this port. Update the stack and restart orchestrator-infra.", panelsListPath, base)
		}
		return fmt.Errorf("GET %s on the panels streamer at %s failed: %w", panelsListPath, base, err)
	}
	// A 200 whose body is not the terminals list: usually another service on the port (a proxy's
	// login page, the orchestrator API), which is otherwise indistinguishable from "no Terminals".
	var syntax *json.SyntaxError
	var typ *json.UnmarshalTypeError
	if errors.As(err, &syntax) || errors.As(err, &typ) {
		return fmt.Errorf("the panels streamer at %s answered with a body that is not a terminals list: %w\n"+
			"  check that %s is the streamer's port (KONTRA_PANEL_PORT, default %s) and not another service's", base, err, base, defaultPanelPort)
	}
	// Transport. The client's own message names the ORCHESTRATOR — right for every other command,
	// wrong for this port: the streamer is a forked CHILD of orchestrator-infra (ADR 0020's PID
	// split), so the container can be healthy, `kontra infra status` green, and the child dead.
	return fmt.Errorf("cannot reach the panels streamer at %s: %s\n"+
		"  the streamer is a forked child of the orchestrator-infra container — the container can be up\n"+
		"  while the child is not, so `kontra infra status` does not answer this. Check both:\n"+
		"    docker compose logs orchestrator-infra | tail -50\n"+
		"    curl -s %s/api/panels/health", base, panelTransportCause(err), base)
}

// panelTransportCause keeps the syscall detail (`connection refused` vs `i/o timeout` vs a DNS
// failure — they mean different things) and drops the api.go prose that wraps it, which names the
// orchestrator. Trimming another file's message is a coupling, so panels_test.go asserts on both
// halves: if that wording changes, the test says so instead of the CLI quietly misattributing.
func panelTransportCause(err error) string {
	if _, after, ok := strings.Cut(err.Error(), "`kontra infra status`): "); ok {
		return after
	}
	return err.Error()
}

// --- execution mode ---

// modeOrder is the reading order of the three modes, and it is deliberately not alphabetical:
// fleet first because it owns Machines, local last because it is the one running on the box you
// are typing on. A mode this CLI does not know sorts after all three and renders verbatim.
var modeOrder = map[string]int{"fleet": 0, "docker": 1, "local": 2}

// modeNote is the one sentence that distinguishes the three, and it is a SAFETY sentence rather
// than a description (ADR 0020, finding 2): a stalled viewer was measured segfaulting a tmux
// server, which destroys every session on that socket. On the fleet the panes hold `journalctl`,
// so that costs the view. In the other two the panes can hold the Worker itself, so it costs a
// running Worker — same logic, different stakes, and an operator must be able to see which they
// are looking at.
var modeNote = map[string]string{
	"fleet":  "a Machine over SSH — panes hold `journalctl -fu`, so a crashed tmux server there costs the VIEW",
	"docker": "a worker container over `docker exec` — a session there holds the container's own processes",
	"local":  "this host's tmux server — panes hold the REAL actor and handler, so a crashed tmux server there costs a running WORKER",
}

// modeName is the execution mode of a Terminal: the `mode` field when the streamer sends one, else
// the id's own first segment. The fallback is what lets this CLI read a streamer that predates the
// field instead of printing a blank; an unrecognised value is shown verbatim and never blessed,
// exactly as `signalText` treats a health word it does not know.
func (t terminal) modeName() string {
	if t.Mode != "" {
		return safeText(t.Mode)
	}
	if i := strings.IndexByte(t.ID, ':'); i > 0 {
		return safeText(t.ID[:i])
	}
	return "?"
}

// panelModes is every mode present, in reading order. One list drives the section headers, the
// census line and the safety notes, so a mode cannot appear in the table and be missing from the
// summary.
func panelModes(ts []terminal) []string {
	count := map[string]int{}
	for _, t := range ts {
		count[t.modeName()]++
	}
	modes := make([]string, 0, len(count))
	for m := range count {
		modes = append(modes, m)
	}
	sort.Slice(modes, func(i, j int) bool {
		a, aok := modeOrder[modes[i]]
		b, bok := modeOrder[modes[j]]
		if aok != bok {
			return aok // a known mode before an unknown one
		}
		if aok && bok && a != b {
			return a < b
		}
		return modes[i] < modes[j]
	})
	return modes
}

// panelsModeCensus is the "where are these Workers running" line: a count per mode, plus the
// stakes of any mode whose panes can hold a Worker. Printed for one mode as well as three,
// because a wall that is ENTIRELY local is exactly the case where the asymmetry matters and there
// is no second section to compare it against.
func panelsModeCensus(out io.Writer, ts []terminal) {
	modes := panelModes(ts)
	count := map[string]int{}
	for _, t := range ts {
		count[t.modeName()]++
	}
	parts := make([]string, 0, len(modes))
	for _, m := range modes {
		parts = append(parts, fmt.Sprintf("%d %s", count[m], m))
	}
	fmt.Fprintf(out, "  modes: %s\n", strings.Join(parts, " · "))
	for _, m := range modes {
		if m == "fleet" {
			continue
		}
		if note, ok := modeNote[m]; ok {
			fmt.Fprintf(out, "  %s: %s.\n", m, note)
			continue
		}
		fmt.Fprintf(out, "  %s: a mode this CLI does not know — shown verbatim, not assumed safe.\n", m)
	}
}

// sortTerminals orders by mode, then Machine, then Window — how a wall is read, and what makes two
// invocations diff cleanly instead of showing phantom moves from map iteration order. Mode leads
// because the three are read as three groups (and because they carry different stakes); within one
// mode the order is exactly what it was before slice 6. The id breaks ties (a node can carry more
// than one session, which is the normal case in `local`).
func sortTerminals(ts []terminal) {
	sort.SliceStable(ts, func(i, j int) bool {
		a, b := ts[i], ts[j]
		am, bm := a.modeName(), b.modeName()
		switch {
		case am != bm:
			ao, aok := modeOrder[am]
			bo, bok := modeOrder[bm]
			if aok != bok {
				return aok
			}
			if aok && bok {
				return ao < bo
			}
			return am < bm
		case a.Machine != b.Machine:
			return a.Machine < b.Machine
		case a.Window != b.Window:
			return a.Window < b.Window
		default:
			return a.ID < b.ID
		}
	})
}

// --- health rendering ---

// severity is how alarming one signal's value is. UNKNOWN is its own value, never folded into
// either neighbour: "we could not tell" and "fine" rendering identically is the bug ADR 0020 and
// heartbeat.ts both exist to prevent, and it is the reason this type has three members.
type severity int

const (
	sevOK severity = iota
	sevBad
	sevUnknown
)

// healthWords is CONTRACT.md's four vocabularies in one table — they do not collide (`none` is
// only ever a poller, `no-tmux` only ever a session). Anything NOT listed, including "" from a
// field the streamer omitted or a value a newer streamer invented, is UNKNOWN by construction.
// That default is the load-bearing part: an unrecognised value must never render as healthy.
var healthWords = map[string]struct {
	text string
	sev  severity
}{
	"ok":      {"ok", sevOK},
	"present": {"present", sevOK},
	"live":    {"live", sevOK},
	"fail":    {"FAIL", sevBad},
	"absent":  {"ABSENT", sevBad},
	"no-tmux": {"NO-TMUX", sevBad},
	"none":    {"NONE", sevBad},
	"failing": {"FAILING", sevBad},
	"unknown": {"?", sevUnknown},
}

func severityOf(value string) severity {
	if w, ok := healthWords[value]; ok {
		return w.sev
	}
	return sevUnknown
}

// signalText renders one signal so that all three severities survive being piped: healthy is
// lowercase, failing is SHOUTED, unknown is `?`. Deliberately NOT coloured — text/tabwriter
// measures cells in bytes, so an ANSI escape inflates a column's width and breaks the alignment
// of every row after it, on exactly the TTY where colour would apply. (doctor.go gets away with
// colour because its coloured cell is the last one on the line; four signal columns are not.)
func signalText(value string) string {
	if w, ok := healthWords[value]; ok {
		return w.text
	}
	if value == "" {
		return "?"
	}
	return "?" + value // a value this CLI does not know: show it verbatim, do not bless it
}

// healthSignals is the four signals in reading order. One list drives the header, the row cells
// AND the summary counters, so a signal cannot be silently dropped from one of the three.
var healthSignals = []struct {
	column string
	value  func(terminalHealth) string
	failed string // how a failing one reads in the summary
}{
	{"REACHABLE", func(h terminalHealth) string { return h.Reachable }, "unreachable"},
	{"SESSION", func(h terminalHealth) string { return h.Session }, "without a session"},
	{"POLLER", func(h terminalHealth) string { return h.Poller }, "with no live poller"},
	{"LOADS", func(h terminalHealth) string { return h.Loads }, "failing loads"},
}

// --- the table ---

// renderTerminals prints the list. The shape is a design constraint, not a preference: four
// independent signal columns (never one light), the streamer's own sentence under any row that
// has one, and the statement that a Terminal is not a record — ADR 0020 requires that wherever
// Terminals surface.
func renderTerminals(out io.Writer, base string, ts []terminal) error {
	fmt.Fprintf(out, "%s\n", paint("1", fmt.Sprintf("Terminals — %d on %d machine(s) — %s", len(ts), machineCount(ts), base)))
	if len(ts) == 0 {
		// A 200 with an empty list is an ANSWER: the streamer looked and found nothing. Every way
		// of FAILING to look returns an error above, which is what keeps this line honest.
		fmt.Fprintln(out, "  none — the streamer sees no session in any mode it is watching.")
		fmt.Fprintln(out, "  `kontra fleet up --count N --role <r>` makes Machines; a session is converged on demand,")
		fmt.Fprintln(out, "  so a Machine deployed without --tmux still gets a Terminal with no re-deploy.")
		// The zero-fleet path, named here because this is where someone with no fleet lands: local
		// mode is the dev environment, not a fallback, and it needs nothing but tmux.
		fmt.Fprintln(out, "  With no fleet at all: `kontra serve --actor <dir> --mode local --tmux` puts a Worker in a")
		fmt.Fprintln(out, "  local tmux session, which the streamer discovers as `local:<host>/kontra-<actor>/…`.")
		return nil
	}

	// The table is laid out on its OWN and the detail lines are interleaved afterwards, because
	// text/tabwriter ends a column block at the first line without a tab: writing the `↳` lines
	// into the writer re-aligns each group of rows between two details independently, and the
	// columns visibly step left and right down the page. (Measured, not assumed — it is what the
	// first version of this function printed.)
	var tbl bytes.Buffer
	w := tabwriter.NewWriter(&tbl, 2, 8, 2, ' ', 0)
	// ROLE is deliberately not a column: a Machine is named kf-<role>-NN, so the id carries it.
	// CAMPAIGN is, because the session name in the id does not.
	header := []string{"  TERMINAL", "CAMPAIGN", "ACTOR"}
	for _, s := range healthSignals {
		header = append(header, s.column)
	}
	header = append(header, "SNAPSHOT")
	fmt.Fprintln(w, strings.Join(header, "\t"))
	for _, t := range ts {
		cells := []string{"  " + safeText(t.ID), dashIfEmpty(safeText(t.Fleet)), safeText(t.actorRef())}
		for _, s := range healthSignals {
			cells = append(cells, signalText(s.value(t.Health)))
		}
		cells = append(cells, t.snapshotText())
		fmt.Fprintln(w, strings.Join(cells, "\t"))
	}
	if err := w.Flush(); err != nil {
		return err
	}

	lines := strings.Split(strings.TrimRight(tbl.String(), "\n"), "\n")
	fmt.Fprintln(out, lines[0]) // the header
	rows := lines[1:]
	// Section headers only when there is more than one mode on the wall: on a single-mode wall they
	// would be a banner over every row, and the census line below already says which mode it is.
	// They are laid out AFTER the table for the same reason the detail lines are — a line with no tab
	// ends a tabwriter column block, and writing these into the writer would step the columns.
	sectioned := len(panelModes(ts)) > 1
	previous := ""
	for i, t := range ts {
		if i >= len(rows) {
			break // unreachable: safeText keeps every cell on one line, so rows and ts stay 1:1
		}
		if mode := t.modeName(); sectioned && mode != previous {
			previous = mode
			note := modeNote[mode]
			if note == "" {
				note = "a mode this CLI does not know — shown verbatim, not assumed safe"
			}
			fmt.Fprintf(out, "  ── %s — %s\n", mode, note)
		}
		fmt.Fprintln(out, rows[i])
		// The streamer writes one sentence per failing signal; it is the only thing in this output
		// that says WHY, so it is never summarised away.
		for _, line := range strings.Split(t.Health.Detail, "\n") {
			if line = strings.TrimSpace(safeText(line)); line != "" {
				fmt.Fprintf(out, "    ↳ %s\n", line)
			}
		}
	}

	fmt.Fprintf(out, "\n  %s\n", panelsSummary(ts))
	panelsModeCensus(out, ts)
	// ADR 0020's consequence, stated wherever Terminals surface: a wall that looks like a log
	// invites someone to treat a screen as evidence for what a Worker did.
	fmt.Fprintln(out, "  A Terminal is read-only and LOSSY — the journal on the Machine, the Manifest and the lake are the record.")
	return nil
}

// panelsSummary counts failing signals per signal, and unknowns SEPARATELY. An unknown is a
// statement about the streamer's own visibility (an SSH that timed out, a VictoriaMetrics that
// did not answer), not about the Fleet; adding the two together is how a blind spot starts
// reading as a healthy Fleet.
func panelsSummary(ts []terminal) string {
	failed := make([]int, len(healthSignals))
	unknown := 0
	for _, t := range ts {
		for i, s := range healthSignals {
			switch severityOf(s.value(t.Health)) {
			case sevBad:
				failed[i]++
			case sevUnknown:
				unknown++
			}
		}
	}
	parts := []string{}
	for i, s := range healthSignals {
		if failed[i] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", failed[i], s.failed))
		}
	}
	if unknown > 0 {
		parts = append(parts, fmt.Sprintf("%d signal(s) UNKNOWN — the streamer could not tell, which is not the same as ok", unknown))
	}
	if len(parts) == 0 {
		return fmt.Sprintf("all %d signals ok", len(ts)*len(healthSignals))
	}
	return strings.Join(parts, " · ")
}

// safeText strips control characters from everything the streamer sends. These strings are built
// from a Machine's stderr and from Pulumi stack outputs — remote text — and a table is not a place
// to replay escape sequences: a crafted campaign name or an SSH error could otherwise repaint the
// operator's terminal. Tabs and newlines are flattened for the same reason they are dangerous
// here at all, that they would silently desync every column below.
func safeText(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
}

func machineCount(ts []terminal) int {
	seen := map[string]bool{}
	for _, t := range ts {
		seen[t.Machine] = true
	}
	return len(seen)
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// actorRef is the placement, or "-" — a Machine with no Artifact placed on it still has
// Terminals (that is the point of converging a session on demand), so an empty actor is a
// legitimate state and not a missing field.
func (t terminal) actorRef() string {
	switch {
	case t.Actor == "":
		return "-"
	case t.Version == "":
		return t.Actor
	default:
		return t.Actor + "@" + t.Version
	}
}

// snapshotText mirrors workerRow.lastText: an age, or "-" when there has never been one. Never
// "0s ago" for a Terminal that was never painted.
func (t terminal) snapshotText() string {
	if t.LastSnapshotAt <= 0 {
		return "-"
	}
	return time.Since(time.UnixMilli(t.LastSnapshotAt)).Round(time.Second).String() + " ago"
}
