package main

// advice_test.go — A COMMAND THIS BINARY TELLS SOMEBODY TO TYPE IS A COMMAND THAT PARSES.
//
// This replaces two sweeps that read text and compared it with a list. `retired_words_test.go`
// scanned every Go string literal for `campaign`, and `fleet_documented_flags_test.go` regexed
// `--flags` out of the README, the wiki and `fleet.go` and checked each against `fleetFlagSet`.
// What they guarded was real — `kontra fleet` answered a 409 with `kontra fleet status --campaign
// <x>`, a remedy that refused to parse — but the way to check advice is to TAKE it: hand the
// command to `dispatch` and see whether the CLI accepts it. A renamed flag, a verb that went, a
// subcommand nobody routes: each is a refusal here, with no pattern in between that can stop
// matching and no list of words beside the code to keep current.
//
// The markdown half of those sweeps is not carried over. Prose is not something this binary runs,
// and a README that drifts is a docs lint (PRD §8, D10), not a behaviour of `kontra`.

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// probeable is every command a probe may hand to `dispatch`, and it is a SAFETY list, not a truth
// list. A probe appends `-h`, which is harmless only to a command that parses its flags BEFORE it
// acts — and on a box with an install, `kontra down` destroys its containers and `kontra fleet down`
// its Machines. Each entry below was read to parse first (build.go, deploy.go, serve.go, runs.go,
// fleet.go and lease.go, workflow.go's serve and start). A new verb joins only after the same read.
var probeable = map[string]bool{
	"build": true, "deploy": true, "serve": true, "runs": true,
	"fleet up": true, "fleet deploy": true, "fleet preview": true,
	"fleet status": true, "fleet leases": true, "fleet down": true,
	"workflow serve": true, "workflow start": true,
}

// adviceIn returns every `kontra …` command in `msg`, as argv without the leading `kontra`: each
// backticked span that starts with it and each line that does. `[optional]` brackets are dropped,
// a trailing `# comment` is cut, and `serve|start` in the subcommand slot becomes two commands.
func adviceIn(msg string) [][]string {
	var spans []string
	for i, part := range strings.Split(msg, "`") {
		if i%2 == 1 && strings.HasPrefix(part, "kontra ") {
			spans = append(spans, part)
		}
	}
	for _, line := range strings.Split(msg, "\n") {
		if l := strings.TrimSpace(line); strings.HasPrefix(l, "kontra ") {
			spans = append(spans, l)
		}
	}
	var out [][]string
	for _, s := range spans {
		s, _, _ = strings.Cut(s, " #")
		s = strings.NewReplacer("[", " ", "]", " ").Replace(s)
		f := strings.Fields(s)[1:]
		if len(f) > 1 && !strings.HasPrefix(f[1], "-") && strings.Contains(f[1], "|") {
			for _, alt := range strings.Split(f[1], "|") {
				out = append(out, append([]string{f[0], alt}, f[2:]...))
			}
			continue
		}
		out = append(out, f)
	}
	return out
}

// adviceRuns hands `argv` to `dispatch` and reports whether the CLI ACCEPTED it: routed the verb,
// knew every flag, took every value. nil means yes.
//
// IT CANNOT RUN ANYTHING, by construction rather than by care. Every `--flag value` pair is joined
// into `--flag=value` first, so the only tokens left after the verb are flags — Go's flag parser
// stops at the first bare word, and a `-h` behind one would never be read and the command would
// run. A bare word with no flag before it is refused here instead. `-h` then goes last, TWICE:
// advice may elide a value (`kontra serve --actor`), and a string flag with no `=` takes the next
// argument whatever it looks like, so the first `-h` can be eaten as `--actor`'s value. The second
// cannot. A command that knows every flag answers `flag.ErrHelp` and returns; one that does not
// answers the parse error, also without acting.
func adviceRuns(t *testing.T, argv []string) error {
	t.Helper()
	if len(argv) == 0 {
		return errors.New("empty command")
	}
	verb, rest := argv[:1], argv[1:]
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		verb, rest = argv[:2], argv[2:]
	}
	key := strings.Join(verb, " ")
	if !probeable[key] {
		return fmt.Errorf("`kontra %s` is not in `probeable`: read that its command parses before it "+
			"acts, then add it", key)
	}
	var canon []string
	for i := 0; i < len(rest); i++ {
		tok := rest[i]
		if tok == "-" || tok == "--" || !strings.HasPrefix(tok, "-") {
			return fmt.Errorf("`kontra %s`: %q is a bare word, and a probe cannot prove a command with "+
				"positional arguments parses", strings.Join(argv, " "), tok)
		}
		if !strings.Contains(tok, "=") && i+1 < len(rest) && !strings.HasPrefix(rest[i+1], "-") {
			tok, i = tok+"="+rest[i+1], i+1
		}
		canon = append(canon, tok)
	}

	// BELT AND BRACES: should a command above stop parsing first, the orchestrator it would call is
	// the discard port on loopback rather than the install's.
	t.Setenv("KONTRA_ORCHESTRATOR_URL", "http://127.0.0.1:9")
	null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	defer swap(&os.Stderr, null)() // `-h` prints the flag set's defaults; nobody is reading them

	got := dispatch(append(append(append([]string{}, verb...), canon...), "-h", "-h"))
	if errors.Is(got, flag.ErrHelp) {
		return nil
	}
	return fmt.Errorf("`kontra %s` does not parse: %v", strings.Join(argv, " "), got)
}

func TestTheProbeRefusesWhatTheCLIRefuses(t *testing.T) {
	// THE GUARD ON THE GUARD. The exact remedy `kontra fleet` once printed on a 409 must fail.
	for _, dead := range [][]string{
		{"fleet", "status", "--campaign", "c1"},
		{"fleet", "up", "--count", "two"},
		{"build", "--json", "maybe"},
	} {
		if adviceRuns(t, dead) == nil {
			t.Errorf("`kontra %s` was accepted by the probe, which would then accept any advice",
				strings.Join(dead, " "))
		}
	}
	// …and refuses to guess at a positional, which would put `-h` where the parser never reads it.
	if err := adviceRuns(t, []string{"fleet", "up", "takes", "NO", "Lease"}); err == nil ||
		!strings.Contains(err.Error(), "bare word") {
		t.Errorf("a command with a positional must be refused before dispatch, got %v", err)
	}
	// …and the live case does pass, so the refusals above are about the flags.
	if err := adviceRuns(t, []string{"fleet", "status", "--fleet", "dns"}); err != nil {
		t.Errorf("a command that parses was refused: %v", err)
	}
}

func TestAConvergeConflictAdvisesACommandThatParses(t *testing.T) {
	// `--fleet`, NOT `--campaign`: the one remedy a 409 offers has to be a command the CLI accepts,
	// about THIS Fleet.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "a converge is already running", http.StatusConflict)
	}))
	defer srv.Close()
	_, err := startOp(newAPI(srv.URL), fleetProject+"/dns", "up", map[string]any{})
	if err == nil {
		t.Fatal("a 409 from the converge route was not an error")
	}
	advice := adviceIn(err.Error())
	if len(advice) == 0 {
		t.Fatalf("the 409 offers no command at all: %v", err)
	}
	for _, argv := range advice {
		if e := adviceRuns(t, argv); e != nil {
			t.Errorf("the 409's remedy does not run: %v", e)
		}
		f := fleetFlagSet(argv[1])
		f.fs.SetOutput(io.Discard)
		if e := f.fs.Parse(argv[2:]); e != nil || *f.fleet != "dns" {
			t.Errorf("the 409's remedy is not about the Fleet that conflicted: --fleet=%q (%v)", *f.fleet, e)
		}
	}
}

func TestEveryCommandFleetHelpShowsParses(t *testing.T) {
	help := captureStdout(t, fleetUsage)
	// THE COMMAND LINES, NOT THE PROSE. The help indents what you type by two spaces and writes its
	// explanation flush left — `kontra fleet up takes NO Lease` is a sentence, not a command.
	var lines []string
	for _, line := range strings.Split(help, "\n") {
		if strings.HasPrefix(line, "  kontra fleet ") {
			lines = append(lines, line)
		}
	}
	// NON-VACUOUS: one per subcommand the help lists. Zero would pass everything below.
	if len(lines) < 6 {
		t.Fatalf("only %d command lines in `kontra fleet help` — the extraction is broken:\n%s", len(lines), help)
	}
	for _, argv := range adviceIn(strings.Join(lines, "\n")) {
		err := adviceRuns(t, argv)
		if why, known := knownDeadFleetHelp[strings.Join(argv, " ")]; known {
			// LISTED, NOT SUPPRESSED: it must still fail, or the entry is excusing a command that
			// works now and has to go.
			if err == nil {
				t.Errorf("`kontra %s` parses now — delete it from knownDeadFleetHelp", strings.Join(argv, " "))
			} else {
				t.Logf("known dead, and still dead: %v (%s)", err, why)
			}
			continue
		}
		if err != nil {
			t.Errorf("`kontra fleet help` shows a command that does not parse: %v", err)
		}
	}
}

// knownDeadFleetHelp is advice the help prints today that the CLI refuses, FOUND BY THE TEST ABOVE
// on its first run. `--image` is in `fleetUsage`, `usageText` and the wiki's CLI reference, and
// `mcp_fleet_db.go` passes it to `fleetDeployCmd` when an agent names an image — but no version of
// `fleet.go` in this repository's history registers it. MEASURED on the MCP path: `mcpFleetDeploy`
// with an image returns `flag provided but not defined: -image`. The regex sweep this file replaces
// could not see it: it stopped matching at the `[` in `[--image <ref>]`.
//
// Whether `--image` should exist or stop being advertised is the owner's call — PRD D3 replaces
// `fleet deploy` with a Deployment by digest — so it is recorded here rather than decided.
var knownDeadFleetHelp = map[string]string{
	"fleet deploy --actor <dir> --image <ref>": "advertised, never registered",
}
