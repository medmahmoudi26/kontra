/**
 * Persistence for the orchestrator backend: the actor catalog and saved designs.
 *
 * It holds NO run records. A **Run** is one execution of a caller's workflow, identified by that
 * workflow's id (ADR 0023 §12) — Temporal already holds it, so a row here could only be a copy
 * that drifts. The `runs` table existed because the SERVER minted the id and had to persist it
 * before starting the interpreter; there is no interpreter and no server-minted id.
 *
 * Backed by SQLite via Node's built-in `node:sqlite` (`DatabaseSync`, Node 22.5+), so
 * there is NO native/third-party dependency — the engine ships with the runtime. Every
 * mutation runs in its own transaction, so a crash can't leave a half-written store the
 * way the old "rewrite the whole JSON file on every change" design could, and writes
 * touch only the changed row instead of serializing the entire dataset.
 *
 * Pass `':memory:'` (the default) for an ephemeral store (tests); any other path is an on-disk
 * SQLite file.
 *
 * Nested blobs (an actor's `operations`, a graph's opaque `document`) are stored as JSON
 * text columns — the store never queries into them, so a column-per-field schema would be
 * bespoke overhead for no gain.
 */

import { randomUUID } from 'node:crypto';
import { createRequire } from 'node:module';
import type { DatabaseSync } from 'node:sqlite';

import type { JsonSchemaDoc } from '../../contract/types';
import type { Incompatibility } from '../compat';
import { parseScratchDocument, type ScratchDocument } from '../scratch';

// `node:sqlite` is loaded via a runtime require rather than a static `import`: the Vite
// bundler vitest runs under doesn't yet list it as a Node builtin and tries (and fails)
// to bundle it. `createRequire` keeps it a genuine runtime load the bundler leaves alone;
// the `import type` above still gives full type-checking. ponytail: drop this indirection
// for a plain `import` once the toolchain's builtin list includes `node:sqlite`.
const { DatabaseSync: DatabaseSyncCtor } = createRequire(__filename)('node:sqlite') as typeof import('node:sqlite');

/** One actor operation with its resolved I/O JSON Schemas (the persisted form of the
 *  frontend's ActorOperation). Schemas are optional — a partial catalog is tolerated. */
export interface ActorOperation {
  name: string;
  /** What this Method is FOR, in the author's own words — a Python docstring's first paragraph or
   *  a Go `Does("…")`. Absent when the author wrote none; see the frontend's ActorOperation. */
  description?: string;
  params?: JsonSchemaDoc;
  input?: JsonSchemaDoc;
  output?: JsonSchemaDoc;
}

/** A catalogued actor (the persisted form of the frontend's CatalogActor). */
export interface ActorRecord {
  key: string;
  name: string;
  version: string;
  schemaVersion: string;
  operations: ActorOperation[];
  /**
   * Content-pinned identity (ADR 0011): the OCI image digest registered for this
   * actor. Optional — a hand-fed/dev catalog leaves it unset (pinning is off). The
   * worker verifies its running digest against the one POST /api/runs stamps from here.
   */
  digest?: string;
  /**
   * The directory the WORKER loaded this actor from.
   *
   * Nothing linked a registered actor back to its source: `.kontra/actors/` is where an operator's
   * own actors go and is usually empty, so a catalog of twenty-three actors offered no way to
   * reach any of their code. On a fleet Machine this is `/opt/kontra/actor/<name>` rather than a
   * path on the reader's machine — still the honest answer to "where did this come from".
   */
  source?: string;
  /**
   * What registering this version said about the version before it (`src/compat.ts`): a new
   * required input field, a removed output field — named per Method and per direction.
   *
   * STORED, not recomputed on read. It is a statement about the catalog as it was AT REGISTRATION:
   * it names the version it was compared against, and that row can be deleted, or a version can
   * arrive out of order, afterwards. Recomputing would make the Actors page stop saying a version
   * broke a caller the moment the older row was deleted — deleting the evidence is the last thing
   * that should erase the finding. (A re-registration of THIS version re-makes it, which is the one
   * event that legitimately restates it.) And it has to survive a restart, because the whole point
   * is that nobody finds out by having the shape break inside a run.
   *
   * ABSENT when nothing was reported, and absent is NOT a claim of compatibility. A first version,
   * a Method new in this one and a schema that declares no properties are all "nothing to compare";
   * `compat.ts` keeps those as `unknown` verdicts and only breaks are written here.
   *
   * THE CATALOG'S OWN FACT, like `savedAt`: a worker declares its schemas and cannot declare
   * whether they break its predecessor, so this is not a descriptor field on the wire and
   * `catalog.contract.ts` exempts it by name.
   */
  incompatibilities?: Incompatibility[];
  /**
   * What the image was BUILT on: the Runtime by name and major, pinned to the digest the build
   * resolved. ABSENT when unknown, and absent is not "no runtime" — it is an actor built before this
   * field existed, or one whose worker was started without the environment that carries it.
   *
   * Not a fact the worker discovers. Nothing inside a container can see the run image it was layered
   * onto, so the deploying CLI records it and the Warden hands it back; the registrar echoes it so a
   * re-registration preserves it. Stored as JSON text, like `operations`, because it is a structure.
   *
   * `major` beside `digest` because they answer different questions: the major is what the author
   * asked for and the digest is whether it is still current — which is the whole of rebase detection.
   */
  runtime?: ActorRuntimeRecord;
  /** The CNB builder's digest, from the same stamp. Absent when unknown, for the same reason. */
  builderDigest?: string;
  savedAt: number;
}

/** The Runtime an Actor image was layered onto, as the catalog keeps it. */
export interface ActorRuntimeRecord {
  name: string;
  major: number;
  digest: string;
}

/**
 * A CALLER WORKFLOW as the catalog stores it — `WorkflowDescriptor` in
 * `shared/contracts/kontra/v1/catalog.proto`, field for field, plus the store's own `savedAt`.
 *
 * KEYED BY THE TYPE, not by a file. `name` is the `@workflow.defn` type a caller starts
 * (`NsCheck`), which is what Temporal routes on and what `kontra workflow start` names; two
 * workflows can live in one file and one workflow can be served from anywhere. A row keyed by
 * filename would be a row about this host's disk, and the worker that registered it may be
 * serving code from another machine entirely.
 *
 * `description`, `input` and `output` are ABSENT when the author declared none — not '' and not
 * `{}`. "This workflow says nothing about its input" and "this workflow takes an object with no
 * fields" are different facts, and the Workflows page draws them differently.
 */
export interface WorkflowRecord {
  name: string;
  /** First paragraph of the class docstring. Absent when the author wrote none. */
  description?: string;
  /** The task queue the registering worker is polling — the address half of starting this. */
  queue?: string;
  /** The import error, when watch mode caught one. Absent means the file imports (the ordinary
   *  case) and the reader draws the contract; present means it does not, and the reader draws the
   *  error instead of a stale form. A broken descriptor carries no input/output, so an overwrite
   *  clears those. */
  error?: string;
  /** JSON Schema of the run method's argument, or absent when it is unannotated. */
  input?: JsonSchemaDoc;
  /** JSON Schema of its return, or absent. */
  output?: JsonSchemaDoc;
  savedAt: number;
}

interface WorkflowRow {
  name: string;
  description: string | null;
  queue: string | null;
  error: string | null;
  input: string | null;
  output: string | null;
  saved_at: number;
}

/** A saved design — the opaque editor document (nodes+positions, field links,
 *  catalog, runId) plus a name and id. Opaque here: the UI owns its shape. */
export interface GraphRecord {
  id: string;
  name: string;
  document: unknown;
  updatedAt: number;
}

/**
 * A saved **Scratch** — a drawing of an orchestration that an agent reads back to write code from.
 *
 * Unlike a graph's `document`, which this store treats as opaque, this is a KNOWN shape
 * (`src/scratch.ts`). That is the whole point of the feature: something other than the editor reads
 * it, so the editor cannot be the only thing that knows what it means.
 */
export interface ScratchRecord {
  id: string;
  name: string;
  document: ScratchDocument;
  updatedAt: number;
}

/** Raw column shape of the `actors` table (SQLite returns snake_case rows). */
interface ActorRow {
  key: string;
  name: string;
  version: string;
  schema_version: string;
  operations: string;
  digest: string | null;
  source: string | null;
  incompatibilities: string | null;
  runtime: string | null;
  builder_digest: string | null;
  saved_at: number;
}

interface GraphRow {
  id: string;
  name: string;
  document: string;
  updated_at: number;
}

/** A registered folder. Structurally `Source` from ../sources, restated to keep db/ standalone. */
export interface SourceRecord {
  id: string;
  kind: string;
  name: string;
  path: string;
  version: string;
  description: string;
  registeredAt: number;
  /** `sha256:<hex>` over the folder's files AT REGISTRATION — see ../sources.ts:folderDigest. */
  digest?: string;
  /** The manifest as it was, verbatim JSON. A record rather than `unknown` because every caller
   *  reads fields off it and `unknown` would make each one cast; the values stay `unknown` because
   *  this layer does not get to have an opinion about what an author may put in `actor.json`. */
  manifest?: Record<string, unknown>;
  /** The Nexus endpoint this registration created and owns. */
  endpoint?: string;
}

interface SourceRow {
  id: string;
  kind: string;
  name: string;
  path: string;
  version: string;
  description: string;
  registered_at: number;
  digest: string | null;
  manifest: string | null;
  endpoint: string | null;
}

export class Repo {
  private readonly db: DatabaseSync;

  constructor(path = ':memory:') {
    this.db = new DatabaseSyncCtor(path);
    // WAL keeps readers unblocked and shrinks the crash-corruption window for the
    // on-disk case; it's rejected on an in-memory db, so guard on the path.
    if (path !== ':memory:') this.db.exec('PRAGMA journal_mode = WAL;');
    this.db.exec(SCHEMA);
    this.migrate();
  }

  /**
   * Columns added after this table already existed somewhere.
   *
   * `CREATE TABLE IF NOT EXISTS` is a no-op against a database that HAS the table, so a new column
   * in `SCHEMA` reaches a fresh install and never an existing one — and the failure is a `SELECT *`
   * that returns rows without it, which reads as "no actor has a source" rather than as a
   * migration nobody ran.
   *
   * Additive and idempotent: `PRAGMA table_info` is asked rather than an error caught, because
   * catching one from `ADD COLUMN` means also catching the ones that matter.
   */
  private migrate(): void {
    const columns = (
      this.db.prepare('PRAGMA table_info(actors)').all() as unknown as Array<{ name: string }>
    ).map((c) => c.name);
    if (!columns.includes('source')) this.db.exec('ALTER TABLE actors ADD COLUMN source TEXT');
    // The build facts. Same reason as every column below: an installation that has ever registered
    // an actor is the "table already exists" case, so without these the columns reach only a fresh
    // database and every existing catalog answers `SELECT *` without them — which reads as "no actor
    // was ever built on a runtime", and rebase detection would then find nothing to do, forever.
    if (!columns.includes('runtime')) this.db.exec('ALTER TABLE actors ADD COLUMN runtime TEXT');
    if (!columns.includes('builder_digest')) {
      this.db.exec('ALTER TABLE actors ADD COLUMN builder_digest TEXT');
    }
    // The cross-version finding (src/compat.ts). Every installation that has ever registered an
    // actor is the "table already exists" case, so without this line the column reaches only a
    // fresh database and every existing catalog answers `SELECT *` without it — which reads as
    // "no version has ever broken a caller".
    if (!columns.includes('incompatibilities')) {
      this.db.exec('ALTER TABLE actors ADD COLUMN incompatibilities TEXT');
    }
    /* REGISTRATION BECAME ITS OWN ACT, and these three columns are what it records beyond the path:
       what the folder held (`digest`), what it declared (`manifest`), and the Nexus endpoint the
       registration created and therefore owns (`endpoint`). Same reasoning as the two above — every
       installation that has ever registered a folder is the "table already exists" case, so without
       this the columns reach only a fresh database and every existing row answers `SELECT *`
       without them, which reads as "nothing has ever been registered with a digest". */
    const sourceColumns = (
      this.db.prepare('PRAGMA table_info(sources)').all() as unknown as Array<{ name: string }>
    ).map((c) => c.name);
    if (!sourceColumns.includes('digest')) this.db.exec('ALTER TABLE sources ADD COLUMN digest TEXT');
    if (!sourceColumns.includes('manifest')) this.db.exec('ALTER TABLE sources ADD COLUMN manifest TEXT');
    if (!sourceColumns.includes('endpoint')) this.db.exec('ALTER TABLE sources ADD COLUMN endpoint TEXT');
    /* The task queue a workflow's worker registered itself on. Same migration reasoning as above:
       every installation that has ever served a workflow already HAS this table, so the column in
       SCHEMA reaches a fresh database and nothing else — and the symptom would be a Run button
       that never learns which queue to prefill, which is the bug this column exists to fix. */
    const workflowColumns = (
      this.db.prepare('PRAGMA table_info(workflows)').all() as unknown as Array<{ name: string }>
    ).map((c) => c.name);
    if (!workflowColumns.includes('queue')) this.db.exec('ALTER TABLE workflows ADD COLUMN queue TEXT');
    /* The import error watch mode posts when a save breaks the served file (instrument-panel slice
       03). Same migration reasoning as `queue` above: every installation that has ever served a
       workflow already HAS this table, so the column in SCHEMA reaches a fresh database and nothing
       else — and the symptom would be a broken-file post that 500s on an unknown column, taking down
       the very liveness signal the column exists to carry. */
    if (!workflowColumns.includes('error')) this.db.exec('ALTER TABLE workflows ADD COLUMN error TEXT');
  }

  /** Run `fn` inside a single transaction so a mutation is all-or-nothing. */
  private tx<T>(fn: () => T): T {
    this.db.exec('BEGIN');
    try {
      const result = fn();
      this.db.exec('COMMIT');
      return result;
    } catch (err) {
      this.db.exec('ROLLBACK');
      throw err;
    }
  }

  // --- actors ---

  listActors(): ActorRecord[] {
    const rows = this.db.prepare('SELECT * FROM actors ORDER BY key').all() as unknown as ActorRow[];
    return rows.map(rowToActor);
  }

  getActor(key: string): ActorRecord | undefined {
    const row = this.db.prepare('SELECT * FROM actors WHERE key = ?').get(key) as unknown as ActorRow | undefined;
    return row ? rowToActor(row) : undefined;
  }

  /**
   * Write a descriptor. It is NOT screened here: `src/catalog.ts` does that at
   * `POST /api/actors`, which is the only caller and the only place that can answer a refusal
   * with a status. A direct caller of this method therefore writes whatever it hands over —
   * including a schema change ADR 0004 forbids — so new write paths go through the route or
   * through `parseDescriptor`/`refuseSchemaChange` first.
   */
  upsertActor(a: Omit<ActorRecord, 'savedAt'>): ActorRecord {
    return this.tx(() => {
      const prev = this.getActor(a.key);
      const rec: ActorRecord = {
        ...a,
        // Preserve a worker-registered digest (ADR 0011) when the design-tool upload omits
        // one, so the two registration paths (schemas vs digest) don't clobber each other.
        digest: a.digest ?? prev?.digest,
        savedAt: Date.now(),
      };
      this.writeActor(rec);
      return rec;
    });
  }

  /**
   * Upsert ONLY the image digest (ADR 0011 worker self-registration), preserving any
   * operations/schemas already catalogued. Creates a minimal record if the actor is new
   * (the design-tool upload later fills operations, keeping this digest via upsertActor).
   */
  setActorDigest(input: { key: string; name: string; version: string; digest: string }): ActorRecord {
    return this.tx(() => {
      const prev = this.getActor(input.key);
      const rec: ActorRecord = prev
        ? { ...prev, digest: input.digest, savedAt: Date.now() }
        : {
            key: input.key,
            name: input.name,
            version: input.version,
            schemaVersion: 'kontra.actor.v1',
            operations: [],
            digest: input.digest,
            savedAt: Date.now(),
          };
      this.writeActor(rec);
      return rec;
    });
  }

  deleteActor(key: string): boolean {
    return this.tx(() => Number(this.db.prepare('DELETE FROM actors WHERE key = ?').run(key).changes) > 0);
  }

  private writeActor(rec: ActorRecord): void {
    this.db
      .prepare(
        `INSERT INTO actors (key, name, version, schema_version, operations, digest, source, incompatibilities, runtime, builder_digest, saved_at)
         VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
         ON CONFLICT(key) DO UPDATE SET
           name = excluded.name,
           version = excluded.version,
           schema_version = excluded.schema_version,
           operations = excluded.operations,
           digest = excluded.digest,
           source = excluded.source,
           incompatibilities = excluded.incompatibilities,
           runtime = excluded.runtime,
           builder_digest = excluded.builder_digest,
           saved_at = excluded.saved_at`
      )
      .run(
        rec.key,
        rec.name,
        rec.version,
        rec.schemaVersion,
        JSON.stringify(rec.operations),
        rec.digest ?? null,
        rec.source ?? null,
        // NULL for nothing reported rather than `[]`: the row says "no finding", which is what the
        // reader has to be able to tell apart from "compared and compatible".
        rec.incompatibilities && rec.incompatibilities.length > 0
          ? JSON.stringify(rec.incompatibilities)
          : null,
        rec.runtime ? JSON.stringify(rec.runtime) : null,
        rec.builderDigest ?? null,
        rec.savedAt
      );
  }

  // --- workflows (the caller's half of a run) ---
  //
  // ITS OWN TABLE, and not a row in `actors`. An actor is identified by `(name, version)` and its
  // version is immutable (ADR 0004); a workflow has no version at all — the worker serving it is
  // the operator's own process over the operator's own file — so folding one into the other would
  // put a row with no version into the table whose primary key is built from one.

  listWorkflows(): WorkflowRecord[] {
    const rows = this.db
      .prepare('SELECT * FROM workflows ORDER BY name')
      .all() as unknown as WorkflowRow[];
    return rows.map(rowToWorkflow);
  }

  getWorkflow(name: string): WorkflowRecord | undefined {
    const row = this.db.prepare('SELECT * FROM workflows WHERE name = ?').get(name) as unknown as
      | WorkflowRow
      | undefined;
    return row ? rowToWorkflow(row) : undefined;
  }

  /**
   * Write a descriptor a worker pushed on serve.
   *
   * IT IS NOT SCREENED HERE: `src/catalog.ts:parseWorkflowDescriptor` does that at
   * `POST /api/workflows/catalog`, which is the only caller and the only place that can answer a
   * refusal with a status. A direct caller of this method writes whatever it hands over.
   *
   * A RE-REGISTRATION OVERWRITES, unlike an actor's. There is no version to bump: re-serving an
   * edited workflow is the ordinary way to change one, and the descriptor that must win is the
   * one the running worker just derived from the code it imported.
   */
  upsertWorkflow(w: Omit<WorkflowRecord, 'savedAt'>): WorkflowRecord {
    return this.tx(() => {
      const rec: WorkflowRecord = { ...w, savedAt: Date.now() };
      this.db
        .prepare(
          `INSERT INTO workflows (name, description, queue, error, input, output, saved_at)
           VALUES (?, ?, ?, ?, ?, ?, ?)
           ON CONFLICT(name) DO UPDATE SET
             description = excluded.description,
             queue = excluded.queue,
             error = excluded.error,
             input = excluded.input,
             output = excluded.output,
             saved_at = excluded.saved_at`
        )
        // NULL, not '' or '{}', for what the author declared nothing about — see WorkflowRecord.
        // `error` overwrites like the rest: a broken descriptor clears input/output to NULL and sets
        // error, and the clean descriptor that recovers it clears error and re-sets the schemas.
        .run(
          rec.name,
          rec.description ?? null,
          rec.queue ?? null,
          rec.error ?? null,
          rec.input === undefined ? null : JSON.stringify(rec.input),
          rec.output === undefined ? null : JSON.stringify(rec.output),
          rec.savedAt
        );
      return rec;
    });
  }

  deleteWorkflow(name: string): boolean {
    return this.tx(
      () => Number(this.db.prepare('DELETE FROM workflows WHERE name = ?').run(name).changes) > 0
    );
  }

  // --- graphs ---

  /** Graph metadata (id, name, updatedAt) — without the document, for listing. */
  listGraphs(): Array<Omit<GraphRecord, 'document'>> {
    const rows = this.db
      .prepare('SELECT id, name, updated_at FROM graphs ORDER BY updated_at DESC')
      .all() as unknown as Array<Omit<GraphRow, 'document'>>;
    return rows.map((r) => ({ id: r.id, name: r.name, updatedAt: r.updated_at }));
  }

  getGraph(id: string): GraphRecord | undefined {
    const row = this.db.prepare('SELECT * FROM graphs WHERE id = ?').get(id) as unknown as GraphRow | undefined;
    return row ? rowToGraph(row) : undefined;
  }

  saveGraph(input: { id?: string; name: string; document: unknown }): GraphRecord {
    return this.tx(() => {
      const id = input.id ?? randomUUID();
      const rec: GraphRecord = { id, name: input.name, document: input.document, updatedAt: Date.now() };
      this.db
        .prepare(
          `INSERT INTO graphs (id, name, document, updated_at)
           VALUES (?, ?, ?, ?)
           ON CONFLICT(id) DO UPDATE SET
             name = excluded.name,
             document = excluded.document,
             updated_at = excluded.updated_at`
        )
        .run(rec.id, rec.name, JSON.stringify(rec.document), rec.updatedAt);
      return rec;
    });
  }

  deleteGraph(id: string): boolean {
    return this.tx(() => Number(this.db.prepare('DELETE FROM graphs WHERE id = ?').run(id).changes) > 0);
  }

  // --- scratches ---
  //
  // ITS OWN TABLE, and not the `graphs` one it resembles. `graphs` holds the retired canvas's
  // opaque documents, and the CLI console still lists and DISPATCHES them (`cli/console.go`,
  // `cli/dispatch.go`) — so folding a Scratch in would put a document the console cannot dispatch
  // into a list it offers to dispatch. Two things that are both "a saved drawing" are still two
  // things when one of them runs.

  listScratches(): Array<Omit<ScratchRecord, 'document'>> {
    const rows = this.db
      .prepare('SELECT id, name, updated_at FROM scratches ORDER BY updated_at DESC')
      .all() as unknown as Array<Omit<GraphRow, 'document'>>;
    return rows.map((r) => ({ id: r.id, name: r.name, updatedAt: r.updated_at }));
  }

  getScratch(id: string): ScratchRecord | undefined {
    const row = this.db.prepare('SELECT * FROM scratches WHERE id = ?').get(id) as unknown as
      | GraphRow
      | undefined;
    if (!row) return undefined;
    return {
      id: row.id,
      name: row.name,
      document: parseScratchDocument(JSON.parse(row.document)),
      updatedAt: row.updated_at,
    };
  }

  saveScratch(input: { id?: string; name: string; document: unknown }): ScratchRecord {
    return this.tx(() => {
      // NARROWED ON THE WAY IN, not on the way out. What is stored is what this build understands,
      // so a document read back is one every reader — the canvas, the spec, an agent — sees the
      // same way, and a node of an unknown kind cannot sit in the store waiting to be misread.
      const document = parseScratchDocument(input.document);
      const rec: ScratchRecord = {
        id: input.id ?? randomUUID(),
        name: input.name,
        document,
        updatedAt: Date.now(),
      };
      this.db
        .prepare(
          `INSERT INTO scratches (id, name, document, updated_at)
           VALUES (?, ?, ?, ?)
           ON CONFLICT(id) DO UPDATE SET
             name = excluded.name,
             document = excluded.document,
             updated_at = excluded.updated_at`
        )
        .run(rec.id, rec.name, JSON.stringify(rec.document), rec.updatedAt);
      return rec;
    });
  }

  deleteScratch(id: string): boolean {
    return this.tx(
      () => Number(this.db.prepare('DELETE FROM scratches WHERE id = ?').run(id).changes) > 0
    );
  }

  // --- registered folders (src/sources.ts) ---

  listSources(kind: string): SourceRecord[] {
    const rows = this.db
      .prepare('SELECT * FROM sources WHERE kind = ? ORDER BY name')
      .all(kind) as unknown as SourceRow[];
    return rows.map(rowToSource);
  }

  saveSource(s: SourceRecord): SourceRecord {
    return this.tx(() => {
      this.db
        .prepare(
          `INSERT INTO sources (id, kind, name, path, version, description, registered_at,
                                digest, manifest, endpoint)
           VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
           ON CONFLICT(kind, path) DO UPDATE SET
             name = excluded.name, version = excluded.version, description = excluded.description,
             digest = excluded.digest, manifest = excluded.manifest, endpoint = excluded.endpoint`
        )
        .run(
          s.id,
          s.kind,
          s.name,
          s.path,
          s.version,
          s.description,
          s.registeredAt,
          s.digest ?? null,
          // `undefined` and JSON `null` are different answers — "never recorded" versus "recorded as
          // nothing" — and only the first is what an un-migrated row means.
          s.manifest === undefined ? null : JSON.stringify(s.manifest),
          s.endpoint ?? null
        );
      return s;
    });
  }

  deleteSource(id: string): boolean {
    return this.tx(
      () => Number(this.db.prepare('DELETE FROM sources WHERE id = ?').run(id).changes) > 0
    );
  }
}

const SCHEMA = `
CREATE TABLE IF NOT EXISTS actors (
  key            TEXT PRIMARY KEY,
  name           TEXT NOT NULL,
  version        TEXT NOT NULL,
  schema_version TEXT NOT NULL,
  operations     TEXT NOT NULL,
  digest         TEXT,
  -- Where the WORKER loaded this actor from. See ActorRecord.source; added by migrate().
  source         TEXT,
  -- What registering this version said about the one before it (src/compat.ts), as JSON; NULL when
  -- nothing was reported. See ActorRecord.incompatibilities; added by migrate().
  incompatibilities TEXT,
  -- What the image was BUILT on, as JSON; NULL when unknown. See ActorRecord.runtime; added by
  -- migrate().
  runtime        TEXT,
  builder_digest TEXT,
  saved_at       INTEGER NOT NULL
);
-- A caller workflow as its worker described it on serve (shared/contracts/kontra/v1/catalog.proto's
-- WorkflowDescriptor). Keyed by the @workflow.defn TYPE — see WorkflowRecord for why that is not
-- the filename. The schemas are JSON text and NULL when the author declared none.
CREATE TABLE IF NOT EXISTS workflows (
  name        TEXT PRIMARY KEY,
  description TEXT,
  -- The queue the worker that registered this was about to poll. Nullable: an older SDK sends
  -- none, and "no worker has said" is a different answer from "the empty queue". See migrate().
  queue       TEXT,
  -- The import error when watch mode caught one; NULL when the file imports. See migrate().
  error       TEXT,
  input       TEXT,
  output      TEXT,
  saved_at    INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS graphs (
  id         TEXT PRIMARY KEY,
  name       TEXT NOT NULL,
  document   TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);
-- A drawing of an orchestration, read back by an agent. Separate from graphs: see listScratches.
CREATE TABLE IF NOT EXISTS scratches (
  id         TEXT PRIMARY KEY,
  name       TEXT NOT NULL,
  document   TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);
-- A folder an operator registered as holding an Actor's or a Workflow's code. See src/sources.ts.
-- Only the PATH and the identity read off it at registration are stored: name, version and
-- description are re-read from disk on every listing, so editing description.md is enough.
CREATE TABLE IF NOT EXISTS sources (
  id            TEXT PRIMARY KEY,
  kind          TEXT NOT NULL,
  name          TEXT NOT NULL,
  path          TEXT NOT NULL,
  version       TEXT NOT NULL,
  description   TEXT NOT NULL,
  registered_at INTEGER NOT NULL,
  -- What the folder HELD and DECLARED when it was registered, and the Nexus endpoint that
  -- registering it created. Unlike the three above these are NOT re-read on listing: the point of
  -- them is to say what was true then, so that comparing it with now shows a divergence.
  -- Nullable because every row written before this existed has no answer, which is not the same
  -- as an empty one. See migrate().
  digest        TEXT,
  manifest      TEXT,
  endpoint      TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS sources_path ON sources (kind, path);
`;

function rowToActor(row: ActorRow): ActorRecord {
  const rec: ActorRecord = {
    key: row.key,
    name: row.name,
    version: row.version,
    schemaVersion: row.schema_version,
    operations: JSON.parse(row.operations) as ActorOperation[],
    savedAt: row.saved_at,
  };
  // Keep `digest` absent (not `undefined`-valued) when unset, matching the old shape so
  // callers that check `'digest' in rec` / structural-equal against a no-digest object hold.
  if (row.digest !== null) rec.digest = row.digest;
  // `?? null` rather than `!== null`: a database migrated a moment ago has the column and no
  // value, and `undefined` from a row that predates it must read the same as an explicit NULL.
  if ((row.source ?? null) !== null) rec.source = row.source as string;
  // ABSENT rather than `[]` for a row with no finding — the conformance fixture compares what the
  // catalog hands back to what the SDKs posted, key for key, and an empty array would be the store
  // adding a key that says "checked, and fine" about a version nothing was compared against.
  if ((row.incompatibilities ?? null) !== null) {
    rec.incompatibilities = JSON.parse(row.incompatibilities as string) as Incompatibility[];
  }
  if ((row.runtime ?? null) !== null) {
    rec.runtime = JSON.parse(row.runtime as string) as ActorRuntimeRecord;
  }
  if ((row.builder_digest ?? null) !== null) rec.builderDigest = row.builder_digest as string;
  return rec;
}

/**
 * ABSENT, not `undefined`-valued and not '', for a column the worker said nothing about.
 *
 * The same rule `rowToActor` follows, and for the same reader: a descriptor read back is compared
 * key for key against what was posted, and a row that answers `description: undefined` for a
 * workflow whose author wrote no docstring has grown a key that says "described, with nothing".
 */
function rowToWorkflow(row: WorkflowRow): WorkflowRecord {
  const rec: WorkflowRecord = { name: row.name, savedAt: row.saved_at };
  if ((row.description ?? null) !== null) rec.description = row.description as string;
  if (row.queue) rec.queue = row.queue;
  // Truthy, like `queue`: NULL and '' both mean "the file imports", and only a real message is a
  // broken state the page should draw.
  if (row.error) rec.error = row.error;
  if ((row.input ?? null) !== null) rec.input = JSON.parse(row.input as string) as JsonSchemaDoc;
  if ((row.output ?? null) !== null) rec.output = JSON.parse(row.output as string) as JsonSchemaDoc;
  return rec;
}

function rowToGraph(row: GraphRow): GraphRecord {
  return { id: row.id, name: row.name, document: JSON.parse(row.document) as unknown, updatedAt: row.updated_at };
}

function rowToSource(row: SourceRow): SourceRecord {
  const rec: SourceRecord = {
    id: row.id,
    kind: row.kind,
    name: row.name,
    path: row.path,
    version: row.version,
    description: row.description,
    registeredAt: row.registered_at,
  };
  // ABSENT STAYS ABSENT. A row written before the migration has NULL in all three, and the record
  // must not carry `digest: null` — the surfaces branch on "was this recorded", and null is a value.
  if (row.digest) rec.digest = row.digest;
  if (row.endpoint) rec.endpoint = row.endpoint;
  if (row.manifest) {
    try {
      rec.manifest = JSON.parse(row.manifest) as Record<string, unknown>;
    } catch {
      // A manifest that will not parse back is a corrupted row, not a reason to fail the listing:
      // every other field on it is still the registration, and losing the whole list would take the
      // forget button down with it.
    }
  }
  return rec;
}
