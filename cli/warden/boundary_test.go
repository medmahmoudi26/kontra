package warden

// The Machine side imports nothing that belongs to the control plane.
//
// WHAT THE COMPILER ALREADY GUARANTEES, so that this file does not claim credit for it: `warden`
// cannot import `package main`, so no amount of carelessness gets `kontra up`, the Pulumi engine or
// the fleet commands in here. That is the whole reason the Warden became a package (ADR 0042) and
// it needs no test.
//
// WHAT IT DOES NOT. Nothing stops someone adding `cli/internal/somethingControlPlaneOnly`, or a
// third-party client for the orchestrator's API, and the compiler would be perfectly happy. The
// boundary would then be a directory name again — which is the state this package was created to
// leave. So the rule is stated positively and narrowly: every kontra import here is under
// `cli/internal/`, and each one is a thing BOTH halves genuinely need.
//
// It also fails when a NEW internal package appears in this list, which is the point: adding one is
// a decision about what the two halves share, and it should be made on purpose rather than by
// whatever the compiler accepted.

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The kontra packages the Machine side may import, and why each is shared.
var allowed = map[string]string{
	"cli/internal/cliio":                  "stdout/stderr, so a test captures both halves' output",
	"cli/internal/cliutil":                "envOr, fileExists, the install root",
	"cli/internal/config":                 "the Temporal address and namespace an unconfigured install answers with",
	"cli/internal/corpus":                 "the conformance corpora, driven by both arms",
	"cli/internal/ociref":                 "the OCI reference grammar — a Bundle's address",
	"cli/internal/queues":                 "the task-queue derivation, one of four language arms",
	"cli/internal/trustpolicy":            "what may run here, and who signed it",
	"cli/internal/trustpolicy/cosignstub": "a fake cosign, shared so both arms fake it the same way",
}

var kontraImport = regexp.MustCompile(`"github\.com/medmahmoudi26/kontra/([^"]+)"`)

func TestTheMachineSideImportsNothingFromTheControlPlane(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	// A GLOB THAT MATCHED NOTHING WOULD PASS. This package is not small; assert it was read.
	if len(files) < 10 {
		t.Fatalf("only %d .go files here — the walk is broken, so the assertions below mean nothing", len(files))
	}

	seen := map[string]bool{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range kontraImport.FindAllStringSubmatch(string(b), -1) {
			pkg := m[1]
			seen[pkg] = true
			if _, ok := allowed[pkg]; !ok {
				t.Errorf("%s imports %q, which is not on the Machine side's list.\n"+
					"  If both halves genuinely need it, move it under cli/internal/ and add it to `allowed`\n"+
					"  with the reason. If only the control plane needs it, this import is the bug.", f, pkg)
			}
		}
	}

	// AND THE LIST DOES NOT ROT. An entry for a package nothing imports any more is a claim about a
	// dependency that is gone, and the next reader would take it for one that exists.
	var stale []string
	for pkg := range allowed {
		if !seen[pkg] && !strings.HasSuffix(pkg, "cosignstub") {
			stale = append(stale, pkg)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("`allowed` lists %v, which nothing here imports — delete the entries", stale)
	}
}
