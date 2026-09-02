package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// The data directory is the appliance's whole state, so where it lands is a decision and not an
// implementation detail: `$KONTRA_HOME/data` is the same installation the config, the workflows
// and the actors already live in.
func TestApplianceDataDirFollowsKontraHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KONTRA_HOME", home)

	got, err := applianceDataDir("")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, "data"); got != want {
		t.Errorf("applianceDataDir() = %q, want %q", got, want)
	}
}

// A relative --data-dir is made absolute before the server sees it. The server resolves the
// SQLite filename against its own working directory, so a relative path that looks right on the
// command line can open a different file than the one the operator meant.
func TestApplianceDataDirMakesTheOverrideAbsolute(t *testing.T) {
	got, err := applianceDataDir("appliance-state")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("applianceDataDir(%q) = %q, which is not absolute", "appliance-state", got)
	}
	if filepath.Base(got) != "appliance-state" {
		t.Errorf("applianceDataDir lost the name it was given: %q", got)
	}
}

// `kontra up` takes flags and nothing else. Without this, `kontra up --data-dir x somedir` would
// silently ignore the argument — and the operator who typed it would be watching a control plane
// writing to a directory they did not name.
func TestUpRefusesPositionalArguments(t *testing.T) {
	err := cmdUp([]string{"nonsense"})
	if err == nil {
		t.Fatal("`kontra up <arg>` must be refused")
	}
	if !strings.Contains(err.Error(), "nonsense") {
		t.Errorf("the refusal must quote the argument, got: %v", err)
	}
}

// Help is the one surface nothing else checks (see serve_test.go's version of this).
func TestHelpListsUp(t *testing.T) {
	if !strings.Contains(usageText, "kontra up ") {
		t.Error("`kontra help` does not list `up`, the command that starts the appliance")
	}
	// …and still lists the compose one, which is a different topology and not a synonym.
	if !strings.Contains(usageText, "kontra infra up|down|status") {
		t.Error("`kontra help` no longer lists `infra up`; `up` did not replace it")
	}
}

// The appliance's third service has to be REACHABLE, which means `kontra up` has to expose the
// same two knobs the other two have: which port, and where the data lives. Without the flag an
// operator whose box already runs a redis-server on 6379 has no way to start the appliance at
// all — and 6379 is the most commonly-taken of the three ports by a wide margin.
func TestUpTakesTheStateStorePort(t *testing.T) {
	err := cmdUp([]string{"--kv-port", "not-a-number"})
	if err == nil {
		t.Fatal("`kontra up --kv-port not-a-number` must be refused")
	}
	if !strings.Contains(err.Error(), "kv-port") {
		t.Errorf("the refusal must name the flag, got: %v", err)
	}
	// The ceiling is a flag too, because the compose service it replaces carried `--maxmemory`
	// and dropping a bound silently is how a control plane gets OOM-killed by its own state.
	if err := cmdUp([]string{"--kv-max-bytes", "not-a-number"}); err == nil ||
		!strings.Contains(err.Error(), "kv-max-bytes") {
		t.Errorf("`kontra up --kv-max-bytes` is not a flag: %v", err)
	}
}

// `kontra up` prints what came up and what to point at it. The state store's line is what tells
// an operator the value of KONTRA_REDIS_HOST — the variable whose absence once cost a whole
// run, because every surface reported the worker healthy while it spun on a name that
// resolved to nothing.
func TestApplianceReadyNamesTheStateStore(t *testing.T) {
	if !strings.Contains(usageText, "--kv-port") {
		t.Error("`kontra help` does not mention --kv-port, so the port cannot be found without reading the source")
	}
	if !strings.Contains(usageText, "state") {
		t.Error("`kontra help`'s appliance entry does not mention the state store")
	}
}

// The codec is the appliance's fourth service and the only one a BROWSER calls, so it takes the
// same knob the other three take. The flag also stands in for what the fold removed: the container
// needed a listen port, a published port and a `--ui-codec-endpoint` that had to equal the second
// by hand, and this is now the only value an operator sets — the endpoint is read off the listener.
func TestUpTakesTheCodecPort(t *testing.T) {
	err := cmdUp([]string{"--codec-port", "not-a-number"})
	if err == nil {
		t.Fatal("`kontra up --codec-port not-a-number` must be refused")
	}
	if !strings.Contains(err.Error(), "codec-port") {
		t.Errorf("the refusal must name the flag, got: %v", err)
	}
	if !strings.Contains(usageText, "--codec-port") {
		t.Error("`kontra help` does not mention --codec-port, so the port cannot be found without reading the source")
	}
	if !strings.Contains(usageText, "codec") {
		t.Error("`kontra help`'s appliance entry does not mention the codec")
	}
}

// The sixth thing `kontra up` starts is the one that is not a library, so it gets the two knobs
// the other five have: which one to run, and what port it answers on. A missing value must fail
// at PARSE — before a listener is bound, before an artifact is hydrated — so that a typo costs
// nothing.
func TestUpTakesTheOrchestratorFlags(t *testing.T) {
	err := cmdUp([]string{"--orchestrator"})
	if err == nil {
		t.Fatal("`kontra up --orchestrator` with no value must be refused")
	}
	if !strings.Contains(err.Error(), "orchestrator") {
		t.Errorf("the refusal must name the flag, got: %v", err)
	}
	if err := cmdUp([]string{"--api-port", "not-a-number"}); err == nil ||
		!strings.Contains(err.Error(), "api-port") {
		t.Errorf("`kontra up --api-port` is not a flag: %v", err)
	}
}

// The DEVELOPER STORY IS PART OF THE INTERFACE, not a note in a source file. `kontra infra up`
// silently reverts a container to its image and undoes a locally built API; the equivalent here
// has to be findable by somebody who has been bitten and is looking for the switch.
func TestHelpExplainsWhichOrchestratorRuns(t *testing.T) {
	for _, want := range []string{"--orchestrator", "--api-port", "auto runs YOUR build"} {
		if !strings.Contains(usageText, want) {
			t.Errorf("`kontra help` does not mention %q, so the choice cannot be found without reading the source", want)
		}
	}
}
