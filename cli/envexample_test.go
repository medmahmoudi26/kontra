package main

// envexample_test.go — `.env.example` is the documentation of what an installation needs, so it has
// to stay true.
//
// THE FAILURE IT PREVENTS IS SILENCE. `.env` is gitignored, so a variable added to
// `docker-compose.yml` and not to `.env.example` reaches a fresh install as an empty substitution.
// Compose does not warn. The service starts, binds nothing or mounts the wrong path, and fails
// somewhere far from the cause — which is the exact shape this repo already recorded: "each one
// fails without naming itself".
//
// THE SECOND GUARD IS THE OPPOSITE ONE: this file must never ship a working credential. A
// checked-in default password is the first thing tried against any install, and Windmill's own
// threat model lists its compose defaults (`changeme`, exposed Postgres) as an entry point. The
// smoothness is worth copying; the defaults are not.

import (
	"regexp"
	"strings"
	"testing"
)

var composeVar = regexp.MustCompile(`\$\{([A-Z_][A-Z0-9_]*)`)

// envExampleNames is the set of `NAME=` assignments in .env.example.
func envExampleNames(t *testing.T) map[string]string {
	t.Helper()
	body := repoFile(t, ".env.example")
	out := map[string]string{}
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		name, value, ok := strings.Cut(trimmed, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(name)] = strings.TrimSpace(value)
	}
	if len(out) == 0 {
		t.Fatal(".env.example declares no variables at all; every assertion below would be vacuous")
	}
	return out
}

// Every variable docker-compose.quickstart.yml substitutes must be documented in
// .env.quickstart, because those are the two files a first-time install copies.
func TestEveryComposeVariableIsInEnvExample(t *testing.T) {
	compose := repoFile(t, "docker-compose.quickstart.yml")
	found := map[string]bool{}
	for _, line := range strings.Split(compose, "\n") {
		// COMMENTS ARE NOT SUBSTITUTIONS, and scanning the whole file as one string made every
		// `${VAR}` an author MENTIONED indistinguishable from one compose expands. It reported
		// KONTRA_HOME — which this file sets as a literal and only discusses in a note about
		// `secrets/keyring.ts` defaulting to `${KONTRA_HOME}/secrets` — so the failure named a
		// variable that needs no entry, beside two that genuinely did. A guard whose output
		// contains a name nobody should act on is a guard people learn to skim.
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		for _, m := range composeVar.FindAllStringSubmatch(line, -1) {
			if m[1] == "PWD" {
				continue // compose's own default, not an install setting
			}
			found[m[1]] = true
		}
	}
	if len(found) == 0 {
		t.Fatal("no ${VAR} substitutions found in docker-compose.quickstart.yml — this guard is not looking at the right file")
	}

	documented := envFileNames(t, ".env.quickstart")
	var missing []string
	for name := range found {
		if _, ok := documented[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Errorf("docker-compose.quickstart.yml substitutes %v, and .env.quickstart does not mention them.\n"+
			"A variable missing here reaches a fresh install as an empty substitution, silently.", missing)
	}
}

func envFileNames(t *testing.T, rel string) map[string]string {
	t.Helper()
	body := repoFile(t, rel)
	out := map[string]string{}
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		name, value, ok := strings.Cut(trimmed, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(name)] = strings.TrimSpace(value)
	}
	if len(out) == 0 {
		t.Fatalf("%s declares no variables at all; every assertion below would be vacuous", rel)
	}
	return out
}

// NO CREDENTIAL SHIPS. Anything whose name says secret must be assigned an empty value.
func TestEnvExampleShipsNoCredentials(t *testing.T) {
	secretish := regexp.MustCompile(`(?i)(TOKEN|PASSWORD|PASSPHRASE|SECRET|CREDENTIAL|_KEY_IDS|USERS)$`)
	// KONTRA_FLEET_SSH_KEY is a PATH, not a key, and a path is the useful default.
	allowedNonEmpty := map[string]bool{"KONTRA_FLEET_SSH_KEY": true}

	checked := 0
	for name, value := range envExampleNames(t) {
		if !secretish.MatchString(name) || allowedNonEmpty[name] {
			continue
		}
		checked++
		if value != "" {
			t.Errorf("%s ships a value (%q). A credential in a checked-in file is the first one "+
				"an attacker tries; generate it with `kontra init` instead.", name, value)
		}
	}
	if checked == 0 {
		t.Fatal("no credential-shaped variables were examined; the pattern no longer matches anything")
	}
}

// THE ASYMMETRY MUST BE STATED. A blank `run` token leaves a surface OPEN; a blank `state` or
// `panel` token leaves one DISABLED. They are opposites, and an operator who reads this file and
// blanks the wrong one either exposes a surface or breaks a page, with nothing to warn them.
func TestEnvExampleStatesWhichBlanksMeanOpen(t *testing.T) {
	body := repoFile(t, ".env.example")
	for _, want := range []string{"OPEN", "DISABLED"} {
		if !strings.Contains(body, want) {
			t.Errorf(".env.example never says %q; the two kinds of blank are indistinguishable to a reader", want)
		}
	}
	// And specifically on the one that is dangerous.
	runIdx := strings.Index(body, "KONTRA_RUN_TOKEN=")
	if runIdx < 0 {
		t.Fatal("KONTRA_RUN_TOKEN is not in .env.example")
	}
	// The CONTIGUOUS comment block immediately above the assignment — not just the last line.
	// A warning two lines up is still the warning an operator reads; requiring it on the final
	// line would pass or fail on where a sentence happened to wrap.
	block := commentBlockAbove(body, runIdx)
	if block == "" {
		t.Fatal("KONTRA_RUN_TOKEN has no comment above it at all")
	}
	if !strings.Contains(block, "OPEN") {
		t.Errorf("the comment block above KONTRA_RUN_TOKEN never says a blank leaves the Run "+
			"surface OPEN. It says:\n%s", block)
	}
}

// commentBlockAbove returns the run of `#` lines directly preceding the assignment at idx.
func commentBlockAbove(body string, idx int) string {
	lines := strings.Split(body[:idx], "\n")
	// The last element is the partial line holding the assignment itself.
	if len(lines) > 0 {
		lines = lines[:len(lines)-1]
	}
	var block []string
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if strings.HasPrefix(l, "#") {
			block = append([]string{l}, block...)
			continue
		}
		break // a blank line or another assignment ends the block
	}
	return strings.Join(block, "\n")
}
