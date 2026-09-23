// Package registrar implements describe() + best-effort dev self-registration. describe()
// is the one intrinsically per-language piece (JSON Schema derived from the actor's Go
// types via reflection); registration is shared transport — the worker POSTs its
// descriptor to the orchestrator catalog so the Actors page, `kontra workers list` and a
// caller's `catalog.actor(...)` discover it. There is deliberately NO per-language
// registration CLI: discovery is a push on the normal run path, reusing the orchestrator's
// POST /api/actors endpoint.
package registrar

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/medmahmoudi26/kontra/sdk/go/core"
	"github.com/medmahmoudi26/kontra/sdk/go/schema"
)

// The catalog wire shape lives HERE, locally, so actorkit stays decoupled from /handler
// (no shared import): the descriptor is just JSON the orchestrator's POST /api/actors
// endpoint accepts.
const schemaVersion = "kontra.actor.v1" // actor.json / descriptor schema tag

// catalogKey is the orchestrator's stable identity for an actor: "name@version".
func catalogKey(name, version string) string { return name + "@" + version }

// actorDescriptor is the catalog registration body — the Go peer of python's _post_catalog
// payload. Kept local (not a shared type) so this SDK does not import /handler.
type actorDescriptor struct {
	Key           string           `json:"key"`
	Name          string           `json:"name"`
	Version       string           `json:"version"`
	SchemaVersion string           `json:"schemaVersion"`
	Operations    []actorOperation `json:"operations"`
	// Digest is the OCI image digest this worker is running (ADR 0011), from
	// KONTRA_ACTOR_DIGEST.
	//
	// OMITTED WHEN UNSET, and that is not cosmetic. The catalog keeps a previously registered
	// digest only when the key is ABSENT (`body.digest ?? prev.digest`); an empty string is a
	// value and overwrites it. This sent `"digest":""` unconditionally, so every dev worker
	// booting without the env var silently unpinned the image the design tool had pinned.
	Digest string `json:"digest,omitempty"`
	// Source is the directory this worker loaded the actor from — the Go peer of Python's
	// `actor_dir`, which is the dir of the entry script; here, the dir of the running binary.
	//
	// Go emitted no source at all. `source` was added to the wire, to the `actors` table and to
	// the Python SDK in 209b0ad and this emitter never got it, so a catalogued Go actor was the
	// one row on the Actors page with no way back to its code — indistinguishable from an actor
	// nobody had registered a source for.
	Source string `json:"source,omitempty"`
}

// actorOperation is one dispatchable operation + its JSON Schemas (Draft 2020-12).
type actorOperation struct {
	Name string `json:"name"`
	// Description is what this Method is FOR — `Does("…")` on the registration. Omitted when the
	// author declared none, never sent as an empty string: a blank description and an absent one
	// render differently and mean different things.
	Description string          `json:"description,omitempty"`
	Params      json.RawMessage `json:"params,omitempty"`
	Input       json.RawMessage `json:"input,omitempty"`
	Output      json.RawMessage `json:"output,omitempty"`
}

// Describe builds the catalog descriptor for a registry — the Go peer of python's
// _post_catalog body. Schemas are JSON Schema (Draft 2020-12) reflected from the actor's
// registered Go types; a type left nil yields an unset schema.
//
// ONE OPERATION PER METHOD, each with its own schemas. An Actor has many Methods with different
// signatures (ADR 0023 §9), so a single Actor-level input/output pair cannot describe it — it
// would typecheck every node against whichever Method happened to be declared first. The Method's
// name IS the operation's name, which is what lets a caller resolve the operation it dispatches.
//
// An actor that declares no types still registers: it is a real deployed Worker and must appear
// in the workers list and stay dispatchable by name. Its operations simply carry no schemas.
// `params` is Actor-level — run-wide config, not a per-Method signature — so it rides every
// operation, which is where the wire carries it.
func Describe(reg *core.Registry) actorDescriptor {
	params := schemaJSON(reg.ParamsType)
	ops := make([]actorOperation, 0, len(reg.Methods)) // empty, never null: the reader iterates it
	for _, m := range reg.Methods {
		ops = append(ops, actorOperation{
			Name:        m.Name,
			Description: m.Description,
			Params:      params,
			Input:       schemaJSON(m.Takes),
			Output:      schemaJSON(m.Emits),
		})
	}
	return actorDescriptor{
		Key:           catalogKey(reg.Name, reg.Version),
		Name:          reg.Name,
		Version:       reg.Version,
		SchemaVersion: schemaVersion,
		Operations:    ops,
		Digest:        os.Getenv("KONTRA_ACTOR_DIGEST"),
		Source:        actorSource(),
	}
}

// actorSource answers "where did this actor come from", as the WORKER sees it: the directory
// holding the running binary, which on a fleet Machine is `/opt/kontra/actor/<name>` — the dir
// systemd's ExecStart points into — and in a container is wherever the image put the binary.
// Python takes the same answer from `sys.argv[0]`'s parent. It is a path on the machine that
// SAID it, not on the reader's, which is the honest answer either way.
//
// It is read here rather than declared on core.Registry because it is a fact about the process,
// not about the actor an author wrote — the same reason the digest is read from the environment
// two lines up. A var so the conformance test can pin it: under `go test` the binary lives in a
// temp dir whose name changes every run.
var actorSource = func() string {
	exe, err := os.Executable()
	if err != nil {
		return "" // no path is better than a wrong one; the key is then omitted
	}
	return filepath.Dir(exe)
}

// schemaJSON is the catalog's half of ONE derivation, shared with an ask's `takes` — see
// sdk/go/schema for why two of them would be two dialects of the same struct.
func schemaJSON(v any) json.RawMessage { return schema.Of(v) }

// SelfRegister POSTs the descriptor to the orchestrator catalog. Best-effort and
// env-gated: a no-op when KONTRA_ORCHESTRATOR_URL is unset (the default dev posture);
// transport failures are swallowed (the orchestrator may not be up yet).
func SelfRegister(reg *core.Registry) {
	base := os.Getenv("KONTRA_ORCHESTRATOR_URL")
	if base == "" {
		return
	}
	body, err := json.Marshal(Describe(reg))
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/api/actors", bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if resp, err := http.DefaultClient.Do(req); err == nil {
		_ = resp.Body.Close()
	}
}
