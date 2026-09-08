# 45. The console signs in, rather than carrying a bearer baked into its bundle

## Status

**Accepted.** Closes the `VITE_KONTRA_EXPLORE_TOKEN` half of **0038**'s data-plane finding. Extends
**0031 §3**, which removed the *reason* to bake a token on loopback but not the mechanism, and
**0020**, whose `/api/panels/ticket` proxy solved the same problem for the Dashboard and named this
one as the precedent it improved on. Supersedes no ADR.

## Context

The CLI authenticates by reading `~/.kontra/config.yaml`. **A browser cannot**, and that one sentence
is the whole of this decision.

So the console carried `VITE_KONTRA_EXPLORE_TOKEN`, a bearer substituted into the bundle at build
time. It works, and everything wrong with it follows from *where* it lives rather than from what it
is:

- **It cannot rotate.** Changing the server's token invalidates every built bundle.
- **It is identical for every operator.** There is nobody to attribute anything to.
- **It outlives the container.** Whatever registry, backup or image layer holds that artifact holds
  a live credential.
- **It fails silently, and did.** `run/query.ts` fell back to `''`, `/api/datasets/query` is
  deliberately fail-closed — its own comment calls it *"a PRIVILEGED surface: it can read every
  dataset"* — so a bundle built without the variable answers `query: unauthorized` for **every**
  query while looking entirely healthy.

That last one is not hypothetical. `dist/` is bind-mounted read-only into the running orchestrator
(`/root/oss/kontra-console/dist -> /app/web/dist`), so a `pnpm build` in that directory does not
stage an artifact to deploy later — it **replaces what is being served, immediately**. Builds run for
unrelated reasons took the query workbench down, and the error an operator saw named neither the
token nor the build.

`routes/panels.ts` already solved this shape for the Dashboard and said so, naming this exact
variable: *"anyone who could reach it could previously read the token baked into the bundle by
`VITE_KONTRA_EXPLORE_TOKEN`. What it removes is the DURABLE leak."*

## Decision

**The console signs in, and the token it gets back is the `Authorization` header for everything it
does.**

- **The credential lives where the CLI's already does.** `kontra init` generates a console account
  at install, **prints the password once**, and stores only an scrypt hash in `~/.kontra/config.yaml`.
  Nothing can recover it afterwards, which is the point of a hash rather than a secret.

- **`POST /api/login` mints a SESSION, not a service token,** and that distinction is what makes it
  safe to hand a browser. `KONTRA_STATE_TOKEN` admits the infra routes and can spend money. A session
  is minted per sign-in, expires on twelve idle hours, lives in the orchestrator's memory and dies
  with the process. `checkBearer` and `checkOptionalBearer` accept it, so it authenticates to
  everything the console does and nothing beyond it.

- **scrypt, encoded self-describingly** as `scrypt$N$r$p$salt$hash`. It is the one password KDF both
  sides have with **no new dependency** — `golang.org/x/crypto/scrypt` and `node:crypto`'s builtin.
  The Go CLI writes it and the TypeScript orchestrator verifies it, so the format is a corpus
  (`shared/conformance/login.json`), executed by both: a hash one writes and the other cannot read is
  a login nobody can pass, and each half would be correct on its own.

- **One `fetch` interceptor, not fifty call sites.** `run/api.ts` alone makes 45 `fetch` calls with no
  shared wrapper. The wrapper is narrow on purpose: same-origin `/api/…` only, never overwriting an
  `Authorization` the caller set, and adding nothing when signed out.

- **The build guard inverts.** It was added to *require* the baked token, because a bundle without
  one 401s silently. With the console no longer reading it, baking one is a leak with no upside — so
  a production build that sets `VITE_KONTRA_EXPLORE_TOKEN` is now **refused**.

## Considered options

**A ticket, as `/api/panels/ticket` does.** The closest precedent, and rejected because the shapes
differ: panels proxies to a *second process* on another port, so the API can hold the credential the
browser never sees. Here the token-holder and the route are the same server, so "proxying" is just
removing the gate.

**Serve the token to the SPA at runtime** — an injected `<script>` or a config endpoint. Removes the
durable artifact leak and nothing else: the browser still holds a long-lived service token, still
cannot be attributed, and still cannot be signed out.

**Leave it, and only fix the build guard.** The cheapest, and what the first attempt did. It stops
the *silent* failure and keeps every other property that made this worth changing.

**One shared instance password rather than named users.** Smaller, and it kills the baked token just
as well. Rejected because "two engineers share an instance" is the stated case, and an account per
person costs one more field in a YAML list.

## Consequences

- **Sessions do not survive an orchestrator restart,** deliberately. A redeploy signs everyone out.
  For a two-person instance that is a fair trade for having no session store on disk, and a restart
  is exactly when you want every browser to prove itself again. A 401 clears the local token and the
  login form returns, rather than each panel rendering its own failure.

- **The token sits in `localStorage`,** readable by script on that origin. The defence is that
  nothing else is on that origin, and that a session is not a service token — it expires, it dies
  with the process, and it cannot spend money.

- **This does not authenticate the rest of the API.** `auth.ts` still records that most routes are
  unauthenticated, and `hosted-readiness/03` is still open. What changed is that the console has a
  credential of its own instead of a shared one in an artifact — a prerequisite for that work, not a
  substitute for it.

- **The accounts reach the orchestrator through the environment,** base64 of JSON in
  `KONTRA_CONSOLE_USERS`, exported by `config.ApplyConfig` like every other setting. The orchestrator
  has no YAML parser and adding one to read two fields is a dependency it does not need. Base64
  because the hash contains `$` and compose substitutes `$` in values it passes through — the raw
  form would arrive corrupted into something that still *looks* like a hash, so every login would
  fail as "wrong password" rather than as the configuration error it is.

- **An install with no console user is not locked out.** `GET /api/login` reports whether signing in
  is possible; when it is not, the gate steps aside and the login route 503s with the command that
  creates an account. That is the pre-login behaviour preserved, so this cannot brick an existing
  installation — and the server still decides what those requests may do.
