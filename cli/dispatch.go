// dispatch.go — `kontra actor`: the verb table for one Actor, and the catalog lookup a name
// goes through on the way to it.
//
// THE DISPATCH VERB IS GONE, and with it most of what this file used to be. `kontra actor <ref>
// dispatch` and `kontra graph <id> dispatch` built a K-node graph and POSTed it to `/api/runs`,
// the route the server-side interpreter answered, then polled `/status` and read `/output`.
// ADR 0023 §12 made a Run one execution of a CALLER's workflow and took the interpreter with it,
// so against a running orchestrator all four ends are dead:
//
//	POST /api/runs {"graph":…}       400 {"error":"file is required"}
//	POST /api/graphs/:id/dispatch    404
//	GET  /api/runs/:id/status        404
//	GET  /api/runs/:id/output        404
//
// DISPATCHING IS THE CALLER'S NOW. Work starts by serving a workflow you wrote and starting it;
// see `kontra workflow serve|start` and examples/python/workflows/ for two that dispatch a
// Method over a Batch.
package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// resolveActor maps "name" / "name@version" to a registered (name, version) via the
// catalog; a bare name must resolve to exactly one version (else list the choices).
func resolveActor(api *apiClient, ref string) (string, string, error) {
	name, version, _ := strings.Cut(ref, "@")
	if name == "" {
		return "", "", errors.New("empty actor name")
	}
	var actors []actorRecord
	if err := api.getJSON("/api/actors", &actors); err != nil {
		return "", "", fmt.Errorf("GET /api/actors failed: %w", err)
	}
	var versions []string
	for _, a := range actors {
		if a.Name != name {
			continue
		}
		if version != "" && a.Version == version {
			return name, version, nil
		}
		versions = append(versions, a.Version)
	}
	if version != "" {
		return "", "", fmt.Errorf("actor %s is not registered (`kontra workers list` to see what is)", ref)
	}
	switch len(versions) {
	case 0:
		return "", "", fmt.Errorf("actor %q is not registered (`kontra workers list`)", name)
	case 1:
		return name, versions[0], nil
	}
	return "", "", fmt.Errorf("actor %q has multiple versions (%s); specify one as %s@<version>",
		name, strings.Join(versions, ", "), name)
}

// --- kontra actor ---

func cmdActor(args []string) error {
	// REGISTER IS CHECKED BEFORE THE REF, because the grammar here is `kontra actor <ref> <verb>`
	// and `register` would otherwise be read as the name of an Actor. Nothing is lost: an Actor
	// literally called `register` would collide, and `kontra actor register dispatch …` is not a
	// command anybody means.
	if len(args) >= 1 && args[0] == "register" {
		return cmdRegister("actor", args[1:])
	}
	// A REDIRECT, not a 404, for the one word that used to be here. `dispatch` was this CLI's
	// way in for long enough to be muscle memory, and an operator who types it is asking a
	// question ("how do I run this actor?") that has an answer — it just is not a subcommand any
	// more. Reporting `unknown command` would send them looking for a broken build.
	if len(args) >= 2 && args[1] == "dispatch" {
		return fmt.Errorf(
			"`kontra actor %s dispatch` is gone: the orchestrator stopped starting runs with "+
				"ADR 0023 §12, and this built a graph for a route that now answers "+
				"`file is required`.\n"+
				"Dispatching is the CALLER's — drive the actor from a workflow you own:\n"+
				"  kontra workflow serve <your_workflow_folder>\n"+
				"  kontra workflow start <your_workflow_folder> --wait --input '…'\n"+
				"See examples/python/workflows/ for two that dispatch a Method over a Batch.", args[0])
	}
	return errors.New("usage: kontra actor register <dir> [--init] [--json]")
}

// fileExists reports whether path is an existing regular file. False for a directory, which is
// the distinction `kontra workflow` leans on when a folder and a file are both plausible.
func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}
