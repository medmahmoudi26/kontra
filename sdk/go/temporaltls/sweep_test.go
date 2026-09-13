// EVERY Go Temporal client in this repository goes through this package — the half of the change
// that is not a change, and the half that keeps being true.
//
// Six call sites across three modules is the whole difficulty. A version that reaches four of them
// does not fail loudly: it produces a deployment that mostly works and has one binary talking
// plaintext to a server that accepts both, which is the worst of the three outcomes because it
// looks like the good one. So this walks the tree rather than trusting a list.
package temporaltls

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

func repoRoot() string {
	_, self, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(self), "..", "..", "..")
}

// `client.Dial(` however the package was named at the import. Not `Dial(` alone: the appliance's
// embedded Temporal server has its own Dial and is not a client of anything.
var dialSite = regexp.MustCompile(`\bclient\.Dial\(`)

func goSources(t *testing.T) map[string]string {
	t.Helper()
	skip := map[string]bool{".git": true, "node_modules": true, "dist": true, "_gen": true, ".scratch": true}
	out := map[string]string{}
	root := repoRoot()
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // an unreadable directory is not this guard's business
		}
		if info.IsDir() {
			if skip[info.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		out[rel] = string(raw)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}

func TestTheWalkFindsTheTreeItIsWalking(t *testing.T) {
	// NON-VACUOUS. A walk that returns nothing passes every assertion below, which is how a guard
	// reports success for a tree it never read — this repository's most repeated failure shape.
	sources := goSources(t)
	if len(sources) < 100 {
		t.Fatalf("only %d Go sources under %s — the walk is broken", len(sources), repoRoot())
	}
	for _, want := range []string{
		filepath.Join("cli", "workers.go"),
		filepath.Join("runtime", "handler", "main.go"),
		filepath.Join("runtime", "go", "temporalhost", "host.go"),
	} {
		if _, ok := sources[want]; !ok {
			t.Errorf("the walk did not reach %s, so it cannot be judging it", want)
		}
	}
}

func TestEveryTemporalClientGoesThroughThisPackage(t *testing.T) {
	var sites, bare []string
	for rel, body := range goSources(t) {
		for _, line := range strings.Split(body, "\n") {
			if !dialSite.MatchString(line) || strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			sites = append(sites, rel+": "+strings.TrimSpace(line))
			// The options are built a few lines above the Dial, so the FILE is the unit here: a
			// site that dials without this package's name anywhere in the file is a bypass.
			if !strings.Contains(body, "temporaltls.ConnectionOptions(") {
				bare = append(bare, rel+": "+strings.TrimSpace(line))
			}
		}
	}
	// The count first: a regex that stopped matching would otherwise report a clean sweep over
	// nothing, which is the same failure as an empty walk wearing a different hat.
	if len(sites) < 6 {
		t.Fatalf("found %d client.Dial sites, expected at least 6 — the pattern stopped matching", len(sites))
	}
	if len(bare) > 0 {
		t.Errorf("a Temporal client that bypasses temporaltls.ConnectionOptions:\n  %s",
			strings.Join(bare, "\n  "))
	}
}

// A READ, not a mention. `Getenv("X")`, `LookupEnv("X")` or a helper like `EnvOr("X", …)`.
//
// IT WAS `strings.Contains` AND THAT WAS WRONG, which this guard proved on itself: the
// `.kontra/config.yaml` template documents these five variables so the file can answer "what does
// this installation need", and a guard forbidding the DOCUMENTATION of an environment variable is a
// guard that makes the codebase worse to satisfy. What must not exist is a second READING — a call
// site that resolves the value itself and becomes a second policy agreeing today and drifting later.
var envRead = regexp.MustCompile(`(?:Getenv|LookupEnv|EnvOr)\(\s*"(KONTRA_TEMPORAL_TLS[A-Z_]*)"`)

func TestTheReadPatternCanSeeARead(t *testing.T) {
	// The guard on the guard. A regex that matched nothing would make the sweep below pass over a
	// codebase full of second readings — success reported by looking at nothing, again.
	for _, spelling := range []string{
		`os.Getenv("KONTRA_TEMPORAL_TLS")`,
		`os.LookupEnv("KONTRA_TEMPORAL_TLS_CA")`,
		`cliutil.EnvOr("KONTRA_TEMPORAL_TLS_CERT", "")`,
	} {
		if !envRead.MatchString(spelling) {
			t.Errorf("envRead does not match %q", spelling)
		}
	}
	// …and does NOT match the template's prose, which is the case that made it exist.
	if envRead.MatchString(`#   KONTRA_TEMPORAL_TLS   1|true|yes|on — TLS with the system trust store`) {
		t.Error("envRead matches documentation, which is what it was narrowed to stop")
	}
}

func TestNothingElseReadsTheTlsEnvironment(t *testing.T) {
	// One function, one reading. A call site that RESOLVED KONTRA_TEMPORAL_TLS_* itself would be a
	// second policy that agrees today and drifts later. Documenting the names is fine and wanted.
	var offenders []string
	for rel, body := range goSources(t) {
		if strings.HasPrefix(rel, filepath.Join("runtime", "handler", "temporaltls")) {
			continue
		}
		for _, m := range envRead.FindAllStringSubmatch(body, -1) {
			offenders = append(offenders, rel+" reads "+m[1])
		}
	}
	if len(offenders) > 0 {
		t.Errorf("the TLS environment is read outside temporaltls:\n  %s", strings.Join(offenders, "\n  "))
	}
}
