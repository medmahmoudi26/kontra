# Threat model: kontra

> **Status:** first draft, 2026-09-12. Written in the shape of Windmill's
> `backend/THREAT_MODEL.md`, which is the best example of the form the author has read.
> **Needs maintainer review** — the entry points are enumerated from code and from a real incident,
> but the *acceptances* in §4 are decisions, and a decision nobody signed is not a decision.

## 1. System context

kontra runs untrusted-ish code against hostile third-party infrastructure, on capacity it provisions
and bills for. That sentence is the whole reason this document differs from a typical SaaS model.

    ┌─ CONTROLLER (one box, "the control plane") ───────────────────────────┐
    │  orchestrator-api      HTTP API + the console SPA                     │
    │  orchestrator-infra    Pulumi; the ONLY holder of the cloud credential│
    │  orchestrator-materializer   the only writer of typed DuckLake output │
    │  temporal · redis · seaweedfs · OCI registry · payload codec          │
    │  panels streamer       Terminals (ADR 0020), its own port             │
    └───────────────┬───────────────────────────────────────────────────────┘
                    │  private VPC (10.124.0.0/20)
    ┌───────────────▼─── FLEET MACHINE (n of them, disposable) ─────────────┐
    │  warden       outbound-only reconcile loop; mTLS identity             │
    │  worker       handler (Go) + actor (Python/Go) — YOUR code            │
    └───────────────┬───────────────────────────────────────────────────────┘
                    │  egress
              the open internet — scan targets, which are HOSTILE BY DESIGN

**Tenant = Temporal namespace.** It is the only authorisation boundary Temporal has, and therefore
the only one kontra can lean on. A queue name is namespace-relative; two tenants shipping
`nscheck@0.1.0` land on the same queue *name* and never meet, because the namespace is the boundary.

## 2. Assets

| asset | why it matters | sensitivity |
|---|---|---|
| **Cloud credential** (`fleet.digitalocean_token`) | creates and destroys Machines; spends money with no ceiling | **critical** |
| **Pulumi passphrase** | decrypts stack state secrets; losing it strands infrastructure, leaking it exposes every stored secret | **critical** |
| **Fleet CA + Warden identities** (ADR 0037) | mints the mTLS identity a Machine authenticates with; forging one joins the fleet | **critical** |
| **Fleet SSH key** | root on every Machine | **critical** |
| `KONTRA_STATE_TOKEN` | admits the infra routes — the ones that destroy fleets | **critical** |
| **Console password hashes** (`auth.users`, scrypt) | sign-in to the console (ADR 0045) | **critical** |
| **Secret values and slots** (`secrets/`) | third-party credentials an actor is handed at runtime | **critical** |
| **Host integrity of the Controller** | holds every item above | **critical** |
| `KONTRA_RUN_TOKEN` | serve/start/stop and a Dataset's tag/rename — can start a workflow that provisions Machines | **high** |
| `KONTRA_PANEL_TOKEN` | mints Terminal WebSocket tickets | **high** |
| **Run output / Datasets** (DuckLake, S3) | the findings — customer IP, and often the evidence of a vulnerability in someone else's system | **high** |
| **Temporal history** | arguments, results and failures of every run; routinely contains targets and sometimes secrets | **high** |
| **Actor and workflow source** (`~/.kontra/actors`, `workflows`) | the operator's own code | **high** |
| **OCI registry contents** | the Bundles Machines pull and execute | **high** |
| **Scope of record** (bbscope DB) | which targets are authorised — an error here is an unauthorised scan | **high** |
| Machine host integrity | disposable, but holds a tenant's credentials while it lives | medium |

## 3. Entry points

Numbered so code comments, issues and reviews can cite them.

| # | entry point | trust boundary | reachable assets |
|---|---|---|---|
| **EP1** | **Host services on the public interface.** Anything binding `0.0.0.0`/`*` on the Controller. | internet → host | everything |
| **EP2** | **The Run surface** — `POST /api/workflows/serve`, `/start`, `/stop`, `PUT /api/runs/:id/workflow`, Dataset tag/rename. `checkOptionalBearer` + `KONTRA_RUN_TOKEN`. | HTTP caller → arbitrary workflow from the checkout, and cloud provisioning | cloud credential (indirectly), run output, host |
| **EP3** | **The infra routes** — fleet up/down/status. `checkBearer` + `KONTRA_STATE_TOKEN`, fail-closed. | HTTP caller → Machines created/destroyed | cloud credential, money |
| **EP4** | **The query workbench** — `POST /api/datasets/query`. Operator SQL over the lake. | HTTP caller → every Dataset | run output, scope |
| **EP5** | **Console sign-in** — `POST /api/login`, scrypt against `~/.kontra/config.yaml`; session in memory, 12h idle, dies with the process (ADR 0045). | browser → a session that authenticates every `/api/` route | whatever EP2/EP4 reach |
| **EP6** | **The unauthenticated remainder of the API.** Most routes predate any auth and are open; `auth.ts` records this rather than hiding it. | HTTP caller → reads | run metadata, actor list, dataset names |
| **EP7** | **Terminals / panels streamer** (ADR 0020) — separate port, ticket-minted WebSockets. A local `--tmux` pane holds the REAL worker. | browser → a live process's stdin/stdout | host, worker |
| **EP8** | **Warden enrolment** (ADR 0037) — one-time token carrying the CA fingerprint, exchanged for a persistent mTLS identity. | new Machine → fleet membership | Fleet CA, tenant queues |
| **EP9** | **Actor code itself.** kontra runs code the operator did not necessarily write, in a container, on a Machine. | actor author → Machine, then egress | Machine, tenant credentials, downstream targets |
| **EP10** | **Actor egress to hostile targets.** The scanned system is an adversary and its responses are attacker-controlled input to the parser. | scan target → actor process | actor, Machine, run output |
| **EP11** | **The OCI registry.** Machines pull Bundles by digest and execute them. | registry contents → every Machine | fleet-wide code execution |
| **EP12** | **Secret resolution** — `secrets/store.ts`, `slotStore.ts`, `fileBackend.ts`, and the actor-side fetch. | a caller asking for a path → a secret value | secret values, downstream systems |
| **EP13** | **Temporal itself.** Plaintext by default; TLS is environment-only and unset locally. Anyone who can reach 7233 in a namespace can poll its queues. | network → task queues | run execution integrity, history |
| **EP14** | **The payload codec.** Every argument and result passes through it. | codec compromise → all run data | history, run output |
| **EP15** | **Operator config and deployment defaults** — `.env`, `~/.kontra/config.yaml`, compose files, bind addresses. | a wrong default → any of the above | everything |
| **EP16** | **Supply chain** — base images, Go/npm/PyPI dependencies, the pinned Node runtime in the appliance bundle. | build input → Controller and Machines | everything |

## 4. What is NOT defended, and the acceptance

A model that only lists controls is marketing. These are real and currently accepted.

**The VPC is flat, and Redis on it is unauthenticated.** Any Machine can reach any other Machine's
services and the Controller's Redis. The acceptance: a Fleet is single-tenant and disposable, and
the VPC is not reachable from the internet. **This does not survive a second tenant** — see
`client-controller-provisioning`. It is the first thing that must change before anyone else's
workload runs here.

**Most of the API is unauthenticated (EP6).** ADR 0045 gave the console a credential of its own; it
did not authenticate the rest. `hosted-readiness/03` is open and this is recorded in `auth.ts`
rather than inherited silently.

**Temporal runs in the clear locally (EP13).** TLS exists and is wired across all sixteen client
sites, but nothing turns it on for a local install, and there is no mutual auth between a Worker and
the cluster beyond the namespace.

**An actor is not sandboxed from its Machine (EP9).** It runs in a container, which is a boundary
between *Workers*, not between *authors*. ADR 0036 says so explicitly. A malicious actor owns the
Machine it runs on and the tenant credentials on it.

**A Dataset's tag is a delete authority with no second factor.** Untagging hands it to the retention
sweep one tick later. Gated by EP2's token, and nothing else.

## 5. Incident, 2026-09-12 — and what it says about this model

The Controller was compromised for three days. The vector was **EP1**, and specifically **not**
kontra: `xrdp` on `*:3389`, installed 2025-10-19 alongside a full desktop, enabled at boot, 190 RDP
connections in one day, zero failed logins. An intruder held `adminuser`, dropped a compute-theft
agent polling `176.65.139.211`, and drove the load average to 114.

Three things this validates, and two it indicts.

**Validated.** The binding discipline held: `weed`, `temporal`, redis and the API bind `127.0.0.1`
and the VPC address, never `eth0`. `adminuser` was **not in sudoers** — the log shows `sudo su`
refused at 22:18:16, eight hours after they got in — so EP15's critical assets stayed out of reach.
And root-owned 0600 config meant the cloud credential was not readable.

**Indicted.** `.env` was mode **0674** — world-readable — so the DigitalOcean token and Pulumi
passphrase *were* readable by any local account for that window. Treat them as exposed.
And the OCI registry (**EP11**) was bound `0.0.0.0:5000`: fleet-wide code execution, on the
internet, found only because this incident prompted a look.

**The control that would not have helped.** ufw cannot see Docker-published ports — they are DNATed
in `PREROUTING` and accepted in `FORWARD`, never traversing `ufw-user-input`. A rule scoping `5000`
to the bridge existed and did nothing. **For anything containerised, the bind address is the only
control.** Verify from off-box, never from a rule table.

## 6. Review checklist for a change

- Does it add a listener? What does it **bind** — not what does the firewall say.
- Does it add a route? `checkBearer` (fail-closed) or `checkOptionalBearer` (open when unset)? If
  the latter, is that written down where an operator sees it?
- Does it cache a secret or a resource value? Is the cache key **identity + path**, never path alone?
- Does it mutate on behalf of an identity it did not itself check? Say so in the docstring.
- Does a blank config value mean OPEN or DISABLED? Both exist here and they are opposites.
