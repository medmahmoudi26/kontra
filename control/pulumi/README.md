# `kontra-control` — the install, as a Pulumi program

ADR 0052 §1-§3. This directory is the port of `docker-compose.yml`: 15 containers, 13 volumes, one
private network, the same images, the same 10 healthchecks and the same 17 ordering gates.
`kontra up` shells to the host `pulumi` and converges it.

```
Pulumi.yaml   the program — the whole topology, one file
parity.py     asserts this program and docker-compose.yml still describe the same topology
```

`Pulumi.yaml` carries its reasoning inline, beside the service it is about, because that is where
whoever edits a service will read it. This file is the parts that are about the program as a whole:
why it has the shape it has, and how it is driven.

## Why this is YAML, and why ADR 0019's engine stays TypeScript

There are now two Pulumi engines in kontra and they do not share a program shape. That is a decision,
not an accident of who wrote which one.

| | Host engine — **this directory** | Infra engine — `control/orchestrator/src/infra/` |
|---|---|---|
| Program | Pulumi **YAML** on disk, one file | inline TypeScript, Automation API (`workspace.ts:36`) |
| Projects | `kontra-control`, and nothing else | `kontra-fleet`, `kontra-docker-fleet`, … |
| Runs in | `kontra`'s own process tree, host `pulumi` | `orchestrator-infra`, its own PID (ADR 0019) |
| Backend | `file://~/.kontra/state` | `file:///data/pulumi-state` |
| Triggered by | a person typed a command | a Run's `fleet.up()` / `f.place()` |

**The infra engine is inline TypeScript because a Fleet's resource set is computed per converge, from
a Run's arguments.** That is literal, not a figure of speech —
`infra/programs/dockerFleet.ts:46` is

```ts
const machines = Array.from({ length: args.machines }, (_, i) => { … new docker.Container(…) });
```

so `machines` decides *how many resources exist*, and `machines: 0` coerces to a destroy.
`assignmentFor` (`dockerFleet.ts:111`) then folds the Run's placements into a per-Machine assignment
and refuses a `workerImage` that is not `repo@sha256:<64 hex>` **before any resource is constructed**.
None of that is expressible as a document, in any templating language, without inventing one.

**The control stack has no arguments.** It is 15 containers, and it is 15 containers on every box and
every converge. A static topology written as a static document is the honest representation of it, and
what that buys is the reason the ADR calls it fact 2: **a YAML program resolves providers as binary
plugins, so the host needs `pulumi` and `docker` and nothing else.** No Node, no `@pulumi/*` package,
no Automation API, no `node_modules` — which is what lets a Go CLI converge this at all. ADR 0052 §3's
installer "resolves Docker, resolves or installs `pulumi`, and refuses rather than falling back";
introducing a Node dependency here makes that sentence false and the failure is a silent third host
prerequisite discovered by an operator, not by us.

There is a second, sharper reason to keep the shapes apart. ADR 0052 §1: **the host dispatch table
contains `kontra-control` and refuses every other project, and `planFor` refuses `kontra-control`.**
`infra/stacks.ts`'s header says why the table exists at all — *"a caller that could supply the program
could provision anything the credential allows"* — and a second engine reopens that from the other
side: if the host table could name `kontra-fleet/dns`, `kontra up` would reach cloud Machines with no
Lease, no `checkCloudCredential`, no Temporal record, no mutex and no audit. A program that is a file
on disk in a fixed location, with `name: kontra-control` as its first field, cannot be pointed at
another project by a caller. A program that is a function taking arguments can.

### What YAML costs, and it is paid in this file

Named here so nobody spends an afternoon looking for a feature that is not there. The builtins
`pulumi-language-yaml` actually carries are `fn::invoke`, `fn::join`, `fn::split`, `fn::select`,
`fn::length`, `fn::singleOrNone`, `fn::readFile`, `fn::fileAsset`, `fn::assetArchive`,
`fn::toBase64`/`fn::fromBase64`, `fn::secret` and `fn::pulumiResourceName`/`Type` — read out of the
binary, so treat it as the set that exists rather than as an exhaustive citation. **There is no
conditional and no loop among them,** and that absence is what the four items below are.

- **No merge key for the 24-variable env anchor.** YAML's `<<` merges mappings; this provider's `envs`
  is a sequence of `KEY=value` strings. The single source is one block scalar (`orchestratorEnv`) plus
  `fn::split` per role. `parity.py` compares every variable in every service so the two spellings
  cannot drift.
- **No loop, so the six published ports are written out six times.** Duplication a program would have
  factored out. `parity.py` counts them against compose's six and asserts every `ip` is `127.0.0.1`.
- **No conditional, so ADR 0052 §4's coupling cannot live here.** "Refuse to serve on a non-loopback
  bind while the admin password is `changeme`" is a rule about two values; `bind` is a config key in
  this file and the refusal belongs to the control plane, with a test, in Go.
- **`fn::readFile` rather than an inlined script.** `logship.sh` interpolates four `${…}` expressions
  of its own, three of them shell defaults — `logship.sh:71` is
  `LOGS_URL="${KONTRA_LOGS_URL:-http://victorialogs:9428}"`. Pasted into this file those become Pulumi
  interpolations against config keys that do not exist and the program fails to load. `fn::readFile`'s
  result is runtime data and is never interpolated. (`postgres-init.sh` and `logline.py` contain no
  `${` at all today, which is why the rule has to be about the mechanism and not about the current
  contents of three files.)
- **A secret config key may not carry a plaintext default.** Re-measured on 3.244.0 with a two-key
  throwaway program: `error: validating stack config: Stack 'v' with configuration key 'passphrase'
  must be encrypted as it's secret`. So `infraPassphrase` is declared unmarked with compose's own
  `local-dev` default rather than `secret: true` with none, which would make the program
  unconvergeable until somebody set it — a worse trade than matching what compose already ships in a
  tracked file. `config set --secret` still encrypts it when an operator overrides it.

## Prerequisites

Two, and the second is new (ADR 0052 Consequences: *"Docker is no longer the only host
prerequisite"*).

```sh
docker version
pulumi version                                   # >= 3.244.0 measured here
pulumi plugin install resource docker 4.11.2     # HARD PREREQUISITE — see below
```

**The docker provider plugin is not part of a stock Pulumi install.** Before this work, `pulumi plugin
ls` on this box listed `digitalocean` and `random` and nothing else, and `find /root/.pulumi -name
'*docker*'` returned nothing. A YAML program that declares `docker:index:Container` with no plugin
fails at resource registration, which reads as a program error rather than as a missing binary.
The one-line installer installs it; `kontra doctor` reports it. ADR 0052 §1 also lists `tls` in the host engine's
provider set; nothing here declares it yet and it is still absent.

4.11.2 and not `latest`, because that is the version ADR 0052 fact 1 was established against and the
version `control/orchestrator/package.json` pins for the *other* engine. The program pins it on an
explicit `pulumi:providers:docker` resource (38 references) so both engines agree about what a
`Container` is.

## The `waitTimeout` trap, and where every number came from

`depends_on: condition: service_healthy` translates to `healthcheck:` + `wait: true` on the
dependency and `dependsOn` on the dependent. The provider's own schema, read out of the installed
4.11.2 plugin:

> **`wait`** — If `true`, then the Docker container is waited for being healthy state after creation.
> This requires your container to have a healthcheck, otherwise this provider will error.
>
> **`waitTimeout`** — The timeout in seconds to wait the container to be healthy after creation.
> Defaults to **`60`**.

**Compose's gates have no timeout at all.** So the 60 does not replace an existing limit — it
*introduces* one, at a value no compose user ever had to satisfy, on every gate at once. A first boot
that `docker compose up -d --wait` sat through happily becomes a `pulumi up` that fails partway, with
`timeout while waiting for container` and no probe named. That reads as a Pulumi problem and gets
debugged as one; the actual event is `temporalio/auto-setup` still creating four databases.

Every value below is **`start_period + retries × (interval + timeout)` from the compose healthcheck,
rounded up** — that is the moment **Docker itself** gives up and marks the container unhealthy.

| container | healthcheck — `start_period +` `interval`/`timeout` `×` `retries` | Docker's budget | `waitTimeout` |
|---|---|---|---|
| `postgres` | 5s/5s × 20 | 200 | **210** |
| `redis` | 5s/3s × 10 | 80 | **90** |
| `registry` | 5s/5s × 10 | 100 | **110** |
| `seaweed` | 20s + 5s/5s × 30 | 320 | **330** |
| `victoriametrics` | 10s + 10s/5s × 10 | 160 | **170** |
| `victorialogs` | 10s + 10s/5s × 10 | 160 | **170** |
| `temporal` | 40s + 10s/10s × 30 | 640 | **660** |
| `orchestrator-api` | 20s + 10s/5s × 20 | 320 | **330** |
| `orchestrator-infra` | 30s/5s × 5 | 175 | **180** |
| `orchestrator-probe` | 30s/5s × 5 | 175 | **180** |

Three containers carry no `waitTimeout` and each is a different reason: `temporal-dynamicconfig` is
not waited on at all (`attach: true` blocks on its *exit*, not its health), and `logship` and `cli`
have no healthcheck, so `wait: false` — `wait: true` there would error out of the provider by its own
documentation.

**Why the ceiling and not something shorter an operator would rather wait.** These are not estimates
of how long a boot takes; they are derived from the probes, and choosing them makes the timeout a
non-event. Docker always reaches its verdict first, so the message an operator reads is Docker's
*"container is unhealthy"* naming the probe that failed, and never Pulumi's *"timeout while
waiting"*, which names nothing and points at the wrong tool. Anything shorter reintroduces the 60 s
bug at a larger number: a probe that would have gone healthy on retry 18 of 20 is abandoned by the
wrapper before Docker has ruled, intermittently, on cold boots only.

**Why not longer, either.** A value well past Docker's budget means a genuinely dead probe hangs
`kontra up` after Docker has already decided, with nothing new to learn from the extra minutes.

The rounding-up margin — 5 s to 20 s — exists only so the two clocks cannot expire on the same tick
and hand an operator whichever message won the race. **That margin is reasoned, not measured.** What
is enforced is the side of the budget it falls on: `parity.py` fails any value below Docker's computed
budget, and fails a healthchecked container with no explicit `waitTimeout` at all.

These are ceilings, not expectations. ADR 0052 §3's claim for a warm boot is ~40 s for all 13. **A
cold first boot has not been timed on this box** — the live compose stack makes a from-empty create
impossible here — so which of these ceilings actually gets approached is the one thing in this section
that is reasoned from the probe definitions rather than observed.

## The four kontra image refs

Every image is a `docker:index:RemoteImage` with `keepLocally: true`, and the four kontra-owned ones
are each behind a config key:

| config key | default | run by |
|---|---|---|
| `kontraImage` | `kontra:latest` | `cli` — and exported as `KONTRA_IMAGE` to all three anchor consumers |
| `orchestratorImage` | `kontra-orchestrator:latest` | `orchestrator-api` **and** `orchestrator-infra` — one image, two roles |
| `hostImage` | `kontra-host:1` | `orchestrator-probe` — and exported as `KONTRA_HOST_IMAGE` |
| `workerBaseImage` | `kontra-worker-base:1` | **nothing** — see below |

All four are also stack outputs (`images`), so `kontra doctor` can answer *which images is this
control plane actually running* without shelling to `docker inspect`.

**`RemoteImage` resolves an image that is already in the daemon without consulting a registry, and
that single fact is why `pull_policy` has no counterpart here.** Re-measured for this README, with the
installed 4.11.2 plugin, against `name: kontra-host:1` — a tag that cannot exist on docker.io:

```
 +  docker:index:RemoteImage img created (0.35s)
    id: "sha256:643204e0d29f4caaae8469705cf516ac44ffbce254b666c208110c077467264a"
```

So the same declaration behaves as compose's `pull_policy: never` when the image was built here, and
as a pull when it was not. **The local-build track and ADR 0052 §3's `ghcr.io/medmahmoudi26` pull
track are the same program**; migrating between them is four `pulumi config set` calls, on ADR 0038's
single tag, with no edit to `Pulumi.yaml`:

```sh
pulumi -C control/pulumi config set orchestratorImage ghcr.io/medmahmoudi26/kontra-orchestrator:<tag>
```

`keepLocally: true` on every one, kontra-owned and third-party alike. Without it `pulumi destroy`
takes the images with the containers, and the next `kontra up` is a re-pull — or, for the four built
by hand today, a rebuild measured in minutes against a compose `down`/`up` measured in seconds.
Verified: after destroying a stack holding `kontra-host:1`, `docker image inspect` still returned
`sha256:643204e0…`.

Two details that look wrong until you know the reason:

- **The containers run `${img*.imageId}`, but the env vars carry the `${…Image}` *tag*.** A digest is
  the right thing to run and the wrong thing to hand onward: the control plane starts Workers by name
  through the Docker socket, so `KONTRA_IMAGE` must be a name the Warden can pass to `docker run` as
  written.
- **`workerBaseImage` is declared and no container runs it.** `kontra deploy` COPYs the
  actor-agnostic handler out of it into every actor image (`cli/deploy.go`, `cli/serve.go:197`), so an
  install that lacks it turns the first `kontra deploy` into a build. It appears in no compose service,
  which is exactly why `make image` building *it* and not `kontra-orchestrator:latest` reads as
  arbitrary until you know this.

## What `kontra up` will run against it

**None of this exists yet, and the honest state is worth stating plainly:** `kontra up` today is the
install — `cli/up.go:1` opens *"up.go — `kontra up`, the install"* — which ADR 0052 §2 retires
into this program. Nothing in the repository outside `docs/adr/0052` and this directory contains the
string `kontra-control` — the one apparent hit, `scripts/provision-controller.sh:139`, is
`kontra-controller`, a DigitalOcean tag name. Everything below is the contract this file is built to
satisfy, written down so the Go lane implements the same thing this lane verified.

**The one-line installer holds up its half.** Re-checked: it pins
`DOCKER_PLUGIN_VERSION=4.11.2`, runs `pulumi plugin install resource docker` and then *re-reads*
`pulumi plugin ls` rather than trusting the exit code, and it prints the
`pulumi login file://$KONTRA_HOME/state` warning this file asks for. What it does **not** do is name a
stack, so the one-segment rule above is still only written here — and `kontra up` is still the
install.

```sh
# 1. THE BACKEND, BEFORE ANYTHING ELSE.
pulumi login file://~/.kontra/state

# 2. Project comes from Pulumi.yaml's `name:`; the CLI only picks the stack — and the stack name is
#    one segment. `kontra-control/local` is parsed as <org>/<stack> and a file:// backend has exactly
#    one org, spelled `organization`: measured, `error: organization name must be 'organization'`.
pulumi -C <program-dir> stack select --create local

# 3. The two keys a program cannot guess.
pulumi -C <program-dir> config set workspaces /absolute/path/to/workspaces
pulumi -C <program-dir> config set --secret infraPassphrase <x>      # optional; defaults to local-dev

# 4. Converge. `kontra up --preview` is this with `preview --json` instead.
pulumi -C <program-dir> up --yes --non-interactive

# 5. What §3 says every boot ends with.
pulumi -C <program-dir> stack output consoleUrl
```

Five things that belong to the caller and are not in this directory, each with the failure it
prevents:

- **Step 1 is load-bearing and is the sharpest trap in the tool.** `infra/workspace.ts:91`
  `assertBackend` records it for the container's engine: *"A failed `pulumi login` does not stop the
  CLI — it silently creates an ephemeral Pulumi Cloud account and deploys THERE, state included."* In
  a container the ambient environment is ours; on a developer's laptop an ambient
  `PULUMI_ACCESS_TOKEN` makes that the likely case rather than the unlucky one. ADR 0052 fact 4 asks
  for a second `assertBackend`, on the host, with a louder message. On this box the current backend is
  `s3://kontra-pulumi?endpoint=localhost:8333` — the stack's own SeaweedFS, which does not resolve
  without AWS credentials in the shell — so a caller that skips step 1 is not starting from neutral.
- **The dispatch table.** ADR 0052 §1: the host table accepts `kontra-control` and refuses every other
  project, `planFor` refuses `kontra-control`, and a test asserts the intersection is empty. Two
  `default:` arms that throw.
- **A lock-file mutex.** The infra engine's mutex is a Temporal Entity Workflow keyed by the fqn; the
  host has no Temporal at the moment it needs one, so §1 specifies a lock file with the CLI as the
  only writer. Two concurrent `kontra up`s against one `file://` backend is a corrupted checkpoint.
- **`workspaces` is required and has no default.** Compose could write
  `${KONTRA_WORKSPACES:-${PWD}/workspaces}` because it knew where the operator stood. This program
  does not: `${pulumi.cwd}` is wherever the program was materialised, which on an installed machine is
  not a checkout. A guessed default is a bind mount nobody can find, and the symptom is a workspace
  selector with nothing in it. It is mounted **at the same absolute path inside and out** in all three
  consumers, because a Source id is literally `at:<absolute path>` and that id crosses the queue hop
  from `orchestrator-api` to `orchestrator-infra`. Missing config already fails closed —
  `error: validating stack config: Stack 'x' is missing configuration value 'workspaces'`.
- **Materialising the program.** `<program-dir>` is a checkout here and is not one after
  `curl … | sh`. Whatever writes `Pulumi.yaml` out must also write
  `../images/{postgres-init.sh,logship.sh,logline.py}` beside it — `uploads` read those three with
  `fn::readFile`, relative to the program directory. A `go:embed` of both directories is the obvious
  shape. Issue 16 baking the two logship scripts into an image removes two of the three.

## Running it by hand

```sh
pulumi login file://~/.kontra/state
pulumi -C control/pulumi stack init local
pulumi -C control/pulumi config set workspaces "$PWD/workspaces"
pulumi -C control/pulumi up
```

`local`, one segment. Anything with a slash in it is read as `<org>/<stack>`, and a `file://` backend
accepts exactly one organization name — `organization` — so `stack init kontra-control/local` fails
with `error: organization name must be 'organization'`, which names neither the project nor the fix.
`organization/kontra-control/local` is the fully-qualified spelling of the same stack.

Every other default is the value `docker compose --env-file .env.quickstart config` already resolves
to, so a bare `up` and the documented compose invocation produce the same topology.

## What survives a `down`

`pulumi destroy` removes the 15 containers and the network and **keeps every volume**: 9 of the 13 carry
`retainOnDelete: true`, so Pulumi drops them from state and leaves the data on disk. That is
`docker compose down`, not `down -v`.

**There is deliberately no flag here that deletes them.** Destroying a volume is `docker volume rm`
typed by a person. `infra/paths.ts` already warns that `down -v` orphans Machines, and ADR 0052
extends that warning to `~/.kontra/state`: `pulumi stack export` is the answer for both roots.

The volume names are compose's own — `kontra_postgres-data`, `kontra_seaweed-data`, … — because
`docker volume create` is idempotent and keeping the names means `kontra up` **adopts** an existing
install's data. Renaming them would hand the operator an empty control plane that looks like a clean
install. Measured on this box, six days into one compose stack: **41 GB across the 12 `kontra_`
volumes** — 34 GB of it `seaweed-data`, 6.2 GB `registry-data`, 129 MB of Postgres — which is what a
rename silently orphans.

## Migrating a box that is already running compose

`pulumi up` from empty state will **collide**, and it is worth knowing before rather than during.
Re-measured on this host just now: **all 13 `container_name`s exist** (`docker ps -a` lists 21
`kontra-*` containers — the other 8 are the Temporal UI and live Workers), the `kontra` network
exists, and **13 of 13 volumes exist** plus one orphan, `kontra_tmux-sock`, left by the
shared-tmux-socket removal that the compose file records as a security fix. Volumes and the network
adopt silently; container names do not — Docker refuses a second container with the same name.

So: `docker compose down` first (not `-v`), then `pulumi up`. Or `pulumi import` the containers, which
buys nothing here because they are recreated from the same images anyway.

## Keeping the two files honest

```sh
python3 control/pulumi/parity.py
```

It resolves both sides — `docker compose config --format json` and `pulumi preview --json` — and
compares container names, image refs, every environment variable, commands, entrypoints, hostnames,
labels, published ports, mounts, network aliases, healthchecks field-for-field, the `dependsOn` graph
edge-for-edge, each gate's condition, and the loopback assertion. It uses a scratch `file://` backend
and a throwaway stack, leaves nothing in the working tree, and never touches `~/.kontra/state`.

Delete it in the same commit that deletes `docker-compose.yml` (ADR 0052 Consequences). Until then the
two files are two spellings of one topology and a drift between them is invisible, because each one
works on its own.

## Verified, and how

Re-run for this README, not inherited.

| | |
|---|---|
| `pulumi preview` | **44 resources plan clean** — 15 containers, 15 images, 13 volumes, 1 network, 1 provider, 1 stack. Every input passed the provider's own `Check`, so no property name is guessed. |
| `parity.py` | **13 pre-existing problems, none of them the registry.** 15 containers / 13 volumes / 15 images matched; 21 ordering edges checked; 6 published ports, all `127.0.0.1`. What it reports is `porter`, which is missing here and from its own map, and the orchestrator env/mount drift ADR 0056 already records. |
| `parity.py` is not vacuous | **Nine mutations of `Pulumi.yaml`, each caught with the right message**, the file checksummed before and restored byte-identical after every one: `temporal` `waitTimeout` 660→60 (*"< Docker's own budget 640 — Pulumi gives up first"*); `bind` default widened to `0.0.0.0` (7 problems — 6 ports plus the loopback assertion); the `victoriametrics` alias renamed to `victoria-metrics`; `KONTRA_S3_BUCKET` dropped from the shared env (caught in **all three** consumers, which is the `fn::split` splice working); `postgres` `restart` flipped to `always`; `cli` given a healthcheck compose does not have; `logship` given `wait: true` with no healthcheck (*"the provider errors on this"*); `temporal`'s `dependsOn` on the one-shot removed; the one-shot's `mustRun: false` + `attach: true` inverted (*"needs attach:true + mustRun:false so the exit code is the gate"*). |
| the 60 s default is real | Read out of the installed plugin's own schema, not from the ADR: `pulumi package get-schema docker@4.11.2` → `waitTimeout` *"Defaults to `60`"*, and `wait` *"requires your container to have a healthcheck, otherwise this provider will error"*. |
| `RemoteImage` does not pull a local image | Real `pulumi up` against `name: kontra-host:1` — impossible on docker.io — **created in 0.35 s** and returned `sha256:643204e0…`. |
| `keepLocally: true` | After `pulumi destroy` of that stack, `docker image inspect kontra-host:1` still returned `sha256:643204e0…`. |
| the YAML builtin list | Read out of `pulumi-language-yaml`: no conditional, no loop. The costs section above is the consequence, not a guess. |
| `uploads` land before the entrypoint | A throwaway busybox container whose own `command` was `ls -l` of its upload directory: both files present, exit 0. So postgres's init script is in `initdb.d` before the entrypoint scans it. |
| the upload's **mode** | Same container, both spellings side by side: `executable: true` → `-rwxr--r--` (0744); `permissions: "0755"` → `-rwxr-xr-x`, which is what the bind mount it replaces gave. That bit decides whether postgres's entrypoint *runs* the script or *sources* it — it tests `-x` as the `postgres` user — and sourcing leaks the script's `set -eu` into an entrypoint that deliberately runs without `-u`. The program writes `permissions: "0755"`. |
| the one-shot's gate mechanism | The same throwaway container carried `mustRun: false` + `attach: true` + `logs: true`: the create call blocked for its whole 1 s run and surfaced `exitCode: 0` and the container's stdout as outputs. That is `service_completed_successfully` — a non-zero code would have failed the create and `temporal`'s `dependsOn` with it. |
| a missing asset fails closed | With `assetsDir` pointed at a directory that does not exist: `Error reading file at path ../nope/postgres-init.sh: no such file or directory`, and the whole converge stops. |
| missing required config fails closed | `error: validating stack config: Stack 'x' is missing configuration value 'workspaces'`. |

**`pulumi up` has never been run against this program.** The live compose stack on this box owns all
13 container names and the `kontra` network, so a create from empty state collides by construction —
see the migration note above. Everything short of that is verified; the first real `up` is on a host
with no compose stack running, which is also issue 03's cold-machine acceptance criterion. Two claims
are therefore reasoned and not measured: **that a second `kontra up` reports no changes**, and **that
the exited one-shot produces no diff on the second converge** (reasoned from `mustRun: false` plus the
provider not reading `attach`/`logs`/`mustRun` back off a container).

## What this program does not do, and who does it

Beyond the five in *What `kontra up` will run against it*:

- **The passphrase.** ADR 0052 §4 puts the host engine's `PULUMI_CONFIG_PASSPHRASE` in the OS
  keychain. `kontra-control:infraPassphrase` in this program is a *different* secret: it is the one
  handed to `orchestrator-infra` for ADR 0019's engine and the `pulumi-state` volume.
- **Seed the secret store.** `~/.kontra/secrets.yaml` → the store, per §4. **`DIGITALOCEAN_TOKEN` and
  `KONTRA_CLOUD_CREDENTIAL` are passed to `orchestrator-infra` present-and-empty and have no config
  key here, on purpose** — and this is a deliberate behaviour change from compose, where an operator
  could set both in `.env`. A cloud credential must not become Pulumi stack config (`credential.ts`
  negative 4, guarded by `pulumiState.test.ts` with a non-vacuous control case); the value reaches the
  infra worker through the secret store on the `secrets-store` volume, resolved by
  `resolveProviderEnv` inside the activity that is about to converge. A config key for either would
  put the credential back exactly where ADR 0034 took it from, and no existing test would notice.
- **Print the login.** §3 says `kontra up` ends by printing the credentials file path and the console
  URL on **every** boot, not only the first, because the path is what an operator looks for on the
  second day. The URL is the `consoleUrl` output, built from `bind` and `apiPort` so a changed port
  cannot leave the printed line pointing at nothing.
- **`kontra doctor` reporting both Pulumi versions.** ADR 0052 Consequences makes host-CLI-vs-SDK skew
  a real axis. Measured here: host CLI 3.244.0, `@pulumi/pulumi` 3.256.0, `@pulumi/docker` 4.11.2.
