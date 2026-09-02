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
