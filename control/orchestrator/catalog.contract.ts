/**
 * Compile-time congruence guard for the CATALOG descriptor — enforced by `tsc --noEmit`
 * (the CI `typecheck` step), NOT vitest. ts-proto emits this file's imports as types-only
 * (`onlyTypes=true`, no runtime classes) and TS types are erased, so there is nothing for a
 * runtime test to introspect; the peer guard for the dispatch envelope is entry.contract.ts.
 *
 * WHAT WENT WRONG WITHOUT IT. `description` (what a Method is for) and `source` (where the
 * worker loaded the actor from) were added to the wire, to `POST /api/actors` and to the
 * `actors` table, and neither ever reached shared/contracts/kontra/v1/catalog.proto. Nothing outside
 * `_gen/` imports ActorDescriptor, so the file that is supposed to BE the shared type
 * definition could sit two fields behind the thing it defines with every suite green. This
 * makes that distance a compile error instead of a discovery.
 */
import type {
  ActorDescriptor,
  ActorOperation as DescribedOperation,
  WorkflowDescriptor,
} from './_gen/kontra/v1/catalog';
import type {
  ActorOperation as StoredOperation,
  ActorRecord,
  WorkflowRecord,
} from './src/db/repo';

/** `true` only when `T` is empty — the constraint is what fails, and it names the stray field. */
type NoneLeftOver<T extends never> = [T] extends [never] ? true : never;

type AssertEqual<A, B> = [A] extends [B] ? ([B] extends [A] ? true : never) : never;

/**
 * proto3's JSON mapping lowerCamelCases field names, so the descriptor field `schema_version`
 * is `schemaVersion` on the wire and in the store — while the GENERATED TS keeps snake_case
 * (ts-proto is configured `snakeToCamel=false` so the generated names line up with the Python
 * dataclasses). One field, two spellings; this line is the only place that knows.
 */
type StoreName<K extends string> = K extends 'schema_version'
  ? 'schemaVersion'
  : K extends 'builder_digest'
    ? 'builderDigest'
    : K;

/** Every descriptor field is a field the catalog stores. A proto field with nowhere to land is
 *  a descriptor the orchestrator would accept and silently drop on the way to the table. */
const _everyDescriptorFieldIsStored: NoneLeftOver<
  Exclude<StoreName<keyof ActorDescriptor & string>, keyof ActorRecord & string>
> = true;

/**
 * …and the other direction, minus the row's own bookkeeping: WHEN a row was written is the
 * catalog's fact about itself, not something a worker declares about its actor. Anything else the
 * store grows is a field the descriptor should have carried.
 *
 * `incompatibilities` is the second such fact and is exempted for the same reason, not as a
 * convenience: it is what the catalog CONCLUDED by comparing this registration against the version
 * before it (`src/compat.ts`), and the comparison needs a catalog to be made from. A wire field for
 * it would let a worker declare itself compatible — the one claim on this route nothing could
 * check.
 */
const _theStoreAddsOnlyItsOwnBookkeeping: NoneLeftOver<
  Exclude<
    keyof ActorRecord & string,
    StoreName<keyof ActorDescriptor & string> | 'savedAt' | 'incompatibilities'
  >
> = true;

/** The per-Method pair, both ways: the stored operation is exactly the described one. */
const _everyOperationFieldIsStored: NoneLeftOver<
  Exclude<keyof DescribedOperation & string, keyof StoredOperation & string>
> = true;
const _theStoreAddsNoOperationFields: NoneLeftOver<
  Exclude<keyof StoredOperation & string, keyof DescribedOperation & string>
> = true;

/** A schema is CARRIED, never modelled: JSON Schema Draft 2020-12 is not proto, so both sides
 *  hold an opaque object. Typing it as a message here would be the contract claiming to
 *  understand a document it only forwards. */
type OpaqueSchema = DescribedOperation['input'];
const _storedSchemasAreTheSameOpaqueDocument: AssertEqual<
  NonNullable<StoredOperation['input']>,
  NonNullable<OpaqueSchema>
> = true;

/**
 * The WorkflowDescriptor says what a workflow IS and WHERE IT IS SERVED — the four things
 * `GET /api/workflows` could never say about the file it listed by name, byte count and mtime.
 *
 * It is pinned as a literal as well as against the store, because the store is now a second
 * spelling of it: a field dropped from BOTH at once would leave the two congruent and the proto
 * quietly not describing what the SDK emits.
 *
 * `queue` is the newest of the four and the one the literal earns its keep on: it was added to the
 * SDK, the route and the table in one change, and without this line the proto could have been the
 * only place it was missing — which is precisely the distance this file exists to close.
 */
const _aWorkflowDescriptorSaysWhatAWorkflowIs: AssertEqual<
  WorkflowDescriptor,
  {
    name: string;
    description: string;
    queue: string;
    error: string;
    input: OpaqueSchema;
    output: OpaqueSchema;
  }
> = true;

/**
 * …and now something emits one (`internals/catalog.py:publish_workflow_catalog`, on serve), so the
 * same two directions the actor descriptor is held to apply here.
 *
 * `savedAt` is exempt for the reason `ActorRecord`'s is: WHEN the catalog heard about a workflow is
 * the catalog's fact about itself, not something a worker declares about its code.
 */
const _everyWorkflowFieldIsStored: NoneLeftOver<
  Exclude<keyof WorkflowDescriptor & string, keyof WorkflowRecord & string>
> = true;
const _theWorkflowStoreAddsOnlyItsOwnBookkeeping: NoneLeftOver<
  Exclude<keyof WorkflowRecord & string, (keyof WorkflowDescriptor & string) | 'savedAt'>
> = true;
const _storedWorkflowSchemasAreTheSameOpaqueDocument: AssertEqual<
  NonNullable<WorkflowRecord['input']>,
  NonNullable<OpaqueSchema>
> = true;

void _everyDescriptorFieldIsStored;
void _theStoreAddsOnlyItsOwnBookkeeping;
void _everyOperationFieldIsStored;
void _theStoreAddsNoOperationFields;
void _storedSchemasAreTheSameOpaqueDocument;
void _aWorkflowDescriptorSaysWhatAWorkflowIs;
void _everyWorkflowFieldIsStored;
void _theWorkflowStoreAddsOnlyItsOwnBookkeeping;
void _storedWorkflowSchemasAreTheSameOpaqueDocument;
