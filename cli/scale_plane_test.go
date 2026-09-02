package main

import (
	"strings"
	"testing"

	"github.com/medmahmoudi26/kontra/cli/appliance"
)

// WHICH CONTROL PLANE A WORKER CONTAINER DIALS — the resolution that replaced four hard-coded
// compose DNS names.
//
// The failure being pinned is not "the wrong address" in the abstract. `temporal:7233`,
// `seaweed:8333` and `redis:6379` stopped resolving when ADR 0031 moved those services into the
// binary, and a worker handed them starts, retries a name that does not exist, restarts under
// `unless-stopped`, and appears in `docker ps` and in `kontra workers list` as a running replica.
// Nothing anywhere reports a difference. docker-compose.yml's own header names this function as
// the caller that had not been fixed yet.
func TestResolveWorkerPlanePrefersARunningAppliance(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KONTRA_DATA_DIR", dir)
	if err := appliance.WriteEndpoints(dir, appliance.Endpoints{
		Bind:     "172.17.0.1",
		Temporal: "172.17.0.1:39101",
		S3:       "http://172.17.0.1:39102",
		KV:       "172.17.0.1:39103",
		Registry: "172.17.0.1:39105",
		API:      "http://172.17.0.1:39106",
	}); err != nil {
		t.Fatal(err)
	}

	p, err := resolveWorkerPlane(scaleOpts{name: "echo", version: "0.1.0"})
	if err != nil {
		t.Fatalf("resolveWorkerPlane: %v", err)
	}
	for _, tc := range []struct{ what, got, want string }{
		{"temporal", p.address, "172.17.0.1:39101"},
		{"object store", p.s3, "http://172.17.0.1:39102"},
		{"state store", p.redis, "172.17.0.1:39103"},
		{"orchestrator", p.orchestrator, "http://172.17.0.1:39106"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want the appliance's own %q", tc.what, tc.got, tc.want)
		}
	}
	// `bridge`, not `kontra`: the named network is compose's, and on a machine that has only ever
	// run the binary it does not exist — ContainerCreate would fail with `network kontra not found`.
	if p.network != "bridge" {
		t.Errorf("network = %q, want bridge", p.network)
	}
	if !p.appliance {
		t.Error("the plane must record that it came from an appliance; the host-gateway alias depends on it")
	}
	if got := applianceExtraHosts(p); len(got) != 1 || !strings.Contains(got[0], "host-gateway") {
		t.Errorf("an appliance-backed worker needs the host.docker.internal alias, got %v", got)
	}
}

// A CALLER WHO NAMED AN ADDRESS KEEPS IT. The appliance is the second rung, never an override —
// the same order `registryAddress` uses, so a worker pointed at a remote controller stays pointed
// at it while an appliance happens to be running on the same box.
func TestResolveWorkerPlaneKeepsWhatTheCallerSaid(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KONTRA_DATA_DIR", dir)
	if err := appliance.WriteEndpoints(dir, appliance.Endpoints{
		Bind: "172.17.0.1", Temporal: "172.17.0.1:39101", S3: "http://172.17.0.1:39102",
	}); err != nil {
		t.Fatal(err)
	}
	p, err := resolveWorkerPlane(scaleOpts{
		name: "echo", version: "0.1.0",
		address: "controller.internal:7233", network: "kontra",
	})
	if err != nil {
		t.Fatalf("resolveWorkerPlane: %v", err)
	}
	if p.address != "controller.internal:7233" {
		t.Errorf("the caller's address was overridden: %q", p.address)
	}
	if p.network != "kontra" {
		t.Errorf("the caller's network was overridden: %q", p.network)
	}
	// The addresses the caller did NOT name still come from the appliance: it is a fallback chain,
	// not an all-or-nothing switch.
	if p.s3 != "http://172.17.0.1:39102" {
		t.Errorf("s3 = %q, want the appliance's", p.s3)
	}
}

// A LOOPBACK-BOUND APPLIANCE IS REFUSED BY NAME. The alternative is what this whole slice is
// about: containers that start, poll nothing, and count as replicas.
func TestResolveWorkerPlaneRefusesALoopbackAppliance(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KONTRA_DATA_DIR", dir)
	if err := appliance.WriteEndpoints(dir, appliance.Endpoints{
		Bind: "127.0.0.1", Temporal: "127.0.0.1:7233", S3: "http://127.0.0.1:8333", KV: "127.0.0.1:6379",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveWorkerPlane(scaleOpts{name: "echo", version: "0.1.0"}); err == nil {
		t.Fatal("a worker container was allowed to be pointed at a loopback appliance")
	} else if !strings.Contains(err.Error(), "--bind") {
		t.Fatalf("the refusal must name the fix, got: %v", err)
	}

	// `--network host` is the one topology where loopback IS reachable, because the container has
	// no network namespace of its own. Refusing it there would be refusing a working setup.
	p, err := resolveWorkerPlane(scaleOpts{name: "echo", version: "0.1.0", network: "host"})
	if err != nil {
		t.Fatalf("--network host must be allowed against a loopback appliance, got %v", err)
	}
	if p.address != "127.0.0.1:7233" {
		t.Errorf("address = %q, want the appliance's loopback", p.address)
	}
	if got := applianceExtraHosts(p); got != nil {
		t.Errorf("a host-network container must get no host-gateway alias (docker rejects it), got %v", got)
	}
}

// WITH NO APPLIANCE RUNNING, NOTHING CHANGES. The compose control plane is still a supported
// topology (ADR 0031 §5) and this resolution must not have quietly moved its defaults.
func TestResolveWorkerPlaneFallsBackToCompose(t *testing.T) {
	t.Setenv("KONTRA_DATA_DIR", t.TempDir()) // an empty data dir: no appliance record
	p, err := resolveWorkerPlane(scaleOpts{name: "echo", version: "0.1.0"})
	if err != nil {
		t.Fatalf("resolveWorkerPlane: %v", err)
	}
	for _, tc := range []struct{ what, got, want string }{
		{"temporal", p.address, "temporal:7233"},
		{"orchestrator", p.orchestrator, "http://orchestrator-api:8088"},
		{"object store", p.s3, "http://seaweed:8333"},
		{"state store", p.redis, "redis:6379"},
		{"network", p.network, controlPlaneNetwork},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want the compose default %q", tc.what, tc.got, tc.want)
		}
	}
	if p.appliance {
		t.Error("no appliance record must not report an appliance-backed plane")
	}
	if got := applianceExtraHosts(p); got != nil {
		t.Errorf("a compose worker resolves by compose DNS and needs no alias, got %v", got)
	}
}
