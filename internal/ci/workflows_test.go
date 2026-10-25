package ci

import (
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
