// Package cliutil holds the few helpers that BOTH halves of the CLI need — the control plane's
// commands and the Warden's — and that belong to neither.
//
// IT IS DELIBERATELY SMALL AND IT IS NOT A HOME FOR ANYTHING ELSE. Each function here earned its
// place by being reached from two packages that must not import each other; a helper used in one
// place belongs beside its caller. `FirstLine` arrived from `driver_podman.go`, which is to say
// the trust policy was reaching into a container driver for a string function.
package cliutil

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// FirstLine is `podman run -d`'s container id without the newline it comes with.
func FirstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func EnvOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// FileExists reports whether path is an existing regular file. False for a directory, which is
// the distinction `kontra workflow` leans on when a folder and a file are both plausible.
func FileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}

// KontraRoot is this installation's `.kontra/`: KONTRA_HOME, else an installation in the checkout
// you are standing in, else `~/.kontra`.
//
// THREE ANSWERS IN ORDER, because the two-answer versions each broke a real case and the order is
// what reconciles them.
//
// It used to be the checkout alone (findRepoRoot). That made ONE variable with TWO answers across
// languages: `control/orchestrator/src/sources.ts:kontraHome` resolves `~/.kontra` when KONTRA_HOME is
// unset — what the register form prefills and what a registration RECORDS — while this side
// answered `<checkout>/.kontra`. Worse, the orchestrator spawns this CLI with `cwd` set to the
// REGISTERED FOLDER (`actorControl.ts`), so for a folder under `~/.kontra/actors` the walk found
// no docker-compose.yml anywhere above it, KontraRoot returned an error, loadConfig quietly
// answered with the zero value, and the worker started with no state token, no explore token and
// no fleet credentials — each failing later without naming this.
//
// Then it was `~/.kontra` alone, and that broke the everyday case in the other direction: `kontra`
// typed inside a checkout stopped reading the `config.yaml` that `kontra init` had written there.
// On a machine where `~/.kontra` belongs to something else entirely — it is not a namespace this
// project owns, and an unrelated tool's credentials were already sitting in it — that is not even
// an empty directory, it is a stranger's.
//
// Ordered, both cases land right. A registered folder outside any checkout finds no repo above it
// and falls through to the home, which is the property the spawned child needs. A shell inside the
// checkout finds the installation that is actually there. `config.yaml` is the marker rather than
// the bare directory: `.kontra/` gets created by things other than `init`, and an empty one is not
// an installation to prefer over the home.
func KontraRoot() (string, error) {
	if v := os.Getenv("KONTRA_HOME"); v != "" {
		return filepath.Clean(v), nil
	}
	if checkout, err := FindRepoRoot(""); err == nil {
		if cand := filepath.Join(checkout, KontraDir); FileExists(ConfigPath(cand)) {
			return cand, nil
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("no home directory to hold %s (set KONTRA_HOME): %w", KontraDir, err)
	}
	return filepath.Join(home, KontraDir), nil
}

// Derive returns base + extra as a FRESH slice, so two derivations of one environment cannot
// overwrite each other through a shared backing array.
func Derive(base []string, extra ...string) []string {
	out := make([]string, 0, len(base)+len(extra))
	out = append(out, base...)
	return append(out, extra...)
}

// --- what KontraRoot needs, which came with it ---

// KontraDir is the directory's name, wherever cliutil.KontraRoot puts it.
const KontraDir = ".kontra"

func ConfigPath(root string) string { return filepath.Join(root, "config.yaml") }

// findRepoRoot walks up from CWD until it sees docker-compose.yml (the control-plane
// contract); --repo overrides. Also used by deploy to locate the base-image context.
func FindRepoRoot(override string) (string, error) {
	if override != "" {
		if _, err := os.Stat(filepath.Join(override, "docker-compose.yml")); err != nil {
			return "", fmt.Errorf("--repo %s: no docker-compose.yml there", override)
		}
		return override, nil
	}
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "docker-compose.yml")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("no docker-compose.yml found walking up from CWD (pass --repo <dir>)")
		}
		dir = parent
	}
}

// SafeWorkerName makes a Worker name usable as a container name and as a filename.
//
// IT INHERITS THE TMUX SCHEME AND OUTLIVED IT. `.` and `:` became `_` because tmux's
// `session_check_name()` rewrote them silently at creation, so a name that addressed a session
// addresses the same Worker now and an operator's muscle memory survives every mechanism change
// underneath it. Both substitutions are still load-bearing on their own terms: a `:` is not a legal
// byte in a container name, and `shared/conformance/queues.json` §tmux_session pins the folding
// across three languages, so changing it renames every Worker at once.
//
// THE SEPARATORS ARE THIS FUNCTION'S OWN ADDITION, because these names reach a filesystem: `a/b`
// was a legal tmux session name and here it would be a path, writing into a directory that does not
// exist or — worse — one that does.
//
// `shared/core/src/panels/tmux.ts:tmuxSafeName` is the TypeScript peer for the first two.
func SafeWorkerName(name string) string {
	return strings.NewReplacer(".", "_", ":", "_", "/", "_", `\`, "_").Replace(name)
}

// foldWorkerName is the Worker-name fold that `shared/conformance/queues.json` §tmux_session pins,
// and it is NARROWER than {@link SafeWorkerName} on purpose.
//
// TWO RULES, AND CONFLATING THEM WOULD BREAK THE CORPUS. This one folds `.` and `:` and nothing
// else, because that is exactly what tmux's `session_check_name()` did and what three languages now
// agree on. `SafeWorkerName` additionally folds separators, because the names IT produces reach a
// filesystem or a container name, where a `/` is a path rather than a character. A single function
// doing both would either start folding separators the corpus says survive, or stop folding ones a
// filename cannot hold.
func foldWorkerName(name string) string {
	return strings.NewReplacer(".", "_", ":", "_").Replace(name)
}

// ActorWorkerName is what an Actor's Worker is called: `<actor>-<version>`, folded.
//
// THE FALLBACK IS THE CONTRACT'S, not this function's convenience. A name that folds to "" or "_"
// identifies nothing, and anything looked up by it matches whatever else in the inventory happens to
// have no name — so it is `actor`, which is what `shared/core/src/panels/tmux.ts:actorSession`
// answers on the other side of the language boundary. This function used to return the unusable
// string; `shared/conformance/queues.json` §tmux_session is what found it and what holds the two
// together now. The `fleet` fallback in `fleetSessionName` is a DIFFERENT domain and the corpus
// says why.
//
// IT WAS `tmux.Session` AND THE SCHEME OUTLIVED TMUX. The folding began as a workaround for a
// silent rewrite at session creation; it is now simply the naming rule, and a `:` is not a legal
// byte in a container name either. Changing it renames every Worker at once.
func ActorWorkerName(actor, version string) string {
	name := foldWorkerName(actor)
	if version != "" {
		name = foldWorkerName(actor + "-" + version)
	}
	if name == "" || name == "_" {
		return "actor"
	}
	return name
}
