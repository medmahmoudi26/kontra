<picture>
  <source media="(prefers-color-scheme: dark)" srcset=".github/assets/kontra-logo-dark.svg">
  <img src=".github/assets/kontra-logo-light.svg" alt="kontra" width="238" height="48">
</picture>

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

> [!CAUTION]
> ## kontra is unstable, in active development, and not ready for production use by anyone but its authors.
>
> **What that means concretely, so you can decide rather than guess:**
>
> - **The API moves between minor versions.** `0.x` is what `0.x` is for. Pin an exact version.
> - **Documented commands may be ahead of, or behind, the code.** The install above is real and
>   tested; the `kontra up` half of ADR 0052 is still landing, so anything you read about that
>   command may be ahead of the code.
> - **The images the install pulls have not been published yet.** `:dev` is built by
>   `.github/workflows/publish.yml`, which has never run — there is no tag. Until one exists, use
>   the from-a-clone path below. This is the last thing between the two commands above and working.
> - **Boundaries described in the [Security Model](../../wiki/Security-Model) are partly built.**
>   The data plane has no tenant boundary: any actor on any Machine can read every Dataset on that
>   control plane. Run one control plane per tenant, and read that page before running untrusted
>   actors.
> - **`docker compose down -v` destroys every Dataset and the Pulumi state that tracks Machines you
>   own** — including cloud Machines it will then be unable to find or destroy.
> - **Upgrading is `docker compose pull && docker compose up -d`, and nothing verifies it.** That
>   replaces containers and keeps volumes, so it is the intended path — but there is no schema
>   migration and no check that an image and the volume it opens agree. `kontra update`, which adds
>   both, is designed (ADR 0052 §7) and not shipped.
>
> Issues and discussion are very welcome. Production dependency is not advised.

See [Versioning](#versioning) for what a minor bump is allowed to break.

---

## Where the actors and workflows are

**This repository seeds one actor and one workflow** (`examples/python/hello`, `examples/workflows/hello`)
so a first-time cluster has something to run. Everything else lives in two repositories of their own:

- **[kontra-actors](https://github.com/medmahmoudi26/kontra-actors)** — the capabilities
- **[kontra-workflows](https://github.com/medmahmoudi26/kontra-workflows)** — the programs that call them

Fork either to write your own.

The only actor here is `testdata/fixtureactor/`, which exists so kontra's own test suite has something to dispatch to. It is deliberately boring and is not a template.

## Install

One file. Docker is the only prerequisite — no clone, no build, no `kontra` binary.

```bash
curl -O https://raw.githubusercontent.com/medmahmoudi26/kontra/dev/docker-compose.yml
docker compose up -d --wait
docker compose exec cli cat /var/lib/kontra/console-password
```

The console is on <http://127.0.0.1:8088> (ADR 0047). Sign in as `admin` with the password that
last line prints.

The password is **never written to stdout**, because `init` runs in the `cli` container and stdout
there is `docker compose logs` — readable by anyone in the docker group. It is generated into that
0600 file instead, and `config.yaml` keeps only an scrypt hash.

`docker-compose.yml` is the whole install: every default in it resolves to a published image, and
the two scripts it used to need from a checkout are now an inline `configs:` entry and an image. It
creates `./workspaces` for your code beside itself and seeds `hello/` into it on first boot, so
there is nothing to make first.

<details>
<summary><b>Optional:</b> a second file, for ports, the bind address, and the query workbench</summary>

```bash
curl -o .env https://raw.githubusercontent.com/medmahmoudi26/kontra/dev/.env.quickstart
```

Every knob is in there with a comment. The four people actually come for are `KONTRA_BIND` (read
the note beside it), the port block if something on your machine already holds 8088,
`KONTRA_EXPLORE_TOKEN` to turn on the query workbench and the logs rail, and `KONTRA_WORKSPACES` to
keep your code somewhere other than `./workspaces`.

It is fetched as `.env` because a repository cannot ship a tracked `.env` without a developer's own
ignored one shadowing it.

</details>

<details>
<summary><b>From a clone instead</b> — contributors, and anyone running unpublished code</summary>

The published images are built from `dev`. To run what is in your working tree, build them under the
**same names the install resolves** and tell compose not to reach for a registry:

```bash
git clone https://github.com/medmahmoudi26/kontra-console.git
git clone https://github.com/medmahmoudi26/kontra.git
cd kontra
R=ghcr.io/medmahmoudi26

make image                                            # needs kontra-console beside this checkout (the SPA)
docker build -f control/images/Dockerfile.orchestrator --build-arg SPA_IMAGE=$R/kontra:dev -t $R/kontra-orchestrator:dev .
docker build -f control/images/Dockerfile.pyworker  -t $R/kontra-host:dev .
docker build -f control/images/Dockerfile.logship   -t $R/kontra-logship:dev .

cp .env.quickstart .env
echo 'KONTRA_PULL_POLICY=never' >> .env
docker compose up -d --wait
```

**Build under the qualified names, not bare ones.** Docker resolves by *name*, so
`ghcr.io/medmahmoudi26/kontra:dev` and `kontra:latest` are two names for the same bytes — build the
second and the install still goes to the registry for the first. `make image` and `make worker-base`
already write the qualified names (`KONTRA_IMAGE` / `KONTRA_WORKER_BASE_IMAGE` override them), which
is why `make image` is enough for two of the five.

`KONTRA_PULL_POLICY=never` is the rest of it: it stops compose quietly running a published image over
the one you just built. It is the only line the `.env` needs for this.

`scripts/install-cluster.macos.sh` runs this whole path and asserts the result — login, seed,
loopback, the log shipper, and the DuckLake catalog.

</details>

That `grep` prints the password on a **first** boot. On any later boot — a recreate, an image
upgrade — it prints which user exists and says the password cannot be recovered, because only the
hash is kept. `docker compose exec cli kontra user add <name>` is the way back in.

Each child of `workspaces/` is one workspace and `.current` picks it; the console's top-right
selector writes that file. Pick another after login.

Serve and start the seeded hello workflow (discovery does not run it for you):

```bash
docker compose exec -d cli sh -c 'kontra workflow serve "$KONTRA_WORKSPACES/hello/workflows/hello"'
docker compose exec -T cli sh -c 'kontra workflow start "$KONTRA_WORKSPACES/hello/workflows/hello" --wait'
```

That Run places `hello@0.1.0` on a one-machine `dockerFleet`, writes `{"message": "hello world"}`
into Dataset `hello`, and destroys the Fleet on scope exit.

`docker compose down` keeps Datasets and Pulumi state. **`down -v` throws both away** — every
Dataset this control plane recorded, and any local Fleet Pulumi still thought it owned.

**Every published port binds `127.0.0.1` by default**, which is the one security control this
install has. Docker publishes a port by DNAT in `PREROUTING`, so a host firewall does not protect
it — widening `KONTRA_BIND` to `0.0.0.0` exposes Postgres, Temporal, Redis, the registry and the
console to the network, and `ufw` will not stop it. Read the note beside that variable first.

`orchestrator-infra` mounts the host Docker socket so `dockerFleet` can create Warden containers.
That is host-level Docker authority, fine on a single-operator laptop, not tenant isolation.

**[First run](docs/first-run.md)**: cluster → hello Dataset → secrets → a cloud fleet if you need one.

### The editor extension

`tools/vscode` renders an Actor's Method as a form beside the file you are editing and calls it
against the code on disk. It is not on the Marketplace yet, so it is built from the clone:

```bash
cd tools/vscode
npm install && npm run compile
npm run package                                   # -> kontra-0.1.0.vsix
code --install-extension kontra-0.1.0.vsix
```

Or press <kbd>F5</kbd> in that folder to launch an Extension Development Host with it loaded, which
is the faster loop while you are changing the extension itself.

Then, in VS Code:

1. **kontra: Connect to an orchestrator** — `http://127.0.0.1:8088`, the same login the log printed.
   The session token goes to the OS keychain, never to settings (they sync) and never to the
   workspace (it gets committed). The address is `kontra.orchestratorUrl` if you moved the port.
2. Open your `actor.py` and run **kontra: Run this actor**. The pane is an `<iframe>` of the
   console's own `MethodCall` — there is no second form implementation to drift.
3. **kontra: Pin the runner to this file** holds the pane while you navigate away.

The form derives its controls from your type hints: `Literal[...]` becomes a dropdown, `bool` a
toggle with `not set` distinct from `false`, and `kontra.File` / `kontra.Folder` a drop zone that
uploads to `/api/uploads` and passes a content-addressed `{name, sha256, size}` — so a 2 GB input
never travels as a workflow argument. Use the pane's **choose** button rather than dragging into the
editor; a webview's drag surface belongs to the editor, the picker always works.

### The development stack

`docker-compose.yml` is the cluster. After the Install image builds,
`docker compose --env-file .env.quickstart up -d --wait` brings it up. `orchestrator-infra`
stays its own PID because Pulumi's Node language host installs process-global rejection handlers for
every inline `up` ([ADR 0019](docs/adr)) — API and materializer share one process; infra stays
separate.

A DigitalOcean token is optional. Local `docker_fleet()` does not use it. `do_fleet()` does.

Every port publishes to `${KONTRA_BIND}`, defaulting to loopback: a published port is DNATed in
`PREROUTING` and never traverses `ufw-user-input`, so **the publish address is the control and a host
firewall is not**.

**[First run](docs/first-run.md)** walks the cluster, the seeded hello Dataset, then secrets and a
cloud Fleet if you need one.

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
| [Glossary](../../wiki/Glossary) | the vocabulary, and the pairs it is easy to confuse |
| [First Run](../../wiki/First-Run) | nothing → an actor → a workflow → a fleet, one step at a time |
| [Getting Started](../../wiki/Getting-Started) · [Dev Cycle](../../wiki/Dev-Cycle) | install, run one, iterate |
| [Writing Actors: Python](../../wiki/Writing-Actors-Python) · [Go](../../wiki/Writing-Actors-Go) | the authoring surface |
| [Writing Workflows](../../wiki/Writing-Workflows) | the only dispatcher — input shapes, Fleet scopes, Method calls |
| [CLI Reference](../../wiki/CLI-Reference) · [Configuration](../../wiki/Configuration) | every verb; every token, image pin and manifest field |
| [Execution Model](../../wiki/Execution-Model) · [Durability](../../wiki/Durability-and-Failures) | what happens when things break |
| [Data Plane](../../wiki/Data-Plane) · [Query Surface](../../wiki/Query-Surface) | where records go and how to ask |
| [Fleet & the Warden](../../wiki/Fleet-and-the-Warden) | machines, Leases, and running code you did not write |
| [Deployment](../../wiki/Deployment) · [Security Model](../../wiki/Security-Model) | operating it, and what it does and does not isolate |
| [`docs/adr/`](docs/adr) | every architectural decision, with its trade-offs |
| [`docs/event-log-audit.md`](docs/event-log-audit.md) | what a Method call costs in Temporal events, measured — and the two workflows on a clock |

**The ADRs are worth reading before the code.** They are unusually candid — several record a decision *and* the measurement that later corrected it.

## Repo layout

**A directory says where its contents run.** That is the organising rule, and there are only three
answers: the control plane, a Machine, or neither — material both halves have to agree on.

```
sdk/            WHAT AN ACTOR AUTHOR IMPORTS               (Apache-2.0)
  go/  python/    actor + caller (+ fleet, in Python)

runtime/        WHAT RUNS ON A MACHINE, beside the actor   (AGPL-3.0)
  go/  python/    the hosts that load and drive an actor
  handler/        the Go Temporal handler — one per actor, owns the backing workflow

control/        WHAT RUNS WHERE `kontra up` RUNS           (AGPL-3.0)
  orchestrator/   catalog, datasets, fleet, panels — three roles, one build (TypeScript)
  images/         the container definitions for it

cli/            THE ONE BINARY, WHICH IS BOTH
  appliance/      the embedded services `kontra up` supervises   (control plane)
  warden/         the Machine agent and its container drivers    (a Machine)
  internal/       what both halves share, and nothing else

shared/         NEITHER — what more than one implementation must agree on
  core/           @kontra/core, the kernel the console imports too  (Apache-2.0)
  contracts/      the .proto envelope
  conformance/    the corpora — one contract, every implementation drives it

testdata/       the fixture actor kontra's own tests dispatch to
docs/           ADRs and the wiki source
```

**`sdk/` and `runtime/` cannot move**, which is a fact about Go rather than a preference: an actor
imports `github.com/medmahmoudi26/kontra/runtime/go` and `…/sdk/go` BY PATH, and that path is also
the git tag prefix that publishes them. Their directory *is* their API. It is why there is no
`machine/` directory — a Machine's side of the system is split between a published surface that is
frozen and a binary that is also the operator's.

**The console is not here.** It lives in [kontra-console](https://github.com/medmahmoudi26/kontra-console) and depends on `@kontra/core` — this repository's `shared/core/` — so the two halves read a Run through one set of declarations rather than two (ADR 0041). It ships as a content-addressed artifact the release pins by digest, which is why building kontra does not need it.

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
