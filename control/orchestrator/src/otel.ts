/**
 * OpenTelemetry bootstrap for the orchestrator (roadmap platform-x100 #03).
 *
 * The orchestrator is no longer the root of the distributed trace. It was, while it started
 * every run: the client interceptor opened the root span and the interpreter's activities and
 * Nexus dispatches hung off it. A **Run** is now one execution of a caller's workflow (ADR 0023
 * §12), so the caller's own worker opens that span and this process contributes the spans of
 * what it still does — materialization and provisioning. This module owns the process-level
 * tracer provider + the shared exporter/resource those wirings reuse.
 *
 * TRACING IS AN OUTBOUND ENDPOINT, NOT A SERVICE (ADR 0031). kontra runs no collector of its
 * own; it exports to whatever the operator points it at. Zero-config from the standard env:
 * OTEL_EXPORTER_OTLP_ENDPOINT (or the signal-specific OTEL_EXPORTER_OTLP_TRACES_ENDPOINT) is
 * the on/off switch, OTEL_SERVICE_NAME names the service (default kontra-orchestrator), and
 * KONTRA_OTEL_SAMPLING sets the head sample rate.
 *
 * WITH NEITHER ENDPOINT SET NOTHING HERE IS EVEN CONSTRUCTED. That is the load-bearing part,
 * not a nicety: the OTLP SDK's own fallback endpoint is `http://localhost:4317`, so an eagerly
 * built exporter on a collector-less box holds a gRPC channel that retries an address nobody
 * listens on. The exporter is therefore built lazily, inside {@link startTracing}, and only
 * when an endpoint is configured — so a collector-less deploy is byte-for-byte unchanged and,
 * more importantly, silent.
 */

import {
  BatchSpanProcessor,
  NodeTracerProvider,
  ParentBasedSampler,
  TraceIdRatioBasedSampler,
} from '@opentelemetry/sdk-trace-node';
import { OTLPTraceExporter } from '@opentelemetry/exporter-trace-otlp-grpc';
import { Resource } from '@opentelemetry/resources';
import { ATTR_SERVICE_NAME } from '@opentelemetry/semantic-conventions';

/** Tracing is opt-in via the standard OTLP endpoint env — the single on/off signal the
 * client/worker wirings gate on, so a collector-less deploy is byte-for-byte unchanged.
 *
 * BOTH standard variables count. The exporter itself honours the signal-specific
 * OTEL_EXPORTER_OTLP_TRACES_ENDPOINT over the general one, so a gate that read only the general
 * one would silently disable tracing for an operator who configured traces the specific way —
 * the endpoint would be set, correctly, and nothing would ever export. */
export const tracingEnabled = Boolean(
  process.env.OTEL_EXPORTER_OTLP_TRACES_ENDPOINT || process.env.OTEL_EXPORTER_OTLP_ENDPOINT,
);

const serviceName = process.env.OTEL_SERVICE_NAME ?? 'kontra-orchestrator';

/** service.name resource — shared by the process provider AND any span sink that exports on
 * this process's behalf, so every span carries the same service identity in the backend. */
export const otelResource = new Resource({ [ATTR_SERVICE_NAME]: serviceName });

let exporter: OTLPTraceExporter | undefined;

/**
 * The one OTLP/gRPC exporter (reads OTEL_EXPORTER_OTLP_ENDPOINT /
 * OTEL_EXPORTER_OTLP_TRACES_ENDPOINT), built on first use and shared by every span processor in
 * this process. Returns `undefined` when tracing is off — see the module header: constructing
 * one with no endpoint configured is what makes a collector-less box noisy.
 */
export function traceExporter(): OTLPTraceExporter | undefined {
  if (!tracingEnabled) return undefined;
  exporter ??= new OTLPTraceExporter();
  return exporter;
}

/** Head sample rate from KONTRA_OTEL_SAMPLING (0.0–1.0); default 1.0 (capture all — the
 * local-verify default). Applied at the ROOT only (ParentBased), so once the root run is
 * sampled, every downstream Temporal span in that trace is kept; keep this the SAME across the
 * orchestrator, the handler and the actor so parents and children agree — a kept parent with
 * dropped children reads as a missing hop, not as a sampling decision. */
function samplingRatio(): number {
  const raw = process.env.KONTRA_OTEL_SAMPLING;
  if (!raw) return 1;
  const n = Number(raw);
  if (!Number.isFinite(n) || n < 0) return 1;
  return n > 1 ? 1 : n;
}

let provider: NodeTracerProvider | undefined;

/**
 * Start + register the global tracer provider ONCE per process. register() also installs
 * the Node async-context manager and the W3C trace-context propagator globally. No-op when
 * tracing is disabled or already started (idempotent — safe to call from both the API's
 * lazy client and the worker bootstrap).
 */
export function startTracing(): void {
  if (!tracingEnabled || provider) return;
  const exp = traceExporter();
  if (!exp) return;
  provider = new NodeTracerProvider({
    resource: otelResource,
    sampler: new ParentBasedSampler({ root: new TraceIdRatioBasedSampler(samplingRatio()) }),
    spanProcessors: [new BatchSpanProcessor(exp)],
  });
  provider.register();
}

/** Best-effort flush + shutdown of the process provider (for a graceful drain on exit). */
export async function shutdownTracing(): Promise<void> {
  if (!provider) return;
  await provider.shutdown().catch(() => undefined);
  provider = undefined;
  // The provider's shutdown drained and closed the exporter with it; drop the reference so a
  // later startTracing() builds a live one rather than exporting into a closed channel.
  exporter = undefined;
}
