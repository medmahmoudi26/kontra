package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The CLI's half of shared/conformance/workspace_namespace.json. The orchestrator drives the same
// file, so a workflow is served and started in the namespace the control plane reads.
func TestNamespaceForWorkspaceConformance(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "shared", "conformance", "workspace_namespace.json"))
	if err != nil {
		t.Fatal(err)
	}
	var corpus struct {
		Cases []struct {
			Workspace    string  `json:"workspace"`
			EnvNamespace *string `json:"env_namespace"`
			Namespace    string  `json:"namespace"`
		} `json:"cases"`
		Refused []struct {
			Workspace string `json:"workspace"`
		} `json:"refused"`
	}
	if err := json.Unmarshal(raw, &corpus); err != nil {
		t.Fatal(err)
	}
	if len(corpus.Cases) == 0 || len(corpus.Refused) == 0 {
		t.Fatal("the corpus is empty; this test would pass checking nothing")
	}
	for _, c := range corpus.Cases {
		if c.EnvNamespace == nil {
			t.Setenv("KONTRA_NAMESPACE", "")
		} else {
			t.Setenv("KONTRA_NAMESPACE", *c.EnvNamespace)
		}
		got, err := NamespaceForWorkspace(c.Workspace)
		if err != nil || got != c.Namespace {
			t.Errorf("workspace %q, KONTRA_NAMESPACE %v: got %q (%v), want %q", c.Workspace, c.EnvNamespace, got, err, c.Namespace)
		}
	}
	for _, r := range corpus.Refused {
		if ns, err := NamespaceForWorkspace(r.Workspace); err == nil {
			t.Errorf("workspace %q should be refused, got namespace %q", r.Workspace, ns)
		}
	}
}

func TestTheCLIActsInTheCurrentWorkspacesNamespace(t *testing.T) {
	parent := t.TempDir()
	for _, n := range []string{"default", "hello"} {
		if err := os.Mkdir(filepath.Join(parent, n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("KONTRA_WORKSPACES", parent)
	t.Setenv("KONTRA_NAMESPACE", "default")

	write := func(name string) {
		if err := os.WriteFile(filepath.Join(parent, ".current"), []byte(name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("hello")
	if got := TemporalNamespace(); got != "ws-hello" {
		t.Errorf("current hello: got %q", got)
	}
	write("default")
	if got := TemporalNamespace(); got != "default" {
		t.Errorf("current default: got %q", got)
	}
	// SERVING IS BY FOLDER, not by whatever is current: a worker for hello's workflow polls ws-hello
	// even while the console is looking at default.
	if got := NamespaceForPath(filepath.Join(parent, "hello", "workflows", "canary")); got != "ws-hello" {
		t.Errorf("hello folder: got %q", got)
	}
	if got := NamespaceForPath("/somewhere/else"); got != "default" {
		t.Errorf("outside the workspaces: got %q", got)
	}
	// A Machine or Warden has no workspaces layout and keeps the namespace its Fleet gave it.
	t.Setenv("KONTRA_WORKSPACES", "")
	t.Setenv("KONTRA_NAMESPACE", "ws-hello")
	if got := TemporalNamespace(); got != "ws-hello" {
		t.Errorf("no layout: got %q", got)
	}
}
