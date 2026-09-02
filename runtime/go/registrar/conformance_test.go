package registrar

import (
	"encoding/json"
	"os"
	"sort"
	"testing"

	"github.com/medmahmoudi26/kontra-local/sdk/go/core"
)

// The GO ARM of the cross-SDK catalog contract (conformance/catalog.json).
//
// Three hand-written emitters produce the descriptor `POST /api/actors` accepts — this file,
// Python's internals/catalog.py, and whatever the design tool uploads — and they share no code.
// Nothing but a fixture makes them agree, and they had already stopped agreeing: `source` reached
// the proto, the `actors` table and the Python SDK and never reached this file, so a catalogued
// Go actor was the one row on the Actors page with no way back to its code. Every suite was green.
//
// The peers assert the SAME file: tests/test_catalog_conformance.py and
// backend/src/catalog.conformance.test.ts.

// The fixture's Actor, in Go types. The field names and JSON types come FROM the fixture — its
// `expect.operations[].input.properties` is what these must reflect to — so declaring `hostname`
// here, or `int` where the fixture says string, fails this test rather than drifting.
type confTarget struct {
	Host string `json:"host"`
}
type confPage struct {
	URL    string `json:"url"`
	Status int    `json:"status"`
}
type confTitle struct {
	Text string `json:"text"`
}
type confParams struct {
	Depth int `json:"depth"`
}

type catalogFixture struct {
	Expect      map[string]any `json:"expect"`
	UnsetDigest struct {
		Why    string   `json:"why"`
		Absent []string `json:"absent"`
	} `json:"unsetDigest"`
}

func readCatalogFixture(t *testing.T) catalogFixture {
	t.Helper()
	// ../../../conformance/catalog.json — registrar -> go -> runtime -> <repo root>.
	b, err := os.ReadFile("../../../conformance/catalog.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var fx catalogFixture
	if err := json.Unmarshal(b, &fx); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	if len(fx.Expect) == 0 {
		t.Fatal("fixture is empty — a vacuously passing conformance test is worse than none")
	}
	return fx
}

// describeAsJSON runs the real emitter and hands back the JSON it would POST, decoded.
func describeAsJSON(t *testing.T, reg *core.Registry) map[string]any {
	t.Helper()
	b, err := json.Marshal(Describe(reg))
	if err != nil {
		t.Fatalf("marshal descriptor: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("decode descriptor: %v", err)
	}
	return got
}

// projectSchema strips the branding the reflector adds and keeps the part the catalog reads.
//
// invopop stamps every schema with `$schema`, `$id` and `additionalProperties`; pydantic stamps
// `title` on the document and on every property. Neither is a contract — nobody reads them and
// they can never agree — so the fixture holds the dialect-neutral core (type / properties /
// required) and each SDK projects its emission down to it. `required` is sorted because JSON
// Schema's `required` is a SET: its order is whichever order that language's author declared the
// struct fields in, and pinning it would fail on a reorder that changed nothing.
func projectSchema(v any) any {
	m, ok := v.(map[string]any)
	if !ok {
		return v
	}
	out := map[string]any{}
	if t, ok := m["type"]; ok {
		out["type"] = t
	}
	if props, ok := m["properties"].(map[string]any); ok {
		p := map[string]any{}
		for k, v := range props {
			p[k] = projectSchema(v)
		}
		out["properties"] = p
	}
	if req, ok := m["required"].([]any); ok {
		names := make([]string, 0, len(req))
		for _, r := range req {
			s, _ := r.(string)
			names = append(names, s)
		}
		sort.Strings(names)
		asAny := make([]any, len(names))
		for i, n := range names {
			asAny[i] = n
		}
		out["required"] = asAny
	}
	return out
}

// projectDescriptor applies projectSchema to the three schema-carrying keys of every operation,
// in place. Everything else — the identity keys, the operation names, the descriptions, and WHICH
// keys are present at all — is compared verbatim.
func projectDescriptor(d map[string]any) {
	ops, _ := d["operations"].([]any)
	for _, o := range ops {
		op, ok := o.(map[string]any)
		if !ok {
			continue
		}
		for _, k := range []string{"params", "input", "output"} {
			if v, ok := op[k]; ok {
				op[k] = projectSchema(v)
			}
		}
	}
}

// asJSON renders for a readable diff. Go sorts map keys when marshalling, so both sides come out
// in the same order and the failure names the line that differs.
func asJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// fixtureRegistry declares the fixture's Actor: three Methods in declaration order, two with
// different signatures (title takes what fetch emits — the §9 property a single Actor-level pair
// could not express), one described, one declaring nothing at all, and Actor-level params.
func fixtureRegistry() *core.Registry {
	reg := &core.Registry{Name: "demo", Version: "1.2.3", ParamsType: confParams{}}
	reg.AddMethod("fetch", noop,
		core.Takes(confTarget{}), core.Emits(confPage{}), core.Does("Fetch each host once."))
	reg.AddMethod("title", noop, core.Takes(confPage{}), core.Emits(confTitle{}))
	reg.AddMethod("probe", noop)
	return reg
}

func TestDescriptorMatchesCrossSDKFixture(t *testing.T) {
	fx := readCatalogFixture(t)
	want := fx.Expect

	// The two facts the descriptor reports about the PROCESS rather than about the actor: the
	// image it is running and the directory it loaded from. Both are pinned to what the fixture
	// says, because under `go test` the binary lives in a temp dir named differently every run.
	t.Setenv("KONTRA_ACTOR_DIGEST", want["digest"].(string))
	restore := actorSource
	actorSource = func() string { return want["source"].(string) }
	defer func() { actorSource = restore }()

	got := describeAsJSON(t, fixtureRegistry())
	projectDescriptor(got)

	if asJSON(t, got) != asJSON(t, want) {
		t.Errorf("the Go descriptor is not the golden one:\n--- got ---\n%s\n--- want ---\n%s",
			asJSON(t, got), asJSON(t, want))
	}
}

// The unset digest is its own case because the two spellings are not equivalent to the reader:
// the catalog keeps a previously registered digest only when the key is ABSENT
// (`body.digest ?? prev.digest`). This emitter sent `"digest":""` unconditionally, so a dev
// worker booting with no KONTRA_ACTOR_DIGEST unpinned the image the design tool had pinned —
// silently, on every restart.
func TestAnUnknownDigestIsAbsentFromTheDescriptor(t *testing.T) {
	fx := readCatalogFixture(t)
	if len(fx.UnsetDigest.Absent) == 0 {
		t.Fatal("fixture declares no keys to be absent — nothing is being checked")
	}
	t.Setenv("KONTRA_ACTOR_DIGEST", "")

	got := describeAsJSON(t, fixtureRegistry())
	for _, k := range fx.UnsetDigest.Absent {
		if _, present := got[k]; present {
			t.Errorf("%q is on the wire with no value to report — %s", k, fx.UnsetDigest.Why)
		}
	}
}
