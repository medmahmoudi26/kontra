package main

import (
	"os"
	"strings"
	"testing"
)

// Against the LIVE registry. Skipped when it is not reachable, because a unit suite must not depend
// on a running install — but when it IS reachable this is acceptance test 11.
func TestLiveRuntimeResolution(t *testing.T) {
	reg := os.Getenv("KONTRA_TEST_REGISTRY")
	if reg == "" {
		t.Skip("set KONTRA_TEST_REGISTRY to run against a live registry")
	}
	t.Setenv("KONTRA_RUNTIMES_PREFIX", "")

	if want := os.Getenv("KONTRA_TEST_RUNTIME"); want != "" {
		r, err := resolveRuntime(reg, want, "py")
		if err != nil {
			t.Fatalf("resolving %s: %v", want, err)
		}
		t.Logf("resolved %s -> %s (digest %s)", want, r.Ref, r.Digest[:19])
		if r.Digest == "" || !strings.HasPrefix(r.Digest, "sha256:") {
			t.Errorf("no digest recorded for %s", want)
		}
	}

	// An unknown runtime must fail BEFORE the build and name what exists.
	_, err := resolveRuntime(reg, "nosuchruntime:1", "py")
	if err == nil {
		t.Fatal("an unknown runtime resolved")
	}
	t.Logf("unknown runtime refusal: %v", err)
	if !strings.Contains(err.Error(), "available") && !strings.Contains(err.Error(), "no runtimes are published") {
		t.Errorf("the refusal must list what IS there, or say there is nothing: %v", err)
	}
}
