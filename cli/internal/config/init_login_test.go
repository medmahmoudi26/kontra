package config

import (
	"bytes"
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
