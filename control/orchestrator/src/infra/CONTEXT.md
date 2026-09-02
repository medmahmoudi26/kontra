# Fleet

How kontra brings into existence, repairs, and destroys the machines that
[Execution](../CONTEXT.md) dispatches to, and how an actor's code gets onto them. This is the
language of `infra/` and the fleet half of the CLI.

A **Run** may now ask for a Fleet — `actorkit.fleet.hold()` claims capacity and `f.place()` says what
runs on it, both starting `stackWorkflow` as a child workflow on the infra queue; the scope's exit
drops the **Lease**. `actorkit.fleet.up()` is SUGAR for the pair with the placement staged before the
scope opens, so it still costs one converge and still names its **Fleet** `<actor>-<version>`. That
is the *only* door — one door with two spellings — and it does not merge
the two languages: the request crosses in Fleet's vocabulary, on Fleet's worker, and the cloud
credential never leaves `orchestrator-infra` — ADR 0034 makes it a NAMED secret resolved there at
the point of use, so workflow code carries the name and never the value. What crosses BACK is
provider-neutral: **Machines**, their addresses and their **Roles**, never a cloud. A caller may
NAME a provider when it asks (ADR 0034 §3); nothing downstream of `up()` carries provider identity,
and no provider API is reachable from Execution.

## Language

### Machines

**Machine**:
One cloud instance the **Fleet** owns, named without reference to any provider.
_Avoid_: droplet, node, box, VM, instance

**Fleet**:
Tagged capacity: a set of **Machines**, each running a **Warden**, that **Workers** are scheduled
onto. A **Fleet** is NOT owned by one **Run** and is not named after what it runs — several **Runs**
may hold it at once, and it outlives any one of them.
_Avoid_: cluster, pool, swarm

**Lease**:
One **Run's** claim on a **Fleet**, held for as long as its scope is open. **Machines** are
destroyed when the last **Lease** drops, and a **Lease** expires on a clock so that a **Fleet**
outlives neither its holders nor a control plane that died holding it.

THE CLOCK IS A CHECK, NOT A DEADLINE, and the distinction is what stops the mechanism becoming the
failure it prevents. At a **Lease**'s expiry the **Lease** workflow asks Temporal whether its holder is still
RUNNING and renews the ones that are — so the interval bounds how long a **Fleet** survives a holder
nobody can account for, and is not a limit on how long a **Run** may take. A holder pays nothing to
stay held: no heartbeat, no renewal, no events at all between hold and drop. An UNATTRIBUTED
**Lease** — one naming no holder, which is what adopting a **Fleet** for a handoff takes — cannot be
asked about and therefore lives exactly one interval.

A **Fleet** an operator brought up by hand holds NO **Lease** at all, deliberately: `kontra fleet up`
is not a **Run**, so what it creates stands until a hand ends it. That is the one **Fleet** nothing
collects on its own, which is why `kontra fleet leases` says so in words rather than printing an
empty table.
_Avoid_: reservation, hold, claim (a claim-check is a **Ref**, and the two must not share a word),
**ledger** — the workflow holding a **Fleet**'s **Leases** is the **Lease** workflow, and that is
the whole of its name. The word was in this repo's prose for one day and named THREE unrelated
things: the materialization record ADR 0017 owns (`runStats.ts`'s `StatSource`, and the one meaning
that KEEPS it), a credential `ResolutionLog`, and this. A word with three referents and no entry in
any glossary is exactly the shape **campaign** had, and it got in on the DAY campaign went out,
because a purge done by hand left no guard behind it and nothing sweeps new prose against this file.
(The sweep that retired it then ate this very line, replacing the word inside its own retirement
notice — which is what an _Avoid_ entry is for, and why it is written back by hand.)
**campaign** — RETIRED 2026-08-30, and this line is the whole remains of it. It meant "one bounded
period of work a **Fleet** is created for and destroyed after", which described an infrastructure
that no longer exists: a **Fleet** is capacity now, created for nobody and destroyed when the last
**Lease** drops. Both halves of the old word already had owners — the work is a **Run**, the claim
is a **Lease** — so it named nothing of its own, which is why it goes rather than narrows.

**Role**:
Which **Machines** an actor lands on, carried as a **Machine tag** and an inventory group.
_Avoid_: type, class, flavour, environment

**Placement**:
One **Artifact**'s DESIRED STATE on a **Fleet** — which **Actor**, which version, how many
**Sessions** per **Worker**, and how many of the **Fleet**'s **Machines** it lands on.
`f.place(...)` states it and stating it again is the SCALE operation: there is no `scale()` verb,
because a second `place()` with a different density is the same declarative call with a different
desired state.

A **Placement** PUTS AT MOST ONE **WORKER** ON ANY ONE **MACHINE**, and that is structural rather
than a policy. Two of one `<actor>@<version>` there would carry the same `KONTRA_WORKER` label, write
the same units and poll the same queue, so nothing could tell them apart. More concurrency for one
**Artifact** on one **Machine** is **Sessions**, which is density.

THE DESIRED STATE IS TOTAL, which is the property everything else follows from. Pulumi's desired
state carries no patches, so a converge that omits a **Placement** does not leave it alone — it
removes it, runs that **Worker**'s teardown, and stops it with nothing raising on either side.
Hence: every converge carries EVERY **Placement** the scope has made, every converge carries the
**Machine** count (absent, it coerces to zero and DESTROYS the **Fleet**), `hold()` converges only
when its **Lease** is the only one, and `place()` refuses on a shared **Fleet** — where this side
cannot see what the co-tenant placed.
_Avoid_: deploy (a **deploy** is the operator's `kontra fleet deploy`; a **Placement** is a **Run**'s
desired state), assignment, schedule

**Packing**:
Several **Workers** sharing one **Machine**, and therefore its egress address (ADR 0037). It is
REQUESTED BY PLACING A SECOND **ARTIFACT** — there is no `pack=` argument, because `place()` is it —
and it is the reason a **Fleet** is capacity rather than one **Artifact**'s provisioning.

`spread=True` is the property packing takes away, made sayable: one **Worker** of THAT **Placement**
per **Machine**, on every **Machine**. It is `subfinder`'s operating rule — one worker per droplet,
concurrency 1, and a source address it does not share with another `subfinder` — which was an
invisible invariant while a **Machine** held one **Worker** and is a request now that it does not.

IT DOES NOT RESERVE A **MACHINE**, and the difference matters. ADR 0037's own example packs
`subfinder` beside `nscheck` on one four-**Machine** **Fleet**, so the address IS shared with the
co-placed **Worker**; what `spread=True` guarantees is only that no two **Workers** of one
**Placement** share one. A **Worker** that must have a **Machine** to itself needs a **Fleet** of its
own — a different **Machine tag**.
_Avoid_: bin-packing (names an optimiser nothing here runs — which **Machine** a **Placement** lands
on is a prefix, not a search), density (that is **Sessions**, inside one **Worker**), co-location

**Machine tag**:
The LABEL a caller puts on a **Fleet**'s **Machines** — it becomes the cloud provider's tag
(`kontra-<tag>`), the inventory group, and the `kf-<tag>-NN` **Machine** name prefix. Nothing
dispatches on it. It is the ONLY part of a **Fleet**'s identity a caller names, which is why
`fleet["scanners"]` (the subscript form of `fleet.up(..., tag="scanners")`) names THIS and not the
stack.

SINCE ADR 0037 IT IS ALSO A **FLEET**'S NAME WHEN THE **FLEET** IS CAPACITY. `fleet.hold(tag="dns")`
is the stack `kontra-fleet/dns`, whose **Machines** are `kf-dns-01` — one name for one thing, and
two **Runs** asking for `dns` capacity collide on purpose, which is what a **Lease** is for. That is
not the deleted caller-invented name coming back: that one named "one bounded period of work", a
period nothing measured, and it collided when two scopes shared a role and did not collide when two
scopes wanted the same **Machines**. A **Machine tag** is a label that is already on the
infrastructure and already the operator's own word for it.

`fleet.up()` keeps the OTHER rule — `<actor>-<version>`, DERIVED from the **Artifact** it places —
because every stack this repo has created is reachable under it and `kontra fleet down nscheck-0.1.0`
still has to work. Two ways to start a **Fleet** exist for ever (ADR 0037); two ways to name one is
the part of that tax that is paid here. Defaults to the actor's name, which is what almost every
**Fleet** wants.
_Avoid_: campaign, bare "tag" (a **tag** in Execution is a **Dataset**'s keep-me label — a different
idea in a different context, and the two must not be spelled the same in one sentence)

**Controller**:
The single long-lived **Machine** holding the control plane and the only cloud credential.
_Avoid_: main-droplet, master, head node

**Warden**:
The one process kontra installs on a **Machine**. It reconciles which **Workers** run there
against what the control plane asked for, judges their health, enforces the **Machine's** **Egress
policy**, and streams the **Machine's** panes. It runs no actor code itself, and it enforces the
**Machine's** **Trust policy** before it runs anyone else's.
_Avoid_: agent (already banned on **Worker**), supervisor, daemon, node agent, kubelet

**Egress policy**:
Where a **Machine's** **Workers** may reach on the network. It belongs to the **Machine**, not to a
**Worker** — packed **Workers** share a **Machine's** egress address (ADR 0037), and a **Machine**
belongs to one **Tenant** — and it is enforced by the **Warden** as an nftables ruleset in the
**Machine's** OWN network namespace, which is the one place a **Worker** has no way to reach:
nftables state is per network namespace and a container is in its pod's, so no message it can send
names these rules. Default-deny to RFC1918 and a floor (the cloud metadata service) that no
assignment may allow.
_Avoid_: firewall (names the whole host's rules, and ufw's are a different set that Docker already
bypasses on this Fleet); allowlist/blocklist alone (each names one half of a policy that has four
precedence levels); "network policy" (a Kubernetes term for a pod-to-pod rule, which this is not)

**Tenant**:
One party whose work is separated from every other party's. **A Tenant IS a Temporal namespace**
(ADR 0036) — not a name in front of one, and not a prefix on a queue: in Temporal the namespace is
the only authorisation boundary there is, so anything else is a convention rather than a boundary.
A **Warden**'s enrolment mints a certificate scoped to exactly one, chosen when its one-time token
was minted and never taken from anything the **Machine** says.
_Avoid_: customer, org, account, workspace, project (each names a billing or UI grouping somewhere
and would invite a **Tenant** that spans two namespaces, which is a **Tenant** that is not one);
"namespace prefix" and "queue prefix" (both describe the thing this word replaced — queue names
are namespace-RELATIVE and `shared/conformance/queues.json` pins them unchanged)

### Getting an actor onto them

**Target**:
What an actor is given to run in — a container, or a **Machine** of its own.
_Avoid_: env, environment, platform, placement (that verb belongs to putting an Artifact ON a
Target — "the fleet places the Bundle" — so it must not also name where an actor runs)

**Artifact**:
The immutable, content-pinned thing a build produces: an **Image** for a container **Target**,
a **Bundle** for a machine **Target**. Both kinds live in an OCI **registry**, pushed and mirrored
by the same mechanism (ADR 0036); what differs is what is inside them, not how they travel.
_Avoid_: build, package, release

**Bundle**:
The **Artifact** for a machine **Target** — an actor's code plus the script that installs it,
identified by the hash of its contents. Shipped as an OCI artifact: one layer (the tar.gz), one
config blob (which engine the **Machine** runs), one tag per version.
_Avoid_: tarball, payload, archive, object (it stopped being one in the object store)

**Trust policy**:
Which registries a **Machine** will pull an **Artifact** from, and whose signature it accepts before
one runs. Two gates and they are not interchangeable: the allowlist is a question about a STRING and
is answered before anything is contacted; the signature is a question about BYTES and necessarily
costs a request. It is enforced by the **Warden**, at the pull, and it is LOCAL to the **Machine** —
so it bounds a substituted registry and a control plane that asks for the wrong image, and bounds
nothing about a **Machine** that is already compromised.
_Avoid_: verification (it names only half), trusted image (an image is not trusted, a signer is),
security policy (egress policy is also one and they are enforced in different places)

## Relationships

- A **Fleet** holds many **Machines**; a **Machine** runs one **Warden** and many **Workers**
- A **Fleet** is held by zero or more **Leases**; at zero, its **Machines** are destroyed
- A **Role** names which **Machines** an actor lands on; a **Target** names what it runs in.
  They are independent — but the combinations in use are now exactly two: a container **Target**
  on the **Controller**, and a machine **Target** on fleet **Machines**
- Building produces an **Artifact**; deploying places an **Artifact**. One verb never does both
- Every **Artifact** is content-pinned, because "what is actually running" must be answerable
  from the deploy command alone
- Every **Machine** runs one **Warden**; the **Warden** runs every **Worker** on it
- Every **Machine** belongs to exactly one **Tenant**, and a **Tenant** is one Temporal namespace.
  A **Fleet** is capacity and a **Tenant** is a boundary, so they are independent axes: two
  **Tenants** may hold **Machines** tagged the same way and never meet, and one **Tenant**'s
  **Machines** may span many **Fleets**. Two **Tenants** shipping an actor of the same name and
  version land on the SAME task queue name — that is deliberate and unchanged — and do not meet,
  because the queue is a name inside a namespace and the namespace is the boundary
- Cloud authority — creating and destroying **Machines** — belongs to the **Controller** alone,
  because it is the only authority requiring a credential, and fleet **Machines** reach hostile
  third-party infrastructure

## Example dialogue

> **Dev:** "`kontra build --push ghcr.io/acme/bundles/nscheck` — so the **Artifact** lives in *our*
> registry now, and kontra just knows where it is?"
>
> **Domain expert:** "kontra keeps the *meaning* — which digest that version currently names — and
> never the bytes. That is the whole of what `--push` changed: there is no kontra registry to be
> inside of, so an airgap is a registry mirror and a CI job is three commands with your own
> credential. Two things it will refuse, though. A reference with no host is Docker Hub, whose
> hundred manifest reads an hour are per *address*, not per account — one **Fleet** takes that away
> from every other build on the box. And a reference carrying a digest, because the digest is what
> the push is about to compute."
>
> **Dev:** "And a placement finds it wherever I pushed it?"
>
> **Domain expert:** "Not yet, and the command says so rather than letting you find out. A
> placement derives `bundles/<actor>` at the **Fleet**'s own registry from the actor and the
> version alone. Push somewhere else and the **Artifact** is real, mirrorable and unplaceable until
> it is copied there."

## Flagged ambiguities

- **"deploy" meant two things.** `kontra deploy` built and pushed an image while
  `kontra fleet deploy` started one — build-time and run-time under one verb. Resolved
  2026-08-10: **build** produces an **Artifact**, **deploy** places one. The split is what makes
  a machine **Target** expressible at all, since it has an **Artifact** but no registry.

- **"worker" meant three things** — a machine, a container, and a provisioning resource.
  Resolved 2026-08-10: **Machine** names the hardware and lives here; a **Worker** is a
  *process* and stays Execution's word. A **Machine** may host several **Workers**; on the
  fleet it hosts exactly one, because each needs its own egress address.

- **Provider nouns stay out of the language, with exactly one exception: a class name.**
  "Droplet" names a DigitalOcean product; the domain word is **Machine**, and no **Machine**,
  **Role**, inventory entry, CLI output or wire field may spell a cloud. What ADR 0034 changed is
  the COUNT and the exception: there is one provider-coupled file per provider rather than one in
  total, and the fleet class a caller names carries the provider — `doFleet`, `awsFleet`. The bare
  word `fleet` is RESERVED for the local Docker case and may never name a cloud, so a sentence
  about "the fleet" now needs the class name to be unambiguous.

- **A container on a fleet Machine was a Target nobody chose.** It was inherited from the days
  when the only way to ship code was an image. Resolved 2026-08-10: fleet **Machines** run
  actors natively from a **Bundle**; the container **Target** survives only where something
  else already provides the isolation — the **Controller**, where several Workers share a host.
  **ADR 0036 reverses this**, on a fact that was not available in August: kontra will run code it
  did not write, and a **Machine** is a boundary between *Workers*, never one between *authors*.
  The reversal is being landed in slices; what has landed here is the **Artifact** half — a
  **Bundle** is an OCI artifact and the `kontra-bundles/` object-store prefix is gone. A
  **Machine** still runs it natively, which ADR 0036 keeps as the `process` DRIVER rather than as
  a **Target**.

- **An Actor's name and its Artifact's name obey different grammars, and the wider one is the
  Actor's.** `shared/conformance/queues.json` records that a queue name is NOT sanitised — `my actor` and
  `café` reach Temporal verbatim, and `a/b` is carried there as an adversarial case. An OCI
  repository name is lowercase alphanumerics with `.`/`_`/`-` separators in `/`-joined components,
  so the set of nameable **Actors** is strictly larger than the set of nameable **Artifacts**.
  `a/b` publishes (to `bundles/a/b`, unambiguous because a tag is delimited by `:`, not by a slash
  count); `my actor` does not, and `kontra build` refuses it naming the actor. This is not new with
  ADR 0036 — the old object key had the same names in it — but it used to be a URL a store would
  quietly mangle and is now a refusal before any **Machine** exists.

  A **VERSION** IS SUBJECT TO THE SAME GAP, which slice 07 found by measuring:
  `shared/conformance/queues.json` carries `1:2` as an adversarial version and Temporal takes it verbatim,
  while an OCI tag may not hold a colon — `<registry>/bundles/nscheck:1:2` reads back as repository
  `bundles/nscheck:1`. So the set of publishable **Artifacts** is bounded by the version as well as
  by the name.

  The consequence is larger than a build refusal, and it lands in ADR 0036's `process` driver:
  `kontra serve --actor <dir>` runs from source and produces no **Artifact**, so a `café` actor
  serves, polls, and is listed and reconciled forever on the box it was started on. There is
  therefore a class of **Worker** that runs and can never be PLACED — verified against live
  Temporal, both halves whole. What bounds a name is not "can Temporal address it" but "can an
  **Artifact** be named after it".

  THREE SITES NAME AN **ARTIFACT**, AND THEY NOW GIVE ONE ANSWER. Resolved 2026-08-30 with slice 07,
  which added the third (`kontra build --push`) and could not add a third bespoke message: the
  grammar, both refusal sentinels and the sentence itself live in `cli/internal/ociref/ociref.go`, consulted by
  `kontra build`, by `cli/scale.go`'s pull diagnosis and by the podman driver.
  `shared/conformance/ociref.json` drives all three over the same rows, so breaking the grammar in one
  place turns all three red — which is what makes "one shared answer" checkable rather than claimed.

  What that fixed: `cli/scale.go` gave `café` and `a/b` the same headline — "the registry answered,
  and does not hold it" — when only one of them was true. `a/b` really was never deployed and
  deploying it fixes it; `café` can never be an image, and every remedy that message offered was
  unreachable, so **an operator with a `café` actor was told to deploy, forever**. `a/b` still
  reports "never deployed", because for it that is true.

  The trap, kept here because the next site will meet it: there are TWO references in play and they
  are not the same string — the remote `<registry>/<name>:<version>` that is pulled and the local
  `kontra/<name>-worker:<version>` preferred when the daemon holds it, differing in prefix, suffix
  and tag source. A check that parses only the actor name, or only one of the two forms, moves the
  failure one step earlier without removing it. Every entry point in `ociref.go` therefore takes a
  whole reference, and each site passes the exact string its runtime is about to receive.

  AND A **TRUST POLICY** IS A FOURTH READER, added by slice 14 without becoming a fourth grammar.
  "Which registry is this?" is the same field `ociRef` already answers, and answering it again is
  worse than an inconsistency — a hand-rolled `strings.HasPrefix(ref, "ghcr.io")` admits
  `ghcr.io.evil.example/x` and a `strings.Split(ref, "/")[0]` admits `a/b` as though `a` were a
  registry, and both look right. So `cli/internal/trustpolicy/trustpolicy.go` matches on `ociRef.Domain` and
  `ociRef.Path` and judges an allowlist ENTRY with the same three rules the grammar is built from.
  `shared/conformance/ociref.json`'s `allow` section is the coupling made checkable: widening
  `ociPathComponent` or `looksLikeRegistryHost` turns grammar and policy red in the same run.

- **"Where a Bundle lives" was assembled, never spelled.** The old object key
  `kontra-bundles/<actor>/<version>/<sha>.tar.gz` was built out of separate arguments in Go and
  rebuilt out of separate arguments in TypeScript, so no sweep could find it and a drift showed up
  as a resolver that 404s on a Bundle that published perfectly well. Resolved 2026-08-30:
  `shared/conformance/bundleref.json` pins the OCI address on both sides.

## Retired terms

- **Watchdog** — *"The on-machine loop that judges a single Machine's health from its own logs."*
  Retired 2026-08-30. It was a systemd timer running `watchdog.sh` beside three other units; the
  **Warden** absorbed the duty. Judging health is now one of the **Warden's** jobs rather than a
  process of its own, because the thing that decides a **Worker** is sick should be the thing that
  can restart it.

## Not yet settled

The repair model. Whether a sick **Machine** is repaired in place or destroyed and replaced
changes what the **Warden** is allowed to do, and the term for it should be coined once that is
chosen, not before.

Where a **Trust policy** comes from. It is currently the **Warden's** own flags, baked into its
systemd unit at enrolment — so the **Machine** holds the policy it is judged by, and root on the
**Machine** rewrites it. The policy that a **Tenant** would want is one delivered IN the enrolment
credential, signed by the Fleet CA alongside the namespace: slice 08 mints such a credential and it
does not carry this. Naming the delivered thing before it exists would put a word on a design
decision nobody has made.
