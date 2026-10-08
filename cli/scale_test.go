package main

import (
	"context"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// A relative log dir must be REFUSED, not silently accepted: the Docker engine reinterprets a
// relative bind source as a NAMED VOLUME, so the scale would report success while the logs went
// somewhere the operator can't find — the exact failure this flag exists to prevent.
func TestRunScaleRejectsRelativeLogDir(t *testing.T) {
	_, err := runScale(context.Background(), scaleOpts{
		name: "crawl4ai", version: "0.3.0", replicas: 1, logDir: "logs/tonight",
	})
	if err == nil {
		t.Fatal("expected a relative logDir to be refused, got nil error")
	}
	if !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("error should explain the absolute-path requirement, got: %v", err)
	}
}

func TestWorkerLogBind(t *testing.T) {
	// Off by default — no logDir means no mount, and the entrypoint keeps its /tmp behaviour.
	if bind, host := workerLogBind("", "kontra-crawl4ai-0.3.0-0"); bind != "" || host != "" {
		t.Fatalf("no logDir should yield no bind, got bind=%q host=%q", bind, host)
	}

	// Each replica gets its OWN subdirectory, so N workers don't interleave into one file.
	bind, host := workerLogBind("/var/log/kontra", "kontra-crawl4ai-0.3.0-2")
	wantHost := "/var/log/kontra/kontra-crawl4ai-0.3.0-2"
	if host != wantHost {
		t.Fatalf("host path = %q, want %q", host, wantHost)
	}
	if bind != wantHost+":"+containerLogDir {
		t.Fatalf("bind = %q, want %q", bind, wantHost+":"+containerLogDir)
	}

	// The container-side path must match KONTRA_LOG_DIR in infra/worker-entrypoint.sh; if these
	// drift, the mount lands somewhere nothing writes to and every log file is empty.
	if containerLogDir != "/kontra/logs" {
		t.Fatalf("containerLogDir = %q — must stay in step with the entrypoint", containerLogDir)
	}
}

// TestWorkerMemoryCapIsNeverSilentlyRemoved pins the one property that matters here.
//
// Every control-plane service carries a hard mem_limit (docker-compose.yml's whole-controller
// budget); the actor containers carried none, so a runaway actor could take Temporal, the
// orchestrator and every other actor down with it — and the outage would land on the Controller
// rather than on the actor that caused it. The failure mode to guard is not "the number is wrong",
// it is "a typo in an env var quietly means unlimited".
func TestWorkerMemoryCapIsNeverSilentlyRemoved(t *testing.T) {
	const mib = 1024 * 1024
	def := int64(defaultWorkerMemoryMB) * mib

	if got := workerMemoryBytes(""); got != def {
		t.Fatalf("unset should give the default %d, got %d", def, got)
	}
	if got := workerMemoryBytes("  "); got != def {
		t.Fatalf("blank should give the default %d, got %d", def, got)
	}
	if got := workerMemoryBytes("1024"); got != 1024*mib {
		t.Fatalf("1024 should give %d bytes, got %d", 1024*mib, got)
	}
	if got := workerMemoryBytes(" 256 "); got != 256*mib {
		t.Fatalf("a padded value should still parse, got %d", got)
	}

	// The whole point: a value nobody can read falls back to the DEFAULT, never to unlimited.
	for _, bad := range []string{"512m", "half a gig", "-1", "1e9", "0x200"} {
		if got := workerMemoryBytes(bad); got != def {
			t.Fatalf("unreadable %q should fall back to the default %d, got %d", bad, def, got)
		}
	}

	// An explicit zero is the documented escape hatch — an actor that genuinely needs the host.
	if got := workerMemoryBytes("0"); got != 0 {
		t.Fatalf("an explicit 0 should disable the cap, got %d", got)
	}
}

// envCaptureDocker is a fakeScaleDocker that keeps the container Config, so a test can assert
// what a worker is actually STARTED WITH rather than what the code appears to intend.
type envCaptureDocker struct {
	fakeScaleDocker
	env []string
}

func (f *envCaptureDocker) ContainerCreate(ctx context.Context, cfg *container.Config, hc *container.HostConfig, nc *network.NetworkingConfig, pl *ocispec.Platform, name string) (container.CreateResponse, error) {
	f.env = cfg.Env
	return f.fakeScaleDocker.ContainerCreate(ctx, cfg, hc, nc, pl, name)
}

func envValue(env []string, key string) (string, bool) {
	for _, kv := range env {
		if name, value, ok := strings.Cut(kv, "="); ok && name == key {
			return value, true
		}
	}
	return "", false
}

// A worker is handed an OTLP endpoint ONLY when the operator configured one.
//
// This used to default to `jaeger:4317` — the compose service that ran the one collector. That
// service is gone (ADR 0031: tracing is an outbound endpoint, not something kontra runs), and a
// default outlives the thing it names: every managed worker on every host would have started an
// exporter aimed at a name that does not resolve, retrying it for the life of the container. The
// four endpoints beside it DO name components kontra runs, which is exactly why they keep their
// defaults and this one has none. Unset means unset, and the variable is ABSENT rather than
// empty, because infra/worker-entrypoint.sh tests it with `-n`.
func TestWorkerOTelEndpointIsNeverDefaulted(t *testing.T) {
	// AN EMPTY DATA DIRECTORY, so this test measures the compose defaults and not whatever
	// `kontra up` happens to have published on the machine running it. `resolveWorkerPlane` reads
	// the install's endpoints record when there is one, which is the point of it — and would
	// make this suite's answer depend on whether a developer had an install up.
	t.Setenv("KONTRA_DATA_DIR", t.TempDir())
	scale := func(t *testing.T, o scaleOpts) []string {
		t.Helper()
		fake := &envCaptureDocker{fakeScaleDocker: fakeScaleDocker{created: map[string]string{}}}
		old := newContainerDocker
		newContainerDocker = func() (containerAPI, error) { return fake, nil }
		t.Cleanup(func() { newContainerDocker = old })
		o.name, o.version, o.replicas = "echo", "0.1.0", 1
		if _, err := runScale(context.Background(), o); err != nil {
			t.Fatalf("runScale: %v", err)
		}
		return fake.env
	}

	t.Setenv("KONTRA_OTEL_ENDPOINT", "")
	env := scale(t, scaleOpts{})
	if v, present := envValue(env, "KONTRA_OTEL_ENDPOINT"); present {
		t.Fatalf("with no collector configured the worker must carry NO KONTRA_OTEL_ENDPOINT, got %q", v)
	}
	// The control-plane endpoints are unaffected — this is a missing default, not a missing wire.
	for _, k := range []string{"KONTRA_ADDRESS", "KONTRA_ORCHESTRATOR_URL", "KONTRA_S3_ENDPOINT", "KONTRA_REDIS_HOST"} {
		if v, present := envValue(env, k); !present || v == "" {
			t.Fatalf("%s must still be defaulted for a worker on the control-plane network, got %q (present=%v)", k, v, present)
		}
	}

	// Configured explicitly: the caller's collector, verbatim.
	if v, _ := envValue(scale(t, scaleOpts{otel: "otlp:4317"}), "KONTRA_OTEL_ENDPOINT"); v != "otlp:4317" {
		t.Fatalf("explicit collector = %q, want otlp:4317", v)
	}

	// Configured for the whole box: inherited from the operator's own environment, so a host that
	// DOES run a collector traces the whole chain without every call site naming it.
	t.Setenv("KONTRA_OTEL_ENDPOINT", "collector.internal:4317")
	if v, _ := envValue(scale(t, scaleOpts{}), "KONTRA_OTEL_ENDPOINT"); v != "collector.internal:4317" {
		t.Fatalf("inherited collector = %q, want collector.internal:4317", v)
	}
}
