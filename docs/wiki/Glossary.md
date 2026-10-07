# Glossary

The vocabulary is small and load-bearing. Most of these words name a thing that is *almost* another
thing on this list, and the pairs are where mistakes come from — so each entry says what it is and,
where it matters, what it is **not**.

Canonical definitions live in [`CONTEXT.md`](https://github.com/medmahmoudi26/kontra/blob/dev/CONTEXT.md)
and [`control/orchestrator/src/infra/CONTEXT.md`](https://github.com/medmahmoudi26/kontra/blob/dev/control/orchestrator/src/infra/CONTEXT.md).

---

## The work

### Actor
Your job, as a directory: `actor.py` (or Go) plus `actor.json`. It has typed **Methods** and a
`load → methods → close` lifecycle. One Actor = one worker = one image = one task queue.

An Actor is **not** a process — it is the *definition*. The process is a **Worker**.

### Method
One typed entry point on an Actor, declared with `takes=` and `emits=`. It receives a whole
**Batch** and the caller's output **Dataset**, and *you* write the loop over it. Its JSON Schema is
**derived from the code**, never hand-written — `kontra actor schema <dir>` prints it.

### Unit
One item of work: one row of the input. It is the unit of **failure** and the unit of
**resumption** — a Worker killed halfway resumes at the Unit it reached, and one Unit raising does
not take the Batch with it.

### Batch
The collection a Method is called with. **Batch in, Batch out, nothing materialized**: both sides
are content-addressed refs, not rows in a workflow history.

> **The caller owns Batches; the Actor owns resources.** The wire contract carries no sharding
> knobs. Paging is the caller's loop (`catalog.dataset(name).batches(size, order_by=…)`);
> concurrency *inside* a Batch is the Method author's.

### Session
One live process on a **Machine** that Units are handed to. One Batch = one activity call = one
Session.

### Dataset
SQL-queryable output, readable **while the Run is still open**. A Dataset carries a derived,
run-grain **name** and a set of **tags**.

> **A Dataset's name and its run-grain name are two different columns**, and the console shows them
> as two. `canary_signals` is the logical Dataset; `wf-canary-1.0.0--30 Sep 2026 22:40:18--e7c41a`
> is the partition one execution wrote into it.

### Dataset state
`open` · `sealed` · `abandoned` — and **absent is a fourth thing**, not a synonym for `open`.

| state | means |
|---|---|
| `open` | nobody has said yet. What a crash or a run still in flight leaves. |
| `sealed` | the Run that wrote this finished. Nothing more is coming, and that is the intended end. |
| `abandoned` | the Run stopped without finishing. Nothing more is coming, and the result is short. |
| *(no marker)* | no writer ever wrote one. Inventing one would claim a writer touched this. |

### Run
One execution of **your** workflow. Addressed by its workflow id everywhere — the console, the
logs rail (`run_id:"<id>"`), and `kontra runs --run-id`.

---

## The capacity

### Fleet
Tagged capacity: **Machines** that Containers are scheduled onto. A Fleet is **not owned by one
Run** — that is the whole point of the **Lease**.

### Lease
One Run's claim on a Fleet. Machines die when the **last** Lease drops, not when the first one
does. A run takes a Lease before it provisions anything and drops it after, so capacity is never
held by a run that has finished.

### Machine / Container / Worker
The three axes, and they are routinely confused:

| | what it is |
|---|---|
| **Machine** | a host — a Droplet, or a Warden container on the Compose network |
| **Container** | a process on that host |
| **Worker** | a concurrent unit inside that process |

Spelled together: `place(machines=4, containers=3, workers=8)`.

### Warden
The one process kontra installs on a Machine. It reconciles what the control plane asked for
against what the runtime actually holds, **outbound only** — a Machine opens no listening socket.
It runs no actor code itself.

### Driver
How the Warden runs a Container — `podman` or `process`. **An Actor cannot tell which.**

### Controller
The endpoint a Warden enrols against and reports to. For a cloud Fleet it must be an address a
Droplet can actually reach — *a Compose service name is not one*.

### Placement
Putting a built Actor on a Fleet: `await f.place(name, version, sessions=N)`.

---

## The artifacts

### Bundle
The Actor's **artifact** — the code a Machine fetches, published as an OCI artifact to
`<registry>/bundles/<name>:<version>`. Built by `kontra build --actor <dir>` (ADR 0036).

### Image
The container **Image** spelling of the same idea, built by `kontra deploy --actor <dir>`. A
Bundle is fetched by a Warden; an Image is pulled by a Docker daemon. The Actors page lists both
and labels which is which.

Its layers have names once it is built by buildpacks — see **Actor image** under
[the image store](#the-image-store).

### Workspace
A named directory under `KONTRA_WORKSPACES` holding `actors/` and `workflows/`. It is also an
**isolation boundary**: each workspace gets its own DuckLake catalog, so a console pointed at the
wrong workspace shows an empty install rather than someone else's data.

### Scratch
A Run's working area for intermediate values that are not Dataset output.

---

## The image store

The nouns of a build. Several of these name parts of **one** image and get swapped for each other, so
each entry says which part of the stack it is. [[Runtimes]] is the long form.

### Runtime
The OS plus the system packages an actor runs on, published as a CNB **run image** from
`kontra-runtimes` (`python`, `python-browser`, `base`, …). Declared in `actor.json` by name and
**major** (`python-browser:1`), which a build resolves to a digest — with the resolver written in
`cli/runtimes.go` and no build path calling it yet.

A Runtime is **not** where dependencies go — that is the lockfile — and **not** where an interpreter
version goes, which is `.python-version`.

> Unrelated to the `runtime/` directory in this repository, which is the host that loads an actor
> beside it on a Machine. Same word, different axis.

### Builder
The CNB builder that turns an actor directory into an image. The design is **one builder per install,
pinned by digest** (`heroku/builder:24`), with every Runtime compatible with it — but nothing in this
repository pins one yet: `cli/packbuild.go`'s `Builder` field has no caller that sets it, and there is
no builder constant, variable or `builder.json` here. The pin belongs in `kontra-runtimes`
([[Writing-a-Runtime]]).

A Builder is **not** a Runtime: the builder is the thing that *does* the build and is thrown away;
the runtime is what the result sits on and ships inside it.

### Actor image
The output of a build: **runtime layers, then dependency layers, then the app layer.** Identified by
digest, exactly as before (ADR 0032) — what buildpacks add is that the layers have names and a
recorded source.

### Rebase
Moving an Actor image onto a newer digest of **the same Runtime major**, by rewriting its manifest
with no rebuild. The result is a **new digest for the same actor version** — so a version maps to a
*sequence* of digests, and the catalog says which is current.

Rebase is **not** a rebuild: the deps and app layer digests are unchanged, which is both why it is
cheap and why it is safe to do to every actor at once. Not yet implemented.

### Layer kind
`runtime`, `deps` (one buildpack layer, for example `heroku/python:venv`) or `app`. To be **read** from
the image's `io.buildpacks.lifecycle.metadata` label, never guessed — and joined to the manifest through
`config.rootfs.diff_ids` by index, because the label records uncompressed digests and the manifest
records compressed ones. Nothing reads it yet: the reader is the Images page
([[The-Images-Page]]), which is designed and not built.

### `inuse-` tag
A tag the control plane puts on a digest to exempt it from retention: without it, "keep the 5 most
recently pushed" deletes the image a running actor was placed from. A reconciler in the orchestrator
writes them, armed in the API role and running every 10 minutes.

It covers **one** of the three sources the design names — the digest the catalog currently records for
an actor. A digest that is merely *placed on a Machine*, or referenced by a *run inside its retention
window*, is not tagged, because neither is readable from a table. Retention therefore still ships in
`dryrun` ([[Durability-and-Failures]]).

---

## The data plane

### Ref
`{sha256, size}`. **Refs are the currency**: bulk data never rides in a workflow history or the
orchestrator's memory. A claim-check codec content-addresses payloads into an S3 store (SeaweedFS
locally) and everything above trades refs.

### DuckLake
The lake: a Postgres **catalog** plus a `DATA_PATH` on S3. Metadata tables are
`__ducklake_metadata_<name>`.

> **Never hand-edit DuckLake's Postgres metadata.** And `count(*)` answers from file statistics, so
> it reports dead partitions as full — only `SELECT * … LIMIT 1` proves a table is readable.

### Tag / TTL
Untagged output expires on a TTL; a **tag** is what keeps it. Tags are a *set*, and the record is
keyed by `runId`.

---

## Confusable pairs, side by side

| | |
|---|---|
| **Actor** vs **Worker** | the definition vs the process running it |
| **Bundle** vs **Image** | fetched by a Warden vs pulled by a Docker daemon |
| **Runtime** vs **Builder** | what the image sits on vs what built it |
| **Rebase** vs **rebuild** | a new digest with the same deps and app layers vs a new everything |
| **Batch** vs **Unit** | the collection vs one item of it |
| **Fleet** vs **Lease** | the capacity vs one Run's claim on it |
| **Dataset name** vs **run-grain name** | the logical table vs one execution's partition |
| **`open`** vs **absent** | nobody has said yet vs nobody ever wrote a marker |
| **`sealed`** vs **`abandoned`** | finished as intended vs stopped short |
| **Machine** vs **Container** vs **Worker** | host vs process vs concurrency inside it |

---

**See also:** [[Execution-Model]] · [[Data-Plane]] · [[Fleet-and-the-Warden]] · [[Contracts]] ·
[[Runtimes]]
