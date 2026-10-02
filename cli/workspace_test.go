package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestWatchDeployAlreadyPresentIsSettled(t *testing.T) {
	err := errors.New("exit status 1")
	out := "error: hello:0.1.0 is already deployed to 127.0.0.1:5000 — bump the version"
	if !watchDeploySettled(out, err) {
		t.Fatal("already-deployed must settle the watch turn so it does not retry every 3s")
	}
	if watchDeploySettled("docker: Cannot connect to the Docker daemon", err) {
		t.Fatal("a transient deploy failure must retry")
	}
	if !watchDeploySettled("", nil) {
		t.Fatal("a successful deploy is settled")
	}
}

// TestValidateWorkspaceName pins this rule to the one the CONTROL PLANE enforces.
//
// `control/orchestrator/src/workspaces.ts` derives an S3 bucket, a DuckLake catalog and a Temporal
// namespace from a workspace name, and its `NAME_RE` is `^[a-z0-9][a-z0-9-]{0,58}[a-z0-9]$`. This
// validator used to be strictly looser, which meant `kontra workspace use demo_workspace`
// SUCCEEDED and the first Dataset write of the first Run then died inside an Actor activity.
//
// The underscore case is the one that costs an isolation guarantee rather than only a confusing
// error: `catalogDbName` maps hyphens to underscores for Postgres, and that is injective only
// while underscores are illegal — otherwise `a-b` and `a_b` share one catalog database.
func TestValidateWorkspaceName(t *testing.T) {
	for _, ok := range []string{"hello", "demo", "bug-bounty", "w2", "a1"} {
		if err := validateWorkspaceName(ok); err != nil {
			t.Fatalf("%q must be accepted: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"../x",           // path traversal
		".hidden",        // dot-prefix
		"demo_workspace", // underscore — collides with the hyphen mapping in catalogDbName
		"Demo",           // uppercase — not a legal S3 bucket name
		"demo.workspace", // dot — not legal in a Postgres identifier unquoted
		"a",              // one character — cannot address a bucket
		"-demo",          // must start alphanumeric
		"demo-",          // must end alphanumeric
	} {
		if err := validateWorkspaceName(bad); err == nil {
			t.Fatalf("%q must be refused — the control plane refuses it", bad)
		}
	}
}

func TestSeedCreatesHelloChildUnderParent(t *testing.T) {
	parent := t.TempDir()
	seed := t.TempDir()
	if err := os.MkdirAll(filepath.Join(seed, "actors", "hello"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, "actors", "hello", "actor.json"), []byte(`{"name":"hello"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KONTRA_WORKSPACES", parent)
	t.Setenv("KONTRA_SEED_DIR", seed)
	t.Setenv("KONTRA_WORKSPACE", "")
	if err := cmdWorkspaceSeed(nil); err != nil {
		t.Fatal(err)
	}
	got, err := readCurrentName(parent)
	if err != nil || got != "hello" {
		t.Fatalf("current=%q err=%v", got, err)
	}
	if _, err := os.Stat(filepath.Join(parent, "hello", "actors", "hello", "actor.json")); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceUseAndList(t *testing.T) {
	parent := t.TempDir()
	t.Setenv("KONTRA_WORKSPACES", parent)
	// TWO CHARACTERS MINIMUM. These were "a" and "b", which the CONTROL PLANE has never accepted
	// — its `NAME_RE` is `^[a-z0-9][a-z0-9-]{0,58}[a-z0-9]$`, so a one-character name cannot
	// address a bucket or a catalog. The names here are arbitrary; they just have to be names a
	// real install could use.
	if err := os.Mkdir(filepath.Join(parent, "alpha"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(parent, "beta"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeCurrentName(parent, "alpha"); err != nil {
		t.Fatal(err)
	}
	if err := cmdWorkspaceUse([]string{"beta"}); err != nil {
		t.Fatal(err)
	}
	got, _ := readCurrentName(parent)
	if got != "beta" {
		t.Fatalf("got %q", got)
	}
}
