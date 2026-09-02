# 34. Where the cloud fleet lives: compose keeps the provisioner, and a fleet is named for its cloud

## Status

**Accepted, 2026-08-25.** Records three decisions that arrived together and would otherwise be
discovered separately: which deployment owns cloud provisioning after **0031**, what the word
`fleet` means once it means more than one cloud, and where the provider credential comes from.

Downstream of **0031** §4, which moved cloud provisioning out of the appliance and kept the infra
**queue**. This ADR restates that carve-out by quotation rather than paraphrase (§2) and settles the
questions §4 left implicit. Leans on `legacy/0019` for the shape it does not change — programs are
inline Automation API TypeScript, the engine runs in its own process, a Temporal Entity Workflow
keyed by the stack fqn is the mutex — and on **0032**'s clause table, which already records that
"**0031** restates where the credential lives" without saying what replaces the env var. This says.

It **supersedes no ADR**, and it does not reopen **0031** §3's loopback posture or **0023** §12.
The credential half governs `.scratch/ux-v2/issues/24-fleet-credentials-from-the-secret-store.md`,
which is the change that implements it, and depends on issue 19's secret store.

## Context

`fleet` has meant one thing for as long as it has existed — DigitalOcean Droplets converged by one
Pulumi program — and three pressures now land on that word at once. **0031** took cloud provisioning
out of the appliance, so there are two deployments with different capabilities and nothing written
down about which owns what. The UX v2 secret store gives credentials a home, and the cloud token is
the credential most worth moving. And a second provider is wanted, which makes `fleet` ambiguous in
every sentence it appears in.

Six things were established against the tree before deciding. They are cited by file because three
of them contradict the shape a first reading assumes.

1. **The credential is read in exactly one function, and the property worth preserving is not "it is
   an environment variable".** `infra/workspace.ts:engineEnv()` reads `DIGITALOCEAN_TOKEN` "here and
   nowhere else in the orchestrator: it must reach the provider without ever reaching stack config,
   state, or a worker machine." The file's own header gives the mechanism: "Credentials ride in
   `envVars`, never in stack config. Config lands in `Pulumi.<stack>.yaml` and its encrypted form in
   state; an env var does not appear in state at all." The invariant is about *state and Machines*.
   An env var is one way to satisfy it, and not the only one.

2. **The resolution point is already per-operation; what cannot change without a restart is the
   process environment.** `selectStack` calls `engineEnv()` on every `up`, `preview` and `destroy`,
   so the token is read at the last hop already. What is missing is anything behind that read: a
   compose service's environment is fixed at container start, so rotating means editing a `.env` on
   a controller and recreating a service. That is issue 24's complaint, and it locates the fix
   precisely — a store behind an existing read, not a new read.

3. **Two more secrets live in that same process, and issue 24 moves only one.**
   `PULUMI_CONFIG_PASSPHRASE`, which `assertBackend` refuses to run without because "an empty
   passphrase silently changes how secrets encrypt", and `KONTRA_SSH_KEY` — `panels/ssh.ts:keyPath`,
   `programs/fleet.ts:312`, mounted as `/run/secrets/fleet_key`. Any reading of "the cloud credential
   moves to the secret store" that concludes the infra worker ends up secret-free is wrong.

4. **Fleet configuration is already half-expressible, and the missing half caused a measured
   outage.** `stacks.ts:coerceFleetArgs` accepts `region`, `size`, `image`, `vpcUuid` and `sshKeyIds`
   off the wire, and `fleetProgram` merges `{ ...FLEET_DEFAULTS, ...args }`. But Python
   `fleet.up()` exposes `region=` and `size=` and nothing else. `programs/fleet.ts`'s header records
   what that gap produced: "the one combination reachable from a caller was a NEW region with the OLD
   VPC, which DigitalOcean rejects outright because a VPC is regional. MEASURED on a fresh nyc1
   controller: the first fleet run put its Machines in sfo3, where they could not reach the
   controller's Temporal or Redis, and the run hung on `f.ready()` until it was cancelled." The
   region and the VPC are one fact, and the caller could name half of it.

5. **`FLEET_DEFAULTS` is frozen at module load, deliberately, and the reason is a mutable process
   environment.** "Read once — a provision must not change region half-way through because somebody
   edited the environment of a running worker." A defence built against configuration that lives in
   an environment; it costs a restart to change a default, and it has nothing to defend against
   configuration that arrives with the request.

6. **The payload codec is a claim-check, not encryption, and a cloud token is far under its
   threshold.** `codec/claimCheck.ts`: offload iff `data.length > threshold`, default 128 KiB
   (`KONTRA_S3_THRESHOLD`). A DigitalOcean token is about seventy bytes. Anything at that size rides
   **inline in workflow history in the clear**, for the namespace's whole retention, readable by
   anything that can describe the execution. `StackWorkflowInput.args` is a
   `Record<string, unknown>` that crosses exactly that boundary.

One more thing that is not a finding but decides a naming question below: `programs/fleet.ts` calls
itself "the ONLY provider-coupled file in the stack, exactly as `main.tf` was. Everything above it
talks about **Machines** and **Roles**; moving clouds means editing this file and nothing else."
`infra/CONTEXT.md` states the same rule as vocabulary: "Provider nouns stay out of the language."

## Decision

### 1. Cloud provisioning belongs to the compose controller. That is a deployment fact, not a flag

The compose controller keeps `orchestrator-infra`, the Pulumi CLI and provider plugins baked into
its image, the state directory at `KONTRA_PULUMI_STATE_DIR` (`/data/pulumi-state`), and the cloud
credential. **The appliance has none of them** — no Pulumi, no provider plugin, no state directory,
no cloud token (**0031** §4, locked decision 4).

The operator-facing answer is already built and does not change: `kontra fleet` is an HTTP client
(`cli/fleet.go` — `POST /api/infra/stacks/<fqn>/<op>`), so somebody running the appliance who needs
Machines points the CLI at a compose controller. One binary addresses either control plane.

**Nobody adds a `--cloud` flag to `kontra up` without reopening this ADR**, and the flag is written
here as four costs rather than as a prohibition, because prohibitions get argued with and costs get
counted:

- **Four provider plugins and the Pulumi CLI enter a four-platform artifact matrix** whose hard part
  is already reproducibility rather than mechanism (**0031** §6). They are the largest, least
  checksummable things that could be added to it.
- **A cloud credential lands on a laptop whose entire security posture is that there is nothing to
  reach** (**0031** §3). Loopback deletes #19 §2's question by deleting the remote caller; a
  credential that can spend money re-introduces the stake without re-introducing the caller.
- **The three-roles-in-one-process merge becomes unsafe again.** Pulumi's Node language host installs
  process-global `unhandledRejection` / `uncaughtException` handlers for the duration of every inline
  run, so an unrelated rejected promise anywhere in the process fails the in-flight `up`. That hazard
  leaving is what made the merge free (**0031** §1, §4); a `--cloud` flag brings it back into a
  process that now also serves the API, the materializer and the Monitor.
- **The Pulumi state directory ends up on a machine that gets reinstalled.** This is the one that
  matters most and it is not a laptop-grade failure: that directory is the only record of Machines
  that cost money and reach hostile third-party infrastructure. `workspace.ts` already keeps it on
  "a volume no teardown path removes — a `docker compose down -v` that took this with it would
  orphan every machine we own." A laptop reformat is a `down -v` with no undo.

### 2. The infra queue carve-out is **0031** §4's, quoted rather than restated

Quoted, because a paraphrase of a carve-out is how a carve-out drifts, and finding 3 of **0031**
records what a careless reading of this one takes with it — the Monitor and the retention sweep:

> The `kontra-infra` queue survives with `stackWorkflow` and the cloud credential removed from it,
> because two of its three workflows have nothing to do with provisioning:
>
> - **`tmuxSessionWorkflow`** — session existence on a machine is Fleet authority, and it is what the
>   **Monitor** (`DashboardPage`, **0020**) is built on. Local panes are exactly the case the
>   appliance serves best: a served worker's `kontra-wf-<queue>` tmux session on the operator's own
>   host.
> - **`sweepDatasetsWorkflow`** — **0029** §5's retention sweep, hosted there because that is the
>   controller-pinned workflow host, with its one activity proxied onto `DATASET_QUEUE`.
>
> So the queue is not "the Pulumi queue". It is the controller-pinned queue, and provisioning was its
> first tenant, not its purpose.

This ADR adds nothing to that carve-out and subtracts nothing from it. It adds one clarification,
because finding 3 above invites the wrong inference: **the fleet SSH key is not the cloud
credential.** `tmuxSessionWorkflow` needs `KONTRA_SSH_KEY` only to reach a fleet **Machine**, and
`panels/ssh.ts` is one of three transports beside `panels/docker.ts` and `panels/local.ts`. The
appliance's case for that workflow is the local pane, which uses neither. So the queue survives
carrying neither secret, and nobody needs to re-add the SSH key to make Monitor work.

### 3. `fleet` becomes provider-named. The bare word is reserved and may never name a cloud

Per-provider classes — `doFleet`, `awsFleet` — one per provider, each carrying that provider's own
configuration. The provider appears in the class name and nowhere else.

**`fleet` on its own is RESERVED for the local Docker case.** Reserved is the operative word: this
ADR does not build a local-Docker fleet and does not define what `fleet.up()` returns on an
appliance. What it settles is that the bare name is **not available to a provider** — not to
DigitalOcean, which has it today, and not to whichever cloud is second. A reader who finds `fleet`
meaning "DigitalOcean" is reading pre-0034 code.

Three rules bound the shape, and each one exists because the alternative has already gone wrong here:

- **A provider noun may appear in a class name and nowhere else.** `infra/CONTEXT.md`'s rule stands
  for everything downstream: no **Machine**, no **Role**, no inventory entry, no **Dataset**, no CLI
  output and no wire field spells a cloud. What changes is the *count* of provider-coupled files —
  `programs/fleet.ts`'s "the ONLY provider-coupled file" becomes one file per provider — not the
  rule that they are the only ones.
- **The classes are not a union with a `provider:` field.** The configurations are genuinely
  different shapes, and finding 4 is the proof: a DigitalOcean `vpcUuid` is *regional*, so region and
  VPC are one fact that must be named together or defaulted together, and `sshKeyIds` are
  account-scoped DigitalOcean key ids that mean nothing anywhere else. A constructor that takes both
  halves of the region/VPC pairing can make the measured nyc1 failure unrepresentable; a bag of
  optional strings with a discriminator is the current situation with a longer type.
- **Per-provider classes map onto `stacks.ts`'s dispatch table, never onto caller-supplied
  programs.** That file's opening comment is a security property, not an organising preference:
  "Keeping this a table rather than letting callers pass arbitrary programs is deliberate: an HTTP
  route decides *which* stack to act on, never *what* the stack contains. A caller that could supply
  the program could provision anything the credential allows." A second provider adds a second
  project to the table and a second `coerce*Args`. It does not add a way to pass a program.

### 4. The credential is a **named secret**, resolved at the last hop, and fleet config is code

This is the change. Today the token is an environment variable read in one process; it becomes a
**name** that workflow code carries and the infra worker resolves at the point of use, from the
secret store built in issue 19. Fleet configuration — region, size, count, credential name — is
expressed in code, per provider, on the class named in §3.

**The invariant does not weaken, and it is stated as five negatives because that is how it is
tested:** the credential's *value* is never a workflow argument, never an activity argument, never
part of a **Batch**, never in Pulumi stack config or state, and never on a **Machine**. Only the
*name* crosses any of those boundaries, and a name is not a secret.

Finding 6 is why the first three negatives are load-bearing rather than tidy. The codec is a
claim-check with a 128 KiB default; a seventy-byte token is nearly two thousand times under it, so a
credential placed in `StackWorkflowInput.args` — an ordinary `Record<string, unknown>` — rides inline
in workflow history in the clear for the namespace's whole retention. There is no size at which a
credential is safely a workflow argument, because the mechanism that would protect a large one is
offload, not encryption.

**What changes is the kind of guarantee, and this is the point of the decision.** Today "the cloud
token lives only in `orchestrator-infra`" is true because of *where a process runs* — an accident of
topology that nothing enforces and that any future refactor could quietly break by reading the same
env var somewhere else. After this it is true because of *what the type carries*: a `credential`
field documented and typed as a name has no room for a value, and issue 24's required test — inspect
workflow history for a sentinel value and find nothing — is the regression guard that keeps it so.
`CONTEXT-MAP.md`'s sentence therefore becomes a **design guarantee rather than an accident of
process separation**. That sentence is still literally true today, so it is not edited here; it is
issue 24's to update, in the change that makes the new enforcement real.

Across the two deployments the guarantee now holds three ways, and they are not interchangeable:

| deployment | how the guarantee holds |
|---|---|
| appliance (**0031**) | **by absence.** No provisioner, no plugin, no token. Nothing to leak |
| compose controller, today | **by process separation.** One process reads one env var |
| compose controller, after issue 24 | **by design.** Workflow code carries a name; the infra worker resolves the value at `engineEnv()` and it exists for the length of one converge |

Three further consequences follow from the findings, and each removes a workaround:

- **Rotation without a restart is what the store buys, and finding 2 says why it is cheap.** The read
  is already per-operation; only the thing behind it is immovable. Rotate the secret, start a fleet,
  the new value is used — no `.env` edit, no service recreate.
- **`FLEET_DEFAULTS`' module-load freeze stops being necessary** (finding 5). It defends against a
  mutable process environment. Configuration that arrives *with the request* is fixed for that
  converge by Temporal's own determinism — a stronger guarantee than reading an env var once at
  startup, and one that does not cost a restart to change.
- **A missing or revoked credential must fail at the start of `fleet.up()`, naming the secret.** Not
  thirty lines into a provider error. Issue 24 makes this an acceptance criterion; it is recorded
  here because the failure mode it replaces — a workflow retrying an activity that cannot succeed —
  is the one this repo has already paid for twice, and it looks like a hung campaign (**0031** §4).

Finding 3 bounds the claim: `PULUMI_CONFIG_PASSPHRASE` and `KONTRA_SSH_KEY` are **not** moved by this
decision, and the infra worker is not secret-free afterwards. See *Open*.

### 5. `fleet.up()` stays one door, and the door does not learn which cloud it opened

Unchanged from `CONTEXT-MAP.md`, restated because §3 and §4 change the argument and could be misread
as changing the mechanism: **a Run reaches Fleet only by starting Fleet's own workflow as a child
workflow on the infra queue**, with the stack fqn as the workflow id. That id is load-bearing —
Pulumi's DIY lock has no compare-and-swap and no TTL, so Temporal's workflow-id uniqueness is what
actually serialises writers, and a Run that tries to claim a fleet somebody is already converging
fails at the start instead of corrupting it.

The per-provider classes change **what is passed**, not **how it is reached**. Whatever a
local-Docker `fleet` eventually does, it goes through the same door: a program in `stacks.ts`'s
table, started as a child workflow on the infra queue. A second path into Fleet is the thing this
rule exists to refuse, and "it is only local, it does not need a workflow" is exactly how one would
be added.

**Provider-neutrality is a statement about what crosses back, and it is unchanged.** Fleet publishes
an inventory of **Machines**, their addresses and their **Roles**; no provider noun is in it. A
caller may now *name* a provider when it asks — that is what §3's class is — and nothing downstream
of `up()` carries provider identity: no **Machine** entry, no **Worker**, no **Batch**, no
**Dataset**, and no provider SDK, plugin or credential is reachable from Execution's process at all.
Provider code runs on the infra worker and only there. The rule is precise rather than weakened: a
provider *name* may appear in a caller's source; a provider *API call* may not.

One cost of the door being a child workflow, stated because it is the first thing somebody will try:
**a Run on an appliance cannot borrow a compose controller's fleet.** A child workflow is
same-namespace, so `fleet.up()` reaches its own control plane's infra queue or nothing. The *CLI*
crosses control planes over HTTP (§1); a *Run* does not. Cross-control-plane provisioning would be a
Nexus operation rather than a child workflow, and nobody has decided to build one.

## Consequences

- **Two supported topologies with different capabilities, not one topology with a flag.** The
  appliance is the local development and single-operator path; the compose controller is the one that
  runs campaigns. **0031** already said compose "keeps a job with a name" — this ADR is that job,
  written down, so the next person to find Pulumi in the tree and no Pulumi in the binary reads a
  decision instead of a gap.
- **`fleet` stops being a word with one meaning, and the reader test is the class name.** A sentence
  about "the fleet" is now ambiguous unless it says which; a `doFleet` is a DigitalOcean fleet, a
  bare `fleet` is the local Docker case, and neither reading is available for the other.
- **The credential invariant gets narrower and stronger at once.** Narrower because the value exists
  for the length of one converge rather than the life of a container; stronger because it is enforced
  by a type and a test rather than by which process happens to hold an env var.
- **The infra worker is not secret-free, and saying so here prevents a false claim later.** Two
  secrets remain in its environment after issue 24 lands (finding 3).
- **`programs/fleet.ts`'s "only provider-coupled file" claim becomes "one per provider".** The shape
  survives; the count does not. `stacks.ts`'s refusal of caller-supplied programs must survive the
  addition unchanged — it is what bounds what a caller can provision with the credential.
- **`infra/CONTEXT.md` gains one deliberate exception to "provider nouns stay out of the
  language".** The exception is exactly one class name per provider, and everything downstream of
  `up()` is as provider-neutral as it was.
- **Nothing about the fleet's identity changes.** A fleet is still `<actor>-<version>`, derived from
  what it places; the **Machine tag** is still the only part a caller names; placement rules — one
  Worker per **Machine**, never on the **Controller**, unique egress address — are still properties
  of the program rather than of a type. This ADR renames and re-homes; it does not re-model.

## Considered and rejected

- **A `--cloud` flag on `kontra up`.** Rejected on §1's four costs, of which the state directory is
  the one with no recovery. The appliance's premise is that a new user runs it on a laptop a minute
  after `curl`; a cloud credential and four provider plugins are the opposite of that premise, and
  the machines a lost state directory orphans cost money and reach hostile infrastructure.
- **One `fleet` class with a `provider:` field.** Rejected on finding 4: the configurations are not
  the same shape, and the region/VPC pairing that caused a measured outage is a DigitalOcean fact
  that a shared type cannot express as a pair. A discriminated union of every provider's optional
  fields is the current bag of strings with a longer name and worse errors.
- **Keep the credential as an environment variable and rotate by recreating the service.** Rejected:
  it is what happens today, and it makes rotation a controller edit plus a restart while leaving the
  guarantee resting on process separation. Nothing stops a future refactor from reading the same env
  var in another process; a field typed as a name cannot be given a value by accident.
- **Put the credential in Pulumi stack config as an encrypted secret.** Rejected on finding 1, which
  is the file's own argument: config lands in `Pulumi.<stack>.yaml` and its encrypted form in state,
  while an env var appears in state nowhere. That would put the ciphertext of a live cloud token in
  the one directory whose job is to be durable and backed up, protected by
  `PULUMI_CONFIG_PASSPHRASE`, itself an env var in the same process — trading one secret for two.
- **Let a caller pass provider arguments or a program straight through.** Rejected on `stacks.ts`'s
  own comment: a caller that could supply the program could provision anything the credential allows.
  Per-provider classes are a fixed table of shapes, not an escape hatch.
- **Have a Run on the appliance call a compose controller's `/api/infra/*` over HTTP.** Rejected
  here: the CLI already does exactly that and keeps doing it, but a *Run* doing it would be a second
  door into Fleet that is not a child workflow, and it would need a credential that can spend money
  to be reachable from the appliance — the thing §1 and **0031** §3 both refuse. If cross-control-
  plane provisioning is ever wanted it is a Nexus operation and its own decision.
- **Rename `fleet` to something neutral and give DigitalOcean the bare word by seniority.** Rejected:
  it is the current situation defended on the grounds that it is current. The bare word is the one a
  new user types first, and it should mean the case that needs no cloud account.

## Open

- **How the class is spelled at the call site.** Two shapes are reasonable — `doFleet.up(...)` beside
  `fleet.up(...)`, or `fleet.up(doFleet(region=…, machines=4, credential="do-prod"), actor=…)` — and
  the second keeps one `up()` verb while the first reads better. Not decided here; what is decided is
  that the provider is in the name and the bare word is not a provider. Each SDK may spell the same
  name in its own convention.
- **`fleet` collides with `Terminal.mode`.** Monitor's transport enum is `fleet | docker | local`
  (**0020**, `panels/ssh.ts`, `panels/docker.ts`, `panels/local.ts`), where `fleet` means "a Machine
  reached over SSH" and `docker` means the local container case — the *opposite* assignment to §3's.
  That enum is on the wire, so renaming it is not free. One of the two must give way before both
  spellings ship, and the recommendation is that Monitor's enum is the one to rename, since it names
  a *transport* and has three good names available; recorded rather than decided.
- **Revocation while a fleet is standing.** Issue 24 requires that a revoked credential fails at the
  start of `fleet.up()`, which is right for `up` and possibly wrong for `destroy`: the teardown is the
  operation that stops the billing and the OPSEC exposure, and it needs the same credential. Whether
  revocation must keep a destroy path working — or whether the answer is that revocation is precisely
  when you want no further provider calls — is not settled. It should be settled before revocation
  ships, not after a campaign is orphaned by it.
- **The other two secrets in the infra worker** (finding 3). `PULUMI_CONFIG_PASSPHRASE` is a state
  decryption key whose loss makes existing stacks unreadable, so moving it is a different risk from
  moving a rotatable token. `KONTRA_SSH_KEY` is a file, not a string, and the store's shape for files
  is issue 19's question. Neither is in scope for issue 24, and neither should be assumed handled.
- **Which provider is second.** `awsFleet` is written throughout as the illustration, not as a
  commitment. Nothing here has been measured against a second provider, and the first real one will
  test whether §3's "no provider noun downstream" rule survives contact with a cloud whose machine
  model is not one flat VPC of long-lived instances.
