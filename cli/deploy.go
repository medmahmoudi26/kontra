// deploy.go — `kontra deploy --actor <dir>`: build (and optionally publish) an actor image.
//
// ONE BUILD PATH, AND IT IS `pack`. An actor is layered onto a published runtime image by the CNB
// lifecycle; there is no Dockerfile here, no base image this repo builds, no cached worker base and
// no handler recompile per deploy. ADR 0061 records the decision and ADR 0063 the removal of what
// it replaced.
//
// IT SHELLS OUT, AND THAT IS A CHANGE OF BOUNDARY RATHER THAN AN EROSION OF ONE. The file's old
// rule was "no shell-out to `docker build`, the Engine API is the boundary here"; the CNB lifecycle
// is five phases in separate containers with a credential boundary between them, which is not a
// build request an Engine API call can express. `cli/packbuild.go` carries the reasoning.
//
// WHAT THE OLD PATH COST, for the record: the classic builder it used left 28 dangling
// intermediates averaging 2.2 GiB on the box this was written on, and a Go actor's build context
// was the REPO ROOT — because its `replace` directives pointed outside the actor — so any edit
// anywhere invalidated it. The build context is now the actor's own directory, which is why a
// `replace` that points above it is refused rather than worked around.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/image"
	docker "github.com/docker/docker/client"

	"github.com/medmahmoudi26/kontra/cli/internal/cliio"
	"github.com/medmahmoudi26/kontra/cli/internal/cliutil"
	"github.com/medmahmoudi26/kontra/cli/internal/ociref"
)

// defaultRegistryPort is the port actor images are pushed to and pulled from — zot's, and
// `registry:2`'s before it (docker-compose.yml `KONTRA_REGISTRY_PORT:-5000`).
//
// 5000 and not a new number, because the port is half of an image REFERENCE: every
// `kontra/<name>:<ver>` already pushed is spelled `localhost:5000/<name>:<ver>`, and moving it
// would silently orphan all of them.
const defaultRegistryPort = 5000

// defaultRegistry is the last answer to "which registry", used when nothing else says. It is a
// FALLBACK and not the answer — see registryAddress.
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
//	KONTRA_REGISTRY       the environment said so — what docker-compose.yml sets for every
//	                      service, so an install that moved KONTRA_REGISTRY_PORT is followed
//	                      rather than guessed at
//	defaultRegistry       nothing said; say the conventional thing and let the reachability
//	                      check produce the message
func registryAddress(flagVal string) string {
	if v := strings.TrimSpace(flagVal); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("KONTRA_REGISTRY")); v != "" {
		return v
	}
	return defaultRegistry
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

// runPackBuild is the seam `kontra deploy`'s own tests build against: `packBuild` shells out to a
// pinned `pack` and runs a five-phase lifecycle in containers, which a unit test may not do. The
// end-to-end path is `scripts/parity-gate.sh`'s, against a real install.
var runPackBuild = packBuild

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
	// Runtime is the run image this actor is built on, as a name and a MAJOR (`python-browser:1`) or
	// a fully qualified reference. Absent means the default for the engine — `python:1` or `base:1` —
	// which is what every actor written before runtimes existed gets.
	//
	// A MAJOR AND NOT A VERSION, because the point is that the runtime can be patched underneath an
	// actor without rebuilding it (`kontra rebase`). The digest it resolves to at build time is
	// recorded in the catalog; the major is what is asked for next time.
	Runtime string `json:"runtime"`
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
	hostOnly bool   // build the image but do not publish it
	override bool   // replace an already-deployed version instead of refusing
	// dockerConfig is a directory holding a `config.json` with the registry push credential, handed
	// to `pack` as DOCKER_CONFIG. Empty means anonymous, which an authenticated registry refuses.
	dockerConfig string
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

	// 1) THE RUNTIME. An actor is layered onto a published runtime image rather than onto a base
	// this repo builds, and the digest it resolves to is what the catalog records — so a runtime can
	// be patched underneath an actor and `kontra rebase` can move it without a rebuild (ADR 0061).
	rt, err := resolveRuntime(reg, m.Runtime, engine)
	if err != nil {
		return nil, err
	}
	// THE TRUST GATE, BEFORE THE BUILD AND NOT AFTER IT. A runtime the policy would refuse at pull
	// time must not cost a buildpack build first.
	if err := admitRuntime(ctx, rt, progress); err != nil {
		return nil, err
	}
	fmt.Fprintf(progress, "runtime %s:%d → %s\n", rt.Name, rt.Major, rt.Pinned())

	// 2) THE TWO REFUSALS THE BUILDPACK LAYOUT NEEDS, both before anything is built.
	switch deployShellCheck(o.actorDir, os.Getenv("KONTRA_DEPLOY_SH")) {
	case deployShellRefuse:
		return nil, errors.New(deployShellMessage)
	case deployShellWarn:
		fmt.Fprintf(progress, "warning: %s\n", deployShellMessage)
	}
	if engine == "go" {
		gomod, rerr := os.ReadFile(filepath.Join(o.actorDir, "go.mod"))
		if rerr == nil {
			if directive, line := outsideReplace(string(gomod)); directive != "" {
				return nil, fmt.Errorf("%s/go.mod:%d points outside the actor's own directory:\n    %s\n"+
					"  The build context is the actor directory, so a `replace` above it cannot resolve. "+
					"Vendor what it points at, or publish the module.", o.actorDir, line, directive)
			}
		}
	}

	// 3) THE BUILD, WHICH IS `pack` AND NOTHING ELSE. One image, published directly by the
	// lifecycle: there is no separate host image to layer a worker onto any more, so there is no
	// second build, no cached worker base, and no handler recompile per deploy.
	//
	// `--host-only` BECOMES "DO NOT PUBLISH". It used to mean "build the actor image and stop
	// before the worker image"; with one image the only thing left to stop before is the push, and
	// that is the useful half — it is what lets an author check a build without claiming a version.
	res := &deployResult{Name: m.Name, Version: m.Version, HostImage: remote}
	built, err := runPackBuild(ctx, packOpts{
		Image:        remote,
		ActorDir:     o.actorDir,
		Builder:      pinnedBuilder(),
		RunImage:     rt.Pinned(),
		CacheImage:   fmt.Sprintf("%s/%s:cache", reg, m.Name),
		Registry:     reg,
		DockerConfig: o.dockerConfig,
		Publish:      !o.hostOnly,
		Progress:     progress,
	})
	if err != nil {
		return nil, err
	}
	if o.hostOnly {
		return res, nil
	}
	res.Image = remote
	res.Registry = reg

	// THE ADDRESS CHECK, AND IT BELONGS HERE RATHER THAN AT SCALE TIME. `pack` has just reported a
	// digest for what it published to `reg` AS IT RESOLVES IT; this asks the registry THIS PROCESS
	// reaches at `reg` what that tag resolves to now. Agreement means push and pull are the same
	// registry, which is the property `kontra scale` is about to depend on. A disagreement is caught
	// one second after the push that caused it, naming the address — instead of arriving minutes
	// later as `no such image`, which sends an operator to look at Docker rather than at which
	// registry they are talking to.
	digest, err := confirmPushed(reg, m.Name, m.Version, remote, built)
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
	// THE HINT NAMES THE INSTALL, NOT A `docker run`. The registry is a component of the control
	// plane (ADR 0032); telling an operator to hand-start a `registry:2` container was the shape of
	// the thing this replaced — "a control plane that asks you to hand-start one of its own
	// components is not installed, it is assembled".
	hint := "\n  start it:   docker compose up -d registry"
	if reg != defaultRegistry {
		hint = "\n  this address came from --registry or KONTRA_REGISTRY; unset it for the install's own"
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
