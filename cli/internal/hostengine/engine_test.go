package hostengine

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// --- reading a preview --------------------------------------------------------------------------

// createPreview is the SHAPE MEASURED on pulumi 3.244.0 against the committed program, trimmed to
// three steps: top-level `config`/`steps`/`duration`/`changeSummary`, each step with `op` and `urn`,
// and a first converge from empty state reading `changeSummary: {"create": 40}`. The full document is
// 40 steps of the same shape.
const createPreview = `{
  "config": {"kontra-control:bind": "127.0.0.1"},
  "steps": [
    {"op":"create","urn":"urn:pulumi:local::kontra-control::pulumi:pulumi:Stack::kontra-control-local",
     "newState":{"urn":"urn:pulumi:local::kontra-control::pulumi:pulumi:Stack::kontra-control-local","type":"pulumi:pulumi:Stack"}},
    {"op":"create","urn":"urn:pulumi:local::kontra-control::pulumi:providers:docker::dockerProvider",
     "newState":{"type":"pulumi:providers:docker"}},
    {"op":"create","urn":"urn:pulumi:local::kontra-control::docker:index/container:Container::temporal",
     "newState":{"type":"docker:index/container:Container"}}
  ],
  "duration": 2100000000,
  "changeSummary": {"create": 40}
}`

// samePreview is the same shape with the ops changed, and it is CONSTRUCTED rather than measured —
// honestly so. A genuinely all-same preview needs a real converge first, and
// `control/pulumi/README.md:375-381` records that `pulumi up` has never been run against this program
// because the live compose stack on this box owns all 13 container names. So the no-op arm is tested
// against the document pulumi would produce, and the claim that a second converge IS a no-op stays
// where that README already put it: reasoned, not measured.
const samePreview = `{
  "steps": [
    {"op":"same","urn":"urn:pulumi:local::kontra-control::docker:index/container:Container::temporal",
     "newState":{"type":"docker:index/container:Container"}},
    {"op":"same","urn":"urn:pulumi:local::kontra-control::docker:index/container:Container::redis",
     "newState":{"type":"docker:index/container:Container"}}
  ],
  "changeSummary": {"same": 40}
}`

const replacePreview = `{
  "steps": [
    {"op":"same","urn":"urn:pulumi:local::kontra-control::docker:index/container:Container::redis",
     "newState":{"type":"docker:index/container:Container"}},
    {"op":"replace","urn":"urn:pulumi:local::kontra-control::docker:index/container:Container::temporal",
     "newState":{"type":"docker:index/container:Container"},
     "oldState":{"type":"docker:index/container:Container"}}
  ],
  "changeSummary": {"same": 39, "replace": 1}
}`

func TestParseSummaryReadsAFirstConverge(t *testing.T) {
	sum, err := ParseSummary([]byte(createPreview))
	if err != nil {
		t.Fatal(err)
	}
	if sum.Counts["create"] != 40 {
		t.Errorf("Counts[create] = %d, want 40 (changeSummary is the authority when it is there)", sum.Counts["create"])
	}
	if sum.Total != 40 {
		t.Errorf("Total = %d, want 40 — the printed line says '3 of 40' and needs the denominator", sum.Total)
	}
	if !sum.WouldChange() {
		t.Error("40 creates is not a no-op")
	}
	if sum.ChangedCount() != 40 {
		t.Errorf("ChangedCount = %d, want 40", sum.ChangedCount())
	}
}

// A NO-OP CONVERGE PREVIEWS AS ALL-SAME, and the exit code that carries it is issue 04's whole
// promise to CI.
func TestParseSummaryReadsANoOp(t *testing.T) {
	sum, err := ParseSummary([]byte(samePreview))
	if err != nil {
		t.Fatal(err)
	}
	if sum.WouldChange() {
		t.Errorf("an all-same preview reported a change: %v", sum.Counts)
	}
	if sum.ChangedCount() != 0 {
		t.Errorf("ChangedCount = %d, want 0", sum.ChangedCount())
	}
	if len(sum.Changes) != 0 {
		t.Errorf("Changes = %v, want none", sum.Changes)
	}
}

// ADR 0052 §2's own example of why `--preview` ships on day one: *"this will replace `temporal`"* is
// the question somebody asks on the day they upgrade, and a count of 1 does not answer it.
func TestParseSummaryNamesWhatWouldBeReplaced(t *testing.T) {
	sum, err := ParseSummary([]byte(replacePreview))
	if err != nil {
		t.Fatal(err)
	}
	if !sum.WouldChange() {
		t.Fatal("a replacement is a change")
	}
	rep := sum.Replaced()
	if len(rep) != 1 {
		t.Fatalf("Replaced = %v, want one resource", rep)
	}
	if rep[0].Name != "temporal" {
		t.Errorf("Replaced names %q, want temporal — the name is what an operator recognises", rep[0].Name)
	}
	if !strings.Contains(rep[0].String(), "docker:index/container:Container") {
		t.Errorf("the line should carry the type beside the name: %s", rep[0])
	}
	if !strings.Contains(strings.Join(sum.Ops(), ","), "replace") {
		t.Errorf("Ops = %v, and `replace` must be in it", sum.Ops())
	}
	// Ops is ordered by consequence and not alphabetically, so the line an operator must not miss is
	// not below `same`.
	if got := sum.Ops(); got[0] != "replace" {
		t.Errorf("Ops = %v — a replacement must be printed before the 39 resources that are staying", got)
	}
}

// THE ONE THING IN ParseSummary THAT HAS TO BE RIGHT. `--preview`'s whole value is a CI gate, and the
// failure mode of a gate is PASSING: if a later pulumi renamed `changeSummary`, or `--json` grew an
// envelope, a silently-zero Summary would answer "nothing would change" forever, on every install,
// and nothing would ever fail again.
func TestParseSummaryRefusesADocumentItDoesNotRecognise(t *testing.T) {
	for _, doc := range []string{`{}`, `{"steps":[],"changeSummary":{}}`, `{"plan":{"steps":[]}}`} {
		if sum, err := ParseSummary([]byte(doc)); err == nil {
			t.Errorf("%s was read as a clean plan (%v) instead of refused — that is the one wrong answer "+
				"a CI gate cannot notice", doc, sum.Counts)
		}
	}
	if _, err := ParseSummary([]byte("warning: something\n{")); err == nil {
		t.Error("unparseable output must be an error, not an empty plan")
	}
}

// changeSummary is the authority WHEN IT IS THERE; the steps are the fallback, because every resource
// the engine walked is a step and the ops are on them.
func TestParseSummaryFallsBackToTheSteps(t *testing.T) {
	sum, err := ParseSummary([]byte(`{"steps":[
	  {"op":"update","urn":"urn:pulumi:local::kontra-control::docker:index/container:Container::redis"},
	  {"op":"same","urn":"urn:pulumi:local::kontra-control::docker:index/container:Container::temporal"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if sum.Counts["update"] != 1 || sum.Counts["same"] != 1 {
		t.Fatalf("Counts = %v, want one update and one same derived from the steps", sum.Counts)
	}
	if !sum.WouldChange() {
		t.Error("an update is a change")
	}
}

// --- the environment the child runs under -------------------------------------------------------

// THE ENGINE'S ENVIRONMENT IS BUILT, NOT INHERITED. PULUMI_BACKEND_URL is restated because an exported
// one beats everything else (measured, get.sh:307-311), and PULUMI_ACCESS_TOKEN is dropped because
// there is no backend here it could legitimately authenticate to.
func TestEngineEnvStatesTheBackendAndDropsTheToken(t *testing.T) {
	t.Setenv("PULUMI_ACCESS_TOKEN", "pul-deadbeef")
	t.Setenv("PULUMI_BACKEND_URL", "https://api.pulumi.com")
	t.Setenv("PULUMI_CONFIG_PASSPHRASE", "somebody-elses")
	e := &Engine{Backend: "file:///tmp/x/state", Passphrase: "ours"}
	env := e.EngineEnv()
	var backend, pass int
	for _, kv := range env {
		switch {
		case kv == "PULUMI_BACKEND_URL=file:///tmp/x/state":
			backend++
		case strings.HasPrefix(kv, "PULUMI_BACKEND_URL="):
			t.Errorf("a second PULUMI_BACKEND_URL survived: %q — the inherited one would win or lose by "+
				"position, which is not a thing to leave to chance", kv)
		case strings.HasPrefix(kv, "PULUMI_ACCESS_TOKEN="):
			t.Errorf("PULUMI_ACCESS_TOKEN reached the child: %q", kv)
		case kv == "PULUMI_CONFIG_PASSPHRASE=ours":
			pass++
		case strings.HasPrefix(kv, "PULUMI_CONFIG_PASSPHRASE="):
			t.Errorf("somebody else's passphrase reached the child: %q — it would encrypt this stack's "+
				"secrets under a key this installation does not keep", kv)
		}
	}
	if backend != 1 || pass != 1 {
		t.Fatalf("want exactly one backend and one passphrase in the child env, got %d and %d", backend, pass)
	}
}

// --- driving the binary -------------------------------------------------------------------------

// A MISSING BINARY IS A REFUSAL NAMING THE REMEDY AND NEVER A PASS — trustpolicy.go:470-474's rule,
// and here it is the single most likely failure: this is the command an operator runs on a machine
// they have just set up.
func TestReadyRefusesAMissingBinary(t *testing.T) {
	e := &Engine{Bin: filepath.Join(t.TempDir(), "no-such-pulumi")}
	err := e.Ready()
	if err == nil {
		t.Fatal("a missing pulumi must refuse")
	}
	for _, want := range []string{"PATH", "get.pulumi.com", "ADR 0052 §1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must name the remedy; %q missing from:\n%v", want, err)
		}
	}
}

// stubPulumi writes a `pulumi` that records its argv and environment and answers the two calls a
// preview makes. A STUB RATHER THAN THE REAL BINARY because what is under test is the argv and the
// env — that the stack name has no slash in it, that the config arrives as `-c`, that the backend is
// stated — and a real `pulumi` would answer that question by needing Docker, a program and minutes.
func stubPulumi(t *testing.T, previewJSON string) (bin, log string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the stub is a /bin/sh script; this CLI releases for linux and macOS (ADR 0031 §2)")
	}
	dir := t.TempDir()
	bin = filepath.Join(dir, "pulumi")
	log = filepath.Join(dir, "argv.log")
	script := "#!/bin/sh\n" +
		"echo \"$@\" >> " + log + "\n" +
		"env | grep '^PULUMI_' | sort >> " + log + "\n" +
		"case \"$*\" in\n" +
		"  *preview*) cat <<'EOF'\n" + previewJSON + "\nEOF\n" + "  ;;\n" +
		"  *'stack output'*) echo http://127.0.0.1:8088 ;;\n" +
		"esac\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return bin, log
}

func TestPreviewPassesTheStackTheConfigAndTheBackend(t *testing.T) {
	bin, log := stubPulumi(t, replacePreview)
	e := &Engine{
		Bin:        bin,
		Program:    "/opt/kontra/engine/pulumi",
		Backend:    "file:///opt/kontra/state",
		Passphrase: "p",
		Stack:      StackRef{Project: Project, Stack: DefaultStack},
		Config:     map[string]string{"workspaces": "/srv/workspaces", "assetsDir": "/opt/kontra/engine/images"},
	}
	if err := e.SelectStack(); err != nil {
		t.Fatal(err)
	}
	sum, err := e.Preview()
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Replaced()) != 1 {
		t.Fatalf("the stub's document was not parsed: %v", sum.Counts)
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	for _, want := range []string{
		"-C /opt/kontra/engine/pulumi",                // the program directory, never the CWD
		"stack select --create local",                 // one segment, and created if absent
		"preview --json --stack local",                // the structured channel, and the stack restated
		"-c assetsDir=/opt/kontra/engine/images",      // sorted, so two runs produce one argv
		"-c workspaces=/srv/workspaces",               // the one key the program refuses to guess
		"--non-interactive",                           // a prompt in an installer is a hang with no output
		"PULUMI_BACKEND_URL=file:///opt/kontra/state", // stated, because an exported one beats a login
		"PULUMI_CONFIG_PASSPHRASE=p",
		"PULUMI_SKIP_UPDATE_CHECK=true",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the invocation is missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, Project+"/") {
		t.Errorf("a two-segment stack name reached pulumi — measured on 3.244.0 that dies with "+
			"`organization name must be 'organization'`:\n%s", got)
	}
	if !strings.Contains(got, "PULUMI_ACCESS_TOKEN") {
		return // nothing set it; that is the normal case
	}
	t.Errorf("PULUMI_ACCESS_TOKEN reached the child:\n%s", got)
}

// "COULD NOT RUN" AND "RAN AND SAID NO" ARE DIFFERENT CLASSES — trustpolicy.go:504-511. And pulumi's
// own bytes are quoted into the error rather than parsed, which is the same file's other rule
// (:500-503: "branching on its wording would be a promise about a binary this repo does not version").
func TestAFailedInvocationQuotesPulumisOwnWords(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("/bin/sh stub")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "pulumi")
	body := "#!/bin/sh\necho 'error: validating stack config: Stack local is missing configuration " +
		"value kontra-control:workspaces' 1>&2\nexit 255\n"
	if err := os.WriteFile(bin, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	e := &Engine{Bin: bin, Program: dir, Backend: "file:///x", Passphrase: "p",
		Stack:  StackRef{Project: Project, Stack: DefaultStack},
		Config: map[string]string{"infraPassphrase": "hunter2"}}
	_, err := e.Preview()
	if err == nil {
		t.Fatal("a non-zero exit must be an error")
	}
	if !strings.Contains(err.Error(), "missing configuration value") {
		t.Errorf("pulumi's own sentence must be in the error, unparsed: %v", err)
	}
	if !strings.Contains(err.Error(), "exited 255") {
		t.Errorf("the exit code belongs in the error too: %v", err)
	}
	// AN ERROR STRING IS PRINTED, LOGGED AND PASTED INTO ISSUES, and `-c` is how this engine passes
	// the desired state — `infraPassphrase` among it (Pulumi.yaml:283).
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("a config VALUE was echoed into the error: %v", err)
	}
	if !strings.Contains(err.Error(), "infraPassphrase=…") {
		t.Errorf("the redaction should still say which key was passed: %v", err)
	}
}

// --- what `kontra update` adds to the invocation (ADR 0052 §7) ----------------------------------

// `--refresh` ON THE PREVIEW AND ON THE CONVERGE, AND ON NEITHER `stack` CALL.
//
// It is what "re-resolve the image digests" means on this engine: without it a `docker.RemoteImage` is
// diffed against the CHECKPOINT, so a daemon holding newer bytes under the same floating tag reports no
// changes and the update silently does nothing (images.go has the measurement). `stack select` and
// `stack export` do not take the flag at all, which is why it is not simply appended to argv().
func TestRefreshReachesThePreviewAndTheConvergeAndNothingElse(t *testing.T) {
	bin, log := stubPulumi(t, samePreview)
	e := &Engine{Bin: bin, Program: "/opt/p", Backend: "file:///opt/s", Passphrase: "p", Refresh: true,
		Stack: StackRef{Project: Project, Stack: DefaultStack}}
	if err := e.SelectStack(); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ExportStack(); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Preview(); err != nil {
		t.Fatal(err)
	}
	if err := e.Up(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if strings.HasPrefix(line, "PULUMI_") {
			continue
		}
		wantRefresh := strings.Contains(line, "preview") || strings.Contains(line, " up ")
		if got := strings.Contains(line, "--refresh"); got != wantRefresh {
			t.Errorf("--refresh present=%v, want %v on: %s", got, wantRefresh, line)
		}
	}
	if !strings.Contains(string(b), "stack export --stack local") {
		t.Errorf("`stack export` did not reach pulumi:\n%s", b)
	}
	// And it is OFF by default, because a refresh reads all 40 resources against Docker and
	// `kontra control up` is the command that gets you a control plane, not the one that audits one.
	bin2, log2 := stubPulumi(t, samePreview)
	e2 := &Engine{Bin: bin2, Program: "/opt/p", Backend: "file:///opt/s", Passphrase: "p",
		Stack: StackRef{Project: Project, Stack: DefaultStack}}
	if _, err := e2.Preview(); err != nil {
		t.Fatal(err)
	}
	if b2, _ := os.ReadFile(log2); strings.Contains(string(b2), "--refresh") {
		t.Errorf("--refresh was sent without Refresh being set:\n%s", b2)
	}
}

// `--exclude-protected` ON THE DESTROY, AND IT IS WHAT KEEPS `kontra control down` WORKING AT ALL.
//
// Measured on 3.244.0 against a scratch stack of one container, one network and two volumes, one of
// them protected: a PLAIN `pulumi destroy` answers `error: preview failed` / `- 3 to delete, 2 errored`
// and LEAVES THE CONTAINER RUNNING. ADR 0052 §7's protection would therefore have made the install
// unremovable — the opposite of what it is for — while looking like it had only made it safer. With the
// flag, the container and the network go, both docker volumes stay, and pulumi says "All unprotected
// resources were destroyed. There are still 3 protected resources associated with this stack."
func TestDestroyExcludesProtectedResources(t *testing.T) {
	bin, log := stubPulumi(t, samePreview)
	e := &Engine{Bin: bin, Program: "/opt/p", Backend: "file:///opt/s", Passphrase: "p",
		Stack: StackRef{Project: Project, Stack: DefaultStack}}
	if err := e.Destroy(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "destroy --yes --exclude-protected") {
		t.Errorf("the destroy is missing --exclude-protected, so it fails outright on the eight protected "+
			"volumes and leaves the containers running:\n%s", b)
	}
	// And not on the converge, where excluding them would mean an `up` that quietly skipped the volumes
	// it is supposed to adopt.
	if err := e.Up(); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(log)
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if strings.Contains(line, " up ") && strings.Contains(line, "--exclude-protected") {
			t.Errorf("--exclude-protected reached the converge: %s", line)
		}
	}
}

// A FAILED PREVIEW STILL CARRIES THE PLAN, and that is the measurement this whole path exists for.
// Measured on 3.244.0 against a program with a protected volume deleted out of it: exit 1, NOTHING on
// stderr, and a complete `--json` document on stdout carrying the `delete` step and the refusal in
// `diagnostics`. Discarding stdout on a non-zero exit would throw away the only machine-readable
// statement of what was refused, and the operator would get a URN instead of a volume name.
func TestAPreviewPulumiRefusedStillYieldsThePlan(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("/bin/sh stub")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "pulumi")
	body := "#!/bin/sh\ncat <<'EOF'\n" + protectedDeletePreview + "\nEOF\nexit 1\n"
	if err := os.WriteFile(bin, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	e := &Engine{Bin: bin, Program: dir, Backend: "file:///x", Passphrase: "p",
		Stack: StackRef{Project: Project, Stack: DefaultStack}}
	_, err := e.Preview()
	if err == nil {
		t.Fatal("a preview that exited 1 was reported as a success")
	}
	var pe *PreviewError
	if !errors.As(err, &pe) {
		t.Fatalf("the error is not a *PreviewError, so a caller cannot reach the plan: %T %v", err, err)
	}
	if pe.Summary == nil {
		t.Fatal("the plan was discarded")
	}
	if len(pe.Summary.Changes) != 1 || pe.Summary.Changes[0].Name != "volSeaweedData" {
		t.Errorf("the delete step did not survive: %v", pe.Summary.Changes)
	}
	// PULUMI'S OWN WORDS, QUOTED AND NOT PARSED (trustpolicy.go:500-503) — `diagnostics` is the
	// structured channel for exactly the prose that rule forbids branching on.
	if !strings.Contains(err.Error(), "cannot be deleted") {
		t.Errorf("pulumi's own sentence is missing from the error: %v", err)
	}
	if !strings.Contains(err.Error(), "because it is protected") {
		t.Errorf("the reason is missing from the error: %v", err)
	}
}

// A preview that died before walking anything — a program that will not load, a missing required config
// key — has no plan and must still report pulumi's reason rather than a bare exit code.
func TestAPreviewThatDiedEarlyStillReportsWhy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("/bin/sh stub")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "pulumi")
	doc := `{"steps":[],"diagnostics":[{"message":"error: validating stack config: Stack local is missing ` +
		`configuration value 'kontra-control:workspaces'","severity":"error"}]}`
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho '"+doc+"'\nexit 255\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	e := &Engine{Bin: bin, Program: dir, Backend: "file:///x", Passphrase: "p",
		Stack: StackRef{Project: Project, Stack: DefaultStack}}
	_, err := e.Preview()
	if err == nil {
		t.Fatal("exit 255 was reported as a success")
	}
	if !strings.Contains(err.Error(), "missing configuration value") {
		t.Errorf("the reason must reach the operator: %v", err)
	}
	var pe *PreviewError
	if errors.As(err, &pe) && pe.Summary != nil {
		t.Error("a document with no steps must not produce a Summary — a Summary with no changes in it " +
			"would read as 'nothing would change' to anything that asked")
	}
}

func TestANonExecutableBinaryIsNotAToplogyProblem(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode bits")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "pulumi")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o600); err != nil { // present, not executable
		t.Fatal(err)
	}
	e := &Engine{Bin: bin, Program: dir, Backend: "file:///x", Passphrase: "p",
		Stack: StackRef{Project: Project, Stack: DefaultStack}}
	_, err := e.Preview()
	if err == nil {
		t.Fatal("a non-executable pulumi must fail")
	}
	if !strings.Contains(err.Error(), "could not be run") {
		t.Errorf("a permission problem is a fact about the MACHINE, and reporting it as a pulumi exit "+
			"sends somebody to debug their topology: %v", err)
	}
}
