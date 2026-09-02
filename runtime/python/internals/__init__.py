"""Kontra's Python RUNTIME — the engine, the codec, the state tiers and the Temporal hosts.

Not the author surface. Authors import `actorkit` (actor, param, SessionLost, ask, speak, …),
which lives in `sdk/python` and is a different seam on purpose. These modules are implementation
detail and may change without notice.

THE ARROW POINTS THIS WAY AND ONLY THIS WAY: `runtime/` imports `sdk/`, `sdk/` imports nothing of
`runtime/` (ADR 0035 §2). So `internals.engine` importing `actorkit.batch` for Unit/Batch/Dataset,
or `internals.catalog` importing `actorkit.schema` to derive an operation's schemas, is the
NORMAL direction — the vocabulary an author's own Method signature names belongs to the author,
and the engine is what happens to construct it. The reverse is a build failure:
`tests/test_sdk_arrow.py` walks every module under `sdk/python/actorkit`, refuses a module-scope
import of this package, and then re-imports the whole surface in a fresh interpreter with this
package made unimportable.

The single exception is the ENTRY-POINT HANDOFF, and it is spelled as a deferred import on the
other side: `actorkit.actor.Actor.serve` reaches `internals.temporal.host.serve`, and
`actorkit.catalog.serve` reaches `internals.temporal.wfhost.serve_workflows`, each inside the
function body. `import actorkit` therefore costs no runtime module, no temporalio, no Redis and no
object store — measured at 157 sys.modules against 448 once a workflow verb is named.

The Go peer of this package is `runtime/go`, whose author surface is `sdk/go`.
"""
