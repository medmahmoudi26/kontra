# 53. A shared Dataset is addressed, not filtered — and the grant is keyed on the address

Date: 2026-10-05

## Status

Accepted.

Extends [ADR 0051](0051-the-workspace-is-the-isolation-boundary.md), which made the workspace the
isolation boundary and the lake the first store to honour it. This is the **first deliberate hole in
that boundary**, and it is cut the way §3 says a boundary must be drawn: by address, never by filter.

It also walks back one sentence of the plan this work started from — that the `shared` flag belongs
on the Dataset record of [ADR 0029](0029-live-datasets-name-tag-retention.md) §4. The record is the
right *kind* of thing and the wrong *key*, and §1 below says why, measured.

## Context

### A new workspace starts with an empty lake, and that is working as designed

ADR 0051 §1 put every per-workspace store behind an address derived from the workspace name.
`data/parquet.ts` implements the lake half: `activeLakeWorkspace()` answers which workspace this
install is reading, and `workspaceAddress()` turns that name into a bucket and a DuckLake catalog.
Measured on this checkout, 2026-10-05 — four workspaces under the mount, `.current` naming one:

    workspaces/bugbounty   workspaces/default   workspaces/demo   workspaces/scraping
    workspaces/.current → demo

and `docker-compose.yml:86` sets one catalog connstring, `postgres:dbname=kontra_ducklake …`, into
which `workspaceAddress` substitutes the workspace: `dbname=kontra_ducklake_ws_demo`. Four
workspaces, four Postgres databases, four lakes. Create a fifth workspace and its Datasets page is
empty, correctly, because nothing has written to `ws-<name>` yet.

That is the boundary doing its job, and it is also the first thing an operator runs into. A scope
list took work to assemble. `standalone/` is where `kontra dataset create` puts an operator-loaded
list, and a scope list is the clearest thing on this install that two engagements would both want.
Under ADR 0051 the second engagement's workspace cannot see it, and the honest options are "build it
again" or "there is no boundary".

### The platform grants the read; userland does the copy

The shape settled before the mechanism: the control plane makes one workspace's Dataset **readable**
from another, and a workflow in the reading workspace clones it into its own lake through the
ordinary publish path. There is no cross-workspace WRITE anywhere in this decision.

That split is what keeps the blast radius small. A cross-workspace write would mean a connection with
two lakes attached and one of them writable — which `migrationConnection` already is, for the
one-shot migration it was built for (issue 10), and which is exactly the thing not to make routinely
available.

### Where the flag goes: the Dataset record is the wrong key, and the code says so

ADR 0029 §4 makes the Dataset record the durable authority for a Dataset's deviation — its tags and
its optional rename — **keyed by the Run that wrote it**, storing only deviation, so an untagged
un-renamed Dataset has no row. A `shared` boolean follows the deviation-only rule exactly, and the
record is where it was meant to go.

The key cannot express the thing being shared. Measured against the code, not argued:

1. **A standalone Dataset never has a `runId`, and never can.** `withDatasetNames` in
   `data/datasets.ts` opens with `if (info.kind !== 'output' || !info.dt || !info.version) return
   info;` — an operator-loaded list has no Run, no dispatch and no version, so nothing downstream
   ever stamps one. The most shareable kind of Dataset on this install is the one a Run-keyed flag
   structurally cannot mark.

2. **Even for output, the grains do not match.** `DatasetInfo.runId` is documented as "PRESENT
   EXACTLY WHEN THE ROW HAS ONE RUN" and is deliberately absent the moment two Runs share a
   partition — ADR 0029 §2's refusal of a singular where none exists. But a cross-workspace read is
   `SELECT … FROM <schema>.<table>`, which returns **every Run's rows under that name**. `lame` holds
   five Runs' output on this box. So a Run-keyed grant would be a record whose stored grain is
   strictly finer than the access it licenses: one Run's flag opening five Runs' rows.

A grant that reports success over an access wider than itself is the failure mode this repo cares
about most. So the key is the thing the reader actually uses.

### And the read cannot be a parameter on the query workbench

The obvious implementation was a `workspace` parameter on `POST /api/datasets/query`. Everything
lines up: `data/queryEngine.ts` already attaches a lake `READ_ONLY`, already hardens the connection
(`disabled_filesystems`, `lock_configuration`), already caches per `(catalog, dataPath)`, and
`resolveLakeConfig(store, { workspace })` already derives another workspace's address.

It does not work, for one reason that cannot be patched: **the workbench takes SQL a person wrote,
and an attached catalog is reachable by qualified name.** `refreshViews` can expose one shared
Dataset as a bare name, and nothing stops the same statement from writing
`FROM shared_lake.output.<anything>`. Naming one shared Dataset would hand over the whole of the
owning workspace's lake. Filtering the view layer does not help — the views are a convenience over a
catalog the statement can already address.

`routes/datasets.ts` already draws this line, in its own words: "Everything in `routes/datasets.ts`
composes its own SQL from a name checked against the catalog; these take a statement a person wrote."
The cross-workspace read belongs on the first side of it.

## Decision

**1. The grant is keyed on the ADDRESS: (workspace, kind, name).** A new store,
`data/sharedDatasets.ts`, one table, `PRIMARY KEY (workspace, name, kind)`. Those three fields are
exactly what the reader uses — `workspaceAddress(workspace)` derives the catalog and bucket,
`schemaOf(kind)` picks the DuckLake schema, and the name is the table. Nothing is stored that the
read path does not consult, and nothing is consulted that is not stored.

`kind` is in the key because an output Dataset and a standalone list live in different schemas and
can share a name — the console's own row identity is `kind:name` for that reason. Sharing one must
not share the other.

**2. The shape is PRESENCE, not a column.** ADR 0029 §1 makes tags a SET because two writers must
converge, and a rename a SCALAR because it is one choice among many strings. `shared` is neither:
its domain has two values and one of them is **the absence of the row**. Share INSERTs, unshare
DELETEs. That keeps the deviation-only rule literally — an unshared Dataset has no row — and makes a
concurrent double-share idempotent by the primary key rather than by a read-modify-write that could
race. Two writers disagreeing (one shares, one revokes) is a real conflict with two legitimate
outcomes, and last-write-wins is honest there in a way it is not for a rename: nothing is lost that
the loser cannot simply say again.

A repeat grant keeps the **first** `sharedAt`. The audit answer to "since when has this been
exposed" must not be "since you last asked".

**3. A grant names the OWNER, not the READER.** It says *this Dataset may be read from another
workspace*, not *workspace X may read it*, so any workspace on the install can read a granted
Dataset. That is a deliberate first cut and it follows ADR 0051 §1: **the install is the tenant.**
Every workspace here belongs to one person, so a grant is a declaration about the DATA — this is
shareable — rather than an ACL between parties that do not trust each other. An install that ever
becomes multi-tenant needs a reader on the grant, and that is a schema change, not a redesign: the
key gains a column.

**4. The read ATTACHes the owning lake READ_ONLY, and the caller NAMES the workspace.** There is no
default. Defaulting the source to the active workspace would make a cross-workspace read expressible
by *leaving a field out*, which is the opposite of an address. Get the name wrong and the attach
lands on a catalog that does not hold the table, and the read fails — ADR 0051 §3's failure
direction, which is the one worth having.

`READ_ONLY` is on the **ATTACH**, so DuckDB enforces it on statement type and refuses statements this
code never anticipated. `data/sharedRead.test.ts` proves it rather than asserting it: against two
real DuckLakes, `INSERT`, `DROP` and `DELETE` through the shared alias all come back refused and the
row count is unchanged.

**5. The attach primitive is generalised, not duplicated.** `migrationConnection` was a hand-rolled
two-lake attach; it is now a three-line caller of `attachedLakeConnection(store, [{alias, override,
readOnly}, …])`, and `sharedLakeConnection` is the one-lake read-only caller. `readOnly` is a
required field rather than a defaulted option, because the migration's single most valuable property
is that its source cannot be written to and a second attach path is a second chance to forget it.
Every address goes through `resolveLakeConfig`, so a cross-workspace read reaches the same lake every
other caller does.

**6. The reader's own lake is NOT attached, and that is what makes "no cross-workspace write" a
property rather than an intention.** The shared connection holds one database. A one-statement copy
is not expressible on it whatever a future route intends, and the test asserts the alias list.

**7. The SQL is composed by the server; the one caller fragment is held to a grammar.** No caller SQL
reaches the shared connection — the read takes an address plus the bounded narrowing a partition
column allows (`version`, `dt`, `limit`, `offset`). The exception is `orderBy`, which `pageDataset`
requires because a materialized Dataset stamps no row id and LIMIT/OFFSET over it has no defined
order. An ORDER BY is an expression, so
`ORDER BY (SELECT count(*) FROM shared_lake.output.secrets)` is an ordinary one;
`assertSimpleOrderBy` admits `<column> [ASC|DESC] [NULLS FIRST|NULLS LAST]` and nothing else, and
re-emits the parsed terms rather than passing the string through.

**8. A new route, and a workflow reaches it through an ACTIVITY instead.**
`GET|PUT|DELETE /api/datasets/shared…` is the operator's and the console's surface. A workflow in
another workspace calls the `pageSharedDataset` **activity**, for two reasons that are not
convenience: a workflow holds no bearer token and must not be given one, and a page must become a
**claim-checked ref** so the workflow's history carries ~110-byte pointers instead of rows — the
property `pageDataset` exists for. The activity returns the same `{ref, n, done}` shape, so a clone
workflow iterates a cross-workspace Dataset with the code it already has, and the page object lands
in the **reader's** CAS, which is where the copy begins.

**9. The exposure listing is cross-workspace, and says so in the response.** `GET
/api/datasets/shared` returns every grant on the install with `scope: "all-workspaces"`, or one
owner's with `scope: "<workspace>"`. This is the one legitimate cross-workspace listing: the rows are
not any workspace's data, they are the install's record of which boundaries have been opened, and an
operator who cannot see every exposure in one place cannot audit the boundary at all. It reads the
grant table and attaches no lake, so it costs one indexed query however many workspaces exist and
cannot fail because one workspace's lake is down. Each entry carries the address it points at,
derived by `workspaceAddress` — "`lame` is shared from `bugbounty`" does not tell you which bucket a
reader opens.

**10. The whole surface fails CLOSED, on the explore token.** `checkBearer` +
`EXPLORE_TOKEN_VARS` — not the `checkOptionalBearer` the tag and rename routes beside it use.
`auth.ts` decides this in its own words: `checkOptionalBearer`'s header says "Nothing that reads live
scan data or spends money on its own may use this. Fail-closed is the default for a reason and this
is the documented exception." A read of another workspace's lake is live scan data by construction.
The grant WRITES are held to the same bar, because a grant is what creates that read — gating the
read and leaving the grant opt-in would be the inverse of the mistake `routes/datasets.ts` already
records, where "a route that can read every dataset was gated while the route that can DELETE one was
not". On a box with neither `KONTRA_EXPLORE_TOKEN` nor `KONTRA_STATE_TOKEN`, all four routes answer
503 and nothing is shared or read. A console session admits all four (ADR 0045), which is how the
toggle works in a browser holding no service token.

**11. `dataset.share` and `dataset.unshare` are audit actions.** `dataset.delete` is already one;
these belong for a reason it does not have. A deletion announces itself the moment anybody looks for
the rows, while **a grant is invisible until somebody uses it** — "who opened this up, and when" has
no other answer. The target is the full address, `<workspace>/<kind>/<name>`.

**12. The listing reports it, as a fourth best-effort join.** `withSharedDatasets(infos, grants,
workspace)` is pure and total over two arrays, like its two siblings, and keys on `(kind, name)`
within the workspace the resolved `LakeConfig` names. A grant store that cannot be reached leaves
every row unshared — which understates the exposure on screen and changes nothing about admission,
because the read path checks the same store. `resolveLakeConfig` now carries the `workspace` it
resolved, so the join cannot be keyed to a workspace whose lake the listing never read.

## Consequences

**A grant is table-grain, so it exposes every Run's rows under that name.** This is the direct
consequence of §1 and it is the right one — it is what `SELECT … FROM <table>` does — but it means
"share this Dataset" is not "share this run's output". An operator who wants to expose one Run's rows
has no verb for it, and should promote them into a Dataset of their own first (ADR 0028's accepting
act) and share that. The listing draws the badge on every partition of a granted name for the same
reason: a badge finer than the access would be a worse lie than a coarse one.

**A grant can point at nothing, and that state is loud at read and visible at enumeration.** `PUT`
deliberately does not check the catalog, so an operator can expose the Dataset a scheduled run is
about to write. A grant whose Dataset was dropped therefore lists in the exposure listing and fails
the read with "the share is recorded but the Dataset is not in that workspace's catalog", naming the
catalog. The alternative — omitting it from the listing — would leave an exposure nobody could see
in order to revoke it.

**Both refusals answer 404, which costs the operator some diagnosis.** "No grant" and "the grant
points at nothing" are different sentences on deliberately the same status, so this surface cannot be
used to ask whether another workspace holds a table of a given name. The price is that an operator
debugging their own install reads the sentence rather than the status.

**A cross-workspace read pays a full attach, every call.** The connection is not pooled, for the
reason `attachedLakeConnection` states: a pooled connection would leave `shared_lake` attached to
every later caller of the same key. `data/queryEngine.ts` measures ~260 ms for a DuckDB instance plus
three extension loads, and that is the order of what a shared read costs. Fine for a clone paging
through a Dataset; wrong for a poll, and nothing here is polled. If a clone of a very large Dataset
ever makes this the bottleneck, the fix is a cache keyed on `(catalog, dataPath, alias)` — not
reaching for the shared `lakeConnection`.

**The grant table is cluster-wide, which §3 forbids for data.** It holds rows about several
workspaces. The justification is that it is not data: the workspace is the KEY rather than a filter
over a shared pool, the read derives its address from the workspace name on the row it matched, and a
grant that lived inside the workspace it grants from could not be enumerated without attaching every
lake on the install. ADR 0051 §1 keeps "login, users, audit" cluster-wide because they are the
install's; a cross-workspace grant is the install's in the same way.

**What this change CANNOT enforce, stated rather than implied:**

- **Revocation does not recall a copy.** The whole point is that userland clones the rows into its
  own lake. Once it has, `DELETE /api/datasets/shared/…` closes the read and leaves the copy. There
  is no mechanism here that could do otherwise, and anybody reading "unshare" as "take it back" is
  reading something that is not there.
- **Nothing stops a reader cloning, then sharing the clone.** The clone is an ordinary Dataset in the
  reader's own lake, so its workspace can grant it like any other. The boundary is between
  workspaces, not between a Dataset and its lineage.
- **A workspace with no lake yet reads as "no such dataset", not "no such lake".** On a
  Postgres-backed catalog the attach fails on a database that does not exist, which is loud. On the
  appliance's FILE catalog, `workspaceAddress` returns the bare relative name `ws-<name>.ducklake`
  and `ATTACH` creates it — so reading a never-written workspace mints an empty catalog in the
  process's working directory and then answers "no such dataset". That is a pre-existing property of
  `workspaceAddress` (and `defaultCatalogPath`'s own header warns about exactly this class of bare
  relative name); this change inherits it rather than introducing it, and it is not fixed here
  because relocating every workspace's file catalog is its own migration.
- **Sharing a Dataset shares nothing else.** ADR 0051 lists Temporal, logs, metrics, secrets and
  Pulumi as targets, and only the lake is built. That is still true after this change: a reader with
  a grant can read rows and cannot see the owning workspace's runs, logs or secrets. The grant is not
  a tenancy bridge.

**Three new modules and one generalised primitive.** `data/sharedDatasets.ts` (the grant),
`data/sharedRead.ts` (the read and the enumeration), `routes/sharedDatasets.ts` (the surface), and
`attachedLakeConnection` in `data/parquet.ts`, which `migrationConnection` now calls. The migration's
same-address refusal is kept at its own call site, because its reason is specific: copying a lake
onto itself doubles every row.

## What this does not decide

- **Per-reader grants.** §3 makes a grant a property of the owner. Which workspace may read is
  answerable the day the install stops being one tenant, and is a column on the key rather than a new
  design.
- **The clone workflow.** It is userland, in a workspace, and it is not the platform's. What the
  platform owes it — a paged, claim-checked, read-only read addressed by (workspace, name) — is here.
- **Whether a shared Dataset should be visible in the reader's own listing.** Today a reader must
  know the owner and the name, from the exposure listing. A merged view that showed another
  workspace's Datasets in `GET /api/datasets` would be a filter over two addresses, which is the
  thing §3 refuses; if it is ever wanted it should be its own surface, clearly labelled with whose
  lake each row is in.
- **Whether `shared` mirrors anywhere.** ADR 0029 §4's tag mirrors to a Temporal search attribute
  and is never read as truth. A grant has no live window to mirror into — it is about a table, not a
  Run — so there is nothing to project and nothing to reconcile.
