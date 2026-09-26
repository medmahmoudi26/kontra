// engine.go — driving the host `pulumi` binary.
//
// THIS IS THE SECOND SANCTIONED SHELL-OUT IN THIS CLI, and `main.go:5-7` and `infra.go:3-7` both say
// there is one. The sentence they make ("the ONE sanctioned shell-out is `docker compose` in `kontra
// infra`, because compose IS the deployment contract") is true of its reason and no longer of its
// count: ADR 0052 §2 replaces compose YAML with Pulumi YAML as the contract, so the same rule now
// admits a second tool — drive the contract's own engine rather than reimplement it. Both comments
// are updated by the change that retires `kontra infra` (issue 08); this file is the other half of
// that sentence in the meantime.
//
// THREE EXEC SHAPES, TAKEN FROM THE THREE THE REPOSITORY ALREADY HAS:
//
//   - `pulumi up`/`destroy` STREAM to the operator's terminal and the exit code is the answer —
//     `cli/infra.go:96-101`'s shape, os.Stdout/os.Stderr directly and nothing in front of them,
//     because minutes of provider progress is the output (`cliio.go:14-16`: "NOT A LOGGER… a logger
//     would invite both to go through one filter that reorders them").
//   - `pulumi preview --json`, `stack output` and `stack select` CAPTURE, and on failure the tool's
//     own bytes go into the error — the dominant shape in this repository (40+ sites, e.g.
//     `cli/warden/driver_docker.go:87-91`).
//   - A MISSING BINARY IS A REFUSAL NAMING THE REMEDY, and "could not be run" is a different class
//     from "ran and said no" — `cli/internal/trustpolicy/trustpolicy.go:470-511`, which is the most
//     carefully reasoned exec here and the closest analogue to driving a third-party binary. Its
//     other rule applies too: PULUMI'S PROSE IS QUOTED, NEVER PARSED. `--json` is the structured
//     channel; branching on the tool's wording would be a promise about a binary this repository
//     does not version.
package hostengine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
)

// Bin is the binary this engine drives. A variable so a test can point it at a stub; never taken
// from configuration, because "which pulumi" is the operator's PATH and not a kontra setting.
var Bin = "pulumi"

// Engine is one configured engine: one program, one backend, one stack.
type Engine struct {
	// Bin overrides the package default. Empty means Bin.
	Bin string
	// Program is the directory holding Pulumi.yaml — `pulumi -C`.
	Program string
	// Backend is AssertBackend's answer, and it is passed in the child's environment rather than
	// through `pulumi login`, which would rewrite the operator's global credentials.json.
	Backend string
	// Passphrase is PULUMI_CONFIG_PASSPHRASE. Never empty: workspace.ts:100-104 records that an
	// empty one "silently changes how secrets encrypt".
	Passphrase string
	Stack      StackRef
	// Config is `-c key=value` per entry, sorted, on every operation. See config() for why the
	// desired state arrives on the command line rather than through `pulumi config set`.
	Config map[string]string
	// Refresh adds `--refresh` to the preview and the converge. `kontra update` sets it; `kontra
	// control up` does not.
	//
	// WHAT IT IS FOR IS THE IMAGE DIGESTS, and `images.go`'s header has the measurement. Without it,
	// `docker.RemoteImage` is compared against the CHECKPOINT — the digest recorded the last time this
	// stack converged — so an install whose daemon has newer bytes under the same floating tag reports
	// no changes. With it, the provider re-reads what the daemon holds now and the diff is against
	// reality. It is not on by default because a refresh is a read of all 40 resources against Docker
	// and `kontra up` is the command an operator runs to get a control plane, not to audit one.
	Refresh bool
	// Stdout/Stderr are where a STREAMED operation's bytes go. Captured operations ignore them.
	Stdout io.Writer
	Stderr io.Writer
}

// Ready resolves the binary, and a missing one is a refusal that names the remedy.
//
// LOOKPATH FIRST, EVERY TIME, and not once at start-up: trustpolicy.go:470-474 gives the reason for
// the long-lived case ("a Warden runs for weeks and the thing it depends on can be removed under
// it"), and the short-lived case here has the stronger version of it — this is the command an
// operator runs on a machine they have just set up, so "pulumi is not installed" is the single most
// likely failure and the least useful one to discover as `exec: "pulumi": executable file not found`.
func (e *Engine) Ready() error {
	bin := e.bin()
	path, err := exec.LookPath(bin)
	if err != nil {
		return fmt.Errorf("no %s on PATH, and the host engine is that binary — a Pulumi YAML program "+
			"resolves its providers as binary plugins, so this is the whole toolchain (ADR 0052 §1).\n"+
			"      curl -fsSL https://get.pulumi.com | sh\n"+
			"  or let kontra's installer do it, which also pins the docker provider plugin:\n"+
			"      curl -fsSL https://kontra.sh/get.sh | sh", bin)
	}
	e.Bin = path
	return nil
}

func (e *Engine) bin() string {
	if e.Bin != "" {
		return e.Bin
	}
	return Bin
}

// EngineEnv is the environment the child runs under, built rather than inherited.
//
// TWO VARIABLES ARE REMOVED FROM THE INHERITED ENVIRONMENT, and that is the belt beside
// AssertBackend's braces. `PULUMI_BACKEND_URL` is removed and then set to the backend this engine
// decided on, because an exported one beats everything else including a `pulumi login` (measured,
// get.sh:307-311) — so restating it is the only way to be sure. `PULUMI_ACCESS_TOKEN` is removed
// because there is no backend here it could legitimately authenticate to: AssertBackend already
// refused the environment that has one, and a caller that got past it by exporting a `file://` URL
// still has no use for a hosted credential in this process.
//
// `PULUMI_SKIP_UPDATE_CHECK` is set for the reason workspace.ts:75 sets it: this engine is
// non-interactive by construction, and a version nag is one more thing between the operator and the
// output they asked for.
func (e *Engine) EngineEnv() []string {
	drop := map[string]bool{"PULUMI_BACKEND_URL": true, "PULUMI_ACCESS_TOKEN": true, "PULUMI_CONFIG_PASSPHRASE": true}
	out := make([]string, 0, len(os.Environ())+3)
	for _, kv := range os.Environ() {
		if k, _, ok := strings.Cut(kv, "="); ok && drop[k] {
			continue
		}
		out = append(out, kv)
	}
	return append(out,
		"PULUMI_BACKEND_URL="+e.Backend,
		"PULUMI_CONFIG_PASSPHRASE="+e.Passphrase,
		"PULUMI_SKIP_UPDATE_CHECK=true",
	)
}

// config renders Config as `-c key=value`, sorted so two runs produce the same argv.
//
// THE DESIRED STATE ARRIVES ON THE COMMAND LINE, NOT THROUGH `pulumi config set`, and
// `control/pulumi/parity.py:85-89` established the shape: "`workspaces` therefore arrives as `-c` on
// the preview itself". The property that matters here is different from parity's, though. `-c` does
// persist into `Pulumi.<stack>.yaml` (measured on 3.244.0 — it is what the flag's own help says: "and
// save to the stack config file"), so this is not about writing nothing; it is about the CLI being
// the only thing that decides. Every operation restates the whole set it cares about, so a key
// hand-edited into the stack file is corrected on the next converge instead of quietly outliving the
// flag that was supposed to set it.
func (e *Engine) config() []string {
	keys := make([]string, 0, len(e.Config))
	for k := range e.Config {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	args := make([]string, 0, 2*len(keys))
	for _, k := range keys {
		args = append(args, "-c", k+"="+e.Config[k])
	}
	return args
}

// argv is every invocation's shape: `-C <program>` first, `--non-interactive` last.
//
// `--non-interactive` ON EVERY CALL. `pulumi up` without it prompts for confirmation, and a prompt in
// a command an installer runs is a hang with no output — the same failure workspace.ts:59 names for
// the Temporal activity ("a prompt in a Temporal activity is a hang").
func (e *Engine) argv(args ...string) []string {
	return append(append([]string{"-C", e.Program}, args...), "--non-interactive")
}

// refresh is `--refresh` when Refresh is set, and nothing otherwise. A separate helper so the flag
// cannot reach `stack select` or `stack output`, where it is not a valid option.
func (e *Engine) refresh() []string {
	if e.Refresh {
		return []string{"--refresh"}
	}
	return nil
}

// SelectStack creates the stack if it is not there, and selects it.
//
// THE STACK NAME IS ONE SEGMENT HERE, and ParseStack is where that is explained. This is the call
// that measured it: on 3.244.0 against a `file://` backend, `stack select --create kontra-control/local`
// dies with `error: organization name must be 'organization'`.
func (e *Engine) SelectStack() error {
	_, err := e.capture(e.argv("stack", "select", "--create", e.Stack.Stack))
	return err
}

// Preview runs the same converge through `pulumi preview --json` and returns what it would do.
//
// `--json` RATHER THAN READING THE PROSE, per trustpolicy.go:500-503's rule. It also changes how the
// streams are handled: stdout carries ONE document and stderr carries the warnings, so this is the one
// place in the repository that must not use `CombinedOutput()` — interleaving the two makes the
// document unparseable, and the failure would be an unmarshal error about the tool's own warning text.
// A FAILED PREVIEW STILL CARRIES A DOCUMENT, and that measurement is why this function does not just
// return the error. On 3.244.0, previewing a program with a protected volume deleted out of it exits
// 1, writes NOTHING to stderr, and writes a complete `--json` document to stdout — with the `delete`
// step in `steps`, the protection refusal in a new top-level `diagnostics` array, and a
// `changeSummary` of `{"same": 3}` that does not mention the delete at all. So the plan pulumi refused
// to make is right there in the output, and the volume gate can name the volume instead of leaving the
// operator with a diagnostic about a URN. See PreviewError.
func (e *Engine) Preview() (*Summary, error) {
	args := append([]string{"preview", "--json", "--stack", e.Stack.Stack}, e.refresh()...)
	out, err := e.capture(e.argv(append(args, e.config()...)...))
	if err != nil {
		diags := ParseDiagnostics(out)
		sum, perr := ParseSummary(out)
		if perr != nil {
			sum = nil
		}
		if sum != nil || len(diags) > 0 {
			return nil, &PreviewError{Summary: sum, Diagnostics: diags, err: err}
		}
		// Nothing legible came back at all — no document, no diagnostics. That is `classify`'s
		// territory: pulumi's own bytes, quoted and truncated.
		return nil, err
	}
	return ParseSummary(out)
}

// PreviewError is a preview that FAILED and was still legible.
//
// It carries the partial plan so a caller can translate pulumi's own refusal into a sentence about the
// thing that was refused. `errors.As` for it, use `Summary` if it is not nil, and print `Unwrap()`'s
// message underneath — pulumi's words are quoted and never parsed (trustpolicy.go:500-503), and the
// structured half of that rule is what `diagnostics` is for.
type PreviewError struct {
	// Summary is the partial plan, or nil when the preview died before walking any resource (a program
	// that will not load, a missing required config key).
	Summary *Summary
	// Diagnostics is pulumi's own text, verbatim.
	Diagnostics []string
	err         error
}

// THE DIAGNOSTICS ARE RE-INDENTED AND NOTHING ELSE IS DONE TO THEM. Pulumi wraps its own messages at
// its own width, so a multi-line diagnostic dropped into a kontra refusal loses the indentation that
// says "this paragraph is the tool talking". Shifting the continuation lines is the whole of the
// formatting; the words are verbatim, because trustpolicy.go:500-503's rule is that this tool's prose
// is quoted and never parsed.
func (p *PreviewError) Error() string {
	if len(p.Diagnostics) > 0 {
		var b strings.Builder
		b.WriteString("pulumi refused this plan:")
		for _, d := range p.Diagnostics {
			b.WriteString("\n    " + strings.ReplaceAll(d, "\n", "\n    "))
		}
		return b.String()
	}
	return p.err.Error()
}

func (p *PreviewError) Unwrap() error { return p.err }

// ParseDiagnostics pulls pulumi's own error text out of a `preview --json` document, and answers with
// nothing at all for a document it cannot read — this is a nicety on a failure path, not a gate, so it
// must not turn an unreadable document into a second error on top of the first one.
func ParseDiagnostics(doc []byte) []string {
	var d previewDoc
	if json.Unmarshal(doc, &d) != nil {
		return nil
	}
	var out []string
	for _, diag := range d.Diagnostics {
		if m := strings.TrimSpace(diag.Message); m != "" {
			out = append(out, m)
		}
	}
	return out
}

// ExportStack is `pulumi stack export` — this installation's own record of what it has.
//
// CAPTURED AND RETURNED RATHER THAN WRITTEN, because two callers want two things from one invocation:
// `stateexport.go` writes it to a timestamped file before a converge, and `volumes.go` reads the
// volume resources out of it for the identity gate. Running `pulumi` twice for those would leave a
// window in which the file and the checked plan describe different installations.
func (e *Engine) ExportStack() ([]byte, error) {
	return e.capture(e.argv("stack", "export", "--stack", e.Stack.Stack))
}

// Up converges. Streamed, and the exit code is the answer.
//
// NO context.Context AND NO exec.CommandContext, deliberately. `cli/warden/warden.go:54` records what
// CommandContext actually does ("SIGKILLs ONLY the process it started"), and SIGKILL is the wrong
// signal for this child: a converge killed between writing a resource and checkpointing it is exactly
// the corrupted state the lock in program.go exists to prevent. Ctrl-C already reaches `pulumi`
// through the terminal's process group, which is how the operator stops one, and `pulumi` handles it
// — it declines a second interrupt with "confirmed, cancelling" and finishes its checkpoint.
// `cli/infra.go:96-101` runs `docker compose` the same way, for the same reason.
func (e *Engine) Up() error {
	args := append([]string{"up", "--yes", "--stack", e.Stack.Stack}, e.refresh()...)
	return e.stream(e.argv(append(args, e.config()...)...))
}

// Destroy removes what the program creates and keeps every volume. That is `docker compose down`, not
// `down -v`, and there is deliberately no flag here that deletes them:
// `control/pulumi/README.md:310-317` puts that at `docker volume rm` typed by a person.
//
// ── `--exclude-protected` IS LOAD-BEARING, NOT DEFENSIVE, AND THE MEASUREMENT IS WHY ─────────────
//
// ADR 0052 §7 put `protect: true` on eight of the eleven volumes, and A PLAIN `pulumi destroy` ON A
// STACK WITH A PROTECTED RESOURCE FAILS ENTIRELY. Measured on 3.244.0 against a scratch stack of one
// container, one network and two volumes (one protected):
//
//	error: preview failed
//	Resources: - 3 to delete, 2 errored
//	# and the container was STILL RUNNING afterwards
//
// So without this flag, adding protection to the program turned `kontra control down` into a command
// that errors and changes nothing — the protection would have made the install unremovable, which is
// the opposite of what it is for. With it, on the same stack:
//
//   - docker:index:Container svc deleted
//   - docker:index:Network   net deleted
//     Resources: - 3 deleted, 3 unchanged
//     All unprotected resources were destroyed. There are still 3 protected resources …
//     # container gone, network gone, BOTH docker volumes still on disk
//
// WHICH IS EXACTLY THE SEMANTICS THIS COMMAND ALREADY PROMISED, arrived at from the other side: the
// containers and the network go, the data stays. Two differences from before are worth knowing. The
// protected volumes stay IN STATE rather than being dropped from it, so the next `up` re-adopts them by
// URN instead of re-creating the resource and adopting the docker volume by name — strictly better for
// the one volume whose identity ADR 0052 §7 cares most about. And a destroyed stack is no longer empty,
// so `pulumi stack rm` on it needs `--force`, which is the same "unprotect deliberately" cost
// protection is paid for everywhere else.
func (e *Engine) Destroy() error {
	args := append([]string{"destroy", "--yes", "--exclude-protected", "--stack", e.Stack.Stack}, e.config()...)
	return e.stream(e.argv(args...))
}

// Output reads one stack output — `consoleUrl` is the one ADR 0052 §3 requires every boot to end with
// (`Pulumi.yaml:1350`, built from `bind` and `apiPort` so a changed port cannot leave the printed line
// pointing at nothing).
func (e *Engine) Output(name string) (string, error) {
	out, err := e.capture(e.argv("stack", "output", name, "--stack", e.Stack.Stack))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// capture runs one invocation and returns stdout, with the tool's stderr quoted into any error.
//
// STDOUT IS RETURNED EVEN ON FAILURE. A `preview --json` that pulumi refused to plan still wrote the
// whole document (see Preview), so discarding it on a non-zero exit would throw away the only
// machine-readable statement of WHAT was refused. Callers that do not want it ignore it, which is what
// `if err != nil { return nil, err }` already does at every other site.
func (e *Engine) capture(args []string) ([]byte, error) {
	cmd := exec.Command(e.bin(), args...)
	cmd.Env = e.EngineEnv()
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	if err != nil {
		return out.Bytes(), e.classify(args, err, errOut.Bytes(), out.Bytes())
	}
	return out.Bytes(), nil
}

// stream runs one invocation with the child's bytes going straight to the operator's terminal.
func (e *Engine) stream(args []string) error {
	cmd := exec.Command(e.bin(), args...)
	cmd.Env = e.EngineEnv()
	cmd.Stdout, cmd.Stderr = e.Stdout, e.Stderr
	if err := cmd.Run(); err != nil {
		return e.classify(args, err, nil, nil)
	}
	return nil
}

// classify separates "could not run pulumi" from "pulumi ran and said no".
//
// trustpolicy.go:504-511's distinction, and it matters for the same reason there: a permission
// problem or a corrupt binary is a fact about the machine, and a non-zero exit is a fact about the
// program — sending somebody to debug their topology because `pulumi` was not executable is the
// failure this avoids. The tool's own bytes are quoted and truncated, never parsed.
func (e *Engine) classify(args []string, err error, stderr, stdout []byte) error {
	said := strings.TrimSpace(string(stderr))
	if said == "" {
		said = strings.TrimSpace(string(stdout))
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return fmt.Errorf("%s could not be run (a permission problem, a corrupt binary): %v", e.bin(), err)
	}
	if said == "" {
		return fmt.Errorf("pulumi %s exited %d", redact(args), ee.ExitCode())
	}
	return fmt.Errorf("pulumi %s exited %d: %s", redact(args), ee.ExitCode(), truncate(said, 4000))
}

// redact keeps config VALUES out of an error message. `-c key=value` is how this engine passes the
// desired state, `infraPassphrase` is one of the keys (`Pulumi.yaml:283`), and an error string is
// printed, logged and pasted into issues.
func redact(args []string) string {
	out := make([]string, 0, len(args))
	skip := false
	for _, a := range args {
		if skip {
			if k, _, ok := strings.Cut(a, "="); ok {
				out = append(out, k+"=…")
			}
			skip = false
			continue
		}
		if a == "-c" {
			skip = true
			out = append(out, a)
			continue
		}
		out = append(out, a)
	}
	return strings.Join(out, " ")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n  … (truncated)"
}

// --- what a preview says ------------------------------------------------------------------------

// Change is one resource a converge would touch.
type Change struct {
	Op   string
	Name string
	Type string
}

func (c Change) String() string {
	if c.Type == "" {
		return c.Name
	}
	return c.Name + " (" + c.Type + ")"
}

// Summary is one preview, as data.
type Summary struct {
	// Counts is op -> resource count. Pulumi's own vocabulary, not ours: create, update, replace,
	// delete, same, refresh, read, import, and the three halves of a replacement.
	Counts map[string]int
	// Changes is every resource that is not staying as it is, in step order.
	Changes []Change
	// Total is every resource the preview walked, changed or not, so the printed line can say
	// "3 of 40" rather than "3".
	Total int
	// Diagnostics is pulumi's own `error:`/`warning:` text, VERBATIM, in the order it emitted them —
	// the structured channel for the prose trustpolicy.go:500-503 forbids parsing. Measured: a preview
	// that fails during plan generation puts its refusal here and nothing on stderr.
	Diagnostics []string
}

// Replaced is the one op an operator must be told about BY NAME — ADR 0052 §2: *"this will replace
// `temporal`"* is the question somebody asks on the day they upgrade, and a count of 1 does not
// answer it. Pulumi spells a replacement three ways in a step stream (`replace`, and the
// `create-replacement`/`delete-replaced` pair when it shows the halves), so the match is on the
// substring rather than on an enumeration of a vocabulary this repository does not version.
func (s *Summary) Replaced() []Change {
	var out []Change
	seen := map[string]bool{}
	for _, c := range s.Changes {
		if strings.Contains(c.Op, "replac") && !seen[c.Name] {
			seen[c.Name] = true
			out = append(out, c)
		}
	}
	return out
}

// unchangedOps are the ops that are not a change to anything.
//
// `same` is the obvious one. `read` is here because a read step is a provider LOOKING at something
// (`fn::invoke`, a getter) and not touching it; the committed program has none, so the honest reason
// to list it is that a future one must not turn `--preview` red in CI for asking a question.
//
// `refresh` IS HERE BECAUSE OF A MEASUREMENT, AND IT USED TO BE DOCUMENTED AS A CHANGE. This comment
// previously read "every other op — including `refresh`, which means the state and the world had
// drifted — is a change", which was a guess made before anything in this engine passed `--refresh`.
// `kontra update` does (images.go says why), and measured on 3.244.0 against a stack that had just
// converged, with nothing drifted:
//
//	changeSummary: {"same": 13}
//	step ops:      {"refresh": 13, "same": 1}
//
// A REFRESH STEP IS EMITTED FOR EVERY RESOURCE, unconditionally — it is the provider re-reading the
// world, which is the same class of thing as `read`, not evidence that anything moved. Counting it as
// a change made `kontra update --check` exit 1 on an install that was exactly up to date, which is the
// gate answering "an update is available" forever. What a refresh FINDS still shows up: it lands in
// `changeSummary` as the ordinary op, and measured — with a volume deleted out from under the stack —
// as a `create` there and a second step beside the refresh.
var unchangedOps = map[string]bool{"same": true, "read": true, "refresh": true}

// WouldChange is the answer `kontra up --preview`'s exit code carries.
//
// IT ASKS BOTH THE SUMMARY AND THE STEPS, AND THAT IS A MEASUREMENT AND NOT BELT-AND-BRACES. On
// 3.244.0, a preview that pulumi refused to plan — a protected volume deleted out of the program —
// emitted a `delete` STEP for that volume and a `changeSummary` of `{"same": 3}` WITH NO DELETE IN IT.
// Keying only on the counts would answer "nothing would change" about a plan whose one change is the
// destruction of a volume. The steps cannot omit a resource the engine walked, so a non-empty
// `Changes` is a change whatever the summary says. An all-same preview has no Changes at all
// (ParseSummary skips `same` and `read`), so the no-op arm is unaffected.
func (s *Summary) WouldChange() bool {
	if len(s.Changes) > 0 {
		return true
	}
	for op, n := range s.Counts {
		if n > 0 && !unchangedOps[op] {
			return true
		}
	}
	return false
}

// ChangedCount is how many resources are not staying as they are.
func (s *Summary) ChangedCount() int {
	n := 0
	for op, c := range s.Counts {
		if !unchangedOps[op] {
			n += c
		}
	}
	return n
}

// Ops lists the ops present, most-consequential first, so the printed summary has a stable order
// that is not alphabetical (`create` before `delete` before `same` reads as a plan; `create, delete,
// replace, same, update` reads as a dictionary).
func (s *Summary) Ops() []string {
	order := []string{"delete", "replace", "delete-replaced", "create-replacement", "create", "update", "refresh", "import", "read", "same"}
	seen := map[string]bool{}
	var out []string
	for _, op := range order {
		if s.Counts[op] > 0 {
			seen[op] = true
			out = append(out, op)
		}
	}
	rest := make([]string, 0, len(s.Counts))
	for op, n := range s.Counts {
		if n > 0 && !seen[op] {
			rest = append(rest, op)
		}
	}
	sort.Strings(rest)
	return append(out, rest...)
}

// previewDoc is the shape `pulumi preview --json` emits. Measured on 3.244.0 against the committed
// program: the top-level keys are `config`, `steps`, `duration`, `changeSummary`, each step carries
// `op` and `urn`, and a first converge reads `changeSummary: {"create": 40}`.
type previewDoc struct {
	Steps []struct {
		Op       string `json:"op"`
		URN      string `json:"urn"`
		NewState *struct {
			Type string `json:"type"`
		} `json:"newState"`
		OldState *struct {
			Type string `json:"type"`
		} `json:"oldState"`
	} `json:"steps"`
	ChangeSummary map[string]int `json:"changeSummary"`
	// Diagnostics appears when a preview emits an error or a warning. Measured on the protected-volume
	// refusal: `[{"urn": "…::volSeaweedData", "message": "error: Preview failed: … cannot be deleted
	// because it is protected…", "severity": "error"}, {"message": "error: preview failed\n", …}]`.
	Diagnostics []struct {
		URN      string `json:"urn"`
		Message  string `json:"message"`
		Severity string `json:"severity"`
	} `json:"diagnostics"`
}

// ParseSummary turns `pulumi preview --json`'s document into a Summary.
//
// AN EMPTY DOCUMENT IS AN ERROR AND NOT "NO CHANGES", and that is the one thing in this function that
// has to be right. `--preview`'s whole value is a CI gate, and the failure mode of a gate is passing:
// if a later pulumi renamed `changeSummary`, or `--json` grew an envelope, then a silently-zero
// Summary would answer "nothing would change" forever, on every install, and nothing would ever fail.
// So a document with neither steps nor a change summary refuses.
//
// COUNTS COME FROM `changeSummary` WHEN IT IS THERE AND FROM THE STEPS WHEN IT IS NOT, because the
// two are the same numbers from one run and the steps are the ones that cannot be omitted — every
// resource the engine walked is a step, with its op on it.
func ParseSummary(doc []byte) (*Summary, error) {
	var d previewDoc
	if err := json.Unmarshal(doc, &d); err != nil {
		return nil, fmt.Errorf("pulumi preview --json produced something this cannot read: %w", err)
	}
	if len(d.Steps) == 0 && len(d.ChangeSummary) == 0 {
		return nil, errors.New("pulumi preview --json produced no steps and no changeSummary. " +
			"That is not 'no changes' — a preview always walks every resource — so it is refused " +
			"rather than reported as a clean plan, which is the one wrong answer a CI gate cannot notice")
	}
	s := &Summary{Counts: map[string]int{}}
	for _, diag := range d.Diagnostics {
		s.Diagnostics = append(s.Diagnostics, strings.TrimSpace(diag.Message))
	}
	for _, st := range d.Steps {
		if unchangedOps[st.Op] {
			continue
		}
		typ := ""
		if st.NewState != nil {
			typ = st.NewState.Type
		} else if st.OldState != nil {
			typ = st.OldState.Type
		}
		s.Changes = append(s.Changes, Change{Op: st.Op, Name: urnName(st.URN), Type: typ})
	}
	if len(d.ChangeSummary) > 0 {
		for op, n := range d.ChangeSummary {
			s.Counts[op] = n
			s.Total += n
		}
		return s, nil
	}
	for _, st := range d.Steps {
		s.Counts[st.Op]++
		s.Total++
	}
	return s, nil
}

// urnName is the resource's own name — the last segment of a URN, which is
// `urn:pulumi:<stack>::<project>::<type>::<name>`. The name is what an operator recognises
// ("temporal"), and the type is noise beside it in a summary line.
func urnName(urn string) string {
	i := strings.LastIndex(urn, "::")
	if i < 0 {
		return urn
	}
	return urn[i+2:]
}
