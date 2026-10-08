package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// A PULL NEEDS A CREDENTIAL FOR THE SAME REASON A PUSH DOES, and this one was missed when the push
// was fixed: `base64("{}")` is the Engine API's canonical "no credentials" — correct against a
// registry with no users, a 401 against one with them. So an install that turned auth on could
// build an actor image, push it, and then fail to START it, because `kontra scale` is what pulls and
// the daemon it asks has no credential of its own.
func TestAPullCarriesTheInstallsReadCredential(t *testing.T) {
	t.Setenv("KONTRA_REGISTRY", "127.0.0.1:5000")
	t.Setenv(pullUserEnv, "pull")
	t.Setenv(pullPasswordEnv, "pullpw")

	f := &fakeScaleDocker{}
	if err := pullImage(context.Background(), f, "127.0.0.1:5000/probe:0.2.0"); err != nil {
		t.Fatal(err)
	}
	if len(f.pullAuth) != 1 {
		t.Fatalf("expected one pull, got %d", len(f.pullAuth))
	}
	raw, err := base64.URLEncoding.DecodeString(f.pullAuth[0])
	if err != nil {
		t.Fatalf("the header must be base64 of an AuthConfig: %v", err)
	}
	var cfg map[string]string
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["username"] != "pull" || cfg["password"] != "pullpw" {
		t.Errorf("the pull went out as %v, want the read account", redactPassword(cfg))
	}
	if cfg["serveraddress"] != "127.0.0.1:5000" {
		t.Errorf("serveraddress = %q, want the registry the ref names", cfg["serveraddress"])
	}
}

// AND NOT TO SOMEBODY ELSE'S REGISTRY. A Fleet image from a public registry is pulled as the daemon
// would pull it; sending this install's password there would be a credential leak.
func TestAPullFromAnotherRegistryIsAnonymous(t *testing.T) {
	t.Setenv("KONTRA_REGISTRY", "127.0.0.1:5000")
	t.Setenv(pullUserEnv, "pull")
	t.Setenv(pullPasswordEnv, "pullpw")

	f := &fakeScaleDocker{}
	if err := pullImage(context.Background(), f, "ghcr.io/someone/probe:0.2.0"); err != nil {
		t.Fatal(err)
	}
	raw, _ := base64.URLEncoding.DecodeString(f.pullAuth[0])
	if strings.Contains(string(raw), "pullpw") {
		t.Error("this install's password must not be sent to ghcr")
	}
}

func redactPassword(cfg map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range cfg {
		if k == "password" {
			v = "(set)"
		}
		out[k] = v
	}
	return out
}
