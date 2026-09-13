# First Run

From an empty machine to a **Fleet**, one step at a time. Each step is three or four lines, in
order, and every command here is real — nothing is elided and nothing is aspirational.

**There are two things and they are not alternatives**, which is the first thing to get right
because the wrong reading costs an afternoon:

- **`kontra up` IS the control plane.** One process — Temporal, the object store, the state store,
  the payload codec, the OCI registry and the orchestrator — with its own data directory and no
  containers (ADR 0031). Steps 1–10 are this and nothing else.
- **`docker compose` is the fleet half**, and only that. It runs `orchestrator-infra`: the Pulumi
  engine, the fleet SSH key and the cloud credential, which ADR 0031 §4 keeps deliberately OFF the
  appliance — no provider plugins and no cloud credential in an artifact whose premise is that a
  stranger curls it onto a laptop. You need it at step 11 and not before.

`docker-compose.yml` says this in capitals at the top of the file: *"`make up` will not give you a
control plane."* Nine services left it one at a time and each left a block behind saying where it
went. See [[Deployment]].

---

## 1. Get the control plane

```bash
git clone https://github.com/medmahmoudi26/kontra && cd kontra
cp .env.example .env          # the installation's shape — no credentials in it
```

`.env` is which address to bind, where the data directory is, which ports. Read its header: it marks
every blank as either **OPEN** (a surface anyone who can reach the API may use) or **DISABLED** (a
surface that answers 503), and the two are opposites. Nothing in `.env.example` is a working
credential, deliberately — a default password in a checked-in file is the first thing an attacker
tries, which is why the one-command installs that ship one are not copied here.

## 2. Set the root password

```bash
go build -o /usr/local/bin/kontra ./cli
kontra init                   # prints the console login ONCE, and nothing can recover it
```

`kontra init` creates `~/.kontra/` at mode 0700, generates the four service tokens and one console
account, hashes the password into `config.yaml` with scrypt and prints the plaintext **once**. It is
generated rather than prompted because `init` runs in installers with no terminal — and because a
default password is worse than no password. A second engineer gets their own with
`kontra user add <name>`; a blank token in an older install is filled by `kontra token mint <key>`.

## 3. Bring it up

```bash
kontra up                     # blocks. Ctrl-C stops the orchestrator child, then the services
kontra doctor                 # in another shell: what is running, and which actors registered
```

One process, six services, no containers. It persists to the data directory, so history, objects,
`global_state` and the lake's catalog all survive a restart. The console is on
`http://127.0.0.1:8088` — sign in with the account from step 2. Add `--temporal-ui` if you want
Temporal's own web console beside it; it is off by default and hydrated from the CAS.

## 4. A namespace, or the default one

A **Tenant** is a Temporal namespace. `default` already exists and every command below uses it, so
for one installation this step is nothing. A second namespace separates two customers' history and
retention — a real boundary, not a label — and it is the one step here that needs a tool kontra does
not ship:

```bash
temporal operator namespace create --address 127.0.0.1:7233 --namespace acme   # Temporal's own CLI
KONTRA_NAMESPACE=acme kontra up                                                # read by the orchestrator
```

Everything below is unchanged: an actor, a workflow and a run do not know which namespace they are
in.

## 5. Write an actor

```bash
mkdir -p ~/.kontra/actors/hello
kontra actor register ~/.kontra/actors/hello --init    # writes a starter actor.json
```

An **Actor** is a folder with an `actor.json` and an `actor.py` (or Go — see
[[Writing-Actors-Go]]). Each **Method** receives a whole **Batch** and pushes records to the
caller's **Dataset**; you write the loop. The form the console and the editor draw comes from the
types you declare, so declaring them well is the whole of the UI work:

```python
from pydantic import BaseModel
from kontra import actor, File

class Target(BaseModel):
    corpus: File                 # a drop zone in the UI — the operator drags a file in
    strict: bool = False         # a toggle
    mode: str = "fast"           # a box

class Host(BaseModel):
    host: str
    strict: bool

@actor.defn
class Hello:
    # A METHOD RECEIVES THE WHOLE BATCH AND THE CALLER'S DATASET, and you write the loop
    # (ADR 0028 §2). `takes=`/`emits=` are what the schemas — and therefore the form — derive from.
    @actor.method(takes=Target, emits=Host)
    async def scan(self, batch, dataset) -> None:
        async for unit in batch:
            body = await unit.value.corpus.read()      # bytes, from the object store
            for line in body.decode().splitlines():
                await dataset.push(Host(host=line, strict=unit.value.strict))


if __name__ == "__main__":
    actor.serve()
```

The name and version are in `actor.json`, not in the decorator — they are what the queue is derived
from, so they belong to the folder rather than to the class.

`File` and `Folder` are `kontra.File` / `kontra.Folder`. They derive a schema carrying
`x-kontra-input`, which is how the console knows to draw a drop zone instead of asking somebody to
type a SHA-256 — see [[Writing-Actors-Python]].

## 6. Serve it, and see it

```bash
kontra serve --actor ~/.kontra/actors/hello --watch
```

`--watch` re-execs the Worker on save, draining in-flight **Units** first — which is what makes a
saved line testable in seconds rather than after a build. Nothing is uploaded and nothing is built:
local mode runs `python <dir>/actor.py` from the directory. Open **Catalog** in the console and it
is there, `serving`, with its Methods listed.

## 7. Call a Method — from the editor or the console

In **VS Code**: install `tools/vscode`, run `kontra: Connect`, then `kontra: Run this actor` with
`actor.py` open. The pane is an `<iframe>` at `<orchestrator>/dev` — the console's own runner with
the chrome removed — so the form you fill in the editor is the same form, from the same schema.
Drag your file onto `corpus`, flip `strict`, press Run.

In the **console**: Catalog → the actor → a Method → the same form. Either way one execution of a
kontra-owned workflow makes exactly **one** Method call over the Batch you typed, through the same
Nexus operation production uses, into an untagged **Dataset**. Two Methods is a topology, and that
is the next step.

## 8. A workflow that calls it

```python
from temporalio import workflow
from kontra import catalog, speak


@workflow.defn
class HelloRun:
    @workflow.run
    async def run(self, req: dict) -> dict:
        hosts = catalog.dataset(req.get("dataset") or "domains")
        out = catalog.dataset("hello-out")
        await speak(f"reading {hosts.name} into {out.name}")

        async with catalog.actor("hello", "0.1.0") as hello:
            # `order_by` is required: a materialized Dataset stamps no row id, so paging without
            # one may overlap or skip rows and nothing would raise.
            async for batch in hosts.batches(100, order_by="host"):
                await hello.scan(batch, out)

        return {"into": out.name}


if __name__ == "__main__":
    catalog.serve([HelloRun])
```

A **Run** is one execution of *your own* Temporal workflow. There is no dispatch verb on the CLI and
no server-side interpreter behind one — you compose in code, with a loop, a branch, a fan-out whose
width depends on what the last Actor returned.

```bash
kontra workflow register ~/.kontra/workflows/hellorun --init
kontra workflow serve hellorun --watch          # or --tmux to detach and get your shell back
```

## 9. Run it, and watch it

Press **Run** on the Workflows surface, or use the editor pane the same way. The input form is built
from the workflow's own declared type — the same controls, so a `bool` is a toggle here too.

Then read it four ways, none of which substitutes for another:

- **Transcript** — what the run set out to do, what it called, what it wrote, in kontra's words
- **Event log** — what Temporal actually recorded, where a drill from any turn lands
- **Datasets** — the output, SQL-queryable **while the run is still open**
- **Monitor** — the worker's own tmux pane, which is where a traceback is

The Workflows surface is the way in to all four: a run is reached through the workflow that produced
it, because a conversation belongs to a thread.

## 10. Secrets

```bash
kontra token mint state        # if you have not already — the secret store is fail-closed
```

Open **Secrets** in the console and add one. It is **write-only**: named, versioned, never readable
back through any route. Workflow code names a secret and the worker resolves it at the last hop; an
actor fetches its own at load, authenticated as itself:

```python
from kontra import actor, secrets

@actor.load
async def load(self) -> None:
    self.client = Shodan(await secrets.get("shodan-key"))
```

A value is never handed through a **Batch**, a Method argument or a workflow argument, because the
payload codec is a **claim-check and not encryption** — anything under 128 KiB rides inline in
workflow history in the clear for the namespace's whole retention, and a credential is a hundred
bytes. Actors declare **slots**; you bind them on the same page, and every read and refusal is in the
ledger at the bottom.

## 11. A Fleet

**This is where compose comes in**, and the only place it does. Provisioning needs
`orchestrator-infra` — the Pulumi engine — which the appliance deliberately does not carry, and
which has to stay its own process because Pulumi's Node language host installs process-global
rejection handlers for the length of every converge (ADR 0019, measured):

```bash
# .env — the control plane's own infra worker reads these:
PULUMI_CONFIG_PASSPHRASE=<a passphrase you choose>     # encrypts the stack state at rest
KONTRA_FLEET_SSH_KEY=~/.ssh/id_ed25519                 # how a Terminal reaches a Machine

kontra infra up                                        # docker compose up -d, config already in env
```

`kontra infra up` is `docker compose up -d` with one addition that matters: `main()` loads
`~/.kontra/config.yaml` into its environment before any command runs, and compose inherits it — so
the tokens from step 2 reach the container without being copied into `.env` by hand.

The **cloud token goes in the secret store**, not in a file and never in the workflow. Secrets → add
`do-prod` → paste it. That NAME is what the workflow uses:

```python
from kontra import fleet
from kontra.fleet import do_fleet

async with fleet.hold(do_fleet(region="nyc3", machines=4, credential="do-prod"), tag="dns") as f:
    await f.place("hello", "0.1.0", sessions=8)       # density; `machines=` above is scale
    await f.ready()                                   # `place` returns while systemd is still
                                                      #   starting; `ready` waits for POLLERS
    async with catalog.actor("hello", "0.1.0") as hello:
        async for batch in hosts.batches(100, order_by="host"):
            await hello.scan(batch, out)
# scope exit drops the Lease. The Machines die when the LAST one does.
```

`credential=` is the **name** of a secret and the SDK refuses a value that looks like a token —
the field crosses into workflow history, and no rotation takes that back. A DigitalOcean VPC is
regional, so `vpc=` without `region=` is refused outright rather than provisioning into a region
that cannot host it and hanging on `ready()`.

That teardown is a replayable step in a program Temporal finishes whether or not the process that
started it still exists. A script that provisions ten machines and dies leaves ten machines. This
cannot.

---

## What to read next

| | |
|---|---|
| [[Dev-Cycle]] | the edit → serve → run loop, and `--watch` |
| [[Writing-Actors-Python]] · [[Writing-Actors-Go]] | the authoring surface in full |
| [[Execution-Model]] · [[Durability-and-Failures]] | what happens when things break |
| [[Fleet-and-the-Warden]] | Machines, Leases, and running code you did not write |
| [[Security-Model]] | what each blank token leaves open, and what kontra does not isolate |
| `docs/debugging.md` | stopping on a breakpoint inside a Method |

---

## Every command here was run

Not a convention — the commands in this page are checked against the CLI's own dispatch table, and
the API in the two code blocks against the SDK's signatures. Two things were wrong on the first
draft and are worth naming, because they are the two a walkthrough gets wrong by default:

- **A Method receives the whole Batch, not one Unit.** `async def scan(self, batch, dataset)`, and
  you write the `async for`. ADR 0028 §2 — the loop is yours, which is what lets a Method open one
  connection for forty Units instead of forty.
- **`docker compose` is not the control plane.** `kontra up` is. The compose file carries the fleet
  half and says so at the top of itself.

If a command here does not work, that is a bug in this page and not in your install. The CLI's own
`kontra help` is generated from the same table this was checked against.
