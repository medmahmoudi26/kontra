# 66. Kubernetes is the execution plane, and a fleet is a profile

Date: 2026-10-09

## Status

**Accepted for implementation, in slices.** Implements PRD *the simplified platform* D3 and §7
(`.scratch/simplified-platform/PRD.md`). Supersedes, once its last slice lands: **0036**'s driver
clauses, **0037** (the Warden and the fleet as capacity), the "Kubernetes is out of scope" lines in
**0047** and **0052**, and **0034** §3 (provider classes in code). Amends **0064**: a worker's
identity is a projected service-account token. Numbering: 0064 is the owner's uncommitted ADR, and
0065 is D2's.

## Context

Today a fleet is a hand-built scheduler: the Warden, its enrolment CA, its egress code, two Pulumi
fleet programs (`programs/fleet.ts` for DigitalOcean droplets, `programs/dockerFleet.ts` for local
Docker) and `machine.ts`, which installs native systemd units over SSH. About 9,300 lines of source
and 9,500 of tests isolate untrusted actor code with code kontra itself maintains. The PRD's goal 4
says isolation should come from components maintained by others, and goal 3 that local and cloud
use the same mechanics.

## Decision

1. **Every fleet is a k3s cluster.** A **fleet provider is a Pulumi program** that outputs a
   kubeconfig (a Pulumi secret) and a node inventory: `local` (Lima VMs on the control-plane host,
   k3s inside), `digital_ocean` (droplets in a VPC, k3s on them), `byo_kubeconfig` (an existing
   cluster; no nodes created, never destroyed). Adding a cloud is adding a program.
2. **A fleet is a profile** in the install's `kontra.yaml` under `fleets:` — a map of named
   provider inputs with a `default`. `kontra.yaml` is per-install and gitignored like `.env`;
   `kontra.example.yaml` ships with dummy values. **Secrets live in it** (the PRD's choice): the
   infra role reads a profile only at converge time, inside the activity, and a secret never enters
   history, activity arguments or a Batch — ADR 0034's rule, kept, with the file in place of the
   secret store for fleet credentials. `shared/conformance/fleets.json` pins the schema, defaults
   and refusals for the orchestrator's reader and the CLI's.
3. **A placement is a Deployment**, one per (holding scope, actor@version), in the tenant's
   Kubernetes namespace — named like the tenant's Temporal namespace (ADR 0051), with a
   ResourceQuota. The pod runs the actor image **pinned by digest**, under
   **`runtimeClassName: gvisor`**, as non-root with every capability dropped, with no
   automounted service-account token and one **projected token** (audience `kontra`) as its only
   credential. Resources come from `actor.json`'s `resources`; the drain from `worker.yaml`'s
   `graceful_shutdown_timeout` (ADR 0065). Scope exit deletes the Deployment.
4. **NetworkPolicy** per tenant namespace: ingress denied; egress allowed to kube-dns, the
   control-plane endpoints a worker needs (Temporal, S3, the orchestrator, zot, the log and metric
   ingest) and the public internet minus RFC 1918, CGNAT, link-local and the metadata address; the
   control-plane Postgres explicitly unreachable.
5. **Kyverno verifies image signatures at admission** for the install's registry. `kontra deploy`
   signs the pushed digest with cosign. Default: a key pair per install, generated on first deploy,
   its public half in the cluster policy — keyless signing needs an OIDC identity a laptop does not
   have.
6. **KEDA scales a placement on its Temporal task-queue backlog.** `place(replicas=N)` is the
   maximum; the minimum is 1 while the placement exists, so `ready()` is meaningful and a pinned
   Session is not scaled out from under itself.
7. **The Lease holds a node pool.** One Lease per profile; when the last hold drops, the nodes are
   destroyed after `idle_minutes` unless a new hold arrives (the existing Lease workflow, plus the
   grace).
8. **The control plane and the cluster are separate.** The dev box only ever runs the control
   plane; `local`'s nodes are VMs. Workers dial out to the control plane; nothing dials in.
9. **Deleted once the `local` provider passes the PRD §10.3/§10.4 tests in CI:** the Warden, the
   enrolment CA, the egress code, `programs/fleet.ts`, `programs/dockerFleet.ts`, `machine.ts`,
   the Python provider classes in `kontra.fleet`, and `kontra fleet deploy`. The Warden's process
   and dev drivers, which `kontra serve` uses on the control plane for development, are moved,
   not deleted.

## Slices

1. This ADR, `kontra.example.yaml`, and the two `fleets:` readers with their corpus.
2. The manifest builders (namespace, quota, RuntimeClass, NetworkPolicy, Deployment, ScaledObject,
   Kyverno policy) as pure functions with tests, and a Kubernetes client for apply/delete/status.
3. The provider registry, `byo_kubeconfig`, and the cluster bootstrap component.
4. The `local` provider, and a CI job that runs the canary on it with gVisor.
5. The SDK: `fleet.hold(profile=, size=, nodes=)`, `place(replicas=)`, `ready()`; the Lease grace.
6. §10.3 and §10.4 as CI tests on `local` with three nodes.
7. Workload identity: the orchestrator, Temporal and S3 accept the projected token.
8. The deletions in decision 9.
9. `digital_ocean` on k3s (verified against real droplets only with the owner's go-ahead on spend).

## Defaults chosen here, for the owner to confirm

- **Lima**, not Multipass, for `local`: no snap dependency on Linux, scriptable, QEMU/KVM.
- The CLI verb is **`kontra fleet up [--profile p] [--nodes N]`** (§7); §10.1's `kontra fleet
  local --nodes 1` is read as that command with the default profile.
- One Deployment per holding scope, not shared across runs.
- A per-install cosign key pair (decision 5).
- The PRD's "KEDA-scaled workflow host" in the control plane is read as future work: the control
  plane is compose, and KEDA runs in the execution cluster.
