# 36. One Target: actors run in containers, the Bundle becomes an OCI artifact, and a tenant is a namespace

## Status

**Accepted.** Supersedes **exactly one clause** of **0032** — the one that ADR called the clause
"that matters most": *fleet **Machines** run actor code natively, with no container runtime*, itself
carried forward from `legacy/0019`. Every other clause of 0032 survives and is load-bearing here,
above all *"one identity field, one shape, compared as an opaque string"* — `KONTRA_ACTOR_DIGEST`
is what makes this reversal cheap.

Also reverses the resolution recorded in `infra/CONTEXT.md`'s flagged ambiguities on 2026-08-10,
and rewrites `infra/README.md`'s "Two Artifacts, two Targets" table.

Companion to **0037**, which decides what a **Fleet** is once Machines can run anything. That ADR
depends on this one; this one does not depend on it.

## Context

**The new fact is that kontra will run code it did not write.** Nothing below is a discovery about
the code — the code is fine. It is that every one of these decisions was correct for a
single-tenant system and stops being correct for a multi-tenant one.

Seven things were established against this checkout before deciding anything.

1. **The native-execution decision was argued from isolation, not from taste.**
   `infra/README.md`: *"A fleet Machine runs the actor **natively** — no Docker, no image, no
   registry. The Machine is already the isolation boundary (one Worker per Machine, for its unique
   egress address)."* `infra/CONTEXT.md` states the same premise as the reason the container
   **Target** was retired from Machines: *"the container **Target** survives only where something
   else already provides the isolation — the **Controller**, where several Workers share a host."*

   That premise is true exactly while every actor is first-party. A **Machine** is a boundary
   between *Workers*; it was never a boundary between *authors*. Once an actor is a stranger's
   code, the thing sharing a kernel with it is kontra's own handler, its Temporal credential and
   its object-store keys.

2. **0032 already built the seam this reversal needs.** It decided one identity field carrying
   `sha256:<hex>` on both **Targets** — the OCI manifest digest for an **Image**, the **Bundle**'s
   own sha256 for a **Bundle** — and stated that *"nothing parses it, nothing branches on which
   **Target** produced it."* Collapsing to one **Target** therefore disturbs no identity, no
   registration and no conformance arm.

3. **A **Bundle** is a tarball in the object store with a hand-built path.** `kontra build
   --target machine` uploads to `kontra-bundles/<actor>/<version>/<sha>.tar.gz`; a run of this
   checkout produced a 65.7 MiB one and printed the URL Machines fetch it from. The appliance's own
   orchestrator bundle is a second, unrelated 124 MB tarball with its own layout contract, which
   `install-appliance.sh` reads and nothing pins (see issue 21).

4. **A **Machine** runs four systemd units from that Bundle** — `kontra-actor`, `kontra-handler`,
   `kontra-vmagent`, `kontra-watchdog` (`control/orchestrator/src/infra/programs/machine.ts`). Three of the four
   exist to compensate for there being no runtime: install code, restart it when sick, ship its
   metrics.

5. **A handler is bound to one actor version, by construction.** `handler/main.go:31-32` reads
   `KONTRA_ACTOR_NAME` and `KONTRA_ACTOR_VERSION` from the environment; `:83` derives one
   `identity.SharedQueue(name, version)` and registers one worker on it. Nothing about it is
   multi-actor, and nothing about this decision makes it so.

6. **No queue name carries a tenant.** `shared/conformance/queues.json` pins five derivations —
   `shared`, `sessions`, `session`, `endpoint`, `tmux_session` — across eight modules in four
   languages, and every one is a function of `(name, version)` alone. Two customers who both ship
   an actor called `nscheck` at `0.1.0` do not merely see each other: they land on **one task
   queue**, and one tenant's **Batches** are executed by the other's **Workers**.

7. **Temporal has no per-queue authorization.** Its authorization boundary is the namespace. A
   credential that can poll one queue in a namespace can poll every queue in it.

## Decision

- **There is ONE Target, and it is a container.** Actor code runs in a container everywhere it
  runs: on fleet **Machines**, on a customer's VM, in a customer's cluster, and on the
  **Controller**. The `Target` axis in `infra/CONTEXT.md` collapses from a choice to a constant.

- **The machine Target survives as a DRIVER, not as a Target.** `process` — code executed
  directly, no runtime required — is how the single box and the airgapped install still work, and
  is what `kontra serve --actor <dir>` uses today. It is a property of the **Warden**'s
  environment, not a property of the actor. An actor author cannot tell which driver ran them,
  which is the test this reversal has to pass.

- **A Bundle becomes an OCI artifact and stops being a tarball in the object store.** Both
  **Artifact** kinds live in an OCI registry, pushed with `oras`, addressed by digest. The
  `kontra-bundles/` prefix, the presigned fetch on each Machine and the
  `<actor>/<version>/<sha>.tar.gz` convention are deleted. Airgap becomes a standard registry
  mirror rather than an object-store copy with kontra-specific tooling.

- **kontra owns no registry.** `kontra build --push <ref>` takes any OCI reference; the control
  plane stores the digest, never the bytes. `localhost:5000` survives as a convenience for the
  single-box case and becomes one configured endpoint among many. What kontra keeps is *meaning* —
  which digest a version currently names — not storage or transport.

- **A tenant is a Temporal namespace.** Queue names stay exactly as `shared/conformance/queues.json` pins
  them, because they were always namespace-relative; the corpus needs no change and no derivation
  in any of the four languages moves. A **Warden**'s enrolment mints credentials scoped to one
  namespace, so a compromised **Machine** cannot address another tenant's queues at all.

- **The handler stays one process per actor version.** `handler/main.go` is not modified. A single
  handler serving several actors would put several tenants' credentials in one process — a
  co-tenancy leak inside the component with the most authority on the box. The cost is a container
  and ~30 MB per actor, paid deliberately.

## Consequences

- **A cold Machine now pulls an image before it can work.** The last measured bring-up on this
  checkout was **3m 25s** from `fleet.up` to `f.ready()` with no pull at all; a ~900 MB actor image
  lands on top of that, on every Machine, on every campaign. If **Campaign** latency is a product
  property, this is where it got worse, and pull-through caching on the **Controller** is the first
  thing to reach for.

- **Three of a Machine's four systemd units disappear**, replaced by one **Warden** (0037).
  `kontra-vmagent` survives; `kontra-actor` and `kontra-handler` become containers the **Warden**
  starts; `kontra-watchdog` retires into it.

- **Docker is not a security boundary, and this ADR does not claim it is.** A shared kernel is a
  shared kernel. What the container buys is that kontra's own credentials are no longer in the
  actor's process; what bounds a hostile tenant is the isolation tier chosen per plan — up to and
  including one **Machine** per tenant, which this fleet already knows how to provision.

- **Egress is the exposure this does not address.** kontra's actors sweep DNS and crawl; the abuse
  case is renting the platform to scan a third party from kontra's addresses. That is bounded by
  policy on the **Machine**, outside the container, and is 0037's problem because it belongs to the
  **Warden**.

- **ONE TARGET IS NOT THE SAME AS "ANY ACTOR RUNS ANYWHERE", and the gap is a class of Worker.**
  Found by executing it, not by review: `shared/conformance/queues.json` states that a queue name is not
  sanitised and carries `a/b`, `my actor` and `café` as adversarial cases, and both were served
  against live Temporal on this checkout — polling, and visible to the driver's `list()`. But the
  OCI grammar refuses names kontra accepts. So an actor called `café` can be served, listed,
  stopped and reconciled by the `process` driver **forever**, because that path runs from source and
  never produces an **Artifact** — and can never be placed on a **Machine** by any driver that pulls
  an image.

  The set of nameable **Actors** is strictly larger than the set of nameable **Artifacts**. This ADR
  does not close that gap and does not pretend to: closing it means either refusing such names at
  `kontra actor init` — which would stop serving actors that serve today — or encoding them into a
  legal reference, which makes the digest no longer readable back to the name. It is recorded here
  so the first person to hit it finds a decision rather than a surprise.

- **The `--target` flag loses its meaning** and `kontra build` takes `--push <ref>` instead. Every
  document naming two Targets is now wrong: `infra/CONTEXT.md` (updated in this change),
  `infra/README.md`'s table, and 0032's own framing.
