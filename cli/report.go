package main

// `kontra report` — the developer loop for a report template (ADR 0055, spec §6.2).
//
// ── WHY PREVIEW EXISTS AT ALL ───────────────────────────────────────────────────────────────────
//
// A report renders when a run ENDS. So the naive loop for "does my template say what I meant" is: edit
// `report.md`, start a run, wait however long the work takes, read the report, edit again. Preview
// collapses that to one request: the template text goes up, the orchestrator renders it against the
// run's real metadata and return value, and NOTHING IS STORED. `serve`'s lint catches a template that
// cannot work; this answers the question the lint cannot, which is whether it says the right thing.
//
// ── IT PRINTS MARKDOWN, AND DOES NOT RENDER IT ──────────────────────────────────────────────────
//
// The response carries both the mdast snapshot and the Markdown. This prints the Markdown, because a
// second mdast walker written in Go would be two implementations of one rendering that must agree
// forever — and the first time they disagreed, a preview would show something the console does not.

import (
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// reportCmd dispatches `kontra report <subcommand>`.
func reportCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: kontra report preview <runId> [--template report.md] [--open]")
	}
	switch args[0] {
	case "preview":
		return reportPreview(args[1:])
	default:
		return fmt.Errorf("unknown report subcommand %q (want preview)", args[0])
	}
}

// previewResponse is the shape `POST /api/runs/:runId/report/preview` answers with.
type previewResponse struct {
	RunID    string `json:"runId"`
	Preview  bool   `json:"preview"`
	Status   string `json:"status"`
	Markdown string `json:"markdown"`
	Error    string `json:"error"`
}

func reportPreview(args []string) error {
	fs := flag.NewFlagSet("report preview", flag.ContinueOnError)
	template := fs.String("template", reportFile, "the template to render (default: ./report.md)")
	api := fs.String("api", orchestratorURL(), "orchestrator base URL")
	open := fs.Bool("open", false, "open the run's report page in a browser as well")
	runID, rest := leadingPositional(args)
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if runID == "" && fs.NArg() == 1 {
		runID = fs.Arg(0)
	}
	if runID == "" {
		return errors.New("usage: kontra report preview <runId> [--template report.md] [--open]")
	}

	text, err := os.ReadFile(*template)
	if err != nil {
		return fmt.Errorf("cannot read %s: %w", *template, err)
	}

	// LINTED LOCALLY FIRST, so an obvious mistake costs no round trip and reads the same here as it
	// does from `serve`.
	if problems := lintReportTemplate(*template, string(text)); len(problems) > 0 {
		return reportLintRefusal(problems)
	}

	// THE PREVIEW ROUTE IS FAIL-CLOSED on the explore token, the same credential the query workbench
	// takes and for the same stated reason: a report is a rendering of a run's output, which routinely
	// contains targets and sometimes secrets.
	client := newAuthAPI(*api, exploreToken())
	var out previewResponse
	path := "/api/runs/" + url.PathEscape(runID) + "/report/preview"
	if err := client.postJSON(path, map[string]any{"template": string(text)}, &out); err != nil {
		return fmt.Errorf("preview %s: %w", runID, err)
	}
	if out.Status == "error" {
		// THE TEMPLATE'S OWN ERROR, not this command's. Printed as the renderer said it, because the
		// renderer is the authority on what the template did and a paraphrase would lose the line.
		return fmt.Errorf("the template failed to render:\n  %s", out.Error)
	}
	fmt.Print(out.Markdown)
	if !strings.HasSuffix(out.Markdown, "\n") {
		fmt.Println()
	}
	fmt.Fprintf(os.Stderr, "\nkontra: preview only — nothing was stored. %s\n", reportPageURL(*api, runID))
	if *open {
		openInBrowser(reportPageURL(*api, runID))
	}
	return nil
}

// reportPageURL is where a person reads this run's stored report.
func reportPageURL(base, runID string) string {
	return strings.TrimSuffix(base, "/") + "/runs/" + url.PathEscape(runID) + "/report"
}

// openInBrowser is best-effort and says so: a failure prints the URL rather than failing the command,
// because the preview already succeeded and the browser is a convenience.
func openInBrowser(target string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", target)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	default:
		cmd = exec.Command("xdg-open", target)
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "kontra: could not open a browser (%v) — the page is at %s\n", err, target)
	}
}
