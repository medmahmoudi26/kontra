# 26. Scratch is a drawing that produces code, not an execution model

## Status

**Accepted.** Answers the question **0023** §12 left open when it removed the canvas. Does not
reverse it: the interpreter stays gone, a **Run** is still one execution of a caller's workflow, and
nothing in a Scratch runs. Implemented by `control/orchestrator/src/scratch.ts`, stored in its own table by
`db/repo.ts`, drawn by `web/src/panels/ScratchPage.tsx`, and read back by the `get_scratch` MCP
tool. Retires the Catalog surface.

## Context

**0023** §12 removed a three-pane graph editor — catalog | canvas | inspector — along with the
server-side interpreter that ran what it drew. The reasoning holds and is not in question: a
caller's own workflow is a better program than a graph anybody can draw, because a loop, a
condition and an early return are things a programming language already has and a node graph has to
reinvent badly.

But removing the canvas removed the only place the SHAPE of a campaign could be worked out before
committing to it. What replaced it is: open an editor and write fifty lines of Temporal. Those
fifty lines have to be right — the queue, the paging, the fan-out, the scope exits — before you can
see whether the arrangement was right, and the arrangement is the cheap thing to get wrong and the
expensive thing to discover late.

Meanwhile the catalog acquired the two facts that make an arrangement expressible at all: what each
**Method** is FOR (its author's docstring, carried through registration) and where its code lives.
Before those, a drawing of `nscheck.delegation → nscheck.ask` was a drawing of two names.

## Decision

A **Scratch** is a saved drawing whose nodes are REAL things, read back by an agent as a spec it
writes the caller workflow from.

**The nodes are catalogued, the notes are free.** A node is `actor nscheck@0.1.0 method=delegation`
— the catalog's own key plus a Method that key declares — or a workflow file, or a **Dataset** by
name with a direction. Free-form notes carry what the types cannot: "page 200 at a time", "isolate,
do not fail the run", "this fan-out is about 4x".

That split is the whole design. An agent reading a LABEL somebody typed must guess which actor,
which version, and whether the second word is a Method or a note; an agent reading a resolved node
guesses nothing, and cannot generate a dispatch naming a Method that does not exist. An agent
reading only resolved nodes, on the other hand, would generate valid code that is wrong, because
the paging and the failure policy are not in any schema.

**What does not resolve is reported, never corrected, and never refused.** Sketching an Actor that
does not exist yet is how somebody works out what to build; a canvas that refused to save it would
be a canvas nobody could think in. The spec lists them under "What does not resolve" and tells the
agent to say whether it thinks they are yet to be built or the sketch is wrong.

**One rendering, on the server.** The page draws a canvas and the MCP returns markdown. Both come
from `renderScratch`. Two derivations of "what the drawing said" would eventually disagree, and
nobody would be able to tell which was right.

**Its own table.** `graphs` holds the retired canvas's opaque documents and the CLI console still
lists and DISPATCHES them, so folding a Scratch in would put a document the console cannot dispatch
into a list it offers to dispatch.

**No Run button, ever.** The distinction from the canvas 0023 §12 removed is exactly this: that one
was the execution model, this one is input to writing the program that is.

## Consequences

The Catalog surface is gone. It listed the Actors and Workflows this installation holds, which the
Actors and Workflows surfaces each already do for their own kind, with the controls that belong to
them — it was a third place to read the same two lists, and Scratch is what those lists are for.

A Scratch can go stale against the catalog: an Actor is redeployed at a new version, a Method is
renamed, a Dataset is dropped. Nothing pins a Scratch to a moment, deliberately — it is a sketch,
and a sketch that refused to open because the world moved would be worth less than one that opens
and says what has moved. The spec resolves against the catalog at READ time, so the drift is
reported to whoever reads it next.

The document carries a point per node and nothing else of the geometry — no sizes, no z-order, no
viewport. Position is decoration, and the spec drops it entirely; a document that carried more
would be one where moving a box could change what the drawing said.

A Scratch is not versioned and has no history. Saving overwrites. If that turns out to matter, the
answer is a new record per save rather than a diff format, because what an agent reads is a whole
document and not a patch.
