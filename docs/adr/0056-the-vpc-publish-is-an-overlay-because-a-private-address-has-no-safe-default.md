# 56. The VPC publish is an overlay, because a private address has no safe default

Date: 2026-10-06

## Status

**Accepted.** Makes true the claim `docker-compose.yml`'s own header and
`scripts/assert-loopback-publish.py` have both been making since ADR 0047.

## Context

Five of `docker-compose.yml`'s eleven published ports — Temporal, Redis, the registry, S3 and the
orchestrator API — were spelled:

```yaml
- "${KONTRA_VPC_BIND:-10.124.0.2}:${KONTRA_TEMPORAL_PORT:-7233}:7233"
```

`10.124.0.2` is **one particular machine's own private address.** On that machine the default is
invisible and correct. On every other machine it is an address the host does not have, so the port
cannot bind and the documented bare-`docker compose up` quickstart does not come up — while the same
file's header called loopback the security control.

A default cannot fix this, and the two candidates are the reason:

- **A real address** is correct for exactly one host and fails to bind everywhere else.
- **Empty** is worse. Compose emits a publish with no `host_ip`, and no `host_ip` is `0.0.0.0` — the
  entire control plane on every interface, including the public one, reached by leaving a variable
  blank. Postgres still ships `changeme` (ADR 0052 §4), so that is not an abstract exposure.

Two more host-specific values rode along in the same file. `orchestrator-infra` was handed
`KONTRA_REGISTRY`/`KONTRA_TRUST_REGISTRIES`/`KONTRA_TRUST_UNSIGNED` pointing at that address, so an
install elsewhere told its Machines to pull Bundles from a registry belonging to someone else's
network; and the fleet SSH key was mounted from `${KONTRA_SSH_KEY_HOST:-/root/.ssh/axiom_rsa}` — one
developer's private key, by default, into every install that ran the quickstart. The repository's own
`scripts/provision-controller.sh` generates a fresh keypair per controller precisely because "handing
that key to another installation means one compromised controller owns every fleet on the account."

## Decision

**The default stack publishes on loopback only. Reaching a fleet is opt-in, and it is a file.**

1. `docker-compose.yml` keeps six published ports, every one on `KONTRA_BIND` (`127.0.0.1`). It
   contains no private address, no host-specific registry and no key mount.
2. `docker-compose.vpc.yml` carries the five VPC publishes, the three infra registry variables and
   the fleet key mount:

   ```sh
   KONTRA_VPC_BIND=<this host's private address> \
     docker compose -f docker-compose.yml -f docker-compose.vpc.yml up -d
   ```

3. **Every variable in the overlay is `${VAR:?message}`, not `${VAR:-default}`.** Unset, compose
   refuses to start and names what to set. This is the whole point: the failure mode of the old
   spelling was a silent wrong answer, and `:?` has no silent wrong answer.

### Why an overlay and not a profile

A compose `profile` would keep the publishes in the main file, where they remain the thing a reader
has to notice is conditional — and `scripts/assert-loopback-publish.py` reads running containers, so
a profile left on would pass the file review and fail the box. A separate file makes the posture
legible from the command that started it.

## Consequences

- **A bare `docker compose up` now works on a machine that is not the one this file grew up on**, and
  publishes nothing beyond loopback.
- **An existing VPC install must add `-f docker-compose.vpc.yml` and set `KONTRA_VPC_BIND`.** It will
  not silently keep working: the ports move to loopback and Machines stop being able to call home.
  That is the intended direction — the failure is loud and local.
- `control/pulumi/parity.py` goes from 22 problems to 11. Its port leg now compares six against six,
  which is what `cli/control.go:103`, `Pulumi.yaml:127` and `control/pulumi/README.md:73` have each
  claimed all along; the remaining 11 are unrelated env/mount drift and are **still red**.
- `campaign-worker` is deleted. Its own comment asked for this, and it served
  `workspaces/bugbounty`, which is not in the repository.
- `.env.quickstart` gains `KONTRA_REPO` (`cli/envexample_test.go` was red on it) and its VPC block now
  documents the overlay instead of describing a binding `docker-compose.quickstart.yml` never had —
  that file has always published six loopback ports and substitutes `KONTRA_VPC_BIND` nowhere.
- **`server.ts`'s `0.0.0.0` default is deliberately left alone.** `cli/up.go:50` already closes it to
  loopback and `cli/orchestrator_test.go:69` pins the override direction, so changing the default
  would move an invariant that is currently tested into one that is not.

## Tests

`scripts/assert-loopback-publish.py` inspects **running containers**, so it is a check on a box and
not on this change; it still reports the pre-existing stack until that stack is recreated. The
file-level claim is asserted by `control/pulumi/parity.py`, which counts compose's publishes against
Pulumi's six and requires every `ip` to be `127.0.0.1`.
