# 27. Apicurio is removed; per-actor I/O compat is owned in-process

## Status

**Accepted, 2026-08-17.** Supersedes **`legacy/0008`** ("Compat ownership split: buf owns the
envelope, Apicurio owns author I/O"), and replaces its row in **0024** §"What v2 carries forward".

The buf half of 0008 survives unchanged and is now actually run from the repo (`make proto-check`
→ `scripts/proto-check.sh`, and the `proto` job in CI). The Apicurio half is deleted: the code, the
tests, the configuration, the fleet env var and the claims. `control/orchestrator/src/catalog.ts` — the
descriptor gate at `POST /api/actors`, landed in `d272abe` — is what owns per-actor I/O now.

This record exists because an ADR whose subject has been deleted without a word is worse than no
ADR at all, and because the deletion **loses a check that 0008 promised**. That gap is §4 below.

## Context

0008 read as implemented. Its Status line said so in bold — "the Apicurio side is now wired too" —
and named the files: `actorkit/schema_registry.py` pushing input schemas under a **BACKWARD** rule
and output schemas under **FORWARD**, `scripts/register_schemas.py` running it, `make register`
driving that, an HTTP 409 → `SchemaIncompatible` as the gate. `kontra.yaml` carried it as an
`infra.apicurio` block with an endpoint and an `endpointEnv`, plus a `schemas:` profile whose
`requires:` edge named it. `control/orchestrator/src/infra/programs/machine.ts` wrote
`KONTRA_APICURIO_ENDPOINT=http://$CONTROLLER:8081` into `/etc/kontra/worker.env` on **every
Machine of every Fleet**. The `Makefile` header, `infra/README.md`, `docs/wiki/Deployment.md`'s
control-plane diagram and its port table all listed it as part of the control plane.

**None of it was running, and none of it had ever run.** There is no `apicurio` service in
`docker-compose.yml` and no history of one; nothing binds `:8081` on the Controller.
`scripts/register_schemas.py` and `make register` do not exist. `internals/registration.py` — the
only importer of `internals/schema_registry.py` — was imported by nothing but its own test. The
env var was read by no client in either SDK. So the whole apparatus resolved to two Python modules
with 363 lines of tests that passed, describing an HTTP conversation with a service that was never
on the other end.

This is the same failure mode **0024** was written about, one level down. A stale record is not
merely wrong; it is *confidently* wrong in the places a reader is told to go to find out who
checks. Here the claim had spread out of the ADR and into eight more files, including two — the
repo-level facts file and a contract header — whose entire job is to be believed. The concrete
cost we can point at: every Droplet ever placed was handed an endpoint for a schema registry, so
an operator debugging a registration problem on a fleet Machine reads `worker.env` and finds a
component to investigate that has never existed.

Meanwhile the enforcement 0008 was reaching for arrived, in-process and for a different reason.
`d272abe` put two gates on `POST /api/actors`:

- **The descriptor is validated with ajv.** The route used to check three strings were present and
  then write `(body.operations ?? []) as ActorOperation[]` — a cast erased at build time, so an
  `operations` array of numbers registered with a 200. Per-Method `params`/`input`/`output` are
  additionally checked to BE JSON Schema documents, against the 2020-12 meta-schema, *without*
  compiling them (compiling registers a document's `$id` globally and a second compile of the same
  id throws — the bug that 500'd a Go actor dispatched twice).
- **ADR 0004 is enforced.** Re-registering a `(name, version)` the catalog already holds whose
  input/output/params schema differs is refused with **409**, naming the Method that moved.
  Previously the second registration silently overwrote the first.

That is the same-version half of what 0008 wanted, arrived at from the other side: not "is this
schema compatible with its predecessor" but "this identity has already promised a shape and a
promise does not change".

## Decision

1. **Apicurio is removed, not replaced by another registry.** Deleted:
   `runtime/python/internals/schema_registry.py`, `runtime/python/internals/registration.py`,
   `tests/test_schema_registry.py`, `tests/test_registration.py`, the `infra.apicurio` block and
   the `schemas:` profile in `kontra.yaml`, `KONTRA_APICURIO_ENDPOINT` from the fleet Machine's
   `worker.env`, and every prose claim (Makefile header, `infra/README.md`, the Deployment
   diagram and port table, Contracts, Dev-Cycle, Getting-Started, the architecture drawing).
   `docs/adr/legacy/` is untouched — 0024 makes that corpus deliberately historical, and
   `legacy/0002` and `legacy/0008` are the argument this record supersedes, not claims about the
   running system.

2. **The tests go with the code.** `pyproject.toml` has `testpaths = ["tests"]`, so
   `test_registration.py` and `test_schema_registry.py` were collected and green on every run —
   green tests for code nothing imports, exercising a protocol nothing speaks. A passing suite that
   covers a deleted component is worse than no coverage, because it is *evidence* of health.

3. **Per-actor I/O compat is owned at the registration route**, `control/orchestrator/src/catalog.ts`,
   covered by `catalog.test.ts` and `server.test.ts`. It is not optional, not env-gated and has no
   force flag. `kontra.yaml`'s `contracts.actorIO` now records that owner, and records what it does
   and does not check, rather than an owner and two variance rules nobody ran.

4. **The lost check is named, not glossed: there is no CROSS-VERSION compatibility check.**
   The gate compares a registration against what *the same* `(name, version)` already says.
   Nothing compares `myactor@0.2.0`'s input schema to `0.1.0`'s. A bump whose new input schema an
   existing caller cannot satisfy — a field made required, a type narrowed — registers cleanly and
   surfaces later, inside a run, as data that does not fit. 0008 assigned exactly that check to
   Apicurio (BACKWARD on inputs, FORWARD on outputs) and it was never run once, so **nothing
   regresses today**; what changes is that the gap is now written down instead of covered by a
   claim that a registry was handling it.

5. **buf keeps the envelope, and that half is real.** `buf lint` + `buf breaking` against the fork
   point, `make proto-check`, plus `buf generate` drift in CI. 0008's *split* was right — the
   envelope and per-actor I/O evolve on different cadences — and survives this record. What does
   not survive is the second owner being an external service.

## Alternatives considered

- **Wire Apicurio for real: add the compose service, run the registration in the build.** The
  honest version of "0008 said implemented, so implement it". Rejected on what it buys against what
  it costs. It buys the cross-version check in §4, which is real. It costs a stateful service on
  the Controller whose *in-memory* image loses its compat history on every restart (0008's own
  Consequences section says so), so the durable variant needs a database too — and it puts a
  network call to a schema registry into the build path of every actor, where an unreachable
  registry is either a hard build failure or a silent skip, both of which we have been bitten by
  in this repo. It also splits enforcement across two processes for one question: the catalog
  already holds every schema it would register.
- **Keep the modules, delete only the claims.** Rejected: `registration.py` was imported by nothing
  and its test suite was the only thing keeping it alive. Dead code with a green suite is precisely
  what let the claim survive this long.
- **Leave a stub `KONTRA_APICURIO_ENDPOINT` for compatibility.** Nothing reads it. A variable kept
  "for compatibility" with no reader is a claim with a version number.
- **Amend 0008 in place rather than superseding it.** 0008 lives in `legacy/` and 0024 rule 6 makes
  a legacy record evidence about *why*, never authority about *what* — an amended legacy file would
  be a live decision filed as history, the exact ambiguity 0024 exists to remove. Its `legacy/0002`
  companion (the proto-vs-Apicurio payload split) stays untouched for the same reason: its
  conclusion — the payload is opaque at the envelope layer — is carried forward here, and its
  reasoning is worth reading.

## Consequences

- **A fleet Machine provisions with one fewer variable and nothing else changes.** `worker.env` is
  the only file that carried it; no unit, no host and no SDK read it. `machine.test.ts` now pins
  the env block by name — the failure being guarded is a variable *nobody consumes*, which can only
  ever be believed by a reader and never observed failing, so the assertion is that the block
  contains nothing beyond the known set.
- **A changed schema is caught earlier and more loudly than 0008 ever caught it**, at registration
  rather than at build, with a 409 naming the Method. The dev-loop cost is accepted and deliberate
  (see `catalog.ts`): editing an actor's types and re-serving without a version bump now fails.
- **§4's gap is CLOSED, and `kontra.yaml` now records `crossVersionCompat: structural-report`.**
  The gap was written into the facts file rather than left as a paragraph here — because a
  paragraph in an ADR is exactly how it was covered up last time — with the note that the field is
  what someone comes back and changes. This is that change. `control/orchestrator/src/compat.ts` compares a
  registration's per-Method schemas against the version immediately preceding it (by the same
  ordering the palette folds versions with, now shared as `control/orchestrator/src/versions.ts`), under
  0008's own direction rule: **input is BACKWARD** — a new *required* field breaks a producer shaped
  for the old schema, an added optional one does not — and **output is FORWARD** — a removed field
  breaks a consumer, an added one does not.

  **It reports and never refuses.** That is the opposite of the same-version gate above and it is
  the decision, not an unfinished half: a schema change under a fixed version is never legitimate,
  while a breaking change in a NEW version is precisely what versions are for, and a gate that
  refused one would need an escape hatch and then get ignored. `POST /api/actors` answers 200; the
  finding is stored on the actor row (a column, so it survives a restart) and drawn on the Actors
  page beside the version that introduced it, naming the Method and the direction.

  **What it still does not prove**, listed here because §4's failure was a check believed rather
  than read:

  - It is a **structural** comparison of two JSON Schema documents — top-level `properties`,
    `required`, and each property's declared `type` (`anyOf` flattened, because that is how a
    derived optional arrives from pydantic). Nested objects, `$ref`, `allOf`/`oneOf`, enum members,
    `format`, numeric bounds and `additionalProperties` are not read: narrowing `port` from
    0–65535 to 1–1024 reports clean.
  - It cannot see **meaning**. A field whose name and type are unchanged while what it holds is not
    — seconds become milliseconds, an id starts pointing at something else — is not a schema change
    at all.
  - **`params` is not compared.** 0008's rule named inputs and outputs, the two things that flow
    per unit; a third direction invented in the implementation would be policy written where nobody
    would look for it.
  - **A Method the preceding version had and this one drops is not reported.** It is a real break —
    a dispatch to a name that no longer resolves — in a shape that does not fit a per-signature
    finding. (Under an *unchanged* version the immutability gate refuses it outright.)
  - **Unknown is a real answer and is never "compatible":** a first version, a Method new in this
    version, and a schema that declares no properties on either side (`{"type":"object"}` says
    nothing structural). Nothing anywhere draws a compatible badge, because the check cannot
    support one.
  - It compares against **one** version, at registration: `0.3.0` against `0.2.0`, never against
    `0.1.0`, and a version registered out of order afterwards does not recompute an earlier
    finding — which is why a stored finding names the version it was compared against.
- **`shared/contracts/kontra/v1/catalog.proto` and `run.proto` no longer name an external owner**, and the
  generated stubs (`handler/_gen`, `control/orchestrator/_gen`) were regenerated with `buf generate` rather
  than hand-edited.
- **`docs/adr/` is no longer gitignored, and three documents still say it is.** 0024's last
  consequence, `docs/wiki/ADRs.md` and this issue all warn that a new record needs `git add -f` or
  it silently never lands — the reason 0017 and 0020 went missing. The ignore rule is gone from
  `.gitignore` as of HEAD (`git check-ignore` says nothing, `git add --dry-run` stages this file),
  so a plain `git add` is enough. `-f` is still harmless and still the safe habit. The wiki
  paragraph is corrected here; 0024's is left as written, because it was true when 0024 was
  accepted and a record is not a status page.
