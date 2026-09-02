package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/medmahmoudi26/kontra/cli/internal/cliio"
)

// captureStdout runs `body` with the package writer redirected, on top of dispatch_test.go's
// existing `swap` rather than re-implementing the save/restore around it.
func captureStdout(t *testing.T, body func()) string {
	t.Helper()
	var buf bytes.Buffer
	defer swap[io.Writer](&cliio.Stdout, &buf)()
	body()
	return buf.String()
}

// `--tmux` used to be a stack argument that installed a systemd unit; it is now "also converge a
// tmux session after placement" (ADR 0020). The tests here pin the two halves of that: the mapping
// from the inventory to one converge per Machine, and that the CLI never decides what a window runs.
//
// Temporal is behind an interface (`tmuxConverger`), the same seam `queueDescriber` gives
// `kontra workers list`, so nothing here dials anything.

func fleetOutputs() map[string]any {
	return map[string]any{
		"tag":          "crawl",
		"actorName":    "webcrawl",
		"actorVersion": "0.2.0",
		"inventory": map[string]any{
			"kf-crawl-02": map[string]any{
				"name": "kf-crawl-02", "host": "10.124.0.10", "publicIp": "203.0.113.10", "tag": "crawl",
			},
			"kf-crawl-01": map[string]any{
				"name": "kf-crawl-01", "host": "10.124.0.9", "publicIp": "203.0.113.9", "tag": "crawl",
			},
		},
	}
}

func TestTmuxTargetsComeFromTheInventory(t *testing.T) {
	got := tmuxSessionTargets(fleetOutputs())
	if len(got) != 2 {
		t.Fatalf("want one converge per Machine, got %d: %+v", len(got), got)
	}
	// Sorted, so the output an operator reads is stable across converges.
	if got[0].Machine != "kf-crawl-01" || got[1].Machine != "kf-crawl-02" {
		t.Fatalf("not ordered by machine: %+v", got)
	}
	// The PRIVATE VPC address: that is what MachineEntry documents as how the Controller reaches a
	// Machine.
	if got[0].Host != "10.124.0.9" {
		t.Fatalf("want the private address, got %q", got[0].Host)
	}
	// `<actor>-<version>`, matching `panels/discovery.ts:sessionNameFor` — the two are written
	// independently and this is where they are held to the same answer. The VERSION is the
	// load-bearing half: `kontra-webcrawl` named two builds of one Actor identically.
	if got[0].Session != "webcrawl-0_2_0" {
		t.Fatalf("session name: %q", got[0].Session)
	}
}

func TestTmuxTargetsNeverCarryACommand(t *testing.T) {
	// The CLI picks WHICH Machine gets a session; the workflow decides WHAT its windows run. If the
	// CLI sent commands, anyone who can reach Temporal could choose what runs as root on every
	// Machine in the run — the authority machine.ts refuses to hand a caller.
	for _, in := range tmuxSessionTargets(fleetOutputs()) {
		if len(in.Windows) != 0 {
			t.Fatalf("%s: windows should be empty (server-side defaults), got %+v", in.Machine, in.Windows)
		}
	}
}

func TestTmuxTargetsFallBackToTheTagAndThePublicAddress(t *testing.T) {
	out := fleetOutputs()
	out["actorName"] = "" // `fleet up` with no placement — still a Machine, still a tile
	out["tag"] = "crawl"
	out["inventory"] = map[string]any{
		"kf-crawl-01": map[string]any{"name": "kf-crawl-01", "publicIp": "203.0.113.9", "tag": "crawl"},
	}
	got := tmuxSessionTargets(out)
	if len(got) != 1 || got[0].Session != "crawl" {
		t.Fatalf("want the tag as the session name, got %+v", got)
	}
	if got[0].Host != "203.0.113.9" {
		t.Fatalf("want the public address when there is no private one, got %q", got[0].Host)
	}
}

// TestTmuxTargetsOnAPackedMachineAreNamedAfterTheFleet — a tmux session is per MACHINE (ADR 0020)
// and a packed Machine belongs to no single Actor (ADR 0037).
//
// The scalars in these outputs describe only the FIRST placement, so `nscheck-0.1.0` on a Machine
// also running `subfinder` is a name that says the Machine is one Actor's while a second one is on
// it — and it is the name an operator types to attach. The tag is the honest answer, and it is
// already `fleetSessionName`'s fallback for a Machine with no placement at all.
func TestTmuxTargetsOnAPackedMachineAreNamedAfterTheFleet(t *testing.T) {
	out := fleetOutputs()
	out["tag"] = "dns"
	out["placements"] = []any{
		map[string]any{"actorName": "nscheck", "actorVersion": "0.1.0", "bundleUrl": "http://c/n"},
		map[string]any{"actorName": "subfinder", "actorVersion": "0.2.0", "bundleUrl": "http://c/s"},
	}
	got := tmuxSessionTargets(out)
	if len(got) == 0 {
		t.Fatal("no targets; this test proves nothing")
	}
	for _, g := range got {
		if g.Session != "dns" {
			t.Fatalf("session = %q on a packed Machine, want the fleet's tag", g.Session)
		}
	}

	// THE CONTROL, and it is what keeps the change above from being "packing broke the session name".
	// One placement still names the Actor and the VERSION — the substantive half, since two fleets
	// running two builds of one Actor would otherwise share a session name. The scalars are what
	// carry it, and a one-entry `placements` array must not disturb them.
	one := fleetOutputs()
	one["tag"] = "dns"
	one["actorName"] = "nscheck"
	one["actorVersion"] = "0.1.0"
	one["placements"] = []any{
		map[string]any{"actorName": "nscheck", "actorVersion": "0.1.0", "bundleUrl": "http://c/n"},
	}
	if got := tmuxSessionTargets(one); len(got) == 0 || got[0].Session != "nscheck-0_1_0" {
		t.Fatalf("a single-placement Fleet must keep its actor-named session, got %+v", got)
	}
}

func TestTmuxTargetsToleratesNoInventory(t *testing.T) {
	if got := tmuxSessionTargets(map[string]any{}); len(got) != 0 {
		t.Fatalf("want none, got %+v", got)
	}
	if got := tmuxSessionTargets(map[string]any{"inventory": "not an object"}); len(got) != 0 {
		t.Fatalf("want none, got %+v", got)
	}
}

type fakeConverger struct {
	seen   []tmuxSessionInput
	failOn string
	closed bool
}

func (f *fakeConverger) Converge(_ context.Context, in tmuxSessionInput) (tmuxSessionResult, error) {
	f.seen = append(f.seen, in)
	if in.Machine == f.failOn {
		return tmuxSessionResult{}, errors.New("apt-get: Unable to locate package tmux")
	}
	return tmuxSessionResult{
		Machine: in.Machine, Session: in.Session, Windows: []string{"actor", "handler"}, Created: true,
	}, nil
}

func (f *fakeConverger) Close() { f.closed = true }

func withConverger(t *testing.T, fake *fakeConverger) {
	t.Helper()
	prev := newTmuxConverger
	newTmuxConverger = func() (tmuxConverger, error) { return fake, nil }
	t.Cleanup(func() { newTmuxConverger = prev })
}

func convergedFleet() *stackOpStatus {
	st := &stackOpStatus{FQN: "kontra-fleet/run-apex-119", Status: "COMPLETED"}
	st.Result = &struct {
		FQN     string         `json:"fqn"`
		Result  string         `json:"result"`
		Changes map[string]int `json:"changes"`
		Outputs map[string]any `json:"outputs"`
	}{FQN: st.FQN, Result: "succeeded", Outputs: fleetOutputs()}
	return st
}

func TestConvergeSessionsRunsOncePerMachineAndReportsEachOne(t *testing.T) {
	fake := &fakeConverger{}
	withConverger(t, fake)
	out := captureStdout(t, func() {
		if err := convergeSessions(convergedFleet(), 30*time.Second); err != nil {
			t.Fatal(err)
		}
	})
	if len(fake.seen) != 2 {
		t.Fatalf("want one converge per Machine, got %d", len(fake.seen))
	}
	if !fake.closed {
		t.Fatal("the Temporal client was left open")
	}
	for _, want := range []string{"kf-crawl-01", "kf-crawl-02", "created session webcrawl-0_2_0", "2/2"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

func TestConvergeSessionsReportsAFailureWithoutFailingTheDeploy(t *testing.T) {
	// The actor is already placed and running by the time this runs. A Machine that cannot be given
	// a VIEW is not a Machine that cannot work — but the failure must be said out loud, because the
	// silent version of exactly this is what ADR 0020 deleted from machine.ts.
	fake := &fakeConverger{failOn: "kf-crawl-02"}
	withConverger(t, fake)
	out := captureStdout(t, func() {
		if err := convergeSessions(convergedFleet(), 30*time.Second); err != nil {
			t.Fatalf("a session failure must not fail the deploy: %v", err)
		}
	})
	if !strings.Contains(out, "kf-crawl-02: session converge FAILED") {
		t.Fatalf("the failure is not visible:\n%s", out)
	}
	if !strings.Contains(out, "1/2") {
		t.Fatalf("the count should say one of two:\n%s", out)
	}
}

func TestConvergeSessionsSaysSoWhenThereIsNothingToDo(t *testing.T) {
	fake := &fakeConverger{}
	withConverger(t, fake)
	out := captureStdout(t, func() {
		if err := convergeSessions(&stackOpStatus{Status: "COMPLETED"}, time.Second); err != nil {
			t.Fatal(err)
		}
	})
	if len(fake.seen) != 0 {
		t.Fatalf("nothing to converge, but %d calls were made", len(fake.seen))
	}
	if out != "" && !strings.Contains(out, "nothing to converge") {
		t.Fatalf("unexpected output: %s", out)
	}
}

func TestTmuxFlagHelpSaysWhatItNowMeans(t *testing.T) {
	f := fleetFlagSet("deploy")
	flag := f.fs.Lookup("tmux")
	if flag == nil {
		t.Fatal("--tmux is gone")
	}
	// The old help said "install an attachable tmux session on each Machine", which described a
	// systemd unit that no longer exists.
	if !strings.Contains(flag.Usage, "converge") {
		t.Fatalf("--tmux help does not say it converges a session: %q", flag.Usage)
	}
	if strings.Contains(flag.Usage, "install") {
		t.Fatalf("--tmux help still describes an install: %q", flag.Usage)
	}
}

func TestTmuxIsNotAStackArgument(t *testing.T) {
	f := fleetFlagSet("deploy")
	if err := f.fs.Parse([]string{"--tag", "crawl", "--tmux"}); err != nil {
		t.Fatal(err)
	}
	args, err := f.args(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := args["tmux"]; ok {
		t.Fatalf("--tmux must not reach the stack args any more: %+v", args)
	}
}
