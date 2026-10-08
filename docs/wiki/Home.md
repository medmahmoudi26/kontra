<picture>
  <source media="(prefers-color-scheme: dark)" srcset="https://raw.githubusercontent.com/medmahmoudi26/kontra/dev/.github/assets/kontra-logo-dark.svg">
  <img src="https://raw.githubusercontent.com/medmahmoudi26/kontra/dev/.github/assets/kontra-logo-light.svg" alt="kontra" width="238" height="48">
</picture>

# Kontra

Kontra runs a **job over a batch of inputs**. You write the job once — a small Python (or Go) file with one unit-processing function — and Kontra runs it over your inputs in parallel, retries failures, isolates bad units, offloads large data to object storage, and resumes from where it stopped if anything crashes. Your actor process *is* a **[Temporal](https://temporal.io) activity worker** (durable state on Redis); a single Go **handler** owns the backing workflow, schedules each batch onto the actor, and owns the retry/resume-from-committed reload. An **orchestrator** composes many such jobs into a visual workflow graph, and every run's output lands as a SQL-queryable dataset.

## The three ideas

1. **An actor is the unit of work.** A directory with `actor.py` + `actor.json`: typed Methods (`takes=`/`emits=`, whose JSON Schemas are *derived* — no hand-written schema files) and a `load → methods → close` lifecycle. One actor = one worker = one image = one task queue.
2. **The caller owns Batches; the actor owns resources.** A call processes **one Batch** — the wire contract carries no sharding knobs, and the paging is the caller's loop (`catalog.dataset(name).batches(size, order_by=…)`). *Within* the call the concurrency is the Method author's: the framework hands over the whole Batch and does not schedule inside it. One Batch = one activity call = one Session ([[Execution-Model]]).
3. **Refs are the currency.** Bulk data never rides in workflow histories or the orchestrator's memory: a claim-check codec content-addresses it into an S3 store (SeaweedFS locally), and everything above trades small `{sha256, size}` refs.

## Also in the box

- **The `kontra` CLI** (`cli/`, Go) — `kontra doctor` (infra state + web consoles), `kontra infra up|down|status`, `kontra deploy --actor <dir>` (builds + pushes a self-contained worker image), `kontra workers list`, `kontra runs list`, `kontra dataset list|query|create|tag|rename`, and `kontra workflow serve|start`: the command-line front door to the stack ([[Dev-Cycle]]).
- **`kontra dataset`** — the data surface, one noun for both directions. `kontra dataset list` shows the lists you loaded *and* every actor's output; `kontra dataset query <name> --sql "…"` runs on the orchestrator's already-open connection (~50 ms; `--local` runs DuckDB on your workstation instead), and the web **Query** workbench runs the same engine. A Dataset carries a derived, run-grain **name** and a set of **tags** — untagged output expires on a TTL, and a tag is what keeps it ([[Query-Surface]]).
- **`kontra explore <actor[@version]> --dt <when>`** — one dispatch's typed output (the actor's own columns, nested output kept as `STRUCT`/`LIST`), opened in DuckDB **on your machine** over short-lived presigned URLs scoped to that dispatch. Addressed by actor and time, never by a run UUID ([[Query-Surface]]).
- **Your own workflows** — the *only* dispatcher, since ADR 0023 §12 deleted the graph interpreter. `kontra.catalog` lets a Temporal workflow you write and run yourself drive deployed actors (`results, dropped = await dns.addrs(batch, out)` — a Batch in, a Batch out, both refs, with the caller's output **Dataset** as the third parameter). For the shapes a fixed topology cannot hold: a loop, a branch, a fan-out sized from the last result. A run can even provision the machines it needs inside its own scope (`kontra.fleet.up()`). `kontra workflow serve|start` ([[Execution-Model]], ADR 0021, ADR 0028).
- **The per-unit blob plane** — with an object store configured, each unit's output streams to `units/{run_id}/{node_id}/u{i}.json` the moment it completes and the durable commit holds only a small `$ref`; a node's output is an S3 prefix a query engine can scan directly ([[Data-Plane]]).

## Where to go

| You want to… | Read |
|---|---|
| Look a word up | [[Glossary]] |
| Run something in 5 minutes | [[Getting-Started]] |
| Write a Python actor | [[Writing-Actors-Python]] |
| Write a Go actor | [[Writing-Actors-Go]] |
| Pick the OS an actor runs on | [[Runtimes]] |
| Add or fork a runtime | [[Writing-a-Runtime]] |
| See what the image store is holding — the console surface, designed and not yet shipped | [[The-Images-Page]] |
| Understand how a run executes | [[Execution-Model]] |
| Drive Actors from your own workflow | [[Writing-Workflows]] |
| Understand failure handling & recovery | [[Durability-and-Failures]] |
| Understand storage & datasets | [[Data-Plane]] |
| Query a run's output / read its lifecycle | [[Query-Surface]] |
| Watch what a Worker is printing, across the fleet | [[Dashboard]] |
| Deploy the stack (local / remote workers) | [[Deployment]] |
| Follow the day-to-day loop | [[Dev-Cycle]] |
| Look up a CLI verb | [[CLI-Reference]] |
| Configure tokens, images, manifests | [[Configuration]] |
| Tour the examples | [[Examples]] |
| The cross-language contracts & compat gates | [[Contracts]] |
| Decision records | [[ADRs]] |

## Scope

Kontra is **local-first**: it is designed to run whole on one box, and the Compose topology binds to
loopback by default because loopback is the security control for services that have none of their own.

It is no longer *unauthenticated*. The API is gated by four separate tokens — state, explore, run and
panel — and the console signs in against a scrypt-hashed account in `~/.kontra/config.yaml` to mint a
session (ADR 0045). What each one admits, and which surfaces are open by design, is in
[[Security-Model]] and [[Configuration]].

What is still out of scope: multi-tenancy, and sandboxing actor code from the Machine it runs on.
[[Fleet-and-the-Warden]] is explicit that the Warden runs code you did not write.
