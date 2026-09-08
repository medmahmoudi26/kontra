package kontra

// THE ENTRY-POINT HANDOFF, AND WHY IT IS A REGISTRATION AND NOT AN IMPORT.
//
// Serve() is by definition where an author stops writing code and hands control to the engine —
// the one place the author surface has any business naming the runtime at all. Python spells that
// handoff as a DEFERRED import (`from internals.temporal.host import serve`, inside the function),
// which costs the arrow nothing: `import kontra` never reaches the runtime.
//
// Go has no deferred import. A `runtime/go/temporalhost` in this file's import block would be a
// module-graph edge sdk -> runtime, and since runtime/go must import sdk/go/core (Unit, Batch,
// Dataset are the author's vocabulary, ADR 0028), that edge closes a cycle. So the handoff is
// inverted into the driver pattern Go's own database/sql uses: the SDK declares the SEAM (Host),
// the runtime fills it from init(), and the author's main package links it with a blank import:
//
//	import (
//		kontra "github.com/medmahmoudi26/kontra/sdk/go"
//		_ "github.com/medmahmoudi26/kontra/runtime/go"   // links the actor host
//	)
//
// The result is asserted, not asserted-in-prose: `go list -deps ./sdk/go/...` names no package
// under runtime/, no redis client and no object-store client (arrow_test.go).

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/medmahmoudi26/kontra/sdk/go/core"
)

// schemaVersion is the actor.json schema tag this SDK accepts (kept local so the author surface
// imports only sdk/go/core, not a shared identity package).
const schemaVersion = "kontra.actor.v1"

// host is the runtime Serve() hands control to, registered by Host(). Nil until a runtime is
// linked, which is a state Serve() reports rather than panics on.
var host func(*core.Registry) error

// Host registers the actor runtime that Serve() hands control to. The runtime calls it from its
// own init(); an author never does — an author writes the blank import, which is what makes the
// init run. It is the peer of sql.Register, and it exists so this package can name the handoff
// without importing the thing on the other side of it (see the package comment).
func Host(fn func(*core.Registry) error) { host = fn }

// Serve serves this actor as a Temporal activity worker (ADR 0018). THE way to start a Go actor —
// call it from main() and start the binary; it polls `{name}-{version}-sessions` directly, with
// no sidecar to launch it under and no port to serve. A Go actor is run by Go; there is no CLI
// to start one. Identity (name/version) is read from the actor.json beside the program, and the
// controller endpoints come from env (KONTRA_ADDRESS, KONTRA_REDIS_HOST, KONTRA_S3_ENDPOINT).
// Blocks until the process is stopped.
//
// SERVE, not Run. This call does not execute your actor's code — it boots a worker and blocks
// forever waiting to be given a Batch. `run` names the CALLER's direction: it reads like the verb
// for "make this actor do the work", which is what a caller wants, and the caller's verb lives in
// Python (`workflows.actor(...).dispatch(units)`), because Go implements the callee half only
// (ADR 0023 §22). Python's actor.serve() carries the same correction and the same reasoning;
// Prefect draws the line in the same place with flow.serve().
func (a *Actor) Serve() {
	if err := a.resolveIdentity(); err != nil {
		fmt.Fprintf(os.Stderr, "[kontra] %v\n", err)
		os.Exit(1)
	}
	if a.reg.LoadFn == nil && len(a.reg.Methods) == 0 {
		fmt.Fprintln(os.Stderr, "[kontra] actor registered no Load / Method")
		os.Exit(1)
	}
	if host == nil {
		fmt.Fprintln(os.Stderr, "[kontra] no actor runtime is linked into this binary; add\n"+
			"\t_ \"github.com/medmahmoudi26/kontra/runtime/go\"\n"+
			"to your main package's imports (and the module to go.mod)")
		os.Exit(1)
	}
	if err := host(&a.reg); err != nil {
		fmt.Fprintf(os.Stderr, "[kontra] %v\n", err)
		os.Exit(1)
	}
}

// resolveIdentity reads name/version from the actor.json beside the program — the working
// directory for `go run .`, else the compiled binary's own directory (a per-actor image
// lays the binary + actor.json side by side under /actor/<name>).
func (a *Actor) resolveIdentity() error {
	var dirs []string
	if wd, err := os.Getwd(); err == nil {
		dirs = append(dirs, wd)
	}
	if exe, err := os.Executable(); err == nil {
		dirs = append(dirs, filepath.Dir(exe))
	}
	for _, d := range dirs {
		raw, err := os.ReadFile(filepath.Join(d, "actor.json"))
		if err != nil {
			continue
		}
		var m struct {
			SchemaVersion string `json:"schemaVersion"`
			Name          string `json:"name"`
			Version       string `json:"version"`
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			return fmt.Errorf("invalid actor.json in %s: %w", d, err)
		}
		if m.SchemaVersion != "" && m.SchemaVersion != schemaVersion {
			return fmt.Errorf("unsupported actor.json schemaVersion %q, expected %q", m.SchemaVersion, schemaVersion)
		}
		if m.Name == "" || m.Version == "" {
			return fmt.Errorf("actor.json in %s missing required name/version", d)
		}
		a.reg.Name, a.reg.Version = m.Name, m.Version
		return nil
	}
	return fmt.Errorf("no actor.json found beside the program (looked in %v)", dirs)
}
