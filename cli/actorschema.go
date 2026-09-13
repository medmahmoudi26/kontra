package main

// actorschema.go — `kontra actor schema <dir>`: what a Method accepts and emits, from the code on
// disk, with no orchestrator and no deploy.
//
// WHY IT IS NOT A NEW DERIVATION. kontra already turns `takes=Target, emits=Page` into JSON Schema
// through `kontra.schema.schema_of`, and a booting worker publishes exactly that to the catalog. The
// only thing missing was the ability to ask BEFORE registering — which is precisely when a form
// beside an editor needs the answer. So this shells to `internals.schemadump`, which calls the same
// `load_actor` + `operations_of` the worker calls, and derives nothing itself.
//
// A SECOND DERIVATION WOULD BE A SECOND ANSWER to "what does this Method accept", and the two would
// drift the first time one was fixed. `TestActorSchemaMatchesTheCatalogDerivation` holds them to one
// implementation.
//
// IT TOUCHES NOTHING BUT THE FILESYSTEM AND AN INTERPRETER. No API call, no Temporal, no catalog
// read — an actor being edited is usually an actor nothing is serving, and a schema command that
// needed a running control plane would be useless exactly when it is wanted.

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/medmahmoudi26/kontra/cli/internal/cliio"
	"github.com/medmahmoudi26/kontra/cli/internal/cliutil"
)

const actorSchemaUsage = "usage: kontra actor schema <dir> [--method NAME] [--python <bin>]"

func cmdActorSchema(args []string) error {
	// THE DIRECTORY COMES OFF THE FRONT BEFORE PARSING, which is `dataset tag`'s shape and is not
	// a style choice: Go's `flag` stops at the first non-flag argument, so parsing first would
	// leave `--method echo` sitting in `fs.Args()` as two positionals and reject the command with
	// a usage line that itself shows the flag AFTER the directory.
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return errors.New(actorSchemaUsage)
	}
	dirArg, rest := args[0], args[1:]

	fs := flag.NewFlagSet("actor schema", flag.ContinueOnError)
	fs.SetOutput(cliio.Stdout)
	method := fs.String("method", "", "narrow to one Method by name")
	python := fs.String("python", "", "python interpreter (default: .venv/bin/python, else python3)")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q\n%s", fs.Arg(0), actorSchemaUsage)
	}
	actorDir, err := filepath.Abs(dirArg)
	if err != nil {
		return err
	}
	if _, err := os.Stat(actorDir); err != nil {
		return fmt.Errorf("no actor at %s: %w", actorDir, err)
	}

	root, err := cliutil.FindRepoRoot("")
	if err != nil {
		return fmt.Errorf("finding the kontra checkout: %w\n"+
			"  this command reads the SDK's own schema derivation, so it needs the repository", err)
	}

	// The same interpreter rule `kontra serve` uses, through the same function — an actor's schema
	// must be derived by the interpreter that would RUN it, or a type that imports fine in one venv
	// and not the other produces a schema for code that cannot start.
	py := pythonFor(root, *python)

	argv := []string{"-m", "internals.schemadump", actorDir}
	if *method != "" {
		argv = append(argv, "--method", *method)
	}
	cmd := exec.Command(py, argv...)
	cmd.Dir = filepath.Join(root, "runtime", "python")
	// cliutil.Derive, never append: two appends onto one slice with spare capacity write the same
	// index, and this repo has already lost a PYTHONPATH that way. Same three entries as
	// `serve.go`, deliberately — an author's `from kontra import actor` must resolve to the
	// checkout here for the same reason it must there.
	cmd.Env = cliutil.Derive(os.Environ(),
		"PYTHONPATH="+filepath.Join(root, "sdk", "python")+":"+
			filepath.Join(root, "runtime", "python")+":"+
			filepath.Join(root, "sdk", "python", "_gen"))
	cmd.Stdout = cliio.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			// The Python side already printed the real failure — an import error, a missing
			// dependency, an unknown Method — with the traceback. Wrapping it in a Go error here
			// would bury the useful half under this command's summary of it.
			return fmt.Errorf("could not read the actor's schema (see above)")
		}
		return fmt.Errorf("running %s: %w\n"+
			"  --python, or KONTRA_PYTHON, selects the interpreter", strings.Join(append([]string{py}, argv...), " "), err)
	}
	return nil
}
