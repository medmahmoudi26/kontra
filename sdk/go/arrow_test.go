package kontra_test

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// THE ARROW, EXECUTED RATHER THAN DESCRIBED.
//
// `runtime/` may import `sdk/`; `sdk/` may import nothing of `runtime/`. That is the whole point
// of the two directories, and a sentence in a README stops being true in a month — so it is asked
// of the compiler here, over the REAL transitive graph, not over this file's import block.
//
// `go list -deps` is the right instrument because it is transitive: a package three hops down that
// reaches for the engine fails this, and an author reading only the import lines of sdk/go/*.go
// would never see it. The one edge that would have been legitimate — Serve()'s handoff to the
// actor host — is not an import at all: the SDK declares a seam (kontra.Host) that
// `runtime/go`'s init() fills, so even that hop is absent from this list. See serve.go.
//
// WHY THIS RUNS `go list` INSTEAD OF PARSING SOURCE: a build-tag'd file, a generated file or a
// test-only import would each slip past a grep, and the arrow is a property of what LINKS, not of
// what a reader sees. This is the same question `pip`-side test_sdk_arrow.py asks of Python.

// deps returns every package in the transitive import graph of the sdk module's own packages.
func deps(t *testing.T) []string {
	t.Helper()
	cmd := exec.Command("go", "list", "-deps", "./...")
	cmd.Dir = "."
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod")
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			t.Fatalf("go list -deps failed: %v\n%s", err, ee.Stderr)
		}
		t.Fatalf("go list -deps failed: %v", err)
	}
	return strings.Fields(string(out))
}

// TestTheSDKImportsNothingOfTheRuntime is the Go half of the one-way arrow. It fails the moment
// any package under sdk/go acquires an import — direct or transitive, in code or in a test
// helper — of a package under runtime/go.
func TestTheSDKImportsNothingOfTheRuntime(t *testing.T) {
	const runtimePrefix = "github.com/medmahmoudi26/kontra/runtime/"
	for _, p := range deps(t) {
		if strings.HasPrefix(p, runtimePrefix) {
			t.Errorf("sdk/go imports %s — the arrow is runtime -> sdk, never the reverse.\n"+
				"If this is the Serve() handoff, register it through kontra.Host from the runtime's\n"+
				"init() instead of importing the host here (see serve.go).", p)
		}
	}
}

// TestTheSDKCarriesNoInfrastructureClient is the second half of the same claim, and the one that
// says what the arrow is FOR. An author surface that transitively links a Redis client or an S3
// client is a surface that cannot be imported by a caller who has neither — a test, the CLI, a
// tool that only wants the vocabulary. Those clients belong to the state tiers and the blob
// plane, which are the runtime's job.
//
// TEMPORAL IS DELIBERATELY NOT IN THIS LIST, and pretending otherwise would be the dishonest
// version of this test. `catalog`, `hitl` and `narrate` are the WORKFLOW-facing half of the author
// surface: their whole content is workflow.ExecuteActivity, workflow.Await and
// temporal.NewApplicationError. Temporal is the substrate an author writes against there, not an
// implementation detail leaking upward — Python draws the identical line, which is why
// `import actorkit` costs no temporalio while `from actorkit import ask` does.
func TestTheSDKCarriesNoInfrastructureClient(t *testing.T) {
	banned := map[string]string{
		"github.com/redis/go-redis":    "a Redis client (the durable state tiers are the runtime's)",
		"github.com/aws/aws-sdk-go-v2": "an object-store client (the blob plane is the runtime's)",
	}
	for _, p := range deps(t) {
		for prefix, why := range banned {
			if strings.HasPrefix(p, prefix) {
				t.Errorf("sdk/go transitively links %s — %s", p, why)
			}
		}
	}
}
