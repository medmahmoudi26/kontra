package hooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
)

// hooksDef resolves one definition of spec/schema/hooks.json, as a 2020-12 root that refers to it.
func hooksDef(t *testing.T, name string) *jsonschema.Resolved {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "spec", "schema", "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	var root struct {
		Defs map[string]json.RawMessage `json:"$defs"`
	}
	if err := json.Unmarshal(b, &root); err != nil {
		t.Fatal(err)
	}
	doc, _ := json.Marshal(map[string]any{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"$ref":    "#/$defs/" + name,
		"$defs":   root.Defs,
	})
	var s jsonschema.Schema
	if err := json.Unmarshal(doc, &s); err != nil {
		t.Fatal(err)
	}
	rs, err := s.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	return rs
}

func validate(t *testing.T, rs *jsonschema.Resolved, v any) error {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var doc any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	return rs.Validate(doc)
}

// The payloads the gate sends are hooks.json's shapes: the principal's and the task's caller is callerId (D363),
// and a payload that still names buyerId is not one.
func TestPayloadsMatchHooksJSON(t *testing.T) {
	principal := Principal{Kind: "key", ID: "key:kk_a_00000001", CallerID: "0199a3b2-0000-7000-8000-0000000000bb", Via: "api_key"}
	admit := AdmitRequest{Operation: OpTask, TaskID: "0199a3b2-0000-7000-8000-0000000000aa", Protocol: "a2a",
		Service:   Service{TenantID: "0199a3b2-0000-7000-8000-000000000001", Tenant: "default", Name: "hello", Kind: "a2a"},
		Principal: principal, ContextID: "c", Message: &MessageInfo{MessageID: "m", Parts: []PartInfo{{Size: 3}}}}
	if err := validate(t, hooksDef(t, "admitRequest"), admit); err != nil {
		t.Errorf("AdmitRequest against hooks.json: %v", err)
	}
	ref := TaskRef{TaskID: "0199a3b2-0000-7000-8000-0000000000aa", TenantID: "0199a3b2-0000-7000-8000-000000000001", Service: "hello",
		Kind: "a2a", Protocol: "a2a", CallerID: "0199a3b2-0000-7000-8000-0000000000bb", Principal: "key:kk_a_00000001", Via: "api_key",
		ContextID: "c", State: "working"}
	if err := validate(t, hooksDef(t, "taskRef"), ref); err != nil {
		t.Errorf("TaskRef against hooks.json: %v", err)
	}
	b, _ := json.Marshal(principal)
	old := map[string]any{}
	_ = json.Unmarshal([]byte(strings.Replace(string(b), `"callerId"`, `"buyerId"`, 1)), &old)
	if err := validate(t, hooksDef(t, "principal"), old); err == nil {
		t.Error("a principal naming buyerId validates against hooks.json")
	}
}
