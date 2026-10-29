package ci

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

type workflow struct {
	On   map[string]any `yaml:"on"`
	Jobs map[string]struct {
		Env   map[string]any `yaml:"env"`
		Steps []struct {
			Uses string         `yaml:"uses"`
			With map[string]any `yaml:"with"`
			Run  string         `yaml:"run"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

func workflows(t *testing.T) map[string]workflow {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "..", ".github", "workflows", "*.yml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no workflows: %v", err)
	}
	out := map[string]workflow{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var w workflow
		if err := yaml.Unmarshal(b, &w); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		out[filepath.Base(f)] = w
	}
	return out
}

var pinned = regexp.MustCompile(`^[\w.-]+/[\w./-]+@[0-9a-f]{40}$`)

// Every action is pinned by commit (a tag can be moved), uv by version, and a job that installs a
// package refreshes the index first and names the package's version.
func TestToolsArePinned(t *testing.T) {
	for name, w := range workflows(t) {
		for job, j := range w.Jobs {
			for _, s := range j.Steps {
				if s.Uses != "" && !pinned.MatchString(s.Uses) {
					t.Errorf("%s %s: %q is not pinned by commit", name, job, s.Uses)
				}
				if strings.HasPrefix(s.Uses, "astral-sh/setup-uv@") && (s.With["version"] == nil || s.With["python-version"] == nil) {
					t.Errorf("%s %s: setup-uv without a pinned version and python-version", name, job)
				}
				if strings.Contains(s.Run, "apt-get install") {
					if !strings.Contains(s.Run, "apt-get update") {
						t.Errorf("%s %s: apt-get install without apt-get update: %q", name, job, s.Run)
					}
					if strings.Contains(s.Run, "postgresql-client") && !regexp.MustCompile(`postgresql-client-\d+`).MatchString(s.Run) {
						t.Errorf("%s %s: postgresql-client without its major version: %q", name, job, s.Run)
					}
				}
			}
		}
	}
}

// The end-to-end suite runs on pull requests (a schedule or workflow_dispatch trigger needs the file on
// the default branch, so it could not run before a merge), and a job runs the integration tests under the
// race detector.
func TestE2EAndRaceRunBeforeAMerge(t *testing.T) {
	ws := workflows(t)
	e2e, ok := ws["e2e.yml"]
	if !ok {
		t.Fatal("no e2e workflow")
	}
	if _, ok := e2e.On["pull_request"]; !ok {
		t.Errorf("e2e.yml runs on %v, not on pull requests", keys(e2e.On))
	}
	race := false
	for _, w := range ws {
		if _, ok := w.On["pull_request"]; !ok {
			continue
		}
		for _, j := range w.Jobs {
			for _, s := range j.Steps {
				if strings.Contains(s.Run, "make test-race") {
					race = true
				}
			}
		}
	}
	if !race {
		t.Error("no pull request job runs make test-race")
	}
	b, err := os.ReadFile(filepath.Join("..", "..", "Makefile"))
	if err != nil || !regexp.MustCompile(`(?m)^test-race:\n\t.*test_integration\.sh -race`).Match(b) {
		t.Errorf("the Makefile has no test-race target running the integration tests with -race (%v)", err)
	}
}

func keys(m map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// The helloworld seller's tests run in CI and cannot pass by skipping (M2 review, robustness-2): the e2e job
// (TestHelloworldTunnel: hello-world unmodified behind the tunnel, a hard rule of M2) and the integration job
// (run.sh's guards, examples/sellers/helloworld) install uv, fetch a2a-samples at the commit run.sh pins
// into KONTRA_A2A_SAMPLES before the tests, and set KONTRA_REQUIRE_HELLOWORLD, which turns the tests' skips
// (no uv, no checkout, 127.0.0.1:9999 taken) into failures.
func TestHelloworldRunsInCI(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "examples", "sellers", "helloworld", "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^pin=([0-9a-f]{40})$`).FindSubmatch(b)
	if m == nil {
		t.Fatal("run.sh pins no a2a-samples commit")
	}
	pin := string(m[1])
	ws := workflows(t)
	for _, c := range []struct{ file, job, tests string }{
		{"e2e.yml", "e2e", "make test-e2e"},
		{"check.yml", "integration", "make test-integration"},
	} {
		j, ok := ws[c.file].Jobs[c.job]
		if !ok {
			t.Errorf("%s has no job %s", c.file, c.job)
			continue
		}
		if fmt.Sprint(j.Env["KONTRA_REQUIRE_HELLOWORLD"]) != "1" {
			t.Errorf("%s %s: KONTRA_REQUIRE_HELLOWORLD is %v, want 1", c.file, c.job, j.Env["KONTRA_REQUIRE_HELLOWORLD"])
		}
		uv, fetch, tests := -1, -1, -1
		for i, s := range j.Steps {
			switch {
			case strings.HasPrefix(s.Uses, "astral-sh/setup-uv@"):
				uv = i
			case strings.Contains(s.Run, "https://github.com/a2aproject/a2a-samples") && strings.Contains(s.Run, "KONTRA_A2A_SAMPLES=") &&
				strings.Contains(s.Run, "$GITHUB_ENV") && strings.Contains(s.Run, "rev-parse HEAD"):
				fetch = i
			case strings.Contains(s.Run, c.tests):
				tests = i
			}
		}
		if p := fmt.Sprint(j.Env["A2A_SAMPLES_PIN"]); fetch >= 0 && p != pin {
			t.Errorf("%s %s: A2A_SAMPLES_PIN %s, want run.sh's %s", c.file, c.job, p, pin)
		}
		if uv < 0 || fetch < 0 || tests < 0 || uv > tests || fetch > tests {
			t.Errorf("%s %s: setup-uv at step %d, the a2a-samples fetch at %d, %s at %d: want both before the tests",
				c.file, c.job, uv, fetch, c.tests, tests)
		}
	}
}

// Finding robustness-6: CI never ran the M3 tests against server-everything@2.0.0 (the e2e job installed Node 22 only,
// and check.yml no Node at all), so TestMCPModernEverything2 and TestModernServerEverything always skipped. Every job
// that runs them installs a pinned Node 24 first, records it as KONTRA_NODE24, then the pinned Node 22 for the 1.0.0
// servers; KONTRA_REQUIRE_NODE24 makes a missing Node 24 fail them, and no job sets KONTRA_E2E_OFFLINE, so an npm
// install that fails fails its test.
func TestNode24InCI(t *testing.T) {
	ws := workflows(t)
	exact := regexp.MustCompile(`^\d+\.\d+\.\d+$`)
	for _, c := range []struct{ file, job string }{{"e2e.yml", "e2e"}, {"check.yml", "integration"}, {"check.yml", "race"}} {
		j, ok := ws[c.file].Jobs[c.job]
		if !ok {
			t.Fatalf("%s has no job %s", c.file, c.job)
		}
		if fmt.Sprint(j.Env["KONTRA_REQUIRE_NODE24"]) != "1" {
			t.Errorf("%s %s: KONTRA_REQUIRE_NODE24 is %v, want 1", c.file, c.job, j.Env["KONTRA_REQUIRE_NODE24"])
		}
		if _, ok := j.Env["KONTRA_E2E_OFFLINE"]; ok {
			t.Errorf("%s %s: KONTRA_E2E_OFFLINE set: npm failures would skip", c.file, c.job)
		}
		var versions []string
		recorded := -1
		for i, s := range j.Steps {
			if strings.HasPrefix(s.Uses, "actions/setup-node@") {
				v := fmt.Sprint(s.With["node-version"])
				if !exact.MatchString(v) {
					t.Errorf("%s %s: setup-node %q is not an exact version", c.file, c.job, v)
				}
				versions = append(versions, v)
			}
			if strings.Contains(s.Run, "KONTRA_NODE24=") && strings.Contains(s.Run, "GITHUB_ENV") {
				recorded = i
			}
		}
		if len(versions) != 2 || !strings.HasPrefix(versions[0], "24.") || versions[1] != "22.22.2" || recorded < 0 {
			t.Errorf("%s %s: setup-node versions %v, KONTRA_NODE24 recorded at step %d: want Node 24 recorded, then Node 22.22.2",
				c.file, c.job, versions, recorded)
		}
	}
}
