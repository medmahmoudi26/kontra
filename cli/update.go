// update.go — `kontra update`, ADR 0052 §7.
//
// Re-resolve the control stack's image digests, preview, refuse-or-converge, report. THE SAME PROGRAM,
// THE SAME ENGINE, A DIFFERENT DESIRED STATE — and, deliberately, the same function: `converge` in
// `cli/control.go` is what `kontra control up` ends in and what this command ends in, and the two
// differ by the four fields of `convergeOpts` and nothing else. §7 is explicit that it "is not a second
// code path", and the reason is not tidiness: the converge is where the volume gate, the state export
// and the lock live, and a second path is a path with one of them missing.
//
// ── WHY THIS COMMAND EXISTS AT ALL ──────────────────────────────────────────────────────────────
//
// §7's first sentence: "An install nobody can upgrade is an install people pin and abandon." The
// compose install's upgrade is `docker compose pull && up -d`, which replaces containers, keeps
// volumes, and VERIFIES NOTHING — ADR 0052 §3 says so about itself. This is where the verification
// lives.
//
// ── THE THING THAT MAKES AN UPGRADE FRIGHTENING, AND THE TWO ANSWERS ────────────────────────────
//
// ADR 0052 §1's warning at its sharpest: PULUMI'S DESIRED STATE IS TOTAL. A converge that no longer
// mentions a volume does not leave it alone, it deletes it. On a Fleet that costs a Machine; here it
// costs every Dataset the install has ever produced. The answers are in two different files on
// purpose:
//
//   - `control/pulumi/Pulumi.yaml` carries `protect: true` on the eight volumes that hold something
//     nothing else holds. Measured on 3.244.0: the plan fails in PREVIEW, so the program cannot express
//     the destruction for ANY caller — including a person standing in that directory running `pulumi`
//     with the wrong stack selected. Nothing in Go is load-bearing for that, which is the point of
//     putting it there.
//   - `cli/internal/hostengine/volumes.go` asserts volume IDENTITY against `pulumi stack export`,
//     because the dangerous plan is the one with no delete in it: a volume renamed, or moved to another
//     service, comes up healthy and empty.
//
// ── SCHEMA MIGRATION IS OUT OF SCOPE, AND IT FAILS LOUDLY RATHER THAN HALF-MIGRATING ───────────
//
// §7 names this gap rather than filling it: Temporal's auto-setup owns its own schema; `orchestrator-db`
// (SQLite, the actors/workflows/sources tables) and the DuckLake catalog in Postgres do not, and
// NOTHING HERE MIGRATES EITHER. What this command does to them is replace the container that reads
// them and leave the volume exactly as it was — which is the honest behaviour and is also the one that
// breaks if an image expects a shape the volume does not have.
//
// SO THE FAILURE MODE IS DELIBERATE: the new image starts, finds a schema it does not understand, and
// fails its own health gate. `Pulumi.yaml`'s ten explicit `waitTimeout`s turn that into a failed
// converge with the container's own error in it. That is the loud failure §7 asks for, and it is
// strictly better than the alternative this command deliberately does not attempt — a half-migration,
// which leaves a volume in a shape neither the old image nor the new one can read, with the data still
// there and nothing able to open it. Do not add a migration step here without an ADR: the decision §7
// declines to make is WHAT should happen, not whether somebody could write the code.
package main

import (
	"errors"
	"flag"
	"fmt"
	"strings"

	"github.com/medmahmoudi26/kontra/cli/internal/cliio"
	"github.com/medmahmoudi26/kontra/cli/internal/hostengine"
)

// cmdUpdate re-resolves the control stack's images and converges, or says what that would change.
//
// ── THE FLAG SURFACE IS THREE KNOBS, AND TWO OF THEM ARE §7's OWN SHAPE ────────────────────────
//
//	kontra update                # resolve digests, preview, refuse-or-converge, report
//	kontra update --check        # preview only, exit non-zero if anything would change
//	kontra update --to <tag>     # a specific release rather than whatever the program names
//
// `--stack`, `--program` and `--workspaces` are here because they are `kontra control up`'s and this is
// the same converge against the same installation; a command that could not be pointed at the same
// stack as the one that installed it would be a different tool. Everything else — `--bind`,
// `--api-port` — is deliberately absent: an update is not the moment to move the port the console is
// on, and the program's own config is where the topology is stated (`Pulumi.yaml:109-292`).
func cmdUpdate(args []string) error {
	// `help` BEFORE THE FLAG SET, the way `cmdControl` and `cmdFleet` take it: `flag`'s own `-h` prints
	// the defaults and returns ErrHelp, which `main` reports as `error: flag: help requested` and exit 1
	// — true of every command in this binary and not worth fixing here, but a command whose two honest
	// limits are prose (the floating-tag one and the schema one) needs a surface that prints them.
	if len(args) == 1 && (args[0] == "help" || args[0] == "-h" || args[0] == "--help") {
		updateUsage()
		return nil
	}
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	check := fs.Bool("check", false, "preview only; exit non-zero if anything would change (for CI)")
	to := fs.String("to", "", "the release tag to move every kontra-owned image to (default: what the program names)")
	stack := fs.String("stack", "", "which stack of this engine's one project (default: "+hostengine.DefaultStack+")")
	program := fs.String("program", "", "directory holding "+hostengine.ProgramFile+" (default: the checkout, else the installed copy)")
	workspaces := fs.String("workspaces", "", "where the operator's code lives (default: $KONTRA_WORKSPACES, else <checkout>/workspaces)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		// `cli/up.go:68-70`'s refusal, and `cli/up_test.go:44` pins that it QUOTES the argument. Here the
		// likely mistake is `kontra update v1.4.0`, which reads like it names a release and does not.
		return fmt.Errorf("kontra update takes no arguments (got %q) — a release is named with --to, a "+
			"stack with --stack", fs.Arg(0))
	}
	if flagPassed(fs, "to") && strings.TrimSpace(*to) == "" {
		return errors.New("--to needs a tag (e.g. --to v1.4.0); an empty one would retag every image to `:`")
	}

	eng, plan, err := controlEngine(*stack, *program, *workspaces)
	if err != nil {
		return err
	}

	// ── `--refresh`, ALWAYS, AND THAT IS WHAT "RE-RESOLVE THE DIGESTS" MEANS HERE ────────────────
	//
	// Without it the `docker.RemoteImage`s are diffed against the CHECKPOINT — the digest recorded the
	// last time this stack converged — so a daemon holding newer bytes under the same floating tag
	// reports no changes and an update silently does nothing. With it the provider re-reads the daemon.
	// `hostengine/images.go` has the measurement and the honest limit: a bare update cannot invent new
	// bytes for an unchanged tag, and pulling is the operator's decision.
	eng.Refresh = true

	if *to != "" {
		refs, err := hostengine.ImageRefs(plan.source.YAML)
		if err != nil {
			return err
		}
		retagged, err := hostengine.RetagAll(refs, *to)
		if err != nil {
			return err
		}
		for k, v := range retagged {
			eng.Config[k] = v
		}
		printRetag(refs, retagged)
	}

	printPlan(plan)
	if *to == "" {
		// SAID OUT LOUD, because the alternative is an operator concluding that `kontra update` is
		// broken. It is the same sentence images.go's header carries, at the one moment it matters.
		fmt.Fprintln(cliio.Stderr, "\nno --to, so the images are whatever the program names. `--refresh` "+
			"re-reads them from\nthis machine's docker daemon, which cannot invent newer bytes for an "+
			"unchanged floating tag —\n`docker pull` first, or name a release with --to.")
	}

	return converge(convergeOpts{
		eng:              eng,
		plan:             plan,
		verb:             "kontra update",
		previewOnly:      *check,
		requireConverged: true,
	})
}

// printRetag shows every image `--to` moved, and it prints the ones it did NOT move too.
//
// FIVE LINES RATHER THAN ONE, because `--to v1.4.0` is a claim about a set of images whose membership
// is read out of the program (`images.go`: the fifth one arrived mid-slice) and an operator has no other
// way to see which images an update is actually about to change. A `+ unchanged` line is not noise: an
// image already at the requested tag is the difference between "this upgrade is a no-op" and "this
// upgrade did not reach that service".
func printRetag(before, after map[string]string) {
	fmt.Fprintln(cliio.Stderr, "\nimages")
	for _, k := range hostengine.ImageKeys(after) {
		if before[k] == after[k] {
			fmt.Fprintf(cliio.Stderr, "  %-20s %s (unchanged)\n", k, after[k])
			continue
		}
		fmt.Fprintf(cliio.Stderr, "  %-20s %s\n  %-20s   -> %s\n", k, before[k], "", after[k])
	}
}

// updateUsage is printed for `kontra update help`, and it is the one place the two honest limits of
// this command are written down for somebody who is about to run it.
func updateUsage() {
	fmt.Fprint(cliio.Stdout, `kontra update — move this installation to newer images (ADR 0052 §7)

  kontra update                    # re-resolve the digests, preview, refuse-or-converge, report
  kontra update --check            # preview only; exit non-zero if anything would change
  kontra update --to <tag>         # move every kontra-owned image to one release tag
        [--stack local] [--program <dir>] [--workspaces <dir>]

THE SAME CONVERGE AS "kontra control up". Same program, same engine, same lock, same volume gate —
a different desired state. An update that took a second code path would be a path with one of those
missing.

EVERY VOLUME IS ACCOUNTED FOR BEFORE ANYTHING IS APPLIED, twice and in two different ways. Eight of
the eleven carry protect: true in the program, so pulumi refuses to plan their deletion at all —
measured: it fails in preview, for any caller, including pulumi run by hand. And the identity of
every volume this installation already has is asserted against "pulumi stack export": still named
what it was, still attached to the same service. That second one is not redundant — a volume MOVED to
another service is a plan with no volume step in it at all, so nothing refuses it and the stack comes
up healthy and empty.

"pulumi stack export" IS WRITTEN BEFORE EVERY CONVERGE, timestamped, and its path printed. That is
for kontra_pulumi-state, the one volume whose loss is not local: it records every cloud Machine the
control plane owns, so losing it loses the ability to destroy Machines that keep billing. A protected
resource is safe from Pulumi and is not safe from rm.

SCHEMA MIGRATION IS NOT DONE HERE, and that is ADR 0052 §7's decision rather than an omission.
Temporal migrates itself; orchestrator-db and the DuckLake catalog do not. An image that expects a
shape its volume does not have fails its own health gate and fails the converge with that error in
it, which is loud — a half-migration would leave the data readable by nobody.
`)
}
