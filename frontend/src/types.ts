/**
 * UI-side types for the workflow editor. The *contract* (what gets serialized and sent
 * to the interpreter) lives in `@contract/types`; these types describe the editor's
 * working state — the in-memory actor catalog and the React Flow node/edge data.
 */

/**
 * One way a version's schema moved in a direction a caller notices — the catalog's own finding,
 * made when the version was registered (`@core/compat`).
 *
 * CARRIED FROM THE SERVER RATHER THAN RE-DECLARED, unlike the shapes below it. The card renders
 * these fields verbatim — the Method, the direction rule, the version compared against — so a
 * second copy of the shape is exactly how a card keeps drawing a `rule` the server stopped sending.
 */
import type { Incompatibility } from '@core/compat';

export type { Incompatibility };

/** A JSON Schema as the catalog carries it — opaque here; the server derives it. */
export type JsonSchema = Record<string, unknown>;

/** One operation of an actor, with its resolved JSON Schemas (parsed from the folder). */
export interface ActorOperation {
  name: string;
  /**
   * What this Method is FOR, in the author's own words — a Python docstring's first paragraph, or
   * a Go `Does("…")`.
   *
   * THE CATALOG HAD NO WORDS IN IT. A Method advertised a name and two JSON Schemas, so every
   * surface listing one could say what it TAKES and never what it DOES — and an operator composing
   * one Actor's Method into another's had nothing to compose from. The author had already written
   * the answer in a docstring; nothing carried it.
   *
   * ABSENT, NEVER EMPTY. An author who wrote nothing and an author who wrote "" are the same fact,
   * and the surfaces draw the absence as absence rather than as a blank line.
   */
  description?: string;
  params?: JsonSchema;
  input?: JsonSchema;
  output?: JsonSchema;
}

/** A catalogued actor, as served by GET /api/actors — auto-discovered from what's
 *  deployed/registered with the orchestrator (no folder upload). */
export interface CatalogActor {
  /** `${name}@${version}` — the catalog key and node→actor link. */
  key: string;
  name: string;
  version: string;
  schemaVersion: string;
  operations: ActorOperation[];
  /**
   * The actor's registered OCI image digest (ADR 0011), synced from the server catalog
   * (the worker self-registers it). Undefined until a worker has registered / the sync
   * runs. A node captures it at workflow-authoring time so a later rebuild trips the submit drift check.
   */
  digest?: string;
  /**
   * The directory the WORKER loaded this actor from.
   *
   * Nothing linked a registered actor back to its code. `.kontra/actors/` is where an operator's
   * own actors go and is usually empty — every actor in this catalog was run from `examples/` or a
   * checkout somewhere — so the Actors page could list twenty-three of them and offer no way to
   * reach any one's source.
   *
   * IT IS WHERE THE WORKER LOADED FROM, which is not always a path on the reader's machine: on a
   * fleet Machine it is `/opt/kontra/actor/<name>`. Still the honest answer to "where did this come
   * from", and better than the nothing that was there.
   */
  source?: string;
  /**
   * What registering THIS version said about the version before it (`@core/compat`).
   *
   * ABSENT means nothing was reported, which is not the same as compatible: a first version, a
   * Method new in this one and a schema that declares no fields were never compared. The card draws
   * the findings when there are some and says nothing when there are none — it must never draw a
   * "compatible" badge, because the check cannot support one.
   */
  incompatibilities?: Incompatibility[];
}

/** Data carried on a canvas node — one actor invocation being authored. */

// RFNodeData/RFEdgeData/AppNode/AppEdge went with the canvas (ADR 0023 §12): they modelled
// a graph of nodes and edges, and there is no graph. What the UI still needs from an Actor
// is its identity and its operations — one per Method it declares.
