# Execution

How kontra turns a caller's workflow into work that runs on a fleet. This is the language the
orchestrator, the handler and actorkit all speak; it is the repo's core context.

> **This glossary describes v2, decided 2026-08-13 and not yet built.** v2 is a deliberate
> breaking change, not a migration: one deployed kind (the **Actor**), no graph interpreter, and
> a caller's workflow driving everything. **Activity**, **Monitor**, **Node**, **Chunk** and
> **Manifest** are retired below — the code still implements them today. Where the two disagree,
> this file is the target and the code is the present.

## Language

**Actor**:
Versioned code that exposes one or more **Methods** and is activated for the length of a
**Session**.
_Avoid_: entity, service, worker (a **Worker** is a process, not code), grain

**Method**:
A named, individually dispatchable callable on an **Actor**. It receives three parameters: `self`
(the loaded **Actor** instance from the **Session**), a **Batch** of work, and a **Dataset** where
output goes. The author owns the loop over the **Batch**, the framework owns the commit point at
the iterator boundary, and the caller owns the output destination. The **Method** returns
`(results, dropped)`, a tuple the caller must destructure. Output pushes to the dataset; the
framework tracks input position (which **Unit** was last processed) and output offset separately.
_Avoid_: task, step (this glossary bans both, as **Unit** and node-position synonyms), function,
endpoint

**Session**:
The lifetime of one activated **Actor** instance — opened by a caller, bracketed by load and
close, spanning however many **Method** calls the caller makes inside it. Whatever the **Actor**
holds in memory lives exactly this long, and a **Session** that dies takes it with it rather
than silently starting over.
_Avoid_: activation (an **Actor** is not perpetually addressable), connection, instance

**Unit**:
One indivisible piece of work an **Actor** consumes — the grain of retry and of commit.
_Avoid_: task, item, job

**Batch**:
A ~110-byte ref in a workflow's history, either the input to a **Method** call or its chainable
output. When a **Method** call names no output destination, it returns a **Batch** — a ref to the
results that the caller can pass to the next **Method** without materializing to the lake. When it
names a destination, the output becomes a named **Dataset** instead, and the **Batch** is the
internal representation. Distinguished from **Dataset** by cost: a **Batch** is orders of magnitude
cheaper to carry in history.
_Avoid_: page (a **Dataset** is read in **Batches**; there is no second word for the same set)

**Workspace**:
The DIRECTORY this installation keeps its own things in: `~/.kontra/` — `config.yaml`, `workflows/`
and `actors/`, or wherever `$KONTRA_HOME` points. It is where an operator's code and credentials
LIVE. It is not where anything RUNS.
_Avoid_: project, folder (both name a directory without saying whose), and any reading in which a
Workspace isolates work — see below

**A Workspace is not a Tenant, and the difference is the whole reason the word is allowed.**
A **Tenant** IS a Temporal namespace: the only authorisation boundary Temporal has, what a
**Warden**'s certificate is scoped to, and what actually separates one party's work from another's.
A Workspace is a directory; its boundary is Unix file permissions, and those bound who may EDIT the
code and read the credentials — nothing at runtime.

So two Workspaces on one **Tenant** see each other's **Runs**, **Datasets** and queues completely.
That is the intended arrangement for two engineers sharing one instance and it is fine, but it has
to be said out loud, because the intuition runs the other way: the word looks like an isolation
boundary and is not one. **When separation is actually wanted, the answer is a second Tenant, never
a second Workspace.**

`infra/CONTEXT.md` lists `workspace` among the words **Tenant** avoids, and that stands for the
namespace it names. This entry is the other thing the word can mean, defined here so the two cannot
be confused rather than left for someone to conflate.

**Run**:
One execution of a caller's workflow, identified by that workflow's id — which is why it
survives continue-as-new and why every attempt writes to one partition.
_Avoid_: job, execution, dispatch, graph run

**Probe**:
One **Method** call an operator asked for by hand, from the Actors page — one **Actor**, one
version, one **Method**, one **Batch**, into an untagged **Dataset** (**ADR 0033**). It is a **Run**
like any other: there is no probe KIND, and the only thing distinguishing one is which workflow type
it is (`ActorProbe`, kontra's own, served on `kontra-probe`). Two things are definitional rather
than incidental. It goes through the production Nexus operation, because what you debug should be
what runs. And it dispatches UNKEYED — a keyed dispatch attaches to the execution already holding
that key and returns THAT **Batch**'s results, so a probe fired twice in ten seconds would silently
be one. The COUNT is the whole boundary: one **Method**. Two, in any spelling, is a topology, and a
server that executes a topology is the graph interpreter **ADR 0023** §12 removed.
_Avoid_: test run, dry run, debug run (all three suggest a second execution model; there is one)

**Dataset**:
A named, durable, queryable collection written to the lake. A caller supplies a **Dataset** as the
third parameter to a **Method** call, and the author pushes records to it; the caller's act of
naming it makes it durable and queryable. It is **open** while a **Run** is still appending,
**sealed** when the caller declares it complete, and **abandoned** if the **Run** died first — so
a partial **Dataset** can never be read as a finished one. There is no such thing as an unnamed
**Dataset** — omit the destination parameter and you get a **Batch** instead (a ref, ~110 bytes,
live in the caller's workflow history).
_Avoid_: manifest, table, output, index

**derived name**:
What a **Run**'s output is CALLED — `wf-<workflow>-<version>--<dtPartition>Z--<digest6(runId)>`
(**ADR 0029** §2). ONE **Run**, ONE name, even when its output spans several **Actor** tables: the
identity in it is the CALLER WORKFLOW's manifest name and version, held in the **Run's workflow
identity**, and a **Run** with no such record falls back to the producing **Actor**'s. It is
RENDERED, never STORED and never indexed — a name→path registry was deliberately deleted, and
reintroducing a lookup table walks that back. The run-id part is a DIGEST, not a prefix: run ids are
`<type>-<unixseconds>`, so the first six characters are the workflow name and carry no run entropy.
One function spells it (`control/orchestrator/src/data/datasetName.ts`); every surface renders what that
function returns, so the string cannot drift between the CLI, the page and the API. The **Run** it
names comes from whichever authority knows it — the ledger, a **temporary Dataset**'s owner, or the
row's own **contributing Run** when there is exactly one — and its datetime from the row's `dt`
partition, which IS `dtPartition(runStartedAt)`. Stored and printed UTC; the UI re-renders the
instant in the viewer's zone at draw time (`web/src/datasets/localName.ts`) and keeps the canonical
string one hover away, which is the whole of "the UI renders local".
_Avoid_: label (a **tag** is the label), path, id

**Run's workflow identity**:
The caller workflow's manifest `name` and `version`, SNAPSHOTTED when a **Run** starts and keyed by
its `runId` (**ADR 0029** §2, `control/orchestrator/src/data/runWorkflows.ts`). It exists because the
**derived name** renders the workflow and nothing durable knows it: the materialization ledger
carries the **Actor**'s identity, and Temporal forgets the execution's type after retention — so
naming from the ledger alone labelled a **Run**'s output after whichever **Actor** wrote it. EVERY
start path records it — `POST /api/runs`, `kontra workflow start`, and the **Probe**, which has no
`workflow.json` to snapshot and stamps its own name instead — or what a **Dataset** is called would
depend on which command started the run. PROVENANCE, not a registry: nothing is keyed by a name and
nothing is looked up to find a path, and an unrecorded **Run** still gets a name from the **Actor**
fallback.
_Avoid_: registry, catalog, manifest (that is the `workflow.json` this is read FROM)

**Dataset record**:
The durable authority for a **Dataset**'s **tags** and its optional **rename**, keyed by the **Run**
that wrote it (**ADR 0029** §4). It stores ONLY deviation — a **Dataset** with the **derived name**
and no tags has no row — and it is the store the retention sweeper reads, never a Temporal search
attribute (which cannot be written after the execution closes, so it would report "untagged" for
every **Dataset** tagged after its **Run** ended). Independent of Temporal, which is what lets a
**Dataset** be tagged long after its **Run** has closed.
_Avoid_: registry (the deleted name→path table), catalog (that is DuckLake's)

**tag**:
A label an operator or an author attaches to a **Run**'s output to say it MATTERS and should be kept
(**ADR 0029** §1, §4). Tags are a **set** — added and removed, never assigned as a scalar — so two
writers (the author at publish time, the operator afterwards) CONVERGE instead of racing. Held in the
**Dataset record**, which is authoritative; the `KontraTag` search attribute is a fast projection over
the live window and is NEVER read as truth. Only DEVIATION is stored: an untagged **Dataset** has no
row. IT IS RUN-GRAIN, not table-grain — the same grain the **derived name** has, and for the same
reason: the record is keyed by `runId`, so tagging one of a **Run**'s tables marks every table that
**Run** wrote, including its **temporary Dataset** and the durable **Dataset** it promoted into. On a
**temporary Dataset** that is the whole of the effect: a temp is never swept on a clock tagged or
not, and the tag does not stop the explicit delete — what it keeps is the **Run**'s durable output.
_Avoid_: annotation, marker (the temp's owner marker is a different thing), attribute

**rename**:
An explicit name an operator stores for a **Run**'s **Dataset**, OVERRIDING the **derived name**
(**ADR 0029** §4). Optional and singular (a **tag** is a set; a rename is one choice); absent means
the derived name stands, so a surface shows `rename ?? derived name`. Kept in the **Dataset record**
beside the tags, keyed by `runId`.
_Avoid_: alias, title

**retention sweep**:
The periodic collection of UNTAGGED **Datasets** past a TTL this repo owns (**ADR 0029** §3, §5,
`data/retention.ts`). Four rules, each a way it would otherwise lose data: it reads the **Dataset
record** for tags, never `KontraTag` (which cannot be written after a **Run** closes, so it would
report every retroactively-tagged **Dataset** as untagged); its clock is LAST WRITE, not creation,
so a **Run** appending for hours is not collected chunk by chunk; a GRACE window sits behind the TTL
so it does not race an operator tagging at the edge; and an **open** **Dataset** (still being written,
or crashed mid-write) is never collected whatever its age. A tagged, an **open**, and a **temporary
Dataset** are all KEPT. The TTL is a code constant, deliberately NOT the namespace's execution
retention (their both being 24h is the dev server's coincidence — **ADR 0025**'s standing warning).
It runs as ONE heartbeating activity a workflow AWAITS, so a Schedule's `SKIP` overlap guards the real
work; a dry run reports what would be collected before anything is deleted. It is the SECOND thing
that can remove a **Dataset**, and it agrees with the first: it never touches a **temporary Dataset**,
which is deleted explicitly instead.
_Avoid_: garbage collection, TTL expiry (the TTL is one input), cleanup (that is DuckLake's file reclaim)

**row tail**:
The live count of a **Run**'s rows as they land, read from the ONE durable path while its **Dataset**
is still **open** (`control/orchestrator/src/rowTail.ts`). A **Method** pushes each record to its own blob
under `units/run=<id>/` the moment it produces it, and the caller does not publish those into a named
**Dataset** until the call returns — so during a long call the catalog SUM is flat while the durable
path fills. The tail LISTs that path and reports the OBJECT COUNT (one blob is one pushed record), so
the number on screen cannot diverge from what is committed the way a producer-side counter would — the
same divergence family as a **Run** whose units all failed reporting `completed` with empty output. It
is SERVER-SIDE and fanned out: one poller per watched **Run**, every subscriber handed the same
snapshot with the same seq, which is what makes two tabs on one **Run** agree. Its OWN SSE endpoint,
ring and cap (`/api/datasets/rows/stream`), deliberately NOT the state stream's `/api/events` ring —
one large row scan there would evict every ordered fact behind it. Granularity is CHUNK, not row: the
readout is "1,203 rows · last chunk 4s ago", never a per-row trickle, because the storage is chunked
blobs and pretending otherwise is the same lie as a merged status field. A killed stream DEGRADES —
the readout says the stream was lost and keeps the last count — rather than freezing silently on a
number that is no longer current.
_Avoid_: subscribe/publish over Temporal (measured to write history twice), live rows over `/api/events`
(its own ring), per-row streaming (the grain is the chunk)

**temporary Dataset**:
A materialized, owned, short-lived **Dataset** a **Method**'s output stages into before it is
anybody's answer. Like a **Dataset** it is written to the lake AS THE **Run** PROGRESSES and read in
**Batches**; unlike one it is named by the FRAMEWORK (not the caller), OWNED by the **Run** that
opened it (the owning workflow's id is recorded on it, which a later cleanup reads to decide it can
go), and meant to be queried and promoted from and then removed rather than kept. Its storage name
is DERIVED FROM THAT **Run** — `tmp_<run>_<suffix>`, replay-stable, unique per temp within a **Run**,
and still visibly temporary — so it is traceable by eye rather than being a `tmp_4e9b1b23` that names
nothing. The name is storage only: temp-ness and ownership are read from the owner marker, never
from the prefix, so a temp written under the older `tmp_<uuid>` spelling reads exactly the same. It carries the
same **open** / **sealed** / **abandoned** lifecycle. The author cannot tell one from a durable
**Dataset** — a **Method**'s `dataset` parameter reads the same either way (**ADR 0028**), which is
exactly why the name is the framework's to derive and not the caller's to invent. This is the third
word for a reason: a **Batch** is a cheap ref that chains and never materializes; a **temporary
Dataset** materializes, is owned, and is short-lived; a **Dataset** is durable and published. Say
which one you mean. It is removed EXPLICITLY — `kontra dataset delete` or the DELETE route — and
NEVER swept on a clock or dropped when its owning **Run** closes: the whole point of it outliving its
fleet is that triage happens later, so a delete-at-**Run**-close would destroy exactly the case it
exists for. Its owner and age in the listing are what let a human sweep. It CAN be **tagged** — its
owner IS the **Run** the **Dataset record** is keyed by — and doing so neither extends its life (it
had no clock) nor protects it from the explicit delete; it marks the **Run**'s durable output as
kept, which is what an operator tagging a staging table means. The delete confirmation names any
tags, so "kept" and "deletable" are never in silent disagreement.
_Avoid_: staging, scratch, temp table, intermediate

**Promotion**:
The act of reading what a **Run** staged into a **temporary Dataset** and inserting the chosen rows
into a durable **Dataset** — the second half of the two acts temp-datasets separates. Production (a
**Method** pushing into a temp) and PROMOTION (accepting rows into the record) are two acts with a
gap between them, and the gap is where triage fits. A promotion is a QUERY, not a copy of
everything: the rows the query returns ARE the rows promoted (`insert_from(tmp, where="NOT ok")`),
the same `--query` rule the pager follows. It CARRIES PROVENANCE: a promoted row keeps the
**Machine**, **Actor** version and **Run** that produced it, because promotion is a copy of the
source rows (`node`/`version`/`run_id` and all), never a re-stamp of the promoting **Run** over the
producing one. It is a distinct act from the destination argument on a **Method** call (which names
where output STAGES); promotion names what gets ACCEPTED, so the two coexist rather than one
replacing the other. Not idempotent: it is a deliberate act run once, and running it twice appends
twice.
_Avoid_: publish (that is a **Method** call's output landing; promotion is a separate, later act),
copy, merge, flush

**contributing Run**:
A **Run** whose rows are in a **Dataset**. A **Dataset** name spans **Runs** — `lame` holds five
**Runs**' output — so "which **Run** made this" is a SET for a durable **Dataset** and a single
answer only for a **temporary Dataset** (which has exactly one owner) or a single dispatch's
partition. The listing carries both, never merged (**ADR 0017**): the plural set, and the singular
`runId` that is filled ONLY when the set has one member and is the key a **tag** or **rename**
addresses. Read from the LAKE's own per-file `run_id` statistics, which cost catalog metadata and
no scan — not from the materialization ledger, which has no record of any **Run** the SDK path
starts. Where a data file spans several **Runs** the statistics give a min and a max, so the set
becomes a BOUND and the surface says "at least N" rather than N.
_Avoid_: owner (that is the **temporary Dataset**'s one recorded **Run**, and it answers before any
row lands), author, writer, producer (a **Machine** produces; a **Run** contributes)

**Worker**:
A process polling for work on behalf of one **Actor** version.
_Avoid_: agent, machine (a **Machine** is hardware and belongs to Fleet)

**Ref**:
A content-addressed pointer standing in for a payload too large to carry inline.
_Avoid_: pointer, handle, claim check (that names the mechanism, not the value)

## Relationships

- A **Run** opens one or more **Sessions**; a **Session** activates exactly one **Actor**
- A caller opens a **Session**, calls its **Methods**, and closes it. Nothing else can address
  that **Session** — it is not discoverable and has no mailbox
- A **Method** call receives one **Batch** and yields one result per **Unit**, so a retry resumes
  mid-**Batch** rather than redoing it
- Many **Workers** may serve one **Actor** version; exactly one **Worker** serves one **Session**
- A **Session** may be **keyed**, which makes it exclusive across every **Run** and gives it
  durable state that outlives it. An unkeyed **Session** is private to its **Run** and shares
  nothing — keying is a claim on a shared identity, never a tax on an ordinary dispatch
- A **Run** publishes zero or more **Datasets**; a **Dataset** outlives the **Run** that wrote it
- A **Run** may open one or more **temporary Datasets**; each is owned by that **Run** and, unlike
  a **Dataset**, is not meant to outlive it — its recorded owner is what a later cleanup reads
- A **Run** PROMOTES rows out of a **temporary Dataset** into a durable **Dataset**; the promoted
  rows keep the **Machine** and **Actor** version that produced them, never the promoting **Run**'s
- Deleting a **temporary Dataset** is explicit and leaves any **Dataset** promoted FROM it fully
  intact — promotion is a COPY into the target's own files, so dropping the source cannot reach the
  target's rows; the DELETE route refuses a durable **Dataset**, which ages out by retention instead
- A **Run** reads **Datasets** in **Batches** and dispatches each to a **Method** — the loop is
  the caller's, which is what makes the interpreter unnecessary rather than merely optional
- A **Run** never causes a **Machine** to exist, and no code path reachable from one can

## Example dialogue

> **Dev:** "I re-ran the sweep and it finished in forty seconds with an empty **Dataset**.
> Nothing errored anywhere."
>
> **Domain expert:** "Your **Method** opens by checking a dedupe set in keyed state. That state
> belongs to the key, not to the **Run** — nothing about starting a new **Run** resets it. So
> every **Unit** was already seen and returned immediately. The **Dataset** is empty and
> **sealed**, and that is the honest answer to what you actually asked for. If you wanted a
> fresh crawl you wanted a fresh key, or no key at all."

## Flagged ambiguities

- **"worker" meant three things.** This glossary called a **Worker** a *machine*, `cli/scale.go`
  used it for a *container*, and the provisioning resource was named `worker` for a cloud
  instance. Resolved 2026-08-10: a **Worker** is a *process*; the hardware is a **Machine** and
  belongs to [Fleet](./control/orchestrator/src/infra/CONTEXT.md). One **Machine** may run several **Workers**, and on the
  fleet it runs exactly one, because each needs its own egress address.

- **"the record" was ambiguous across three stores.** Resolved by role, not by store: a
  **Dataset** is the durable record of what a **Run** produced, a **Ref** is how an oversized
  payload is carried, and a notification only says a **Unit** exists — it is never the record,
  because it does not outlive the **Run**. Sharpened 2026-08-13 when **Manifest** retired: there
  is now one word for "the durable record of what came out", not two separated only by whether
  somebody named it.

- **"Actor" does not mean the actor model.** Kontra's **Actor** is not a Hewitt/Erlang actor and
  not an Orleans *virtual* actor, and the word invites the assumption every time. Measured
  against Orleans' four facets of virtualization (Bernstein et al., MSR-TR-2014-41 §2.1):

  | Orleans | kontra v2 |
  |---|---|
  | **Perpetual existence** — "cannot be explicitly created or destroyed" | Opposite, and now deliberately so. A **Session** is opened and closed by a caller; between **Sessions** an **Actor** is not addressable at all |
  | **Automatic instantiation** — a *message* creates an activation | No messages, no mailbox. A caller opens a **Session**; nothing arrives unbidden |
  | **Location transparency** — `GetActor(key)` returns a reference | Closer than it was: a keyed **Session** is a reference, and routing to the one **Worker** holding it is the framework's job. Still not Orleans' — the reference dies with the **Session** |
  | **Automatic scale out** — single activation by default | **Holds**, and more strongly: Orleans guarantees it only "in failure-free times" and "eventually" otherwise (§3.9) |
  | **Turns** — "an activation executes one turn at a time" (§2.5) | Inverted, and more so in v2: the author writes the loop inside a **Method** and owns `self.*` safety, where Orleans' whole promise is that "locks and other synchronization primitives are unnecessary" |

  The paper rules this workload out in its own words: an application that "intermixes frequent
  bulk operations on many entities" is named as a pattern that "does not fit Orleans well".
  Kontra is bulk. The model was chosen against, not missed.

- **It IS a virtual object, though.** The narrower pattern — Restate's Virtual Object, the same
  shape as Cloudflare's Durable Objects — asks for a unique key, one write handler at a time per
  key, and K/V state attached to the key. All three hold for a **keyed Session**.

  The distinction that matters when reading either model's docs: Orleans promises an activation
  runs one **turn** at a time, which an author-written loop inverts; Restate serializes *handler
  invocations*, and a **Method** call is one invocation whose internal concurrency is its own
  business. Same code — fails one comparison, passes the other. Two different claims, not a
  contradiction.

  Still absent: Restate's concurrent read-only handlers. Every **Method** call takes the key
  exclusively.

- **"Batch" and "Dataset" are both ~110 bytes when materialized, but cost vastly different.**
  Resolved 2026-08-18 by **ADR 0028**: A **Batch** is a ~110-byte ref in a workflow's history,
  chainable to the next **Method** without touching the lake. A **Dataset** is named and costs
  materialization and queryability. The distinction matters because a caller deciding whether to
  re-page a result needs to know the cost — **Batch** is the cheap ref, **Dataset** is the named
  lake-resident artifact. The author sees the same parameter (named `dataset`) regardless; from
  inside a **Method** the destination must be indistinguishable. The caller's act of naming it
  decides whether it becomes durable. There is no such thing as an unnamed **Dataset** — omit the
  name and you get a **Batch**.

## Retired terms

- **Activity** — *"Deployed code that is a function: one call in, one value out."* Retired
  2026-08-13. An **Actor** with a single **Method** and no load/close is the same thing. The line
  between them was drawn on whether the code happened to hold a **Session** — an implementation
  property, not a domain one — and defending it cost a kind in the catalog, the CLI, the wire and
  both SDKs.

- **Monitor** — *"A durable loop that observes something over time and emits Units when it
  changes."* Retired 2026-08-13. A durable loop is a workflow. In v2 the loop lives on the control
  machine and the observation stays a **Method** call on the fleet, so nothing about the capability
  is lost — only the deployed kind. This also closes the older "**monitor** collided with the CLI
  verb" ambiguity by deleting one side of it.

- **Node** — *"One **Actor** placed in a graph, with the parameters that graph gives it."* Retired
  2026-08-13 with the interpreter. There is no graph, so there is no position in one. What a
  caller now writes is a **Session** and a sequence of **Method** calls.

- **Chunk** — *"The set of **Units** the graph hands downstream as one **Run**."* Retired
  2026-08-13 with the interpreter. Nothing is handed downstream implicitly; a caller reads a
  **Dataset** in **Batches** and decides what to do with each.

- **Manifest** — *"The durable, queryable record of which **Units** crossed one edge of a
  **Run**."* Retired 2026-08-13. With no graph there are no edges. A **Dataset** is the durable
  record, and it is named on purpose rather than produced as a side effect.

- **Turn** — *"one pass an **Actor** takes over its **Batch**."* Retired 2026-08-11 by **ADR
  0018**. It existed because the handler drove the actor through a broker under a fixed
  `StartToCloseTimeout`, so a long **Batch** had to hand control back before the clock ran out.
  With the actor serving its own Temporal activity and heartbeating for itself, there is nothing
  left for the word to name.

  Recorded rather than deleted because ADRs 0015–0016, the roadmap and the git history all use it
  heavily, and a reader meeting it there needs to know it is dead.
