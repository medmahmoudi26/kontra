package config

// config.go — `.kontra/`: where this installation's own configuration and code live. `~/.kontra`,
// or wherever KONTRA_HOME points; `control/orchestrator/src/sources.ts:kontraHome` is the peer that must
// answer the same, and cliutil.KontraRoot below says what it cost when it did not.
//
// Everything kontra needs from an operator used to be environment variables in a gitignored
// `.env`: the DigitalOcean token, the Pulumi passphrase, the fleet SSH key, the Controller
// address, four service tokens. That works and it is invisible — there is no file to read to
// find out what this installation is, no comment saying what a value is for, and each one fails
// separately without naming itself.
//
//	.kontra/
//	  config.yaml     what this installation is: the Controller, the fleet credentials, the tokens
//	  workflows/      YOUR caller workflows — what the Workflows page lists, serves and runs
//	  actors/         YOUR actors — beside the ones in examples/, which stay examples
//
// ENVIRONMENT STILL WINS, and that is not a transitional kindness — it is the rule. A container
// gets its configuration from its environment, `docker-compose.yml` substitutes from `.env`, and
// CI has neither a `.kontra/` nor a reason to grow one. So config.yaml FILLS GAPS: a value set in
// the environment is used as-is, and nothing here can silently change what an existing
// installation already does.
//
// A `.kontra/` INSIDE A CHECKOUT IS GITIGNORED, whole — and one under `~` is outside the repo to
// begin with. config.yaml holds live credentials, and workflows/ and actors/ hold an operator's own
// code, which is theirs and not this repository's.

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"github.com/medmahmoudi26/kontra/cli/internal/cliio"
	"github.com/medmahmoudi26/kontra/cli/internal/cliutil"
	"github.com/medmahmoudi26/kontra/cli/internal/creds"
	"gopkg.in/yaml.v2"
)

// Config is `.kontra/config.yaml`.
//
// Flat-ish and grouped by WHAT ASKS FOR IT rather than by type, because the question an operator
// arrives with is "what do I need to set to run a fleet", not "what are all the strings".
type Config struct {
	// Where fleet Machines call home — Temporal, the catalog, S3, Redis. NOT localhost: on a
	// Machine that means its own loopback, and the Worker starts, registers nothing, looks idle.
	Controller string `yaml:"controller,omitempty"`

	Fleet  FleetConfig  `yaml:"fleet,omitempty"`
	Tokens TokensConfig `yaml:"tokens,omitempty"`
	Data   DataConfig   `yaml:"data,omitempty"`
	Auth   AuthConfig   `yaml:"auth,omitempty"`
}

// AuthConfig is who may sign in to the CONSOLE, and it exists because a browser cannot read this
// file. The CLI authenticates by holding it; a browser gets in by signing in against it and being
// handed a token to use as `Authorization` from then on.
//
// ONLY HASHES ARE STORED. `kontra init` generates the password, PRINTS IT ONCE, and keeps the
// scrypt hash — so this file staying 0600 protects a credential nobody can recover from it anyway.
// The encoding is `scrypt$N$r$p$salt$hash` and `shared/conformance/login.json` pins it against the
// orchestrator's verifier, which is written in another language.
type AuthConfig struct {
	Users []AuthUser `yaml:"users,omitempty"`
}

// AuthUser is one console login. Two engineers sharing an instance is the case this is sized for;
// `kontra user add` appends another.
type AuthUser struct {
	Name string `yaml:"name"`
	// PasswordHash is `scrypt$N$r$p$salt$hash`. Never a password.
	PasswordHash string `yaml:"password_hash"`
}

type FleetConfig struct {
	// The cloud credential. The ONLY process that should ever hold it is orchestrator-infra.
	DigitalOceanToken string `yaml:"digitalocean_token,omitempty"`
	// Encrypts Pulumi's state secrets. Lose it and the stack's secrets are unreadable — which is
	// why it belongs in a file an operator can back up rather than in a shell history.
	PulumiPassphrase string `yaml:"pulumi_passphrase,omitempty"`
	// Path to the private key the Controller reaches its Machines with.
	SSHKey string `yaml:"ssh_key,omitempty"`

	// WHERE A FLEET IS PROVISIONED. Empty leaves the orchestrator's own defaults (sfo3 and the
	// VPC this repo was developed against), so an existing installation is unchanged.
	//
	// THIS WAS NOT CONFIGURABLE AT ALL and it made kontra un-installable anywhere else: a control
	// plane in another region could only provision Machines in sfo3, where they cannot reach its
	// Temporal, Redis or object store — and the run hangs on `fleet.ready()` rather than failing.
	// MEASURED on a fresh nyc1 controller.
	//
	// SET THE REGION AND LEAVE THE VPC EMPTY unless you have a specific one: a VPC is REGIONAL, so
	// a region from here with a VPC from somewhere else is the one combination DigitalOcean
	// refuses. Empty means "that region's default VPC", which is what setting only a region meant.
	Region string `yaml:"region,omitempty"`
	VPC    string `yaml:"vpc,omitempty"`
	// Machine size and image for a fleet Machine. Empty keeps s-1vcpu-2gb / ubuntu-22-04-x64.
	Size  string `yaml:"size,omitempty"`
	Image string `yaml:"image,omitempty"`
	// DigitalOcean SSH KEY IDS (not paths), comma-separated. Account-scoped: another account's ids
	// produce Machines nobody can log into, which stays invisible until a Terminal is opened.
	SSHKeyIDs string `yaml:"ssh_key_ids,omitempty"`
}

type TokensConfig struct {
	// Raw actor state + the infra routes: anything that can spend money.
	State string `yaml:"state,omitempty"`
	// The query workbench's presigned reads. Falls back to `state`, as checkBearer does.
	Explore string `yaml:"explore,omitempty"`
	// Mints Dashboard WebSocket tickets and nothing else.
	Panel string `yaml:"panel,omitempty"`
	// EMPTY MEANS OPEN. serve/start admit anyone who can reach the API unless this is set —
	// stated here because it is the one token whose absence is a decision rather than a default.
	Run string `yaml:"run,omitempty"`
}

type DataConfig struct {
	// The DuckLake catalog's Postgres password. Shared by the materializer and the API, which
	// must read the catalog the other writes.
	DuckLakePassword string `yaml:"ducklake_password,omitempty"`
}

// envFor maps each config field to the environment variable it fills. ONE table, so a new setting
// cannot be added to the struct and silently reach nothing — `TestEveryConfigFieldReachesEnv`
// walks it against the YAML tags.
func (c *Config) EnvFor() map[string]string {
	return map[string]string{
		"KONTRA_CONTROLLER": c.Controller,
		// The address the state store binds to, DERIVED from the controller rather than
		// configured beside it. A fleet Machine's actor dials the Controller for Redis, so the
		// address it calls home on and the address Redis listens on are the same fact; making it
		// a second setting is making it a second thing to get wrong. See the `redis` service.
		"KONTRA_REDIS_BIND": c.Controller,
		// THE SAME FACT FOR EVERY OTHER PORT A MACHINE DIALS. Redis was the only service that
		// bound an address; Temporal (7233), the orchestrator API (8088) and SeaweedFS (8333)
		// published on 0.0.0.0, so a controller on a public droplet served all three — plus the
		// filer, the master and the panels streamer — to the internet. MEASURED from a second
		// droplet: ten open ports, including an unauthenticated API that can run code and
		// provision machines.
		//
		// Derived from the controller for KONTRA_REDIS_BIND's reason, and it is the same reason:
		// the address a Machine calls home on and the address these listen on are one fact.
		// Unset leaves compose's `${KONTRA_BIND:-127.0.0.1}` default, so an installation with no
		// fleet configured is closed rather than open.
		"KONTRA_BIND":              c.Controller,
		"DIGITALOCEAN_TOKEN":       c.Fleet.DigitalOceanToken,
		"PULUMI_CONFIG_PASSPHRASE": c.Fleet.PulumiPassphrase,
		"KONTRA_FLEET_SSH_KEY":     c.Fleet.SSHKey,
		// Where a fleet lands. Read by control/orchestrator/src/infra/programs/fleet.ts:fleetDefaults;
		// every one of these is empty-means-keep-the-default, so an installation that sets none
		// behaves exactly as it did before they existed.
		"KONTRA_FLEET_REGION":         c.Fleet.Region,
		"KONTRA_FLEET_VPC":            c.Fleet.VPC,
		"KONTRA_FLEET_SIZE":           c.Fleet.Size,
		"KONTRA_FLEET_IMAGE":          c.Fleet.Image,
		"KONTRA_FLEET_SSH_KEY_IDS":    c.Fleet.SSHKeyIDs,
		"KONTRA_STATE_TOKEN":          c.Tokens.State,
		"KONTRA_EXPLORE_TOKEN":        c.Tokens.Explore,
		"KONTRA_PANEL_TOKEN":          c.Tokens.Panel,
		"KONTRA_RUN_TOKEN":            c.Tokens.Run,
		"KONTRA_DUCKLAKE_PG_PASSWORD": c.Data.DuckLakePassword,
		// WHO MAY SIGN IN TO THE CONSOLE, travelling the same way every other config value does:
		// through the environment. The orchestrator has no YAML parser and adding one to the
		// control plane to read two fields is a dependency this does not need — the same reasoning
		// that picked scrypt over bcrypt for the hash itself.
		//
		// BASE64 OF JSON, and the encoding is not decoration. The hash is `scrypt$N$r$p$salt$hash`:
		// it contains `$`, and compose substitutes `$` in values it passes through, so the raw form
		// would arrive corrupted in exactly the deployment this is meant for — and corrupted into
		// something that still parses as a hash, so it would fail as "wrong password" rather than
		// as a configuration error.
		"KONTRA_CONSOLE_USERS": encodeConsoleUsers(c.Auth.Users),
	}
}

// encodeConsoleUsers is base64(JSON) of the console accounts, or "" when there are none.
//
// Empty is meaningful and is not the same as absent-and-broken: the login route reports
// `disabled: no console user is configured` and names the command that creates one, rather than
// presenting a form nobody can pass.
func encodeConsoleUsers(users []AuthUser) string {
	if len(users) == 0 {
		return ""
	}
	type wire struct {
		Name         string `json:"name"`
		PasswordHash string `json:"password_hash"`
	}
	out := make([]wire, 0, len(users))
	for _, u := range users {
		if u.Name == "" || u.PasswordHash == "" {
			continue // a half-written entry is not an account; it must not become one that admits nobody
		}
		out = append(out, wire{u.Name, u.PasswordHash})
	}
	if len(out) == 0 {
		return ""
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(raw)
}

// A `strandedConfig` used to live here: it named a `<checkout>/.kontra/config.yaml` that the
// home-only cliutil.KontraRoot had stopped reading, and printed a one-line fix. It is gone because
// cliutil.KontraRoot now READS that file rather than warning about it — the warning existed only to
// describe a loss that no longer happens, and a message that can never fire is worse than no
// message, because the next reader has to work out that it cannot.

// WorkflowsDir and ActorsDir are what the UI lists.
func workflowsDir(root string) string { return filepath.Join(root, "workflows") }
func actorsDir(root string) string    { return filepath.Join(root, "actors") }

// ApplyConfig exports config values into this process's environment, WITHOUT overriding anything
// already set to a VALUE.
//
// The precedence is the whole contract: environment first, file second. A container is configured
// by its environment and must not have that quietly replaced by a file that happened to be
// mounted; an operator on a laptop has no environment and gets the file. Neither can surprise the
// other.
//
// ── AN EMPTY VARIABLE IS A GAP, NOT A DECISION, AND THAT COST A WORKING INSTALL ─────────────────
//
// This used to skip on `LookupEnv` alone, so a variable that was SET TO THE EMPTY STRING blocked
// the file. That is the normal shape of a compose file: `KONTRA_CONSOLE_USERS: "${KONTRA_CONSOLE_USERS:-}"`
// passes the variable through when the operator set one and passes an EMPTY STRING when they did
// not — it is always set. MEASURED on the copy-paste docker install: `kontra init` generated the
// admin account and wrote it to config.yaml, `kontra up` started, and every sign-in answered
// `503 disabled: no console user is configured`. A control plane that had just printed a password
// nobody could use.
//
// The symmetry is the argument. Six lines up, an empty value IN THE FILE is skipped for exactly
// this reason — "not configured" rather than "configured to nothing" — and every consumer reads ""
// as absent. Reading an empty variable the other way made the two halves of one rule disagree.
//
// It is also the safe direction to be wrong in. Filling a blank from the file can only produce the
// configuration the operator wrote down; honouring the blank produces a control plane whose
// credentials, tokens and Controller address all silently vanished.
func ApplyConfig(c *Config) {
	for key, val := range c.EnvFor() {
		if val == "" {
			continue
		}
		if cur, set := os.LookupEnv(key); set && cur != "" {
			continue
		}
		_ = os.Setenv(key, val)
	}
}

// LoadAndApplyConfig is what main() calls once, before any command runs. Best-effort by design:
// a malformed config.yaml should fail the commands that need it with their own message, not stop
// `kontra help` from working.
func LoadAndApplyConfig() {
	c, err := LoadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v\n", err)
		return
	}
	ApplyConfig(c)
}

// --- kontra init ------------------------------------------------------------------------------

// CONFIG_TEMPLATE is what a fresh `.kontra/config.yaml` says.
//
// Written as a commented template rather than an empty struct dump, because the file's job is to
// be READ: it is the one place that answers "what does this installation need", and a bare set of
// empty keys answers nothing. Every value is left blank — a template that ships a default
// Controller address is how somebody deploys to somebody else's machine.
const CONFIG_TEMPLATE = `# kontra — this installation's configuration.
#
# THE ENVIRONMENT WINS. Anything already set in the environment is used as-is and nothing here
# overrides it; these values fill the gaps. That is what lets a container be configured by
# compose and a laptop by this file, without either surprising the other.
#
# This file holds live credentials. .kontra/ is gitignored, whole — keep it that way.

# Where fleet Machines call home: Temporal, the catalog, S3, Redis. NOT localhost — on a Machine
# that means its own loopback, so the Worker starts, registers nothing, and looks idle.
controller: ""

fleet:
  # The cloud credential. Only orchestrator-infra should ever hold it.
  digitalocean_token: ""
  # Encrypts Pulumi's state secrets. Lose it and the stack's secrets are unreadable, so back it
  # up somewhere that is not this machine.
  pulumi_passphrase: ""
  # The private key the Controller reaches its Machines with.
  ssh_key: ""

# WHO MAY SIGN IN TO THE CONSOLE. Generated by "kontra init", which prints the password ONCE and
# keeps only the hash — there is nothing here to recover, which is the point. "kontra user add"
# appends another; two engineers sharing an instance is what this is sized for.
#
# A browser cannot read this file, which is the whole reason a login exists: signing in against
# these hashes is where the console's Authorization token comes from. It used to be BAKED into the
# browser bundle at build time, which broke on every rotation and once took the query workbench
# down silently.
auth:
  users: []

tokens:
  # Raw actor state and the infra routes — anything that can spend money.
  state: ""
  # The query workbench's presigned reads. Falls back to the state token when empty.
  explore: ""
  # Mints Dashboard WebSocket tickets, and nothing else. GENERATED at install: blank means every
  # panel route answers 503 and the Dashboard is a blank rectangle. Nothing has to present it —
  # the browser never holds it and the socket carries only a minted ticket.
  panel: ""
  # GENERATED at install, because EMPTY MEANS OPEN: serve, start, stop and a Dataset's tag and
  # rename admit anyone who can reach the API. They can start a workflow that provisions machines.
  # Blank it deliberately if you want that surface open; it will not be filled in again.
  run: ""

data:
  # The DuckLake catalog's Postgres password, shared by the API and the materializer.
  ducklake_password: ""

# TEMPORAL TLS IS ENVIRONMENT-ONLY, and deliberately has no key here. The same five variables are
# read by the orchestrator (TypeScript), both Python hosts and every Go binary — sixteen call
# sites — and a value that lived in this file as well would be a second policy that agrees today
# and drifts later. It is documented here because this file's job is to answer "what does this
# installation need", and leaving it out would answer that incompletely:
#
#   KONTRA_TEMPORAL_TLS              1|true|yes|on — TLS with the system trust store
#   KONTRA_TEMPORAL_TLS_CA           PEM path: the server's root CA, for a private CA
#   KONTRA_TEMPORAL_TLS_CERT         PEM path: this client's certificate   ] both, or neither
#   KONTRA_TEMPORAL_TLS_KEY          PEM path: this client's private key   ]
#   KONTRA_TEMPORAL_TLS_SERVER_NAME  SNI override, for a proxy in front of the server
#
# Unset is plaintext, which is what a local install wants. ANY ONE of them turns TLS on, so a CA
# with no switch does not silently connect in the clear. A file that cannot be read is a refusal
# and never a fall back. See sdk/go/temporaltls and shared/conformance/temporal_tls.json.
`

// FreshConfig is CONFIG_TEMPLATE with the state token MINTED.
//
// EMPTY MEANS TWO OPPOSITE THINGS IN THIS FILE, and that asymmetry cost a fleet. `run: ""` is
// documented as OPEN — serve and start admit anyone who can reach the API. `state: ""` is the
// reverse: the infra routes fail CLOSED and answer 503 `disabled: set one of KONTRA_STATE_TOKEN`.
//
// MEASURED on a fresh install: a run provisioned four Droplets through a workflow — which
// goes through the orchestrator's own worker and needs no token — then failed, leaving them
// running, and `kontra fleet down` could not destroy them, because the route that destroys a
// fleet was disabled by the blank the template had just written. Create worked and destroy did
// not, on the same installation, for machines that bill by the hour. Recovering them meant
// hand-editing this file and restarting the control plane.
//
// So it is generated rather than blank: functional and closed at the same time. 256 bits from
// crypto/rand — an operator who wants their own value still just edits the file, and an existing
// config is never rewritten (writeIfAbsent), so this only ever affects a genuinely new install.
//
// THREE OF THE FOUR ARE MINTED, and which three is the whole of this function.
//
//	state   the reason above: blank fails CLOSED, and a fleet was stranded by that.
//	panel   blank means every panel route answers 503 `disabled: set KONTRA_PANEL_TOKEN` and the
//	        Dashboard is a blank rectangle on a fresh install. Nothing has to PRESENT it — the
//	        browser never holds it, `routes/panels.ts` proxies with the value from its own
//	        environment and the socket carries only a minted ticket — so generating it costs an
//	        operator nothing and buys them a working wall.
//	run     blank means OPEN: serve, start, stop, a Dataset's tag and rename all admit anyone who
//	        can reach the API. That is the asymmetry this file already warns about in capitals, and
//	        an open default on an instance two people share is the wrong way round.
//
// `explore` STAYS BLANK, and it is not an oversight. It falls back to the state token, and the SPA
// bakes VITE_KONTRA_EXPLORE_TOKEN at IMAGE BUILD time — minting one here would hand a prebuilt
// bundle a token the server no longer expects, and the Datasets console would 401 into an empty grid.
//
// MINTING `run` CLOSES A DOOR THE CLI WALKS THROUGH, so `cli/dataset.go`'s tag and rename now send
// it. Those were the only two CLI call sites on a run-gated route; everything else gated by it is
// the console's, and the console gets the token from the orchestrator's environment rather than
// holding one. Checked route by route rather than assumed.
func FreshConfig() string {
	out := CONFIG_TEMPLATE
	// Each independently: a machine that runs out of entropy midway should still get the tokens it
	// managed to mint rather than none. A blank one is the old behaviour for that key, which is
	// documented above and survivable — refusing to initialise is not.
	for _, key := range []string{"state", "panel", "run"} {
		tok, err := mintToken()
		if err != nil {
			continue
		}
		out = strings.Replace(out, `  `+key+`: ""`, `  `+key+`: "`+tok+`"`, 1)
	}
	return out
}

// mintToken returns 256 bits of randomness, URL-safe so it can travel in a header or a query
// string without escaping.
func mintToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// README_WORKFLOWS and README_ACTORS explain what the two directories are for, in the directories
// themselves — an empty directory tells you nothing, and these are the first things a new
// installation shows in the UI.
const README_WORKFLOWS = `# workflows/

YOUR caller workflows, one FOLDER each — the same shape an actor has with actor.json + actor.py.
The .py is an ordinary Temporal workflow that drives deployed Actors; see
examples/python/workflows/ for runnable ones to copy.

    nscheck/
      workflow.py     the code. serve runs this one.
      description.md  what it does and what it leaves behind. The Workflows page shows it.

The Workflows page lists this directory. Its two buttons are these two commands:

    kontra workflow serve <folder> --mode dev
    kontra workflow start <folder>

The queue is DERIVED from the folder's content (wf-<name>-<digest>), never typed — serve and start
agree on it, and start refuses if nothing is serving that digest.

A flat <name>.py in here still serves, and so does the old spelling of one that has since become
a folder: "serve nscheck.py" finds nscheck/workflow.py.
`

const README_ACTORS = `# actors/

YOUR actors, one directory each (actor.py or main.go, plus actor.json).

The ones in examples/ stay examples; this is where your own live, so a checkout update never
touches them.

    kontra serve --actor ~/.kontra/actors/<name>
    kontra build --actor ~/.kontra/actors/<name>

(or wherever KONTRA_HOME points — that is the one variable that says where this directory is.)
`

// InitKontra creates `.kontra/` and its contents. IDEMPOTENT and non-destructive: an existing
// config.yaml is never rewritten, because it holds credentials somebody typed in.
func InitKontra(w io.Writer) error {
	root, err := cliutil.KontraRoot()
	if err != nil {
		return err
	}
	for _, dir := range []string{root, workflowsDir(root), actorsDir(root)} {
		if err := os.MkdirAll(dir, 0o700); err != nil { // 0700: it holds credentials
			return fmt.Errorf("creating %s: %w", dir, err)
		}
	}

	// THE CONSOLE LOGIN IS GENERATED HERE, and this is the only moment the password exists in a
	// readable form. It is printed once and hashed into config.yaml; nothing can recover it
	// afterwards, which is the point of storing a hash rather than a secret.
	//
	// Generated rather than prompted: `kontra init` runs in installers and scripts where there is
	// no terminal to prompt on, and a default password is worse than no password because it is the
	// one an attacker tries first.
	fresh, password, err := freshConfigWithLogin()
	if err != nil {
		return err
	}
	created, err := writeIfAbsent(cliutil.ConfigPath(root), fresh, 0o600)
	if err != nil {
		return err
	}
	if _, err := writeIfAbsent(filepath.Join(workflowsDir(root), "README.md"), README_WORKFLOWS, 0o644); err != nil {
		return err
	}
	if _, err := writeIfAbsent(filepath.Join(actorsDir(root), "README.md"), README_ACTORS, 0o644); err != nil {
		return err
	}

	rel := root
	if cwd, err := os.Getwd(); err == nil {
		if r, err := filepath.Rel(cwd, root); err == nil && !strings.HasPrefix(r, "..") {
			rel = r
		}
	}
	if created {
		fmt.Fprintf(w, "created %s/ — put your credentials in %s/config.yaml\n", rel, rel)
		if password != "" {
			// PRINTED ONCE, AND SAID SO. There is no second chance and no recovery path, so the
			// sentence has to carry that rather than leave it to be discovered.
			fmt.Fprintf(w, "\nconsole login:\n\n")
			fmt.Fprintf(w, "    user      %s\n", DefaultConsoleUser)
			fmt.Fprintf(w, "    password  %s\n\n", password)
			// "THIS IS THE ONLY TIME THIS IS SHOWN" was the previous sentence here, and it was true:
			// the password went to stdout and nowhere else, so a recreated `cli` container took the
			// only copy with it. It is no longer true, and the line that says where the second copy
			// lives is the whole point of writing one.
			if err := recordConsolePassword(root, DefaultConsoleUser, password); err != nil {
				fmt.Fprintf(w, "NOT saved to %s (%v) — WRITE THE PASSWORD ABOVE DOWN NOW. Only the\n", ConsolePasswordPath(root), err)
				fmt.Fprintf(w, "hash is stored, and this is the only time it is shown.\n")
			} else {
				fmt.Fprintf(w, "Saved to %s (mode 0600); config.yaml keeps only the hash.\n", ConsolePasswordPath(root))
			}
		}
	} else {
		fmt.Fprintf(w, "%s/ is already set up (config.yaml left alone)\n", rel)
		// SAY SOMETHING ABOUT THE LOGIN ON EVERY BOOT, NOT ONLY THE FIRST.
		//
		// The password is printed once, by the branch above, into the logs of the container that
		// happened to run `init`. Recreate that container — an image upgrade, a `--force-recreate`,
		// anything — and `docker compose logs cli` shows only the NEW container, where init is a
		// no-op and said nothing. So the README's own recovery line,
		// `docker compose logs cli | grep -A4 'console login'`, returned NOTHING, and an operator
		// who had lost the password was told to run a command that could not answer.
		//
		// MEASURED: a cluster installed at 23:47 and recreated at 02:16 had zero matches for that
		// grep while a perfectly good `admin` user sat in config.yaml.
		//
		// The phrase "console login" is repeated here deliberately: it is what the documented grep
		// matches on, so this branch has to carry it to be found at all. It cannot reprint the
		// password — only the hash is stored, which is the point — so it names the users that exist
		// and the one command that gets you back in.
		if c, err := LoadConfig(); err == nil && len(c.Auth.Users) > 0 {
			names := make([]string, 0, len(c.Auth.Users))
			for _, u := range c.Auth.Users {
				names = append(names, u.Name)
			}
			fmt.Fprintf(w, "\nconsole login — already created, and NOT recoverable:\n\n")
			fmt.Fprintf(w, "    user(s)   %s\n\n", strings.Join(names, ", "))
			fmt.Fprintf(w, "The password was shown once, when this installation was created, and only\n")
			fmt.Fprintf(w, "the hash is stored. Lost it? `kontra user add <name>` makes another.\n")
		}
	}
	// Node processes in the Compose cluster do not parse YAML. They read the same env ApplyConfig
	// exports. runtime.env is that dump, sourced by orchestrator-entrypoint after workspace-init.
	if err := writeRuntimeEnv(root); err != nil {
		return err
	}
	return nil
}

// RuntimeEnvPath is the KEY=VALUE dump Node services source. It is rewritten on every init, and by
// `kontra user add`, so a new account is visible to the orchestrator after a restart.
//
// THE "and by `kontra user add`" WAS A LIE FOR THE LIFE OF THIS COMMENT. It claimed the rewrite
// already happened there and it did not: `CmdUserAdd` wrote config.yaml and returned. The account
// existed in YAML that nothing in the cluster parses, `KONTRA_CONSOLE_USERS` still carried only the
// original user, and the command printed a password, told the operator to restart, and the restart
// changed nothing. MEASURED on this install: config.yaml listed `med` and `admin`, runtime.env
// base64-decoded to `admin` alone, and POST /api/login as `med` answered 401.
//
// That made it the worst possible bug to be on the end of, because `user add` IS the documented
// recovery path — the sentence `kontra init` prints when a password is lost points straight at it.
// An operator locked out of the console followed the instructions, was given a credential that did
// not work, and had nothing to distinguish "I mistyped it" from "the command does not function".
func RuntimeEnvPath(root string) string { return filepath.Join(root, "runtime.env") }

// ConsolePasswordPath is where the console password is kept IN CLEARTEXT, mode 0600.
//
// WHY A PRODUCT THAT HASHES ITS PASSWORDS ALSO WRITES ONE DOWN. The hash is what protects the
// account if config.yaml leaks, and that stays. What it cannot do is answer "what is the password"
// six weeks after install, and this appliance had no answer at all: `kontra init` printed the
// password once into a container's stdout, `docker compose logs cli` shows only the CURRENT
// container's output, and a recreate therefore destroyed the only copy. The owner of this install
// lost the `admin` login exactly that way, and nothing in the system could recover it.
//
// The threat model is what makes this defensible rather than careless. This is a single-tenant
// local appliance: the API binds to 127.0.0.1 by default, the file is 0600 in a directory only the
// operator uses, and `refuseIfReadableByOthers` already refuses to read config.yaml — which holds
// every service token in cleartext — if its mode has slipped. A machine-local reader who can open
// this file can already open config.yaml beside it and mint tokens for every gated route. So this
// widens no boundary that the existing file did not already define.
//
// APPEND, NEVER REWRITE. Each account is one line, so adding a second login cannot destroy the
// record of the first — which is the failure this whole file exists to stop happening again.
func ConsolePasswordPath(root string) string { return filepath.Join(root, "console-password") }

// recordConsolePassword appends one account's cleartext credential to ConsolePasswordPath.
//
// A FAILURE HERE DOES NOT STOP THE CALLER, matching `freshConfigWithLogin`'s posture: the account
// is already written and already works, so failing the command over the convenience copy would
// trade a working login for no login. The error is returned so the caller can SAY so — an operator
// who is told the file was not written still knows to keep the password that is on their screen.
func recordConsolePassword(root, name, password string) error {
	if password == "" {
		return nil
	}
	path := ConsolePasswordPath(root)
	if err := refuseIfReadableByOthers(path); err != nil {
		return err
	}
	// O_APPEND|O_CREATE, 0600. The mode applies only on creation, which is why the check above
	// exists for a file that already had its permissions loosened by something else.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	defer f.Close()
	if info, statErr := f.Stat(); statErr == nil && info.Size() == 0 {
		if _, err := fmt.Fprintf(f, "%s", consolePasswordHeader); err != nil {
			return fmt.Errorf("writing %s: %w", path, err)
		}
	}
	if _, err := fmt.Fprintf(f, "%s\t%s\n", name, password); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// The header explains the file to whoever opens it months later, which is the entire audience.
const consolePasswordHeader = `# kontra console logins, in CLEARTEXT. Mode 0600 — keep it that way.
#
# This exists because the password is otherwise printed exactly once, to a container's stdout, and
# ` + "`docker compose logs cli`" + ` shows only the current container: one recreate and it is gone for good.
# config.yaml beside this file stores only an scrypt hash, which cannot be reversed.
#
# One TAB-separated "user<TAB>password" line per account, appended as accounts are made.
`

func writeRuntimeEnv(root string) error {
	c, err := LoadConfig()
	if err != nil {
		return err
	}
	env := c.EnvFor()
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("# generated by kontra init — sourced by orchestrator-entrypoint, do not edit\n")
	for _, k := range keys {
		if env[k] == "" {
			continue
		}
		fmt.Fprintf(&b, "%s=%s\n", k, shellSingleQuote(env[k]))
	}
	return os.WriteFile(RuntimeEnvPath(root), []byte(b.String()), 0o600)
}

func shellSingleQuote(v string) string {
	return "'" + strings.ReplaceAll(v, "'", `'"'"'`) + "'"
}

// DefaultConsoleUser is the account `kontra init` creates. A name rather than a blank so the login
// form has something to say, and one an operator can add beside rather than replace.
const DefaultConsoleUser = "admin"

// ConsolePasswordLength is what init generates. Twenty characters of the unambiguous alphabet is
// ~114 bits — comfortably beyond anything reachable against a scrypt hash, and still short enough
// to be read off a terminal and typed once.
const ConsolePasswordLength = 20

// freshConfigWithLogin is FreshConfig plus the generated console credential.
//
// It returns the PLAINTEXT alongside, because the caller is the only thing that will ever see it:
// this function hashes it into the file and forgets it.
//
// A FAILURE HERE DOES NOT STOP THE INSTALL. An install with no console login is the behaviour
// before this existed — usable from the CLI, with the console asking for a credential nobody has —
// and refusing to initialise because the machine had no entropy to spare would be worse. It is the
// same posture the token minting above takes, for the same reason.
func freshConfigWithLogin() (string, string, error) {
	out := FreshConfig()
	password, err := creds.GeneratePassword(ConsolePasswordLength)
	if err != nil {
		return out, "", nil
	}
	hash, err := creds.Hash(password)
	if err != nil {
		return out, "", nil
	}
	// The template ships `users: []`; replacing the empty list keeps one source for the surrounding
	// prose rather than assembling the section here.
	block := "users:\n    - name: " + DefaultConsoleUser + "\n      password_hash: \"" + hash + "\""
	replaced := strings.Replace(out, "users: []", block, 1)
	if replaced == out {
		// The template moved out from under this. Say so rather than silently shipping no login.
		return out, "", fmt.Errorf("config template no longer has an `auth.users` list to fill")
	}
	return replaced, password, nil
}

// writeIfAbsent writes body only when the path does not exist. Reports whether it wrote.
//
// O_EXCL rather than a Stat-then-Write: the check and the write are one syscall, so two `kontra
// init` runs racing cannot both decide the file is missing and one clobber the other's credentials.
func writeIfAbsent(path, body string, mode os.FileMode) (bool, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if errors.Is(err, os.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("creating %s: %w", path, err)
	}
	defer f.Close()
	if _, err := io.WriteString(f, body); err != nil {
		return false, fmt.Errorf("writing %s: %w", path, err)
	}
	return true, nil
}

func CmdInit(args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("usage: kontra init")
	}
	return InitKontra(cliio.Stdout)
}

// refuseIfReadableByOthers is the check SSH has and this file did not.
//
// The config is CREATED 0600 and the directory 0700, and until now nothing looked at either again.
// A mode is not a property of a file's contents: `cp -r` without `-p`, an editor that rewrites
// through a temp file under a loose umask, a restore from backup, a `docker` bind-mount, an
// extracted tarball — every one of those can land this file at 0644, and it holds the DigitalOcean
// token, the Pulumi passphrase, the fleet SSH key and four service tokens.
//
// That matters exactly when kontra is being used the way it is meant to be used: two engineers
// sharing one instance, each with their own account on the box. `ssh` refuses a key in this state
// and says so; there is no reason for a file holding strictly more to be quieter about it.
//
// GROUP AND WORLD ONLY. The owner's bits are their business, and an 0400 config is a legitimate
// thing to want. Windows reports modes that do not mean what they do on Unix, so this is skipped
// there rather than guessed at.
//
// A MISSING FILE IS NOT AN ERROR HERE — that is LoadConfig's contract (an installation configured
// entirely by environment is a legitimate one, and every container is), so a Stat failure of any
// kind falls through to the read, which knows what to do with it.
func refuseIfReadableByOthers(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil // missing, or unreadable for a reason ReadFile will report better
	}
	if bad := info.Mode().Perm() & 0o077; bad != 0 {
		return fmt.Errorf(
			"%s is mode %04o — it holds live credentials and is readable by %s. kontra refuses to "+
				"read it rather than use a secret somebody else can see. Fix it with:\n\n"+
				"    chmod 600 %s\n",
			path, info.Mode().Perm(), whoElse(bad), path)
	}
	return nil
}

// whoElse names the audience, because "mode 0644" is not a sentence most people parse under time
// pressure and "your group" is.
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

// LoadConfig reads `.kontra/config.yaml`. A MISSING file is not an error — an installation
// configured entirely by environment is a legitimate one (every container is), so the zero value
// is the honest answer rather than a failure.
func LoadConfig() (*Config, error) {
	root, err := cliutil.KontraRoot()
	if err != nil {
		return &Config{}, nil // no home to read: environment-only, which is what CI and containers are
	}
	path := cliutil.ConfigPath(root)
	if err := refuseIfReadableByOthers(path); err != nil {
		return nil, err
	}
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &Config{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", cliutil.ConfigPath(root), err)
	}
	var c Config
	if err := yaml.Unmarshal(body, &c); err != nil {
		// Name the file. A YAML error with no path sends an operator to the wrong one.
		return nil, fmt.Errorf("%s is not valid YAML: %w", cliutil.ConfigPath(root), err)
	}
	return &c, nil
}

// --- what reads the config, moved from api.go with it ---

// The two answers a fresh installation gives when nothing is configured. They live here rather
// than beside the flags because both halves of the CLI resolve them the same way: the Warden's
// `ca` commands take them as flag defaults, and every control-plane command dials them.
const (
	DefaultTemporal  = "localhost:7233"
	DefaultNamespace = "default"
)

func Controller() string {
	cfg, err := LoadConfig()
	if err != nil || cfg == nil {
		return ""
	}
	return strings.TrimSpace(cfg.Controller)
}

func TemporalAddress() string {
	if v := os.Getenv("KONTRA_ADDRESS"); v != "" {
		return v
	}
	if host := Controller(); host != "" {
		return host + ":7233"
	}
	return DefaultTemporal
}

func TemporalNamespace() string { return cliutil.EnvOr("KONTRA_NAMESPACE", DefaultNamespace) }

// CmdUserAdd adds a console login: `kontra user add <name>`.
//
// THE PASSWORD IS GENERATED AND PRINTED ONCE, exactly as `kontra init` does it, and for the same
// two reasons. Prompting needs a terminal, and this runs in installers and scripts that have none;
// and a password an operator chooses on the spot is the one they choose everywhere else.
//
// IT REWRITES config.yaml BY APPENDING A USER, which is the one destructive thing in this file — so
// it refuses rather than guesses when the shape is not what it expects, and it never touches an
// existing entry. Re-adding a name that exists is an error, not a silent password reset: a command
// that quietly locks somebody out of their own console is worse than one that makes you type
// `--force`, and there is no `--force` here because rotating a password is not this change's job.
func CmdUserAdd(w io.Writer, args []string) error {
	if len(args) != 1 || args[0] == "" || strings.HasPrefix(args[0], "-") {
		return fmt.Errorf("usage: kontra user add <name>")
	}
	name := args[0]

	root, err := cliutil.KontraRoot()
	if err != nil {
		return err
	}
	path := cliutil.ConfigPath(root)
	if err := refuseIfReadableByOthers(path); err != nil {
		return err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading %s (run `kontra init` first): %w", path, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("%s is not valid YAML: %w", path, err)
	}
	for _, u := range cfg.Auth.Users {
		if u.Name == name {
			return fmt.Errorf("%q already has a console login — remove it from %s first if you "+
				"meant to replace it, so a rotation is never something this command does by accident",
				name, path)
		}
	}

	password, err := creds.GeneratePassword(ConsolePasswordLength)
	if err != nil {
		return err
	}
	hash, err := creds.Hash(password)
	if err != nil {
		return err
	}

	// APPENDED AS TEXT, not by re-serialising the parsed config. A round trip through the YAML
	// marshaller would drop every comment in this file, and those comments are most of what makes
	// it readable — the template exists to be read, not only parsed.
	updated, err := appendUser(string(raw), name, hash)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(updated), 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}

	// THE LINE WHOSE ABSENCE MADE THIS COMMAND A NO-OP. config.yaml is not a channel into the
	// cluster: the orchestrator has no YAML parser and reads KONTRA_CONSOLE_USERS out of
	// runtime.env, which orchestrator-entrypoint sources. Without this the account existed only in
	// a file nothing in the cluster reads, and the restart this command asks for re-sourced an
	// env that still held the previous user list. See RuntimeEnvPath.
	//
	// AFTER the config write, because it reloads from disk — `writeRuntimeEnv` calls LoadConfig
	// rather than taking the value in hand, so it must not run while the new user is still only
	// in this function's local variable.
	if err := writeRuntimeEnv(root); err != nil {
		return fmt.Errorf("account written to %s, but refreshing runtime.env failed "+
			"(the orchestrator will not see it until this succeeds): %w", path, err)
	}

	fmt.Fprintf(w, "\nconsole login:\n\n")
	fmt.Fprintf(w, "    user      %s\n", name)
	fmt.Fprintf(w, "    password  %s\n\n", password)
	if err := recordConsolePassword(root, name, password); err != nil {
		// Said out loud rather than swallowed: the password is still on screen, and an operator who
		// knows the copy did not land is an operator who writes it down themselves.
		fmt.Fprintf(w, "NOT saved to %s (%v) — keep the password above.\n", ConsolePasswordPath(root), err)
	} else {
		fmt.Fprintf(w, "Also saved to %s (mode 0600).\n", ConsolePasswordPath(root))
	}
	fmt.Fprintf(w, "Restart the orchestrator for it to take effect: docker restart kontra-api\n")
	return nil
}

// appendUser inserts one entry under `auth.users`, handling both shapes the file can be in: the
// empty list the template ships, and a list that already has entries.
func appendUser(body, name, hash string) (string, error) {
	entry := "    - name: " + name + "\n      password_hash: \"" + hash + "\"\n"
	if strings.Contains(body, "  users: []") {
		return strings.Replace(body, "  users: []\n", "  users:\n"+entry, 1), nil
	}
	if i := strings.Index(body, "  users:\n"); i >= 0 {
		// After the `users:` line, before whatever follows. Appending at the END of the list would
		// need to know where the list ends, which is the parsing this deliberately does not do.
		at := i + len("  users:\n")
		return body[:at] + entry + body[at:], nil
	}
	return "", fmt.Errorf("config has no `auth.users` list to add to — `kontra init` writes one; " +
		"add it by hand if this file predates that")
}

// tokenEffect is what FILLING each key changes, in the words the operator needs to decide.
//
// THE KEYS ARE NOT SYMMETRIC and that asymmetry is the whole reason this command exists. A blank
// `run` means the Run surface is OPEN; a blank `state` or `panel` means a surface is DISABLED. So
// minting `run` closes a door and minting the others opens one, and a message that said "minted"
// for both would hide the only thing worth knowing.
var tokenEffect = map[string]string{
	"state": "the infra routes and raw actor state stop answering 503 `disabled` — they now require this bearer.\n" +
		"    That surface can SPEND MONEY: it is what `kontra fleet down` calls to destroy Droplets.",
	"explore": "the query workbench's presigned reads stop falling back to the state token and take this instead.",
	"panel":   "the Dashboard's panel routes stop answering 503 `disabled` and can mint WebSocket tickets.",
	"run": "THE RUN SURFACE IS NOW CLOSED. serve, start, stop and a Dataset's tag and rename stop\n" +
		"    admitting anyone who can reach the API, and require this bearer instead.",
}

// TokenKeys is every key under `tokens:`, in the order the template writes them.
var TokenKeys = []string{"state", "explore", "panel", "run"}

// CmdTokenMint fills a BLANK key under `tokens:` in an EXISTING config.
//
// WHY THIS IS A COMMAND AND NOT SOMETHING `kontra init` DOES FOR YOU. `FreshConfig` mints three of
// these, but `InitKontra` writes the file with `writeIfAbsent` — so it only ever affects a
// genuinely new install, and the template says so out loud about `run`: *"Blank it deliberately if
// you want that surface open; it will not be filled in again."* That promise is worth keeping. A
// backfill that ran on its own could not tell an operator who chose an open Run surface from an
// installation that predates the key, and it would quietly overrule the first to repair the second.
//
// WHAT WAS ACTUALLY MISSING was any way to accept the offer. Every blank here has a remedy written
// beside it — `describeExposure()` ends with "Set KONTRA_RUN_TOKEN to gate them", the infra routes
// 503 with "set one of KONTRA_STATE_TOKEN" — and NONE of them named a command, because there was
// none. The remedy was to hand-edit YAML and restart the control plane, which is precisely the
// recovery `FreshConfig`'s comment records as the cost of a stranded fleet: *"Recovering them meant
// hand-editing this file and restarting the control plane."* This is that edit, done by the tool
// that knows the file's shape.
//
// IT NEVER ROTATES. A key that already has a value is refused rather than replaced — same posture
// as `kontra user add` refusing to overwrite a login. Overwriting a live token is how the console
// and the CLI end up holding different ones, and a command that can do it by accident will.
func CmdTokenMint(w io.Writer, args []string) error {
	if len(args) != 1 || args[0] == "" || strings.HasPrefix(args[0], "-") {
		return fmt.Errorf("usage: kontra token mint <%s>", strings.Join(TokenKeys, "|"))
	}
	key := args[0]
	if _, ok := tokenEffect[key]; !ok {
		return fmt.Errorf("unknown token %q — it is one of: %s", key, strings.Join(TokenKeys, ", "))
	}

	root, err := cliutil.KontraRoot()
	if err != nil {
		return err
	}
	path := cliutil.ConfigPath(root)
	if err := refuseIfReadableByOthers(path); err != nil {
		return err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading %s (run `kontra init` first): %w", path, err)
	}

	token, err := mintToken()
	if err != nil {
		return fmt.Errorf("minting a token: %w", err)
	}
	// EDITED AS TEXT, for the reason `appendUser` gives: a round trip through the YAML marshaller
	// would drop every comment in this file, and the comments are most of what makes it readable.
	updated, err := setBlankToken(string(raw), key, token, path)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(updated), 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}

	fmt.Fprintf(w, "minted tokens.%s in %s\n\n", key, path)
	fmt.Fprintf(w, "    %s\n\n", token)
	fmt.Fprintf(w, "%s\n\n", tokenEffect[key])
	// THE VALUE IS PRINTED BECAUSE THE ORCHESTRATOR MAY NOT READ THIS FILE. `ApplyConfig` exports it
	// for anything the CLI launches, but a container gets its environment from `docker-compose.yml`
	// and `.env` — and this file's own header says the environment WINS. An operator running the
	// control plane in a container has to carry the value across, and telling them to `cat` the
	// config to find it would be a worse version of printing it here.
	fmt.Fprintf(w, "Restart the orchestrator for it to take effect. If it runs in a container it reads\n")
	fmt.Fprintf(w, "its environment rather than this file — set KONTRA_%s_TOKEN there too.\n", strings.ToUpper(key))
	return nil
}

// setBlankToken replaces one blank key inside the `tokens:` block, and nothing else in the file.
//
// SCOPED TO THE BLOCK RATHER THAN THE FILE, because a bare `strings.Replace` of `run: ""` is the
// kind of edit that is correct until some other section grows a key of the same name. The scan ends
// at the first line in column zero, which is where a YAML block ends.
//
// A COMMENTED-OUT KEY IS NOT A KEY: the pattern requires the name immediately after the
// indentation, so `# run: ""` cannot match and cannot be "filled in" into a line that is still a
// comment — which would look like it worked and change nothing.
func setBlankToken(body, key, value, path string) (string, error) {
	lines := strings.Split(body, "\n")
	start := -1
	for i, line := range lines {
		if strings.TrimRight(line, " \t") == "tokens:" {
			start = i
			break
		}
	}
	if start < 0 {
		return "", fmt.Errorf("%s has no `tokens:` block — `kontra init` writes one; add it by hand "+
			"if this file predates that", path)
	}

	keyRe := regexp.MustCompile(`^([ \t]+)` + regexp.QuoteMeta(key) + `:[ \t]*(.*)$`)
	for i := start + 1; i < len(lines); i++ {
		line := lines[i]
		if strings.TrimSpace(line) == "" {
			continue
		}
		// Column zero ends the block. Anything indented is still inside it, comments included.
		if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			break
		}
		m := keyRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		// `""`, `''` and a bare `key:` are all blank. ANYTHING ELSE IS A VALUE, including something
		// this does not recognise — refusing to touch what it cannot read is the safe direction.
		switch strings.TrimSpace(m[2]) {
		case "", `""`, `''`:
			lines[i] = m[1] + key + `: "` + value + `"`
			return strings.Join(lines, "\n"), nil
		}
		return "", fmt.Errorf("tokens.%s already has a value in %s — this command fills a blank and "+
			"never rotates, so that a live token is not replaced by accident. Change it by hand if "+
			"you meant to rotate it", key, path)
	}
	return "", fmt.Errorf("%s has no `tokens.%s` key to fill — add it under `tokens:` by hand if "+
		"this file predates it", path, key)
}
