# Examples

**Actors and workflows each live in their own repository.**

| | |
|---|---|
| [kontra-actors](https://github.com/medmahmoudi26/kontra-actors) | actors — the capabilities |
| [kontra-workflows](https://github.com/medmahmoudi26/kontra-workflows) | workflows — the programs that call them |

An Actor is a capability someone publishes; a workflow is a program someone runs. Different authors,
different lifecycles — a client writes workflows against actors they did not write.

This repository ships no actors. That is deliberate — an actor is *your* code, and kontra is the thing that runs it. Keeping them apart means you fork a repo of actors and point it at your own registry, rather than carrying a framework's examples around forever.

## Where to start

| | |
|---|---|
| **[python/probe](https://github.com/medmahmoudi26/kontra-actors/tree/main/python/probe)** | the smallest complete actor — one Method, no dependencies, ~100 lines |
| **[go/nscheck](https://github.com/medmahmoudi26/kontra-actors/tree/main/go/nscheck)** | two Methods on one Actor, where the first fans one input out into many Units for the second |
| **[kontra-workflows](https://github.com/medmahmoudi26/kontra-workflows)** | the caller side — the programs that page a Dataset and drive those Methods |

And the two guides that answer the questions this page used to:

- **[Running an actor locally](https://github.com/medmahmoudi26/kontra-actors/blob/main/docs/running-locally.md)** — no build, no registry, no fleet
- **[Running a Warden](https://github.com/medmahmoudi26/kontra-actors/blob/main/docs/running-a-warden.md)** — on a machine you own

## The one actor that is here

`testdata/fixtureactor/` exists so kontra's own test suite has something to dispatch to — the parity gate serves it, and the `build-actor` CI template builds it.

**It is not a template.** It is deliberately boring, does nothing useful, and is not documented beyond this sentence. Copy something from `kontra-actors` instead.

## Writing your own

Fork `kontra-actors`, copy the closest thing to what you want, and change it. The authoring surface is documented here:

- [[Writing-Actors-Python]]
- [[Writing-Actors-Go]]
- [[Execution-Model]] — what a Batch is, and why a Method receives the whole thing
