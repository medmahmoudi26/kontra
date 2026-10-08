# Configuration

Three things get configured: **the installation** (tokens, addresses, accounts), **an Actor**
(`actor.json`), and **a Workflow** (`workflow.json`). This page is the reference for all three.

---

## Where configuration lives

| | |
|---|---|
| `~/.kontra/config.yaml` | the installation: accounts and the four tokens. Written by `kontra init`. |
| `$KONTRA_HOME/runtime.env` | what the control plane sources at boot. **Generated** — not hand-edited. |
| `.env` | the Compose topology's knobs (image pins, ports, bind address) |
| `actor.json` | one Actor's identity and resource targets |
| `workflow.json` | one Workflow's manifest |

> **The login and the four tokens are not set in `.env`.** `kontra init` generates them on first
> boot into `$KONTRA_HOME/runtime.env`, which `control/images/orchestrator-entrypoint.sh` sources
> before starting the process. That survives a recreate because the home is a named volume.
>
> This also means **`docker exec <c> env` will not show them** — it shows the image's config
> environment, not what the entrypoint exported. Read `/proc/1/environ` instead.

---

## The four tokens

Each one gates a different surface, and they are deliberately not interchangeable.

| Variable | Gates | Blank means |
|---|---|---|
| `KONTRA_STATE_TOKEN` | `/api/fleet`, `/api/infra`, `/api/uploads` | that surface is **disabled** (`503`) |
| `KONTRA_EXPLORE_TOKEN` | `/api/datasets/query`, `/api/explore`, `/api/logs` | falls back to `KONTRA_STATE_TOKEN`; disabled (`503`) if that is blank too |
| `KONTRA_RUN_TOKEN` | **writes** on `/api/runs` and `/api/workflows`, dataset tag/rename | the Run surface is **OPEN** to anyone who can reach the API |
| `KONTRA_SECRETS_TOKEN` | `/api/audit` | falls back to `KONTRA_STATE_TOKEN` |

```sh
kontra token mint <state|explore|panel|run>   # fill a BLANK token in an existing config
```

### The variable lists are a fallback chain, not an accept-list

This is the subtlety that costs an afternoon. A route names an *ordered list* of variables, and
`configuredToken()` returns **the first one that is set** — that single value is then the only token
that route accepts:

```ts
EXPLORE_TOKEN_VARS = ['KONTRA_EXPLORE_TOKEN', 'KONTRA_STATE_TOKEN']
```

Read that as *"use the explore token; if nobody set one, use the state token"* — **not** as "either
token works". On an install where `KONTRA_EXPLORE_TOKEN` is set, the state token is **rejected** on
`/api/datasets/query`. Measured on a live install:

| route | state | explore | run | none |
|---|---|---|---|---|
| `POST /api/datasets/query` | `401` | `200` | `401` | `401` |
| `GET /api/infra/stacks` | `200` | `401` | `401` | `401` |
| `GET /api/runs/<id>` | `404` | `404` | `404` | `404` |

### Reads on `/api/runs` are open by design

That bottom row is not a bug. `routes/runs.ts` gates every **write** with `checkOptionalBearer` +
`RUN_TOKEN_VARS` and deliberately leaves the reads ungated — a read "can never perturb the run", and
gating it would cost an operator who set the token every status check. `GET /api/runs/<id>`
answering `404` without a credential means *that run does not exist*, not that you were let in by
accident.

> The generated OpenAPI spec lists the whole `/api/runs` prefix under `runToken`, which over-claims
> for the read routes. `everyOpenPathIsActuallyOpen` in the suite catches the opposite direction —
> a path documented as open that actually `401`s — and nothing yet checks this one.

### `KONTRA_PANEL_TOKEN`

Minted by `kontra init` and `kontra token mint panel`, and present in `runtime.env` — but **no route
in the orchestrator reads it**. Treat it as reserved.

### A console session beats all four

```
checkBearer():  sessions.verify(bearer)  →  admitted
                else compare against the route's configured token
```

A browser has nothing else to send, so a **console session** — minted by signing in against the
account in `config.yaml` — admits everything the console does. A service token admits exactly one
surface.

> This is why driving the console with a single service token does not work end to end: the run
> page needs the **state** token for its fleet panel and the **explore** token for logs and
> datasets, and the console stores one bearer. A `401` makes it clear that bearer and drop to the
> sign-in screen.

### Accounts

```sh
kontra init              # generates the first account and prints the password ONCE
kontra user add <name>   # a second login; only the scrypt hash is stored
```

Accounts reach the control plane as `KONTRA_CONSOLE_USERS` — **base64 of JSON**, and the encoding is
load-bearing: a stored hash is `scrypt$N$r$p$salt$hash`, it contains `$`, and Compose substitutes
`$` in values it passes through. The raw form would arrive corrupted in exactly the deployment this
exists for, and corrupted into something that still *looks* like a hash — so every login would fail
as "wrong password" rather than as the configuration error it is.

> The password is in `/var/lib/kontra/console-password` (mode 0600), not in the logs:
> `docker compose exec cli cat /var/lib/kontra/console-password`. Stdout was the old channel and was
> wrong twice over — `docker compose logs cli` shows only the container that ran `init`, so one
> recreate destroyed the only copy, and until then the credential sat in a log stream readable by
> anyone in the docker group.

---

## Addresses

| Variable | Default | Notes |
|---|---|---|
| `KONTRA_ORCHESTRATOR_URL` | `http://localhost:8088` | the control plane API |
| `KONTRA_ADDRESS` | `localhost:7233` | Temporal frontend |
| `KONTRA_NAMESPACE` | `default` | |
| `KONTRA_HOME` | `~/.kontra` | config, workflows, actors |
| `KONTRA_WORKSPACES` | — | parent directory of named workspaces |
| `KONTRA_CONTROLLER` | — | what a Warden enrols against. **A Compose service name is not reachable from a Droplet.** |
| `KONTRA_REGISTRY` | — | bare `host:port` — it is what an image reference is tagged with, and a reference cannot carry a scheme |
| `KONTRA_REGISTRY_URL` | `http://registry:5000` | the same registry for an **HTTP client**, which does need a scheme |
| `KONTRA_PORTER_URL` | — | Arrow Flight SQL endpoint |
| `KONTRA_LOGS_URL` | — | VictoriaLogs |

> `KONTRA_REGISTRY` and `KONTRA_REGISTRY_URL` are separate on purpose. Keeping them apart is what
> stops a `10.124.0.2:5000` meant for `docker pull` being handed to `fetch` as a relative URL.
>
> **`KONTRA_REGISTRY_URL` is set by no compose or env file**, so the default in the code is the live
> value: the orchestrator reaches the registry at `http://registry:5000`, by **compose service name**.
> That is why the registry service kept the name `registry` when zot replaced `registry:2` — renaming
> it empties `/api/images` with no error anywhere.

### Binding

`KONTRA_VPC_BIND` controls which address published ports bind to. **Loopback is the security
control** for services with no auth of their own — Porter in particular has no auth and no TLS, so
its Compose service must never get a `ports:` entry.

---

## Images

| Variable | What runs it |
|---|---|
| `KONTRA_IMAGE` | the CLI / workflow-worker containers |
| `KONTRA_ORCHESTRATOR_IMAGE` | **the API and infra roles** |
| `KONTRA_PORTER_IMAGE` | Arrow Flight SQL |
| `KONTRA_LOGSHIP_IMAGE` | the log shipper |

The registry's own image is named **literally** in `docker-compose.yml`, not through a
`${KONTRA_*_IMAGE:-…}` default, and that is deliberate: the release workflow derives the set of
images *kontra owns* by grepping those defaults out of the compose file, so a variable there would
make a third-party image look like one this repository builds.

> **`make image` does not update the API.** It builds `KONTRA_IMAGE`; the API runs
> `KONTRA_ORCHESTRATOR_IMAGE`, which is a *different* image built `FROM` the first one for its SPA.
> Building only the first serves old code under a green `up`. Build order is forced:
> `make image` → then the orchestrator image.
>
> **An actor's SDK comes from its runtime, not from an image this repo builds.** `kontra deploy`
> layers the actor onto a published runtime (`actor.json`'s `runtime` field, default `python:1`),
> pinned by digest at build time and recorded in the catalog. `kontra rebase` is how an actor moves
> onto a newer digest of the same major without rebuilding.

---

## The image store

The compose controller's `registry` service is **zot**, pinned to the exact version `v2.1.21`. `kontra up`'s
install has a **different**, in-process registry: unauthenticated, loopback-only, with no retention
and no garbage collection. Everything in this section is the compose one.

| Variable | Default | Notes |
|---|---|---|
| `KONTRA_REGISTRY_PUSH_ACTORS_PASSWORD` | — | the credential a build pushes `actors/**` with |
| `KONTRA_REGISTRY_PUSH_RUNTIMES_PASSWORD` | — | CI and `kontra runtime build`, scoped to `kontra-runtimes/**` |
| `KONTRA_REGISTRY_PUSH_ACTORS_USER` | `push-actors` | the account `kontra deploy` pushes an actor image as |
| `KONTRA_REGISTRY_PULL_PASSWORD` | — | Machines and the Warden: read everything |
| `KONTRA_REGISTRY_RETENTION` | `dryrun` | `enforce` deletes; `dryrun` logs what it *would* delete |
| `KONTRA_INUSE_TAGS` | — (unset ⇒ **on**) | `off` disarms the `inuse-` tag reconciler, the thing that exempts a digest from the row above. Leave retention on `dryrun` while it is off |
| `KONTRA_INUSE_TAGS_MS` | `600000` (10 min) | the reconciler's interval. A non-numeric or non-positive value falls back to the default rather than failing |
| `KONTRA_REGISTRY_MIGRATION` | — | `skip` starts a deliberately empty zot on a box that still holds a `registry:2` store |
| `KONTRA_REGISTRY_CVE` | — | `1` turns on zot's Trivy integration, which downloads a vulnerability database on first boot |
| `KONTRA_RUNTIMES_PREFIX` | `ghcr.io/medmahmoudi26/kontra-runtimes` | where a bare `name:major` in `actor.json` resolves — the published set, read anonymously ([[Runtimes]]) |
| `KONTRA_PACK_BIN` | `/usr/local/bin/pack`, then `/opt/kontra/pack`, then `PATH` | the pinned `pack` `0.40.9`, checksummed into the `kontra` image and copied into the orchestrator image. Set this only to point at your own |
| `KONTRA_DEPLOY_SH` | — (unset ⇒ warn) | `refuse` makes an actor's `deploy.sh` an error instead of a warning. A buildpack build does not run it, so an actor that depended on it builds fine and is missing whatever it installed ([[Writing-Actors-Python]]) |

> **Blank is a choice here, not an oversight.** With all three passwords empty the registry accepts
> **anonymous** pulls and pushes — which is what `registry:2` always did, and the loopback publish is
> the control for it. Fill **all three** to turn authentication on: a partial set is *refused*,
> because a registry with auth enabled and no users answers 401 to everything and reads as a broken
> install rather than a locked one.
>
> Under `docker-compose.vpc.yml` the three stop being optional and become `:?` required, beside the
> publish that creates the exposure — the same treatment `KONTRA_REDIS_PASSWORD` gets.

> **Anonymous still cannot delete.** zot answers an unauthenticated manifest `DELETE` with **202**
> where `registry:2` answered 405, so even the no-credential rendering carries an `accessControl` of
> `read`/`create`/`update`. Every existing push path keeps working; erasing a Bundle a Machine is
> about to pull does not. [[Security-Model]] and `docs/THREAT_MODEL.md` §4 carry the exposure.

> **`KONTRA_REGISTRY_RETENTION` defaults to reporting** because "keep the 5 most recently pushed" is
> only safe once something protects a version older than those five that is still placed on a Machine.
> That is what `inuse-` tags are for, and the reconciler that writes them is armed in the API role —
> but it tags only the digest the **catalog** records for each actor, not a Machine's placements, so the
> protection is narrower than the policy assumes. `dryrun` stays the default until that gap closes.
> [[Durability-and-Failures]] has the detail.

### What the catalog records about a build

| Variable | Set by | Read by |
|---|---|---|
| `KONTRA_ACTOR_DIGEST` | the Warden, when it pulls by digest | both registrars |
| `KONTRA_RUNTIME_NAME` · `_MAJOR` · `_DIGEST` | **nothing yet** | both registrars, which *echo* them into the catalog |
| `KONTRA_BUILDER_DIGEST` | **nothing yet** | the same |

These are not facts a worker can discover: nothing inside a running container can see the run image it
was layered onto or the builder that layered it. So the **deploying CLI** is what has to record them and
the worker only ever echoes them back — which is what stops a restart from erasing what it cannot
independently know. The echo is live in both registrars; the CLI side is not, so a catalog entry today
carries neither. Both are **omitted when unset**, never sent empty, because the catalog keeps a
previous value only when the key is absent.

---

## Data plane

| Variable | Notes |
|---|---|
| `KONTRA_S3_ENDPOINT` | the object store (SeaweedFS locally) |
| `KONTRA_S3_ACCESS_KEY` / `KONTRA_S3_SECRET_KEY` | |
| `KONTRA_DUCKLAKE_CATALOG` | the Postgres catalog |
| `KONTRA_LAKE_WORKSPACE` | which workspace's lake to address; falls back to the current workspace |
| `KONTRA_DUCKDB_MEMORY_LIMIT` | |
| `KONTRA_DUCKDB_TEMP_DIR` | spill directory — set it, or a large query dies on a small `/tmp` |
| `KONTRA_UNITS_RETENTION_DAYS` | |
| `KONTRA_RETENTION_COLLECT` | **`1` arms the retention sweep.** Unset, it previews and deletes nothing. |

---

## Logging

`KONTRA_LOG_FORMAT=json` is what puts a worker's lines on the run page's rail. The Python runtime's
structured logging is off unless asked, because a developer watching a tmux pane wants the human
form. In a **container** nobody reads stdout directly — `logship` does, and its parser has a
dedicated branch for kontra's own JSON. Without it the worker ships readable text whose workflow id
is buried in a Python dict repr, so `run_id:"<id>"` — the query the console's rail runs — matches
nothing and the rail is empty for the whole run.

---

## `actor.json`

```json
{
  "schemaVersion": "kontra.actor.v1",
  "name": "desync",
  "version": "1.3.3",
  "runtime": "python:1",
  "targets": {
    "container": { "memory": "1g", "cpus": 2 },
    "machine":   { "size": "s-2vcpu-4gb", "region": "sfo3", "image": "ubuntu-22-04-x64" }
  }
}
```

| field | meaning |
|---|---|
| `schemaVersion` | pinned contract — `kontra.actor.v1` |
| `name` · `version` | identity. Together they derive the **task queue**, so changing either moves the queue |
| `runtime` | the OS and system packages this Actor runs on, as a name and a **major** (`python-browser:1`) or a fully qualified reference. Absent means `python:1` for a Python actor and `base:1` for a Go one ([[Runtimes]]) |
| `targets.container` | resources for one Container — `memory`, `cpus` |
| `targets.machine` | what a Machine must be for this Actor — `size`, `region`, `image` |

There is **no schema section**: a Method's JSON Schema is derived from the code. Check it with
`kontra actor schema <dir>`.

---

## `workflow.json`

```json
{
  "entry": "workflow.py",
  "name": "canary",
  "version": "1.0.0",
  "workflow": "Canary"
}
```

| field | meaning |
|---|---|
| `entry` | the file to import |
| `name` · `version` | identity in the catalog and console |
| `workflow` | the `@workflow.defn` **class name** inside `entry` |

No queue field — the task queue is derived from the folder's content. See [[Writing-Workflows]].

---

**See also:** [[CLI-Reference]] · [[Security-Model]] · [[Deployment]] · [[Glossary]] · [[Runtimes]]
