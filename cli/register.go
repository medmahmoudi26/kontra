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
)

// registerResult is what `POST /api/sources/:kind` answers with. `endpointState` and
// `endpointError` are the endpoint half of the act — see server.ts.
type registerResult struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	Path        string `json:"path"`
	Version     string `json:"version"`
	Description string `json:"description"`
	Digest      string `json:"digest"`
	Endpoint    string `json:"endpoint"`
	// "created" — this call made it; "existed" — it was already there, which is the normal answer
	// for a re-register and for an Actor whose worker booted first.
	EndpointState string `json:"endpointState"`
	EndpointError string `json:"endpointError"`
}

// cmdRegister serves both `kontra actor register` and `kontra workflow register`.
//
// ONE IMPLEMENTATION, TWO VERBS, because the difference between the kinds is a string in the URL
// and a manifest filename — and two copies would drift in exactly the place that matters, which is
// what the two commands promise the operator they have recorded.
func cmdRegister(kind string, args []string) error {
	// THE DIRECTORY COMES OFF THE FRONT FIRST. Go's flag package stops parsing at the first
	// positional, so `register <dir> --init` — the way every other CLI reads, subject then options —
	// parses as zero flags and leaves `--init` as a stray argument, silently. `leadingPositional`
	// is what the rest of this CLI already uses for the same reason.
	dir, rest := leadingPositional(args)
	fs := flag.NewFlagSet(kind+" register", flag.ContinueOnError)
	initManifest := fs.Bool("init", false, "write a starter "+manifestFile(kind)+" if the folder has none")
	version := fs.String("version", "", "with --init: the version to declare (default 0.1.0)")
	workflowType := fs.String("workflow", "", "with --init, workflows only: the @workflow.defn class (default: read from the code)")
	apiURL := fs.String("api", orchestratorURL(), "orchestrator base URL")
	jsonOut := fs.Bool("json", false, "print the registration as JSON")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	// `--init <dir>` — options first — still works: the positional lands after the flags instead.
	if strings.TrimSpace(dir) == "" {
		dir = fs.Arg(0)
	}
	if strings.TrimSpace(dir) == "" {
		return fmt.Errorf("usage: kontra %s register <dir> [--init] [--json]", kind)
	}

	// ABSOLUTE, HERE. The orchestrator resolves the path against ITS filesystem, and a relative
	// path means the shell's cwd — two different directories the moment the control plane is in a
	// container. Resolving before sending is what makes `kontra actor register .` mean this one.
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if st, err := os.Stat(abs); err != nil {
		return fmt.Errorf("%s: %w", abs, err)
	} else if !st.IsDir() {
		return fmt.Errorf("%s is a file — register the folder that contains it", abs)
	}

	if *initManifest {
		written, err := writeStarterManifest(kind, abs, *version, *workflowType)
		if err != nil {
			return err
		}
		if written != "" {
			fmt.Fprintf(stdout, "wrote %s\n", written)
		}
	}

	api := newAPI(*apiURL)
	var got registerResult
	if err := api.postJSON("/api/sources/"+kind, map[string]any{"path": abs}, &got); err != nil {
		return err
	}

	if *jsonOut {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(got)
	}

	label := got.Name
	if got.Version != "" {
		label += "@" + got.Version
	}
	fmt.Fprintf(stdout, "registered %s %s\n", kind, label)
	fmt.Fprintf(stdout, "  path    %s\n", got.Path)
	if got.Digest != "" {
		fmt.Fprintf(stdout, "  digest  %s\n", got.Digest)
	}
	switch {
	case got.EndpointError != "":
		// THE REGISTRATION STOOD. Saying so in the same breath as the failure is the difference
		// between "run this again" and "your folder is not registered" — and only the first is true.
		fmt.Fprintf(stdout, "  nexus   NOT created: %s\n", got.EndpointError)
		fmt.Fprintf(stdout, "          the folder IS registered; re-run this command to create it\n")
	case got.Endpoint != "":
		fmt.Fprintf(stdout, "  nexus   %s (%s)\n", got.Endpoint, got.EndpointState)
	case kind == "workflow":
		// Said rather than silently absent: an operator who knows Actors get an endpoint will look
		// for the workflow's, and "there isn't one" is the answer, not an omission.
		fmt.Fprintf(stdout, "  nexus   none — a workflow dispatches, nothing dispatches to it\n")
	}
	fmt.Fprintf(stdout, "\nit is NOT running. Start it with:\n")
	if kind == "actor" {
		fmt.Fprintf(stdout, "  kontra serve --actor %s\n", got.Path)
	} else {
		fmt.Fprintf(stdout, "  kontra workflow serve %s\n", got.Name)
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
