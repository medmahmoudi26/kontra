package main

import (
	"testing"
)

// WHICH CONTROL PLANE A WORKER CONTAINER DIALS — the resolution that replaced four hard-coded
// compose DNS names scattered through startWorker.
//
// The failure being pinned is not "the wrong address" in the abstract. A worker handed an address
// nothing answers on starts, retries a name that does not resolve, restarts under
// `unless-stopped`, and appears in `docker ps` and in `kontra workers list` as a running replica.
// Nothing anywhere reports a difference, which is why these defaults are asserted by name rather
// than trusted to stay correct.
func TestResolveWorkerPlaneUsesTheComposeDefaults(t *testing.T) {
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
}

// A CALLER WHO NAMED AN ADDRESS KEEPS IT, and the ones they did not name still get a default: it is
// a fallback per field, not an all-or-nothing switch. `kontra workers scale --address` against a
// remote controller has to stay pointed at it.
func TestResolveWorkerPlaneKeepsWhatTheCallerSaid(t *testing.T) {
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
	if p.s3 != "http://seaweed:8333" {
		t.Errorf("an unnamed address must still get its default, got s3 = %q", p.s3)
	}
}
