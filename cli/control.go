// control.go — `kontra control up|down`, the HOST ENGINE (ADR 0052 §1-§2).
//
// This converges `kontra-control` — the local control plane as a Pulumi YAML program,
// `control/pulumi/Pulumi.yaml`, 13 containers on one private Docker network — by shelling to the host
// `pulumi`. The engine itself is `cli/internal/hostengine`, which holds the dispatch table, the
// backend refusal and the exec wiring; this file is the command surface and nothing else.
//
// ── WHY THE WORD IS `control` TODAY AND `up` TOMORROW ───────────────────────────────────────────
//
// ADR 0052 §2 is unambiguous that this IS `kontra up`: "`kontra up` converges `kontra-control` with
// the host engine", and the appliance "goes". It cannot be spelled that way yet, and the reason is a
// fact about the switch rather than a preference. `cli/main.go:331` is `case "up": err = cmdUp(...)`,
// Go refuses a duplicate constant case, and `cli/up.go`'s appliance is pinned by eight assertions in
// `cli/up_test.go` — `:12`, `:28`, `:44` (which asserts the positional-argument refusal QUOTES the
// argument), `:55` (which additionally asserts help "still lists the compose one, which is a
// different topology and not a synonym"), `:69`, `:89`, `:102`, `:122`. Removing the appliance is
// issue 08 and it is 11,719 lines with a seven-row import audit in the ADR; doing it as a side effect
// of adding this command would delete another slice's work without its tests.
//
// SO THE SWAP IS KEPT TO ONE LINE, ON PURPOSE. `cmdControlUp` takes the same `args[1:]` the switch
// hands every case and parses `--preview` itself, so issue 08's change to this command is exactly
//
//	case "up":
//	-	err = cmdUp(args[1:])
//	+	err = cmdControlUp(args[1:])
//
// plus the `usageText` block and retiring the appliance's tests. Nothing in this file assumes the word
// it is reached by. `kontra control up --preview` today is `kontra up --preview` then, character for
// character after the verb.
//
// ── AND THE EXIT CODE, WHICH IS THE ONE PROMISE `--preview` MAKES TO CI ────────────────────────
//
// See cmdControlUp. 0 means the converge is a no-op; 1 means it would change something. A preview
// that could not RUN also exits 1, and carries that in its message rather than in a third code —
// `cli/workflowreplay.go:132-145` is the established precedent for exactly this shape (python's three
// codes collapsed into main's two, with the difference stated in the sentence), because a third code
// needs a third arm in `main`'s fork at `cli/main.go:282-289` and that fork is not this slice's to
// widen. `--json` is the exact channel for a caller that must tell the two apart.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/medmahmoudi26/kontra/cli/internal/cliio"
	"github.com/medmahmoudi26/kontra/cli/internal/cliutil"
	"github.com/medmahmoudi26/kontra/cli/internal/hostengine"
)

func cmdControl(args []string) error {
	if len(args) == 0 {
		controlUsage()
		return errors.New("control: expected a subcommand")
	}
	switch args[0] {
	case "up":
		return cmdControlUp(args[1:])
	case "down":
		return cmdControlDown(args[1:])
	case "help", "-h", "--help":
		controlUsage()
		return nil
	default:
		controlUsage()
		return fmt.Errorf("unknown control subcommand %q (want up|down)", args[0])
	}
}

func controlUsage() {
	fmt.Fprint(cliio.Stdout, `kontra control — the control plane, converged by the host Pulumi engine (ADR 0052 §1)

  kontra control up [--preview] [--json]           # converge kontra-control: 13 containers, one network
        [--stack local] [--workspaces <dir>]       #   --preview changes nothing and exits 0 only if the
        [--bind 127.0.0.1] [--api-port 8088]       #   converge would be a no-op, so CI can gate on it
        [--program <dir>]
  kontra control down [--stack local]              # destroy the containers and the network; KEEPS all
                                                   #   11 volumes (retainOnDelete), so this is
                                                   #   "compose down", not "down -v"

THIS ENGINE CONVERGES ONE PROJECT AND REFUSES EVERY OTHER. kontra-control is a topology of
containers on this box and needs no cloud credential to exist; a Fleet is the other engine's
(kontra fleet up), which goes API -> Temporal -> infra worker so a converge leaves a Lease, a
run record and an audit trail. --stack kontra-fleet/<tag> is refused here rather than honoured.

THE BACKEND IS file://<this installation's .kontra>/state AND IT IS ASSERTED, NEVER ASSUMED. A
failed "pulumi login" does not stop pulumi — it creates an ephemeral Pulumi Cloud account and
deploys there, state included — so an environment that can reach a hosted backend is refused
before any operation runs.

--workspaces is the one key the program cannot guess (it is mounted at the SAME absolute path
inside and out, because a Source id is literally at:<absolute path>). Default: $KONTRA_WORKSPACES,
else <checkout>/workspaces, else <.kontra>/workspaces.

--bind and --api-port are sent ONLY when you type them; otherwise the program's own defaults stand,
which is what keeps Pulumi.yaml the single statement of the topology. --bind is the security
control: it is the publish address of all six published ports.
`)
}

// --- shared flags -------------------------------------------------------------------------------

// controlFlags is the FlagSet plus one typed pointer per flag, so every subcommand shares ONE
// registration point — `cli/fleet.go:121-154`'s shape, for the reason its own comment gives: two
// FlagSets are two places a flag can be documented in help and missing from the parser.
//
// `--preview`, `--json`, `--bind` and `--api-port` are meaningful on `up` ALONE, and unlike
// `fleet.go:132-134` (which answers the same situation with a comment) `cmdControlDown` REFUSES them
// rather than ignoring them. The asymmetry is deserved: `fleet down --force` ignored on the wrong verb
// is a no-op, and `control down --preview` ignored is a destroy somebody thought they were previewing.
type controlFlags struct {
	fs         *flag.FlagSet
	preview    *bool
	asJSON     *bool
	stack      *string
	program    *string
	workspaces *string
	bind       *string
	apiPort    *int
}

// controlFlagSet registers every flag this command family has.
//
// DEFAULTS THAT ARE NOT SENT: `--bind` and `--api-port` hold the values `control/pulumi/Pulumi.yaml`
// declares (`:123`, `:144`), and they are handed to `pulumi` only when `flagPassed` says the operator
// typed them. Carrying the same default in two files is a drift waiting to happen; holding it here
// only so `--help` can print something true, and letting the program own it otherwise, is the
// arrangement where the topology has one statement.
func controlFlagSet(verb string) *controlFlags {
	fs := flag.NewFlagSet("control "+verb, flag.ContinueOnError)
	return &controlFlags{
		fs:         fs,
		preview:    fs.Bool("preview", false, "print what WOULD change and change nothing (exits 0 only if nothing would)"),
		asJSON:     fs.Bool("json", false, "one JSON document on stdout instead of prose (--preview only)"),
		stack:      fs.String("stack", "", "which stack of this engine's one project (default: "+hostengine.DefaultStack+")"),
		program:    fs.String("program", "", "directory holding "+hostengine.ProgramFile+" (default: the checkout, else the installed copy)"),
		workspaces: fs.String("workspaces", "", "where the operator's code lives; mounted at this same absolute path inside the containers (default: $KONTRA_WORKSPACES, else <checkout>/workspaces)"),
		bind:       fs.String("bind", "127.0.0.1", "IP every published port binds to — sent only if you type it"),
		apiPort:    fs.Int("api-port", 8088, "the control plane's HTTP port — sent only if you type it"),
	}
}

// errWouldChange is `--preview`'s answer when the converge is not a no-op.
//
// A SENTINEL RATHER THAN A BARE STRING because `main`'s fork keys on `errors.Is` (`cli/main.go:282`)
// and a future third exit code — the honest one for "would change", distinct from "broke" — is one
// arm added there against this value. Until then it exits 1 like any other error, and the message says
// which kind of 1 it is.
var errWouldChange = errors.New("the converge would change this installation")

// cmdControlUp converges the control plane, or previews the same converge.
//
// THIS IS THE FUNCTION `case "up":` CALLS AFTER ISSUE 08 — see the file header. It therefore takes
// `args` exactly as the switch hands them and owns its whole flag surface.
//
// ── THE ORDER OF OPERATIONS IS THE CONTRACT, AND EVERY STEP BEFORE `pulumi` FAILS CLOSED ────────
//
//  1. Parse. Positional arguments are refused the way `cli/up.go:68-70` refuses them (and
//     `up_test.go:44` pins that the refusal QUOTES the argument), because `kontra up local` looks
//     like it names a stack and does not.
//  2. Resolve the stack THROUGH THE DISPATCH TABLE. `hostengine.ParseStack` is where a fleet project
//     is refused, and it happens before the backend is even computed: a request this engine must not
//     serve should not cause a state directory to be created.
//  3. Assert the backend. Fails closed on the three environments that can reach Pulumi Cloud.
//  4. Find and materialise the program, so `pulumi -C` points at a directory this engine owns rather
//     than at somebody's checkout (see hostengine.Layout for the two failures that forces).
//  5. Take the lock — CONVERGE ONLY. A preview writes nothing to the backend and takes none.
//  6. `stack select --create`, then `preview --json` or `up --yes`.
//
// ── THE FLAG SURFACE IS DELIBERATELY TWO KNOBS AND NOT SEVEN ───────────────────────────────────
//
// `Pulumi.yaml:109-292` has 27 config keys and seven of them are the appliance's flags under a
// different spelling — `--temporal-port`/`temporalPort`, `--kv-port`/`kvPort`, `--s3-port`/`s3Port`,
// `--registry-port`/`registryPort`, plus `postgresPort`, which the appliance has no equivalent for.
// Reconciling that spelling belongs with the change that retires the appliance's flags (issue 08);
// adding all seven here would mean two commands claiming the same knobs with different names in the
// same binary. `--bind` and `--api-port` are here because they are the two an operator changes on the
// day they install — one is the security control, the other is the URL they are about to be handed —
// and both are passed ONLY when typed (`flagPassed`, `cli/fleet.go:922`), so an untouched flag leaves
// the program's own default as the single statement of the topology rather than re-asserting it from
// a Go literal that can drift.
func cmdControlUp(args []string) error {
	f := controlFlagSet("up")
	if err := f.fs.Parse(args); err != nil {
		return err
	}
	if f.fs.NArg() > 0 {
		return fmt.Errorf("kontra control up takes no arguments (got %q) — a stack is named with --stack", f.fs.Arg(0))
	}
	if *f.asJSON && !*f.preview {
		// A converge streams pulumi's own output for minutes; there is no document to emit at the end
		// of it, and a flag that silently did nothing would be read as one that worked.
		return errors.New("--json describes a preview; use it with --preview")
	}

	eng, plan, err := controlEngine(*f.stack, *f.program, *f.workspaces)
	if err != nil {
		return err
	}
	if flagPassed(f.fs, "bind") {
		eng.Config["bind"] = *f.bind
	}
	if flagPassed(f.fs, "api-port") {
		eng.Config["apiPort"] = fmt.Sprint(*f.apiPort)
	}
	// WHAT THIS RUN IS ACTING ON, BEFORE IT ACTS, and on stderr in every mode including `--json` —
	// stdout stays the document, and the four lines that answer "why did that not do what I expected"
	// are as useful in a CI log as in a terminal.
	printPlan(plan)

	return converge(convergeOpts{
		eng:         eng,
		plan:        plan,
		verb:        "kontra control up",
		previewOnly: *f.preview,
		asJSON:      *f.asJSON,
	})
}

// --- THE converge. THERE IS ONE, AND `kontra update` IS THE OTHER CALLER --------------------------

// convergeOpts is everything the two commands differ by. They differ by four fields and nothing else,
// which is the whole of ADR 0052 §7's "it is not a second code path" as a type.
type convergeOpts struct {
	eng  *hostengine.Engine
	plan controlPlan
	// verb is what somebody typed, for the one refusal that has to name it back — `kontra update` on a
	// never-converged install has to say which command it is that cannot do that.
	verb string
	// previewOnly is `--preview` and `--check`: nothing is applied, no lock is taken (program.go:243
	// says why), and no state export is written.
	previewOnly bool
	asJSON      bool
	// requireConverged refuses an installation that has never converged. `kontra update` sets it: an
	// update of nothing is an install, and 40 creates reported as an upgrade is the one sentence that
	// would hide it.
	requireConverged bool
}

// converge takes the program from wherever this installation is to what the program declares — or
// says what that would take, and changes nothing.
//
// ── THE ORDER IS THE SAFETY, AND EVERY STEP BEFORE `up` CAN ONLY REFUSE ─────────────────────────
//
//  1. The lock (converge only).
//  2. `stack select --create`, because every step below needs a stack to exist.
//  3. `stack export` — the installation's own record of what it has, read before anything else asks a
//     question about it. Two things come out of it, in this order:
//     a. whether this install has EVER converged, because `kontra update` refuses if it has not;
//     b. on the converge path, a timestamped COPY, written and its path printed before anything is
//     applied. ADR 0052 §7 asks for it because `pulumi-state` is the one volume whose loss is not
//     local, and a protected resource is safe from Pulumi and not from `rm`.
//  4. THE VOLUME IDENTITY GATE, against that export. This is the failure `protect: true` cannot see: a
//     volume moved to another service is a plan with no volume step in it at all.
//  5. `preview --json`, and the SAME plan is checked for a delete or a replace of any volume — which
//     is how the refusal gets to name the volume instead of pulumi naming a URN.
//  6. `up --yes`. Then the report, then the console URL from the stack's own output.
//
// STEP 5 RUNS ON THE CONVERGE PATH TOO, which costs one extra `pulumi preview` (seconds against a
// resolved stack) and is the only way to refuse BEFORE the apply rather than during it. The window
// between that preview and the apply is real and the lock is what covers it for another `kontra`; a
// person running `pulumi` by hand in the same seconds is covered by `protect: true` and not by this.
func converge(o convergeOpts) error {
	if !o.previewOnly {
		release, err := hostengine.Lock(o.plan.layout.Dir)
		if err != nil {
			return err
		}
		defer release()
	}

	// The DECLARED side of the gate comes from the same bytes the engine is about to converge — the
	// program as resolved and materialised, never a re-read of the checkout.
	declared, err := hostengine.DeclaredVolumes(o.plan.source.YAML)
	if err != nil {
		return err
	}

	if err := o.eng.SelectStack(); err != nil {
		return err
	}

	raw, err := o.eng.ExportStack()
	if err != nil {
		return err
	}
	recorded, err := hostengine.RecordedVolumes(raw)
	if err != nil {
		return err
	}
	if o.requireConverged && !recorded.Converged {
		return fmt.Errorf("this installation has never been converged, so there is nothing to update.\n"+
			"  `%s` exists to move an EXISTING install to newer images and to refuse anything that would\n"+
			"  cost it a volume. On an empty stack it would be an install reported as an upgrade — 40\n"+
			"  resources created, described as a replacement — so it says so instead.\n"+
			"      kontra control up", o.verb)
	}
	if !o.previewOnly {
		path, err := hostengine.SaveExport(o.plan.layout.Dir, raw, time.Now())
		if err != nil {
			return err
		}
		// PRINTED ON EVERY RUN, and to stdout, because the path is the deliverable: it is what re-adopts
		// eleven existing docker volumes if this installation's own state directory is ever lost.
		fmt.Fprintf(cliio.Stdout, "\nstate export  %s\n", path)
	}

	if err := hostengine.CheckVolumeIdentity(declared, recorded); err != nil {
		return err
	}

	sum, err := o.eng.Preview()
	if err != nil {
		// A PREVIEW PULUMI REFUSED TO PLAN STILL CARRIES THE PLAN — measured, see
		// hostengine.PreviewError. If the thing it refused was a volume, the operator gets kontra's
		// sentence about which volume, with pulumi's own diagnostic under it rather than instead of it.
		var pe *hostengine.PreviewError
		if errors.As(err, &pe) && pe.Summary != nil {
			if vErr := hostengine.CheckPlanKeepsEveryVolume(pe.Summary, declared, recorded); vErr != nil {
				// BOTH SENTENCES, KONTRA'S FIRST. Dropping pulumi's would be hiding the tool's own words
				// from somebody debugging, and leading with it is what this gate exists to stop.
				return fmt.Errorf("%w\n\n  %v", vErr, err)
			}
		}
		return err
	}
	if err := hostengine.CheckPlanKeepsEveryVolume(sum, declared, recorded); err != nil {
		return err
	}

	if o.previewOnly && o.asJSON {
		// The document is the only thing on stdout in this mode, so the volume report is skipped rather
		// than printed to stderr beside it — `wouldChange` plus the exit code is what a gate reads.
		return writePreviewJSON(o.eng, sum)
	}
	printVolumes(declared, recorded)

	if o.previewOnly {
		printSummary(sum)
		if sum.WouldChange() {
			return fmt.Errorf("%w: %d of %d resources (exit 1). This exits 0 only when the converge is a "+
				"no-op; this same 1 is also what a preview that could NOT run returns, and that one says so "+
				"above", errWouldChange, sum.ChangedCount(), sum.Total)
		}
		fmt.Fprintln(cliio.Stderr, "No changes. This installation already matches the program.")
		return nil
	}

	if err := o.eng.Up(); err != nil {
		return err
	}
	printWhatHappened(sum, recorded)
	// ADR 0052 §3 (docs/adr/0052…:207-208) requires every boot to end by printing the console URL —
	// not only the first, "because the path is what an operator looks for on the second day". The URL
	// is a stack OUTPUT rather than a string built here: `Pulumi.yaml:1350` is
	// `consoleUrl: http://${bind}:${apiPort}`, so a changed port cannot leave this line pointing at
	// nothing. §3's other half, the credentials FILE path, belongs to the secrets seed (issue 05);
	// this command does not write that file and so does not claim it exists.
	url, err := o.eng.Output("consoleUrl")
	if err != nil {
		return err
	}
	fmt.Fprintf(cliio.Stdout, "\nControl plane: %s\n", url)
	return nil
}

// cmdControlDown destroys the control stack and says what it kept.
//
// IT SAYS WHAT SURVIVED BECAUSE THE DEFAULT IS THE SURPRISING ONE. All 11 volumes carry
// `retainOnDelete: true` and eight of them carry `protect: true` (ADR 0052 §7), so the data stays on
// disk twice over — measured at 41 GB across the 12 `kontra_` volumes on this box six days into one
// stack (`control/pulumi/README.md:327-330`). An operator who reads "destroyed" and expects an empty box
// gets a surprise in the other direction on the next `up`, when their data comes back.
//
// THE PROTECTION CHANGED WHAT THIS VERB HAD TO DO, and `hostengine.Engine.Destroy` carries the
// measurement: a plain `pulumi destroy` against a protected resource FAILS and leaves the containers
// running, so the destroy is `--exclude-protected` now. The printed line says which volumes are still
// in state as a consequence, because that is the one visible difference for somebody who has run this
// before.
func cmdControlDown(args []string) error {
	f := controlFlagSet("down")
	if err := f.fs.Parse(args); err != nil {
		return err
	}
	if f.fs.NArg() > 0 {
		return fmt.Errorf("kontra control down takes no arguments (got %q)", f.fs.Arg(0))
	}
	// NOTHING IS SILENTLY IGNORED. The flag set is shared (see controlFlagSet), so `--preview` parses
	// here — and a `kontra control down --preview` that destroyed the stack because the flag meant
	// nothing to this verb would be the worst refusal in this file to have skipped.
	for _, name := range []string{"preview", "json", "bind", "api-port"} {
		if flagPassed(f.fs, name) {
			return fmt.Errorf("--%s is a flag of `kontra control up`, not `down` — down destroys what the "+
				"program declares and has nothing to preview or to bind", name)
		}
	}
	eng, plan, err := controlEngine(*f.stack, *f.program, *f.workspaces)
	if err != nil {
		return err
	}
	release, err := hostengine.Lock(plan.layout.Dir)
	if err != nil {
		return err
	}
	defer release()
	if err := eng.SelectStack(); err != nil {
		return err
	}
	if err := eng.Destroy(); err != nil {
		return err
	}
	fmt.Fprintf(cliio.Stdout, "\nThe containers and the network are gone. KEPT: every volume — Postgres, the "+
		"object store,\nthe registry, the lake. All 11 carry retainOnDelete, so the data stays on disk, and 8 of\n"+
		"them carry protect: true (ADR 0052 §7), so those 8 are also STILL IN STATE — the destroy is\n"+
		"`--exclude-protected` and skipped them, which is why the next `up` re-adopts them rather than\n"+
		"re-creating the resources. The Pulumi state itself is in\n%s.\n"+
		"Deleting a volume is `docker volume rm` typed by a person.\n",
		filepath.Join(plan.layout.Root, hostengine.StateDirName))
	return nil
}

// controlPlan is what the command resolved before it ran anything, so it can be printed once and
// referred to twice.
type controlPlan struct {
	layout     hostengine.Layout
	backend    string
	source     hostengine.Source
	workspaces string
	wrote      []string
	stack      hostengine.StackRef
}

// controlEngine does every step that can fail without touching Docker or the backend, in the order
// cmdControlUp's header lists them.
func controlEngine(stack, program, workspaces string) (*hostengine.Engine, controlPlan, error) {
	var plan controlPlan

	// (2) The dispatch table, first, before anything exists on disk.
	ref, err := hostengine.ParseStack(stack)
	if err != nil {
		return nil, plan, err
	}
	plan.stack = ref

	root, err := cliutil.KontraRoot()
	if err != nil {
		return nil, plan, err
	}
	plan.layout = hostengine.NewLayout(root)

	// (3) The backend. Nothing below here may run if this refuses.
	backend, err := hostengine.AssertBackend(root, os.Getenv)
	if err != nil {
		return nil, plan, err
	}
	plan.backend = backend
	// The backend directory, which Pulumi will NOT create — measured, and the first `kontra up` on a
	// new machine dies with "unable to open bucket" without this. See hostengine.EnsureBackendDir.
	if err := hostengine.EnsureBackendDir(backend); err != nil {
		return nil, plan, err
	}

	// (4) The program. A missing checkout is not an error here — Resolve's fourth candidate is the
	// installed copy, which is what a machine that ran the installer has and a checkout does not.
	checkout, _ := cliutil.FindRepoRoot("")
	src, err := hostengine.Resolve(plan.layout, program, checkout, os.Getenv)
	if err != nil {
		return nil, plan, err
	}
	plan.source = src
	wrote, err := hostengine.Materialise(plan.layout, src)
	if err != nil {
		return nil, plan, err
	}
	plan.wrote = wrote

	ws, err := controlWorkspaces(workspaces, root, checkout)
	if err != nil {
		return nil, plan, err
	}
	plan.workspaces = ws

	pass, err := hostengine.Passphrase(plan.layout.Dir)
	if err != nil {
		return nil, plan, err
	}

	eng := &hostengine.Engine{
		Program:    plan.layout.Program,
		Backend:    backend,
		Passphrase: pass,
		Stack:      ref,
		Config: map[string]string{
			"workspaces": ws,
			// ABSOLUTE, AND STATED RATHER THAN INHERITED. The program's own default is `../images`
			// relative to itself (`Pulumi.yaml:290`), which is true of the materialised layout too —
			// but `uploads` reads its files with `fn::readFile` at program LOAD, and a converge that
			// fails there fails before any resource, naming a path the operator never wrote. Saying it
			// means the layout in hostengine.Layout is the one thing that has to be right.
			"assetsDir": plan.layout.Assets,
		},
		// Pattern A: the child's bytes go straight to the terminal, with nothing in front of them.
		// `cli/infra.go:99` does the same for `docker compose`, and `cliio.go:14-16` says why nothing
		// sits between a child's two streams and the operator.
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	}
	if err := eng.Ready(); err != nil {
		return nil, plan, err
	}
	return eng, plan, nil
}

// controlWorkspaces answers the one config key the program refuses to guess.
//
// `Pulumi.yaml:203-216` is explicit that a DEFAULT would be wrong THERE: "`${pulumi.cwd}` is wherever
// the program was materialised, which on an installed machine is not a checkout. A default would be a
// bind mount pointing at a directory nobody can find, and the symptom is a workspace selector with
// nothing in it." That argument is about the PROGRAM, which knows nothing about the machine. This
// function is allowed to answer it because it does: it has `$KONTRA_WORKSPACES` (the variable compose
// already used, `docker-compose.yml:39`), the checkout it is standing in, and this installation's
// `.kontra`.
//
// IT IS MADE ABSOLUTE AND CREATED. Absolute because the value is mounted at the SAME path inside and
// out in all three consumers — a Source id is literally `at:<absolute path>` and that id crosses the
// api→infra queue hop, so two roles disagreeing about it fail with `no registered source "at:/…"`.
// Created because Docker creates a missing bind-mount source itself, as root, and the first thing the
// operator then cannot do is write a workspace into their own directory.
func controlWorkspaces(flagVal, root, checkout string) (string, error) {
	dir := flagVal
	if dir == "" {
		dir = os.Getenv("KONTRA_WORKSPACES")
	}
	if dir == "" && checkout != "" {
		dir = filepath.Join(checkout, "workspaces")
	}
	if dir == "" {
		dir = filepath.Join(root, "workspaces")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return "", fmt.Errorf("workspaces directory %s: %w", abs, err)
	}
	return abs, nil
}

// printPlan states what this run is about to act on, to STDERR, before it acts.
//
// Four facts, because each one has been the answer to "why did that not do what I expected": which
// backend (the whole point of AssertBackend), which program and where it was found (`--program` vs the
// checkout vs the installed copy), which stack, and which workspaces directory. It is stderr and not
// stdout so `--json`'s document stays the only thing on stdout.
func printPlan(plan controlPlan) {
	w := tabwriter.NewWriter(cliio.Stderr, 2, 8, 2, ' ', 0)
	fmt.Fprintf(w, "project\t%s\n", hostengine.Project)
	fmt.Fprintf(w, "stack\t%s\n", plan.stack.Stack)
	fmt.Fprintf(w, "backend\t%s\n", plan.backend)
	fmt.Fprintf(w, "program\t%s (via %s)\n", filepath.Join(plan.source.Dir, hostengine.ProgramFile), plan.source.How)
	fmt.Fprintf(w, "workspaces\t%s\n", plan.workspaces)
	if len(plan.wrote) > 0 {
		fmt.Fprintf(w, "materialised\t%v -> %s\n", plan.wrote, plan.layout.Dir)
	}
	w.Flush()
}

// printSummary prints the per-operation counts, and names every resource a converge would REPLACE.
//
// THE REPLACEMENTS ARE NAMED AND THE CREATES ARE COUNTED, and that asymmetry is ADR 0052 §2's:
// *"this will replace `temporal`"* is the question somebody asks on the day they upgrade. A first
// converge is 40 creates and listing all 40 buries the one line that would have stopped somebody; a
// replacement is a container destroyed and rebuilt, so it gets a line of its own.
func printSummary(sum *hostengine.Summary) {
	w := tabwriter.NewWriter(cliio.Stdout, 2, 8, 2, ' ', 0)
	fmt.Fprintf(w, "\n%d resources previewed\n", sum.Total)
	for _, op := range sum.Ops() {
		fmt.Fprintf(w, "  %s\t%d\n", op, sum.Counts[op])
	}
	w.Flush()
	if rep := sum.Replaced(); len(rep) > 0 {
		fmt.Fprintf(cliio.Stdout, "\nWOULD REPLACE (destroyed and rebuilt):\n")
		for _, c := range rep {
			fmt.Fprintf(cliio.Stdout, "  %s\n", c)
		}
	}
}

// printVolumes states the volume position BEFORE anything is applied.
//
// IT PRINTS THE ASSERTION THAT PASSED, NOT ONLY THE ONE THAT FAILS. A gate that is silent when it is
// happy is a gate nobody knows is there, and the line an operator needs on the day they upgrade is
// "your data is accounted for" — `kontra_seaweed-data` is 34 GB on this box and `kontra update`
// replacing thirteen containers beside it is exactly when somebody wants to be told. The unprotected
// ones are named because §7 requires an unprotected volume to read as a decision, and a report that
// only ever says "all fine" would hide a protection somebody removed by hand.
func printVolumes(declared, recorded hostengine.VolumeSet) {
	w := tabwriter.NewWriter(cliio.Stderr, 2, 8, 2, ' ', 0)
	if !recorded.Converged {
		fmt.Fprintf(w, "\nvolumes\t%d declared, %d protected — nothing recorded yet, so this is a first converge\n",
			len(declared.ByName), declared.Protected())
		w.Flush()
		return
	}
	fmt.Fprintf(w, "\nvolumes\t%d recorded, %d protected — each still named what it was and still attached to the same service\n",
		len(recorded.ByName), recorded.Protected())
	if un := declared.Unprotected(); len(un) > 0 {
		fmt.Fprintf(w, "unprotected\t%s (deliberate — see the program)\n", strings.Join(un, ", "))
	}
	// A volume protected in the program and NOT in state has been unprotected by hand
	// (`pulumi state unprotect <urn>`). That is not refused — somebody doing surgery has a reason — but
	// it is the one thing in this report they must not be allowed to forget about.
	var lost []string
	for _, n := range recorded.Names() {
		if declared.ByName[n].Protected && !recorded.ByName[n].Protected {
			lost = append(lost, n)
		}
	}
	if len(lost) > 0 {
		fmt.Fprintf(w, "WARNING\t%s: the program protects these and this installation's state does not.\n",
			strings.Join(lost, ", "))
		fmt.Fprintf(w, "\tSomething ran `pulumi state unprotect`. The next converge that stops declaring one\n")
		fmt.Fprintf(w, "\tof them will delete it, and the gate above is then the only thing in the way.\n")
	}
	w.Flush()
}

// printWhatHappened is the report ADR 0052 §7 asks a converge to end with: what it replaced, and what
// it left alone.
//
// BOTH HALVES, AND THE SECOND ONE IS THE POINT. "Replaced 13 containers" on its own is the sentence
// that makes somebody check their data; naming the volumes that were not touched in the same breath is
// what makes the upgrade boring, which is what an upgrade should be.
func printWhatHappened(sum *hostengine.Summary, recorded hostengine.VolumeSet) {
	if rep := sum.Replaced(); len(rep) > 0 {
		fmt.Fprintf(cliio.Stdout, "\nREPLACED (destroyed and rebuilt):\n")
		for _, c := range rep {
			fmt.Fprintf(cliio.Stdout, "  %s\n", c)
		}
	}
	if recorded.Converged {
		fmt.Fprintf(cliio.Stdout, "\nLEFT ALONE: %d volumes, %d of them protected — %s.\n",
			len(recorded.ByName), recorded.Protected(), strings.Join(recorded.Names(), ", "))
	}
}

// writePreviewJSON is the channel for a caller that must tell "would change" from "could not run"
// without reading a sentence. One document on cliio.Stdout, the way `kontra build --json` and
// `kontra workflow replay --json` put one there.
func writePreviewJSON(eng *hostengine.Engine, sum *hostengine.Summary) error {
	type change struct {
		Op   string `json:"op"`
		Name string `json:"name"`
		Type string `json:"type"`
	}
	doc := struct {
		Project     string         `json:"project"`
		Stack       string         `json:"stack"`
		WouldChange bool           `json:"wouldChange"`
		Total       int            `json:"total"`
		Changed     int            `json:"changed"`
		Counts      map[string]int `json:"counts"`
		Changes     []change       `json:"changes"`
	}{
		Project:     hostengine.Project,
		Stack:       eng.Stack.Stack,
		WouldChange: sum.WouldChange(),
		Total:       sum.Total,
		Changed:     sum.ChangedCount(),
		Counts:      sum.Counts,
		Changes:     []change{},
	}
	for _, c := range sum.Changes {
		doc.Changes = append(doc.Changes, change{Op: c.Op, Name: c.Name, Type: c.Type})
	}
	enc := json.NewEncoder(cliio.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return err
	}
	// THE EXIT CODE IS STILL THE ANSWER even with --json, so a `&&` in a shell script and a `jq` in a
	// pipeline agree. A document on stdout plus exit 0 would make the gate silently pass.
	if sum.WouldChange() {
		return fmt.Errorf("%w: %d of %d resources", errWouldChange, sum.ChangedCount(), sum.Total)
	}
	return nil
}
