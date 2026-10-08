// deploy_test.go — the pure bits of `kontra deploy`'s worker-image path: the generated
// Dockerfile, the controller-host resolution, the registry-reachability guard, and the
// deploy summary. The build/push itself is validated live against the Docker engine.
//
// The three tests at the foot of this file arrived from workflow_test.go when workflow.go was
// split into its four seams. They were never about the caller's side: they cover the generated
// worker image, the actor manifest's kind, and the deploy summary — this file's subject.
package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/image"
)

func TestVersionDeployed(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/beacon/tags/list", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"name":"beacon","tags":["0.1.0","0.2.0"]}`))
	})
	mux.HandleFunc("/v2/fresh/tags/list", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound) // repo unknown → not deployed
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	reg := strings.TrimPrefix(srv.URL, "http://")

	if !versionDeployed(reg, "beacon", "0.2.0") {
		t.Error("0.2.0 is in the registry → should report deployed")
	}
	if versionDeployed(reg, "beacon", "0.9.0") {
		t.Error("0.9.0 is not a tag → should report not deployed")
	}
	if versionDeployed(reg, "fresh", "0.1.0") {
		t.Error("unknown repo (404) → should report not deployed")
	}
}

func TestControllerHost(t *testing.T) {
	// --controller flag always wins.
	if got := controllerHost("box.example"); got != "box.example" {
		t.Errorf("flag should win: got %q", got)
	}
	// localhost orchestrator → placeholder (localhost is useless from a droplet).
	t.Setenv("KONTRA_ORCHESTRATOR_URL", "http://localhost:8088")
	if got := controllerHost(""); got != "<controller>" {
		t.Errorf("localhost should become placeholder, got %q", got)
	}
	// a real host is used as-is.
	t.Setenv("KONTRA_ORCHESTRATOR_URL", "http://10.0.0.7:8088")
	if got := controllerHost(""); got != "10.0.0.7" {
		t.Errorf("remote host should pass through, got %q", got)
	}
}

func TestRegistryProbeBases(t *testing.T) {
	got := registryProbeBases("127.0.0.1:5000")
	want := []string{
		"http://127.0.0.1:5000",
		"http://host.docker.internal:5000",
		"http://registry:5000",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("loopback registry probes: got %v want %v", got, want)
	}
	got = registryProbeBases("ghcr.io")
	if len(got) != 1 || got[0] != "http://ghcr.io" {
		t.Errorf("remote registry should not grow compose aliases: %v", got)
	}
}

func TestRegistryUnreachable(t *testing.T) {
	// A port nothing listens on → unreachable → clear error; the default registry adds
	// a "start one" hint.
	err := registryReachable("127.0.0.1:1")
	if err == nil {
		t.Fatal("expected an error for an unreachable registry")
	}
	err = registryReachable(defaultRegistry + "x") // not the default → no hint, but must still be present-ish
	if err == nil {
		t.Fatal("expected an error for an unreachable registry")
	}
}

func TestDeploySummary(t *testing.T) {
	out := withStdout(t, func() {
		printDeploySummary(actorManifest{Name: "beacon", Version: "0.2.0"}, "localhost:5000/beacon:0.2.0", "sha256:"+strings.Repeat("b", 64), "<controller>")
	})
	for _, want := range []string{
		"actor deployed: beacon@0.2.0",
		"image:  localhost:5000/beacon:0.2.0",
		"pull:   docker pull localhost:5000/beacon:0.2.0",
		"docker run -d --name kontra-beacon",
		"KONTRA_ADDRESS=<controller>:7233",
		"KONTRA_REDIS_HOST=<controller>:6379",
		"kontra workers list",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("deploy summary missing %q in:\n%s", want, out)
		}
	}
}

// --- arrived from workflow_test.go when workflow.go was split into its four seams ---------------

func TestManifestKind(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) {
		if err := os.WriteFile(filepath.Join(dir, "actor.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write(`{"name":"a","version":"1"}`)
	m, err := readManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if m.kindOf() != kindActor || m.entryFile() != "actor.py" {
		t.Errorf("no kind must mean actor, got kind=%q entry=%q", m.kindOf(), m.entryFile())
	}

	write(`{"name":"a","version":"1","entry":"probes.py"}`)
	m, _ = readManifest(dir)
	if m.entryFile() != "probes.py" {
		t.Errorf("an explicit entry wins, got %q", m.entryFile())
	}

	// The retired kind is REFUSED, and the refusal has to name it. A manifest that still says
	// "activity" was written against a model where a bundle got a handler-less image and its own
	// queue; deploying it as an actor would build something its author never asked for, and
	// "unknown kind" would send them looking for a typo (ADR 0023 §9).
	write(`{"name":"a","version":"1","kind":"activity"}`)
	_, err = readManifest(dir)
	if err == nil {
		t.Fatal("kind=activity must be refused: it retired with ADR 0023 §9")
	}
	if !strings.Contains(err.Error(), "§9") || !strings.Contains(err.Error(), "activity") {
		t.Errorf("the refusal must name the retired kind and the decision, got: %v", err)
	}

	// A typo must not deploy as an actor by accident.
	write(`{"name":"a","version":"1","kind":"activities"}`)
	if _, err := readManifest(dir); err == nil {
		t.Error("an unknown kind must be refused, not normalized")
	}
}

// TestDeploySummaryIsOneShape pins what a deploy tells the operator now that there is one kind
// (ADR 0023 §9). The summary used to fork: a bundle got its own block, without Redis or the
// orchestrator, because it held no session and published no catalog entry. Every deployed thing
// is a session-holding, self-registering Actor now, so every deploy needs the full wiring —
// and an operator who is handed a half-wired run command meets it as a worker that boots,
// looks healthy, and reaches a Redis that is not there.
func TestDeploySummaryIsOneShape(t *testing.T) {
	out := withStdout(t, func() {
		printDeploySummary(
			actorManifest{Name: "probe", Version: "0.1.0"},
			"localhost:5000/probe:0.1.0", "sha256:"+strings.Repeat("a", 64), "<controller>")
	})
	for _, want := range []string{
		"actor deployed: probe@0.1.0",
		"KONTRA_REDIS_HOST",
		"KONTRA_ORCHESTRATOR_URL",
		"kontra workers list",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("deploy summary missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "bundle") || strings.Contains(out, "activities") {
		t.Errorf("the bundle vocabulary retired with the kind:\n%s", out)
	}
}

// fakeBaseDocker records whether ensureBase decided to build, and with which labels.
type fakeBaseDocker struct {
	labels map[string]string // what ImageList reports on the existing base ("" summary = absent)
	absent bool
	built  int
	stamp  map[string]string // labels the build was asked to stamp
}

func (f *fakeBaseDocker) ImageList(context.Context, image.ListOptions) ([]image.Summary, error) {
	if f.absent {
		return nil, nil
	}
	return []image.Summary{{Labels: f.labels}}, nil
}
func (f *fakeBaseDocker) ImageBuild(_ context.Context, _ io.Reader, o types.ImageBuildOptions) (types.ImageBuildResponse, error) {
	f.built++
	f.stamp = o.Labels
	return types.ImageBuildResponse{Body: io.NopCloser(strings.NewReader(`{"stream":"ok\n"}`))}, nil
}
func (f *fakeBaseDocker) ImageTag(context.Context, string, string) error { return nil }
func (f *fakeBaseDocker) ImagePush(context.Context, string, image.PushOptions) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}
