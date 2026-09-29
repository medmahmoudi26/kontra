// deploy.go — `kontra deploy --actor <dir>`: build (and optionally push) an actor
// image via the Docker Engine API. No shell-out to `docker build` — the engine API is
// the boundary here — and the CLASSIC builder on purpose: no BuildKit session dance,
// and these Dockerfiles are linear COPY+pip anyway.
package main

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	docker "github.com/docker/docker/client"
	"github.com/docker/docker/pkg/archive"

	"github.com/medmahmoudi26/kontra/cli/appliance/registry"
	"github.com/medmahmoudi26/kontra/cli/internal/cliio"
	"github.com/medmahmoudi26/kontra/cli/internal/cliutil"
	"github.com/medmahmoudi26/kontra/cli/internal/ociref"
)

// baseImage is the canonical Python host (infra/Dockerfile.pyworker) every
// Dockerfile-less actor builds FROM. MAJOR tag — see the Dockerfile's comment.
const baseImage = "kontra-host:1"

func hostImage() string {
	if v := strings.TrimSpace(os.Getenv("KONTRA_HOST_IMAGE")); v != "" {
		return v
	}
	return baseImage
}

// defaultRegistry is the last answer to "which registry", used when nothing else says: the
// appliance's own port on loopback, spelled the way it has always been spelled here. It is a
// FALLBACK and not the answer — see registryAddress, which prefers the address the running
// appliance actually bound.
const defaultRegistry = "localhost:5000"

// registryAddress resolves THE address, once, for both halves of the round trip.
//
// THIS FUNCTION IS THE FIX FOR THE OLDEST BUG ON THIS PATH. `deploy` tags an image with a
// registry address and pushes it; `scale` pulls by an address it resolves separately; and an
// image the daemon does not have does NOT auto-pull on container create. Two resolutions that
// disagree therefore surface as `no such image` at scale time — a message about Docker, three
// commands away from the deploy that caused it. So there is one resolution and both callers use
// it, in one order:
//
//	--registry            the operator said so
//	KONTRA_REGISTRY       the environment said so (a second appliance, a remote controller)
//	the appliance         the address `kontra up` actually BOUND, read from its data directory
//	defaultRegistry       nothing is running; say the conventional thing and let the
//	                      reachability check produce the message
//
// The third rung is why a `--registry-port` that had to move does not silently orphan every
// deploy: the port is discovered, not assumed.
func registryAddress(flagVal string) string {
	if v := strings.TrimSpace(flagVal); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("KONTRA_REGISTRY")); v != "" {
		return v
	}
	if dir, err := applianceDataDir(""); err == nil {
		if addr, ok := registry.ReadAddress(dir); ok {
			return addr
		}
	}
	return defaultRegistry
}

// workerBaseImage caches the actor-AGNOSTIC worker parts (the compiled Go handler +
// entrypoint) so a per-actor deploy COPYs the pre-built handler instead of recompiling
// it every time. Rebuilt only when handler/ changes:
//
//	docker rmi kontra-worker-base:1
//
// THE CONSTANT IS THE DEFAULT, NOT THE ANSWER — see workerBase() below. It stays a literal, spelled
// out rather than assembled, because `.github/workflows/publish.yml` reads the declaration below to
// learn which image it has to build.
//
// AND THIS COMMENT MUST NOT RESTATE THE PATTERN THAT GREP LOOKS FOR. It used to quote it, and the
// grep was unanchored, so it matched the quotation too and handed the workflow an ellipsis as a
// second image name — `0.0.0-test7` pushed all five images and then failed with "the install
// references a kontra-owned image '…' that this workflow does not know how to build". The grep is
// anchored to `^const` now, so a comment cannot match it; this note stays wordy instead.
const workerBaseImage = "kontra-worker-base:1"

// workerBase resolves THE worker base reference, the same way hostImage() resolves the Python host.
//
// IT IS AN ENVIRONMENT VARIABLE BECAUSE A LOCAL BUILD AND THE INSTALL HAVE TO AGREE ON A NAME.
// `Makefile`'s `worker-base` target tags whatever `KONTRA_WORKER_BASE_IMAGE` says, and it defaults to
// `ghcr.io/medmahmoudi26/kontra-worker-base:dev` so a `make image` writes the name the published
// install resolves. With this function absent, that target wrote one name and this file looked for
// another: Docker resolves by NAME, so `deploy` found nothing at :500, fell through to :510, and
// recompiled the Go handler on a machine that had just built it. Not a failure — the fallback is
// real and it works — but a silent minute, and `make worker-base` became work with no consumer.
//
// The same split is why `publish.yml` publishes `kontra-worker-base:<version>` at all: without a
// variable, a published worker base is a name nothing ever asks for.
func workerBase() string {
	if v := strings.TrimSpace(os.Getenv("KONTRA_WORKER_BASE_IMAGE")); v != "" {
		return v
	}
	return workerBaseImage
}

// imageAPI is the slice of the Docker client deploy needs — a tiny interface so tests
// can fake it and never touch a daemon.
type imageAPI interface {
	ImageList(ctx context.Context, options image.ListOptions) ([]image.Summary, error)
	ImageBuild(ctx context.Context, buildContext io.Reader, options types.ImageBuildOptions) (types.ImageBuildResponse, error)
	ImageTag(ctx context.Context, source, target string) error
	ImagePush(ctx context.Context, ref string, options image.PushOptions) (io.ReadCloser, error)
}

var newDocker = func() (imageAPI, error) {
	return docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
}

type actorManifest struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	// Kind is retained to REFUSE the one value that used to be legal beside "actor". There is
	// one deployed kind now (ADR 0023 §9), so "" and "actor" are the whole vocabulary and a
	// manifest still saying "activity" gets told what to do rather than silently deploying as
	// something else.
	Kind string `json:"kind"`
	// Entry names the module the worker runs, when it is not the default (actor.py). Rarely
	// needed; here so an actor is not forced to rename a file it already ships under another
	// name.
	Entry string `json:"entry"`
	// Engine is "py" or "go", when the author says so. Most manifests do not: the folder already
	// says which it is — a `go.mod` and a `main.go`, or an `actor.py` — and `engineFor` reads that.
	// Declaring it is how an author settles a folder that somehow holds both.
	Engine string `json:"engine"`
}

// engineFor decides which engine an actor folder runs under: the flag, then the manifest, then the
// FOLDER ITSELF.
//
// THE DEFAULT USED TO BE "py" AND THAT WAS A REAL BUG, not a conservative choice. Serving a Go
// actor without `-engine go` ran `python <dir>/actor.py` against a directory that has never held
// one, and the failure — `can't open file …/actor.py` — reads as a missing file rather than as the
// wrong engine, so it sends you to look for a file that should not exist. The Actors page's Serve
// button passes no engine at all, deliberately (there is no placement or runtime picker on that
// surface), so under the old default it could only ever serve Python actors and reported a Python
// error for every Go one.
//
// THE FOLDER IS THE EVIDENCE. A Go actor has a `go.mod`; a Python one has the entry module. Both
// present is an author error worth refusing rather than resolving by coin-flip: the two would serve
// different code from one directory depending on a flag nobody passed.
func engineFor(dir, flagValue string, m actorManifest) (string, error) {
	if v := strings.TrimSpace(flagValue); v != "" {
		if v != "py" && v != "go" {
			return "", fmt.Errorf("unknown engine %q (want py|go)", v)
		}
		return v, nil
	}
	if v := strings.TrimSpace(m.Engine); v != "" {
		if v != "py" && v != "go" {
			return "", fmt.Errorf("actor.json declares engine %q (want py|go)", v)
		}
		return v, nil
	}
	entry := m.entryFile()
	hasGo := cliutil.FileExists(filepath.Join(dir, "go.mod"))
	hasPy := cliutil.FileExists(filepath.Join(dir, entry))
	switch {
	case hasGo && hasPy:
		// THE ONE CASE WORTH REFUSING. Both present means the folder would serve different code
		// depending on a flag nobody passed, and a coin-flip between two real programs is worse
		// than a sentence asking which.
		return "", fmt.Errorf(
			"%s holds both go.mod and %s — say which with -engine py|go, or `\"engine\"` in actor.json",
			dir, entry)
	case hasGo:
		return "go", nil
	default:
		// PY, INCLUDING WHEN NEITHER FILE IS THERE. Detection is here to stop a Go actor being run
		// as Python; it is not a new place to fail. A folder with neither file is broken either
		// way, and refusing here would turn a manifest-only fixture — and any folder whose entry
		// is generated at build time — into an error where it used to work.
		return "py", nil
	}
}

// Manifest kinds. The zero value is an actor, so every actor.json ever written keeps meaning
// exactly what it meant. kindActivity exists only to be rejected by readManifest.
const (
	kindActor    = "actor"
	kindActivity = "activity"
)

// kindOf normalizes the manifest's kind ("" → actor).
func (m actorManifest) kindOf() string {
	if m.Kind == "" {
		return kindActor
	}
	return m.Kind
}

// entryFile is the module the worker entrypoint runs for this manifest.
func (m actorManifest) entryFile() string {
	if m.Entry != "" {
		return m.Entry
	}
	return "actor.py"
}

func cmdDeploy(args []string) error {
	fs := flag.NewFlagSet("deploy", flag.ContinueOnError)
	actorDir := fs.String("actor", "", "actor directory (contains actor.json)")
	engine := fs.String("engine", "", "actor engine: py|go (default: detected from the folder)")
	registry := fs.String("registry", "", "registry to push the worker image to (default: "+defaultRegistry+")")
	controller := fs.String("controller", "", "controller host to print in the run command (default: from KONTRA_ORCHESTRATOR_URL)")
	hostOnly := fs.Bool("host-only", false, "build only the actor host image; skip the worker bundle + push")
	override := fs.Bool("override", false, "replace an already-deployed version (default: refuse — bump the version instead)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *actorDir == "" {
		return errors.New("deploy needs --actor <dir>")
	}
	// The CLI streams docker build/push progress straight to stdout.
	res, err := runDeploy(context.Background(), cliio.Stdout, deployOpts{
		actorDir: *actorDir, engine: *engine, registry: *registry,
		hostOnly: *hostOnly, override: *override,
	})
	if err != nil {
		return err
	}
	if res.Image == "" { // --host-only: just the host tag, no worker/push/summary
		fmt.Fprintln(cliio.Stdout, res.HostImage)
		return nil
	}
	printDeploySummary(actorManifest{Name: res.Name, Version: res.Version}, res.Image, res.Digest, controllerHost(*controller))
	return nil
}

// deployOpts is the actor-image build/push request — the flag-free core the CLI and the
// `deploy_actor` MCP tool both drive.
type deployOpts struct {
	actorDir string
	engine   string // "py" (default) | "go"
	registry string // "" → defaultRegistry
	hostOnly bool   // build the host image only; skip the worker bundle + push
	override bool   // replace an already-deployed version instead of refusing
}

// deployResult is the structured outcome of a deploy — what `deploy_actor` returns to an
// agent, and what the CLI renders into its human summary.
type deployResult struct {
	Name      string `json:"name"`
	Version   string `json:"version"`
	HostImage string `json:"hostImage"`
	Image     string `json:"image,omitempty"`    // pushed remote image ("" when hostOnly)
	Registry  string `json:"registry,omitempty"` // the registry it was pushed to
	// Digest is the OCI MANIFEST digest the registry returned for the push — the actor's
	// identity under ADR 0032, and the value a run records.
	//
	// IT COMES FROM THE PUSH AND NOWHERE ELSE. An image that was built but never pushed has an
	// image ID, which is the digest of its local config blob and is not this: identity is the
	// registry accepting the content, because that is the thing another machine can resolve. So
	// `--host-only` leaves it empty rather than substituting something that looks like it.
	Digest string `json:"digest,omitempty"`
}

// runDeploy builds the actor's HOST image and (unless hostOnly) the self-contained WORKER
// image, then tags + pushes it to the registry. Docker build/push progress is written to
// `progress` — stdout for the CLI, io.Discard for the MCP server (whose cliio.Stdout is the
// JSON-RPC channel and must never carry build noise). Returns the structured outcome.
func runDeploy(ctx context.Context, progress io.Writer, o deployOpts) (*deployResult, error) {
	if o.actorDir == "" {
		return nil, errors.New("deploy needs an actor directory")
	}
	m, err := readManifest(o.actorDir)
	if err != nil {
		return nil, err
	}
	// DETECTED, not defaulted to "py" — the same rule `serve` uses, so an actor is built with the
	// engine it is served with. Two defaults for one fact is how a folder gets deployed as Python
	// and served as Go, which produces an image whose entrypoint cannot start.
	engine, err := engineFor(o.actorDir, o.engine, m)
	if err != nil {
		return nil, err
	}
	d, err := newDocker()
	if err != nil {
		return nil, fmt.Errorf("docker engine unreachable: %w", err)
	}

	// A worker deploy pushes to the registry — resolve it up front so we can refuse an
	// accidental re-deploy of an existing version BEFORE spending a build.
	reg := registryAddress(o.registry)
	remote := fmt.Sprintf("%s/%s:%s", reg, m.Name, m.Version)
	if !o.hostOnly {
		// A FOURTH SITE NAMES AN ARTIFACT, and it is checked here for the same reason the other three
		// are (cli/internal/ociref/ociref.go). `.scratch/warden/issues/15-*` counted three — build, pull, and the
		// driver — and this is the push half of `deploy`, which the issue folded into "deploy/pull".
		// It is not the same string as the pull site's: this one is built here, from the registry and
		// the manifest, and an actor whose name the OCI grammar cannot express would otherwise spend a
		// full image build and then fail on docker's own wording at ImageTag.
		//
		// It deliberately does NOT re-cliutil.Derive anything. The count of sites is not written down anywhere
		// — shared/conformance/README.md names a count in a comment as "the least reliable kind of
		// documentation there is" — the corpus drives whichever sites its driver lists.
		if err := ociref.Check(remote); err != nil {
			return nil, fmt.Errorf("%s@%s cannot be deployed as an Image.\n%w\n"+
				"  `kontra serve` and every queue derivation take the Actor's name verbatim, so it serves\n"+
				"  fine and only its ARTIFACT is unnameable. Rename it in actor.json.", m.Name, m.Version, err)
		}
		if err := registryReachable(reg); err != nil {
			return nil, err
		}
		if !o.override && versionDeployed(reg, m.Name, m.Version) {
			return nil, fmt.Errorf("%s:%s is already deployed to %s — bump the version (schema/code "+
				"changes need a new version), or pass override to replace it", m.Name, m.Version, reg)
		}
	}

	// 1) the actor HOST image (the actor itself) — the worker builds FROM it. A Python
	// actor layers its code on the shared python host base; a Go actor is a compiled binary, so
	// its host image is built by COMPILING the actor (context = repo root, so the sdk/go and
	// runtime/go `replace`s resolve) into a slim runtime image. Both yield /actor/<name>/ the worker runs.
	hostTag := fmt.Sprintf("kontra/%s:%s", m.Name, m.Version)
	if engine == "go" {
		if err := buildGoActor(ctx, d, progress, o.actorDir, m.Name, hostTag); err != nil {
			return nil, err
		}
	} else {
		if err := ensureBase(ctx, d, progress); err != nil {
			return nil, err
		}
		if err := buildActor(ctx, d, progress, o.actorDir, m.Name, hostTag); err != nil {
			return nil, err
		}
	}
	res := &deployResult{Name: m.Name, Version: m.Version, HostImage: hostTag}
	if o.hostOnly {
		return res, nil
	}

	// 2) the self-contained WORKER image. The actor-agnostic parts (the Go handler and the
	// entrypoint) live in a cached base built ONCE; the per-actor build just layers this
	// actor's code on top — no handler recompile per deploy.
	if err := ensureWorkerBase(ctx, d, progress); err != nil {
		return nil, err
	}
	workerTag := fmt.Sprintf("kontra/%s-worker:%s", m.Name, m.Version)
	if err := buildWorker(ctx, d, progress, m, engine, hostTag, workerTag); err != nil {
		return nil, err
	}

	// 3) push to the registry so any droplet can pull it. `remote` was built and judged before the
	// build (see the grammar check above), so this is the same string and not a second derivation.
	if err := d.ImageTag(ctx, workerTag, remote); err != nil {
		return nil, err
	}
	// Local/insecure registry: the engine still requires an X-Registry-Auth header;
	// base64("{}") is the canonical "no credentials".
	rc, err := d.ImagePush(ctx, remote, image.PushOptions{
		RegistryAuth: base64.URLEncoding.EncodeToString([]byte("{}")),
	})
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	pushed, err := streamPushOutput(progress, rc)
	if err != nil {
		return nil, fmt.Errorf("push %s: %w", remote, err)
	}
	res.Image = remote
	res.Registry = reg

	// THE ADDRESS CHECK, AND IT BELONGS HERE RATHER THAN AT SCALE TIME. The daemon has just
	// reported a digest for a push it made to `reg` AS IT RESOLVES IT; this asks the registry
	// THIS PROCESS reaches at `reg` what that tag resolves to now. Agreement means push and pull
	// are the same registry, which is the property `kontra scale` is about to depend on. A
	// disagreement is caught one second after the push that caused it, naming the address —
	// instead of arriving minutes later as `no such image`, which sends an operator to look at
	// Docker rather than at which registry they are talking to.
	digest, err := confirmPushed(reg, m.Name, m.Version, remote, pushed)
	if err != nil {
		return nil, err
	}
	res.Digest = digest
	return res, nil
}

// manifestAccept is what a registry is told this client can read. Without it a v2 registry may
// answer a HEAD for a tag with a 404 rather than with the schema2 manifest it holds.
var manifestAccept = strings.Join([]string{
	"application/vnd.docker.distribution.manifest.v2+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.oci.image.index.v1+json",
}, ", ")

// errNotInRegistry is "the registry answered, and it does not have this".
var errNotInRegistry = errors.New("not in the registry")

// registryHTTP is separate from statusHTTP's 2s: this one may cross a VPC to a controller.
var registryHTTP = &http.Client{Timeout: 10 * time.Second}

// registryManifestDigest asks a registry what <name>:<ref> resolves to right now, by the digest
// IT reports. Returns errNotInRegistry when the registry answers and does not hold it.
func registryManifestDigest(reg, name, ref string) (string, error) {
	var last error
	for _, base := range registryProbeBases(reg) {
		req, err := http.NewRequest(http.MethodHead, base+"/v2/"+name+"/manifests/"+ref, nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("Accept", manifestAccept)
		resp, err := registryHTTP.Do(req)
		if err != nil {
			last = err
			continue
		}
		digest := resp.Header.Get("Docker-Content-Digest")
		code := resp.StatusCode
		status := resp.Status
		resp.Body.Close()
		switch code {
		case http.StatusOK:
			return digest, nil
		case http.StatusNotFound:
			return "", errNotInRegistry
		default:
			last = fmt.Errorf("registry %s answered %s for %s:%s", reg, status, name, ref)
		}
	}
	if last == nil {
		last = fmt.Errorf("registry %s did not answer", reg)
	}
	return "", last
}

// confirmPushed reconciles what the daemon says it pushed with what the registry says it holds,
// and returns the digest to record. See the call site for why a mismatch is fatal.
func confirmPushed(reg, name, version, remote, pushed string) (string, error) {
	resolved, err := registryManifestDigest(reg, name, version)
	switch {
	case errors.Is(err, errNotInRegistry):
		return "", fmt.Errorf("push/pull address mismatch at %s: docker reported pushing %s, but the registry "+
			"this process reaches at %s does not hold %s:%s.\n  Your Docker daemon and kontra are resolving %q to "+
			"DIFFERENT registries — a remote DOCKER_HOST, a container network, or a second registry on that port.\n"+
			"  `kontra scale` would fail later with \"no such image\", which is a message about Docker and not about "+
			"this.\n  Pass --registry <host:port> (or set KONTRA_REGISTRY) to an address both sides reach",
			reg, remote, reg, name, version, reg)
	case err != nil:
		return "", fmt.Errorf("pushed %s, but could not ask the registry at %s to confirm it — push and pull must "+
			"resolve the SAME address, and this one did not answer: %w", remote, reg, err)
	}
	if resolved == "" {
		// A registry that does not report Docker-Content-Digest. It held the tag, which is the
		// address question answered; trust the daemon for the identity.
		return pushed, nil
	}
	if pushed != "" && pushed != resolved {
		return "", fmt.Errorf("push/pull digest mismatch at %s: docker pushed %s as %s, but %s:%s resolves at that "+
			"address to %s — two registries are answering on %q, and the one you pulled from is not the one you "+
			"pushed to", reg, remote, pushed, name, version, resolved, reg)
	}
	return resolved, nil
}

// registryBase turns a bare host:port into a URL, leaving an explicit scheme alone.
func registryBase(reg string) string {
	if strings.Contains(reg, "://") {
		return strings.TrimRight(reg, "/")
	}
	return "http://" + strings.TrimRight(reg, "/")
}

// registryProbeBases is the HTTP view of a registry from THIS process.
//
// `kontra deploy` tags and pushes through the Docker daemon (the mounted socket, so the
// host). A Compose workspace-watch container's 127.0.0.1 is not that host; the daemon's
// 127.0.0.1:5000 is. host.docker.internal and the compose service name `registry` reach
// the same registry from inside the container. Image names stay 127.0.0.1:5000/... so
// the daemon can push and pull them.
func registryProbeBases(reg string) []string {
	seen := map[string]struct{}{}
	var out []string
	add := func(b string) {
		if b == "" {
			return
		}
		if _, ok := seen[b]; ok {
			return
		}
		seen[b] = struct{}{}
		out = append(out, b)
	}
	add(registryBase(reg))
	raw := strings.TrimPrefix(strings.TrimPrefix(reg, "http://"), "https://")
	host, port, ok := strings.Cut(raw, ":")
	if !ok {
		host, port = raw, "5000"
	}
	if host == "127.0.0.1" || host == "localhost" || host == "::1" {
		add("http://host.docker.internal:" + port)
		add("http://registry:" + port)
	}
	return out
}

// ensureWorkerBase builds the actor-AGNOSTIC worker parts image (kontra-worker-base:1) if
// absent: a golang stage compiles the Go handler ONCE, and the result — handler + entrypoint —
// is stashed in a slim image the per-actor build COPYs from. This is what makes a re-deploy fast (no handler recompile) and dodges the
// memory-heavy compile on every build. Rebuilt only when handler/ changes (docker rmi it).
// Context = the REPO ROOT (needs handler/ + infra/); the Dockerfile is injected.
func ensureWorkerBase(ctx context.Context, d imageAPI, progress io.Writer) error {
	sums, err := d.ImageList(ctx, image.ListOptions{
		Filters: filters.NewArgs(filters.Arg("reference", workerBase())),
	})
	if err != nil {
		return err
	}
	if len(sums) > 0 {
		return nil
	}
	root, err := cliutil.FindRepoRoot("")
	if err != nil {
		return fmt.Errorf("worker base %s missing and no repo root to build it (handler/ + infra/): %w", workerBase(), err)
	}
	fmt.Fprintf(os.Stderr, "worker base %s missing — building it once (compiles the handler; cached after this)\n", workerBase())
	tarCtx, err := archive.TarWithOptions(root, &archive.TarOptions{ExcludePatterns: dockerignore(root)})
	if err != nil {
		return err
	}
	tarCtx = injectFile(tarCtx, "Dockerfile.kontra-worker-base", []byte(workerBaseDockerfile()))
	defer tarCtx.Close()
	return buildImage(ctx, d, progress, tarCtx, "Dockerfile.kontra-worker-base", workerBase(), nil)
}

// workerBaseDockerfile compiles the handler (-p=1: serial, so the memory-heavy temporal+aws
// deps don't OOM a small host) and parks it + the entrypoint in a slim image the per-actor
// worker COPYs from.
func workerBaseDockerfile() string {
	// `runtime/go` IS REQUIRED AND WAS MISSING, and the way it failed is the reason this comment
	// is here rather than just the line.
	//
	// `runtime/handler` imports `runtime/go/codec` and its go.mod `replace`s that module to
	// `../go`, so the build needs the directory present. It was not copied — but the image is
	// built ONCE and cached (`ensureWorkerBase` returns early when the tag exists), so the
	// existing `kontra-worker-base:1` predated the import and nothing failed for as long as
	// nobody deleted it. The moment one is deleted — which the docs tell you to do, as the way to
	// pick up a handler or entrypoint change — every deploy on the machine breaks with a bare
	// `returned a non-zero code: 1`.
	//
	// `control/images/Dockerfile.workerbase` is a SECOND COPY of this same image and already has
	// the line. Two spellings of one artifact, one of them fixed: the standalone file is what a
	// human reads and edits, and this is what actually runs.
	return fmt.Sprintf(`FROM golang:1.25 AS handler-build
ENV GOTOOLCHAIN=go1.26.4
WORKDIR /src
COPY sdk/go ./sdk/go
COPY runtime/go ./runtime/go
COPY runtime/handler ./runtime/handler
RUN cd runtime/handler && GOWORK=off go build -p=1 -trimpath -o /out/handler .

FROM alpine:3
COPY --from=handler-build /out/handler /kontra/handler
COPY control/images/worker-entrypoint.sh /kontra/entrypoint.sh
`)
}

// buildWorker builds the per-actor worker image FAST: FROM the actor host image (so it
// carries the actor's own deps) and COPY the pre-built parts from kontra-worker-base:1 — no
// handler recompile. The build context is empty (the Dockerfile only FROMs/COPYs existing
// images), so this is a handful of quick layers.
func buildWorker(ctx context.Context, d imageAPI, progress io.Writer, m actorManifest, engine, hostTag, tag string) error {
	df := workerDockerfile(hostTag, m, engine)
	buildCtx := emptyTarWith("Dockerfile.kontra-worker", []byte(df))
	defer buildCtx.Close()
	return buildImage(ctx, d, progress, buildCtx, "Dockerfile.kontra-worker", tag, nil)
}

// workerDockerfile assembles a worker from the actor host image + the cached worker base.
// No golang stage and no handler compile — just COPYs from kontra-worker-base:1.
//
// Every worker gets the handler, because there is one deployed kind (ADR 0023 §9): the handler
// owns the Actor's backing workflow and serves its Nexus op, and an Actor with one Method and
// no load is still dispatched through it.
func workerDockerfile(hostTag string, m actorManifest, engine string) string {
	env := fmt.Sprintf("ENV KONTRA_ACTOR_NAME=%s KONTRA_ACTOR_VERSION=%s KONTRA_ACTOR_ENGINE=%s KONTRA_ACTOR_KIND=%s KONTRA_ACTOR_ENTRY=%s",
		m.Name, m.Version, engine, m.kindOf(), m.entryFile())
	return fmt.Sprintf(`FROM %[1]s
USER root
COPY --from=%[2]s /kontra/handler /kontra/handler
COPY --from=%[2]s /kontra/entrypoint.sh /kontra/entrypoint.sh
RUN chmod +x /kontra/entrypoint.sh /kontra/handler
%[3]s
ENTRYPOINT ["/kontra/entrypoint.sh"]
`, hostTag, workerBase(), env)
}

// buildGoActor builds a Go actor's HOST image by COMPILING it. Unlike a Python actor (which
// layers actor.py on the shared python host base), a Go actor is a self-contained binary, so
// there is no shared Go base: a golang stage compiles the actor and it is parked with its
// actor.json under /actor/<name>/ in a slim runtime image. The build CONTEXT is the REPO ROOT,
// not the actor dir, because the actor's go.mod `replace`s — `=> ../../../sdk/go` and, for an
// actor (not a caller), `=> ../../../runtime/go` — point OUTSIDE the actor dir, so the whole
// module tree (both SDK seams + the actor) must be in the context. The
// actor's repo-relative path is computed so the Dockerfile can `cd` into it to build.
func buildGoActor(ctx context.Context, d imageAPI, progress io.Writer, actorDir, name, tag string) error {
	root, err := cliutil.FindRepoRoot("")
	if err != nil {
		return fmt.Errorf("go actor build needs the repo root (sdk/go + runtime/go + the actor): %w", err)
	}
	absActor, err := filepath.Abs(actorDir)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(root, absActor)
	if err != nil || strings.HasPrefix(rel, "..") {
		return fmt.Errorf("go actor dir %s must live under the repo root %s", actorDir, root)
	}
	rel = filepath.ToSlash(rel)
	// Optional per-actor runtime layer. A Go actor that shells to an external tool (subfinder,
	// masscan, …) needs that binary in its image, and unlike the Python path — where a
	// Dockerfile in the actor dir wins — buildGoActor always synthesizes. `runtime.Dockerfile`
	// is the Go-side escape hatch: its contents are appended to the RUNTIME stage only, so the
	// actor can install what it needs without being able to disturb the build stage or the
	// /actor/<name>/ layout the entrypoint depends on.
	var runtimeExtra []byte
	if b, err := os.ReadFile(filepath.Join(absActor, "runtime.Dockerfile")); err == nil {
		runtimeExtra = b
		fmt.Fprintf(os.Stderr, "using %s/runtime.Dockerfile for the runtime stage\n", rel)
	}
	tarCtx, err := archive.TarWithOptions(root, &archive.TarOptions{ExcludePatterns: dockerignore(root)})
	if err != nil {
		return err
	}
	tarCtx = injectFile(tarCtx, "Dockerfile.kontra-go-actor", []byte(goActorDockerfile(rel, name, runtimeExtra)))
	defer tarCtx.Close()
	return buildImage(ctx, d, progress, tarCtx, "Dockerfile.kontra-go-actor", tag, nil)
}

// goActorDockerfile compiles the actor at repo-relative path `rel` into a STATIC binary and
// parks it + its actor.json under /actor/<name>/ in a slim runtime image. GOWORK=off (the
// example actors are standalone modules outside the root go.work); CGO_ENABLED=0 so the binary
// runs on any base; golang 1.26.4 matches sdk/go's toolchain directive. At runtime the
// binary self-locates actor.json beside itself (the SDK's resolveIdentity), and the worker
// entrypoint launches it because the worker image is stamped KONTRA_ACTOR_ENGINE=go.
// runtimeExtra (from the actor's optional runtime.Dockerfile) is appended to the RUNTIME stage,
// after the binary and manifest are in place — so an actor can install external tools it shells
// to without touching the build stage or the /actor/<name>/ layout the entrypoint relies on.
func goActorDockerfile(rel, name string, runtimeExtra []byte) string {
	df := fmt.Sprintf(`FROM golang:1.26.4 AS actor-build
WORKDIR /src
COPY . .
RUN cd %[1]s && GOWORK=off CGO_ENABLED=0 go build -trimpath -o /out/%[2]s .
FROM debian:12-slim
COPY --from=actor-build /out/%[2]s /actor/%[2]s/%[2]s
COPY %[1]s/actor.json /actor/%[2]s/actor.json
`, rel, name)
	if len(runtimeExtra) > 0 {
		df += "\n# --- from " + rel + "/runtime.Dockerfile ---\n" + string(runtimeExtra) + "\n"
	}
	return df
}

// emptyTarWith returns a tar stream containing just one file — a build context that carries
// only the (injected) Dockerfile, for a build whose Dockerfile pulls everything via FROM /
// COPY --from and needs no context files.
func emptyTarWith(name string, data []byte) io.ReadCloser {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data))})
	_, _ = tw.Write(data)
	_ = tw.Close()
	return io.NopCloser(&buf)
}

// versionDeployed reports whether <name>:<version> already exists in the registry (a prior
// deploy) — the signal `kontra deploy` refuses to overwrite without --override.
func versionDeployed(reg, name, version string) bool {
	for _, base := range registryProbeBases(reg) {
		resp, err := statusHTTP.Get(base + "/v2/" + name + "/tags/list")
		if err != nil {
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			continue
		}
		var res struct {
			Tags []string `json:"tags"`
		}
		err = json.NewDecoder(resp.Body).Decode(&res)
		resp.Body.Close()
		if err != nil {
			continue
		}
		for _, t := range res.Tags {
			if t == version {
				return true
			}
		}
		return false
	}
	return false
}

// registryReachable verifies the registry answers on /v2/ before we tag+push, so a
// missing registry is a clear message (with the fix) instead of a raw push error.
func registryReachable(reg string) error {
	for _, base := range registryProbeBases(reg) {
		if httpAnswers(base + "/v2/") {
			return nil
		}
	}
	// THE HINT IS `kontra up`, NOT A `docker run`. The registry is a component of the control
	// plane and is served from the binary (ADR 0032); telling an operator to hand-start a
	// `registry:2` container was the shape of the thing this replaced — "a control plane that
	// asks you to hand-start one of its own components is not installed, it is assembled".
	hint := "\n  start it:   kontra up   (the registry is served from the appliance)"
	if reg != defaultRegistry {
		hint = "\n  this address came from --registry or KONTRA_REGISTRY; unset it to use the appliance's own"
	}
	return fmt.Errorf("registry %s is unreachable — nothing answers /v2/ there%s", reg, hint)
}

// controllerHost resolves the host to print in the run command: --controller wins,
// else the host from KONTRA_ORCHESTRATOR_URL, else the literal <controller> placeholder
// (localhost is useless from a remote droplet, so we don't pretend otherwise).
func controllerHost(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	h := hostOf(orchestratorURL())
	if h == "" || h == "localhost" || h == "127.0.0.1" {
		return "<controller>"
	}
	return h
}

// printDeploySummary prints the "actor deployed" block: the image address, the pull
// command, and the run command that turns a pulled image into a live worker.
func printDeploySummary(m actorManifest, image, digest, ctrl string) {
	fmt.Fprintf(cliio.Stdout, "\nactor deployed: %s@%s\n", m.Name, m.Version)
	fmt.Fprintf(cliio.Stdout, "  image:  %s\n", image)
	// THE DIGEST IS THE IDENTITY (ADR 0032); the tag above is the convenience. It is printed
	// beside the tag rather than instead of it because both are true — the tag is what an
	// operator types and the digest is what a run records — and because a tag that has moved is
	// only visible when the digest beside it changed.
	if digest != "" {
		fmt.Fprintf(cliio.Stdout, "  digest: %s\n", digest)
	}
	fmt.Fprintf(cliio.Stdout, "  pull:   docker pull %s\n", image)
	fmt.Fprintf(cliio.Stdout, "  run:    docker run -d --name kontra-%s \\\n", m.Name)
	fmt.Fprintf(cliio.Stdout, "            -e KONTRA_ADDRESS=%s:7233 \\\n", ctrl)
	fmt.Fprintf(cliio.Stdout, "            -e KONTRA_ORCHESTRATOR_URL=http://%s:8088 \\\n", ctrl)
	fmt.Fprintf(cliio.Stdout, "            -e KONTRA_S3_ENDPOINT=http://%s:8333 \\\n", ctrl)
	// Redis is REQUIRED, not an upgrade. The worker used to bundle its own, which made each
	// container an island of state; ADR 0018 removed it along with the sidecar, so the actor's
	// durable state (the commit map, unit_state, object_state, global_state) lives on the
	// Controller. A
	// worker started without this points at a localhost Redis that is not there.
	fmt.Fprintf(cliio.Stdout, "            -e KONTRA_REDIS_HOST=%s:6379 \\\n", ctrl)
	fmt.Fprintf(cliio.Stdout, "            %s\n", image)
	fmt.Fprintln(cliio.Stdout, "  creds:  append  -e KONTRA_S3_ACCESS_KEY=… -e KONTRA_S3_SECRET_KEY=…  if the object store is not anonymous")
	fmt.Fprintln(cliio.Stdout, "  then:   kontra workers list   # the worker appears, wherever it runs")
}

func readManifest(dir string) (actorManifest, error) {
	var m actorManifest
	b, err := os.ReadFile(filepath.Join(dir, "actor.json"))
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("%s: %v", filepath.Join(dir, "actor.json"), err)
	}
	if m.Name == "" || m.Version == "" {
		return m, fmt.Errorf("%s needs name and version", filepath.Join(dir, "actor.json"))
	}
	switch m.kindOf() {
	case kindActor:
	case kindActivity:
		// Named rather than merely rejected: the manifest is the only place the retired kind
		// survives, and "unknown kind" would send its author looking for a typo.
		return m, fmt.Errorf("%s: kind %q retired with ADR 0023 §9 — there is one deployed "+
			"kind. Drop the field and write the functions as an Actor with one Method and no "+
			"@actor.load; the caller reaches it with catalog.actor(...) like everything else",
			filepath.Join(dir, "actor.json"), kindActivity)
	default:
		return m, fmt.Errorf("%s: unknown kind %q (want %q)",
			filepath.Join(dir, "actor.json"), m.Kind, kindActor)
	}
	return m, nil
}

// sdkLabel carries a digest of the Python the base image bakes in, so ensureBase can tell a
// current base from a stale one. The tag cannot answer that question: `kontra-host:1` is a MAJOR
// tag on purpose (Dockerfile.pyworker says so — per-actor images pin it and survive base patches),
// so it is the same string before and after any SDK edit.
const sdkLabel = "org.kontra.sdk"

// sdkDigest hashes every file the base image COPYs — sdk/python and runtime/python — so an edit
// to any of them changes the answer. Paths go into the hash beside contents, so that adding,
// deleting or renaming a module counts as a change even when the bytes are a permutation.
//
// __pycache__ is skipped: it is build output, it is in .dockerignore, and its mtime-keyed .pyc
// names would otherwise make the digest differ from itself between two runs over one tree.
func sdkDigest(root string) (string, error) {
	h := sha256.New()
	for _, seam := range []string{"sdk/python", "runtime/python"} {
		dir := filepath.Join(root, seam)
		err := filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if e.IsDir() {
				if e.Name() == "__pycache__" {
					return filepath.SkipDir
				}
				return nil
			}
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			// Length-prefixed, so "a" + "bc" and "ab" + "c" cannot collide.
			fmt.Fprintf(h, "%s\x00%d\x00", filepath.ToSlash(rel), len(b))
			h.Write(b)
			return nil
		})
		if err != nil {
			return "", fmt.Errorf("digest %s: %w", seam, err)
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ensureBase builds kontra-host:1 (infra/Dockerfile.pyworker) when the daemon lacks it OR when
// the one it has bakes a different SDK than the checkout.
//
// IT USED TO STOP AT "the daemon has an image with that name", AND THAT SILENTLY PINNED EVERY
// ACTOR TO WHATEVER SDK WAS CURRENT THE DAY THE BASE WAS FIRST BUILT. Measured 2026-09-29: this
// machine's `kontra-host:1` was eight days old, so `kontra/canary-worker:1.1.0` — built minutes
// earlier, from a checkout where `say.py` was long deleted and `facts.py` long added — still
// carried a `kontra` package with `say.py` and no `facts.py`. `from kontra import progress`
// therefore bound to the DELETED `say.progress(**fields)` rather than the current
// `facts.progress(phase, axis, ...)`, and every unit of every run died at the first call with
// `TypeError: progress() takes 0 positional arguments but 2 were given`. The failure presents as
// an actor bug ("units permanently dropped"), three layers away from the deploy that caused it,
// and no amount of rebuilding the ACTOR fixes it — the stale bytes are in its FROM.
//
// Context = the REPO ROOT (the Dockerfile COPYs sdk/python), honoring the root .dockerignore —
// the engine API does not read it for us the way the docker CLI does.
func ensureBase(ctx context.Context, d imageAPI, progress io.Writer) error {
	sums, err := d.ImageList(ctx, image.ListOptions{
		Filters: filters.NewArgs(filters.Arg("reference", hostImage())),
	})
	if err != nil {
		return err
	}
	root, rootErr := cliutil.FindRepoRoot("")
	// No checkout means no digest to compare and nothing to build from. An existing base is then
	// the best available answer and is used as-is — a cluster install has no repo (the Dockerfile
	// says as much), so this is the normal path there, not a degraded one.
	if rootErr != nil {
		if len(sums) > 0 {
			return nil
		}
		return fmt.Errorf("base image %s missing and no repo root to build it from: %w", hostImage(), rootErr)
	}
	want, err := sdkDigest(root)
	if err != nil {
		return err
	}
	if len(sums) > 0 {
		if got := sums[0].Labels[sdkLabel]; got == want {
			return nil
		} else if got == "" {
			fmt.Fprintf(os.Stderr, "base image %s predates the SDK stamp — rebuilding it from %s\n",
				hostImage(), root)
		} else {
			fmt.Fprintf(os.Stderr, "base image %s bakes SDK %s, checkout is %s — rebuilding it from %s\n",
				hostImage(), got[:12], want[:12], root)
		}
	} else {
		fmt.Fprintf(os.Stderr, "base image %s missing — building it from %s\n", hostImage(), root)
	}
	tarCtx, err := archive.TarWithOptions(root, &archive.TarOptions{ExcludePatterns: dockerignore(root)})
	if err != nil {
		return err
	}
	defer tarCtx.Close()
	return buildImage(ctx, d, progress, tarCtx, "control/images/Dockerfile.pyworker", hostImage(),
		map[string]string{sdkLabel: want})
}

// buildActor builds the per-actor image from the actor dir. A Dockerfile in the dir
// wins; otherwise the canonical two-liner is synthesized in memory and injected into
// the context tar — a deps-free actor never has to write one.
//
// ponytail: the synthesized Dockerfile stamps no ENTRYPOINT (infra/Dockerfile.pyworker
// expects the build step to add `ENTRYPOINT ["python3","/actor/<name>/actor.py"]`);
// compose/actors supplies the command today — stamp it here when bare `docker run`
// of an actor image must work.
func buildActor(ctx context.Context, d imageAPI, progress io.Writer, dir, name, tag string) error {
	tarCtx, err := archive.TarWithOptions(dir, &archive.TarOptions{ExcludePatterns: dockerignore(dir)})
	if err != nil {
		return err
	}
	if _, statErr := os.Stat(filepath.Join(dir, "Dockerfile")); statErr != nil {
		df := fmt.Sprintf("FROM %s\nCOPY . /actor/%s/\n", hostImage(), name)
		// deploy.sh is the actor's dependency install, and it is the SAME script the machine
		// Target runs over SSH on a bare Machine. Running it here is what keeps the two
		// Targets honest: an actor whose deps only exist in a Dockerfile cannot be placed on a
		// Machine, and one whose deps only exist in deploy.sh silently ships a broken image.
		// KONTRA_TARGET tells the script which side of that it is on.
		if _, err := os.Stat(filepath.Join(dir, "deploy.sh")); err == nil {
			df += fmt.Sprintf("ENV KONTRA_TARGET=container\nRUN sh /actor/%s/deploy.sh\n", name)
		}
		tarCtx = injectFile(tarCtx, "Dockerfile", []byte(df))
	}
	defer tarCtx.Close()
	return buildImage(ctx, d, progress, tarCtx, "Dockerfile", tag, nil)
}

// buildImage builds one tag. `labels` is stamped onto the result and may be nil; ensureBase uses
// it to record which SDK the layer actually contains, since the tag cannot say.
func buildImage(ctx context.Context, d imageAPI, progress io.Writer, buildCtx io.Reader, dockerfile, tag string, labels map[string]string) error {
	resp, err := d.ImageBuild(ctx, buildCtx, types.ImageBuildOptions{
		Tags:       []string{tag},
		Dockerfile: dockerfile,
		Labels:     labels,
		Remove:     true,
		Version:    types.BuilderV1, // classic builder — no BuildKit session needed
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := streamDockerOutput(progress, resp.Body); err != nil {
		return fmt.Errorf("build %s: %w", tag, err)
	}
	return nil
}

// dockerignore reads <dir>/.dockerignore into engine exclude patterns.
func dockerignore(dir string) []string {
	b, err := os.ReadFile(filepath.Join(dir, ".dockerignore"))
	if err != nil {
		return nil
	}
	var pats []string
	for _, line := range strings.Split(string(b), "\n") {
		l := strings.TrimSpace(line)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		pats = append(pats, l)
	}
	return pats
}

// streamDockerOutput prints the engine's JSON message stream (build or push) as plain
// lines on stdout and turns an in-stream {"error": …} into a Go error.
func streamDockerOutput(w io.Writer, r io.Reader) error {
	return streamDockerMessages(w, r, nil)
}

// streamPushOutput is streamDockerOutput plus the one thing a PUSH stream carries that a build
// does not: the manifest digest the registry returned, in the trailing `aux` message. That digest
// is the actor's identity under ADR 0032, and this is the only place it exists — it is computed by
// the registry over the bytes it accepted, so nothing local can produce it.
//
// An empty return is not an error: a registry that does not send `aux` is unusual but legal, and
// confirmPushed then asks the registry directly instead.
func streamPushOutput(w io.Writer, r io.Reader) (string, error) {
	var digest string
	err := streamDockerMessages(w, r, func(aux json.RawMessage) {
		var a struct {
			Digest string `json:"Digest"`
		}
		if json.Unmarshal(aux, &a) == nil && a.Digest != "" {
			digest = a.Digest
		}
	})
	return digest, err
}

func streamDockerMessages(w io.Writer, r io.Reader, onAux func(json.RawMessage)) error {
	dec := json.NewDecoder(r)
	for {
		var msg struct {
			Stream string          `json:"stream"`
			Status string          `json:"status"`
			Error  string          `json:"error"`
			Aux    json.RawMessage `json:"aux"`
		}
		if err := dec.Decode(&msg); err == io.EOF {
			return nil
		} else if err != nil {
			return err
		}
		if msg.Error != "" {
			return errors.New(msg.Error)
		}
		if msg.Stream != "" {
			fmt.Fprint(w, msg.Stream) // already newline-terminated
		}
		if msg.Status != "" {
			fmt.Fprintln(w, msg.Status)
		}
		if onAux != nil && len(msg.Aux) > 0 {
			onAux(msg.Aux)
		}
	}
}

// injectFile appends one synthetic file to a tar stream (the in-memory Dockerfile).
// Re-streamed through a pipe so a big actor dir never buffers whole in RAM.
func injectFile(src io.ReadCloser, name string, data []byte) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		defer src.Close()
		tr := tar.NewReader(src)
		tw := tar.NewWriter(pw)
		err := func() error {
			for {
				hdr, e := tr.Next()
				if e == io.EOF {
					break
				}
				if e != nil {
					return e
				}
				if e := tw.WriteHeader(hdr); e != nil {
					return e
				}
				if _, e := io.Copy(tw, tr); e != nil {
					return e
				}
			}
			if e := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data))}); e != nil {
				return e
			}
			if _, e := tw.Write(data); e != nil {
				return e
			}
			return tw.Close()
		}()
		pw.CloseWithError(err)
	}()
	return pr
}
