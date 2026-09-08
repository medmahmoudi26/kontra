package config

// `.kontra/` — the guards on a directory that holds live credentials.
//
// Two failures here would be quiet and expensive. Re-running install.sh must never overwrite a
// config.yaml somebody typed a DigitalOcean token into; and a value in the ENVIRONMENT must never
// be replaced by one in the file, because every container is configured by its environment and a
// mounted `.kontra/` silently winning would repoint a production process at a laptop's settings.

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/medmahmoudi26/kontra/cli/internal/cliutil"
)

// home points `.kontra/` at a temp dir for one test. KONTRA_HOME is the same override the
// orchestrator container uses to find a mounted directory, so this exercises the real path.
func home(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), ".kontra")
	t.Setenv("KONTRA_HOME", dir)
	return dir
}

func TestInitCreatesTheLayout(t *testing.T) {
	dir := home(t)
	var out strings.Builder
	if err := InitKontra(&out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"config.yaml",
		filepath.Join("workflows", "README.md"),
		filepath.Join("actors", "README.md"),
	} {
		if _, err := os.Stat(filepath.Join(dir, want)); err != nil {
			t.Errorf("%s missing after init: %v", want, err)
		}
	}

	// It holds a cloud credential. 0600/0700, not whatever the umask happened to be.
	fi, err := os.Stat(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if mode := fi.Mode().Perm(); mode != 0o600 {
		t.Errorf("config.yaml is %o, want 0600 — it holds a DigitalOcean token", mode)
	}
	if fi, err := os.Stat(dir); err == nil && fi.Mode().Perm() != 0o700 {
		t.Errorf(".kontra/ is %o, want 0700", fi.Mode().Perm())
	}
}

func TestInitNeverOverwritesCredentials(t *testing.T) {
	// install.sh runs this on EVERY run. Rewriting the template over an operator's filled-in
	// config would destroy a Pulumi passphrase, and a lost passphrase makes a stack's secrets
	// unreadable — unrecoverable, not merely annoying.
	dir := home(t)
	var out strings.Builder
	if err := InitKontra(&out); err != nil {
		t.Fatal(err)
	}
	mine := "controller: \"10.9.9.9\"\ntokens:\n  run: \"sekrit\"\n"
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(mine), 0o600); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	if err := InitKontra(&out); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != mine {
		t.Fatalf("init rewrote config.yaml:\n%s", got)
	}
	// And it says so, rather than reporting a creation that did not happen.
	if !strings.Contains(out.String(), "left alone") {
		t.Errorf("a second init must say it changed nothing, got: %s", out.String())
	}
}

func TestMissingConfigIsNotAnError(t *testing.T) {
	// An installation configured entirely by environment is legitimate — every container is one.
	home(t)
	c, err := LoadConfig()
	if err != nil {
		t.Fatalf("a missing config.yaml must be the zero value, not an error: %v", err)
	}
	if c.Controller != "" || c.Tokens.Run != "" {
		t.Errorf("want the zero value, got %+v", c)
	}
}

func TestMalformedConfigNamesTheFile(t *testing.T) {
	dir := home(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("controller: [unclosed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadConfig()
	if err == nil {
		t.Fatal("malformed YAML must fail")
	}
	// A YAML error with no path sends an operator to the wrong file.
	if !strings.Contains(err.Error(), "config.yaml") {
		t.Errorf("the error must name the file, got: %v", err)
	}
}

func TestTheEnvironmentAlwaysWins(t *testing.T) {
	// THE RULE. config.yaml FILLS GAPS; it never overrides. A container gets its configuration
	// from compose, and a `.kontra/` that happened to be mounted must not repoint it.
	t.Setenv("KONTRA_CONTROLLER", "10.0.0.1")
	t.Setenv("KONTRA_RUN_TOKEN", "from-the-environment")

	ApplyConfig(&Config{
		Controller: "10.9.9.9",
		Tokens:     TokensConfig{Run: "from-the-file", State: "only-in-the-file"},
	})

	if got := os.Getenv("KONTRA_CONTROLLER"); got != "10.0.0.1" {
		t.Errorf("the file overrode the environment: KONTRA_CONTROLLER=%q", got)
	}
	if got := os.Getenv("KONTRA_RUN_TOKEN"); got != "from-the-environment" {
		t.Errorf("the file overrode the environment: KONTRA_RUN_TOKEN=%q", got)
	}
	// …and a value the environment did NOT set is filled from the file. That is the whole point.
	if got := os.Getenv("KONTRA_STATE_TOKEN"); got != "only-in-the-file" {
		t.Errorf("the file did not fill an unset value: KONTRA_STATE_TOKEN=%q", got)
	}
}

func TestAnEmptyConfigValueSetsNothing(t *testing.T) {
	// An empty string in the file is "not configured", not "configure it to empty". Exporting it
	// would turn an unset optional into a set-but-blank one — and KONTRA_RUN_TOKEN blank vs unset
	// is the difference between a gated control surface and an open one.
	t.Setenv("KONTRA_PANEL_TOKEN", "")
	os.Unsetenv("KONTRA_PANEL_TOKEN")
	ApplyConfig(&Config{})
	if _, set := os.LookupEnv("KONTRA_PANEL_TOKEN"); set {
		t.Error("an empty config value must leave the variable UNSET")
	}
}

func TestEveryConfigFieldReachesEnv(t *testing.T) {
	// A setting added to the struct and forgotten in envFor reads perfectly in the file and
	// reaches nothing — the exact failure `.kontra/` exists to end. Counting leaf fields against
	// the table catches it at compile-time-adjacent distance instead of in production.
	full := Config{
		Controller: "c",
		// The placement fields are here for the same reason as the credentials: `region` and `vpc`
		// were CONSTANTS in the orchestrator until a control plane outside sfo3 proved it could not
		// provision a fleet it could reach, and a setting that reaches nothing is exactly what this
		// test exists to catch.
		Fleet: FleetConfig{
			DigitalOceanToken: "a", PulumiPassphrase: "b", SSHKey: "d",
			Region: "nyc1", VPC: "vpc-1", Size: "s-2vcpu-4gb", Image: "ubuntu-24-04-x64",
			SSHKeyIDs: "111,222",
		},
		Tokens: TokensConfig{State: "e", Explore: "f", Panel: "g", Run: "h"},
		Data:   DataConfig{DuckLakePassword: "i"},
		// The console login. `Auth.Users` is a SLICE, so `leafFields` counts it as one setting and
		// `envFor` collapses it into one base64 value — which is why the two agree here even though
		// the shape differs from every other section.
		Auth: AuthConfig{Users: []AuthUser{{Name: "admin", PasswordHash: "scrypt$1$1$1$AA$BB"}}},
	}
	mapped := full.EnvFor()

	// DERIVED variables are not settings. They carry a field's value to a second place that needs
	// it under another name, so they must not be counted against the struct — but they are listed
	// here by name rather than skipped by a rule, so adding one stays a deliberate act.
	//
	// KONTRA_REDIS_BIND is the controller address again: a fleet Machine dials the Controller for
	// the state store, so the address it calls home on IS the address Redis must bind to. Making
	// that a second setting would make it a second thing to hold in agreement, and the failure
	// when they disagree is a run that provisions a fleet and dies on `connection refused`.
	//
	// KONTRA_BIND is the same fact for every OTHER port a Machine dials — Temporal 7233, the API
	// 8088, SeaweedFS 8333. They published on 0.0.0.0 until this existed, so a controller on a
	// public droplet served all of them to the internet, including an unauthenticated API that can
	// run code and provision machines. Derived, not configured, for the reason above: one address.
	derived := map[string]string{"KONTRA_REDIS_BIND": "Controller", "KONTRA_BIND": "Controller"}

	settings := len(mapped) - len(derived)
	if n := leafFields(reflect.TypeOf(full)); n != settings {
		t.Fatalf("Config has %d settings but envFor maps %d (+%d derived) — a new one reaches nothing",
			n, settings, len(derived))
	}
	for name := range derived {
		if _, ok := mapped[name]; !ok {
			t.Errorf("%s is listed as derived but envFor does not map it", name)
		}
	}
	// Every mapped value must be non-empty for a fully-populated Config: a field wired to the
	// wrong source would show up here as a blank.
	for k, v := range mapped {
		if v == "" {
			t.Errorf("%s is mapped to an empty value — wired to the wrong field?", k)
		}
	}
}

// leafFields counts the non-struct fields of a struct, recursively.
func leafFields(t reflect.Type) int {
	n := 0
	for i := 0; i < t.NumField(); i++ {
		if f := t.Field(i); f.Type.Kind() == reflect.Struct {
			n += leafFields(f.Type)
		} else {
			n++
		}
	}
	return n
}

func TestTheTemplateParsesAsItsOwnConfig(t *testing.T) {
	// The template is prose with keys in it, and it is the first thing an operator edits. If it
	// does not round-trip through the struct that reads it, every fresh install starts with a
	// warning about a file kontra itself wrote.
	dir := home(t)
	var out strings.Builder
	if err := InitKontra(&out); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig()
	if err != nil {
		t.Fatalf("the shipped template does not parse: %v", err)
	}
	// Every value blank EXCEPT the minted state token: a template that ships a Controller address
	// or a credential is how somebody deploys to somebody else's machine — but the state token is
	// this installation's own secret, generated here, and blank means the infra routes are
	// DISABLED. A fresh install left them off, which is how a run provisioned four Droplets
	// and `kontra fleet down` then answered 503.
	if c.Controller != "" || c.Fleet.DigitalOceanToken != "" || c.Fleet.PulumiPassphrase != "" {
		t.Errorf("the template ships non-empty values: %+v", c)
	}
	if c.Tokens.State == "" {
		t.Error("a fresh config must mint a state token, or the infra routes are off")
	}
	// Minted, not constant — two installations must not share a secret.
	second := FreshConfig()
	if strings.Contains(second, c.Tokens.State) {
		t.Error("the state token is the same on every install")
	}
	if _, err := os.Stat(filepath.Join(dir, "config.yaml")); err != nil {
		t.Fatal(err)
	}
}

// ONE VARIABLE, ONE ANSWER, IN BOTH LANGUAGES. `control/orchestrator/src/sources.ts:kontraHome` resolves
// `~/.kontra` when KONTRA_HOME is unset — that is what the register form prefills and what a
// registration records — and this side used to walk up to the checkout's `.kontra/` instead.
//
// The half that could not survive is the CWD. The orchestrator spawns this CLI with `cwd` set to
// the registered FOLDER, so for a folder under `~/.kontra/actors` the walk found no
// docker-compose.yml above it at all: cliutil.KontraRoot errored, LoadConfig answered with the zero value,
// and the worker started with none of the tokens or credentials — each failing later, none of them
// naming this.
func TestKontraRootIsTheHomeTheOrchestratorAlsoResolves(t *testing.T) {
	t.Setenv("KONTRA_HOME", "")
	os.Unsetenv("KONTRA_HOME")
	// A directory with no checkout above it — what a registered folder under `~/.kontra/actors`
	// looks like, and what the old rule turned into an error.
	t.Chdir(t.TempDir())

	root, err := cliutil.KontraRoot()
	if err != nil {
		t.Fatalf("with no checkout in sight, `.kontra/` must still resolve: %v", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory on this machine")
	}
	if want := filepath.Join(home, cliutil.KontraDir); root != want {
		t.Errorf("cliutil.KontraRoot() = %q, want %q — the TypeScript peer answers the second one", root, want)
	}
}

func TestKontraHomeStillWins(t *testing.T) {
	// The override is the ONLY reason the container works: compose points it at the mounted
	// `.kontra/`, and the CLI it spawns inherits it.
	t.Setenv("KONTRA_HOME", "/srv/checkout/.kontra/")
	root, err := cliutil.KontraRoot()
	if err != nil {
		t.Fatal(err)
	}
	if root != "/srv/checkout/.kontra" {
		t.Errorf("KONTRA_HOME must win (cleaned), got %q", root)
	}
}

// A shell standing in a checkout reads THAT checkout's installation. This is the everyday case and
// it is the one a home-only resolver broke: `kontra init` writes `<checkout>/.kontra/config.yaml`,
// and with it unread `kontra explore` says "no explore token", `kontra panels list` says "no panel
// token" and a fleet command finds no DigitalOcean token — three failures, none of them naming the
// file that holds all three values.
func TestACheckoutInstallationWinsOverTheHome(t *testing.T) {
	checkout := t.TempDir()
	if err := os.WriteFile(filepath.Join(checkout, "docker-compose.yml"), []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	own := filepath.Join(checkout, cliutil.KontraDir)
	if err := os.MkdirAll(own, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(own, "config.yaml"), []byte("controller: \"10.0.0.1\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KONTRA_HOME", "")
	os.Unsetenv("KONTRA_HOME")
	t.Setenv("HOME", t.TempDir()) // a home this test owns, with no installation in it
	t.Chdir(checkout)

	got, err := cliutil.KontraRoot()
	if err != nil {
		t.Fatal(err)
	}
	if got != own {
		t.Errorf("cliutil.KontraRoot() = %q, want the checkout's own %q", got, own)
	}

	// KONTRA_HOME still wins outright — it is the operator's explicit answer, and compose sets it
	// for every container in the control plane.
	elsewhere := filepath.Join(t.TempDir(), cliutil.KontraDir)
	t.Setenv("KONTRA_HOME", elsewhere)
	if got, err := cliutil.KontraRoot(); err != nil || got != elsewhere {
		t.Errorf("KONTRA_HOME must win, got %q (err %v)", got, err)
	}
}

// The case that made the home the default in the first place: the orchestrator spawns this CLI
// with cwd set to a REGISTERED FOLDER, which need not sit under any checkout. Walking up finds no
// docker-compose.yml, and the old checkout-only resolver returned an ERROR there — LoadConfig then
// answered with the zero value and the worker started with no tokens and no fleet credentials.
func TestNoCheckoutAboveCwdFallsBackToTheHome(t *testing.T) {
	home := t.TempDir()
	folder := filepath.Join(home, cliutil.KontraDir, "actors", "probe")
	if err := os.MkdirAll(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KONTRA_HOME", "")
	os.Unsetenv("KONTRA_HOME")
	t.Setenv("HOME", home)
	t.Chdir(folder)

	got, err := cliutil.KontraRoot()
	if err != nil {
		t.Fatalf("a registered folder outside a checkout must still resolve a home: %v", err)
	}
	if want := filepath.Join(home, cliutil.KontraDir); got != want {
		t.Errorf("cliutil.KontraRoot() = %q, want %q", got, want)
	}
}

// `config.yaml` is the marker, not the bare directory. `.kontra/` gets created by things other
// than `init` — this very repo carries one in its worktrees — and an empty one is not an
// installation to prefer over a home that has a real config in it.
func TestAnEmptyCheckoutDirIsNotAnInstallation(t *testing.T) {
	checkout := t.TempDir()
	if err := os.WriteFile(filepath.Join(checkout, "docker-compose.yml"), []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(checkout, cliutil.KontraDir), 0o700); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, cliutil.KontraDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, cliutil.KontraDir, "config.yaml"), []byte("controller: \"10.0.0.2\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KONTRA_HOME", "")
	os.Unsetenv("KONTRA_HOME")
	t.Setenv("HOME", home)
	t.Chdir(checkout)

	got, err := cliutil.KontraRoot()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, cliutil.KontraDir); got != want {
		t.Errorf("an empty checkout dir must not shadow a real home installation: got %q, want %q", got, want)
	}
}
