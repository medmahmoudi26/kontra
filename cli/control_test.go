package main

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/medmahmoudi26/kontra/cli/internal/hostengine"
)

// A NEW WORD WIRED TO NOTHING is a switch-statement bug, and `dispatch`'s own header says it was split
// out of `main` so that exact mistake could be tested. `control` is a new word.
func TestControlIsARoutedCommand(t *testing.T) {
	var err error
	out := captureStdout(t, func() { err = dispatch([]string{"control"}) })
	if err == nil {
		t.Fatal("`kontra control` with no subcommand succeeded; it should say what the subcommands are")
	}
	if errors.Is(err, errUsage) {
		t.Fatal("`kontra control` fell through to the unknown-command branch, so it is not wired up")
	}
	for _, want := range []string{"kontra control up", "kontra control down", "--preview"} {
		if !strings.Contains(out, want) {
			t.Errorf("the usage it printed does not mention %q", want)
		}
	}
}

func TestControlNamesItsSubcommandsWhenGivenAWrongOne(t *testing.T) {
	var err error
	captureStdout(t, func() { err = dispatch([]string{"control", "converge"}) })
	if err == nil {
		t.Fatal("an unknown subcommand was accepted")
	}
	if !strings.Contains(err.Error(), "up|down") {
		t.Errorf("the message must list the verbs this command has: %v", err)
	}
}

// Help is the one surface nothing else checks — see `usageText`'s own comment. A command that reaches
// the switch and not that block is a command nobody finds.
func TestControlIsDocumented(t *testing.T) {
	for _, want := range []string{"kontra up [", "kontra down [", "--preview", "kontra-control"} {
		if !strings.Contains(usageText, want) {
			t.Errorf("usageText does not advertise %q", want)
		}
	}
	// And it says WHICH topology this is, because `kontra infra up` is a second way to start a
	// control plane in the same help text. Two verbs that both start one, with nothing saying which
	// is which, is how somebody runs two at once on the same ports.
	if !strings.Contains(usageText, "HOST ENGINE") {
		t.Error("usageText does not say that `kontra up` is the host engine, and this CLI also " +
			"advertises `kontra infra up`")
	}
}

// flagRe pulls every `--flag` out of a block of prose.
var flagRe = regexp.MustCompile(`--([a-z][a-z0-9-]*)`)

// EVERY FLAG THIS COMMAND TELLS SOMEBODY TO TYPE EXISTS.
//
// `cli/fleet_documented_flags_test.go` does this for the fleet family and its SCOPE note (line 21-24)
// is explicit that it is deliberately NOT a repo-wide sweep — so a new command's documented flags are
// unguarded unless the equivalent is written, and this is it. Both surfaces are swept: `usageText`,
// which nothing else checks, and `controlUsage()`, which is what a wrong subcommand prints.
func TestEveryDocumentedControlFlagIsRegistered(t *testing.T) {
	registered := map[string]bool{}
	controlFlagSet("up").fs.VisitAll(func(f *flag.Flag) { registered[f.Name] = true })
	if len(registered) == 0 {
		t.Fatal("no flags are registered at all, so this sweep would pass on any prose")
	}

	// THE `kontra up` AND `kontra down` BLOCKS OF usageText, AND ONLY THEM. Scoped rather than swept
	// over the whole text for the reason `fleet_documented_flags_test.go:21-24` gives about its own
	// scope: this file knows one command's flags. A continuation line is any indented line that does
	// not itself start a new `kontra …` entry.
	var prose []string
	inBlock := false
	for _, line := range strings.Split(usageText, "\n") {
		// A BLOCK STARTS ONLY WHERE THE LINE ITSELF IS THE ENTRY, not wherever the words appear, and
		// the `[` is load-bearing: a bare "kontra up" prefix also matches `kontra update`, whose
		// --check and --to would then be swept as though they were this command's. The same rule is
		// in `update_test.go`'s copy of this extraction.
		if t := strings.TrimLeft(line, " "); strings.HasPrefix(t, "kontra up [") || strings.HasPrefix(t, "kontra down [") {
			inBlock, prose = true, append(prose, line)
			continue
		}
		if !inBlock {
			continue
		}
		if t := strings.TrimLeft(line, " "); t == "" || strings.HasPrefix(t, "kontra ") {
			inBlock = false
			continue
		}
		prose = append(prose, line)
	}
	prose = append(prose, captureStdout(t, controlUsage))
	if len(prose) < 4 {
		t.Fatalf("only %d lines of `kontra up`/`kontra down` help were found in usageText — the "+
			"extraction is broken and the assertion below proves nothing", len(prose))
	}

	seen := 0
	for _, line := range prose {
		for _, m := range flagRe.FindAllStringSubmatch(line, -1) {
			name := m[1]
			// `--force`-style words that belong to another command's sentence would be false positives;
			// there are none here, and the sweep is deliberately strict so one would be noticed.
			seen++
			if !registered[name] {
				t.Errorf("the help tells somebody to type --%s and `control up` does not register it:\n  %s", name, strings.TrimSpace(line))
			}
		}
	}
	if seen < 5 {
		t.Fatalf("only %d flags were found in the documentation — that is fewer than this command has, "+
			"so the regex is not reading the help", seen)
	}
}

// ADR 0052 §1's acceptance criterion: "`kontra up --stack kontra-fleet/dns` (or any non-control
// project) is refused."
//
// AND THE REFUSAL NAMES THE OTHER ENGINE. If the host table could name `kontra-fleet/dns`, this
// command would be a path to cloud Machines with no Lease, no `checkCloudCredential`, no Temporal
// record, no mutex and no audit — so the message has to say where that request does belong, not merely
// that this command does not want it.
func TestControlUpRefusesAFleetStack(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KONTRA_HOME", home)
	for _, stack := range []string{"kontra-fleet/dns", "kontra-docker-fleet/local"} {
		err := dispatch([]string{"control", "up", "--stack", stack})
		if err == nil {
			t.Fatalf("--stack %s was accepted", stack)
		}
		for _, want := range []string{"kontra fleet", "Lease", hostengine.Project} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("--stack %s: the refusal must name the other engine; %q missing from:\n%v", stack, want, err)
			}
		}
	}
	// THE ORDER IS PART OF THE CONTRACT: the dispatch table is consulted before anything exists on
	// disk, so a request this engine must not serve does not leave a state directory behind.
	for _, dir := range []string{hostengine.StateDirName, "engine"} {
		if _, err := os.Stat(filepath.Join(home, dir)); err == nil {
			t.Errorf("a refused stack still created %s — the table must be consulted first", dir)
		}
	}
}

// The install refuses positional arguments and `cli/up_test.go:44` asserts the refusal QUOTES the
// argument. The same rule here, for the same reason: `kontra control up local` looks like it names a
// stack and does not.
func TestControlUpRefusesPositionalArguments(t *testing.T) {
	err := dispatch([]string{"control", "up", "local"})
	if err == nil {
		t.Fatal("a positional argument was accepted")
	}
	if !strings.Contains(err.Error(), `"local"`) {
		t.Errorf("the refusal must quote what was typed: %v", err)
	}
	if !strings.Contains(err.Error(), "--stack") {
		t.Errorf("and it must name the flag that does what they meant: %v", err)
	}
}

// `--json` describes a preview. A converge streams pulumi's own output for minutes and has no document
// to emit at the end of it, and a flag that silently did nothing would be read as one that worked.
func TestControlUpRefusesJSONWithoutPreview(t *testing.T) {
	err := dispatch([]string{"control", "up", "--json"})
	if err == nil {
		t.Fatal("--json without --preview was accepted")
	}
	if !strings.Contains(err.Error(), "--preview") {
		t.Errorf("the refusal must name the flag it belongs with: %v", err)
	}
}

// The flag set is shared, so `--preview` PARSES on `down`. It must not be ignored: a
// `kontra control down --preview` that destroyed the stack because the flag meant nothing to that verb
// is the worst thing in this file to have got wrong.
func TestControlDownRefusesTheFlagsThatBelongToUp(t *testing.T) {
	for _, arg := range []string{"--preview", "--json", "--bind=0.0.0.0", "--api-port=9000"} {
		err := dispatch([]string{"control", "down", arg})
		if err == nil {
			t.Fatalf("`kontra control down %s` was accepted", arg)
		}
		if !strings.Contains(err.Error(), "kontra control up") {
			t.Errorf("%s: the refusal must name the verb the flag belongs to: %v", arg, err)
		}
	}
}

// THE BACKEND IS ASSERTED BEFORE ANY OPERATION, and this asserts it through the command rather than
// only in the engine's own tests: `pulumi` is never looked up, nothing is materialised, and no state
// directory appears.
//
// The trap, in `workspace.ts:16-20`'s words: "A failed `pulumi login` does not stop the CLI — it
// silently creates an ephemeral Pulumi Cloud account and deploys THERE, state included." On a laptop
// an ambient PULUMI_ACCESS_TOKEN makes that the likely case rather than the unlucky one, which is why
// ADR 0052 fact 4 asks for the host's message to be the louder of the two.
func TestControlUpRefusesAnEnvironmentThatCanReachPulumiCloud(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KONTRA_HOME", home)
	t.Setenv("HOME", home)
	t.Setenv("PULUMI_ACCESS_TOKEN", "pul-deadbeef")
	t.Setenv("PULUMI_BACKEND_URL", "")

	err := dispatch([]string{"control", "up", "--preview"})
	if err == nil {
		t.Fatal("an environment configured for Pulumi Cloud was allowed to converge")
	}
	for _, want := range []string{"Pulumi Cloud", "state included"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must name the trap; %q missing from:\n%v", want, err)
		}
	}
	if _, err := os.Stat(filepath.Join(home, hostengine.StateDirName)); err == nil {
		t.Error("the state directory was created before the backend was asserted")
	}
	if _, err := os.Stat(filepath.Join(home, "engine", hostengine.ProgramFile)); err == nil {
		t.Error("the program was materialised before the backend was asserted")
	}
}

// An exported hosted backend URL is the one setting nothing this command does can undo — measured on
// 3.244.0, it beats a later `pulumi login` and a set token both.
func TestControlUpRefusesAnExportedHostedBackend(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KONTRA_HOME", home)
	t.Setenv("HOME", home)
	t.Setenv("PULUMI_BACKEND_URL", "https://api.pulumi.com")
	err := dispatch([]string{"control", "up", "--preview"})
	if err == nil {
		t.Fatal("an exported hosted backend was accepted")
	}
	if !strings.Contains(err.Error(), "unset PULUMI_BACKEND_URL") {
		t.Errorf("the refusal must carry the remedy: %v", err)
	}
}

// `--preview`'s exit code is its whole promise to CI (issue 04: "Exit code distinguishes 'would
// change' from 'no change', so CI can gate on it"). It is carried by a sentinel so that a future third
// exit code is one arm added to `main`'s fork at `cli/main.go:282-289` rather than a rewrite of this
// command — and so that "would change" is distinguishable in Go from "the preview broke", which shares
// its exit status today the way `kontra workflow replay` shares its two.
func TestWouldChangeIsASentinelAndNotJustAMessage(t *testing.T) {
	if !errors.Is(errWouldChange, errWouldChange) {
		t.Fatal("errWouldChange is not usable with errors.Is")
	}
	if errors.Is(errWouldChange, errUsage) {
		t.Fatal("errWouldChange must not be errUsage — `main` exits 2 on that and prints the whole usage, " +
			"which would tell CI the command does not exist")
	}
}
