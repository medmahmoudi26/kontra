// rebase.go — `kontra rebase`: moving an actor image onto a newer digest of the same runtime major,
// without rebuilding it.
//
// WHAT IT IS FOR. A runtime is the OS and system packages every actor built on it runs on. Patching
// one used to mean rebuilding every actor that used it — which is the whole fleet, each build
// reinstalling its own dependencies. `pack rebase` rewrites a manifest instead: the deps and app
// layers are untouched and only the run-image layers below them change. Seconds, not hours, and the
// deps layer digests are provably the same ones.
//
// IT DOES NOT CHANGE THE VERSION, AND THAT IS THE WHOLE DESIGN DECISION. ADR 0032's "the digest is
// the identity" holds; what changes is that a VERSION no longer names one digest. The catalog's
// `history` keeps the rest of the sequence, and the `inuse-` reconciler tags all of it — a Lease keeps
// its old digest until it drops, so the predecessor has to stay protected or retention takes the image
// an in-flight Run is still pulling.
//
// DETECTION IS A COMPARISON AND NOT A SUBSCRIPTION. Every catalog entry records the runtime digest its
// build resolved (`de21cfd`); a runtime's `:<major>` tag moves when it is patched. An entry whose
// recorded digest differs from what that tag resolves to now is behind, and that is all "rebase
// available" means. Nothing has to be notified, and a runtime published while the orchestrator was
// down is found on the next look.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/medmahmoudi26/kontra/cli/internal/cliio"
)

const rebaseUsage = `usage: kontra rebase [<actor>[@<version>]] [--runtime <name>:<major>] [--dry-run]

  Moves an actor image onto the current digest of the SAME runtime major, by rewriting its manifest.
  The version does not change; the digest does, and the one it replaces is kept in the catalog's
  history so retention cannot take it while a Lease is still running it.

  With no argument, lists every version that is behind its runtime and rebases nothing.

  <actor>[@<version>]      rebase one actor, or one version of it
  --runtime <name>:<major> rebase every version built on that runtime
  --dry-run                say what would be rebased
  --registry <host:port>   the registry to read and write (default: --registry / KONTRA_REGISTRY)
`

// rebaseTimeout bounds one manifest rewrite. A rebase moves no layers, so this is generous by an
// order of magnitude — it exists so a wedged registry does not hang an operator's terminal.
const rebaseTimeout = 10 * time.Minute

// behind is one version whose image is not on the current digest of its runtime major.
type behind struct {
	Actor   string
	Version string
	Image   string // the actor image reference this rebase rewrites
	Runtime resolvedRuntime
	Was     string // the runtime digest the build recorded
	Digest  string // the actor image digest, before
}

func cmdRebase(args []string) error {
	fs := flag.NewFlagSet("rebase", flag.ContinueOnError)
	fs.SetOutput(cliio.Stdout)
	fs.Usage = func() { fmt.Fprint(cliio.Stdout, rebaseUsage) }
	runtimeOnly := fs.String("runtime", "", "rebase every version built on this runtime (name:major)")
	dryRun := fs.Bool("dry-run", false, "say what would be rebased and rebase nothing")
	regFlag := fs.String("registry", "", "the registry to read and write")
	if err := fs.Parse(args); err != nil {
		return err
	}
	target := strings.TrimSpace(fs.Arg(0))
	if target != "" && *runtimeOnly != "" {
		// Two answers to one question. Resolving it silently by precedence is how an operator ends up
		// rebasing a fleet when they meant one actor.
		return errors.New("give an actor or --runtime, not both")
	}
	reg := registryAddress(*regFlag)

	ctx, cancel := context.WithTimeout(context.Background(), rebaseTimeout)
	defer cancel()

	var actors []actorRecord
	if err := newAPI(orchestratorURL()).getJSON("/api/actors", &actors); err != nil {
		return fmt.Errorf("GET /api/actors failed: %w", err)
	}
	rows, err := rebaseCandidates(ctx, reg, actors, target, *runtimeOnly)
	if err != nil {
		return err
	}

	out := cliio.Stdout
	if len(rows) == 0 {
		fmt.Fprintln(out, "every catalogued version is on the current digest of its runtime")
		return nil
	}

	w := tabwriter.NewWriter(out, 2, 8, 2, ' ', 0)
	fmt.Fprintln(w, "ACTOR\tVERSION\tRUNTIME\tBUILT ON\tNOW")
	for _, r := range rows {
		fmt.Fprintf(w, "%s\t%s\t%s:%d\t%s\t%s\n", r.Actor, r.Version, r.Runtime.Name, r.Runtime.Major,
			short(r.Was), short(r.Runtime.Digest))
	}
	if err := w.Flush(); err != nil {
		return err
	}

	// LISTING IS THE DEFAULT WHEN NOTHING WAS NAMED. `kontra rebase` with no argument is the question
	// "what is behind", and answering it by rewriting every manifest on the install would be the most
	// expensive possible reading of a bare verb.
	if *dryRun || (target == "" && *runtimeOnly == "") {
		fmt.Fprintf(out, "\n%d version(s) behind. Name one, or --runtime <name>:<major>, to rebase.\n", len(rows))
		return nil
	}

	done, failed := 0, 0
	for _, r := range rows {
		if err := rebaseOne(ctx, reg, r, out); err != nil {
			fmt.Fprintf(out, "%s@%s: %v\n", r.Actor, r.Version, err)
			failed++
			continue
		}
		done++
	}
	fmt.Fprintf(out, "\n%d rebased, %d failed\n", done, failed)
	if failed > 0 {
		return fmt.Errorf("%d version(s) could not be rebased", failed)
	}
	return nil
}

// rebaseCandidates is the detection, and it is a pure comparison over what the catalog recorded.
//
// An entry with no recorded runtime is SKIPPED rather than guessed at: it was built before the field
// existed, and rebasing it would mean choosing a runtime on its author's behalf and putting an OS
// under their actor that they never named.
func rebaseCandidates(ctx context.Context, reg string, actors []actorRecord, target, runtimeOnly string) ([]behind, error) {
	wantActor, wantVersion := "", ""
	if target != "" {
		wantActor, wantVersion, _ = strings.Cut(target, "@")
	}
	wantRuntime, wantMajor := "", uint32(0)
	if runtimeOnly != "" {
		n, m, _, err := parseRuntimeDeclaration(runtimeOnly, reg)
		if err != nil {
			return nil, err
		}
		wantRuntime, wantMajor = n, m
	}

	// RESOLUTION IS INJECTED, so the comparison that decides what gets rewritten is a pure function
	// over what the catalog recorded — testable without a registry, which is the same reason the
	// Warden's prune splits its decision from its engine.
	//
	// One resolution per runtime major, not one per actor: a hundred actors on `python:1` ask the
	// registry once.
	resolved := map[string]resolvedRuntime{}
	resolve := func(decl string) (resolvedRuntime, error) {
		if r, ok := resolved[decl]; ok {
			return r, nil
		}
		r, err := resolveRuntime(reg, decl, "")
		if err != nil {
			resolved[decl] = resolvedRuntime{}
			return resolvedRuntime{}, err
		}
		if err := admitRuntime(ctx, r, cliio.Stdout); err != nil {
			return resolvedRuntime{}, err
		}
		resolved[decl] = r
		return r, nil
	}
	host, _ := registryHost(reg)
	return behindRuntimes(actors, rebaseFilter{
		Actor: wantActor, Version: wantVersion, Runtime: wantRuntime, Major: wantMajor,
	}, host, resolve, cliio.Stdout)
}

// rebaseFilter is what the operator named, already split.
type rebaseFilter struct {
	Actor   string
	Version string
	Runtime string
	Major   uint32
}

// behindRuntimes is the comparison, and it is the whole of "rebase available": an entry whose recorded
// runtime digest differs from what that runtime's major resolves to NOW.
//
// AN ENTRY WITH NO RECORDED RUNTIME IS SKIPPED RATHER THAN GUESSED AT. It was built before the field
// existed, and rebasing it would mean choosing a runtime on its author's behalf — putting an OS under
// their actor that they never named.
func behindRuntimes(
	actors []actorRecord,
	want rebaseFilter,
	host string,
	resolve func(decl string) (resolvedRuntime, error),
	notes io.Writer,
) ([]behind, error) {
	var out []behind
	for _, a := range actors {
		if a.Runtime == nil || a.Runtime.Name == "" || a.Digest == "" {
			continue
		}
		if want.Actor != "" && a.Name != want.Actor {
			continue
		}
		if want.Version != "" && a.Version != want.Version {
			continue
		}
		if want.Runtime != "" && (a.Runtime.Name != want.Runtime || a.Runtime.Major != want.Major) {
			continue
		}
		decl := a.Runtime.Name + ":" + strconv.FormatUint(uint64(a.Runtime.Major), 10)
		cur, err := resolve(decl)
		if err != nil {
			// A runtime that has gone missing is NOT a rebase candidate, and it is not silence either:
			// an actor is running on an image its own registry can no longer name.
			if notes != nil {
				fmt.Fprintf(notes, "note: %s@%s is built on %s, which cannot be resolved: %v\n",
					a.Name, a.Version, decl, err)
			}
			continue
		}
		if cur.Digest == "" || cur.Digest == a.Runtime.Digest {
			continue
		}
		out = append(out, behind{
			Actor: a.Name, Version: a.Version,
			Image:   host + "/" + a.Name + ":" + a.Version,
			Runtime: cur, Was: a.Runtime.Digest, Digest: a.Digest,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Actor != out[j].Actor {
			return out[i].Actor < out[j].Actor
		}
		return out[i].Version < out[j].Version
	})
	return out, nil
}

// rebaseOne rewrites one manifest and records what it produced.
//
// THE REGISTRY IS THE AUTHORITY ON THE NEW DIGEST, as it is for a push: `pack rebase` reports one and
// `confirmPushed` asks the registry, because an image the catalog names and the registry does not is
// the one failure every surface would agree about.
func rebaseOne(ctx context.Context, reg string, r behind, progress io.Writer) error {
	bin, err := packBinary()
	if err != nil {
		return err
	}
	argv := []string{"rebase", r.Image, "--run-image", r.Runtime.Pinned(), "--publish"}
	if host, plain := registryHost(reg); plain && host != "" {
		argv = append(argv, "--insecure-registry", host)
	}
	fmt.Fprintf(progress, "rebasing %s@%s onto %s\n", r.Actor, r.Version, short(r.Runtime.Digest))
	cmd := exec.CommandContext(ctx, bin, argv...)
	cmd.Stdout, cmd.Stderr = progress, progress
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("pack rebase: %w", err)
	}

	digest, err := confirmPushed(reg, r.Actor, r.Version, r.Image, "")
	if err != nil {
		return err
	}
	if digest == r.Digest {
		// A rebase that changed nothing is not a success to report as one: the manifest was already on
		// that run image, or `pack` wrote nothing, and either way the catalog must not record a move.
		return fmt.Errorf("the registry still reports %s, so nothing was rewritten", short(digest))
	}

	// The catalog learns BOTH facts at once — the new image digest and the runtime it now sits on —
	// through the digest route, which keeps the previous digest in `history` itself.
	body := map[string]any{
		"name": r.Actor, "version": r.Version, "digest": digest,
		"runtime": map[string]any{"name": r.Runtime.Name, "major": r.Runtime.Major, "digest": r.Runtime.Digest},
	}
	if err := newAPI(orchestratorURL()).postJSON("/api/actors/"+r.Actor+"@"+r.Version+"/digest", body, nil); err != nil {
		return fmt.Errorf("rebased to %s but the catalog did not record it (the old digest is still "+
			"current there, and a placement will keep using it): %w", short(digest), err)
	}
	fmt.Fprintf(progress, "  %s@%s is now %s (was %s)\n", r.Actor, r.Version, short(digest), short(r.Digest))
	return nil
}
