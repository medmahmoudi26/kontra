package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// A CONFIG SOMEBODY ELSE CAN READ IS REFUSED, NOT USED — and the tokens are generated at install.
//
// The file is created 0600 and the directory 0700, and until now nothing looked at either again. A
// mode is not a property of a file's contents: `cp -r` without `-p`, an editor rewriting through a
// temp file under a loose umask, a restore from backup, a bind-mount, an extracted tarball. Every
// one of those lands it at 0644, holding the DigitalOcean token, the Pulumi passphrase, the fleet
// SSH key and four service tokens.
//
// It matters exactly when kontra is used the way it is meant to be: two engineers on one instance,
// each with their own account. `ssh` refuses a key in this state; there was no reason for a file
// holding strictly more to be quieter.

func writeConfig(t *testing.T, mode os.FileMode) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("controller: \"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAConfigOthersCanReadIsRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("modes do not mean the same thing here")
	}
	for _, tc := range []struct {
		mode os.FileMode
		who  string
	}{
		{0o640, "your group"},
		{0o604, "every other user on this machine"},
		{0o644, "your group and every other user on this machine"},
		{0o666, "your group and every other user on this machine"},
	} {
		t.Run(tc.mode.String(), func(t *testing.T) {
			err := refuseIfReadableByOthers(writeConfig(t, tc.mode))
			if err == nil {
				t.Fatalf("mode %04o was accepted — this is somebody else reading the fleet key", tc.mode)
			}
			// The message has to carry the fix. An operator meeting this is mid-command and the
			// difference between "permission problem" and `chmod 600 <path>` is the whole value.
			for _, want := range []string{tc.who, "chmod 600", "live credentials"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not say %q: %v", want, err)
				}
			}
		})
	}
}

func TestTheOwnersOwnBitsAreTheirBusiness(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("modes do not mean the same thing here")
	}
	// 0400 is a legitimate thing to want and 0600 is what install writes. Refusing either would be
	// a guard that fires on the correct state, which is how a guard gets deleted.
	for _, mode := range []os.FileMode{0o600, 0o400, 0o200} {
		if err := refuseIfReadableByOthers(writeConfig(t, mode)); err != nil {
			t.Errorf("mode %04o should be accepted: %v", mode, err)
		}
	}
}

func TestAMissingConfigIsNotAnError(t *testing.T) {
	// LoadConfig's contract: an installation configured entirely by environment is legitimate, and
	// every container is one. A guard that turned "no file" into a failure would break all of them.
	if err := refuseIfReadableByOthers(filepath.Join(t.TempDir(), "nope.yaml")); err != nil {
		t.Errorf("a missing config must fall through to the read: %v", err)
	}
}

func TestLoadConfigActuallyConsultsTheCheck(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("modes do not mean the same thing here")
	}
	// The check being right buys nothing if LoadConfig does not call it. Driven through KONTRA_HOME
	// so this exercises the real resolution rather than the helper in isolation.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("controller: \"x\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KONTRA_HOME", dir)
	if _, err := LoadConfig(); err == nil {
		t.Fatal("LoadConfig read a world-readable config — the check is not wired in")
	}
	if err := os.Chmod(filepath.Join(dir, "config.yaml"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("0600 should load: %v", err)
	}
	if cfg.Controller != "x" {
		t.Errorf("the config did not actually load: %+v", cfg)
	}
}

func TestInstallMintsTheTokensThatShouldBeMinted(t *testing.T) {
	fresh := FreshConfig()

	// THE THREE THAT ARE GENERATED. `panel` blank is a Dashboard that 503s on a new install;
	// `run` blank is OPEN — serve, start, stop, tag and rename admit anyone who reaches the API.
	for _, key := range []string{"state", "panel", "run"} {
		if strings.Contains(fresh, "  "+key+`: ""`) {
			t.Errorf("%s was not minted — a fresh install ships it blank", key)
		}
	}

	// `explore` STAYS BLANK, and it is not an oversight: it falls back to the state token, and the
	// SPA bakes VITE_KONTRA_EXPLORE_TOKEN at IMAGE BUILD time. Minting one here hands a prebuilt
	// bundle a token the server no longer expects and the Datasets console 401s into an empty grid.
	if !strings.Contains(fresh, `  explore: ""`) {
		t.Error("explore was minted — that 401s a prebuilt console into an empty grid")
	}

	// Distinct values. One token reused three times would pass every assertion above while making
	// the panel credential able to spend money, which is the separation `tickets.ts` exists for.
	seen := map[string]bool{}
	for _, line := range strings.Split(fresh, "\n") {
		for _, key := range []string{"state", "panel", "run"} {
			prefix := "  " + key + `: "`
			if strings.HasPrefix(line, prefix) {
				tok := strings.TrimSuffix(strings.TrimPrefix(line, prefix), `"`)
				if len(tok) < 32 {
					t.Errorf("%s is only %d chars — mintToken should be 256 bits", key, len(tok))
				}
				if seen[tok] {
					t.Errorf("%s reuses another token's value", key)
				}
				seen[tok] = true
			}
		}
	}
	if len(seen) != 3 {
		t.Fatalf("found %d minted tokens, expected 3 — the template keys moved", len(seen))
	}
}
