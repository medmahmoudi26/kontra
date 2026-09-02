// OpenTelemetry tracing bootstrap for the handler (roadmap platform-x100 #03).
//
// The handler is the Temporal Nexus/workflow worker per actor. This wires a native
// OTLP tracer provider so the spans the temporal contrib interceptor creates
// (Nexus StartOperation, RunWorkflow, RunBatch, …) are exported, AND sets the OTel GLOBAL
// W3C propagator so trace context is carried in the standard format across every hop.
//
// Every hop is a Temporal hop — client start, workflow, activity, and the Nexus boundary — so
// the interceptor covers the whole chain and there is no manual `traceparent` injection anywhere
// in this codebase.
//
// TRACING IS AN OUTBOUND ENDPOINT, NOT A SERVICE (ADR 0031). kontra runs no collector; this
// exports to whatever the operator points it at. Zero-config from the standard env: the OTLP
// exporter reads OTEL_EXPORTER_OTLP_ENDPOINT (e.g. http://otel-collector.internal:4317), or the
// signal-specific OTEL_EXPORTER_OTLP_TRACES_ENDPOINT; the worker entrypoint exports one of them
// from KONTRA_OTEL_ENDPOINT, plus KONTRA_OTEL_SAMPLING.
// When NEITHER is set the whole thing is a no-op (a self-contained `docker run` worker with no
// collector runs exactly as before — no tracing, no exporter, no connection errors).
package main

import (
	"context"
	"fmt"
	"os"
	"strconv"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.27.0"
)

// initTracing builds an OTLP/gRPC tracer provider (service.name=serviceName) from
// OTEL_EXPORTER_OTLP_ENDPOINT, installs it + the W3C global propagator, and returns a
// shutdown that flushes on exit. It is a NO-OP (nil-safe shutdown, no error) when NO endpoint
// is configured, so tracing is strictly opt-in via env — no exporter is constructed at all,
// which is what keeps a collector-less worker from retrying the SDK's default localhost:4317.
//
// BOTH standard variables are honoured: the exporter prefers the signal-specific
// OTEL_EXPORTER_OTLP_TRACES_ENDPOINT over the general one, so a gate reading only the general
// one would refuse to trace for an operator who configured traces the specific way.
//
// The sampler is ParentBased(TraceIDRatioBased(KONTRA_OTEL_SAMPLING)): the handler is
// always DOWNSTREAM of the orchestrator root, so ParentBased makes it honor the root's
// head-sampling decision (kept traces stay whole, dropped ones stay fully dropped) — the
// ratio only applies to any span the handler would ever root itself.
func initTracing(ctx context.Context, serviceName string) (func(context.Context) error, error) {
	noop := func(context.Context) error { return nil }
	if !tracingEnabled() {
		return noop, nil
	}

	// otlptracegrpc reads OTEL_EXPORTER_OTLP_ENDPOINT (an http://host:port is insecure);
	// New does NOT dial synchronously, so a momentarily-down collector never blocks boot.
	exp, err := otlptracegrpc.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("otlp trace exporter: %w", err)
	}

	// NewSchemaless (no schema URL) so the merge with resource.Default() — whose schema URL
	// is the SDK's own semconv version — never fails with a "conflicting Schema URL" error
	// (which would silently disable handler tracing). service.name from us wins over any
	// OTEL_SERVICE_NAME the entrypoint exported for the actor host.
	res, err := resource.Merge(
		resource.Default(),
		resource.NewSchemaless(semconv.ServiceName(serviceName)),
	)
	if err != nil {
		return nil, fmt.Errorf("otel resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(samplingRatio()))),
	)
	otel.SetTracerProvider(tp)
	// GLOBAL propagator, set explicitly because the default one is a NO-OP: anything reading or
	// writing trace context through the global (rather than through the temporal contrib
	// interceptor, which carries its own for Temporal/Nexus headers) would silently inject an
	// empty traceparent and start a new root trace. W3C tracecontext + baggage.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))
	return tp.Shutdown, nil
}

// tracingEnabled is the single on/off signal: an OTLP endpoint the operator configured, in
// either of the two standard variables. Unset means UNSET — nothing defaults it, because a
// default endpoint is an address nobody listens on the moment the operator runs no collector.
func tracingEnabled() bool {
	return os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != "" ||
		os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != ""
}

// samplingRatio reads KONTRA_OTEL_SAMPLING (0.0–1.0); defaults to 1.0 (capture all —
// the local-verify default) and clamps out-of-range/garbage. Fleets dial it down (e.g. 0.1) for
// high-fan-out recon runs; keep it the SAME on every process of a run (handler and actor both
// read it) so a parent and its children agree — a kept parent with dropped children is a broken
// trace, and it reads as a missing hop rather than as a sampling decision.
func samplingRatio() float64 {
	v := os.Getenv("KONTRA_OTEL_SAMPLING")
	if v == "" {
		return 1.0
	}
	r, err := strconv.ParseFloat(v, 64)
	if err != nil || r < 0 {
		return 1.0
	}
	if r > 1 {
		return 1.0
	}
	return r
}
