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

func TestValidateWorkspaceName(t *testing.T) {
	if err := validateWorkspaceName("hello"); err != nil {
		t.Fatal(err)
	}
	if err := validateWorkspaceName("../x"); err == nil {
		t.Fatal("path traversal must fail")
	}
	if err := validateWorkspaceName(".hidden"); err == nil {
		t.Fatal("dot-prefix must fail")
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
	if err := os.Mkdir(filepath.Join(parent, "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(parent, "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeCurrentName(parent, "a"); err != nil {
		t.Fatal(err)
	}
	if err := cmdWorkspaceUse([]string{"b"}); err != nil {
		t.Fatal(err)
	}
	got, _ := readCurrentName(parent)
	if got != "b" {
		t.Fatalf("got %q", got)
	}
}
