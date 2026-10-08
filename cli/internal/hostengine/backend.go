// backend.go — where the host engine's state goes, and the refusal that keeps it from going
// anywhere else.
package hostengine

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// StateDirName is the directory under this installation's `.kontra/` that holds the `file://`
// backend. `control/pulumi/README.md:243` spells it
// `$KONTRA_HOME/state`, so the name is theirs and this constant only stops a third spelling.
const StateDirName = "state"

// Env is the process environment as a lookup, so AssertBackend can be handed an operator's shell
// without a test having to mutate this one's. `os.Getenv` is the production value.
type Env func(string) string

// Backend is the backend URL for an installation rooted at `root` (cliutil.KontraRoot()'s answer).
//
// It is a FUNCTION OF THE INSTALLATION rather than a constant, because `~/.kontra` is not where this
// CLI necessarily keeps its state: `cliutil.KontraRoot()` answers `$KONTRA_HOME`, else the checkout
// you are standing in when it holds a `config.yaml`, else `$HOME/.kontra` — three answers, in that
// order, each of which broke a real case (cliutil.go:41-66).
//
// THAT IS A DISAGREEMENT WORTH NAMING: the one-line installer hardcodes `KONTRA_HOME="${KONTRA_HOME:-$HOME/.kontra}"`
// and `control/pulumi/README.md:243` writes `file://~/.kontra/state`, and neither goes through
// KontraRoot. Inside a checkout that holds its own `config.yaml` those name a DIFFERENT directory
// than this function does, and the two halves of one install would then log in to two backends. The
// Go side deriving the path from KontraRoot is the half that can be right; the shell side's default
// is right whenever there is no checkout, which is every installed machine.
func Backend(root string) string {
	return "file://" + filepath.Join(root, StateDirName)
}

// selfManaged is the file://|s3:// pair, and it is the pair `workspace.ts:93` accepts.
//
// THE SAME POLICY IN BOTH ENGINES, DELIBERATELY. the one-line installer states the rule for the shell half:
// "file:// and s3:// are the accepted pair because those are the two `workspace.ts:93` accepts, and a
// second, different policy for the same question is how two engines start disagreeing."
func selfManaged(url string) bool {
	return strings.HasPrefix(url, "file://") || strings.HasPrefix(url, "s3://")
}

func hosted(url string) bool {
	return strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://")
}

// AssertBackend refuses to run unless this machine's Pulumi configuration cannot reach Pulumi Cloud,
// and it returns the backend URL the engine should actually use.
//
// ── THIS IS THE SHARPEST TRAP IN THE TOOL, AND IT IS WHY THE FUNCTION EXISTS TWICE ──────────────
//
// `control/orchestrator/src/infra/workspace.ts:16-20`, about the container's engine: *"A failed
// `pulumi login` does not stop the CLI — it silently creates an ephemeral Pulumi Cloud account and
// deploys THERE, state included. For a design whose whole premise is no hosted dependency, that is
// the sharpest trap in the tool, so assertBackend fails closed before any operation runs."*
//
// ADR 0052 fact 4 asks for a second one, on the host, LOUDER — and the reason it must be louder is
// the difference between the two machines. In a container the ambient environment is ours. On a
// developer's laptop an ambient `PULUMI_ACCESS_TOKEN` is ordinary (any other Pulumi project puts one
// there), which turns the trap from the unlucky case into the likely one. And the trap needs no token
// at all to spring: re-measured for the one-line installer on host CLI 3.244.0, non-interactive, empty
// PULUMI_HOME, NO credential of any kind —
//
//	warning: failed to get user account details: this command requires logging in
//	PULUMI_EPHEMERAL_AGENT_ACCOUNT
//	CLAIM_URL=https://app.pulumi.com/claim/01a0dee8-…
//
// An account was created, server-side, out of nothing.
//
// ── WHAT THIS CHECKS, AND IN THIS ORDER, BECAUSE THE ORDER IS MEASURED ──────────────────────────
//
// The ladder is the one-line installer's, verbatim in shape, so the installer and the CLI cannot disagree
// about what a dangerous environment is. Each rung was measured on 3.244.0 with `pulumi whoami
// --verbose` (the one-line installer):
//
//  1. `PULUMI_BACKEND_URL` WINS OVER EVERYTHING, including a later `pulumi login` and a set
//     `PULUMI_ACCESS_TOKEN`. So an exported hosted URL is the one setting that cannot be undone by
//     anything this command does, and it is refused. An exported SELF-MANAGED url is honoured and
//     returned: silently ignoring a backend somebody stated explicitly is its own trap.
//  2. `PULUMI_ACCESS_TOKEN` with no URL set goes straight to the cloud. This engine always states
//     the backend itself (see EngineEnv), so the token is already beaten — but it is refused anyway,
//     because the operator's shell is then configured so that every invocation this repository
//     DOCUMENTS by hand (`control/pulumi/README.md:293-299`) goes to a hosted account, and they have
//     to learn that once rather than discover it from a bill.
//  3. `credentials.json`'s `current` is what a machine logged in to Pulumi Cloud reads back. Only a
//     HOSTED value is refused; a DIY backend belongs to whatever else the operator does with Pulumi.
//
// WHAT IT DELIBERATELY DOES NOT DO IS `pulumi login`. That writes the operator's global
// `~/.pulumi/credentials.json` — the one-line installer declines for the same reason ("rewriting an
// operator's current backend on the way past would be an installer deciding something it was not
// asked about"). Stating `PULUMI_BACKEND_URL` in the child's environment is measurably stronger
// (rung 1) and mutates nothing.
func AssertBackend(root string, env Env) (string, error) {
	backend := Backend(root)
	if amb := env("PULUMI_BACKEND_URL"); amb != "" {
		if !selfManaged(amb) {
			return "", fmt.Errorf("refusing to converge: PULUMI_BACKEND_URL is %q, which is not a "+
				"self-managed backend.\n"+
				"  An exported backend URL beats every other setting, including a later `pulumi login`, so\n"+
				"  this would converge kontra's whole control plane there — 13 containers and their state.\n"+
				"  A failed login does not stop `pulumi`: it creates an ephemeral Pulumi Cloud account and\n"+
				"  deploys THERE (workspace.ts:16, measured again in the one-line installer).\n"+
				"      unset PULUMI_BACKEND_URL\n"+
				"      # or point it at this installation's own state:\n"+
				"      export PULUMI_BACKEND_URL=%s", amb, backend)
		}
		return amb, nil
	}
	if env("PULUMI_ACCESS_TOKEN") != "" {
		return "", fmt.Errorf("refusing to converge: PULUMI_ACCESS_TOKEN is set in this environment "+
			"and PULUMI_BACKEND_URL is not.\n"+
			"  That is the configuration `pulumi` resolves to Pulumi Cloud, and it is what turns a failed\n"+
			"  `pulumi login %s` from an error into a converge into somebody's hosted\n"+
			"  account — state included (workspace.ts:16, ADR 0052 fact 4).\n"+
			"      env -u PULUMI_ACCESS_TOKEN kontra control up\n"+
			"      # or state the backend, which wins over the token (measured on 3.244.0):\n"+
			"      export PULUMI_BACKEND_URL=%s", backend, backend)
	}
	if cur := currentBackend(env); hosted(cur) {
		return "", fmt.Errorf("refusing to converge: this machine is logged in to a hosted Pulumi "+
			"backend.\n"+
			"      %s  current = %s\n"+
			"  This command states its own backend, so ITS state would stay local — but anything run by\n"+
			"  hand out of control/pulumi/ (README.md:293-299) would go to that account instead, and per\n"+
			"  workspace.ts:16 a login that fails does not stop the CLI. Point Pulumi at kontra's state,\n"+
			"  or say so explicitly for this shell:\n"+
			"      pulumi login %s\n"+
			"      # or:\n"+
			"      export PULUMI_BACKEND_URL=%s",
			credentialsPath(env), cur, backend, backend)
	}
	return backend, nil
}

// EnsureBackendDir creates a `file://` backend's directory, because Pulumi will not.
//
// MEASURED, on this box, with `kontra control up --preview` against a fresh KONTRA_HOME:
//
//	error: unable to open bucket file:///…/state?no_tmp_dir=true:
//	stat /…/state: no such file or directory
//
// `workspace.ts:127-130` had already recorded the same failure for the container's engine — "the DIY
// backend does not create its own bucket either: on a fresh volume `pulumi stack select` fails with
// 'unable to open bucket', which says nothing about the directory simply not being there yet" — and
// the host half reproduced it exactly. So this is the first-run step, not an optimisation: without it
// the very first `kontra up` on a new machine fails with a sentence about a bucket.
//
// 0700, LIKE THE REST OF THIS ENGINE'S DIRECTORY. The state holds the encrypted form of every secret
// the stack carries, and `config.go:733`'s rule for the file that holds credentials applies to the
// directory that holds their ciphertext: nobody else on the box has business reading it.
//
// A NON-`file://` BACKEND IS LEFT ALONE. An operator who exported `s3://…` has a bucket that exists
// or does not, and creating a local directory shaped like a URL would be the wrong answer to both.
func EnsureBackendDir(backend string) error {
	path, ok := strings.CutPrefix(backend, "file://")
	if !ok {
		return nil
	}
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i] // pulumi appends query parameters of its own (?no_tmp_dir=true)
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("creating the Pulumi state directory %s: %w", path, err)
	}
	return nil
}

func credentialsPath(env Env) string {
	home := env("PULUMI_HOME")
	if home == "" {
		home = filepath.Join(env("HOME"), ".pulumi")
	}
	return filepath.Join(home, "credentials.json")
}

// currentBackend reads the one field of `credentials.json` that says where this machine is logged in.
//
// ONE FIELD, AND A MISSING OR UNPARSEABLE FILE IS NOT A REFUSAL. A box that has never run `pulumi`
// has no file, which is the safest state there is; a file this cannot read is not evidence of a
// hosted login, and treating it as one would make `kontra up` fail on a corrupt unrelated dotfile.
// the one-line installer reads the same field with `sed`; Go has a JSON parser, so it uses it.
func currentBackend(env Env) string {
	b, err := os.ReadFile(credentialsPath(env))
	if err != nil {
		return ""
	}
	var doc struct {
		Current string `json:"current"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return ""
	}
	return doc.Current
}

// PassphraseFileName is where this engine keeps `PULUMI_CONFIG_PASSPHRASE`, relative to the engine's
// working directory.
//
// IT IS NOT UNDER `state/`, AND THAT IS THE WHOLE REASON FOR A SEPARATE PATH. The passphrase
// DECRYPTS the state directory, so a backup, a snapshot or an `scp` of `state/` that carried the key
// beside the ciphertext would be a copy of both halves. `control/orchestrator/src/secrets/keyring.ts`
// is explicit that this is the threat model it defends — the store FILE, not root on the box.
const PassphraseFileName = "passphrase"

// Passphrase returns this installation's `PULUMI_CONFIG_PASSPHRASE`, minting one on first use.
//
// ── THE OS KEYCHAIN IS WHAT ADR 0052 §4 ASKS FOR AND THERE IS NO KEYCHAIN CODE IN THIS REPOSITORY ─
//
// Swept before writing this: no Go keychain call anywhere, and no keychain dependency — `cli/go.mod`
// and `cli/go.sum` contain no `zalando/go-keyring`, `99designs/keyring`, `danieljoos/wincred` or
// `godbus/dbus`. The only real OS-keychain use in the repository is TypeScript through VS Code's own
// API (`tools/vscode/src/extension.ts:136`), which is not reachable from here, and
// `control/orchestrator/src/secrets/keyring.ts` is a FILE-BACKED master key despite its name. So a
// keychain here is three platform backends or a new dependency, and either is a decision this slice
// is not entitled to make on its own.
//
// THE FALLBACK, AND THE TRADE, STATED: a 0600 file in a 0700 directory, refused rather than repaired
// when its mode is wider. That is strictly weaker than a keychain in exactly one way — any process
// running AS THIS USER can read it, where a keychain can require a prompt — and equal in every other:
// both lose to root, and the file already matches how this installation stores its other secrets
// (`config.yaml` holds the fleet SSH key and four service tokens at 0600; `config.go:733`
// `refuseIfReadableByOthers` is the enforcement, and this function copies it). What it buys is that
// §1's premise still holds — this engine's one secret protects a TOPOLOGY, not an account, because
// there is no account — and that nothing new enters `go.mod` for it.
//
// A ROTATED PASSPHRASE MAKES AN EXISTING STACK UNREADABLE, which is why this mints once and then only
// ever reads. `Pulumi.<stack>.yaml`'s first line is an `encryptionsalt` derived from it (verified on
// 3.244.0), so a regenerated passphrase does not fail loudly — it fails at the next `config get` of a
// secret, long after the file that could have decrypted it was overwritten.
func Passphrase(dir string) (string, error) {
	path := filepath.Join(dir, PassphraseFileName)
	if b, err := os.ReadFile(path); err == nil {
		if err := refuseIfReadableByOthers(path); err != nil {
			return "", err
		}
		if v := strings.TrimSpace(string(b)); v != "" {
			return v, nil
		}
		// An EMPTY file is not a passphrase and must not be used as one. `workspace.ts:100-104`:
		// "a DIY backend cannot use the default secrets provider, and an empty passphrase silently
		// changes how secrets encrypt." Treated as absent, so the next line mints one.
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("no randomness to mint a Pulumi passphrase: %w", err)
	}
	v := hex.EncodeToString(buf)
	// O_EXCL FIRST, AND A LOSER OF THE RACE READS RATHER THAN OVERWRITES. Two `kontra up`s on a fresh
	// install must not both write: the loser's value would already be in a stack's encryptionsalt by
	// the time it was replaced. The lock in program.go makes that race unreachable for a converge;
	// this is the same rule one layer down, because `--preview` takes no lock on purpose and also
	// mints. An EXISTING-BUT-EMPTY file is not a competing mint — it is the state the read above
	// already called absent — so that one is overwritten.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		if b, rerr := os.ReadFile(path); rerr == nil && strings.TrimSpace(string(b)) != "" {
			return strings.TrimSpace(string(b)), nil
		}
		f, err = os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	}
	if err != nil {
		return "", fmt.Errorf("writing %s: %w", path, err)
	}
	if _, err := f.WriteString(v + "\n"); err != nil {
		f.Close()
		return "", err
	}
	return v, f.Close()
}

// refuseIfReadableByOthers is `config.go:733`'s check, at the one other file in this installation
// that holds a live secret.
//
// GROUP AND WORLD ONLY — the owner's bits are their business — and skipped on Windows, where the
// reported mode does not mean what it does on Unix. It is a COPY rather than an import because
// `internal/config` is the configuration loader and importing it here would make the host engine
// depend on config parsing to read one file; the shared thing is the rule, and the rule is four
// lines.
func refuseIfReadableByOthers(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	if bad := info.Mode().Perm() & 0o077; bad != 0 {
		return fmt.Errorf("%s is mode %04o — it is the key that decrypts %s and it is readable by %s. "+
			"kontra refuses to use a secret somebody else can see. Fix it with:\n\n    chmod 600 %s\n",
			path, info.Mode().Perm(), StateDirName, whoElse(bad), path)
	}
	return nil
}

func whoElse(bad os.FileMode) string {
	switch {
	case bad&0o070 != 0 && bad&0o007 != 0:
		return "your group and every other user on this machine"
	case bad&0o070 != 0:
		return "your group"
	default:
		return "every other user on this machine"
	}
}
