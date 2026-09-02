# kontra

**Run a job over a batch of inputs, across a fleet you don't have to babysit.**

You write the job once — a small Python or Go file whose **Methods** each take a whole **Batch** and push records to the caller's **Dataset**. kontra runs it across a fleet, retries failures, isolates bad units, offloads large payloads to object storage, and resumes from the last committed **Unit** if anything crashes. Your actor *is* a [Temporal](https://temporal.io) activity worker: it polls its own task queue and keeps durable state in Redis, while a single Go handler owns the workflow and the exactly-once reload.

**You compose in code, not on a canvas.** A **Run** is one execution of *your own* Temporal workflow, which pages a **Dataset** into Batches and drives deployed **Actors** with ordinary control flow — a loop, a branch, a fan-out whose width depends on what the last Actor returned.

```python
async with fleet.hold(tag="dns", machines=4) as f:          # capacity, and a Lease on it
    await f.place("nscheck", "0.1.0", containers=3, workers=8)  # what runs on it
    await f.ready()                                          # …and Containers actually POLLING
    async for batch in domains.batches(100, order_by="domain"):
        verdicts, dropped = await ns.ask(batch, out)
# scope exit drops the Lease. The Machines die when the LAST one does.
```

That teardown is a replayable step in a program Temporal finishes whether or not the process that started it still exists. A script that provisions ten machines and dies leaves ten machines. This cannot.

---

> [!WARNING]
> **kontra is `0.x` and the API will move.** It is used in production by its authors and is not yet stable for anyone else. Pin an exact version, read the release notes, and expect breaking changes between minors until `1.0`. See [Versioning](#versioning).

---

## Where the actors and workflows are

**This repository contains no actors and no workflows.** They live in two repositories of their own:

- **[kontra-actors](https://github.com/medmahmoudi26/kontra-actors)** — the capabilities
- **[kontra-workflows](https://github.com/medmahmoudi26/kontra-workflows)** — the programs that call them

Fork either to write your own.

The only actor here is `testdata/fixtureactor/`, which exists so kontra's own test suite has something to dispatch to. It is deliberately boring and is not a template.

## Install

```bash
curl -fsSL https://raw.githubusercontent.com/medmahmoudi26/kontra/main/install.sh | sh
kontra up
```

One command, one process: Temporal, the object store, the state store, the payload codec, an OCI registry and the orchestrator — with its own data directory and no containers. Four platforms are published (linux and macOS, amd64 and arm64); the installer picks yours, checks it against the release's `SHA256SUMS`, and refuses to unpack anything that does not match.

## The shape of it

```
        CONTROL PLANE                          A MACHINE
   ┌──────────────────────┐          ┌────────────────────────────┐
   │ Temporal             │◄─────────┤ warden   (Temporal Worker,  │
   │   per-tenant ns      │  blocked │           blocked on signal)│
   │ orchestrator         │◄─────────┤    ├─ actor + handler  ×N   │
   │   pane snapshots     │ telemetry│    └─ metrics agent         │
   │ OCI registry (yours) │◄─────────┤    image pull by digest     │
   └──────────────────────┘  outbound└────────────────────────────┘
```

**Every arrow is dialled by the Machine.** No inbound ports, so NAT, a private VPC and a customer's firewall all work unchanged. Telemetry never touches workflow history. kontra owns no registry — you push artifacts to one you control.

## Concepts

The vocabulary is small and load-bearing. Full definitions live in [`CONTEXT.md`](CONTEXT.md) and [`control/orchestrator/src/infra/CONTEXT.md`](control/orchestrator/src/infra/CONTEXT.md).

| | |
|---|---|
| **Actor** | your job — typed Methods with a `load → methods → close` lifecycle |
| **Method** | receives a whole **Batch** and the caller's output **Dataset**; you write the loop |
| **Batch** / **Unit** | the work, and one item of it. Every `push` is durable as you move through it |
| **Dataset** | SQL-queryable output, readable while the Run is still open |
| **Run** | one execution of *your* workflow |
| **Fleet** | tagged capacity: Machines that Containers are scheduled onto. Not owned by one Run |
| **Machine** / **Container** / **Worker** | the three axes: a host, a process on it, a concurrent unit inside that. `place(machines=4, containers=3, workers=8)` |
| **Lease** | one Run's claim on a Fleet. Machines die when the last Lease drops |
| **Warden** | the one process kontra installs on a Machine. Runs no actor code itself |
| **Driver** | how the Warden runs a Container — `podman` or `process`. An actor cannot tell which |

## Documentation

The **[wiki](../../wiki)** is the manual — start at [Getting Started](../../wiki/Getting-Started).

| | |
|---|---|
| [Getting Started](../../wiki/Getting-Started) · [Dev Cycle](../../wiki/Dev-Cycle) | install, run one, iterate |
| [Writing Actors: Python](../../wiki/Writing-Actors-Python) · [Go](../../wiki/Writing-Actors-Go) | the authoring surface |
| [Execution Model](../../wiki/Execution-Model) · [Durability](../../wiki/Durability-and-Failures) | what happens when things break |
| [Data Plane](../../wiki/Data-Plane) · [Query Surface](../../wiki/Query-Surface) | where records go and how to ask |
| [Fleet & the Warden](../../wiki/Fleet-and-the-Warden) | machines, Leases, and running code you did not write |
| [Deployment](../../wiki/Deployment) · [Security Model](../../wiki/Security-Model) | operating it, and what it does and does not isolate |
| [`docs/adr/`](docs/adr) | every architectural decision, with its trade-offs |

**The ADRs are worth reading before the code.** They are unusually candid — several record a decision *and* the measurement that later corrected it.

## Repo layout

```
cli/          the kontra binary — up, build, serve, fleet, warden, dataset, workflow
handler/      the Go Temporal handler: one per actor, owns the backing workflow
sdk/          the authoring surface you import          (Apache-2.0)
  go/         actor + caller
  python/     actor + caller + fleet
runtime/      the hosts that run an actor                (AGPL-3.0)
backend/      the orchestrator: catalog, datasets, fleet, panels  (TypeScript)
core/         @kontra/core — the shared kernel the console imports  (Apache-2.0)
infra/        Pulumi programs; machines, and what is written on them
conformance/  the corpora — one contract, every implementation drives it
testdata/     the fixture actor kontra's own tests dispatch to
docs/         ADRs and the wiki source
```

**The console is not here.** It lives in [kontra-console](https://github.com/medmahmoudi26/kontra-console) and depends on `@kontra/core` — this repository's `core/` — so the two halves read a Run through one set of declarations rather than two (ADR 0041). It ships as a content-addressed artifact the release pins by digest, which is why building kontra does not need it.

### The conformance corpora

`conformance/*.json` is how a contract with more than one implementation stays honest. Each file is inputs and expected outputs; every language that implements the rule drives the same file. A drift fails a test **in the other language**, which is the only arrangement that makes a one-sided rename impossible.

They exist because the alternatives were tried and measured: a hand-copied golden that could not differ, a source scrape that broke on refactors and passed on drift, and a count in a comment that three files disagreed about. See [ADR 0035](docs/adr).

## Versioning

**One version for everything.** A single tag `v0.4.0` moves `sdk/go`, `runtime/go`, `sdk/python`, the CLI and the handler together.

They are not independently useful: an actor imports the SDK, links the runtime, is built by the CLI and runs against the handler — all four must agree on the wire. Versioning them separately would invent a compatibility matrix that nothing tests.

```bash
go get github.com/medmahmoudi26/kontra/sdk/go@v0.4.0
pip install kontra-sdk==0.4.0
```

While `0.x`, a minor version may break. That is what `0.x` is for.

## Licence

**AGPL-3.0** for the control plane, runtime, CLI and console — see [`LICENSE`](LICENSE).

**Apache-2.0** for `sdk/go` and `sdk/python` — see [`LICENSE.SDK`](LICENSE.SDK). Your actor links the SDK, and your actor is yours; a copyleft SDK would reach into your code, which is not the intent.

If AGPL does not suit your organisation, a **commercial licence** is available. Open an issue or contact the maintainer.

## Security

kontra runs code it did not write, on machines it may not own. [`SECURITY.md`](SECURITY.md) is the disclosure process; the [Security Model](../../wiki/Security-Model) page states what is isolated, what is not, and where the boundaries actually are — including the ones that are still being built.

Please report vulnerabilities privately rather than as public issues.

## Contributing

See [`CONTRIBUTING.md`](CONTRIBUTING.md). Issues and discussion are very welcome. Note the licensing position before opening a pull request.
