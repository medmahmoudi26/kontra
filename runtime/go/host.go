// Package kontrahost links the Go actor runtime into a program. It is imported for its side
// effect only — the author's main package blank-imports it, and it registers the Temporal actor
// host as the implementation behind kontra.Actor.Serve():
//
//	import (
//		kontra "github.com/medmahmoudi26/kontra/sdk/go"
//		_ "github.com/medmahmoudi26/kontra/runtime/go"
//	)
//
//	func main() {
//		a := kontra.New()
//		a.Method("resolve", resolve)
//		a.Serve()
//	}
//
// WHY THIS EXISTS AT ALL — it is the Go half of the one edge that survives the sdk/runtime split.
// The arrow is runtime -> sdk and never the reverse: the engine, the codec, the state tiers and
// the Temporal hosts all import the author's vocabulary (sdk/go/core's Unit, Batch, Dataset), so
// an import the other way would close a module cycle. Python spells the same handoff as a
// deferred import inside serve(); Go cannot defer an import, so it registers instead — the
// pattern database/sql draws with its drivers, for the same reason.
//
// FORGETTING THE BLANK IMPORT IS NOT SILENT. Serve() exits with a message naming this import path
// rather than blocking on a worker that was never built.
package kontrahost

import (
	"github.com/medmahmoudi26/kontra/runtime/go/temporalhost"
	kontra "github.com/medmahmoudi26/kontra/sdk/go"
)

func init() { kontra.Host(temporalhost.Serve) }
