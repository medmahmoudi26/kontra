# 70. A Workspace runs as one Tenant, and an Account holds Workspaces

Date: 2026-10-10

## Status

**Accepted.** Implements PRD *the simplified platform* D4 and D8
(`.scratch/simplified-platform/PRD.md`, plan item WP-70, owner default A39). Amends **0051** §1 and
its "the install is the tenant" heading. Replaces the **Workspace** and **Tenant** entries in
`CONTEXT.md` and `control/orchestrator/src/infra/CONTEXT.md`. kontra-cloud's ADR 0001 ("one
orchestrator per Account") needs a matching ADR in that repository. Numbering: 0068 and 0069 are
left for work in flight.

## Context

Four documents defined three words in ways that cannot all hold:

| Source | Tenant | Workspace | Account |
|---|---|---|---|
| `infra/CONTEXT.md` (ADR 0036) | a Temporal namespace | "not this one": a directory | avoided |
| `CONTEXT.md` | a Temporal namespace | a directory; "two Workspaces on one Tenant see each other's every Run" | — |
| ADR 0051 | "the install is the tenant" | the isolation boundary | — |
| kontra-cloud ADR 0001 | — | — | one orchestrator process per Account; a multi-namespace orchestrator rejected |
| PRD D4 | Temporal namespace + Kubernetes namespace, one shared control plane | — | — |

ADR 0051 as built (PR #42) settled the runtime facts, and they agree with the PRD:

- workspace `bugbounty` runs in Temporal namespace `ws-bugbounty`, and nothing it starts is visible
  from `ws-scraping` (proven on a fresh install in CI, `e2e/canary-report.spec.ts`);
- its reports live in Postgres schema `kontra_ws_bugbounty`, its lake in catalog and bucket
  `ws-bugbounty` (`workspaceAddress`), and its actors' Nexus endpoints carry `--ws-bugbounty`;
- the execution cluster gives it Kubernetes namespace `ws-bugbounty` (`kubeNamespaceFor`, PR #55),
  with its own ResourceQuota, LimitRange and NetworkPolicy;
- one orchestrator serves every workspace, so the shared control plane exists.

So `CONTEXT.md`'s sentence that two Workspaces share a Tenant is false on that branch, and
kontra-cloud ADR 0001 rejects the orchestrator the branch now is. ADR 0051's "the install is the
tenant" was a statement about who signs in, which is the question this ADR gives a separate word.

## Decision

1. **A Workspace is the unit of isolation, and the name people use.** It is a set of installed
   actors and workflows (ADR 0049) and everything they produce: Runs, Datasets, reports, logs,
   Placements. Each Workspace has exactly one name, and every per-workspace store is addressed by it
   (0051 §3). The console, the CLI and the API say *workspace*.

2. **A Tenant is the boundary a Workspace runs inside, named from the execution side.** It is the
   pair Temporal namespace `ws-<name>` plus Kubernetes namespace `ws-<name>`, with the
   ResourceQuota, LimitRange and NetworkPolicy that come with the second, and the Workspace's own
   lake bucket. **One Workspace, one Tenant, always.** The word survives because the Fleet context
   (`infra/CONTEXT.md`) needs a name for a namespace pair that does not mention code, and because
   the PRD and the Kubernetes manifests use it. When a sentence needs both words, it is crossing the
   Execution–Fleet boundary (CONTEXT-MAP.md), and that is allowed. `KontraTenant` holds the Temporal
   namespace, as it already does.

   The legacy namespace (`KONTRA_NAMESPACE`, `default` when unset) is the Tenant of an install that
   has not created a workspace. Its Kubernetes namespace is `kontra-<namespace>`, because `default`
   is a reserved Kubernetes namespace.

3. **An Account is the party that signs in and pays, and it holds one or more Workspaces.** An
   Account is never a boundary at runtime: nothing in Temporal, Kubernetes or S3 is addressed by it,
   and two Workspaces of one Account are as separate as two Workspaces of two Accounts. Membership
   and role are per Workspace. With OIDC (WP-74), a user's groups name them:
   `kontra:<workspace>:<role>`. A session carries the workspaces it may use, and a request names one
   (WP-71). Metering is collected per Workspace and summed per Account (ADR 0071).

4. **The laptop tier is one implicit Account.** The built-in admin login is its only user and is a
   member of every workspace on the install. That is what ADR 0051 meant by "the install is the
   tenant", and it stays true for that tier only.

5. **One shared control plane is the default; a dedicated plane is an Account with a control plane
   of its own.** It is the same software with one Account on it, which makes it an operations choice
   rather than a second architecture. This supersedes kontra-cloud ADR 0001's one-orchestrator-per-
   Account rule; that repository records it in its own ADR.

6. **Deleting a Workspace deletes its Tenant.** It is a workflow with a 30-day export window and a
   two-step confirm then destroy (owner default A41, WP-81). Removing an Account removes its
   Workspaces that way, one at a time.

## Consequences

- *Separation is a second Workspace.* `CONTEXT.md`'s old advice ("a second Tenant, never a second
  Workspace") is now the same advice, because the two are one-to-one.
- The words to avoid shrink: *tenant* is fine in Fleet code and in the PRD's sense, *customer*,
  *org* and *project* remain avoided (an Account is the commercial word, and only one exists).
- Things still addressed install-wide, and why: the console login and user table (they belong to the
  Account, and the laptop tier has one), the audit log (it records the install's own actions), the
  image registry (images are addressed by digest and signed; ADR 0066), and the fleet profiles in
  `kontra.yaml` (nodes are shared, namespaces are not).
- Under PR #42, the CAS (one bucket, `cas/<sha>`, readable by anyone holding a ref), the
  materialization ledger and some SQL stores are still shared, with isolation by filter rather than
  address, and neither Temporal nor SeaweedFS yet checks a credential against the namespace. Those
  are listed in ADR 0051's addendum and are work (WP-06, WP-51, WP-52, WP-75), not a disagreement
  with this vocabulary.
