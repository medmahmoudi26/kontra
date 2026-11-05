# Conflict: the `call` tool on `/mcp` (Track M, item 9)

Status: **stopped**, as the brief allows ("if it cannot be done cleanly without restructuring mcprelay, STOP for call
only"). Every other item of Track M is built (D332 to D339); this file records why `call` is not, and the design that
would build it. Decision record: [`decisions.md`](decisions.md) D339.

## What the brief asked

`call {service, tool, arguments?}`: a hosted MCP service's tool as a plain `tools/call` through the very relay
`/mcp/{svc}` uses, as the authenticated principal, in process, never a network loopback that re-enters the edge with
a credential; Admission, §11.3 policy, §11.4 metering, sealing and required-extension refusals, `tasks: required`
refusal, anonymous limits; the upstream `CallToolResult` as it is within D295's 50 s; the gate's internal client
declaring no client capabilities.

## Why it does not fit the relay as it is

1. **The relay never translates between eras** ([`relay.md`](../spec/relay.md) §11.12, "Where a modern request goes";
   D177). A plain `tools/call` reaches an upstream only in that upstream's own era. A 2026-07-28 request to a legacy
   upstream is answered by the upstream's own 400, 404 or 405, which the relay passes on so that a dual-era client
   falls back to `initialize`. The stock servers the gate hosts, `examples/mcp/everything` (SDK 1.32.2) and
   `server-filesystem`, are legacy. So the modern request path, the one that needs no session, cannot carry `call` to
   most services.
2. **The legacy path is a session, not a request.** A legacy `tools/call` needs an upstream session opened by
   `initialize` (§11.2): Admission's `session` operation, the session choice of §6.1 (this replica's tunnel sessions
   first, else **another replica's through the peer hop**, `internal/peer`), `max_sessions_per_principal` (D213,
   D231), an `mcp_sessions` row pinned to that tunnel session, and later requests relayed on the pinned session,
   hopping to the replica that holds it. `mcprelay.Handler` builds all of this inside `ServeHTTP` from the client's
   `http.Request`: the credential is read from its headers (`AuthenticateMCP`), the service from its path, the pinned
   session from its `Mcp-Session-Id`, and the peer hop forwards that request to the holding replica. There is no entry
   point that takes an already-authenticated principal and a tool call, and the peer hop is a network hop by design.
3. **A gate-held client session has no owner in the spec.** `call` would make the gate the MCP client of a legacy
   upstream on the principal's behalf: who opens the session (per call, or one kept per principal and service), when
   it is closed, how it counts against `max_sessions_per_principal` and an anonymous tenant's caps
   ([`tunnel.md`](../spec/tunnel.md) §6.2), and what Admission's `session` operation and `per_session` pricing
   (§11.12, D269: "/mcp is not a service") make of it. None of this is decided, and each answer changes metering and
   money, which the owner's rules keep to exact reads (tool name, bytes, duration, isError).

Building it in process therefore means restructuring `mcprelay`'s request path (2 above) and deciding the session
model (3), which is the stop condition.

## Proposed design

1. **An in-process entry point in `mcprelay`**: `(*Handler).CallTool(ctx, p auth.Principal, svc *Service, tool string,
   arguments json.RawMessage) (CallToolOutcome, error)`, factored out of `pinned`/`modern` so that `ServeHTTP` and the
   new entry share one function after authentication: `allow:` (the caller passes the principal it authenticated),
   §11.3's policy (a hidden or denied tool is `-32602 Unknown tool`, answered as `isError`), `tasks: required`'s
   refusal, the sealed-required refusal, the anonymous `calls` slot (`takeCall`), Admission's `call` operation, and the
   `Meter` (one `calls` row: tool name, bytes, duration, isError). It writes to an in-memory response writer, never to
   the network, and the client's `Authorization` never exists on it.
2. **The upstream session**: one short-lived legacy session per `call` (initialize with empty client capabilities,
   `notifications/initialized`, the `tools/call`, `DELETE`), opened through the existing `initialize` code with
   Admission's `session` operation consulted as for any client, so `per_session` pricing and the session caps apply
   unchanged. A modern upstream (era learnt, §11.12) gets one modern request instead.
3. **The hop**: when the picked tunnel session is on another replica, the hop carries the principal as the peer
   protocol already carries a pinned session's requests (the replica's peer credential, `internal/peer`, D163), not
   the caller's credential, so nothing re-enters the edge.
4. **Bounds**: D295's 50 s; past it `isError` "still running; connect to `{public_url}/mcp/<ref>` with MCP tasks for
   long calls", and the upstream exchange is closed, as §11 cancels an abandoned plain call.
5. **D272** is amended by a new decision ("MCP services are called directly" becomes "directly, or through `call` on
   `/mcp`"), with the console's reason: "every service, one connection".

Until then `describe` gives each MCP service's URL and tool list, and the instructions of `/mcp` say that MCP servers
are called directly at the URL `describe` gives.
