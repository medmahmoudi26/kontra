# Security Model

kontra runs code it did not write, on machines it may not own. This page states what is isolated, what is not, and which boundaries are still being built.

> [!IMPORTANT]
> **kontra is `0.x`.** Several boundaries described here are load-bearing and complete; others are named honestly as gaps. Read the gaps before running untrusted actors or serving more than one tenant from one control plane.

## What a tenant is

**A tenant is a Temporal namespace.** One string, no derivation — because a `tenant → namespace` mapping would be a second thing to get right in every surface.

**The namespace is the only authorisation boundary Temporal has.** Not the task queue, not the workflow id, not a naming convention. Two tenants sharing a namespace can read each other's workflow history, and no amount of queue discipline changes that.

The namespace lives **in the Machine's certificate**, as its single URI SAN:

```
kontra:///ns/<namespace>/warden/<warden-id>
```

Five enforcement points, each a refusal: the tenant is fixed when the enrolment token is minted; signing writes the SAN from the token and never reads the CSR's; loading an identity refuses a state file that disagrees with the certificate; the assignment is fetched from `assignments/<tenant>/` with the tenant taken from the certificate; and the dial namespace comes only from the certificate.

## What isolates, today

| Boundary | Mechanism | Strength |
|---|---|---|
| Workflow history, task queues | Temporal namespace, from the certificate | **Strong** — Temporal's own boundary |
| Machine identity | mTLS, key generated on the Machine, one-time enrolment token carrying the CA fingerprint | **Strong** |
| Which image runs | digest-pinned pulls; optional cosign verification against an identity configured **on the Machine** | **Strong when signing is enforced** |
| Where a Container may connect | nftables in the host's initial network namespace, outside the container | **Strong** — the workload's netlink socket cannot address it |
| Runtime access | Docker or podman; the Warden holds the runtime socket, Workers never do | **Meaningful on a laptop, not tenant isolation** — local `dockerFleet` mounts the host Docker socket into the Warden. Documented as single-operator only. |

### On containers

Docker and podman are **not a security boundary** on their own. The driver refuses `--network=host` and never exposes the runtime socket, and `--userns=auto` means container root is not host root — all real. A determined workload sharing a kernel is a different threat model, and gVisor / Kata / microVM tiers are not implemented.

### On egress

The policy is nftables **in the host's initial network namespace**. The argument is not "the container has no tools" — that is one flag from false. It is that nftables state is per network namespace, a netlink socket only ever addresses its own, and the Container is in its pod's. Measured across five postures: the driver's own gets `EPERM` and cannot even ask.

## What does NOT isolate, today

> [!WARNING]
> **The data plane has no tenant boundary.** A Container is handed `KONTRA_S3_ENDPOINT` and `KONTRA_REDIS_HOST` **with no credentials**, because the object store and Redis accept unauthenticated access. Any actor on any Machine can read and write **every Dataset of every tenant** on that control plane.

Namespace-per-tenant isolates Temporal. It does not touch the data plane. Until that is fixed:

- **Run one control plane per tenant.** With BYOC this is free — the control plane is *their* machine, and there is no other tenant's data on it to reach.
- **Do not put a shared control plane on a network a tenant can reach.** A VPN to one client is also a path to every other client's Datasets.

Two smaller gaps in the same family:

- **The pane and telemetry ingest is gated by one Fleet-wide token**, so any holder can file a report naming any Machine.
- **Containers placed by the Pulumi path receive no namespace** and poll `default`, outside the Warden's scoping.

## The direction

In order of how much each buys:

1. **Stop giving actors a store credential at all** — hand out presigned URLs instead: per run, write-only, scoped to that run's prefix, short TTL. A compromised actor can then only write where it was told and can read nothing. This removes the capability rather than guarding it.
2. **Per-tenant, prefix-scoped credentials** where a credential is unavoidable, plus Redis ACLs.
3. **Egress policy as the backstop** — already shipped. Good defence in depth, wrong as a primary control.
4. **One control plane per tenant** — the strongest, and what BYOC gives you anyway.

## Trust roots

**The Machine's trust policy is local**, configured at enrolment and baked into the systemd unit: a key, or a workload identity and its OIDC issuer. Three configurations are refused **at load, not at use**, because each reads as safety while proving nothing — an identity with no issuer, an issuer with no identity, and a key and identity together.

What a signature proves here: the digest about to run was signed by the key or identity named in *this Machine's own configuration*. What it does not prove: that the code is good (a signed backdoor verifies), anything about Rekor or the certificate chain beyond what `cosign` itself checks, and anything against a Machine whose root already has it.

## Reporting

Please report vulnerabilities privately — see `SECURITY.md` in the repository root. Do not open a public issue for a security finding.
