// workflowworker_test.go — the launch profile a served workflow worker gets: which interpreter,
// and which environment. Every failure these pin is SILENT and LATE, which is why they are unit
// tests of a list of strings rather than an assertion about a worker that started.
package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPythonForPrefersWhatItIsToldOverWhatItCanSee(t *testing.T) {
	// The venv probe runs on one side of a container boundary and the process runs on the other.
	// Serving from the Dashboard executes this code inside orchestrator-api, whose tmux client
	// drives the HOST's tmux server — so `<root>/.venv/bin/python` is resolved here and executed
	// there. When that venv is a symlink out of the mount it dangles in the container, the probe
	// falls through to `python3`, and the host's `python3` has no temporalio: the worker dies on
	// import. Being able to SAY which interpreter is the fix; probing harder is not.
	root := t.TempDir()
	venv := filepath.Join(root, ".venv", "bin")
	if err := os.MkdirAll(venv, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(venv, "python"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("KONTRA_PYTHON", "")
	if got := pythonFor(root, ""); got != filepath.Join(root, ".venv", "bin", "python") {
		t.Errorf("with a real venv and nothing set, pythonFor = %q, want the venv", got)
	}

	// The environment beats a venv that IS there — the local one may not be the one the process
	// will run under.
	t.Setenv("KONTRA_PYTHON", "/opt/host/bin/python")
	if got := pythonFor(root, ""); got != "/opt/host/bin/python" {
		t.Errorf("pythonFor ignored KONTRA_PYTHON: %q", got)
	}
	// And the flag beats the environment, so one serve can differ without editing compose.
	if got := pythonFor(root, "/usr/bin/python3.12"); got != "/usr/bin/python3.12" {
		t.Errorf("pythonFor ignored --python: %q", got)
	}

	// Nothing set and no venv: `python3`, not an error. A checkout without a venv is an ordinary
	// way to run this where temporalio is installed system-wide.
	t.Setenv("KONTRA_PYTHON", "")
	if got := pythonFor(t.TempDir(), ""); got != "python3" {
		t.Errorf("pythonFor fell through to %q, want python3", got)
	}
}

func TestCodecEnvCarriesTheClaimCheckStore(t *testing.T) {
	// Forwarding these is what keeps a served worker out of PASSTHROUGH. The failure it prevents
	// is late and quiet: passthrough only matters once a payload crosses the threshold, and then
	// it fails at DECODE on another machine rather than at the dispatch that caused it.
	for _, want := range []string{"KONTRA_S3_ENDPOINT", "KONTRA_S3_ACCESS_KEY", "KONTRA_S3_SECRET_KEY"} {
		found := false
		for _, name := range codecEnv {
			if name == want {
				found = true
			}
		}
		if !found {
			t.Errorf("codecEnv does not carry %s — the worker will run in passthrough", want)
		}
	}
}

// THE PREFIX IS FORWARDED TOO, and it is the one this CLI got wrong on its own side until the
// fifth codec was folded into the handler's (see claimcheck.go). A worker told a different prefix
// from the CLI that reads its result addresses the same object under two names.
func TestCodecEnvCarriesTheKeyPrefixTheCLIAlsoReadsWith(t *testing.T) {
	found := false
	for _, name := range codecEnv {
		if name == "KONTRA_S3_PREFIX" {
			found = true
		}
	}
	if !found {
		t.Error("codecEnv does not carry KONTRA_S3_PREFIX — the worker would write under a " +
			"different address from the one `workflow start --wait` reads")
	}
}

// A served workflow worker self-registers what each of its @workflow.defn classes takes, returns
// and is for — and `publish_workflow_catalog` returns immediately when KONTRA_ORCHESTRATOR_URL is
// unset. A tmux pane inherits the tmux SERVER's environment, so a variable exported in the
// operator's shell reaches the worker only if it is passed explicitly here. Without this line the
// whole registration path is a no-op on the ONE way workflows are actually served, and it fails
// silently by design: the worker serves perfectly and the Workflows page shows a file with no
// contract. `kontra serve` passes the same variable to an actor for the same reason.
func TestServeEnvCarriesTheCatalogItRegistersWith(t *testing.T) {
	t.Setenv("KONTRA_ORCHESTRATOR_URL", "http://10.0.0.7:8088")
	delta := serveEnvDelta("/checkout", "recon")
	if !has(delta, "KONTRA_ORCHESTRATOR_URL=http://10.0.0.7:8088") {
		t.Errorf("serveEnvDelta does not carry the orchestrator URL — nothing registers: %v", delta)
	}
}

// --watch is OPT-IN: without the flag it adds nothing, so a plain serve behaves exactly as it did
// before the flag existed; with it, it sets the ONE variable `serve_workflows_async` reads to arm the
// re-registration loop. And it is deliberately NOT in serveEnvDelta, which `resume` shares — resuming
// a paused run must not re-arm a save loop it never asked for.
func TestWatchEnvIsOptInAndSeparateFromResume(t *testing.T) {
	if watchEnv(false) != nil {
		t.Errorf("a plain serve must add no watch env, got %v", watchEnv(false))
	}
	if !has(watchEnv(true), "KONTRA_WORKFLOW_WATCH=1") {
		t.Errorf("--watch does not set KONTRA_WORKFLOW_WATCH — the worker never arms the loop: %v", watchEnv(true))
	}
	t.Setenv("KONTRA_ORCHESTRATOR_URL", "http://10.0.0.7:8088")
	if has(serveEnvDelta("/checkout", "recon"), "KONTRA_WORKFLOW_WATCH=1") {
		t.Error("serveEnvDelta carries the watch flag — resume would re-arm a loop it did not ask for")
	}
}
