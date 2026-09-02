// Package schema is the ONE JSON Schema derivation this SDK has.
//
// Two things reflect a shape into a document, and they must not be two derivations: the actor
// catalog, which describes what a Method takes and emits (`runtime/go/registrar`), and an ask, which
// declares what an answer has to look like (`lib/hitl`). Both are rendered as a form by the same
// component in the console and both are validated by the same ajv instance in the orchestrator, so
// two derivations would be two dialects of the same struct — one form that accepts a value the other
// refuses, over nothing an author wrote.
//
// The Python peer keeps the same rule for the same reason: `lib/hitl._schema_of` calls the same
// `actorkit.schema.schema_of` the catalog uses.
//
// INLINE, NOT REFERENCED (`DoNotReference`). A `$ref`/`$defs` document is legal JSON Schema and the
// form renderers here do not resolve one, so a nested struct would render as an empty field.
package schema

import (
	"encoding/json"
	"reflect"

	"github.com/invopop/jsonschema"
)

// Of is the JSON Schema of a zero VALUE — `schema.Of(URL{})`, which is how a Method declares what it
// takes. nil in, nil out: the declaration is optional and an absent schema is not an error.
func Of(v any) json.RawMessage {
	if v == nil {
		return nil
	}
	return marshal(reflector().Reflect(v))
}

// OfType is the JSON Schema of a TYPE, for a caller that has no value to hand — `hitl.Ask[T]` knows
// the shape of its answer only as a type parameter.
//
// AN INTERFACE DECLARES NOTHING. `Ask[any]` is an ask that constrains nothing, and reflecting `any`
// would produce a document that accepts everything while looking like a rule — worse than saying
// there is no rule, which is what nil says.
func OfType(t reflect.Type) map[string]any {
	if t == nil || t.Kind() == reflect.Interface {
		return nil
	}
	raw := marshal(reflector().ReflectFromType(t))
	if raw == nil {
		return nil
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil
	}
	return doc
}

func reflector() *jsonschema.Reflector {
	return &jsonschema.Reflector{DoNotReference: true} // inline nested types (no $defs/$ref)
}

// marshal turns a reflected schema into its document, or nothing. A schema that will not marshal is
// omitted rather than reported: the catalog entry and the ask are both still useful without one, and
// neither is the place to fail a boot or a park over a reflection edge case.
func marshal(s *jsonschema.Schema) json.RawMessage {
	b, err := json.Marshal(s)
	if err != nil {
		return nil
	}
	return b
}
