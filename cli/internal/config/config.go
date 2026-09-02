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
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/medmahmoudi26/kontra/cli/internal/cliio"
	"github.com/medmahmoudi26/kontra/cli/internal/cliutil"
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
	}
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
// already set.
//
// The precedence is the whole contract: environment first, file second. A container is configured
// by its environment and must not have that quietly replaced by a file that happened to be
// mounted; an operator on a laptop has no environment and gets the file. Neither can surprise the
// other.
func ApplyConfig(c *Config) {
	for key, val := range c.EnvFor() {
		if val == "" {
			continue
		}
		if _, set := os.LookupEnv(key); set {
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

tokens:
  # Raw actor state and the infra routes — anything that can spend money.
  state: ""
  # The query workbench's presigned reads. Falls back to the state token when empty.
  explore: ""
  # Mints Dashboard WebSocket tickets, and nothing else.
  panel: ""
  # EMPTY MEANS OPEN: serve and run admit anyone who can reach the API. Set it to require a
  # bearer token on both — they can start a workflow that provisions machines.
  run: ""

data:
  # The DuckLake catalog's Postgres password, shared by the API and the materializer.
  ducklake_password: ""
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
// Only `state`. `explore` deliberately stays blank: it falls back to the state token, and the
// SPA bakes VITE_KONTRA_EXPLORE_TOKEN at IMAGE BUILD time — minting one here would hand a
// prebuilt bundle a token the server no longer expects, and the Datasets console would 401 into
// an empty grid.
func FreshConfig() string {
	tok, err := mintToken()
	if err != nil {
		// A config with a blank state token is the old behaviour: usable, with the infra routes
		// off. Better than refusing to initialise because the machine has no entropy to spare.
		return CONFIG_TEMPLATE
	}
	return strings.Replace(CONFIG_TEMPLATE, `  state: ""`, `  state: "`+tok+`"`, 1)
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

    kontra workflow serve <folder> --tmux
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

	created, err := writeIfAbsent(cliutil.ConfigPath(root), FreshConfig(), 0o600)
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
	} else {
		fmt.Fprintf(w, "%s/ is already set up (config.yaml left alone)\n", rel)
	}
	return nil
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

// LoadConfig reads `.kontra/config.yaml`. A MISSING file is not an error — an installation
// configured entirely by environment is a legitimate one (every container is), so the zero value
// is the honest answer rather than a failure.
func LoadConfig() (*Config, error) {
	root, err := cliutil.KontraRoot()
	if err != nil {
		return &Config{}, nil // no home to read: environment-only, which is what CI and containers are
	}
	body, err := os.ReadFile(cliutil.ConfigPath(root))
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
