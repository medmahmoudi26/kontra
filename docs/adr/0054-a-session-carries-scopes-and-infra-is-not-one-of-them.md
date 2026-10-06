# 54. A session carries scopes, and `infra` is not one of them

Date: 2026-10-06

## Status

**Accepted.** Amends **0045**, whose Decision bullet claimed a session "authenticates to everything
the console does and nothing beyond it" — aspirational at the time, and now true by construction
rather than by admission order.

## Context

`checkBearer` admitted a live console session **before** it consulted the route's token list:

```ts
if (sessions.verify(bearerOf(authorization))) return null;
const token = configuredToken(vars);   // never reached by a session
```

So one browser credential reached `POST /api/infra/stacks/:fqn/:op` — the route that converges a
Pulumi stack, provisions Droplets and spends money — **including on an install with no
`KONTRA_STATE_TOKEN` configured at all**, where there was nothing for the session to be compared
against.

The invariant was asserted in four places and held in none of them: `auth/session.ts`'s header
("a session is NOT a service token"), `auth.ts`'s own comment at the bypass, `infraRoutes.ts`
("TOKEN-GATED FROM THE FIRST COMMIT, WITHOUT EXCEPTION"), and ADR **0045**. The documentation was
already written for the world this ADR creates; the code was behind it.

The token half needed no work: `INFRA_ROUTE_TOKEN_VARS` is a one-element list with no fallback,
unlike `EXPLORE_TOKEN_VARS` and `SECRETS_TOKEN_VARS`, which fall back to the state token.

## Decision

**A session proves who is asking. A scope says what that buys.**

1. A session carries a scope set. `mint()` grants `["console"]` and **no path grants `infra`**, which
   is what keeps it a service-token capability rather than something a person inherits by signing in.
2. `checkBearer` and `checkOptionalBearer` take a scope, defaulting to `console`. A live session
   without the scope is refused **403**.
3. The one mutating infra route asks for `infra`. The five reads stay on `console`.

### Why 403, and why not a fall-through

**401 signs the console out.** The browser clears its token on one, so a scope refusal spelled 401
would end the operator's session and read as an orchestrator restart. Falling through to the token
compare is worse: a session would be compared against the service token, miss, and 401 anyway — the
right answer by accident, and only while the two strings differ.

### Why only the mutation is scoped

Reading which Machines exist is not the capability that was leaking; spending money is. Scoping the
five GETs would 403 the console's Settings › Infra page and the run page's Fleet strip, which are
documented browser clients of exactly those paths — a product regression bought with no security.

## Consequences

- **The console's infra *reads* keep working unchanged.** Its infra *writes*, if any are ever added,
  will need a service token rather than the signed-in session.
- `sessions.verify` is now a thin caller of a new `look()`, which returns the user **and** the
  scopes from one constant-time scan. Two scans would mean two TTL slides per request, the second on
  a session the first just extended.
- Sessions remain in memory and die with the process, so the scope set needs no migration.
- **Still open, and not closed by this ADR:** `/api/state/:tier`, `/api/uploads` and `/api/audit`
  also fail closed on service tokens and still admit any `console` session. Leaving them on the
  default scope is deliberate here — it preserves today's behaviour exactly — but "a session cannot
  reach a service-token surface" is not yet true of them.
- `docs/openapi.json` still advertises `consoleSession` on the infra surface. That generator lives in
  a file being edited concurrently and is left to a follow-up rather than merged blind.

## Tests

`control/orchestrator/src/infraRoutes.test.ts` — the first test this surface has had. It drives the
mutating route with a plain session (403), with an `infra`-scoped session (admitted), with the
service token (admitted), with no credential configured (503 naming the variable) and with a wrong
token (401); asserts no mint path grants `infra`; and walks all five reads behind a count assertion
so a loop over an empty list cannot pass.
