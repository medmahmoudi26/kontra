package wire

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	kontrav1 "github.com/medmahmoudi26/kontra/handler/_gen/kontra/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// These tests are the Go peer of python tests/test_workflows_client.py: they hold the
// hand-written wire structs to the proto contract-of-record, so a proto field add/rename
// can't silently drift from the Go wire shape. The wire stays JSON (ADR 0002) — proto is
// only the shared type definition.

// jsonFieldNames returns the set of json tag names (sans options) of a struct's fields.
func jsonFieldNames(t reflect.Type) map[string]bool {
	out := map[string]bool{}
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		out[strings.Split(tag, ",")[0]] = true
	}
	return out
}

// protoFieldNames returns the set of proto field names (snake_case, the wire JSON keys
// since ts-proto/python keep snake_case).
func protoFieldNames(m proto.Message) map[string]bool {
	out := map[string]bool{}
	fields := m.ProtoReflect().Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		out[string(fields.Get(i).Name())] = true
	}
	return out
}

func assertProtoSubset(t *testing.T, name string, goStruct any, msg proto.Message, allowedExtra ...string) {
	t.Helper()
	got := jsonFieldNames(reflect.TypeOf(goStruct))
	want := protoFieldNames(msg)
	extra := map[string]bool{}
	for _, e := range allowedExtra {
		extra[e] = true
	}
	// Every proto field MUST exist on the Go struct (no missing contract field).
	for f := range want {
		if !got[f] {
			t.Errorf("%s: proto field %q missing from Go wire struct", name, f)
		}
	}
	// Every Go field MUST be a proto field or an explicitly-allowed internal.
	for f := range got {
		if !want[f] && !extra[f] {
			t.Errorf("%s: Go wire field %q is neither a proto field nor an allowed internal", name, f)
		}
	}
}

func TestEntryInputCongruentWithProto(t *testing.T) {
	// The actor runs one batch per run (ADR 0012 — the dispatcher shards), so the Go
	// wire struct is EXACTLY the proto field set: no extra internals.
	assertProtoSubset(t, "EntryInput", EntryInput{}, &kontrav1.EntryInput{})
}

// The v2 dispatch carries which Method it means and which Session it belongs to (ADR 0023
// §5, §6). Both arrive as JSON keys written by a caller that shares no code with this struct
// — the Python SDK's entry_input and the TS interpreter — so the tag spelling is the contract
// and a drift is silent: the handler would run the batch against the wrong Method, or against
// the sole one, and report success.
func TestEntryInputCarriesTheMethodAndSession(t *testing.T) {
	var in EntryInput
	if err := json.Unmarshal([]byte(`{"method":"crawl","session_id":"s-7f3a"}`), &in); err != nil {
		t.Fatal(err)
	}
	if in.Method != "crawl" {
		t.Errorf("method: got %q, want %q", in.Method, "crawl")
	}
	if in.SessionID != "s-7f3a" {
		t.Errorf("session_id: got %q, want %q", in.SessionID, "s-7f3a")
	}
}

func TestBareRefCongruentWithProto(t *testing.T) {
	assertProtoSubset(t, "BareRef", BareRef{}, &kontrav1.BareRef{})
}

// --- the catalog descriptor ---
//
// This one has no hand-written Go peer to be congruent WITH: the Go SDK emits it from
// runtime/go/registrar, which deliberately imports nothing from /handler, and its
// descriptor struct is unexported. So what these pin is the generated contract itself, read
// back through protoreflect — the real descriptor, not the .proto text.
//
// Worth pinning because of how the last drift happened. `description` (what a Method is for)
// and `source` (where the worker loaded the actor from) were added to the wire, to
// POST /api/actors and to the `actors` table in 209b0ad, and neither reached catalog.proto:
// nothing outside `_gen/` loads ActorDescriptor, so a contract file two fields behind the thing
// it defines broke no build anywhere.

// fieldNumber returns the number `name` is declared at, or 0 if the message has no such field.
func fieldNumber(m proto.Message, name string) protoreflect.FieldNumber {
	f := m.ProtoReflect().Descriptor().Fields().ByName(protoreflect.Name(name))
	if f == nil {
		return 0
	}
	return f.Number()
}

// The numbers are asserted, not just the names. `buf breaking` is the real guard against a
// renumber and it is configured ADVISORY in CI while v2 lands (ADR 0023 flag day), so for now a
// renumber would only print a warning — and a renumbered field re-points every descriptor
// already registered against the old numbering.
func TestCatalogDescriptorCarriesDescriptionAndSource(t *testing.T) {
	if got := fieldNumber(&kontrav1.ActorOperation{}, "description"); got != 5 {
		t.Errorf("ActorOperation.description is field %d, want 5 (appended, never renumbered)", got)
	}
	if got := fieldNumber(&kontrav1.ActorDescriptor{}, "source"); got != 7 {
		t.Errorf("ActorDescriptor.source is field %d, want 7 (appended after digest=6)", got)
	}
	// The names ARE the wire keys — both SDKs hand-write this JSON — so a field spelled
	// differently here than in the emitters is a value that arrives and is dropped.
	for _, name := range []string{"key", "name", "version", "schema_version", "operations", "digest", "source"} {
		if fieldNumber(&kontrav1.ActorDescriptor{}, name) == 0 {
			t.Errorf("ActorDescriptor has no field %q", name)
		}
	}
	for _, name := range []string{"name", "params", "input", "output", "description"} {
		if fieldNumber(&kontrav1.ActorOperation{}, name) == 0 {
			t.Errorf("ActorOperation has no field %q", name)
		}
	}
}

// The other half of a run describes nothing today: `GET /api/workflows` lists
// `.kontra/workflows/*.py` by filename, byte count and mtime, so a workflow can be shown to
// exist and never to say what it takes, what it returns or what it is for. The type lands
// BEFORE the registration path that fills it — nothing emits one yet — so that path has
// something to be checked against instead of inventing a shape as it goes.
func TestWorkflowDescriptorDescribesAWorkflow(t *testing.T) {
	for _, name := range []string{"name", "description", "input", "output"} {
		if fieldNumber(&kontrav1.WorkflowDescriptor{}, name) == 0 {
			t.Errorf("WorkflowDescriptor has no field %q", name)
		}
	}
	// Opaque by decision: per-Method I/O is JSON Schema Draft 2020-12, which is not proto, so
	// these are google.protobuf.Struct and not modelled messages (same stance as ActorOperation).
	for _, name := range []string{"input", "output"} {
		f := (&kontrav1.WorkflowDescriptor{}).ProtoReflect().Descriptor().Fields().ByName(protoreflect.Name(name))
		if f == nil {
			continue // already reported above
		}
		if got := string(f.Message().FullName()); got != "google.protobuf.Struct" {
			t.Errorf("WorkflowDescriptor.%s is %s, want google.protobuf.Struct — a JSON Schema "+
				"document is carried, not modelled", name, got)
		}
	}
}

// Sanity: the proto descriptor really enumerates fields (guards against a broken gen).
func TestProtoDescriptorsNonEmpty(t *testing.T) {
	for _, m := range []proto.Message{
		&kontrav1.EntryInput{}, &kontrav1.BareRef{},
		&kontrav1.ActorDescriptor{}, &kontrav1.ActorOperation{}, &kontrav1.WorkflowDescriptor{},
	} {
		if got := m.ProtoReflect().Descriptor().Fields().Len(); got == 0 {
			t.Errorf("%s: proto descriptor has no fields", m.ProtoReflect().Descriptor().Name())
		}
	}
	var _ protoreflect.Message // keep the import even if the helper above changes
}
