// program.go — getting the Pulumi program onto disk somewhere the engine may write, and the lock
// that keeps two converges out of one backend.
package hostengine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

// ProgramFile is the program, and its name is fixed by Pulumi itself.
const ProgramFile = "Pulumi.yaml"

// Layout is where the engine keeps everything it owns, under one installation's `.kontra/`.
//
// ── WHY THE PROGRAM IS COPIED AND NOT RUN WHERE IT LIVES ────────────────────────────────────────
//
// `pulumi stack select --create <s>` WRITES `Pulumi.<s>.yaml` beside `Pulumi.yaml` (measured on
// 3.244.0), which makes the program directory a directory the engine must be able to WRITE. Note what
// the reason is not: `control/pulumi/.gitignore:7` is `Pulumi.*.yaml`, so that file would not show up
// in anybody's `git status` — checked, and there is one sitting in the checkout right now from an
// earlier hand-run. The reasons that hold are the other two.
//
// It is the wrong directory for the case this command is FOR. `control/pulumi/README.md:287-291`:
// "`<program-dir>` is a checkout here and is not one after `curl … | sh`." An installed machine has no
// `control/pulumi` — and where it does have a copy, that copy may sit under a root-owned prefix a
// `pulumi` running as the operator cannot write a stack file into. The engine has to own a directory
// either way, and then there is exactly one layout instead of two.
//
// And a checkout is SHARED. `Pulumi.local.yaml` carries an encryptionsalt bound to one passphrase; two
// installations (a developer's `~/.kontra` and a `KONTRA_HOME=/srv/kontra` service) pointed at one
// checkout would take turns making each other's stack undecryptable, with no error until the first
// read of a secret.
//
// A `go:embed` of the program is what that README calls the obvious shape, and it is NOT available
// here: `cli/` is its own Go module and `control/pulumi/Pulumi.yaml` is outside it, so `go:embed`
// cannot reach it without a second copy of the file in the module — which is a drift nobody would
// see, because both copies work on their own. Shipping the program with the binary is the
// installer's slice (issue 09/16); until then the source is the checkout, and Resolve says so when
// there is none.
type Layout struct {
	// Root is this installation's `.kontra/` — cliutil.KontraRoot()'s answer.
	Root string
	// Dir is the engine's own directory: the materialised program, the assets, the passphrase, the lock.
	Dir string
	// Program is the directory `pulumi -C` is pointed at.
	Program string
	// Assets is where the files `uploads` reads with `fn::readFile` land.
	Assets string
}

// NewLayout derives the paths. `engine/` rather than `pulumi/` because what lives there is this
// engine's whole working state and not only a program, and because `~/.kontra/pulumi` reads like the
// infra engine's `/data/pulumi` work directory (workspace.ts:56), which it is not.
func NewLayout(root string) Layout {
	dir := filepath.Join(root, "engine")
	return Layout{
		Root:    root,
		Dir:     dir,
		Program: filepath.Join(dir, "pulumi"),
		Assets:  filepath.Join(dir, "images"),
	}
}

// assetRe finds every file the program reads at load time.
//
// THE ASSET LIST IS READ OUT OF THE PROGRAM RATHER THAN TYPED HERE, and the count is exactly why.
// There is ONE `fn::readFile: ${assetsDir}/…` line today — `Pulumi.yaml:681`, `postgres-init.sh` —
// and there were three until `logship.sh` and `logline.py` were baked into `logshipImage`. A Go list
// would have kept copying two files that are no longer read, and would miss the next one added: that
// failure lands at converge time as `Error reading file at path …` naming a path under `~/.kontra`
// the operator never wrote, which reads as a corrupted install rather than a missing copy step.
var assetRe = regexp.MustCompile(`fn::readFile:\s*\$\{assetsDir\}/([A-Za-z0-9._-]+)`)

// Assets lists the asset file names the program loads, in source order, deduplicated.
func Assets(program []byte) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range assetRe.FindAllSubmatch(program, -1) {
		name := string(m[1])
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

// programName reads the `name:` the program declares for itself.
//
// This is the dispatch table meeting a real file. `Pulumi.yaml:98` is `name: kontra-control` and
// `README.md:52-56` calls the fixed location plus that first field a security property: a program on
// disk with its own name in it "cannot be pointed at another project by a caller". So `--program`
// pointing somewhere else is checked against {@link Project} rather than trusted, and the check is a
// read of the file rather than of the flag.
var nameRe = regexp.MustCompile(`(?m)^name:\s*([A-Za-z0-9._-]+)`)

func programName(program []byte) string {
	if m := nameRe.FindSubmatch(program); m != nil {
		return string(m[1])
	}
	return ""
}

// Source is where the program was found, for the line the command prints.
type Source struct {
	Dir  string
	How  string // "--program", "$KONTRA_PULUMI_PROGRAM", "the checkout", "already installed"
	YAML []byte
}

// Resolve finds the Pulumi program to converge, in four places, in this order.
//
//  1. `--program <dir>`, because an operator working on the program itself must be able to say so.
//  2. `$KONTRA_PULUMI_PROGRAM`, the same override for CI and for the installer's own tests.
//  3. `<checkout>/control/pulumi`, which is where it lives in this repository.
//  4. An already-materialised `<engine>/pulumi/Pulumi.yaml`, which is what an installed machine has
//     and a checkout does not.
//
// FOUR, AND THE REFUSAL NAMES ALL FOUR. A program that cannot be found is the one failure here that
// is nobody's mistake — it is what a released binary looks like before issue 09 ships the program
// beside it — so the message has to be a list of where to put it, not "not found".
//
// `checkout` is the caller's `cliutil.FindRepoRoot("")` answer, or "" when there is no checkout. It
// is a PARAMETER rather than a read of the environment because FindRepoRoot walks up from the working
// directory and lives in cliutil with 26 lines about why (cliutil.go:41-66); a second, quieter answer
// to "which checkout am I in" derived here is how that ordering gets contradicted.
func Resolve(lay Layout, flagDir, checkout string, env Env) (Source, error) {
	type cand struct{ dir, how string }
	var cands []cand
	if flagDir != "" {
		cands = append(cands, cand{flagDir, "--program"})
	}
	if v := env("KONTRA_PULUMI_PROGRAM"); v != "" {
		cands = append(cands, cand{v, "$KONTRA_PULUMI_PROGRAM"})
	}
	if checkout != "" {
		cands = append(cands, cand{filepath.Join(checkout, "control", "pulumi"), "the checkout"})
	}
	cands = append(cands, cand{lay.Program, "already installed"})

	var tried []string
	for _, c := range cands {
		path := filepath.Join(c.dir, ProgramFile)
		b, err := os.ReadFile(path)
		if err != nil {
			tried = append(tried, fmt.Sprintf("    %-26s %s", c.how, path))
			// An explicitly-named program that is not there is a typo, not a fallthrough: falling
			// through would converge the installed one and never mention that `--program` was ignored.
			if c.how == "--program" || c.how == "$KONTRA_PULUMI_PROGRAM" {
				return Source{}, fmt.Errorf("%s %s has no %s in it", c.how, c.dir, ProgramFile)
			}
			continue
		}
		if got := programName(b); got != Project {
			return Source{}, fmt.Errorf("%s is project %q, and the host engine converges %s "+
				"(found via %s).\n  ADR 0052 §1: the project comes from the program's own `name:` and never "+
				"from a caller, so a\n  program declaring something else is refused rather than run under this "+
				"engine's backend.", path, got, Project, c.how)
		}
		return Source{Dir: c.dir, How: c.how, YAML: b}, nil
	}
	return Source{}, fmt.Errorf("no %s for project %s anywhere. Looked in:\n%s\n"+
		"  Point at one with --program <dir>, or run this from a kontra checkout.",
		ProgramFile, Project, strings.Join(tried, "\n"))
}

// Materialise writes the program and its assets into the engine's directory, and returns the files
// it had to write.
//
// THE COPY IS DERIVED AND IS REWRITTEN WHENEVER IT DIFFERS, so the committed program always wins and
// a stale copy cannot outlive an edit to it. The one file in that directory this never touches is
// `Pulumi.<stack>.yaml`: it is Pulumi's, it carries the encryptionsalt, and rewriting it is how a
// stack's secrets become unreadable.
func Materialise(lay Layout, src Source) ([]string, error) {
	if err := os.MkdirAll(lay.Program, 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(lay.Assets, 0o700); err != nil {
		return nil, err
	}
	var wrote []string
	w, err := writeIfDifferent(filepath.Join(lay.Program, ProgramFile), src.YAML, 0o600)
	if err != nil {
		return nil, err
	}
	if w {
		wrote = append(wrote, ProgramFile)
	}
	for _, name := range Assets(src.YAML) {
		// `../images` relative to the program, which is where the committed program's own default
		// puts them (`Pulumi.yaml:290`, `assetsDir: ../images`).
		from := filepath.Join(src.Dir, "..", "images", name)
		b, err := os.ReadFile(from)
		if err != nil {
			if src.How == "already installed" {
				// Nothing to copy FROM — the installed layout is the source. The program reads these
				// with fn::readFile, so a genuinely missing one fails the converge naming the file
				// (measured, README.md:362) and that message is better than anything here.
				continue
			}
			return nil, fmt.Errorf("the program reads %s and it is not beside it: %w", name, err)
		}
		// 0755 for the same reason `uploads` sets `permissions: "0755"` and not `executable: true`:
		// postgres's entrypoint tests `-x` as the `postgres` user and SOURCES a script it cannot
		// execute, which leaks the script's `set -eu` into an entrypoint that deliberately runs
		// without `-u` (README.md, "the upload's mode").
		w, err := writeIfDifferent(filepath.Join(lay.Assets, name), b, 0o755)
		if err != nil {
			return nil, err
		}
		if w {
			wrote = append(wrote, name)
		}
	}
	sort.Strings(wrote)
	return wrote, nil
}

func writeIfDifferent(path string, want []byte, mode os.FileMode) (bool, error) {
	if got, err := os.ReadFile(path); err == nil && string(got) == string(want) {
		return false, nil
	}
	return true, os.WriteFile(path, want, mode)
}

// --- the mutex ----------------------------------------------------------------------------------

// LockFileName is the host engine's mutex, and it is a file because there is nothing else here.
//
// The infra engine's mutex is a Temporal Entity Workflow keyed by the stack's fqn (ADR 0019); the
// host has no Temporal at the moment it needs one — it is the thing being installed — so ADR 0052 §1
// specifies a lock file with the CLI as the only writer. `control/pulumi/README.md:277-280` states
// the failure: "Two concurrent `kontra up`s against one `file://` backend is a corrupted checkpoint."
const LockFileName = "up.lock"

// Lock takes the converge lock and returns the release.
//
// A PREVIEW TAKES NO LOCK, and that is a decision rather than an omission: `pulumi preview` writes
// nothing to the backend, so two previews cannot corrupt anything, and a preview that queued behind
// a ten-minute converge would answer a question about a topology that had already changed. ADR 0052
// §2's `--preview` is documented as taking no lock for this reason (issue 04).
//
// A STALE LOCK IS CLEARED, NOT WORSHIPPED. `kontra up` blocks for minutes and gets Ctrl-C'd, and a
// SIGKILL leaves the file behind; requiring a manual `rm` of a file the operator has never heard of
// is a worse failure than the one the lock prevents. So the holder's pid is recorded and a lock whose
// holder is gone is taken over, naming what happened. When the holder IS alive the refusal names the
// pid and the file, because the honest answer then is "something else is already doing this".
func Lock(dir string) (func(), error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, LockFileName)
	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			fmt.Fprintf(f, "%d\n", os.Getpid())
			f.Close()
			return func() { os.Remove(path) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		holder := lockHolder(path)
		if holder > 0 && processAlive(holder) {
			return nil, fmt.Errorf("another converge is already running (pid %d).\n"+
				"  Two concurrent converges against one file:// backend corrupt the checkpoint, so this one\n"+
				"  refuses rather than queues. If that pid is gone, delete %s.", holder, path)
		}
		// Gone, or a lock file with nothing readable in it. Take it over.
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("clearing the stale lock %s: %w", path, err)
		}
	}
	return nil, fmt.Errorf("could not take the converge lock %s", path)
}

func lockHolder(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0
	}
	return pid
}

// processAlive asks whether a pid is still there. Signal 0 is the portable "does this exist" probe;
// on a platform where Signal cannot ask, an error is read as "gone", which errs toward clearing a
// lock rather than toward a permanent one nobody can explain.
func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}
