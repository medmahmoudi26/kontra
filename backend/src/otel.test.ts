/**
 * Tracing is an outbound endpoint, not a service kontra runs (ADR 0031) — and the case that
 * actually bites is the one where the operator runs no collector at all.
 *
 * The failure this suite pins: the OTLP SDK's own fallback endpoint is `http://localhost:4317`,
 * so an exporter constructed unconditionally at module load holds a gRPC channel to an address
 * nobody listens on. That was harmless while compose ran a Jaeger on 4317; with Jaeger gone it is
 * a connection retried forever and, once any span is sampled, an error per export. So the
 * assertion is not "tracing is off" — it is that **nothing is constructed and nothing is said**.
 *
 * The exporter module is mocked in both directions: it lets the disabled case prove the
 * constructor is never reached, and it keeps the enabled case from opening a real socket.
 */

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { context, propagation, trace } from '@opentelemetry/api';

/** Every OTLPTraceExporter construction, whether or not an endpoint was configured. */
const constructed = vi.fn();

vi.mock('@opentelemetry/exporter-trace-otlp-grpc', () => ({
  OTLPTraceExporter: class {
    constructor(...args: unknown[]) {
      constructed(...args);
    }
    export(_spans: unknown, done: (r: unknown) => void) {
      done({ code: 0 });
    }
    async shutdown() {}
    async forceFlush() {}
  },
}));

/** The console methods an exporter would complain through. */
const CONSOLE_METHODS = ['debug', 'log', 'info', 'warn', 'error', 'trace'] as const;

function spyOnConsole() {
  return CONSOLE_METHODS.map((m) => vi.spyOn(console, m).mockImplementation(() => {}));
}

/** The OTel env this module reads, cleared so the ambient shell cannot flip a test's premise. */
const OTEL_ENV = [
  'OTEL_EXPORTER_OTLP_ENDPOINT',
  'OTEL_EXPORTER_OTLP_TRACES_ENDPOINT',
  'OTEL_SERVICE_NAME',
  'KONTRA_OTEL_SAMPLING',
];

describe('otel bootstrap', () => {
  beforeEach(() => {
    constructed.mockClear();
    for (const k of OTEL_ENV) vi.stubEnv(k, '');
    vi.resetModules();
  });

  afterEach(() => {
    // The OTel API keeps its provider/propagator/context-manager on globalThis, so a test that
    // registered one would otherwise decide the next test's answer. (The globals are shared even
    // across the module instances vi.resetModules() hands out — that is the point of the symbol.)
    trace.disable();
    context.disable();
    propagation.disable();
    vi.unstubAllEnvs();
    vi.restoreAllMocks();
  });

  it('exports nothing and says nothing when no endpoint is configured', async () => {
    const spies = spyOnConsole();
    const otel = await import('./otel');

    expect(otel.tracingEnabled).toBe(false);

    // No exporter, so no channel to localhost:4317 and nothing to retry.
    expect(otel.traceExporter()).toBeUndefined();
    expect(constructed).not.toHaveBeenCalled();

    // Idempotent no-ops, called the way the API's lazy client and the worker bootstrap call them.
    otel.startTracing();
    otel.startTracing();
    await otel.shutdownTracing();
    expect(constructed).not.toHaveBeenCalled();

    // And still no provider installed — a span taken from the global tracer is not recorded,
    // so nothing is buffered, batched or dropped with a complaint.
    const span = trace.getTracer('kontra-test').startSpan('probe');
    expect(span.isRecording()).toBe(false);
    span.end();

    // The whole point: not one line on any console stream.
    for (const spy of spies) expect(spy).not.toHaveBeenCalled();
  });

  it('treats an empty endpoint as unset, because that is how compose passes an unset one', async () => {
    // `OTEL_EXPORTER_OTLP_ENDPOINT: ${VAR:-}` in a compose file yields an empty string, not an
    // absent variable. A truthiness gate that read `!== undefined` would turn that into tracing.
    vi.stubEnv('OTEL_EXPORTER_OTLP_ENDPOINT', '');
    vi.stubEnv('OTEL_EXPORTER_OTLP_TRACES_ENDPOINT', '');
    const otel = await import('./otel');
    expect(otel.tracingEnabled).toBe(false);
    expect(constructed).not.toHaveBeenCalled();
  });

  it('builds exactly one exporter when an endpoint is configured', async () => {
    vi.stubEnv('OTEL_EXPORTER_OTLP_ENDPOINT', 'http://otlp.example:4317');
    const otel = await import('./otel');

    expect(otel.tracingEnabled).toBe(true);
    expect(constructed).not.toHaveBeenCalled(); // lazy: not at import time

    otel.startTracing();
    otel.startTracing(); // idempotent — one provider, one exporter, however many callers
    expect(constructed).toHaveBeenCalledTimes(1);
    expect(otel.traceExporter()).toBeDefined();

    // Registered globally, so the Temporal interceptors and anything else on this process share
    // one provider and one trace id rather than each opening their own.
    const span = trace.getTracer('kontra-test').startSpan('probe');
    expect(span.isRecording()).toBe(true);
    expect(span.spanContext().traceId).toMatch(/^[0-9a-f]{32}$/);
    span.end();

    await otel.shutdownTracing();
  });

  it('honours the signal-specific traces endpoint on its own', async () => {
    // The exporter prefers OTEL_EXPORTER_OTLP_TRACES_ENDPOINT over the general one, so a gate
    // that read only the general one would refuse to trace for an operator who had configured
    // it correctly — the endpoint set, and nothing ever exported.
    vi.stubEnv('OTEL_EXPORTER_OTLP_TRACES_ENDPOINT', 'http://otlp.example:4317/v1/traces');
    const otel = await import('./otel');
    expect(otel.tracingEnabled).toBe(true);
    otel.startTracing();
    expect(constructed).toHaveBeenCalledTimes(1);
    await otel.shutdownTracing();
  });
});
