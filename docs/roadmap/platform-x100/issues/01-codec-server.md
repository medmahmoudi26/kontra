# 01 — Temporal codec server (UI payload decode)

Status: **done** — `runtime/handler/codecserver` implements Temporal's remote-codec contract
(`POST /encode` / `POST /decode`) and is served by the appliance at `cli/appliance/codec/codec.go`
on a listener it already owns. Do not work this.

> **The "Problem" section below is STALE and was believed on 2026-09-26.** It says "there is **no**
> codec server", which was true when written and has not been for some time. The package exists,
> has a test, and is wired — what changed is HOW: ADR 0031 §1 collapsed it from `cmd/codec-server`
> + `Dockerfile.codec-server` + a compose service with a published port into an HTTP handler on the
> appliance's own listener ("it is an HTTP handler in a container; it becomes an HTTP handler").
> That also collapsed the codec's port and the address the UI is told to call from two hand-synced
> settings into one derived one.
>
> What is still true: the **docker-compose** stack has no Temporal Web UI (no `temporal-ui`
> service, nothing on :8233 — it publishes only `127.0.0.1:7233`), so there is nothing there for a
> codec server to decode *for*. The UI and the codec endpoint are an appliance (`kontra up`)
> feature. If you want them under compose, that is the open work — not building the codec server.
**Tier:** 1 | **Effort:** S–M | **Depends on:** —

## Problem
We already offload large Temporal payloads to S3 via a claim-check codec
(`runtime/handler/internal/codec/codec.go`, `control/orchestrator/src/codec/claimCheck.ts`, wired at
`runtime/handler/main.go:46-47`). But there is **no codec server**, so the Temporal Web UI (:8233) and
`temporal workflow show` display offloaded inputs/outputs as opaque `$ref` blobs. Half the point of
the claim-check pattern — being able to *read* payloads in the native UI — is unrealized.

## Native capability to use
Temporal **Remote Data Encoder / Codec Server**: an HTTP service exposing `POST /encode` and
`POST /decode` that the Web UI/CLI call to transform payloads for display. This is the sanctioned
pattern; we only need to expose our existing decode logic over that contract.

## Approach
- New Go binary `runtime/handler/cmd/codec-server/` that reuses `runtime/handler/internal/codec` to implement the
  remote-codec HTTP contract: body `{ "payloads": [ <Payload>... ] }` → same shape transformed.
  Honor the `X-Namespace` header; set permissive CORS (`Access-Control-Allow-Origin` for the UI
  origin, allow `X-Namespace`, `Content-Type`) so the browser UI can call it.
- Reuse the object-store client construction already in `runtime/handler/main.go` (`ThresholdFromEnv`,
  `codec.New(store, …)`) so encode/decode is byte-identical to the workers'.
- docker-compose: add a `codec-server` service on the control plane; set the Temporal UI's codec
  endpoint (`TEMPORAL_UI_CODEC_ENDPOINT` / dynamic UI config) to it. Keep it on the private network.

## Files
- new `runtime/handler/cmd/codec-server/main.go`
- `runtime/handler/main.go` (optional: extract shared store/codec construction into a helper)
- `docker-compose.yml` (+`codec-server` service, Temporal UI codec env)

## Verify (local, no fleet)
- Dispatch a run whose input exceeds the codec threshold so it offloads.
- `curl -s codec-server:PORT/decode -H 'content-type: application/json' -d '{"payloads":[<ref>]}'`
  returns the decoded payload.
- Open :8233 on that workflow → inputs/outputs render decoded, not as `$ref`.

## Risks
- Remote-codec wire format + CORS specifics; get the `Payload` JSON (base64 `metadata`/`data`)
  shape exactly right (there is a conformance test to lean on).
- Endpoint must be reachable from the *browser*, not just the Temporal server.

## Comments
