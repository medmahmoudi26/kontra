// Command handler is the Go Temporal Nexus handler, run ONE-PER-ACTOR (configured by env).
// It serves the kontra.actor:run Nexus op, owns the backing workflow, and schedules each batch
// onto the actor's own task queue as a RunBatch activity (ADR 0018). It is fully decoupled from
// actorkit — it never imports it and never links against it; the only coupling is the JSON wire
// and the queue name, both derived independently on each side.
package main

import (
	"context"
	"log"
	"os"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"

	"github.com/medmahmoudi26/kontra/sdk/go/temporaltls"
	"go.temporal.io/sdk/contrib/opentelemetry"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/interceptor"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/medmahmoudi26/kontra/runtime/go/codec"
	kontrav1 "github.com/medmahmoudi26/kontra/runtime/handler/_gen/kontra/v1"
	"github.com/medmahmoudi26/kontra/runtime/handler/internal/cas"
	"github.com/medmahmoudi26/kontra/runtime/handler/internal/identity"
	"github.com/medmahmoudi26/kontra/runtime/handler/internal/objectstore"
)

func main() {
	ctx := context.Background()

	name := os.Getenv("KONTRA_ACTOR_NAME")
	version := os.Getenv("KONTRA_ACTOR_VERSION")
	if name == "" {
		log.Fatal("KONTRA_ACTOR_NAME is required")
	}

	// Object store + claim-check codec (nil store => passthrough).
	store, _, err := objectstore.FromEnv(ctx)
	if err != nil {
		log.Fatalf("object store: %v", err)
	}
	cdc := codec.New(objectstore.AsCodecStore(store), codec.ThresholdFromEnv())
	dc := converter.NewCodecDataConverter(converter.GetDefaultDataConverter(), cdc)

	// OTel tracing (roadmap platform-x100 #03): init the tracer provider + W3C global
	// propagator BEFORE building the interceptor (the contrib defaults its tracer to the
	// OTel GLOBAL provider). No-op when OTEL_EXPORTER_OTLP_ENDPOINT is unset.
	shutdownTracing, err := initTracing(ctx, "kontra-handler-"+name)
	if err != nil {
		log.Printf("otel tracing init failed (continuing WITHOUT tracing): %v", err)
		shutdownTracing = func(context.Context) error { return nil }
	}
	defer func() { _ = shutdownTracing(context.Background()) }()

	// The native Temporal OTel interceptor: it creates spans for every Temporal hop and
	// carries trace context across them (client start, workflow, activity, AND the Nexus
	// boundary — reading the traceparent the orchestrator's interceptor wrote into the
	// nexus.Header). Set on the CLIENT: the SDK extracts its worker-side and applies it to
	// every worker created from this client, so the workflow and blob activities inherit it.
	tracingInterceptor, err := opentelemetry.NewTracingInterceptor(opentelemetry.TracerOptions{})
	if err != nil {
		log.Fatalf("otel interceptor: %v", err)
	}

	namespace := getenv("KONTRA_NAMESPACE", "default")
	address := getenv("KONTRA_ADDRESS", "localhost:7233")
	conn, err := temporaltls.ConnectionOptions(nil)
	if err != nil {
		log.Fatalf("temporal TLS: %v", err)
	}
	log.Printf("temporal: %s", temporaltls.Describe(address, conn))
	c, err := client.Dial(client.Options{
		HostPort:          address,
		Namespace:         namespace,
		DataConverter:     dc,
		Interceptors:      []interceptor.ClientInterceptor{tracingInterceptor},
		ConnectionOptions: conn,
		// Stated rather than defaulted — see identity/worker.go. Field three is the role, because
		// a client polls no queue.
		Identity: identity.WorkerIdentity(getenv("KONTRA_WORKER_ROLE", "handler")),
	})
	if err != nil {
		log.Fatalf("temporal dial: %v", err)
	}
	defer c.Close()

	var blob *cas.CAS
	if store != nil {
		blob = cas.New(store)
	}
	a := &Activities{blob: blob}

	queue := identity.SharedQueue(name, version)
	w := worker.New(c, queue, worker.Options{
		Identity: identity.WorkerIdentity(queue),
		BuildID:  identity.BuildID(),
	})

	w.RegisterWorkflowWithOptions(a.RunWorkflow, workflow.RegisterOptions{Name: kontrav1.RunWorkflowName})
	w.RegisterActivityWithOptions(a.StoreBlob, activity.RegisterOptions{Name: identity.StoreBlobActivity})
	w.RegisterActivityWithOptions(a.FetchBlob, activity.RegisterOptions{Name: identity.FetchBlobActivity})
	// RunBatch and Close are NOT registered here. ADR 0018: the actor process is itself a
	// Temporal activity worker and polls "{queue}-sessions" directly, so this handler owns the
	// WORKFLOW and the blob activities and nothing else. Temporal splits workflow and activity
	// across languages by design, which is what made the sidecar removable at all.
	//
	// The live-session cap moved with them: KONTRA_MAX_PARALLEL_SESSIONS is read on that side.
	// It is a count of live Sessions there, not an activity-slot count (ADR 0023 §6) — a
	// Session now outlives the activity that opened it, so slots stopped bounding it.

	svc, err := a.newNexusService(queue)
	if err != nil {
		log.Fatalf("nexus service: %v", err)
	}
	w.RegisterNexusService(svc)

	// A FALLBACK, NOT THE OWNER — `kontra actor register` creates this now, and forgetting the
	// registration removes it. Kept for a worker started outside that control plane (a fleet
	// Machine, a container run by hand), because a worker polling a queue no endpoint addresses is
	// the silent failure the registration change exists to remove. See nexus.go for the history.
	endpoint, err := ensureNexusEndpoint(ctx, c, namespace, name, version)
	if err != nil {
		log.Printf("ensure nexus endpoint %q: %v (non-fatal — `kontra actor register` is what owns it)", endpoint, err)
	}

	log.Printf("serving %s@%s: workflow on %q, actor activities on %q (the actor's own worker), endpoint %s",
		name, version, queue, identity.SessionsQueue(name, version), endpoint)

	// One worker now: the workflow plus the two blob activities. The actor's own process is
	// the other half, on the sessions queue.
	if err := w.Start(); err != nil {
		log.Fatalf("worker: %v", err)
	}
	defer w.Stop()
	<-worker.InterruptCh()
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
