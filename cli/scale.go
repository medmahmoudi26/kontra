// scale.go — `scale_actor`: run/scale an actor's WORKERS via the Docker Engine API.
//
// deploy builds+pushes a self-contained worker IMAGE; scale turns that image into live
// worker CONTAINERS (each one a Temporal poller on the actor's queue). This is the "run"
// and "scale" half of deploy→run→scale: replicas=1 runs one worker, N scales out, 0 stops
// them all. Managed containers carry a `kontra.actor=<name>@<version>` label so scale can
// find exactly the workers it owns. Same engine-API boundary as deploy — never a shell-out.
package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	docker "github.com/docker/docker/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/medmahmoudi26/kontra/cli/appliance"
	"github.com/medmahmoudi26/kontra/cli/internal/ociref"
)

// controlPlaneNetwork is the compose network the control plane runs on; a same-host worker
// attaches to it so `temporal:7233` / `orchestrator-api:8088` / `seaweed:8333` DNS resolves.
const controlPlaneNetwork = "kontra"

// managedLabel marks a container scale owns; its value is "<name>@<version>".
const managedLabel = "kontra.actor"

// containerAPI is the slice of the Docker client scale needs — a tiny interface so tests
// fake it and never touch a daemon (mirrors deploy's imageAPI).
type containerAPI interface {
	ImageList(ctx context.Context, options image.ListOptions) ([]image.Summary, error)
	ImagePull(ctx context.Context, ref string, options image.PullOptions) (io.ReadCloser, error)
	ContainerList(ctx context.Context, options container.ListOptions) ([]types.Container, error)
	ContainerCreate(ctx context.Context, config *container.Config, hostConfig *container.HostConfig, networkingConfig *network.NetworkingConfig, platform *ocispec.Platform, containerName string) (container.CreateResponse, error)
	ContainerStart(ctx context.Context, containerID string, options container.StartOptions) error
	ContainerRemove(ctx context.Context, containerID string, options container.RemoveOptions) error
}

var newContainerDocker = func() (containerAPI, error) {
	return docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
}

// scaleOpts is the flag-free run/scale request the MCP tool drives.
type scaleOpts struct {
	name, version string
	replicas      int
	network       string // "" → controlPlaneNetwork
	image         string // "" → resolve (local kontra/<name>-worker:<ver>, else registry)
	// registry is the address to pull from; "" means registryAddress resolves it — the running
	// appliance's own, which is the SAME resolution `kontra deploy` pushed to.
	registry string
	// controller endpoints baked into each worker's env; "" → the compose service DNS
	// defaults below (the local control plane).
	address, orchestrator, s3 string
	// redis is the SHARED state store every worker points at — all three tiers (ADR 0015/0018).
	// "" → the `redis:6379` compose service on the control-plane network. It is REQUIRED: a
	// worker bundles no Redis of its own, so without it the actor's durable state has nowhere
	// to go.
	redis string
	// otel is the OTLP tracing collector (roadmap platform-x100 #03) as a bare host:port —
	// a collector the OPERATOR runs, because kontra runs none (ADR 0031). "" falls back to
	// this process's own KONTRA_OTEL_ENDPOINT and, failing that, stays UNSET: the worker is
	// then started with no OTLP endpoint at all and exports nothing. It used to default to
	// `jaeger:4317`, a compose service that no longer exists — every managed worker would
	// export to a name that does not resolve. Set it to turn on the worker's tracing — the
	// handler acts on it; the actor host installs no tracer — see infra/worker-entrypoint.sh.
	otel string
	// logDir turns on DURABLE logging: an ABSOLUTE host directory into which each worker's
	// logs are bind-mounted at /kontra/logs. Without it a worker's logs live only inside the
	// container and die with it (ContainerRemove below force-removes on every scale-down), so
	// a long run leaves nothing to inspect. Per worker we bind <logDir>/<container-name>.
	logDir string
}

// scaleResult is the structured outcome — the current managed-worker fleet after scaling.
type scaleResult struct {
	Actor   string   `json:"actor"`
	Version string   `json:"version"`
	Image   string   `json:"image"`
	Network string   `json:"network"`
	Desired int      `json:"desired"`
	Running int      `json:"running"`
	Started []string `json:"started,omitempty"` // container names newly started this call
	Stopped []string `json:"stopped,omitempty"` // container names removed this call
	Workers []string `json:"workers"`           // all managed worker container names now present
	LogDir  string   `json:"logDir,omitempty"`  // host dir holding each worker's durable logs
	// ControlPlane is the Temporal address these workers were told to poll, and Source says
	// where that address came from. Both are reported rather than assumed because a worker
	// pointed at the wrong control plane is INDISTINGUISHABLE from a healthy one at every other
	// surface: it runs, it restarts, and it polls a queue nobody dispatches to.
	ControlPlane string `json:"controlPlane"`
	Source       string `json:"source"`
}

// runScale reconciles the actor's managed worker containers toward `replicas`: it starts
// new ones (create+start on the control-plane network, wired to the controller) or removes
// excess, and returns the resulting fleet.
func runScale(ctx context.Context, o scaleOpts) (*scaleResult, error) {
	if o.name == "" {
		return nil, errors.New("scale needs an actor name")
	}
	if o.version == "" {
		return nil, errors.New("scale needs an actor version")
	}
	if o.replicas < 0 {
		return nil, fmt.Errorf("replicas must be >= 0, got %d", o.replicas)
	}
	// A bind mount source must be an absolute HOST path — the engine silently reinterprets a
	// relative one as a named volume, which would look like it worked and leave the logs
	// nowhere findable. Refuse instead.
	if o.logDir != "" && !filepath.IsAbs(o.logDir) {
		return nil, fmt.Errorf("log dir %q must be an absolute path (it is a host bind mount)", o.logDir)
	}
	// WHICH CONTROL PLANE THESE WORKERS DIAL, resolved once, before a container is created.
	// Before the appliance there was one answer and it was compose's DNS; there are two now, and
	// picking the wrong one produces a container that runs, polls a name that does not resolve,
	// and counts as a live replica. See workerPlane.
	plane, err := resolveWorkerPlane(o)
	if err != nil {
		return nil, err
	}
	net := plane.network
	d, err := newContainerDocker()
	if err != nil {
		return nil, fmt.Errorf("docker engine unreachable: %w", err)
	}

	image, needPull, reg, err := resolveWorkerImage(ctx, d, o)
	if err != nil {
		return nil, err
	}
	label := o.name + "@" + o.version

	existing, err := listManaged(ctx, d, label)
	if err != nil {
		return nil, err
	}

	// A registry image the daemon doesn't have won't auto-pull on ContainerCreate — pull it
	// before we try to start new workers (skip when scaling down/to-zero: nothing to start).
	if needPull && o.replicas > len(existing) {
		if err := pullImage(ctx, d, image); err != nil {
			return nil, pullFailure(reg, o.name, o.version, image, err)
		}
	}

	res := &scaleResult{
		Actor: o.name, Version: o.version, Image: image, Network: net, Desired: o.replicas, LogDir: o.logDir,
		ControlPlane: plane.address, Source: plane.source,
	}

	// Scale DOWN: remove the newest-indexed extras first (stable: keep the lowest indices).
	for len(existing) > o.replicas {
		last := existing[len(existing)-1]
		if err := d.ContainerRemove(ctx, last.id, container.RemoveOptions{Force: true}); err != nil {
			return nil, fmt.Errorf("removing worker %s: %w", last.name, err)
		}
		res.Stopped = append(res.Stopped, last.name)
		existing = existing[:len(existing)-1]
	}

	// Scale UP: fill the lowest unused indices so names stay stable across calls.
	used := map[int]bool{}
	for _, c := range existing {
		used[c.index] = true
	}
	for i := 0; len(existing) < o.replicas; i++ {
		if used[i] {
			continue
		}
		name := workerName(o.name, o.version, i)
		id, err := startWorker(ctx, d, name, image, label, plane, o)
		if err != nil {
			return nil, fmt.Errorf("starting worker %s: %w", name, err)
		}
		res.Started = append(res.Started, name)
		existing = append(existing, managedContainer{id: id, name: name, index: i})
		used[i] = true
	}

	sort.Slice(existing, func(i, j int) bool { return existing[i].index < existing[j].index })
	res.Running = len(existing)
	for _, c := range existing {
		res.Workers = append(res.Workers, c.name)
	}
	return res, nil
}

// resolveWorkerImage picks the worker image and whether it must be PULLED first: an
// explicit override (assume the caller made it available), else the local self-contained
// image `kontra/<name>-worker:<ver>` if the daemon has it, else the pushed registry image
// `<registry>/<name>:<ver>` (which the daemon likely lacks → needs a pull).
func resolveWorkerImage(ctx context.Context, d containerAPI, o scaleOpts) (img string, needPull bool, reg string, err error) {
	// registryAddress, not `o.registry else defaultRegistry`: this is the SAME resolution
	// `kontra deploy` used to tag the image it pushed, in one function, so the two cannot drift.
	// See its comment for why that mattered enough to have a function.
	reg = registryAddress(o.registry)
	if o.image != "" {
		return o.image, false, reg, nil
	}
	local := fmt.Sprintf("kontra/%s-worker:%s", o.name, o.version)
	sums, err := d.ImageList(ctx, image.ListOptions{
		Filters: filters.NewArgs(filters.Arg("reference", local)),
	})
	if err != nil {
		return "", false, reg, err
	}
	if len(sums) > 0 {
		return local, false, reg, nil
	}
	return fmt.Sprintf("%s/%s:%s", reg, o.name, o.version), true, reg, nil
}

// pullFailure turns Docker's report of a failed pull into a sentence about the REGISTRY, by
// asking the registry the question the daemon cannot answer for us.
//
// IT RUNS AFTER THE PULL AND NOT BEFORE IT, and that is a decision. A gate before the pull would
// also refuse the cases a gate cannot see: a registry that needs the credentials the DAEMON holds
// and this process does not, or one that does not answer an anonymous HEAD. Diagnosing afterwards
// costs one HTTP call on a path that is already failing and costs nothing at all on the happy one.
//
// The three branches are three different problems, and Docker reports all of them as some variant
// of `no such image` or a bare connection refused — which sends an operator to look at the daemon.
// The third is the one worth the code: the registry HAS the image and the daemon still could not
// get it, which means the daemon and this process resolve the same string to different places.
//
// ═══ THERE IS A FOURTH, AND IT IS NOT A REGISTRY PROBLEM AT ALL ═══
//
// This function used to give ONE message for two opposite truths, measured in
// `.scratch/warden/issues/15-*` against the live registry on this checkout:
//
//	a/b     "does not hold a/b:1.0.0 … Either it was never deployed…"   TRUE — deploying fixes it
//	café    IDENTICAL                                                   FALSE — nothing fixes it
//
// `a/b` is a legal repository path whose image really was never pushed. `café` can never be an
// image at all — and every remedy the first branch offers, re-running `kontra deploy` or pointing
// both sides at one registry, is unreachable for it. The only line that distinguished them was
// docker's own `invalid reference format`, buried fourth as an aside. **An operator with a `café`
// actor was told to deploy, forever.**
//
// So the grammar is asked FIRST, and it is asked of `img` — the exact string the daemon was handed.
// That matters more than it looks: `resolveWorkerImage` produces TWO different references for one
// Actor, the local `kontra/<name>-worker:<version>` and the remote `<registry>/<name>:<version>`,
// differing in both prefix and suffix, so a check aimed at the actor's NAME or at either form alone
// would pass and let the pull die later on docker's wording — the same bug moved one step earlier.
// The answer itself is cli/internal/ociref/ociref.go, shared with `kontra build --push` and the podman driver.
//
// IT IS STILL AFTER THE PULL, and the paragraph above about diagnosing afterwards still holds: this
// costs one string parse on a path that has already failed. A gate before the pull would also have
// to be right about the cases a gate cannot see.
func pullFailure(reg, name, version, img string, cause error) error {
	if err := ociref.Check(img); err != nil {
		return fmt.Errorf("cannot pull %s.\n%w\n"+
			"  This is not about the registry at %s and no deploy reaches it: `kontra serve` (process\n"+
			"  mode) and every queue derivation take the Actor's name verbatim, so %s@%s serves fine and\n"+
			"  only its ARTIFACT is unnameable. Rename it in actor.json, or serve it without --mode docker.\n"+
			"  docker said: %v", img, err, reg, name, version, cause)
	}
	_, err := registryManifestDigest(reg, name, version)
	switch {
	case errors.Is(err, errNotInRegistry):
		return fmt.Errorf("cannot pull %s: the registry at %s answered, and does not hold %s:%s.\n"+
			"  Either it was never deployed, or `kontra deploy` pushed it to a DIFFERENT address than this one.\n"+
			"  Deploy it (`kontra deploy --actor <dir>`), or point both at one registry with --registry / KONTRA_REGISTRY.\n"+
			"  docker said: %v", img, reg, name, version, cause)
	case err != nil:
		return fmt.Errorf("cannot pull %s: nothing answers as a registry at %s from here (%v).\n"+
			"  The worker image comes from there, and an image the daemon does not already hold is NOT fetched "+
			"on container create — so this is the failure, not a later one.\n  docker said: %v",
			img, reg, err, cause)
	}
	return fmt.Errorf("cannot pull %s, although the registry at %s does hold %s:%s.\n"+
		"  Your Docker daemon resolves %q to something this process does not — a remote DOCKER_HOST, a container "+
		"network, or an insecure-registry rule the daemon is missing.\n  docker said: %v",
		img, reg, name, version, reg, cause)
}

// pullImage pulls a worker image (drains the engine's progress stream, surfacing an in-stream
// error). base64("{}") is the canonical "no credentials" for a local/insecure registry.
func pullImage(ctx context.Context, d containerAPI, ref string) error {
	rc, err := d.ImagePull(ctx, ref, image.PullOptions{
		RegistryAuth: base64.URLEncoding.EncodeToString([]byte("{}")),
	})
	if err != nil {
		return err
	}
	defer rc.Close()
	return streamDockerOutput(io.Discard, rc)
}

type managedContainer struct {
	id, name string
	index    int
}

// listManaged returns this actor's managed worker containers (any state), sorted by index.
func listManaged(ctx context.Context, d containerAPI, label string) ([]managedContainer, error) {
	cs, err := d.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("label", managedLabel+"="+label)),
	})
	if err != nil {
		return nil, err
	}
	out := make([]managedContainer, 0, len(cs))
	for _, c := range cs {
		name := ""
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		out = append(out, managedContainer{id: c.ID, name: name, index: workerIndex(name)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].index < out[j].index })
	return out, nil
}

// workerLogBind returns the bind spec mounting this worker's own host log directory at
// /kontra/logs (where the entrypoint's KONTRA_LOG_DIR points), plus that host path. Each
// worker gets its OWN subdirectory so N replicas don't interleave into one file. Returns
// "" when durable logging is off. The engine creates the host dir if it doesn't exist.
func workerLogBind(logDir, workerName string) (bind, hostPath string) {
	if logDir == "" {
		return "", ""
	}
	hostPath = filepath.Join(logDir, workerName)
	return hostPath + ":" + containerLogDir, hostPath
}

// containerLogDir is the in-container path the entrypoint writes to when KONTRA_LOG_DIR is
// set. Deliberately NOT /tmp: an actor's own scratch (crawl4ai's chromium profiles) lands in
// /tmp, and mounting that through to the host would fill the host disk during a long scan.
const containerLogDir = "/kontra/logs"

// defaultWorkerMemoryMB caps ONE worker container.
//
// EVERY CONTROL-PLANE SERVICE CARRIES A HARD LIMIT (`docker-compose.yml`'s whole-controller
// budget) and, until this, the actor containers — the ones most likely to run away — carried
// none. A crawler holding pages or a scanner buffering responses could take the Controller with
// it, and the failure lands on Temporal and the orchestrator rather than on the actor that caused
// it, which is the hardest kind of outage to attribute.
//
// 512 MiB matches what `orchestrator-api` and `seaweed` are given, and is roughly 4× the measured
// resident set of the actors running on this Controller (~110–130 MiB each). A worker that exceeds
// it is OOM-killed and restarted by its `unless-stopped` policy, which is the outcome to want: one
// actor dies and says so, instead of the host dying quietly.
//
// `KONTRA_WORKER_MEMORY_MB` overrides it, and `0` disables the cap for an actor that genuinely
// needs the host (a browser fleet on a dedicated box).
const defaultWorkerMemoryMB = 512

// workerMemoryBytes is the cap to put on one worker, in bytes. `0` means uncapped.
//
// PURE, so the parse — including what an unreadable value does — is pinned by a test rather than
// by whatever happens to be in the environment. An unparseable or negative value falls back to the
// default rather than to "unlimited": a typo in an env var must not silently remove a memory
// bound.
func workerMemoryBytes(env string) int64 {
	raw := strings.TrimSpace(env)
	if raw == "" {
		return int64(defaultWorkerMemoryMB) * 1024 * 1024
	}
	mb, err := strconv.Atoi(raw)
	if err != nil || mb < 0 {
		return int64(defaultWorkerMemoryMB) * 1024 * 1024
	}
	return int64(mb) * 1024 * 1024
}

// workerPlane is the resolved answer to "where is the control plane, from inside a worker
// container" — four addresses and the network that can reach them, settled together because they
// are one decision and not five.
type workerPlane struct {
	address, orchestrator, s3, redis string
	network                          string
	// appliance is true when these came from a running `kontra up` rather than from the compose
	// defaults. It is what decides whether the container gets a host-gateway alias.
	appliance bool
	// source is one sentence for the summary line: an operator who scales a worker against the
	// wrong control plane must be able to see it in the output rather than in a poll timeout.
	source string
}

// resolveWorkerPlane picks the control plane a worker container dials.
//
// THERE ARE TWO CONTROL PLANES NOW AND THE OLD DEFAULT SILENTLY NAMES THE DEAD ONE. This function
// used to be four `if x == "" { x = "<compose-dns>" }` lines inside startWorker, written when
// `temporal:7233`, `seaweed:8333` and `redis:6379` were compose services on the `kontra` network.
// None of those three is a service any more — they are the appliance's, in one host process (ADR
// 0031 §1) — and docker-compose.yml's own header names this function as the caller that had not
// been fixed yet. A worker handed the old defaults starts, retries a DNS name that does not
// resolve, and shows up in `docker ps` and in `kontra workers list` as a running replica. That is
// the same silent-healthy failure as a false `poller: NONE`, and it is why the resolution is one
// function with one order rather than four defaults:
//
//	the caller said so             --address/--s3/--redis, or the MCP tool's arguments
//	a running appliance            the addresses `kontra up` actually BOUND, from its data dir
//	the compose control plane      the DNS names, for a deployment that still runs one
//
// The middle rung is the same mechanism `registryAddress` uses for the image address, reading the
// same data directory, so a deploy and the scale that follows it cannot disagree about which
// installation they belong to.
func resolveWorkerPlane(o scaleOpts) (workerPlane, error) {
	p := workerPlane{
		address:      o.address,
		orchestrator: o.orchestrator,
		s3:           o.s3,
		redis:        o.redis,
		network:      o.network,
		source:       "compose control plane (temporal:7233, orchestrator-api:8088, seaweed:8333, redis:6379)",
	}

	if dir, err := applianceDataDir(""); err == nil {
		if e, ok := appliance.ReadEndpoints(dir); ok {
			// A CONTAINER CANNOT REACH A LOOPBACK BIND, and this is where that is refused rather
			// than discovered. The check runs whenever an appliance record exists and the worker
			// is not on the host's own network stack — including when the caller passed every
			// address by hand, because a hand-typed `127.0.0.1:7233` is the same unreachable
			// address by a different route.
			if p.network != "host" {
				if err := e.ReachableFromContainer(); err != nil {
					return workerPlane{}, err
				}
			}
			p.appliance = true
			p.source = "the appliance at " + dir
			if p.address == "" {
				p.address = e.Temporal
			}
			if p.s3 == "" {
				p.s3 = e.S3
			}
			if p.redis == "" {
				p.redis = e.KV
			}
			if p.orchestrator == "" && e.API != "" {
				p.orchestrator = e.API
			}
			// THE DEFAULT BRIDGE, not `kontra`. That named network is compose's, created by the
			// stack this topology replaces, so on a machine that has only ever run the binary it
			// does not exist and ContainerCreate fails with `network kontra not found`. `bridge`
			// is the daemon's own and is always there.
			if p.network == "" {
				p.network = "bridge"
			}
		}
	}

	if p.network == "" {
		p.network = controlPlaneNetwork
	}
	if p.address == "" {
		p.address = "temporal:7233"
	}
	if p.orchestrator == "" {
		p.orchestrator = "http://orchestrator-api:8088"
	}
	if p.s3 == "" {
		p.s3 = "http://seaweed:8333"
	}
	// The shared statestore every worker points at, so global_state is one backend across the
	// fleet rather than a per-container island (platform-x100 #02).
	if p.redis == "" {
		p.redis = "redis:6379"
	}
	return p, nil
}

// startWorker create+starts one worker container wired to the control plane.
func startWorker(ctx context.Context, d containerAPI, name, img, label string, plane workerPlane, o scaleOpts) (string, error) {
	otel := o.otel
	env := []string{
		"KONTRA_ADDRESS=" + plane.address,
		"KONTRA_ORCHESTRATOR_URL=" + plane.orchestrator,
		"KONTRA_S3_ENDPOINT=" + plane.s3,
		"KONTRA_REDIS_HOST=" + plane.redis,
	}
	// The tracing collector (platform-x100 #03), and the one endpoint here with NO default.
	// The others name control-plane components kontra runs; a collector is not one of them
	// (ADR 0031), so there is nothing to point a default at. Inherited from the operator's own
	// KONTRA_OTEL_ENDPOINT when the caller did not pass one — that is how a box that DOES run a
	// collector gets the whole chain (orchestrator → Temporal → handler workflow) in one trace —
	// and simply absent otherwise, so the worker's exporters never start. Absent, not empty:
	// the entrypoint tests for a non-empty value, and an unset variable is the honest statement
	// that nobody is collecting.
	if otel == "" {
		otel = os.Getenv("KONTRA_OTEL_ENDPOINT")
	}
	if otel != "" {
		env = append(env, "KONTRA_OTEL_ENDPOINT="+otel)
	}
	// Durable logging (opt-in): point the entrypoint at the mounted dir. Unset => the entrypoint
	// keeps writing to the container's /tmp exactly as before.
	var binds []string
	if bind, _ := workerLogBind(o.logDir, name); bind != "" {
		binds = append(binds, bind)
		env = append(env, "KONTRA_LOG_DIR="+containerLogDir)
	}

	resp, err := d.ContainerCreate(ctx,
		&container.Config{
			Image:  img,
			Labels: map[string]string{managedLabel: label, "kontra.managed": "true"},
			Env:    env,
		},
		&container.HostConfig{
			NetworkMode:   container.NetworkMode(plane.network),
			RestartPolicy: container.RestartPolicy{Name: "unless-stopped"},
			Binds:         binds,
			// `host.docker.internal` is not a Linux default; this alias is what defines it, and
			// it is the same one docker-compose.yml gives every service that dials the embedded
			// Temporal. The resolved addresses above do not need it — they already name an
			// address the bridge can see — but an operator overriding one with
			// `--address host.docker.internal:7233`, which is the spelling the compose file
			// documents, would otherwise get a DNS failure and no clue that the name is the
			// thing that is missing.
			ExtraHosts: applianceExtraHosts(plane),
			// Cap the engine's own json-file log. The default is UNBOUNDED, and a chatty actor on
			// a large run can fill the host disk through it. The durable copy lives in the bind
			// mount above; this is just the `docker logs` buffer, so a tight cap costs nothing.
			LogConfig: container.LogConfig{
				Type:   "json-file",
				Config: map[string]string{"max-size": "100m", "max-file": "5"},
			},
			// Cap the worker's memory for the same reason the log is capped, and with higher
			// stakes: the default is UNBOUNDED, and an actor that runs away takes the Controller —
			// Temporal, the orchestrator and every other actor — down with it. See
			// {@link defaultWorkerMemoryMB}.
			Resources: container.Resources{
				Memory: workerMemoryBytes(os.Getenv("KONTRA_WORKER_MEMORY_MB")),
			},
		},
		nil, nil, name)
	if err != nil {
		return "", err
	}
	if err := d.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		// Don't leave a created-but-unstarted container behind: it still carries the managed
		// label, so the next reconcile would count it as a live replica and scale would never
		// self-heal. Remove it before surfacing the start error.
		_ = d.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
		return "", err
	}
	return resp.ID, nil
}

// applianceExtraHosts gives the container the `host.docker.internal` alias when the control plane
// is a host process. Empty on the compose topology and on `--network host`: the first resolves
// its services by compose DNS, and the second shares the host's resolver already — and Docker
// rejects a host-gateway alias on a container with no network of its own.
func applianceExtraHosts(plane workerPlane) []string {
	if !plane.appliance || plane.network == "host" {
		return nil
	}
	return []string{"host.docker.internal:host-gateway"}
}

// workerName / workerIndex are the (name,version,index) ↔ container-name contract. Dots in
// the version are kept (Docker allows them); index is the trailing "-N".
func workerName(name, version string, i int) string {
	return fmt.Sprintf("kontra-%s-%s-%d", name, version, i)
}

func workerIndex(containerName string) int {
	if i := strings.LastIndexByte(containerName, '-'); i >= 0 {
		n := 0
		if _, err := fmt.Sscanf(containerName[i+1:], "%d", &n); err == nil {
			return n
		}
	}
	return 0
}
