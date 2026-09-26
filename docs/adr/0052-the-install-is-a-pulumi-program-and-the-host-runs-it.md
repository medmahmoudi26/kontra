# 52. The install is a Pulumi program, and a second engine on the host runs it

## Status

**Accepted, 2026-09-26.** Amends **0047**: the supported local install is still one topology, but it
is converged by Pulumi rather than by `docker compose`, and Docker is no longer the only host
prerequisite. It does not reopen **0047**'s reasons for splitting the appliance back into services —
those services stay, with the same healthchecks, on the same private network.

Amends **0019**: there is now a *second* Pulumi engine, with a different program shape and a
different mutex. **0019**'s engine — inline Automation API TypeScript, its own PID, a Temporal Entity
Workflow keyed by the stack fqn — is unchanged and keeps every fleet.

Restates **0034** §4 by adding a negative: the cloud credential does not reach the host engine,
because the host engine has no provider that could use one. The five negatives
(`infra/credential.ts`) are untouched and still measured about `orchestrator-infra`.

**Retires 0031.** The appliance is removed, not renamed — see §2. **0031**'s measurements stand and
are not reopened; what changes is that its premise no longer holds here.

It is not a new idea. `legacy/0019` decided *"the control plane stays `docker-compose.yml`; Pulumi
provisions the Controller and runs compose"*, and **0032**'s clause table records the fate of that
half in as many words: **Split** — *"The Pulumi-provisions-the-Controller half never shipped — there
is no controller program under `infra/programs/`, and `scripts/provision-controller.sh` does that
job."* This ships it, four ADRs later, for the local control plane rather than the cloud one.

## Context

kontra goes public. The install is the first thing anyone sees and it is currently four commands, two
clones, three `docker build`s, and a `grep` of a log for a generated password. Every one of those is
defensible and together they are a bounce.

Five facts were established against the tree before deciding. Three contradict what a first reading
assumes.

1. **`@pulumi/docker` already does health-gated ordering, so the port is mechanical.** The obstacle
   everyone names first — 13 services, 10 `healthcheck:` blocks and 14
   `depends_on: condition: service_healthy` gates — is not an obstacle. `@pulumi/docker` 4.11.2's
   `Container` carries `healthcheck` (`tests`, `interval`, `timeout`, `retries`, `startPeriod`) and
   `wait: true` — *"the Docker container is waited for being healthy state after creation. This
   requires your container to have a healthcheck, otherwise this provider will error."* `wait` plus
   `dependsOn` **is** `service_healthy`. The one trap is `waitTimeout`, which defaults to **60
   seconds**; compose's gates have no timeout, and first-boot Postgres and Temporal exceed it. It is
   set explicitly per service or the port fails as a flake.

2. **Pulumi YAML needs no language host, which is what makes a Go CLI able to converge.** **0019**
   runs inline TypeScript because a fleet's desired state is computed per converge from a Run's
   arguments. The control stack has no such arguments — it is a static topology — and a YAML program
   resolves providers as binary plugins. So `kontra` (Go) shells to `pulumi` and needs no Node,
   no `@pulumi/*` package, and no Automation API on the host.

3. **The host cannot be given a cloud credential without undoing 0034.** `credential.ts`'s five
   negatives are measured about one process: `pulumiState.test.ts` runs a real `pulumi up` against a
   `file://` backend with a sentinel and sweeps the state directory, *with a control case that passes
   the same sentinel as stack config and finds it*. None of that describes an operator's laptop. A
   host program that wanted a DigitalOcean token would put the credential back where **0034** took it
   from, and no existing test would notice.

4. **`assertBackend` is the sharpest trap in the tool and it is about to exist twice.**
   `infra/workspace.ts` records it: *"A failed `pulumi login` does not stop the CLI — it silently
   creates an ephemeral Pulumi Cloud account and deploys THERE, state included."* In a container the
   ambient environment is ours. On a developer's machine a `PULUMI_ACCESS_TOKEN` is likely, which
   makes the trap more probable on the half that is new.

5. **Most of what this buys is already built and unread.** `stackPreview` is a complete activity
   (`activities/infra.ts:223`), proxied by `workflows/stack.ts:111`, reachable as
   `kontra fleet preview`. `.pulumi/history/<project>/<stack>/*.history.json` holds one record per
   converge — measured on the live volume: `kind`, `startTime`, `endTime`, `result`,
   `resourceChanges`, `config` — and nothing reads it. `infra/state.ts` reads only `stacks/`.

Kubernetes remains out of scope. Pulumi Cloud is not used and is refused rather than defaulted to.

## Decision

### 1. Two engines, and their dispatch tables are disjoint

| | Host engine | Infra engine |
|---|---|---|
| Runs in | `kontra`'s process tree, host `pulumi` | `orchestrator-infra`, its own PID (**0019**) |
| Program | Pulumi **YAML** on disk | inline TS, Automation API |
| Projects | `kontra-control` | `kontra-fleet`, `kontra-docker-fleet`, … |
| Backend | `file://~/.kontra/state` | `file:///data/pulumi-state` |
| Providers | `docker`, `command`, `random`, `tls` | `digitalocean`, `docker`, `command` |
| Cloud credential | **none, structurally** | one `SecretRef`, resolved at the last hop |
| Mutex | a lock file; the CLI is the only writer | Temporal Entity Workflow keyed by the fqn |
| Triggered by | a person typed a command | a Run's `fleet.up()` / `f.place()` |

**The host table contains `kontra-control` and refuses every other project; `planFor` refuses
`kontra-control`.** Two `default:` arms that throw, and a test asserting the intersection is empty.

**Everything is a container, and every container belongs to exactly one stack.** The control stack
is `kontra-control`; a Fleet is one stack of its own — `kontra-fleet/<tag>` or
`kontra-docker-fleet/<tag>`, which is how **0037** already names them. There is no embedded service
and no process that is a control plane by itself. (The word for a Fleet is **Fleet**, not *cluster*:
`infra/CONTEXT.md` puts cluster on its _Avoid_ list, and **0047** spends it on the control plane's
own services.)

This is not tidiness. `infra/stacks.ts` says the table exists because *"a caller that could supply
the program could provision anything the credential allows"* — an HTTP route decides *which* stack,
never *what is in it*. A second engine reopens that from the other side: if the host table could name
`kontra-fleet/dns`, then `kontra up` would be a path to cloud Machines with **no Lease, no
`checkCloudCredential`, no Temporal record, no mutex and no audit**. That is distinct from the
operator Fleet `infra/CONTEXT.md` already blesses — `kontra fleet up` holds no Lease *and still goes
API → Temporal → infra worker*.

The corollary is §1's last row: **the host engine may declare no provider that takes a cloud
credential.** It is enforced by the provider list above being a constant, and swept the way
`machineSecret.test.ts` sweeps.

### 2. `kontra up` is the install, and the appliance is removed

`kontra up` converges `kontra-control` with the host engine. `kontra infra up` — which meant *"drive
a deployment contract written in YAML"* — retires into it: the contract is now Pulumi YAML rather
than compose YAML, which is a change of file, not of verb.

**0031**'s appliance exists so that starting a control plane needs nothing installed — not Docker,
not compose. **§3 requires Docker and Pulumi, so that premise is void.** What would remain is 11,719
lines of a second control plane that **0047** already made unsupported: shipped in the binary,
advertised in `--help`, and offering a first-time reader two ways to start kontra on the day the
repository goes public. It goes.

**The removal is not `rm -rf cli/appliance`,** and this ADR carries the audit because the change
that does it must start from one rather than discover it. Derived from per-file import blocks. Two methods that
look right and are not: a **grep** for `cli/appliance` matches three *comment* lines in
`runtime/handler/codecserver` and reads as an import, which is how the first draft of this paragraph
got the codec exactly backwards; and **`go list`** cannot produce this table at all, because every
file under `cli/` is in the single package `github.com/medmahmoudi26/kontra/cli` and collapses to one
row. File attribution has to be read out of the import blocks themselves.

| package | imported from OUTSIDE `cli/appliance` | fate |
|---|---|---|
| `bundle` | `bundlecmd.go`, `orchestrator.go`, `release.go` (+ `orchestrator_test.go`, `release_test.go`) | **survives** — moves out |
| `registry` | `bundle.go`, `deploy.go`, **`up.go`** (+ `bundle_test.go`, `deploy_registry_test.go`) | **survives** — moves out |
| `codec` | `up.go` only | goes |
| `kv` | `up.go` only | goes |
| `objstore` | `up.go` only | goes |
| `temporalsrv` | `up.go` only | goes |
| the root package | `scale.go`, `temporalui.go`, `up.go` (+ `scale_plane_test.go`, `temporalui_test.go`) | goes |

`registry` is imported by `up.go` **as well as** by `bundle.go` and `deploy.go`, which does not
change its fate but does change the order: when `up.go` goes, `registry` loses one importer rather
than its last, so a "no importers left" check must not read it as having become appliance-only.

The test files move with their packages. `bundle` is imported under the alias `applbundle`, so a
rename that greps for the bare path will miss four call sites.

**`runtime/handler/codecserver` imports nothing from `cli/appliance`.** The direction is the other
way: `cli/appliance/codec` imports *it*. So codecserver is the shared thing, it already lives
outside the appliance, and deleting the appliance does not break it — it orphans its only caller.
There is nothing to extract for the codec.

So the real shared surface is **two packages, `bundle` and `registry`**, both reached by commands
that outlive the appliance — `kontra bundle`, `deploy`, `release`, and the orchestrator hydrator.
Everything else falls out with `up.go`'s appliance path, and each of those has a container
counterpart already running in the stack §3 converges: Temporal, SeaweedFS, Redis, `registry:2`.

**REMOVING THE APPLIANCE IS NOT REMOVING THE `kontra` IMAGE, and conflating the two breaks the
build.** `control/images/Dockerfile.appliance` produces `kontra:latest`, and that image is
load-bearing for two things that have nothing to do with the appliance:

- `Dockerfile.orchestrator:2-3` is `ARG SPA_IMAGE=kontra:latest` / `FROM ${SPA_IMAGE} AS spa-src`,
  and then copies the CLI binary (`:71`), `duckdb` (`:77`) and `/var/lib/kontra/bundles` (`:45`) out
  of it. The orchestrator image is built **from** the appliance image.
- `programs/dockerFleet.ts` takes its Warden image from `process.env.KONTRA_IMAGE || 'kontra:latest'`.

So the appliance's *code* goes and the *image* stays — renamed to what it actually is, a CLI image.
A change that deletes the Dockerfile along with the package fails the first tag, and fails it in the
orchestrator build rather than anywhere near the appliance.

**One call site has to be unpicked by hand:** `cli/scale.go:426` calls `appliance.ReadEndpoints(dir)`
to recognise that it is talking to an appliance. That branch goes with the thing it detects.

`control/orchestrator/src/workflows/appliance.ts` goes whole. It exists so that a process with no
Pulumi engine refuses `fleet.up()` in one second instead of failing the workflow task forever — the
invisible failure **0031** §4 says must not be how an appliance says "no provisioner here". With no
appliance there is one infra bundle, and `workflows/infra.ts` is it.

`kontra up --preview` is the same converge through `stack.preview()`. It exists on day one rather
than being added later, because the operator case for it — *"this will replace `temporal`"* — is
stronger than the fleet case that already has it.

### 3. One command installs, and the same command is CI

```
curl -fsSL https://raw.githubusercontent.com/medmahmoudi26/kontra/main/get.sh | sh
kontra up                                   # 13 services, healthy, ~40 s
```

The installer is interactive on a TTY and strictly non-interactive without one, which is what makes
the quickstart path and the CI path the same path (**0047** §4's rule, kept). It resolves Docker,
resolves or installs `pulumi`, and **refuses rather than falling back**: no Docker is an error, and a
`pulumi login` that would reach Pulumi Cloud is an error naming fact 4 above.

Artifacts move on the single tag **0038** already defines, and there is no new version axis: **the
per-platform `kontra` binaries on the GitHub release, and the control plane's own container images
at `ghcr.io/medmahmoudi26`.** The install is a pull, not a build — four kontra-owned images are
built by hand today, and that is the difference between seconds and minutes.

**A vanity domain, a Homebrew tap and a PyPI release are deliberately not in this ADR.** The script
— `get.sh`, because a tracked `install.sh` at the root is already the developer bootstrap (venv,
buf, proto codegen) and is a different script for a different reader — is served from the repository, which needs no DNS and no hosting; `get.kontra.run` becomes a
redirect to it when there is a site to put it on. `kontra-sdk` keeps publishing as it does today.
Each of those is an addition that changes nothing here, and none of them is on the critical path to
an install that works.

`kontra up` ends by printing the credentials file path and the console URL — **on every boot**, not
only the first. The path is what an operator looks for on the second day.

### 4. The credentials file seeds the store; it is never the store

`~/.kontra/secrets.yaml` is read by `kontra up` and written into the secret store. Nothing reads it
at converge time: a Run still names a `SecretRef`, and `resolveProviderEnv` still resolves inside the
activity that is about to converge. It replaces `kontra secret set` typed by hand — not the store.

**Provider credentials never become Pulumi stack config.** That is `credential.ts` negative 4 and
`pulumiState.test.ts` guards it with a non-vacuous control. The host engine holds exactly one secret,
`PULUMI_CONFIG_PASSPHRASE`, in the OS keychain; because of §1 it protects a topology and not an
account.

**The admin password is `changeme`, and `changeme` is bound to loopback.** The generated-password
boot line goes, and with it the branch that says a password cannot be recovered. In exchange: the
control plane **refuses to serve on a non-loopback bind while the password is still the default.**
The README already makes `KONTRA_BIND=127.0.0.1` the security control; this makes the two defaults
fail together instead of independently. A documented default credential on a public repo, on a
control plane that can spend money, is scanned for — and a forced change at first login does not help
the API or the CLI, which do not go through login.

### 5. A Fleet provider is a registered module, in-tree

`programs/fleet.ts` claims to be *"the ONLY provider-coupled file"* and `programs/dockerFleet.ts`
imports `machinesFor`, `placementsOf`, `validateTag`, `FleetArgs`, `MachineEntry` and
`PlacementArgs` from it. It is two things: the DigitalOcean provider, and the shared kernel. They
split.

```
infra/fleet/       contract.ts  FleetArgs, PlacementArgs, MachineEntry, FleetOutputs, FleetProvider
                   placement.ts placementsOf, machinesFor, validateTag   ← out of fleet.ts
                   registry.ts  replaces planFor's switch
infra/providers/   digitalocean/  docker/  <contributed>/
```

```ts
export interface FleetProvider {
  project: string;                                    // also the fqn's first half
  providerEnvVar: string;                             // '' = no cloud credential
  coerceArgs(raw: Record<string, unknown>): FleetArgs; // must drop what it does not name
  program(args: FleetArgs): () => Promise<FleetOutputs>;
}
```

`shared/conformance/fleet.json` is driven by **every registered provider**, which is the only form of
"good contract" **0035** trusts. Providers register **at build time**: no plugin path, no dynamic
import from a workspace, because §1's reason applies unchanged. A contributed fleet is a pull request
and is AGPL with the rest of the control plane.

### 6. Settings gains `Infra`, and it is how an install is verified

A read-only Surface whose subject is the **Machine**, split into the control stack and the fleets —
one engine each, visibly. Under each Machine is whatever is serving on it. Four sources, joined:

| | |
|---|---|
| Pulumi checkpoint | name, size, region, address, price, created/modified (`infra/state.ts` reads this today) |
| Pulumi history | one record per converge, with `resourceChanges` and duration (unread today) |
| Temporal | `DescribeTaskQueue` per queue, per identity (`pollers.ts:QueueState`) |
| the registry | the digest a Placement pinned, against what the tag resolves to now |

**The join is `identityHost` and costs no new field.** A Worker's identity is `<pid>@<host>@<queue>`
(`shared/core/src/queues.ts:workerIdentity`) and a Machine's hostname *is* its name — `dockerFleet`
sets `hostname: name`, the cloud program names Machines `kf-<tag>-NN`.

Two rules the surface may not round off. **Four poll states, not three**: `serving` inside
`POLL_FRESH_MS`, `stale` for an identity Temporal still lists (it keeps them ~5 minutes after a
Worker stops), `undated` for `lastPoll === 0` which `pollIsFresh` counts as neither, and `nothing
polling` for a queue with no identity at all — the Placement converged and a dispatch to it hangs to
`StartToClose`. And **unattributed pollers are shown**, because `queues.ts` is explicit that a Worker
we cannot attribute is `unknown`, not `none`; that bucket is where a developer's `kontra serve
--actor` against the same control plane appears.

It reads only. `dashboard.ts`'s rule stands: converging goes through Temporal so the operation has an
id, a retry policy and a record.

### 7. `kontra update` is the same converge, and a volume delete is a refusal

An install nobody can upgrade is an install people pin and abandon. `kontra update` re-resolves the
image digests, previews, and converges — the same program, the same engine, a different desired
state. It is not a second code path.

**Its whole safety rests on one property of the thing it is built from, and that property cuts both
ways.** §1's own warning applies here at its sharpest: *Pulumi's desired state is TOTAL.* A converge
that no longer mentions a volume does not leave it alone — it deletes it. On a Fleet that costs a
Machine; here it costs every Dataset the install has ever produced.

So the volumes are graded, in the program, by what losing one actually costs — and **ten of the
eleven are protected**, because protection is free and the only cost is having to unprotect
deliberately.

**`seaweed-data` and `postgres-data` are ONE unit.** Postgres holds the DuckLake *catalog* — which
files are live — plus the materialization ledger and the Dataset records carrying tags and renames.
Seaweed holds the parquet those rows point at, plus `cas/`, `units/` and `history/`. Protect one and
not the other and you get a catalog pointing at nothing, or gigabytes of files nothing can find.
`data/maintenance.ts`'s reclamation chain assumes the catalog is the authority for liveness, so an
out-of-band loss on either side breaks that assumption **silently**. They are protected together or
the protection means nothing.

Two that look like cache and are not. **`redis-data`** carries `object_state` and `global_state` —
cross-Session durable author state with no other home (`kontra-global:{actor}:{key}`); only
`unit_state` is scratch, and that is safe because Units are idempotent. **`registry-data`** holds the
digests Placements are pinned to (ADR 0011's `expected_digest`): rebuilding an image yields a *new*
digest, so a Fleet recorded against the old one can never be re-run as recorded. It is rebuildable
as software and not rebuildable as the thing a Run referenced.

`temporal-dynamicconfig` is the only genuinely rebuildable volume — a config file that lives in the
repo — and it is the only one left unprotected, with a comment beside it saying so. An unprotected
volume must read as a decision rather than as an oversight.

**The dangerous case is replacement, not deletion.** A converge that creates a volume under a new
name and orphans the old one contains no delete at all: a check looking for destructive operations
passes, the stack comes up healthy and empty, and the data sits on disk where nobody looks. So
`kontra update` asserts **identity** — each volume still named what it was, still attached to the
same service — rather than inferring safety from what a plan does not contain. A thing that looks
healthy while being wrong is worse than a thing that fails.

**`pulumi-state` is the one whose loss is not local.** It is the record of every Machine the control
plane owns. Losing it does not lose data — it loses *the ability to destroy cloud Machines that are
still billing*, which `infra/paths.ts` already names: a `down -v` that took it "would orphan every
machine we own". It is therefore protected, and it is the one volume `kontra update` exports before
it converges, because a protected resource is safe from Pulumi and not from `rm`.

`protect: true` is not documentation: Pulumi refuses to delete a protected resource and fails the
converge, so the program cannot express the destructive plan at all, for any caller — `kontra up`,
`kontra update`, or a person running `pulumi` by hand with the wrong stack selected. The identity
assertion above is what covers the failure protection cannot see. Both name the volume when they
refuse, because a person who trips either should be told which one and why rather than handed a
provider error about a resource URN.

What it does not decide: **schema migration**. Temporal's auto-setup owns its own; `orchestrator-db`
and the DuckLake catalog do not, and nothing here says what happens when an image expects a shape
the volume does not have. Naming that before a migration exists would put a word on a decision
nobody has made.

## Consequences

- **Docker is no longer the only host prerequisite.** `pulumi` joins it. The installer resolves both
  and the README says so in the first paragraph rather than in a footnote.
- **Two state roots.** `~/.kontra/state` and the `pulumi-state` volume. `infra/state.ts`'s
  `stateDir()` becomes two roots and the `/infra` reader concatenates; converging never crosses.
  `paths.ts`'s warning that `down -v` orphans Machines now applies to the host root as well, and
  `stack export` is the answer for both.
- **`docker-compose.yml` stops being the install** and becomes a development convenience or is
  deleted. Only one topology is documented (**0047** §4's rule survives its own file).
- **The binary loses its second control plane.** 11,719 lines and six subpackages, minus whatever the
  audit finds is shared. `kontra --help` stops offering two ways to start, and the wiki stops
  describing a path nobody should take.
- **The appliance's absence removes a whole class of "it works here and not there".** There is one
  Temporal, one object store, one KV and one registry, and they are the same images on a laptop and
  on a Controller.
- **`kontra fleet history` and per-stack cost become directory reads.** `priceMonthly` is already in
  `state.ts`'s `SHOWN`; crossed with history it answers what a Run *spent* rather than what
  `cost_words()` *projected*.
- **The default password is a documented credential.** The loopback gate is the whole mitigation and
  it is load-bearing; widening `KONTRA_BIND` before changing it must fail closed, with a test.
- **A second `assertBackend`**, on the host, with a louder message than the container's.
- Pulumi version skew between the host CLI and the cluster's `@pulumi/pulumi` is now a real axis. The
  installer pins a minimum and `kontra doctor` reports both.

## What this does not decide

**Whether the control stack is also a Fleet.** It is tempting — Docker containers, like
`dockerFleet` — and it is refused here without a mechanism being designed: a Fleet is capacity with
Leases and Placements, and `machines: 0` coerces to a destroy. That semantics does not belong on the
control plane. Separate project, same reader, different vocabulary.

**Where a contributed provider's credential comes from.** §5 gives a provider a `providerEnvVar` and
**0034** gives it a `SecretRef`. Nothing yet says how a third party's provider declares *which*
secrets it needs, or what refuses a provider that asks for two. Naming it before a second cloud
exists would put a word on a decision nobody has made.

**Whether the landing illustration is redrawn from §6.** The marketing diagram and the console now
describe the same object and do not share a drawing.
