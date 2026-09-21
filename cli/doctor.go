// doctor.go — `kontra doctor`: a human-facing snapshot of the control plane.
//
// Where `infra status` is a terse pass/fail probe, `doctor` is the "where is
// everything" view: each core service with its endpoint + health, the browsable
// web consoles (with their ports and what each is for), and how many actors are
// registered. Reuses the probe helpers from infra.go (httpOK/httpAnswers/tcpUp)
// and the color helper from main.go. Read-only; never mutates.
package main

import (
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/medmahmoudi26/kontra/cli/internal/cliio"
	"github.com/medmahmoudi26/kontra/cli/internal/cliutil"
	"github.com/medmahmoudi26/kontra/cli/internal/config"
)

// Local-dev host-mapped ports (docker-compose.yml). Local-first scope: these are the
// contract's own ports, not env knobs — same stance infra.go takes for the seaweed URL.
const (
	portTemporalUI = "8233" // Temporal Web UI
	portSeaweedS3  = "8333" // SeaweedFS S3 gateway
	portSeaweedUI  = "8888" // SeaweedFS filer (object browser)
	// The Dashboard streamer's port is NOT repeated here: `panels.go` owns that fact as
	// `defaultPanelPort` + KONTRA_PANEL_PORT, and two spellings of one port is how a moved port
	// starts showing up in one table and not the other.

	// THE SAME STRING `control/orchestrator/src/panels/metrics.ts` HARD-CODES as
	// `DEFAULT_METRICS_URL`, and `metrics.test.ts` asserts. It is a compose hostname, not a host
	// port, because the service is deliberately unpublished — see `metricsHealth`.
	defaultMetricsURL = "http://victoriametrics:8428"
)

// hostOf extracts the bare host from "host:port" or "scheme://host:port[/path]"; "" →
// localhost. Lets doctor point every UI at the same host the orchestrator/temporal use,
// so a remote KONTRA_ORCHESTRATOR_URL surfaces the right links, not hardcoded localhost.
func hostOf(s string) string {
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimPrefix(s, "https://")
	if i := strings.IndexAny(s, ":/"); i >= 0 {
		s = s[:i]
	}
	if s == "" {
		return "localhost"
	}
	return s
}

// svcRow is one line of the Services table: a probed component + how to reach it.
type svcRow struct {
	name     string
	role     string
	endpoint string
	ok       bool
	stat     string // status override (e.g. "4 running"); "" → up/down from ok
}

// uiRow is one browsable web console.
type uiRow struct {
	name string
	url  string
	desc string
}

func cmdDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	apiURL := fs.String("api", orchestratorURL(), "orchestrator base URL")
	if err := fs.Parse(args); err != nil {
		return err
	}
	api := newAPI(*apiURL)
	orchHost := hostOf(*apiURL)
	tempHost := hostOf(config.TemporalAddress())

	// --- control-plane compose count (best-effort; docker/compose may be absent) ---
	composeOK, composeStat := false, "docker compose unavailable"
	if root, err := cliutil.FindRepoRoot(""); err == nil {
		if out, perr := dockerOut(root, "compose", "ps", "-q", "--status", "running"); perr == nil {
			n := 0
			if out != "" {
				n = len(strings.Split(out, "\n"))
			}
			composeOK, composeStat = n > 0, fmt.Sprintf("%d running", n)
		}
	}

	// The Dashboard streamer's base URL comes from `panels.go`'s resolver — one source of truth for
	// KONTRA_PANEL_URL and KONTRA_PANEL_PORT. Its fallback is `localhost`, though, and doctor's own
	// rule (see hostOf) is that every row points at the host the orchestrator is on, so a remote
	// `--api` does not quietly probe the operator's laptop. An explicit KONTRA_PANEL_URL still wins.
	panelsURL := panelURL()
	if os.Getenv("KONTRA_PANEL_URL") == "" && orchHost != "localhost" {
		panelsURL = ui(orchHost, cliutil.EnvOr("KONTRA_PANEL_PORT", defaultPanelPort))
	}
	panelsOK, panelsStat := panelsHealth(panelsURL)
	metricsAddr, metricsOK, metricsStat := metricsHealth()

	services := []svcRow{
		{"control plane", "docker compose containers", "-", composeOK, composeStat},
		{"orchestrator", "catalog + run/dataset API, web UI", *apiURL, httpOK(*apiURL + "/api/health"), ""},
		{"temporal", "workflow engine (gRPC)", config.TemporalAddress(), tcpUp(config.TemporalAddress()), ""},
		{"seaweedfs", "S3 object store", ui(orchHost, portSeaweedS3), httpAnswers(ui(orchHost, portSeaweedS3)), ""},
		{"panels", "Dashboard streamer (forked child of orchestrator-infra)", panelsURL, panelsOK, panelsStat},
		{"metrics", "VictoriaMetrics — what every Machine's vmagent pushes to", metricsAddr, metricsOK, metricsStat},
	}

	consoles := []uiRow{
		{"Orchestrator", *apiURL, "graphs · runs · SQL-queryable datasets"},
		// TWO ORIGINS, and the table says both: the Dashboard is the SPA's third view, so the link is
		// the SPA's, while its bytes come from the streamer's own URL (ADR 0020 — panel bytes never
		// touch the event loop serving the CLI's DuckDB queries). "read-only" and "lossy" are stated
		// on purpose: a wall that looks like a log invites someone to treat a Terminal as evidence for
		// what a Worker did, and the Manifest, the journal and the lake are the record.
		{"Dashboard", *apiURL, "Dashboard tab — read-only Terminals over the Fleet's tmux; lossy, NOT the record (streams from " + panelsURL + ")"},
		{"Temporal", ui(tempHost, portTemporalUI), "workflow executions & history"},
		{"SeaweedFS", ui(orchHost, portSeaweedUI) + "/buckets/kontra/", "object / blob browser"},
	}

	renderDoctor(services, consoles)

	// --- actors (best-effort; a down orchestrator just skips this block) ---
	var actors []actorRecord
	if err := api.getJSON("/api/actors", &actors); err != nil {
		fmt.Fprintf(cliio.Stdout, "\n%s actors: orchestrator unreachable (%s)\n", paint("1;31", "✗"), *apiURL)
		return nil
	}
	fmt.Fprintf(cliio.Stdout, "\n%s\n", paint("1", "Actors"))
	if len(actors) == 0 {
		fmt.Fprintln(cliio.Stdout, "  none registered — deploy one, then `kontra workers list`")
		return nil
	}
	refs := make([]string, len(actors))
	for i, a := range actors {
		refs[i] = a.Name + "@" + a.Version
	}
	fmt.Fprintf(cliio.Stdout, "  %d registered: %s\n", len(actors), strings.Join(refs, ", "))
	fmt.Fprintln(cliio.Stdout, "  run `kontra workers list` to see live Temporal workers per actor")

	reportStuck(api)
	return nil
}

// stuckReport mirrors `control/orchestrator/src/routes/stuck.ts`. Only the fields this prints.
type stuckReport struct {
	Executions []struct {
		WorkflowID string `json:"workflowId"`
		Type       string `json:"type"`
		Queue      string `json:"queue"`
		AgeMs      int64  `json:"ageMs"`
		Pollers    *int   `json:"pollers"`
		Wedged     bool   `json:"wedged"`
	} `json:"executions"`
	Wedged int  `json:"wedged"`
	Capped bool `json:"capped"`
}

// reportStuck prints open executions nothing is polling — the ones that will never move.
//
// WHY IT IS IN `doctor` AND NOT ON A PAGE: every one of the nine an audit found was an INTERNAL
// workflow type, which the Runs surface excludes on purpose, so the console could not have shown
// them without becoming a different page. "Is my installation healthy" is this command's question
// and these are an answer to it.
//
// SILENT WHEN THERE ARE NONE. A resting installation printing "0 wedged" is a line that stops being
// read, and this section only earns its space when it has something to say.
//
// BEST-EFFORT, like the rest of this command: a server too old for the route, or a cluster that
// cannot be listed, costs the section and not the report.
func reportStuck(api *apiClient) {
	var report stuckReport
	if err := api.getJSON("/api/stuck", &report); err != nil {
		return
	}
	if report.Wedged == 0 {
		return
	}
	fmt.Fprintf(cliio.Stdout, "\n%s\n", paint("1;33", "Wedged"))
	fmt.Fprintf(cliio.Stdout,
		"  %d open execution(s) on a task queue nobody is polling — they will not move until a worker\n"+
			"  returns to their queue, and they will RESUME when one does.\n", report.Wedged)
	for _, e := range report.Executions {
		if !e.Wedged {
			continue
		}
		fmt.Fprintf(cliio.Stdout, "    %-46s %-24s queue=%-28s age=%s\n",
			e.WorkflowID, e.Type, e.Queue, ageWords(e.AgeMs))
	}
	if report.Capped {
		fmt.Fprintln(cliio.Stdout, "    (the listing was capped — there may be more)")
	}
	// NOT A COMMAND THIS RUNS FOR YOU. Terminating somebody's execution is a decision: a wedged
	// Warden may be a Machine that is coming back. The report names them; a person decides.
	fmt.Fprintln(cliio.Stdout,
		"  serve the queue to let one finish, or end it deliberately:\n"+
			"    temporal workflow terminate -w <id> --reason 'wedged, no poller'")
}

// ageWords is a duration a person reads. Days past a day, because these are measured in days.
func ageWords(ms int64) string {
	switch {
	case ms >= 86_400_000:
		return fmt.Sprintf("%dd", ms/86_400_000)
	case ms >= 3_600_000:
		return fmt.Sprintf("%dh", ms/3_600_000)
	case ms >= 60_000:
		return fmt.Sprintf("%dm", ms/60_000)
	default:
		return fmt.Sprintf("%ds", ms/1000)
	}
}

// metricsHealth probes the backend every Machine's vmagent remote-writes to, and returns a status
// that distinguishes DOWN from "correctly not reachable from where you typed this".
//
// WHY THIS ROW EXISTS. `machine.ts` wrote vmagent's unit, its scrape config and its remote-write
// target, and for the life of every Fleet nothing listened on `:8428` — every fleet metric was
// discarded, and the only evidence was a `connection refused` line in a unit's journal on a Machine
// that no longer exists. ADR 0050 stands this backend up; this row is so the next person learns it
// from `kontra doctor` instead of by reading a unit file.
//
// "NOT PUBLISHED" IS NOT "DOWN", and conflating them would be the same false-green this file's
// neighbours warn about. `docker-compose.yml` publishes no host port for VictoriaMetrics on purpose
// (a published Docker port is DNATed in PREROUTING and a host firewall does not protect it), so the
// compose hostname does not resolve from the host — which is CORRECT, not broken. Run it where the
// name means something: `docker compose exec cli kontra doctor`.
func metricsHealth() (addr string, ok bool, stat string) {
	addr = cliutil.EnvOr("KONTRA_METRICS_URL", defaultMetricsURL)
	if httpOK(addr + "/health") {
		return addr, true, ""
	}
	// A name that does not resolve is the unpublished case; a name that resolves and refuses is a
	// backend that is actually down. The two want different sentences.
	if _, err := net.LookupHost(hostOf(addr)); err != nil {
		return addr, false, "not reachable from here (unpublished by design — try `compose exec cli`)"
	}
	return addr, false, "DOWN — fleet vmagent remote-writes are being discarded"
}

func ui(host, port string) string { return "http://" + host + ":" + port }

// panelsHealth probes the Dashboard streamer, and distinguishes SWITCHED OFF from DOWN.
//
// The streamer fails closed: with no KONTRA_PANEL_TOKEN configured, every panel route answers 503
// and serves nothing (auth.ts's own behaviour, reused). That is a deliberate configuration, not a
// fault, and reporting it as "DOWN" would send an operator looking for a crashed process. A
// connection refused, on the other hand, really is down — or the port is not published.
func panelsHealth(base string) (bool, string) {
	resp, err := statusHTTP.Get(base + "/api/panels/health")
	if err != nil {
		// The port is named from panels.go's constant, not spelled again here.
		return false, "DOWN (is " + cliutil.EnvOr("KONTRA_PANEL_PORT", defaultPanelPort) + " published?)"
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return true, ""
	case http.StatusServiceUnavailable:
		return false, "disabled (set KONTRA_PANEL_TOKEN)"
	default:
		return false, fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
}

// renderDoctor prints the Services table and the Web-consoles table.
func renderDoctor(services []svcRow, consoles []uiRow) {
	fmt.Fprintf(cliio.Stdout, "%s\n", paint("1", "Services"))
	w := tabwriter.NewWriter(cliio.Stdout, 2, 8, 2, ' ', 0)
	fmt.Fprintln(w, "  COMPONENT\tROLE\tENDPOINT\tSTATUS")
	fmt.Fprintln(w, "  ---------\t----\t--------\t------")
	for _, s := range services {
		fmt.Fprintf(w, "  %s\t%s\t%s\t%s\n", s.name, s.role, s.endpoint, statusText(s))
	}
	w.Flush()

	fmt.Fprintf(cliio.Stdout, "\n%s\n", paint("1", "Web consoles (open in a browser)"))
	uw := tabwriter.NewWriter(cliio.Stdout, 2, 8, 2, ' ', 0)
	for _, u := range consoles {
		fmt.Fprintf(uw, "  %s\t%s\t%s\n", u.name, u.url, u.desc)
	}
	uw.Flush()
}

// statusText renders a row's status: the override text if any, else up/down, colored.
func statusText(s svcRow) string {
	if s.stat != "" {
		if s.ok {
			return paint("1;32", s.stat)
		}
		return paint("1;31", s.stat)
	}
	if s.ok {
		return paint("1;32", "up")
	}
	return paint("1;31", "DOWN")
}
