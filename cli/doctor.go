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
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
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
	tempHost := hostOf(temporalAddress())

	// --- control-plane compose count (best-effort; docker/compose may be absent) ---
	composeOK, composeStat := false, "docker compose unavailable"
	if root, err := findRepoRoot(""); err == nil {
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
		panelsURL = ui(orchHost, envOr("KONTRA_PANEL_PORT", defaultPanelPort))
	}
	panelsOK, panelsStat := panelsHealth(panelsURL)

	services := []svcRow{
		{"control plane", "docker compose containers", "-", composeOK, composeStat},
		{"orchestrator", "catalog + run/dataset API, web UI", *apiURL, httpOK(*apiURL + "/api/health"), ""},
		{"temporal", "workflow engine (gRPC)", temporalAddress(), tcpUp(temporalAddress()), ""},
		{"seaweedfs", "S3 object store", ui(orchHost, portSeaweedS3), httpAnswers(ui(orchHost, portSeaweedS3)), ""},
		{"panels", "Dashboard streamer (forked child of orchestrator-infra)", panelsURL, panelsOK, panelsStat},
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
		fmt.Fprintf(stdout, "\n%s actors: orchestrator unreachable (%s)\n", paint("1;31", "✗"), *apiURL)
		return nil
	}
	fmt.Fprintf(stdout, "\n%s\n", paint("1", "Actors"))
	if len(actors) == 0 {
		fmt.Fprintln(stdout, "  none registered — deploy one, then `kontra workers list`")
		return nil
	}
	refs := make([]string, len(actors))
	for i, a := range actors {
		refs[i] = a.Name + "@" + a.Version
	}
	fmt.Fprintf(stdout, "  %d registered: %s\n", len(actors), strings.Join(refs, ", "))
	fmt.Fprintln(stdout, "  run `kontra workers list` to see live Temporal workers per actor")
	return nil
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
		return false, "DOWN (is " + envOr("KONTRA_PANEL_PORT", defaultPanelPort) + " published?)"
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
	fmt.Fprintf(stdout, "%s\n", paint("1", "Services"))
	w := tabwriter.NewWriter(stdout, 2, 8, 2, ' ', 0)
	fmt.Fprintln(w, "  COMPONENT\tROLE\tENDPOINT\tSTATUS")
	fmt.Fprintln(w, "  ---------\t----\t--------\t------")
	for _, s := range services {
		fmt.Fprintf(w, "  %s\t%s\t%s\t%s\n", s.name, s.role, s.endpoint, statusText(s))
	}
	w.Flush()

	fmt.Fprintf(stdout, "\n%s\n", paint("1", "Web consoles (open in a browser)"))
	uw := tabwriter.NewWriter(stdout, 2, 8, 2, ' ', 0)
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
