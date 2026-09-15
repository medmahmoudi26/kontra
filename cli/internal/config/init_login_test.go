package config

import (
	"bytes"
	"encoding/base64"
	"os"
	"strings"
	"testing"

	"github.com/medmahmoudi26/kontra/cli/internal/creds"
	"gopkg.in/yaml.v2"
)

func TestInstallProducesAWorkingLogin(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KONTRA_HOME", dir)
	var out bytes.Buffer
	if err := InitKontra(&out); err != nil {
		t.Fatal(err)
	}
	t.Log("\n" + out.String())

	raw, err := os.ReadFile(dir + "/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var c Config
	if err := yaml.Unmarshal(raw, &c); err != nil {
		t.Fatalf("the generated config does not parse: %v", err)
	}
	if len(c.Auth.Users) != 1 || c.Auth.Users[0].Name != DefaultConsoleUser {
		t.Fatalf("no console user was generated: %+v", c.Auth)
	}
	if !strings.HasPrefix(c.Auth.Users[0].PasswordHash, "scrypt$") {
		t.Fatalf("password_hash is not an scrypt hash: %q", c.Auth.Users[0].PasswordHash)
	}
	// THE REAL PROPERTY, and the first version of this test got it wrong by grepping for
	// "password:" — which matches "password_hash:" and fails on a correct file.
	//
	// Parse the printed password back out of the install banner, then assert BOTH halves: it
	// verifies against the stored hash (so the login actually works), and it does not appear in the
	// file (so the hash is a hash).
	var password string
	for _, line := range strings.Split(out.String(), "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[0] == "password" {
			password = f[1]
		}
	}
	if password == "" {
		t.Fatal("init did not print a password — an install with no console login")
	}
	ok, err := creds.Verify(password, c.Auth.Users[0].PasswordHash)
	if err != nil || !ok {
		t.Fatalf("the printed password does not verify against the stored hash: ok=%v err=%v", ok, err)
	}
	if strings.Contains(string(raw), password) {
		t.Error("the PLAINTEXT password reached the config file")
	}
	// NON-VACUOUS: if Verify said yes to anything, the assertion above would prove nothing.
	if wrong, _ := creds.Verify(password+"x", c.Auth.Users[0].PasswordHash); wrong {
		t.Error("a different password verified")
	}
}

// A SECOND `init` — every recreate, every image upgrade — must still answer the question the
// README tells an operator to ask.
//
// THE BUG THIS PINS. The password is printed once, into the logs of whichever container ran the
// first init. Recreate it and `docker compose logs cli` shows only the new container, where init
// is a no-op. The README's recovery line is `docker compose logs cli | grep -A4 'console login'`,
// and it returned NOTHING — measured on a cluster installed at 23:47 and recreated at 02:16, with
// a perfectly good `admin` sitting in config.yaml the whole time. An operator who lost the
// password was told to run a command that could not answer, about an account that did exist.
//
// The literal "console login" is what that grep matches, so it is asserted rather than described.
func TestASecondInitStillAnswersWhereTheLoginWent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KONTRA_HOME", dir)

	var first bytes.Buffer
	if err := InitKontra(&first); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(first.String(), "console login") {
		t.Fatalf("the first init printed no login banner:\n%s", first.String())
	}

	var second bytes.Buffer
	if err := InitKontra(&second); err != nil {
		t.Fatal(err)
	}
	got := second.String()
	t.Log("\n" + got)

	if !strings.Contains(got, "console login") {
		t.Errorf("a second init said nothing a `grep 'console login'` could find:\n%s", got)
	}
	if !strings.Contains(got, DefaultConsoleUser) {
		t.Errorf("it did not name the user that exists:\n%s", got)
	}
	if !strings.Contains(got, "kontra user add") {
		t.Errorf("it did not name the one command that gets you back in:\n%s", got)
	}
	// It must NOT invent a password: only the hash is stored, and printing anything that looked
	// like one would be worse than saying nothing.
	if strings.Contains(got, "THIS IS THE ONLY TIME") {
		t.Errorf("a second init reprinted the first-run banner:\n%s", got)
	}
}

// THE LOGIN AN OPERATOR CAN STILL FIND SIX WEEKS LATER.
//
// `kontra init` used to print the password to stdout and nowhere else, and say so: "THIS IS THE
// ONLY TIME THIS IS SHOWN". In the compose install that stdout belongs to the `cli` container, and
// `docker compose logs cli` shows only the CURRENT container — so one recreate destroyed the only
// copy of the credential and config.yaml kept an scrypt hash nobody can reverse. That is how the
// owner of this appliance lost the `admin` login with no recovery path at all.
func TestTheInstallLeavesThePasswordOnDiskInCleartext(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KONTRA_HOME", dir)
	var out bytes.Buffer
	if err := InitKontra(&out); err != nil {
		t.Fatal(err)
	}

	body, err := os.ReadFile(ConsolePasswordPath(dir))
	if err != nil {
		t.Fatalf("install did not write %s: %v", ConsolePasswordPath(dir), err)
	}

	// The file must hold the credential that actually opens the account — not merely SOME password.
	// Verifying it against the stored hash is the only assertion that cannot pass on a wrong value.
	raw, err := os.ReadFile(dir + "/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var c Config
	if err := yaml.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	var recorded string
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, "#") || strings.TrimSpace(line) == "" {
			continue
		}
		name, pass, ok := strings.Cut(line, "\t")
		if ok && name == DefaultConsoleUser {
			recorded = pass
		}
	}
	if recorded == "" {
		t.Fatalf("no %q line in %s:\n%s", DefaultConsoleUser, ConsolePasswordPath(dir), body)
	}
	ok, err := creds.Verify(recorded, c.Auth.Users[0].PasswordHash)
	if err != nil {
		t.Fatalf("verifying the saved cleartext: %v", err)
	}
	if !ok {
		t.Fatal("the saved cleartext does not open the account it names")
	}

	// 0600, because the whole justification for writing a password down is that only this operator
	// can read it. A mode that has slipped makes this file the leak instead of the fix.
	info, err := os.Stat(ConsolePasswordPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if bad := info.Mode().Perm() & 0o077; bad != 0 {
		t.Fatalf("%s is mode %04o — it holds a cleartext password and must be 0600",
			ConsolePasswordPath(dir), info.Mode().Perm())
	}
}

// `kontra user add` IS THE DOCUMENTED RECOVERY PATH, AND IT DID NOT WORK.
//
// It wrote config.yaml and returned. The orchestrator has no YAML parser: it reads
// KONTRA_CONSOLE_USERS out of runtime.env, which orchestrator-entrypoint sources. So the command
// printed a password, told the operator to restart, and the restart re-sourced an env that still
// carried only the original user. MEASURED on the live appliance before the fix: config.yaml listed
// `med` and `admin`, runtime.env base64-decoded to `admin` alone, and POST /api/login as `med`
// answered 401 — an operator following the printed instructions, locked out, with no way to tell a
// typo from a broken command.
func TestUserAddReachesTheOrchestratorAndNotOnlyTheYAML(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KONTRA_HOME", dir)
	var out bytes.Buffer
	if err := InitKontra(&out); err != nil {
		t.Fatal(err)
	}

	var added bytes.Buffer
	if err := CmdUserAdd(&added, []string{"second"}); err != nil {
		t.Fatal(err)
	}

	env, err := os.ReadFile(RuntimeEnvPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	line := ""
	for _, l := range strings.Split(string(env), "\n") {
		if strings.HasPrefix(l, "KONTRA_CONSOLE_USERS=") {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("runtime.env has no KONTRA_CONSOLE_USERS after `user add`:\n%s", env)
	}
	encoded := strings.Trim(strings.TrimPrefix(line, "KONTRA_CONSOLE_USERS="), "'")
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("KONTRA_CONSOLE_USERS is not base64: %v", err)
	}
	// Both accounts, because adding one must never drop the other — that would turn a recovery
	// command into a lockout.
	for _, want := range []string{DefaultConsoleUser, "second"} {
		if !strings.Contains(string(decoded), `"name":"`+want+`"`) {
			t.Fatalf("runtime.env does not carry %q, so the orchestrator cannot admit it:\n%s", want, decoded)
		}
	}

	// And the new password is on disk too, for the same reason the install's is.
	body, err := os.ReadFile(ConsolePasswordPath(dir))
	if err != nil {
		t.Fatalf("`user add` did not record the password: %v", err)
	}
	if !strings.Contains(string(body), "second\t") {
		t.Fatalf("no `second` line in %s:\n%s", ConsolePasswordPath(dir), body)
	}
}
