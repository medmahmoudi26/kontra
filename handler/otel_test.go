package main

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// With no collector configured the handler must build NOTHING — not a quiet exporter, not one.
//
// Tracing is an outbound endpoint, not a service kontra runs (ADR 0031), and the OTLP SDK's own
// fallback endpoint is localhost:4317. An exporter constructed anyway on a box with no collector
// is a gRPC channel retrying an address nobody listens on, plus an error per export once anything
// is sampled — which is precisely what a self-contained `docker run` worker would have inherited
// when the compose `jaeger` service went away. So the assertion is silence, spelled out as: no SDK
// provider installed, no global propagator, and a span that does not record.
func TestInitTracingBuildsNothingWithoutAnEndpoint(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")

	shutdown, err := initTracing(context.Background(), "kontra-handler-test")
	if err != nil {
		t.Fatalf("initTracing with no endpoint must not error: %v", err)
	}
	if shutdown == nil {
		t.Fatal("initTracing must return a callable shutdown, never nil — main defers it unconditionally")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("the no-op shutdown must not error: %v", err)
	}

	if tp, isSDK := otel.GetTracerProvider().(*sdktrace.TracerProvider); isSDK {
		t.Fatalf("no endpoint configured, yet an SDK tracer provider was installed globally: %T", tp)
	}

	// The default global propagator is a no-op, and it must stay one: nothing to inject means
	// nothing downstream reads a half-written trace context.
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(context.Background(), carrier)
	if len(carrier) != 0 {
		t.Fatalf("a propagator was installed with tracing off: %v", carrier)
	}

	_, span := otel.Tracer("kontra-handler-test").Start(context.Background(), "probe")
	if span.IsRecording() {
		t.Fatal("a span is recording with no exporter behind it — something is buffering spans nobody will collect")
	}
	span.End()
}

// The on/off signal reads BOTH standard variables. The exporter prefers the signal-specific
// OTEL_EXPORTER_OTLP_TRACES_ENDPOINT over the general one, so a gate that read only the general
// one would refuse to trace for an operator who had configured traces correctly — endpoint set,
// nothing ever exported. And an EMPTY value is unset, because that is what a compose file's
// pass-through yields on a host that set nothing.
func TestTracingEnabledReadsBothStandardEndpoints(t *testing.T) {
	for _, c := range []struct {
		name, general, traces string
		want                  bool
	}{
		{"neither", "", "", false},
		{"general only", "http://collector.internal:4317", "", true},
		{"traces only", "", "http://collector.internal:4317/v1/traces", true},
		{"both", "http://a:4317", "http://b:4318/v1/traces", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", c.general)
			t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", c.traces)
			if got := tracingEnabled(); got != c.want {
				t.Fatalf("tracingEnabled() = %v, want %v (general=%q traces=%q)", got, c.want, c.general, c.traces)
			}
		})
	}
}
