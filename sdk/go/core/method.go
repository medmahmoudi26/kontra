package core

import (
	"fmt"
	"strings"
)

// MethodFunc is what an author writes: one named, individually dispatchable callable that
// receives the whole Batch and its output Dataset, loops over the Batch, and pushes to the Dataset
// (ADR 0023 §2, §23; ADR 0028 §2).
//
//	func crawl(s *kontra.Session, b *kontra.Batch, ds *kontra.Dataset) error {
//		for unit := range b.All() {
//			ds.Push(fetch(unit.Str("url")))
//		}
//		return b.Err()
//	}
//
// The Dataset is the third parameter, exactly like the Batch is the first: both are things the
// caller hands over, and self is the one the caller established with a scope (ADR 0028 §3). It is
// NOT the framework's per-unit callback: cross-unit context — a bulk API taking 100 at a time, one
// transaction per Batch, dedupe within a Batch — is the author's to write, and a Go SDK that kept
// the callback would be v1 with new names.
type MethodFunc func(*Session, *Batch, *Dataset) error

// Method is one registered Method: the name a caller dispatches, the body, and what it takes and
// emits.
type Method struct {
	// Name is the dispatch name. Go cannot read a function's declared name the way Python's
	// decorator reads `f.__name__`, so it is given explicitly — the one string an author types
	// per Method, and the string a caller must match.
	Name string
	Fn   MethodFunc
	// Takes and Emits are zero values of this Method's input and output types, from which the
	// catalog reflects one operation's JSON Schemas. They live HERE and not on the Registry
	// because since ADR 0023 §9 an Actor has many Methods with different signatures — `fetch`
	// takes a host and emits a page, `title` takes a page and emits a title — and one
	// Actor-level pair cannot describe that. nil leaves that schema unset: the Method still
	// registers and stays dispatchable by name.
	//
	// Emits cannot be inferred from the body's return type, because a Method pushes rather than
	// returns (ADR 0028 §3): a Method returns only an error. So the declaration is explicit.
	Takes any
	Emits any
	// Description is what this Method is FOR, in a sentence a caller reads.
	//
	// GO CANNOT DO WHAT PYTHON DOES HERE, and it is worth being exact about why rather than
	// leaving the asymmetry to look like an oversight. The Python SDK reads `fn.__doc__`, so an
	// author's docstring travels for free. A Go doc comment is not in the binary — it exists in
	// the source and in `go/doc`, and a running worker has neither — so the same fact has to be
	// typed once more, as a value. `Does("…")` is that value, kept next to the comment it repeats.
	//
	// The catalog carried a name and two schemas and nothing else, so every surface listing a
	// Method could say what it TAKES and never what it DOES.
	Description string
}

// MethodOption declares something about a Method at registration. Variadic options rather than
// more parameters, so declaring types costs no extra top-level lines in the actor file and
// registering without them stays exactly as it reads today.
type MethodOption func(*Method)

// Takes declares what one Unit of this Method's Batch carries. Pass a zero value: `Takes(URL{})`.
func Takes(v any) MethodOption { return func(m *Method) { m.Takes = v } }

// Emits declares what this Method emits per Unit. Pass a zero value: `Emits(Page{})`.
func Emits(v any) MethodOption { return func(m *Method) { m.Emits = v } }

// Does declares what this Method is FOR, in one sentence: `Does("Resolve each domain's NS set")`.
//
// The Go peer of Python's docstring — see Method.Description for why a doc comment cannot serve.
// One sentence, present tense, about what a CALLER gets: it is read on the Actors page and on a
// Scratch node beside the Method's name, where there is room for a line and not a paragraph.
func Does(s string) MethodOption { return func(m *Method) { m.Description = s } }

// AddMethod registers a Method under its dispatch name. A name declared twice PANICS: it happens
// at registration, in main(), before the worker polls anything — so the author sees the line that
// is wrong instead of a dispatch silently reaching whichever body was registered last (the Go
// peer of Python's TypeError at import).
func (r *Registry) AddMethod(name string, fn MethodFunc, opts ...MethodOption) {
	if name == "" {
		panic("kontra: a Method needs a dispatch name — Method(\"crawl\", crawl)")
	}
	if fn == nil {
		panic(fmt.Sprintf("kontra: Method %q has no body", name))
	}
	for _, m := range r.Methods {
		if m.Name == name {
			panic(fmt.Sprintf("kontra: Method %q declared twice; give one of them another name", name))
		}
	}
	m := Method{Name: name, Fn: fn}
	for _, o := range opts {
		o(&m)
	}
	r.Methods = append(r.Methods, m)
}

// ResolveMethod is the Method a dispatch means, resolved PER BATCH rather than at boot — which is
// what lets one loaded Session serve several Methods (ADR 0023 §9).
//
// Named -> that one. Unnamed against a sole Method -> that one, so an Actor with one Method
// (which is what an Activity became) needs no name on the wire. Unnamed against several REFUSES
// rather than taking declaration order: a caller who forgot the name would otherwise silently get
// whichever body the author happened to write first, and declared order as a contract is exactly
// what §16 rejects.
//
// (nil, nil) means the actor declares no Methods at all — a load-only actor, whose batch passes
// its Units through unchanged.
func (r *Registry) ResolveMethod(name string) (*Method, error) {
	if name != "" {
		for i := range r.Methods {
			if r.Methods[i].Name == name {
				return &r.Methods[i], nil
			}
		}
		return nil, fmt.Errorf("no Method named %q (have: %s)", name, r.methodNames())
	}
	switch len(r.Methods) {
	case 0:
		return nil, nil
	case 1:
		return &r.Methods[0], nil
	default:
		return nil, fmt.Errorf("%s declares %d methods (%s); the dispatch must name one",
			r.Name, len(r.Methods), r.methodNames())
	}
}

func (r *Registry) methodNames() string {
	if len(r.Methods) == 0 {
		return "none declared"
	}
	names := make([]string, len(r.Methods))
	for i, m := range r.Methods {
		names[i] = m.Name
	}
	return strings.Join(names, ", ")
}
