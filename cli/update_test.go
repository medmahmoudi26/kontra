package main

import (
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/medmahmoudi26/kontra/cli/internal/hostengine"
)

// A NEW WORD WIRED TO NOTHING is a switch-statement bug, and `dispatch`'s own header says it was split
// out of `main` so that exact mistake could be tested. `update` is a new word.
//
// IT IS PROVEN THROUGH `help` AND THROUGH A REFUSAL, never through a bare `kontra update`, because a
// bare one converges 13 containers on the machine running the test suite.
func TestUpdateIsARoutedCommand(t *testing.T) {
	var err error
	out := captureStdout(t, func() { err = dispatch([]string{"update", "help"}) })
	if errors.Is(err, errUsage) {
		t.Fatal("`kontra update` fell through to the unknown-command branch, so it is not wired up")
	}
	if err != nil {
		t.Fatalf("`kontra update help` should print and succeed: %v", err)
	}
	for _, want := range []string{"kontra update", "--check", "--to", "ADR 0052 §7"} {
		if !strings.Contains(out, want) {
			t.Errorf("the usage it printed does not mention %q", want)
		}
	}
}

// `cli/up.go:68-70`'s rule, pinned by `cli/up_test.go:44`: the refusal QUOTES the argument. The likely
// mistake here is `kontra update v1.4.0`, which reads like it names a release.
func TestUpdateRefusesPositionalArgumentsAndNamesTheFlag(t *testing.T) {
	err := dispatch([]string{"update", "v1.4.0"})
	if err == nil {
		t.Fatal("a positional argument was accepted")
	}
	if errors.Is(err, errUsage) {
		t.Fatal("that refusal came from the unknown-command branch, not from the command")
	}
	for _, want := range []string{`"v1.4.0"`, "--to"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q missing from the refusal: %v", want, err)
		}
	}
}

// Help is the one surface nothing else checks — see `usageText`'s own comment.
func TestUpdateIsDocumented(t *testing.T) {
	for _, want := range []string{
		"kontra update", "--check", "--to",
		"protect: true",    // the guarantee, named where somebody will read it
		"stack export",     // the export, and that it happens before every converge
		"SCHEMA MIGRATION", // §7's out-of-scope, stated rather than left to be discovered
		"same service",     // the identity assertion, not "no destructive operations"
		"ADR 0052 §7",      // where the reasoning lives
	} {
		if !strings.Contains(usageText, want) {
			t.Errorf("usageText does not advertise %q", want)
		}
	}
	// And it says it is the SAME converge, because this binary now documents three ways to bring a
	// control plane up and the reason `kontra update` is safe is that it is not a fourth.
	if !strings.Contains(usageText, "SAME converge") {
		t.Error("usageText does not say that `kontra update` is the same converge as `kontra control up`, " +
			"which is the property ADR 0052 §7 rests on")
	}
}

// EVERY FLAG THE HELP TELLS SOMEBODY TO TYPE EXISTS — `control_test.go:75`'s sweep, for this command's
// own block. `advice_test.go` covers the fleet family only, so each new command needs its own.
func TestEveryDocumentedUpdateFlagIsRegistered(t *testing.T) {
	registered := map[string]bool{}
	updateFlagSetForTest().VisitAll(func(f *flag.Flag) { registered[f.Name] = true })
	if len(registered) == 0 {
		t.Fatal("no flags are registered at all, so this sweep would pass on any prose")
	}
	var prose []string
	inBlock := false
	for _, line := range strings.Split(usageText, "\n") {
		// THE ENTRY LINE, NOT THE WORDS ANYWHERE. `control_test.go`'s copy of this loop used
		// `strings.Contains` and this command's own help broke it: the `kontra update` block says
		// "kontra control up" in prose (it is the same converge), which re-entered control's block and
		// swept --check and --to into it. Both copies now require the line to BE the entry.
		if strings.HasPrefix(strings.TrimLeft(line, " "), "kontra update") {
			inBlock, prose = true, append(prose, line)
			continue
		}
		if !inBlock {
			continue
		}
		if tr := strings.TrimLeft(line, " "); tr == "" || strings.HasPrefix(tr, "kontra ") {
			inBlock = false
			continue
		}
		prose = append(prose, line)
	}
	prose = append(prose, captureStdout(t, updateUsage))
	if len(prose) < 4 {
		t.Fatalf("only %d lines of `kontra update` help were found — the extraction is broken and the "+
			"assertion below proves nothing", len(prose))
	}
	seen := 0
	for _, line := range prose {
		for _, g := range regexp.MustCompile(`--([a-z][a-z0-9-]*)`).FindAllStringSubmatch(line, -1) {
			seen++
			if !registered[g[1]] {
				t.Errorf("the help tells somebody to type --%s and `update` does not register it:\n  %s",
					g[1], strings.TrimSpace(line))
			}
		}
	}
	if seen < 4 {
		t.Fatalf("only %d flags were found in the documentation — fewer than this command has, so the "+
			"regex is not reading the help", seen)
	}
}

// updateFlagSetForTest re-registers what cmdUpdate registers.
//
// A SECOND REGISTRATION IS A THING THAT CAN DRIFT, and it is here anyway because the alternative is
// worse: `cmdUpdate`'s FlagSet is a local, and lifting it to a struct the way `fleet.go:124` does would
// be the right shape for a multi-verb command and is not the right shape for a flat one (`cli/up.go`
// registers 16 flags the same way this does). The drift is guarded from the other side — the sweep above
// fails if the HELP names a flag this list does not have, and `TestUpdateRejectsAnUnknownFlag` fails if
// the real command does not have one this list does.
func updateFlagSetForTest() *flag.FlagSet {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.Bool("check", false, "")
	fs.String("to", "", "")
	fs.String("stack", "", "")
	fs.String("program", "", "")
	fs.String("workspaces", "", "")
	return fs
}

// The other half of updateFlagSetForTest's guard: every flag that list claims is really registered on
// the command, proven by the command ACCEPTING it. A flag `flag` does not know about is an error from
// Parse, which is distinguishable from everything that happens later.
func TestUpdateRejectsAnUnknownFlag(t *testing.T) {
	err := dispatch([]string{"update", "--nonexistent-flag"})
	if err == nil || !strings.Contains(err.Error(), "nonexistent-flag") {
		t.Fatalf("an unknown flag must be refused by name: %v", err)
	}
	updateFlagSetForTest().VisitAll(func(f *flag.Flag) {
		// Parsing stops at the first error, so each flag is offered on its own with a value that cannot
		// be mistaken for the next argument.
		arg := "--" + f.Name
		if _, isBool := f.Value.(interface{ IsBoolFlag() bool }); !isBool {
			arg += "=x"
		}
		err := dispatch([]string{"update", arg, "--nonexistent-flag"})
		if err == nil || strings.Contains(err.Error(), "flag provided but not defined: -"+f.Name) {
			t.Errorf("--%s is documented and the command does not register it: %v", f.Name, err)
		}
	})
}

// `--to` WITH NOTHING AFTER IT would retag every image to `:`, which is a reference the daemon will
// answer for and nobody meant.
func TestUpdateRefusesAnEmptyTo(t *testing.T) {
	err := dispatch([]string{"update", "--to", ""})
	if err == nil {
		t.Fatal("--to '' was accepted")
	}
	if !strings.Contains(err.Error(), "--to needs a tag") {
		t.Errorf("the refusal must say what is missing: %v", err)
	}
}

// --- THERE IS ONE CONVERGE, AND THIS IS THE TEST THAT KEEPS IT THAT WAY --------------------------

// ADR 0052 §7: `kontra update` "is not a second code path". The acceptance criterion is "The update
// path shares the converge with `kontra up` — no second code path to drift", and the only way to assert
// that is structurally: everything that makes a converge SAFE — the lock, the state export, the volume
// gate, the apply — must appear exactly once in this package, in one function, which both commands call.
//
// SWEPT OUT OF THE SOURCE, the way `table_test.go` extracts the infra engine's projects from
// `stacks.ts` rather than re-stating them in Go. A Go-side assertion about Go-side structure is the one
// case where reading the source is not a hack: the property under test IS the shape of the code.
func TestThereIsExactlyOneConverge(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) < 10 {
		t.Fatalf("the sweep found %d files in this package, so it is not reading anything: %v", len(files), err)
	}
	counts := map[string]map[string]int{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		for _, needle := range []string{
			".Up()",                  // the apply
			"hostengine.Lock(",       // the mutex
			"hostengine.SaveExport(", // the state export ADR 0052 §7 requires before every converge
			"CheckVolumeIdentity(",   // the identity gate
			"CheckPlanKeepsEveryVolume(",
			"func converge(",
		} {
			if n := strings.Count(src, needle); n > 0 {
				if counts[needle] == nil {
					counts[needle] = map[string]int{}
				}
				counts[needle][f] = n
			}
		}
	}
	for _, needle := range []string{".Up()", "hostengine.SaveExport(", "CheckVolumeIdentity(", "func converge("} {
		where := counts[needle]
		if len(where) == 0 {
			t.Errorf("%q appears nowhere in this package — the sweep is broken, or the converge lost a step "+
				"that ADR 0052 §7 requires", needle)
			continue
		}
		total := 0
		for _, n := range where {
			total += n
		}
		if total != 1 || where["control.go"] != 1 {
			t.Errorf("%q appears %d time(s), at %v. It must appear exactly once, in control.go's `converge` — "+
				"a second one is `kontra update` drifting away from `kontra control up`, and the way that "+
				"shows up in production is an update that skipped the volume gate", needle, total, where)
		}
	}
	// CheckPlanKeepsEveryVolume is called twice in converge on purpose — once on the plan pulumi refused
	// to make and once on the plan it did make — and still only from that one file.
	if where := counts["CheckPlanKeepsEveryVolume("]; len(where) != 1 || where["control.go"] == 0 {
		t.Errorf("the plan check is called from %v, want control.go only", where)
	}
	// hostengine.Lock appears twice in control.go: the converge and `control down`. Anywhere else is a
	// command taking the converge lock without going through the converge.
	if where := counts["hostengine.Lock("]; len(where) != 1 || where["control.go"] != 2 {
		t.Errorf("hostengine.Lock is taken at %v, want two sites in control.go (converge and down)", where)
	}
	// And `update.go` must reach it through convergeOpts, or the assertions above pass while the two
	// commands share nothing.
	b, err := os.ReadFile("update.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "converge(convergeOpts{") {
		t.Error("update.go does not call `converge`, so `kontra update` is not the same converge")
	}
}

// --- end to end, against a stub pulumi ----------------------------------------------------------

// stubPulumi writes a `pulumi` that answers the five calls a converge makes and records its argv.
//
// A STUB RATHER THAN THE REAL BINARY, and here the reason is stronger than in
// `hostengine/engine_test.go`: what is under test is the ORDER — that the export is written and the
// volume gate runs BEFORE anything is applied — and the real converge would need 13 container names
// that are taken on any machine running the compose stack. The real binary is exercised separately,
// by hand, against a scratch `file://` backend.
func stubPulumi(t *testing.T, exportJSON, previewJSON string) (bin, log string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the stub is a /bin/sh script; this CLI releases for linux and macOS (ADR 0031 §2)")
	}
	dir := t.TempDir()
	bin, log = filepath.Join(dir, "pulumi"), filepath.Join(dir, "argv.log")
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	exp, prev := write("export.json", exportJSON), write("preview.json", previewJSON)
	script := "#!/bin/sh\necho \"$@\" >> " + log + "\ncase \"$*\" in\n" +
		"  *'stack export'*) cat " + exp + " ;;\n" +
		"  *preview*) cat " + prev + " ;;\n" +
		"  *'stack output'*) echo http://127.0.0.1:8088 ;;\n" +
		"esac\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return bin, log
}

// realExport builds the `pulumi stack export` an installation of the COMMITTED program would have.
//
// DERIVED FROM THE PROGRAM, so the passing case below is the real program's eleven volumes and the
// refusals are each one mutation away from it. A hand-written fixture would be eleven names this file
// asserts against itself.
func realExport(t *testing.T, mutate func(name string, v *hostengine.Volume)) string {
	t.Helper()
	prog, err := os.ReadFile(filepath.Join("..", "control", "pulumi", "Pulumi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	declared, err := hostengine.DeclaredVolumes(prog)
	if err != nil {
		t.Fatal(err)
	}
	type res map[string]any
	out := []res{{"urn": "urn:pulumi:local::kontra-control::pulumi:pulumi:Stack::kontra-control-local",
		"type": "pulumi:pulumi:Stack", "custom": false}}
	mounts := map[string][]map[string]any{}
	for _, name := range declared.Names() {
		v := declared.ByName[name]
		if mutate != nil {
			mutate(name, &v)
		}
		r := res{
			"urn":     "urn:pulumi:local::kontra-control::docker:index/volume:Volume::" + v.Logical,
			"type":    "docker:index/volume:Volume",
			"custom":  true,
			"inputs":  map[string]any{"name": v.Name},
			"outputs": map[string]any{"name": v.Name, "id": v.Name},
		}
		if v.Protected {
			r["protect"] = true
		}
		out = append(out, r)
		for _, s := range v.Services {
			mounts[s] = append(mounts[s], map[string]any{"volumeName": v.Name, "containerPath": "/data"})
		}
	}
	for svc, mm := range mounts {
		out = append(out, res{
			"urn":     "urn:pulumi:local::kontra-control::docker:index/container:Container::" + svc,
			"type":    "docker:index/container:Container",
			"custom":  true,
			"inputs":  map[string]any{"name": svc, "volumes": mm},
			"outputs": map[string]any{"name": svc},
		})
	}
	b, err := json.Marshal(map[string]any{"version": 3, "deployment": map[string]any{"resources": out}})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// stubHome points this process at a throwaway installation and a stub `pulumi`.
func stubHome(t *testing.T, exportJSON, previewJSON string) (home, log string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("KONTRA_HOME", home)
	t.Setenv("HOME", home)
	t.Setenv("PULUMI_BACKEND_URL", "")
	t.Setenv("PULUMI_ACCESS_TOKEN", "")
	t.Setenv("KONTRA_WORKSPACES", filepath.Join(home, "workspaces"))
	bin, log := stubPulumi(t, exportJSON, previewJSON)
	t.Cleanup(swap(&hostengine.Bin, bin))
	return home, log
}

const noChangePreview = `{"steps":[
  {"op":"same","urn":"urn:pulumi:local::kontra-control::docker:index/container:Container::postgres",
   "newState":{"type":"docker:index/container:Container"}}],
  "changeSummary":{"same":39}}`

// An update that replaces containers and touches no volume — which is what an update IS.
const containerReplacePreview = `{"steps":[
  {"op":"replace","urn":"urn:pulumi:local::kontra-control::docker:index/container:Container::temporal",
   "newState":{"type":"docker:index/container:Container"},"oldState":{"type":"docker:index/container:Container"}},
  {"op":"same","urn":"urn:pulumi:local::kontra-control::docker:index/volume:Volume::volSeaweedData",
   "newState":{"type":"docker:index/volume:Volume"}}],
  "changeSummary":{"same":38,"replace":1}}`

// AC: "`--check` exits non-zero when an update is available and zero when it is not, so CI can gate."
func TestUpdateCheckExitsZeroOnlyWhenNothingWouldChange(t *testing.T) {
	t.Run("no change", func(t *testing.T) {
		stubHome(t, realExport(t, nil), noChangePreview)
		var err error
		captureStdout(t, func() { err = dispatch([]string{"update", "--check"}) })
		if err != nil {
			t.Fatalf("`--check` against an up-to-date install must exit 0: %v", err)
		}
	})
	t.Run("would change", func(t *testing.T) {
		stubHome(t, realExport(t, nil), containerReplacePreview)
		var err error
		out := captureStdout(t, func() { err = dispatch([]string{"update", "--check"}) })
		if err == nil {
			t.Fatal("`--check` reported success for a converge that would replace a container")
		}
		if !errors.Is(err, errWouldChange) {
			t.Errorf("the error must be errWouldChange so `main`'s fork can tell it from a broken preview: %v", err)
		}
		if !strings.Contains(out, "temporal") {
			t.Errorf("a replacement must be NAMED — ADR 0052 §2's own example is \"this will replace "+
				"`temporal`\":\n%s", out)
		}
	})
}

// `--check` CHANGES NOTHING. No lock, no state export, no apply — and the file system is where that is
// proven, because a `--check` that wrote is a `--check` somebody will stop trusting in CI.
func TestUpdateCheckWritesNothingAndDoesNotApply(t *testing.T) {
	home, log := stubHome(t, realExport(t, nil), noChangePreview)
	captureStdout(t, func() {
		if err := dispatch([]string{"update", "--check"}); err != nil {
			t.Fatal(err)
		}
	})
	if _, err := os.Stat(filepath.Join(home, "engine", hostengine.ExportDirName)); err == nil {
		t.Error("`--check` wrote a state export")
	}
	if _, err := os.Stat(filepath.Join(home, "engine", hostengine.LockFileName)); err == nil {
		t.Error("`--check` left the converge lock behind")
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{" up --yes", "destroy"} {
		if strings.Contains(string(b), forbidden) {
			t.Errorf("`--check` ran `pulumi%s`:\n%s", forbidden, b)
		}
	}
	if !strings.Contains(string(b), "--refresh") {
		t.Errorf("`--check` must still --refresh, or it compares the images against the checkpoint "+
			"instead of against this machine:\n%s", b)
	}
}

// AC: "`pulumi-state` is exported before every converge and the path is printed."
//
// BEFORE, AND THE ORDER IS ASSERTED FROM THE ARGV LOG. An export written after the apply is a copy of
// the state the apply produced, which is not what §7 asked for: the point is having the record of what
// was there if the converge goes wrong.
func TestUpdateExportsTheStateBeforeItConverges(t *testing.T) {
	home, log := stubHome(t, realExport(t, nil), containerReplacePreview)
	out := captureStdout(t, func() {
		if err := dispatch([]string{"update"}); err != nil {
			t.Fatal(err)
		}
	})
	dir := filepath.Join(home, "engine", hostengine.ExportDirName)
	ents, err := os.ReadDir(dir)
	if err != nil || len(ents) != 1 {
		t.Fatalf("want exactly one state export in %s, got %v (%v)", dir, ents, err)
	}
	path := filepath.Join(dir, ents[0].Name())
	if !strings.Contains(out, path) {
		t.Errorf("the export path must be PRINTED — it is what re-adopts eleven docker volumes if this "+
			"installation's state is ever lost. stdout was:\n%s", out)
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	argv := string(b)
	expIdx, upIdx := strings.Index(argv, "stack export"), strings.Index(argv, "up --yes")
	if expIdx < 0 || upIdx < 0 {
		t.Fatalf("both calls should have happened:\n%s", argv)
	}
	if expIdx > upIdx {
		t.Errorf("the state was exported AFTER the converge, which copies the result instead of the "+
			"record:\n%s", argv)
	}
	// And it reports what it replaced and what it left alone (§7: "It reports what it replaced and what
	// it left alone").
	for _, want := range []string{"REPLACED", "temporal", "LEFT ALONE", "kontra_seaweed-data"} {
		if !strings.Contains(out, want) {
			t.Errorf("%q missing from the report:\n%s", want, out)
		}
	}
}

// AC: refuses on any planned volume delete OR replace, NAMING THE VOLUME, before applying.
func TestUpdateRefusesAPlannedVolumeDeleteAndNamesIt(t *testing.T) {
	const deletesAVolume = `{"steps":[
	  {"op":"delete","urn":"urn:pulumi:local::kontra-control::docker:index/volume:Volume::volSeaweedData",
	   "oldState":{"type":"docker:index/volume:Volume"}}],
	  "changeSummary":{"same":38,"delete":1}}`
	_, log := stubHome(t, realExport(t, nil), deletesAVolume)
	var err error
	captureStdout(t, func() { err = dispatch([]string{"update"}) })
	if err == nil {
		t.Fatal("a plan that deletes a volume was applied")
	}
	for _, want := range []string{"kontra_seaweed-data", "docker volume rm", "Nothing has been applied"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q missing from the refusal:\n%v", want, err)
		}
	}
	if b, _ := os.ReadFile(log); strings.Contains(string(b), "up --yes") {
		t.Errorf("the converge ran anyway:\n%s", b)
	}
}

// AC: "A converge that would create a same-purpose volume under a NEW name is refused, with the old name
// and the new one both printed" — and the plan for that contains NO DELETE, which is why the gate reads
// the export instead of the plan.
func TestUpdateRefusesARenamedVolumeWithNoDeleteInThePlan(t *testing.T) {
	// The installation has `kontra_seaweed-data-v1`; the committed program declares
	// `kontra_seaweed-data`. Nothing in the plan is destructive: the old volume is simply never
	// mentioned again.
	export := realExport(t, func(name string, v *hostengine.Volume) {
		if name == "kontra_seaweed-data" {
			v.Name = "kontra_seaweed-data-v1"
		}
	})
	_, log := stubHome(t, export, noChangePreview)
	var err error
	captureStdout(t, func() { err = dispatch([]string{"update"}) })
	if err == nil {
		t.Fatal("a renamed volume was accepted — the stack comes up healthy and EMPTY, and nothing raises")
	}
	for _, want := range []string{"kontra_seaweed-data-v1", "kontra_seaweed-data", "no delete at all"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q missing from the refusal:\n%v", want, err)
		}
	}
	if b, _ := os.ReadFile(log); strings.Contains(string(b), "up --yes") {
		t.Errorf("the converge ran anyway:\n%s", b)
	}
}

// The identity gate's third finding, and the only one with no delete AND no new name: the volume is
// still there, still called what it was, and something else mounts it.
func TestUpdateRefusesAVolumeThatMovedService(t *testing.T) {
	export := realExport(t, func(name string, v *hostengine.Volume) {
		if name == "kontra_pulumi-state" {
			v.Services = []string{"kontra-api"}
		}
	})
	stubHome(t, export, noChangePreview)
	var err error
	captureStdout(t, func() { err = dispatch([]string{"update"}) })
	if err == nil {
		t.Fatal("a volume whose mount moved was accepted")
	}
	if !strings.Contains(err.Error(), "kontra_pulumi-state") || !strings.Contains(err.Error(), "kontra-infra") {
		t.Errorf("the refusal must name the volume and both services:\n%v", err)
	}
}

// AN UPDATE OF NOTHING IS AN INSTALL. 40 resources created and described as an upgrade is the one
// sentence that would hide it.
func TestUpdateRefusesAnInstallationThatHasNeverConverged(t *testing.T) {
	const never = `{"version":3,"deployment":{"manifest":{"version":"v3.244.0"},"metadata":{}}}`
	_, log := stubHome(t, never, noChangePreview)
	var err error
	captureStdout(t, func() { err = dispatch([]string{"update"}) })
	if err == nil {
		t.Fatal("`kontra update` on a stack that has never converged was accepted")
	}
	if !strings.Contains(err.Error(), "kontra control up") {
		t.Errorf("the refusal must name the command that DOES install: %v", err)
	}
	if b, _ := os.ReadFile(log); strings.Contains(string(b), "up --yes") {
		t.Errorf("it converged anyway:\n%s", b)
	}
}

// `--to` MOVES EVERY KONTRA-OWNED IMAGE AND SENDS THEM AS CONFIG, and the keys come out of the program
// (`hostengine.ImageRefs`) so a sixth image added there is retagged without this command changing.
func TestUpdateToRetagsEveryImageAndSendsThem(t *testing.T) {
	_, log := stubHome(t, realExport(t, nil), containerReplacePreview)
	captureStdout(t, func() {
		if err := dispatch([]string{"update", "--to", "v1.4.0"}); err != nil {
			t.Fatal(err)
		}
	})
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	argv := string(b)
	prog, err := os.ReadFile(filepath.Join("..", "control", "pulumi", "Pulumi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	refs, err := hostengine.ImageRefs(prog)
	if err != nil {
		t.Fatal(err)
	}
	// Three: see hostengine/images_test.go for why the set shrank. The floor is the live count so an
	// extraction that stops reading the program fails here rather than asserting over nothing.
	if len(refs) < 3 {
		t.Fatalf("only %d image keys came out of the program", len(refs))
	}
	for k := range refs {
		want := "-c " + k + "="
		if !strings.Contains(argv, want) {
			t.Errorf("%s was not sent to pulumi at all:\n%s", k, argv)
			continue
		}
		if !strings.Contains(argv, ":v1.4.0") {
			t.Errorf("no image reached the tag that was asked for:\n%s", argv)
			break
		}
	}
	// A retag is worthless if it does not survive to the APPLY as well as the preview — `-c` is restated
	// on every operation (engine.go's config()) precisely so a hand-edited stack file cannot win.
	for _, line := range strings.Split(strings.TrimSpace(argv), "\n") {
		if strings.Contains(line, "up --yes") && !strings.Contains(line, "kontraImage=") {
			t.Errorf("the converge did not carry the retagged images:\n%s", line)
		}
	}
}
