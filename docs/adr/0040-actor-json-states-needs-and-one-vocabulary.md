# 40. `actor.json` states needs, the three axes are named for what they count, and there is one vocabulary

## Status

**Accepted.** Carries out a glossary change **0036** announced and never made, and amends **0037**'s "at most one Worker per Machine".

## Context

1. **`actor.json` still described a world 0036 retired.** It carried a `targets` block with two entries — `container: {memory, cpus, shmSize}` and `machine: {size, region, image}`. But 0036 §70 decided *"There is ONE Target, and it is a container. The `Target` axis in `infra/CONTEXT.md` collapses from a choice to a constant"*, and §74 gave the test the reversal had to pass: **"an actor author cannot tell which driver ran them."** A `targets` block is an author declaring hardware *per target* — an author saying which target will run them, which is the one thing that test forbids.

2. **The glossary never got the change.** 0036 announced the collapse; `infra/CONTEXT.md` still defined **Target** as *"a container, or a **Machine** of its own"* and **Artifact** as *"an **Image** for a container **Target**, a **Bundle** for a machine **Target**"*. An ADR promised a glossary edit that nothing carried out.

3. **`size`/`region`/`image` were a second source of truth.** `fleet.hold()` already takes `region=` and `size=`. Two writers of one fact, and the failure when they disagree is not an error — 0034 records it: Machines in a region that cannot reach the Controller, and a run that **hangs on `ready()`** until cancelled. Region is also a per-run operational choice, not a property of an actor: the same crawler wants a different region depending on what is blocking it that week.

4. **`manifest` named two things and was in no glossary.** `readManifest()` reads `actor.json`; `bundle.go` and `deploy.go` use "manifest" for the **OCI manifest**. The obvious alternative was worse: the README called `actor.json` the actor's *identity*, and the **Warden** has an identity — `wardenIdentity`, `loadIdentity`, the mTLS certificate. This repository has now paid three times for one undefined word naming several things (`campaign`, `ledger`, and this).

5. **There was no way to run several containers of one actor on one Machine.** 0037 made it structural: *"A Placement puts at most one Worker on any one Machine… Two of one `<actor>@<version>` there would carry the same `KONTRA_WORKER` label, write the same units and poll the same queue, so nothing could tell them apart."* The only concurrency available was **Sessions** — async slots inside one process — which does not give CPU parallelism and does not give crash isolation.

6. **`workers=` counted Machines.** Slice 11 added it meaning *"how many of the Fleet's Machines this Artifact lands on, one Worker each"*, and its own note had to explain that it *"counts Machines and not processes"*. A parameter that has to disclaim what it counts is misnamed.

## Decision

- **`targets` is deleted. `actor.json` states `needs`.**

  ```json
  {
    "schemaVersion": "kontra.actor.v1",
    "name": "nscheck",
    "version": "0.1.0",
    "needs": { "memory": "512m", "cpus": 1 }
  }
  ```

  One set of requirements, because there is one Target. `size`, `region` and `image` leave `actor.json` entirely and belong to the provider constructor.

- **`needs` rides the config blob, not annotations.** The artifact already carries `bundleConfig{Name, Version, Engine}` under `application/vnd.kontra.bundle.config.v1+json`, and `shared/conformance/bundleref.json` already pins that media type with the reason: *"the config type is what makes the engine part of the digest rather than a mutable file beside it."* The same argument carries `needs`: **what an actor requires becomes part of what it is.** Annotations would have been readable one round trip sooner — ~20 ms against a 65 MiB pull — in exchange for flattening structured data into strings with no media type to version.

- **It is called `actor.json`.** No abstract noun. `manifest` belongs to OCI; `identity` belongs to the Warden. A filename cannot collide, which is what the last three collisions cost.

- **Providers are separate constructors, finishing 0034 §3.** That ADR already decided *"THE PROVIDER IS IN THE CLASS NAME AND NOWHERE ELSE… IT IS NOT A UNION WITH A `provider:` FIELD"*, because a DigitalOcean VPC is regional so region and VPC are one fact. `do_fleet()` takes `region`/`size`; a provider that adopts existing capacity takes neither. `hold()` stops accepting loose `region=`/`size=` beside a provider object — it already refuses the mix, which was the tell that the shape was unfinished.

- **Three axes, each named for what it counts:**

  ```python
  await f.place("nscheck", "0.1.0", machines=4, containers=3, workers=8)
  ```

  `machines` — how many Machines the placement lands on.
  `containers` — how many containers of it run on each Machine.
  `workers` — how many concurrent units run inside each container.

  This amends 0037: a Placement may now put several containers on one Machine, so each needs a distinguishable label, unit name and entry in the driver's `list()`. They may share a task queue — Temporal fans out across pollers — but they may not share an identity.

- **One vocabulary, and the wire moves with it.** **Worker → Container**; **Session → Worker**. Everywhere: symbols, comments, ADRs, corpora, the `KONTRA_WORKER` environment label, and the `<name>-<version>-sessions` task queue suffix.

## Considered options

**Rename `targets` to `specs` and keep both blocks.** Rejected: it preserves the thing 0036 retired, and a reader would reasonably infer kontra still chooses between running in a container and running on a machine.

**Keep an optional `machine: {size}` hint.** Rejected: it reintroduces two sources of truth in a quieter form, and adds a precedence rule to remember.

**Caller-facing rename only** — `machines/containers/workers` at the API, `Worker`/`Session` internally. Genuinely attractive, and rejected deliberately: it leaves two vocabularies with the boundary inside one function signature, and a Warden logging "Worker started" would mean the thing the API calls a container.

**Freeze the wire and rename only the words.** Rejected because of *when* this is being done, not because the migration is cheap. See consequences.

## Consequences

- **The rename is free, and only because it happens before `v0.1.0`.** There are **zero tags** and nothing has ever been published, so no external consumer exists to break. The only systems at risk are the authors' own controller and any Fleet running during the upgrade. After the first tag this same change would require every actor rebuilt and every Warden upgraded in lockstep. **This is the window.**

- **A Fleet must be drained before upgrading.** `KONTRA_WORKER` is what the `/proc` scan and `podman ps` parse to answer "what is running here". A Warden that has learned the new label cannot see containers started under the old one, so it will start duplicates — which is exactly what driver rule 3 ("what is running comes from `list()`, never from a file") exists to prevent. A version bump is not sufficient; a documented drain-first procedure is.

- **The task queue suffix moves**, `<name>-<version>-sessions` → `<name>-<version>-workers`. It is derived independently in four languages and pinned by `shared/conformance/queues.json`, whose own header names the failure: *"the actor registers, polls a queue nobody schedules onto, and reports as a healthy idle Worker while every run hangs to StartToClose."* The corpus is what makes this survivable — a one-sided rename fails a test in another language.

- **The glossary changes rung.** What was a **Worker** is a **Container**; what was a **Session** is a **Worker**. `CONTEXT.md`'s `_Avoid_` line currently reads *"worker (a **Worker** is a process, not code)"* and must be rewritten rather than deleted. **Target** is already retired in favour of **Driver** — a property of the Machine's environment, not of the actor.

- **The glossary must not be renamed ahead of the code.** ~3,064 sites use `worker`/`Worker`, including `workerHandle` (135), `workerSpec` (82), `workerDriver` and `workerPart`. A glossary that describes a vocabulary no code uses is worse than one that lags. They move in the same change.

- **`kontra fleet up --actor <dir>` loses its size.** It reads the machine size from `actor.json` today. It needs `--size` or a default **in the same commit as the schema change**, or it silently provisions whatever the fallback is.

- **`containers=` buys crash isolation and real parallelism**, which `workers=` inside one process cannot: a CPU-bound actor gets more than one core, and a crash takes down one container rather than every concurrent unit on the Machine.
