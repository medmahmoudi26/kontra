package main

// Registering a folder — the act that is neither serving it nor running it.
//
// ── WHY THIS IS ITS OWN VERB ──────────────────────────────────────────────────────────────────
//
// Before this, code became known to kontra by being STARTED. A worker booted, POSTed itself to
// `/api/actors`, and created its own Nexus endpoint on the way past; a workflow became visible by
// being served. That coupling has three costs, and all three were being paid:
//
//   • You could not declare that code exists here without running it. There is no way to say "this
//     checkout is the probe I mean" and then write a caller against it — the endpoint the caller
//     dispatches through would not exist until somebody started the worker.
//   • What was known about an Actor was whatever a RUNNING process said about itself, so when the
//     process stopped, the claim did not become stale, it became absent — the Actors page emptied.
//   • Nothing owned the cluster state. Thirty-one Nexus endpoints on this installation outlived
//     every worker that made them, indistinguishable from live routes.
//
// So: `kontra actor register <dir>` and `kontra workflow register <dir>` record the path, the
// manifest, the version and a content digest in the orchestrator's SQLite, and create the Actor's
// Nexus endpoint. `serve` and `dispatch` are then separate things you may or may not do.
//
// ── WHAT IT REFUSES ───────────────────────────────────────────────────────────────────────────
//
// A folder with no `actor.json` / `workflow.json`. A registration carries a version and a digest,
// and a directory declares neither — so the manifest is the file that makes a folder registrable,
// the way the marker is the file that makes it that KIND of folder. `--init` writes a starter
// manifest for a workflow rather than making the operator look up the shape.

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/medmahmoudi26/kontra/cli/internal/cliio"
)

// cmdRegister serves both `kontra actor register` and `kontra workflow register`.
//
// ONE IMPLEMENTATION, TWO VERBS, because the difference between the kinds is a string in the URL
// and a manifest filename — and two copies would drift in exactly the place that matters, which is
// what the two commands promise the operator they have recorded.
func cmdRegister(kind string, args []string) error {
	// REGISTERING IS GONE AND THIS EXPLAINS ITSELF, the way `kontra actor <ref> dispatch` does.
	//
	// The workspace is the registration: a folder under `<workspace>/actors/` or
	// `<workspace>/workflows/` is served, and nothing else is recorded anywhere. What that removes
	// is a class of state nobody wanted — a recorded path outlives the directory it names, and an
	// install accumulated entries for folders deleted weeks earlier, each listing and rendering and
	// then failing with `404 no such workflow` on the first click.
	//
	// `--init` SURVIVES AS ITS OWN VERB, because writing a starter manifest is a real service and
	// has nothing to do with registration: `kontra <kind> init <dir>`.
	dir, rest := leadingPositional(args)
	if strings.TrimSpace(dir) == "" {
		for _, arg := range rest {
			if !strings.HasPrefix(arg, "-") {
				dir = arg
				break
			}
		}
	}
	where := strings.TrimSpace(dir)
	if where == "" {
		where = "<dir>"
	}
	plural := kind + "s"
	return fmt.Errorf(
		"`kontra %s register` is gone: the workspace IS the registration.\n"+
			"Put the folder in the workspace and it is served:\n"+
			"  kontra workspace path            # where the workspace is mounted\n"+
			"  mv %s \"$(kontra workspace path)/%s/\"\n"+
			"Nothing is recorded, so deleting the folder removes it — no `forget`, and no entry\n"+
			"left pointing at code that is not there any more.\n"+
			"To write a starter manifest: kontra %s init %s",
		kind, where, plural, kind, where)
}

// cmdSourceInit writes the starter manifest a folder is missing — the half of `register` that was
// a real service, kept as its own verb.
//
// IT DOES NOT TELL THE CONTROL PLANE ANYTHING, and that is the change: a manifest makes a folder
// well-formed, and being in the workspace is what makes it SERVED. The two were one command and
// the coupling is what produced entries for code nobody could open.
func cmdSourceInit(kind string, args []string) error {
	dir, rest := leadingPositional(args)
	fs := flag.NewFlagSet(kind+" init", flag.ContinueOnError)
	version := fs.String("version", "", "the version to declare (default 0.1.0)")
	workflowType := fs.String("workflow", "", "workflows only: the @workflow.defn class (default: read from the code)")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if strings.TrimSpace(dir) == "" {
		dir = fs.Arg(0)
	}
	if strings.TrimSpace(dir) == "" {
		return fmt.Errorf("usage: kontra %s init <dir> [--version X.Y.Z]", kind)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if st, err := os.Stat(abs); err != nil {
		return fmt.Errorf("%s: %w", abs, err)
	} else if !st.IsDir() {
		return fmt.Errorf("%s is a file — init the folder that contains it", abs)
	}
	written, err := writeStarterManifest(kind, abs, *version, *workflowType)
	if err != nil {
		return err
	}
	if written == "" {
		fmt.Fprintf(cliio.Stdout, "%s already has a %s\n", abs, manifestFile(kind))
	} else {
		fmt.Fprintf(cliio.Stdout, "wrote %s\n", written)
	}
	// SAY WHERE IT HAS TO BE, every time. A well-formed folder outside the workspace is invisible to
	// the control plane, and the failure mode is silence — it simply does not appear.
	if root := activeWorkspace(); root != "" && !strings.HasPrefix(abs+string(filepath.Separator), filepath.Join(root, kind+"s")+string(filepath.Separator)) {
		fmt.Fprintf(cliio.Stdout, "NOT in the workspace — move it to be served:\n  mv %s %s/\n",
			abs, filepath.Join(root, kind+"s"))
	}
	return nil
}

// manifestFile is the file registration reads for a kind. Peer of control/orchestrator/src/sources.ts's
// MANIFEST — two derivations of one contract, and a drift here is a CLI that writes a starter file
// the server does not look for.
func manifestFile(kind string) string {
	if kind == "workflow" {
		return "workflow.json"
	}
	return "actor.json"
}

// writeStarterManifest creates the manifest a folder is missing, and returns what it wrote (or ""
// when there was already one).
//
// NEVER OVERWRITES. `--init` on a folder that has a manifest is a no-op, not a reset: the file
// holds the version an operator chose and possibly schemas they wrote, and a flag whose name says
// "initialise" must not be the way those are lost.
func writeStarterManifest(kind, dir, version, workflowType string) (string, error) {
	file := filepath.Join(dir, manifestFile(kind))
	if _, err := os.Stat(file); err == nil {
		return "", nil
	}
	if version == "" {
		version = "0.1.0"
	}
	manifest := map[string]any{
		"name":    filepath.Base(dir),
		"version": version,
	}
	if kind == "workflow" {
		manifest["entry"] = "workflow.py"
		cls := workflowType
		if cls == "" {
			cls = workflowClassIn(filepath.Join(dir, "workflow.py"))
		}
		if cls == "" {
			// REFUSE RATHER THAN GUESS. `kontra workflow start` takes this exact string, and a
			// wrong one is a start call that waits on a type nobody registered — which looks like a
			// worker problem, hours later, on the wrong machine.
			return "", errors.New(
				"could not find an @workflow.defn class in workflow.py — pass --workflow <ClassName>")
		}
		manifest["workflow"] = cls
	}
	body, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(file, append(body, '\n'), 0o644); err != nil {
		return "", err
	}
	return file, nil
}

// The class immediately under an `@workflow.defn` decorator. Peer of the browser's
// `workflowSource.ts:typeFromSource`, and deliberately the same shape of rule: a regex over the
// source rather than a Python parse, because the CLI must not need an interpreter to read a name.
var workflowDefn = regexp.MustCompile(`(?m)^@workflow\.defn[^\n]*\n(?:@[^\n]*\n)*class\s+([A-Za-z_][A-Za-z0-9_]*)`)

func workflowClassIn(file string) string {
	src, err := os.ReadFile(file)
	if err != nil {
		return ""
	}
	if m := workflowDefn.FindSubmatch(src); m != nil {
		return string(m[1])
	}
	return ""
}
