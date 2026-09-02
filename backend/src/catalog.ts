/**
 * The catalog's REGISTRATION GATE — what `POST /api/actors` refuses before the store sees it.
 *
 * WHAT WENT WRONG WITHOUT IT. The route checked that three strings were present and then wrote
 * `(body.operations ?? []) as ActorOperation[]`. A TypeScript cast is erased at build time, so
 * `{"operations":[1,2,3]}` registered with a 200 and the first code to look INSIDE an operation —
 * the Actors page, a dispatch resolving a Method by name — was the first thing to find out.
 *
 * And ADR 0004 was prose only: a `version` is immutable and any input/output/params schema change
 * MUST bump it. Nothing compared a re-registration against what `(name, version)` already said, so
 * editing an actor's types and re-serving quietly overwrote the schema a designed pipeline had
 * been typed against. Nothing anywhere recorded that the shape had moved; the break surfaced
 * later, inside a run, as data that did not fit.
 *
 * THE DEV-LOOP COST IS DELIBERATE. Re-serving an edited actor without bumping now FAILS, at
 * registration, saying which Method moved. There is no force flag and no env escape — a bypass is
 * the silent overwrite again under a name that sounds responsible. ADR 0011's digest already
 * distinguishes two builds of one version; what it cannot do is make them mean the same thing to
 * a caller.
 *
 * WHAT THIS FILE DOES NOT ASK is whether a NEW version breaks somebody. Everything here compares a
 * registration to what the SAME `(name, version)` already says; `compat.ts` compares it to the
 * version BEFORE it, and does the opposite thing with the answer — it reports and never refuses,
 * because a breaking change in a new version is what versions are for. Two gates, one route, and
 * the asymmetry between them is the decision (ADR 0027).
 *
 * NOT ON THIS PATH: `POST /api/actors/:key/digest`. A worker self-registering its image digest on
 * startup declares no schemas at all, and it does so on every restart — gating that would refuse
 * a fact about an image over a contract it never states.
 *
 * AND THE CALLER'S HALF, `POST /api/workflows/catalog` (`parseWorkflowDescriptor`), which is
 * screened the same way and gated differently: there is no version to be immutable, so a
 * re-registration overwrites. See that function for why refusing it would describe code that is
 * no longer running.
 */

import Ajv2020 from 'ajv/dist/2020';
import type { ErrorObject } from 'ajv';

import type { JsonSchemaDoc } from '../contract/types';
import type { ActorOperation, ActorRecord, WorkflowRecord } from './db/repo';

/** The body is not an actor descriptor. The route answers 400: nothing was written. */
export class DescriptorRefused extends Error {}

/** The descriptor is well-formed and contradicts what its `(name, version)` already says.
 *  The route answers 409 — the conflict is with the stored version, not with the request. */
export class VersionImmutable extends Error {}

/** A descriptor as the catalog stores it. `savedAt` and `incompatibilities` are the store's own
 *  facts, not the worker's: a worker declares its schemas and cannot declare when it was
 *  registered, nor whether those schemas break the version before it (`compat.ts` decides that,
 *  from the catalog). Omitting them here is what makes a descriptor unable to assert either. */
export type Descriptor = Omit<ActorRecord, 'savedAt' | 'incompatibilities'>;

// strict:false — real schemas carry keywords and formats ajv does not know (pydantic's `title`,
// invopop's `$id`), and strict mode THROWS on them rather than ignoring them. allErrors — a
// descriptor is refused once, so the reply should name every problem, not the first.
const ajv = new Ajv2020({ strict: false, allErrors: true });

/**
 * The descriptor, as `contracts/kontra/v1/catalog.proto` describes it — the runtime half of what
 * `catalog.contract.ts` pins at compile time.
 *
 * `params`/`input`/`output` are `type: 'object'` and nothing more. They are JSON Schema documents
 * the catalog CARRIES and never interprets (the proto types them `google.protobuf.Struct`), so
 * what is checked here is that the wire slot holds a document at all — an operations entry whose
 * `input` is the string "string" is a shape the reader would take apart later, in a browser.
 * Whether the document is a SCHEMA is a separate question, asked below against the 2020-12
 * meta-schema; what is inside it is asked by nobody, which is the point of an opaque field.
 *
 * Unknown top-level keys are ALLOWED. proto3 ignores fields it does not know, the wire is JSON
 * (ADR 0002), and an orchestrator that refused a descriptor because a newer SDK added a field
 * would take a whole fleet's actors out of the catalog over something it could safely drop.
 */
const DESCRIPTOR_SCHEMA = {
  type: 'object',
  required: ['key', 'name', 'version'],
  properties: {
    key: { type: 'string', minLength: 1 },
    name: { type: 'string', minLength: 1 },
    version: { type: 'string', minLength: 1 },
    schemaVersion: { type: 'string' },
    // Both SDKs OMIT these when unset rather than sending "" — but "" is still a legal value on
    // the wire (the conformance fixture posts one to prove it unpins), so no minLength here.
    digest: { type: 'string' },
    source: { type: 'string' },
    operations: {
      type: 'array',
      items: {
        type: 'object',
        required: ['name'],
        properties: {
          name: { type: 'string', minLength: 1 },
          description: { type: 'string' },
          params: { type: 'object' },
          input: { type: 'object' },
          output: { type: 'object' },
        },
      },
    },
  },
} as const;

const validate = ajv.compile(DESCRIPTOR_SCHEMA);

/** The per-Method fields a version PROMISES — the three ADR 0004 names verbatim ("any
 *  input/output/params schema change MUST bump it"). `description` is deliberately not one:
 *  it is the author's sentence about the Method, and making a typo fix in a docstring demand a
 *  version bump would train everyone to bump for nothing. */
const SIGNATURE = ['params', 'input', 'output'] as const;

const BUMP =
  'A version is immutable and any input/output/params schema change must bump it (ADR 0004): ' +
  'raise the actor\'s version and re-serve. There is no override — the same (name, version) ' +
  'describing two shapes is exactly how a caller typed against the first one breaks with ' +
  'nothing anywhere recording that the shape moved.';

/**
 * Read a POSTed body as a descriptor, or refuse it saying why.
 *
 * Narrowed on the way IN, like `parseScratchDocument`: what reaches the store is what this build
 * understands, so a field nobody validated cannot sit in the `actors` table waiting to be
 * misread. Unknown keys are dropped here (they were dropped by the route before, silently and
 * one field at a time).
 */
export function parseDescriptor(body: unknown): Descriptor {
  if (!validate(body)) {
    throw new DescriptorRefused(`not an actor descriptor: ${explain(validate.errors)}`);
  }
  const d = body as {
    key: string;
    name: string;
    version: string;
    schemaVersion?: string;
    digest?: string;
    source?: string;
    operations?: Array<Record<string, unknown>>;
  };

  // The key IS `{name}@{version}` (the proto says so) and the immutability check below looks a
  // registration up BY key. Without this, a re-registration under any other key writes a SECOND
  // row for the same (name, version) instead of colliding with the first — and `resolveActor` in
  // the CLI matches on name+version, so a dispatch then picks whichever row the catalog happens
  // to list first. The gate would be bypassable by a typo.
  const expected = `${d.name}@${d.version}`;
  if (d.key !== expected) {
    throw new DescriptorRefused(
      `key "${d.key}" must be "${expected}" — the catalog's key is {name}@{version}, and a row ` +
        'keyed any other way is a second entry for one actor rather than a re-registration of it'
    );
  }

  const operations: ActorOperation[] = (d.operations ?? []).map((raw) => {
    const op = raw as {
      name: string;
      description?: string;
      params?: JsonSchemaDoc;
      input?: JsonSchemaDoc;
      output?: JsonSchemaDoc;
    };
    const kept: ActorOperation = { name: op.name };
    if (op.description !== undefined) kept.description = op.description;
    for (const field of SIGNATURE) {
      const doc = op[field];
      if (doc === undefined) continue; // a Method that declares neither still registers
      refuseNonSchema(`Method ${JSON.stringify(op.name)}`, field, doc);
      kept[field] = doc;
    }
    return kept;
  });

  const rec: Descriptor = {
    key: d.key,
    name: d.name,
    version: d.version,
    schemaVersion: d.schemaVersion ?? 'kontra.actor.v1',
    operations,
  };
  // ABSENT, not `undefined`-valued, when the worker omitted them: `upsertActor` keeps a
  // previously registered digest only on `a.digest ?? prev.digest`, and an empty string is a
  // value that unpins. Absence has to survive the trip through here to mean anything.
  if (d.digest !== undefined) rec.digest = d.digest;
  if (d.source !== undefined) rec.source = d.source;
  return rec;
}

/** A workflow descriptor as the catalog stores it. `savedAt` is the store's own fact — a worker
 *  declares what its workflow takes and cannot declare when the catalog heard about it. */
export type WorkflowDescriptor = Omit<WorkflowRecord, 'savedAt'>;

/**
 * The workflow descriptor, as `contracts/kontra/v1/catalog.proto` describes it.
 *
 * SCREENED FOR THE SAME REASON `POST /api/actors` IS. That route checked three strings and cast,
 * and `{"operations":[1,2,3]}` registered with a 200 — the first code to look inside was the first
 * check, in a browser. This body arrives from the same kind of sender (a worker, unauthenticated,
 * on serve) and lands in the same kind of reader (a field table on a page), so it is narrowed on
 * the way in rather than trusted.
 *
 * `input`/`output` are `type: 'object'` and nothing more, exactly as an operation's are: JSON
 * Schema documents the catalog CARRIES and never interprets (the proto types them
 * `google.protobuf.Struct`). Whether the document IS a schema is asked below, against the
 * meta-schema; what is inside it is asked by nobody.
 *
 * Unknown top-level keys are ALLOWED and dropped: proto3 ignores fields it does not know, and an
 * orchestrator that refused a descriptor because a newer SDK added a field would take a worker's
 * workflows out of the catalog over something it could safely forget.
 */
const WORKFLOW_SCHEMA = {
  type: 'object',
  required: ['name'],
  properties: {
    name: { type: 'string', minLength: 1 },
    description: { type: 'string' },
    // THE QUEUE THE REGISTERING WORKER IS ABOUT TO POLL. Half of "how do I start this": Temporal
    // routes a start to whatever queue name it is given and only a worker polling that exact one
    // takes the task, so the right TYPE on the wrong QUEUE is a run that starts and never
    // progresses. Optional because an older SDK does not send it, and because a descriptor from a
    // worker that never named one must read as unknown rather than as the empty string.
    queue: { type: 'string' },
    // THE FILE NO LONGER IMPORTS. A watch-mode serve posts this when a save breaks the file: the
    // import error, verbatim, so the contract panel shows the state rather than the last good form.
    // A string like `description` — the text it carries is a message, not a schema — so it is
    // screened for being a string and nothing more, and passed through the same absent-vs-empty rule.
    error: { type: 'string' },
    input: { type: 'object' },
    output: { type: 'object' },
  },
} as const;

const validateWorkflow = ajv.compile(WORKFLOW_SCHEMA);

/**
 * Read a POSTed body as a workflow descriptor, or refuse it saying why.
 *
 * THERE IS NO IMMUTABILITY GATE HERE, and that is a decision rather than an omission. ADR 0004
 * binds a VERSION, and a workflow has none: it is the operator's own file, served from the
 * operator's own process, and editing it and re-serving is the ordinary way to change it. Refusing
 * the second registration would refuse the normal dev loop and leave the catalog describing the
 * code that is no longer running — the opposite of what the actor gate buys.
 */
export function parseWorkflowDescriptor(body: unknown): WorkflowDescriptor {
  if (!validateWorkflow(body)) {
    throw new DescriptorRefused(`not a workflow descriptor: ${explain(validateWorkflow.errors)}`);
  }
  const d = body as {
    name: string;
    description?: string;
    queue?: string;
    error?: string;
    input?: JsonSchemaDoc;
    output?: JsonSchemaDoc;
  };
  const rec: WorkflowDescriptor = { name: d.name };
  // ABSENT, not `undefined`-valued: the store writes NULL for an absent key and '' for a present
  // empty one, and "the author wrote no docstring" must not reach a reader as "described, with
  // nothing".
  if (d.description !== undefined) rec.description = d.description;
  // THE IMPORT ERROR, when watch mode caught one. Absent means the file imports — the ordinary
  // case — and the page draws the contract; present means it does not, and the page draws the error
  // instead of the last good form. A broken descriptor carries no input/output, so a re-registration
  // overwrites those slots to NULL and the stale form is dropped rather than left standing.
  if (d.error) rec.error = d.error;
  // Same rule, and it matters more here: a page that prefills its queue field from this must be
  // able to tell "no worker has told us" from "a worker told us the empty string", because the
  // first is a field to leave alone and the second is a value to put in it.
  if (d.queue) rec.queue = d.queue;
  for (const field of ['input', 'output'] as const) {
    const doc = d[field];
    if (doc === undefined) continue; // an unannotated run method still registers
    refuseNonSchema(`Workflow ${JSON.stringify(d.name)}`, field, doc);
    rec[field] = doc;
  }
  return rec;
}

/**
 * Refuse a re-registration of an already-catalogued `(name, version)` whose schema moved.
 *
 * Compared against what is STORED, per Method by name. Only the Methods the catalog already
 * holds are checked:
 *
 *  - a NEW Method is allowed. `setActorDigest` creates a minimal row with `operations: []` when a
 *    worker's digest lands before its descriptor, and a hand `register_actor` (the MCP tool) does
 *    the same; refusing additions would make the digest arriving first fail the registration that
 *    follows it, which is ordering, not a contract change.
 *  - a Method the catalog holds and this descriptor does NOT declare is refused. That is a Method
 *    disappearing from a version — the case a caller notices as a dispatch to a name that no
 *    longer resolves — and it is also how a rename would otherwise slip through as an add plus a
 *    silent drop.
 */
export function refuseSchemaChange(prev: ActorRecord | undefined, next: Descriptor): void {
  if (!prev) return;
  for (const before of prev.operations) {
    const after = next.operations.find((o) => o.name === before.name);
    if (!after) {
      throw new VersionImmutable(
        `${prev.key} is already catalogued with Method "${before.name}", which this ` +
          `registration does not declare. ${BUMP}`
      );
    }
    for (const field of SIGNATURE) {
      if (canonical(before[field]) !== canonical(after[field])) {
        throw new VersionImmutable(
          `${prev.key} is already catalogued with a different ${field} schema for Method ` +
            `"${before.name}". ${BUMP}`
        );
      }
    }
  }
}

/**
 * Is this document a JSON Schema — not, what does it describe.
 *
 * The meta-schema is the only honest way to ask: a hand-written "looks like a schema" check would
 * be a second, worse JSON Schema implementation living in a route. `validateSchema` compares the
 * document to the 2020-12 meta-schema WITHOUT compiling it, which matters — compiling registers
 * the document's `$id` globally in this ajv instance and a second registration of the same `$id`
 * throws (`schema with key or id "…" already exists`), which is precisely the bug the execution
 * path hit when a Go actor with typed I/O was dispatched twice.
 *
 * `subject` is the already-quoted thing the schema belongs to — `Method "fetch"`, `Workflow
 * "NsCheck"` — because both halves of the catalog land here and a workflow refused as a "Method"
 * would send an operator looking through an Actor for a name that is not in one.
 */
function refuseNonSchema(subject: string, field: string, doc: JsonSchemaDoc): void {
  let ok: boolean;
  try {
    ok = ajv.validateSchema(doc) === true;
  } catch {
    // Ajv THROWS, rather than answering false, for a document that names a dialect it does not
    // hold — `$schema: "http://json-schema.org/draft-07/schema#"` raises `no schema with key or
    // ref`. That document IS a JSON Schema; this instance simply carries the 2020-12 meta-schema
    // and no other. Refusing it would take a real actor out of the catalog over its emitter's
    // choice of dialect, and would say "not a JSON Schema document" about a document that plainly
    // is one.
    return;
  }
  if (!ok) {
    throw new DescriptorRefused(
      `${subject} ${field} is not a JSON Schema document: ${explain(ajv.errors)}`
    );
  }
}

/** ajv's errors as one line an operator can act on: where, and what is wrong there. */
function explain(errors: ErrorObject[] | null | undefined): string {
  const said = (errors ?? []).map((e) => `${e.instancePath || '/'} ${e.message ?? ''}`.trim());
  return said.length > 0 ? said.join('; ') : 'no reason given';
}

/**
 * Deterministic text for a schema document, so two emissions of the SAME schema compare equal.
 *
 * Object keys are SORTED: Go's `encoding/json` writes a map in sorted key order and Python writes
 * a dict in insertion order, so a plain `JSON.stringify` would read a re-ordered but identical
 * schema as a change and demand a version bump for nothing. Arrays keep their order — `prefixItems`
 * is positional, and while `required` is morally a set, its order is stable within one emitter,
 * which is the only comparison this makes (one `(name, version)` is one build's emission).
 *
 * An unset schema is '' and an empty document is '{}': "this Method advertises nothing" and "this
 * Method advertises the empty schema" are different promises.
 */
function canonical(doc: JsonSchemaDoc | undefined): string {
  return doc === undefined ? '' : stable(doc);
}

function stable(value: unknown): string {
  if (value === null || typeof value !== 'object') return JSON.stringify(value) ?? 'null';
  if (Array.isArray(value)) return `[${value.map(stable).join(',')}]`;
  const entries = Object.entries(value as Record<string, unknown>).sort(([a], [b]) =>
    a < b ? -1 : 1
  );
  return `{${entries.map(([k, v]) => `${JSON.stringify(k)}:${stable(v)}`).join(',')}}`;
}
