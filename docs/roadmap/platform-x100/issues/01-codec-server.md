# 01 — Temporal codec server (UI payload decode)

Status: ready-for-agent
**Tier:** 1 | **Effort:** S–M | **Depends on:** —

## Problem
We already offload large Temporal payloads to S3 via a claim-check codec
(`handler/internal/codec/codec.go`, `backend/src/codec/claimCheck.ts`, wired at
`handler/main.go:46-47`). But there is **no codec server**, so the Temporal Web UI (:8233) and
`temporal workflow show` display offloaded inputs/outputs as opaque `$ref` blobs. Half the point of
the claim-check pattern — being able to *read* payloads in the native UI — is unrealized.

## Native capability to use
Temporal **Remote Data Encoder / Codec Server**: an HTTP service exposing `POST /encode` and
`POST /decode` that the Web UI/CLI call to transform payloads for display. This is the sanctioned
pattern; we only need to expose our existing decode logic over that contract.

## Approach
- New Go binary `handler/cmd/codec-server/` that reuses `handler/internal/codec` to implement the
  remote-codec HTTP contract: body `{ "payloads": [ <Payload>... ] }` → same shape transformed.
  Honor the `X-Namespace` header; set permissive CORS (`Access-Control-Allow-Origin` for the UI
  origin, allow `X-Namespace`, `Content-Type`) so the browser UI can call it.
- Reuse the object-store client construction already in `handler/main.go` (`ThresholdFromEnv`,
  `codec.New(store, …)`) so encode/decode is byte-identical to the workers'.
- docker-compose: add a `codec-server` service on the control plane; set the Temporal UI's codec
  endpoint (`TEMPORAL_UI_CODEC_ENDPOINT` / dynamic UI config) to it. Keep it on the private network.

## Files
- new `handler/cmd/codec-server/main.go`
- `handler/main.go` (optional: extract shared store/codec construction into a helper)
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
