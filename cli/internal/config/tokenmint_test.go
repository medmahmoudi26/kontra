package config

// tokenmint_test.go — `kontra token mint <key>`: filling a BLANK token in an existing config.
//
// THE BUG THIS COMMAND EXISTS FOR is not in any of these tests, because it is an absence. `kontra
// init` mints `state`, `panel` and `run`, but it writes config.yaml with `writeIfAbsent` — so an
// installation made before a key was minted keeps the blank forever. For `run` a blank means the
// Run surface is OPEN, and every message that reported it ("Set KONTRA_RUN_TOKEN to gate them")
// named no command, because there was none. What these tests hold down is that the fix cannot
// overreach: it fills a blank, it never rotates, and it never touches anything else in the file.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v2"
)

// parseForTest reads the result back the way the CLI does. Asserting on the FILE would pass for a
// file that is no longer valid YAML, which is the one thing a hand-rolled text edit can break.
func parseForTest(t *testing.T, body string) (Config, error) {
	t.Helper()
	var cfg Config
	err := yaml.Unmarshal([]byte(body), &cfg)
	return cfg, err
}

// A config in the shape this actually meets in the wild: an OLD one, whose `run:` is blank and
// whose comment is the pre-minting wording. The other keys are set, which is the point — a
// half-configured file is the normal case, not an edge one.
const oldConfig = `controller: "10.124.0.2"

fleet:
  ssh_key: "/root/.ssh/id_rsa"

tokens:
  # Raw actor state and the infra routes — anything that can spend money.
  state: "aaaa"
  # The query workbench's presigned reads. Falls back to the state token when empty.
  explore: "bbbb"
  # Mints Dashboard WebSocket tickets, and nothing else.
  panel: "cccc"
  # EMPTY MEANS OPEN: serve and run admit anyone who can reach the API. Set it to require a
  # bearer token on both — they can start a workflow that provisions machines.
  run: ""

data:
  ducklake_password: "dddd"
`

// writeConfig puts body at the config path for a temp KONTRA_HOME, 0600 so the permission guard
// (which is not what these tests are about) stays out of the way.
func writeTokenConfig(t *testing.T, body string) string {
	t.Helper()
	dir := home(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// THE WHOLE POINT: a blank `run` is filled, and the file is otherwise byte-for-byte what it was.
//
// The second half is the half worth having. A YAML round trip would produce a valid file that
// passes any assertion about `run` while silently deleting every comment — and those comments are
// the only thing in this file that says what a blank `run` MEANS.
func TestMintFillsTheBlankAndChangesNothingElse(t *testing.T) {
	path := writeTokenConfig(t, oldConfig)
	var out strings.Builder
	if err := CmdTokenMint(&out, []string{"run"}); err != nil {
		t.Fatalf("mint run: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)

	cfg, err := parseForTest(t, got)
	if err != nil {
		t.Fatalf("the result is not valid YAML: %v", err)
	}
	if cfg.Tokens.Run == "" {
		t.Fatal("run is still blank — the command reported success and filled nothing")
	}
	if len(cfg.Tokens.Run) < 40 {
		t.Errorf("run token is only %d chars; mintToken is 256 bits", len(cfg.Tokens.Run))
	}
	// Every OTHER value survives.
	if cfg.Tokens.State != "aaaa" || cfg.Tokens.Explore != "bbbb" || cfg.Tokens.Panel != "cccc" {
		t.Errorf("other tokens changed: %+v", cfg.Tokens)
	}
	if cfg.Controller != "10.124.0.2" || cfg.Data.DuckLakePassword != "dddd" {
		t.Errorf("unrelated settings changed: controller=%q ducklake=%q", cfg.Controller, cfg.Data.DuckLakePassword)
	}

	// And the file is the SAME FILE: identical once the one line is put back.
	restored := strings.Replace(got, `  run: "`+cfg.Tokens.Run+`"`, `  run: ""`, 1)
	if restored != oldConfig {
		t.Errorf("more than the run line changed.\n--- got ---\n%s\n--- want ---\n%s", restored, oldConfig)
	}

	// The value is printed, because a containerised orchestrator reads its environment and not
	// this file — an operator who cannot see it here has to `cat` the config instead.
	if !strings.Contains(out.String(), cfg.Tokens.Run) {
		t.Error("the minted token is not printed, so it cannot be carried into a container's environment")
	}
	if !strings.Contains(out.String(), "KONTRA_RUN_TOKEN") {
		t.Error("the output does not name the environment variable it corresponds to")
	}
}

// Mode survives. The file holds credentials and is created 0600; an editor that rewrites it must
// not be the thing that widens it.
func TestMintKeepsTheFilePrivate(t *testing.T) {
	path := writeTokenConfig(t, oldConfig)
	if err := CmdTokenMint(&strings.Builder{}, []string{"run"}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("config is now %04o, want 0600", fi.Mode().Perm())
	}
}

// NEVER ROTATES. A key with a value is refused — and the refusal must not have written anything,
// which is the assertion that catches a "check after write" ordering.
func TestMintRefusesToRotate(t *testing.T) {
	path := writeTokenConfig(t, oldConfig)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	err = CmdTokenMint(&strings.Builder{}, []string{"state"})
	if err == nil {
		t.Fatal("minting over a set token must be refused")
	}
	if !strings.Contains(err.Error(), "already has a value") {
		t.Errorf("the refusal does not say why: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Error("a refused mint still wrote to the file")
	}
}

func TestMintRejectsAnUnknownKey(t *testing.T) {
	writeTokenConfig(t, oldConfig)
	err := CmdTokenMint(&strings.Builder{}, []string{"admin"})
	if err == nil {
		t.Fatal("an unknown token name must be refused")
	}
	// It lists the real ones rather than only saying no.
	for _, key := range TokenKeys {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("the error does not name %q as an option: %v", key, err)
		}
	}
}

// A COMMENTED-OUT KEY IS NOT A KEY. This is the failure a `strings.Replace` would produce: it finds
// `run: ""` inside the comment, fills THAT, and reports success over a file whose real key is
// untouched — an operator told the surface is closed when it is still open.
func TestMintIgnoresACommentedKey(t *testing.T) {
	body := `tokens:
  state: "aaaa"
  # run: ""
  run: ""
`
	path := writeTokenConfig(t, body)
	if err := CmdTokenMint(&strings.Builder{}, []string{"run"}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `  # run: ""`) {
		t.Error("the comment was edited; it is a comment, not a key")
	}
	cfg, err := parseForTest(t, string(got))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Tokens.Run == "" {
		t.Fatal("the real key was not filled")
	}
}

// The scan stops at the end of the block. A key of the same name in ANOTHER section must not be
// mistaken for this one — the reason this is a block-scoped scan and not a file-wide replace.
func TestMintStaysInsideTheTokensBlock(t *testing.T) {
	body := `tokens:
  state: "aaaa"

schedule:
  run: ""
`
	writeTokenConfig(t, body)
	err := CmdTokenMint(&strings.Builder{}, []string{"run"})
	if err == nil {
		t.Fatal("there is no `run` under `tokens:` — filling `schedule.run` would be wrong")
	}
	if !strings.Contains(err.Error(), "no `tokens.run` key") {
		t.Errorf("wrong error: %v", err)
	}
}

// A bare `run:` (YAML null) is blank too, and so is `''`.
func TestMintTreatsEveryBlankFormAsBlank(t *testing.T) {
	for _, form := range []string{`  run:`, `  run: ""`, `  run: ''`} {
		body := "tokens:\n  state: \"aaaa\"\n" + form + "\n"
		writeTokenConfig(t, body)
		if err := CmdTokenMint(&strings.Builder{}, []string{"run"}); err != nil {
			t.Errorf("%q should be blank and fillable: %v", strings.TrimSpace(form), err)
		}
	}
}

func TestMintNeedsAConfig(t *testing.T) {
	home(t) // KONTRA_HOME set, but nothing written
	err := CmdTokenMint(&strings.Builder{}, []string{"run"})
	if err == nil {
		t.Fatal("with no config at all this must fail")
	}
	if !strings.Contains(err.Error(), "kontra init") {
		t.Errorf("the error should name the command that creates one: %v", err)
	}
}

// A config with no `tokens:` block at all says so, rather than reporting success over a file it
// never changed.
func TestMintNeedsATokensBlock(t *testing.T) {
	writeTokenConfig(t, "controller: \"10.0.0.1\"\n")
	err := CmdTokenMint(&strings.Builder{}, []string{"run"})
	if err == nil {
		t.Fatal("a config with no tokens block must fail")
	}
	if !strings.Contains(err.Error(), "no `tokens:` block") {
		t.Errorf("wrong error: %v", err)
	}
}

// EVERY KEY IS MINTABLE, and each says what filling it DOES. `run` closes a surface and the others
// open one — a single word like "minted" for both is how an operator enables the money-spending
// routes believing they just locked something down.
func TestEveryTokenKeyHasAnEffectSentence(t *testing.T) {
	for _, key := range TokenKeys {
		effect, ok := tokenEffect[key]
		if !ok || strings.TrimSpace(effect) == "" {
			t.Errorf("%q has no sentence saying what minting it changes", key)
		}
	}
	if len(tokenEffect) != len(TokenKeys) {
		t.Errorf("tokenEffect has %d entries, TokenKeys has %d — one is missing from the other",
			len(tokenEffect), len(TokenKeys))
	}
	// The two asymmetric cases, stated in opposite directions.
	if !strings.Contains(tokenEffect["run"], "CLOSED") {
		t.Error("minting `run` CLOSES the Run surface and the message must say so")
	}
	if !strings.Contains(tokenEffect["state"], "SPEND MONEY") {
		t.Error("minting `state` enables the routes that destroy Droplets; the message must say so")
	}
}
