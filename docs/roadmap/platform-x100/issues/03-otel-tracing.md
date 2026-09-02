# 03 — OTel tracing (Dapr + Temporal + handler)

Status: wontfix — **partly shipped, partly moot.** OTel tracing is wired (the handler, the
orchestrator and the actor all export OTLP, `KONTRA_OTEL_ENDPOINT` / `KONTRA_OTEL_SAMPLING`),
but the daprd-specific half of this issue died with ADR 0018: there is no sidecar span to
correlate and no manually-injected `traceparent` — the Temporal interceptor now covers every
hop, so the orphaned-trace problem this issue exists to solve cannot occur.
**Tier:** 1 | **Effort:** M | **Depends on:** —

## Problem
There is no distributed tracing anywhere (`infra/dapr/config.yaml` only enables `ActorStateTTL`; no
OTel/Zipkin in the tree). An actor `500` (e.g. the crawl4ai `RunBatch` failures) is a stack-less log
line you SSH-tail on the right droplet. For an 8-droplet fleet this is the single biggest
debuggability gap.

## Native capability to use
Both platforms emit OpenTelemetry natively:
- **Dapr**: a `tracing` spec in the Configuration → daprd exports spans (sidecar, actor calls).
- **Temporal**: `go.temporal.io/sdk/contrib/opentelemetry` interceptor on the Go handler client +
  worker; the TS SDK `interceptors-opentelemetry` on the orchestrator worker.
- One collector (Jaeger all-in-one or an OTel collector) on the control plane.

## Approach
- Add a `tracing` stanza to `infra/dapr/config.yaml` (samplingRate, otel endpoint → collector).
- `handler/main.go`: install the OTel tracing interceptor on the Temporal `client.Options` and
  `worker.Options`; init a tracer provider pointing at the collector (endpoint from env).
- Orchestrator worker (TS): add the OTel interceptor to its Temporal `Worker`/`Connection`.
- docker-compose: add `jaeger` (or otel-collector + Jaeger UI); expose the Jaeger UI on a private
  port; pass the OTLP endpoint to daprd/handler/orchestrator via env.

## Files
- `infra/dapr/config.yaml`, `handler/main.go`, orchestrator worker bootstrap (TS),
  `docker-compose.yml`, `infra/worker-entrypoint.sh` (OTLP endpoint env for daprd/handler)

## Verify (local, no fleet)
- Dispatch a run; open the Jaeger UI and find a single trace spanning
  orchestrator dispatch → Temporal workflow → handler activity → daprd → actor host → S3 put.
- Kill an actor mid-`RunBatch`; confirm the failing span carries the error.

## Risks
- Context propagation across the Temporal boundary (interceptors handle it) and across the
  Dapr sidecar hop (traceparent headers) — verify spans actually link, not orphan.
- Sampling rate for high-fan-out recon runs (start low, e.g. 0.1, make it env-tunable).

## Comments
