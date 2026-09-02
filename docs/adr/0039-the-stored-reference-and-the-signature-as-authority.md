# 39. The stored reference, and why the signature is the authority

## Status

**Accepted.** Completes **0036**, which made the **Bundle** an OCI artifact and declared that kontra owns no registry, without giving the control plane anywhere to remember *which* registry.

Depends on **0038**, which makes a separate actors repository the reason this matters.

## Context

1. **A push to a registry you own produces something kontra cannot run.** `control/orchestrator/src/activities/fleet.ts:resolveBundle` re-derives the address on every run:

   ```
   <registry>/v2/bundles/<actor>/manifests/<version>
   ```

   where `<registry>` is `KONTRA_REGISTRY` or the Controller's own. The control plane stores **neither the digest nor the reference**. So `kontra build --push ghcr.io/acme/nscheck:0.1.0` publishes something real, mirrorable and **not placeable**, and `kontra build --json` already reports `"placeable": false` for exactly this case. 0036 said kontra owns no registry; nothing was built to remember which one it does not own.

2. **Building requires the checkout.** `cli/bundle.go` calls `findRepoRoot("")` and refuses with *"building a Bundle needs the checkout (handler/ + sdk/ + runtime/)"*, because the handler is cross-compiled from source — measured at 3m21s cold, almost all of it that compile. A fork of `kontra-actors` has no `handler/`.

3. **There is already a place to write a digest, and it is unauthenticated.** `control/orchestrator/src/routes/catalog.ts:77` serves `POST /api/actors/:key/digest` with no auth guard. `control/orchestrator/src/catalog.ts:27` records that this is deliberate: a worker self-registering its own image digest is not on the catalog's registration path.

   Today that is tolerable **because the address is derived**: the digest is advisory and placement does not consult it. The moment a stored reference drives placement, that endpoint decides which bytes run on every Machine in a Fleet.

4. **0036 already put a trust root on the Machine.** The **Trust policy** — an allowlist of registries and a signature identity — is enforced by the **Warden**, at the pull, and is LOCAL to the Machine. It exists precisely to bound "a control plane that asks for the wrong image".

## Decision

- **The control plane stores a reference per `(actor, version)`**, and placement uses it instead of re-deriving an address. An Artifact pushed to any registry becomes placeable, which is what 0036 promised.

- **The released binary carries prebuilt handlers**, one per published platform, so `kontra build` needs no checkout. The release workflow already cross-compiles for four platforms; this is the same matrix, embedded rather than discarded.

- **The signature is the authority, not the endpoint.** `POST /api/actors/:key/digest` stays permissive. What changes is that the Warden's **cosign verification becomes mandatory at pull**: an image not signed by the identity configured on that Machine is refused, whatever the stored reference says.

  An attacker who repoints the reference achieves nothing — the pull refuses. This is the only arrangement in which **compromising the control plane does not compromise execution**, which is the property the whole Warden design rests on.

- **The allowlist is checked before anything is fetched.** Measured while building the trust policy: the string gates cost **3.8–10.5 µs** and zero syscalls; one signature-manifest request costs **15–21 ms** on a loopback registry; a 20 MiB pull costs **0.40–0.54 s** cold, and a real actor image 19 s. Three to five orders of magnitude. The ordering is not an optimisation — a pull's DNS lookup and ClientHello tell a refused registry the Machine's address and its clock.

## Considered options

**Authenticate the digest endpoint instead.** Scoped by namespace, the way 0036 scopes everything else. Rejected as the *primary* control: it makes the control plane the trust root, so whoever holds the token can repoint any actor in that tenant, and a compromised control plane compromises every Machine. Signature verification bounds that; a token does not.

**Both — authenticate the write and verify at pull.** Rejected for now on honest cost: the auth half needs token issuance, rotation and scoping for a control that is redundant once signing is mandatory, and a half-built auth path invites treating signing as optional later. It can be added; it should not be the reason signing is not.

**Only the CLI may write the reference.** Attractive — one writer instead of two — but `catalog.ts:27` records a reason the worker path exists, and removing it is a separate decision from making placement work.

**Keep deriving the address, and require every push to go to the Controller's own registry.** This is what happens today and it does work: the derived address is correct when everyone pushes to `<controller>:5000/bundles/<actor>`. Rejected because it contradicts 0036's central claim — a client who cannot push to their own ghcr or Harbor does not own the registry in any meaningful sense.

## Consequences

- **Signing stops being optional**, and that is a real cost. `cosign` is not on the development machine, so the keyless path — certificate chain, Rekor, SAN matching — is **unproven end to end**. What is proven is the argv, the pinned-reference-not-tag rule, exit-status handling, and that a missing `cosign` is a REFUSAL rather than a silent pass. A verification that degrades to always-allow is the failure `watchdog.sh` shipped for years in this repository.

- **A signature proves who built the bytes, and nothing else.** A signed backdoor verifies. It says the digest was signed by the key or workload identity named in *this Machine's own configuration* — not that the code is good, not anything `cosign` itself does not check, and nothing against a Machine whose trust root is already compromised.

- **The trust policy is local and therefore rewritable by root on the Machine.** It bounds a substituted registry and a control plane asking for the wrong image. The change that would fix it is delivering the policy *inside* the enrolment credential signed by the Fleet CA — 0036 mints such a credential and it does not carry this. Recorded as unsettled rather than implied.

- **One shared answer, not four.** Three call sites already name an **Artifact** — `build`, the pull in `scale.go`, and the podman driver — and a fourth was found in `deploy`'s push half. They consult one grammar in `cli/internal/ociref/ociref.go`, pinned by `shared/conformance/ociref.json`, and the trust policy extends it rather than adding a fifth. That coupling is the point: **a second derivation of "which registry is this" is a bypass, not an inconsistency.**

- **Until both halves land, `kontra-actors` is a place to keep actors, not a place to build them.** A fork can hold source and CI configuration; it cannot produce a placeable Artifact.
