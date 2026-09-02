package registrar

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/medmahmoudi26/kontra/sdk/go/core"
)

type target struct {
	Host string `json:"host"`
}
type page struct {
	Body string `json:"body"`
}
type title struct {
	Text string `json:"text"`
}
type params struct {
	Depth int `json:"depth"`
}

func noop(*core.Session, *core.Batch, *core.Dataset) error { return nil }

// The catalog registers ONE OPERATION PER METHOD, each with its own schemas (issue 14). One
// Actor-level pair cannot describe an Actor with many Methods: `fetch` takes a host and emits a
// page, `title` takes a page and emits a title. Registering the first pair for both would
// typecheck every node against the wrong schema — and it would do it silently.
func TestOneOperationPerMethodWithItsOwnSchemas(t *testing.T) {
	reg := &core.Registry{Name: "crawler", Version: "0.1.0"}
	reg.AddMethod("fetch", noop, core.Takes(target{}), core.Emits(page{}))
	reg.AddMethod("title", noop, core.Takes(page{}), core.Emits(title{}))

	d := Describe(reg)
	if len(d.Operations) != 2 {
		t.Fatalf("got %d operations, want one per Method", len(d.Operations))
	}
	if d.Operations[0].Name != "fetch" || d.Operations[1].Name != "title" {
		t.Fatalf("operations named %q/%q, want fetch/title — the Method name IS the operation name",
			d.Operations[0].Name, d.Operations[1].Name)
	}
	if !strings.Contains(string(d.Operations[0].Input), "host") {
		t.Errorf("fetch input schema = %s, want it to describe target{Host}", d.Operations[0].Input)
	}
	if !strings.Contains(string(d.Operations[1].Input), "body") {
		t.Errorf("title input schema = %s, want it to describe page{Body} — NOT fetch's input",
			d.Operations[1].Input)
	}
	if !strings.Contains(string(d.Operations[1].Output), "text") {
		t.Errorf("title output schema = %s, want it to describe title{Text}", d.Operations[1].Output)
	}
}

// params is run-wide config, not a per-Method signature, so it stays declared on the Actor — and
// rides every operation, because that is where the wire carries it.
func TestParamsStayActorLevelAndRideEveryOperation(t *testing.T) {
	reg := &core.Registry{Name: "crawler", ParamsType: params{}}
	reg.AddMethod("fetch", noop, core.Takes(target{}))
	reg.AddMethod("title", noop, core.Takes(page{}))

	for _, op := range Describe(reg).Operations {
		if !strings.Contains(string(op.Params), "depth") {
			t.Errorf("operation %q params = %s, want the Actor's params", op.Name, op.Params)
		}
	}
}

// A Method that declares nothing still registers and stays dispatchable by name: it contributes an
// operation with no schemas. The old behaviour refused to register such an actor at all, which
// made it serve happily while being invisible in the workers list and undispatchable.
func TestAnUntypedActorStillRegistersItsIdentityAndItsMethods(t *testing.T) {
	reg := &core.Registry{Name: "diy", Version: "0.1.0"}
	reg.AddMethod("run", noop)

	d := Describe(reg)
	if d.Key != "diy@0.1.0" || d.Name != "diy" || d.Version != "0.1.0" {
		t.Fatalf("identity lost: %+v", d)
	}
	if len(d.Operations) != 1 || d.Operations[0].Name != "run" {
		t.Fatalf("operations = %+v, want the one declared Method", d.Operations)
	}
	if d.Operations[0].Input != nil || d.Operations[0].Output != nil {
		t.Errorf("undeclared types must leave the schemas unset, got in=%s out=%s",
			d.Operations[0].Input, d.Operations[0].Output)
	}
}

// A load-only Actor declares no Methods at all. It still registers its identity — it is a real
// deployed Worker and must appear in the workers list — and simply offers no operations.
func TestALoadOnlyActorRegistersItsIdentityWithNoOperations(t *testing.T) {
	d := Describe(&core.Registry{Name: "holder", Version: "2.0.0"})
	if d.Key != "holder@2.0.0" {
		t.Fatalf("identity lost: %+v", d)
	}
	if d.Operations == nil {
		t.Error("operations must be an empty array, not null — the reader iterates it")
	}
	if len(d.Operations) != 0 {
		t.Errorf("operations = %+v, want none", d.Operations)
	}
}

// What a Method is FOR travels, and a Go doc comment cannot be it.
//
// The Python SDK reads `fn.__doc__`, so an author's docstring is carried for free. A Go doc
// comment is not in the binary — it exists in the source and in `go/doc`, and a running worker has
// neither — so the same sentence has to be typed once more, as a value. That asymmetry is the
// reason `Does` exists, and this pins that it reaches the wire.
func TestDoesReachesTheCatalog(t *testing.T) {
	reg := &core.Registry{Name: "dnsfacts", Version: "0.1.0"}
	reg.AddMethod("ns", noop, core.Takes(target{}), core.Does("Resolve each domain's NS set"))
	reg.AddMethod("addrs", noop, core.Takes(target{}))

	d := Describe(reg)
	if d.Operations[0].Description != "Resolve each domain's NS set" {
		t.Errorf("description = %q, want the sentence Does() was given", d.Operations[0].Description)
	}
	// ABSENT, NEVER EMPTY. `omitempty` is what makes a Method nobody described render as undescribed
	// rather than as described-with-nothing — a distinction only one of which a reader can act on.
	b, err := json.Marshal(d.Operations[1])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "description") {
		t.Errorf("an undescribed Method put a description on the wire: %s", b)
	}
}
