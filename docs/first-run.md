# First run

Control plane → seeded hello Dataset → (optional) your own actor → secrets → a cloud fleet.

**Every command here is the documented install path.** Discovery registers the starter; it does not
serve or start it.

---

## 1 · A control plane

```bash
git clone https://github.com/medmahmoudi26/kontra-console.git
git clone https://github.com/medmahmoudi26/kontra.git
mkdir -p workspaces
cd kontra
make image
docker build -f control/images/Dockerfile.orchestrator -t kontra-orchestrator:latest .
docker build -f control/images/Dockerfile.pyworker -t kontra-host:1 .
docker build -f control/images/Dockerfile.workerbase -t kontra-worker-base:1 .
docker compose --env-file .env.quickstart up -d --wait
docker compose exec cli cat /var/lib/kontra/console-password
```

Sign in at <http://127.0.0.1:8088> as `admin` with the password that last line prints. It is in that
0600 file and nowhere else — never on stdout, because `init` runs in a container and stdout there is
`docker compose logs`. `config.yaml` keeps only an scrypt hash. Deleted the file? Make another
account: `docker compose exec cli kontra user add <name>`.

`docker compose down` keeps Datasets and Pulumi dockerFleet state. **`down -v` destroys both.**

Named workspaces live in `workspaces/` beside `kontra/` and `kontra-console/` (not inside
either repo). Seed creates `hello/` when that parent is empty. The console rail switches the current
workspace; Datasets and runs stay cluster-wide.

### Serve and start the starter

The empty parent was seeded with `./workspaces/hello` (`actors/hello`, `workflows/hello`).
Watch has registered them and published the actor image. You still have to serve and start:

```bash
docker compose exec -d cli sh -c 'kontra workflow serve "$KONTRA_WORKSPACES/hello/workflows/hello"'
docker compose exec -T cli sh -c 'kontra workflow start "$KONTRA_WORKSPACES/hello/workflows/hello" --wait'
```

The Run writes `{"message": "hello world"}` into Dataset `hello` and tears the local Fleet down when
the scope exits. Cancel is the same teardown. Do not terminate the walkthrough.

```bash
docker compose exec -T cli sh -c 'kontra dataset query hello --sql "select message from hello"'
```

### Tokens the CLI inside the cluster already has

`cli` wrote `config.yaml` into the `kontra-home` volume on first boot. The `cli` service mounts that
volume, so `docker compose exec cli kontra …` needs no extra exports. A CLI on the **host** still
does:

```bash
export KONTRA_ORCHESTRATOR_URL=http://127.0.0.1:8088
export KONTRA_ADDRESS=127.0.0.1:7233
docker compose exec -T cli grep -A14 '^tokens:' /var/lib/kontra/config.yaml
```

---

## 2 · A tenant

A **Tenant** *is* a Temporal namespace: separate history, separate retention. `default` exists on
first boot and is the right answer until you have a second customer's runs to keep apart from the
first's.

```bash
export KONTRA_NAMESPACE=default          # or set KONTRA_NAMESPACE in .env and restart
```

---

## 3 · An actor

Copy `examples/python/firstactor`. Its one Method takes a model whose every field draws a
**different control**, which is the fastest way to see what a declared type buys you:

```python
class Target(BaseModel):
    host: str                            # a box
    mode: Literal["quick", "deep"] = "quick"   # a DROPDOWN
    follow_redirects: bool = False             # a TOGGLE (`not set` ≠ `false`)
    wordlist: Optional[File] = None            # a DROP ZONE
    corpus: Optional[Folder] = None            # a DROP ZONE for a directory
```

Register it and serve it:

```bash
kontra actor register examples/python/firstactor
kontra serve --actor examples/python/firstactor --watch
```

`--watch` re-execs the Worker on save. Without it the pane is a viewer attached to whatever was
served last, and an edit changes nothing until you re-serve.

> **The actor runs in YOUR process, not in the container.** The control plane has no Python for you
> — the same shape a Machine uses in production. What the container *does* need is to **read the
> folder**, because registering records a path and the orchestrator opens it to derive the form.
> The quickstart compose mounts your working directory at the same path on both sides for exactly
> this reason; a mount at a different path answers `no such directory`.

---

## 4 · Call a Method — from the console or from VS Code

**Console** → *Catalog* → `firstactor` → `expand`. The form is derived from the schema above:
`mode` is a dropdown with two options, `follow_redirects` is a switch, and `wordlist` / `corpus`
are drop zones. Drop a file and the bytes go to `/api/uploads` and the field carries
`{name, sha256, size}` — a 2 GB input never travels as a workflow argument.

**VS Code** — build the extension once:

```bash
cd tools/vscode && npm install && npm run compile && npm run package
code --install-extension kontra-0.1.0.vsix
```

Then **kontra: Connect to an orchestrator** (`http://127.0.0.1:8088`, the same login; the session
token goes to the OS keychain), open `actor.py`, and **kontra: Run this actor**. The pane is an
`<iframe>` of the console's own form, so there is no second implementation to drift. **kontra: Pin**
holds it while you navigate.

> **A Method call needs the probe worker.** It is a separate process because a Nexus dispatch can
> only be made from a workflow and the control plane is Node-only. Without it the call answers
> *"the probe worker is not running — nothing polls kontra-probe"*:
>
> ```bash
> python3 -m kontra.probe &
> ```

Either way, the same run:

```
units: 1 · dataset: firstrun_probe · queue: kontra-probe
```

```bash
kontra dataset query firstrun_probe --sql "SELECT source, mode, count(*) n FROM firstrun_probe GROUP BY 1,2"
# builtin  deep  13        (5 builtin + 8 deep — the dropdown changed the result)
```

---

## 5 · A workflow that calls the actor

`examples/workflows/firstrun` dispatches to `firstactor`. The Actor is the work; the workflow is the
**decision** — how many Units, on how many Machines, and what to do when one fails.

```bash
kontra workflow register examples/workflows/firstrun
kontra workflow serve examples/workflows/firstrun --watch
kontra workflow start examples/workflows/firstrun --wait \
  --input '{"hosts":["example.com","example.org"],"mode":"quick"}'
# {"candidates": 10, "dataset": "firstrun_demo", "hosts": 2}
```

Or press **Run** in the console's Workflows surface, or from the VS Code pane — same run, same
derived form.

> **A workflow file needs an entry point or it serves nothing:**
>
> ```python
> if __name__ == "__main__":
>     catalog.serve([FirstRun])
> ```
>
> Without it the module defines its workflow and exits. `kontra workflow serve` now refuses that
> with the fix in the message; it used to exit 0 silently and leave `start` failing against a queue
> nobody polls.

---

## 6 · Watch it

```bash
kontra runs list                       # every run: type, status, dispatches, output prefix
kontra runs --run-id <id>              # per-node state and live heartbeat progress
kontra runs --run-id <id> --query "SELECT * FROM run LIMIT 20"
```

In the console, **Workflows** lists the runs and opens one; the run view carries the **history** —
the ADR 0025 reduced log, which is a story for a human rather than a raw event dump — alongside
`historyLength` and `historySizeBytes`. There are three readings of a Run and they are deliberately
not substitutable: **Runs** (did it finish), **Monitor** (what is it doing now), **Datasets** (what
did it produce).

---

## 7 · Secrets

Console → **Secrets**, or:

```bash
curl -fsS -X PUT http://127.0.0.1:8088/api/secrets/do-prod \
  -H "authorization: Bearer $KONTRA_STATE_TOKEN" \
  -H 'content-type: application/json' -d '{"value":"dop_v1_…"}'
```

**The value never comes back.** A `GET` answers the name, the versions and which one is current, and
nothing else. It is stored at `/var/lib/kontra/secrets/secrets.json` inside the volume, `0600`, and
read only by the converge that spends it.

---

## 8 · A fleet

Provisioning is `do_fleet` in the workflow, and the credential is **the name of a secret, never the
token**:

```python
from kontra.fleet import do_fleet

spec = do_fleet(machines=4, region="nyc3", credential="do-prod")
async with fleet.up(spec, actor="firstactor", version="0.1.0", sessions=1) as f:
    await f.ready()                       # wait for POLLERS, not for droplets
    ...                                   # dispatch here
# the Machines are destroyed on the way out of this block
```

Four things that are easy to get wrong, all of them measured:

- **`actor=` and `version=` are required, and `up` places them itself.** It is sugar over
  `hold` + `place` and costs one converge rather than two. The older `fleet.up(tag=…, machines=…)`
  followed by `f.place(…)` now raises `TypeError` before provisioning anything.
- **The credential is a name.** A token written into the workflow would be replayed into history on
  every worker that picks the run up and kept for the namespace's whole retention. Put it in the
  secret store first; an unknown name fails *before* provisioning, which is the order that does not
  bill you.
- **`f.ready()` waits for pollers.** A Machine that exists and is not yet polling its queue takes a
  dispatch nothing answers — a run that looks hung.
- **`cancel`, not `terminate`.** `kontra workflow cancel <run-id>` runs scope exits, so the fleet is
  destroyed. `terminate` skips them and leaves the Machines billing.

The control plane itself provisions nothing: the Pulumi engine, the fleet SSH key and the cloud
credential live in the `orchestrator-infra` service of the *development* compose file, off the
install on purpose (ADR 0031 §4, ADR 0034 §1). You need it when a workflow provisions a Fleet, and
not before.

---

## What broke while this was written

Kept because each one is a thing a second reader will hit, and every one passed CI:

| Symptom | Cause |
|---|---|
| Console printed a password that would not sign in | compose sets `KONTRA_CONSOLE_USERS=""`, and *set-but-empty* blocked `config.yaml` |
| `up -d --force-recreate` bricked the install permanently | a container's hostname is its id, so the catalog lock looked like a live holder on another host |
| `kontra actor register` → `401 unauthorized` | it sent no token to a run-gated route; worked only while every install had a blank run token |
| `kontra actor register` → `no such directory` | the quickstart mounted nothing, so the orchestrator could not read the folder it was given |
| `kontra workflow serve` printed nothing and exited 0 | the file had no `__main__` block; a worker that never started was reported as success |
| The fleet path of two shipped workflows | `fleet.up` without `actor=`/`version=` — a `TypeError` only the fleet branch reached |
| `pull access denied … may require 'docker login'` | a local tag was never built (`make image` / orchestrator / host) and Compose tried a registry pull |
