// up.go — `kontra up`, the appliance.
//
// The one command that does not talk to something already running: it IS the control plane. ADR
// 0031 collapses the compose services into this process one at a time. Five arrived as libraries:
// Temporal, embedded as one; the object store, an S3 API over the appliance's own data directory
// in place of SeaweedFS; the key-value store, the same wire protocol in place of the `redis`
// container; the payload codec, an HTTP handler that was a container for no reason other than
// needing an address; and the OCI registry, which is the one that did not need a store of its own
// — actor image layers are content-addressed, so they go in the CAS beside every other artifact
// (ADR 0032).
//
// THE SIXTH IS THE ONE THAT IS NOT A LIBRARY, and it is what makes this command the whole control
// plane rather than most of it. The orchestrator is ~60 TypeScript files with a compile-time
// congruence guard over the catalog contract; ADR 0031's first locked decision is that it is
// CARRIED, not rewritten. So it ships as a content-addressed bundle — a pinned Node runtime, the
// compiled JavaScript and its native addons — which `kontra up` hydrates out of the CAS on first
// run and execs as a supervised child (orchestrator.go, appliance/child.go). `orchestrator-api`
// left docker-compose.yml with this slice; nothing of the control plane is a container any more.
//
// `kontra infra up` (compose) and `kontra up` (the appliance) are two different verbs on purpose.
// The first drives a deployment contract written in YAML; this one is the contract.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/medmahmoudi26/kontra-local/cli/appliance"
	"github.com/medmahmoudi26/kontra-local/cli/appliance/codec"
	"github.com/medmahmoudi26/kontra-local/cli/appliance/kv"
	"github.com/medmahmoudi26/kontra-local/cli/appliance/objstore"
	"github.com/medmahmoudi26/kontra-local/cli/appliance/registry"
	"github.com/medmahmoudi26/kontra-local/cli/appliance/temporalsrv"
	"github.com/medmahmoudi26/kontra-local/handler/hydratestore"
)

func cmdUp(args []string) error {
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	dataDir := fs.String("data-dir", "", "where the appliance keeps its state (default: $KONTRA_HOME/data)")
	bind := fs.String("bind", "127.0.0.1", "IP the embedded services listen on")
	port := fs.Int("temporal-port", temporalsrv.DefaultPort, "Temporal gRPC port")
	metricsPort := fs.Int("metrics-port", 0, "Prometheus port for the embedded server (0 = pick a free one)")
	namespace := fs.String("namespace", "", "extra namespace to pre-create (\"default\" always is)")
	logLevel := fs.String("log-level", "warn", "embedded server log level: debug|info|warn|error")
	s3Port := fs.Int("s3-port", objstore.DefaultPort, "object-store (S3) port")
	s3Public := fs.String("s3-public-endpoint", "", "base URL presigned object URLs are signed for (default: the bound address)")
	kvPort := fs.Int("kv-port", kv.DefaultPort, "key-value (state store) port")
	codecPort := fs.Int("codec-port", codec.DefaultPort, "Temporal remote-codec port (the address a Web UI is told to call)")
	temporalUI := fs.Bool("temporal-ui", false, "also start Temporal's own Web UI, hydrated from the CAS (off: the common path stays one process)")
	temporalUIPort := fs.Int("temporal-ui-port", appliance.DefaultTemporalUIPort, "port Temporal's Web UI serves on")
	registryPort := fs.Int("registry-port", registry.DefaultPort, "OCI registry port (`kontra deploy` pushes here; `kontra scale` pulls from here)")
	kvMaxBytes := fs.Int64("kv-max-bytes", 0, "dataset ceiling for the key-value store in bytes (0 = 512 MiB, negative = unbounded)")
	orchestrator := fs.String("orchestrator", "", "which control plane to run: auto (default) | local | bundle | none | a path to a bundle .tar.gz or a built checkout (env KONTRA_ORCHESTRATOR; KONTRA_ORCHESTRATOR_BUNDLE and KONTRA_SPA_BUNDLE pin the artifacts)")
	apiPort := fs.Int("api-port", DefaultOrchestratorPort, "port the orchestrator serves its API and the SPA on")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("kontra up takes no arguments (got %q)", fs.Arg(0))
	}

	dir, err := applianceDataDir(*dataDir)
	if err != nil {
		return err
	}

	opts := temporalsrv.Options{
		DataDir:     dir,
		BindIP:      *bind,
		Port:        *port,
		MetricsPort: *metricsPort,
		LogLevel:    *logLevel,
	}
	if ns := strings.TrimSpace(*namespace); ns != "" {
		opts.Namespaces = []string{ns}
	}

	srv, err := temporalsrv.Start(opts)
	if err != nil {
		return err
	}
	defer srv.Stop()

	// The object store shares the data directory and the bind address, because it is the same
	// appliance: one thing to find, one thing to back up, one address to open or not open.
	//
	// NO EXTRA BUCKET IS PRE-CREATED ANY MORE. `kontra-bundles` used to be named here alongside
	// `kontra`, because a fleet's artifact bucket that nothing created is what made
	// `kontra fleet up` fail on its first push. A Bundle is an OCI artifact now (ADR 0036) and
	// goes to the registry five calls down, which creates a repository on first push — so the
	// first-run failure that rung existed for cannot happen, and the bucket it pre-created had
	// no writer left.
	store, err := objstore.Start(objstore.Options{
		DataDir:        dir,
		BindIP:         *bind,
		Port:           *s3Port,
		PublicEndpoint: *s3Public,
	})
	if err != nil {
		return err
	}
	defer store.Stop()

	// The state store, third into the same data directory and the same bind address. It comes
	// after the other two because it is the one whose port an operator is most likely to already
	// own — 6379 is where a developer's own redis-server lives — and failing here once they are up
	// means the refusal names the collision instead of being buried under a partial start.
	kv, err := kv.Start(kv.Options{
		DataDir:  dir,
		BindIP:   *bind,
		Port:     *kvPort,
		MaxBytes: *kvMaxBytes,
	})
	if err != nil {
		return err
	}
	defer kv.Stop()

	// The codec, over the store that holds the claim-checks. It is fourth because it is the only
	// one that needs another of them: a ref names a digest and not a location, so the codec has to
	// read the object store the writers wrote to, which is the one two calls up.
	//
	// WHICH BUCKET IS ASKED AND ANSWERED HERE, and that is the half of the codec that belongs to
	// this command rather than to a package. The bucket and prefix come from the same KONTRA_S3_*
	// the actors, the orchestrator and the handler read, because that is what they wrote under —
	// and this is already the file that reads them. A default inside the codec would be a SECOND
	// answer to "which bucket did the writers use", which is the shape of drift this whole slice
	// removes. The ENDPOINT still comes from nowhere at all: it is read off the listener below.
	claims, err := store.Backing(s3Bucket())
	if err != nil {
		return fmt.Errorf("the codec's bucket %q: %w", s3Bucket(), err)
	}

	// THE ONE BROWSER ORIGIN THE CODEC ADMITS IS DERIVED FROM THE UI'S OWN TWO FLAGS, not typed
	// beside them. CORS is the other half of the wire `--ui-codec-endpoint` used to be: the codec
	// answers with exactly one `Access-Control-Allow-Origin`, and if it is not the string in the
	// operator's address bar every decode fails silently. Empty when the UI is off, which leaves
	// the codec on its own default and admits nothing new.
	uiOrigin := ""
	if *temporalUI {
		uiOrigin = appliance.TemporalUIOrigin(*bind, *temporalUIPort)
	}
	codec, err := codec.Start(codec.Options{
		Store:    claims,
		BindIP:   *bind,
		Port:     *codecPort,
		Prefix:   envOr("KONTRA_S3_PREFIX", ""),
		UIOrigin: uiOrigin,
	})
	if err != nil {
		return err
	}
	defer codec.Stop()

	// THE ARTIFACT STORE, opened once and used twice. The registry keeps OCI layers in it and
	// hydration keeps the orchestrator bundle in it, and ADR 0031 §2 is explicit that these are
	// two customers of ONE store rather than two stores that happen to share a directory. Opening
	// it here — before the registry, which is the first user — is what makes that structural: the
	// registry is HANDED a store instead of finding one, so there is no second `NewLocal` call to
	// drift.
	artifacts, err := hydratestore.Open(dir)
	if err != nil {
		return err
	}

	// The registry, fifth, over the CAS in the same data directory. It is last because it is the
	// only one whose ADDRESS a different command has to find: `kontra deploy` tags an image with
	// it and `kontra scale` pulls by it, in two other processes, and registry.Start publishes the
	// bound address into the data directory for them to read. That file is the whole mechanism —
	// an address these three agree on by construction rather than by three matching defaults.
	registry, err := registry.Start(registry.Options{
		DataDir: dir,
		BindIP:  *bind,
		Port:    *registryPort,
		Store:   artifacts.CAS(),
	})
	if err != nil {
		return err
	}
	defer registry.Stop()

	// THE ADDRESSES, PUBLISHED FOR THE COMMANDS THAT ARE NOT THIS ONE. `kontra deploy` already
	// read the registry's out of this directory; `kontra serve --mode docker` needs the other
	// four, because a worker container has to be TOLD where Temporal, the object store and the
	// state store are and the compose names it used to be given resolve to nothing now
	// (appliance/appliance.go). Written here, once every listener is bound, so the record holds
	// what was ACTUALLY bound rather than what was asked for — `--temporal-port 0` is a real
	// thing an operator types.
	//
	// The API line is missing on purpose until the child is up, a few dozen lines below: a first
	// run hydrates 450 MB before that process exists, and a record naming an address nothing
	// answers is worse than one that admits it has no API yet.
	endpoints := appliance.Endpoints{
		Bind:     *bind,
		Temporal: srv.Address(),
		S3:       store.Endpoint(),
		KV:       kv.Address(),
		Codec:    codec.Endpoint(),
		Registry: registry.Address(),
		PID:      os.Getpid(),
	}
	if err := appliance.WriteEndpoints(dir, endpoints); err != nil {
		return err
	}
	// Withdrawn on the way out so the next `kontra deploy` on this data directory does not
	// resolve a dead address. A crash skips this, which ReadEndpoints' header explains is the
	// failure worth having.
	defer func() { _ = appliance.RemoveEndpoints(dir) }()

	printApplianceReady(os.Stdout, srv, store, kv, codec, registry)

	// THE SIXTH THING IS NOT A LIBRARY. Everything above this line is linked into this process;
	// the orchestrator is carried as an artifact and exec'd as a child (ADR 0031 §1, locked
	// decision 1). It comes last for the obvious reason and one less obvious one: it is the only
	// piece that has to be TOLD the other five's addresses, and they are not known until they are
	// bound.
	//
	// THE SIGNAL HANDLER IS INSTALLED FIRST, before hydration rather than after it. A first run
	// expands 450 MB and an operator who changes their mind during it should get a stop, not a
	// half-written working directory abandoned by a default-SIGINT death.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	// TEMPORAL'S OWN WEB UI, AND ONLY IF ASKED (ADR 0031, issue 16). It is a second process and a
	// second artifact, so the flag being off means nothing here runs at all — that is
	// startTemporalUI's first line rather than this block's position. It comes BEFORE the
	// orchestrator because it is the window you open when the control plane is the thing
	// misbehaving, and after the signal handler because a first run fetches ~9 MB.
	ui, err := startTemporalUI(context.Background(), *temporalUI, appliance.TemporalUIOptions{
		DataDir:         dir,
		TemporalAddress: srv.Address(),
		BindIP:          *bind,
		Port:            *temporalUIPort,
		// HANDED THE SERVER, NOT AN ADDRESS. There is no second place to spell the codec's
		// endpoint, so there is nothing left for the two of them to disagree about.
		Codec:     codec,
		Namespace: temporalNamespace(),
		Store:     artifacts,
		Progress:  os.Stdout,
		Log:       os.Stdout,
	})
	if err != nil {
		return err
	}
	if ui != nil {
		// Stopped before the services it reads, like every other deferred stop here.
		defer ui.Stop(appliance.DefaultStopTimeout)
		printTemporalUI(os.Stdout, ui)
		reportTemporalUIExit(os.Stderr, ui)
	}

	repo, _ := findRepoRoot("")
	src, err := resolveOrchestrator(context.Background(), orchestratorOptions{
		Mode:     *orchestrator,
		DataDir:  dir,
		RepoRoot: repo,
		Store:    artifacts,
		Progress: os.Stdout,
	})
	if err != nil {
		return err
	}
	printOrchestratorChoice(os.Stdout, src)

	var child *appliance.Child
	if src.Kind != "none" {
		home, _ := kontraRoot()
		env, replaced := orchestratorEnv(os.Environ(), orchestratorEnvOptions{
			TemporalAddress: srv.Address(),
			S3Endpoint:      store.Endpoint(),
			S3Public:        strings.TrimSpace(*s3Public),
			S3Bound:         store.Endpoint(),
			KVAddress:       kv.Address(),
			DataDir:         dir,
			Port:            *apiPort,
			KontraHome:      home,
			BindIP:          *bind,
			// ALL THREE ROLES, and the ADR is explicit about why this process may hold the third
			// one: it ships no Pulumi engine, so the hazard that kept `infra` in its own container
			// is not in here (ADR 0031 §4). A deployment that keeps a compose provisioner beside
			// this one sets KONTRA_ORCHESTRATOR_ROLES itself and that value is left alone.
			Roles: "api,materializer,infra",
		})
		for _, r := range replaced {
			fmt.Fprintf(os.Stdout, "%-22s %s\n", "", "overriding "+r)
		}
		child, err = appliance.StartChild(appliance.ChildOptions{
			Name: "orchestrator",
			Path: src.Node,
			Args: []string{src.Entry},
			Dir:  src.Dir,
			Env:  env,
			Log:  os.Stdout,
		})
		if err != nil {
			return err
		}
		// THE ADDRESS IT ACTUALLY BOUND, not a conventional one. This line said `127.0.0.1`
		// whatever `--bind` was, which on `kontra up --bind 172.17.0.1` — the form an operator
		// needs for worker containers — printed a URL that answers nothing.
		fmt.Fprintf(os.Stdout, "%-22s http://%s:%d  (pid %d)\n", "Control plane:", *bind, *apiPort, child.PID())
		// The record gains its sixth address now that something answers on it. A second atomic
		// write, not a mutation: a reader either sees the five or the six, never a half.
		endpoints.API = fmt.Sprintf("http://%s:%d", *bind, *apiPort)
		if err := appliance.WriteEndpoints(dir, endpoints); err != nil {
			return err
		}
	}
	fmt.Fprintln(os.Stdout, "\nCtrl-C to stop.")

	// TWO WAYS THIS ENDS, AND BOTH OF THEM SAY SO. The operator stops us, or the child dies —
	// and a `kontra up` that outlives its control plane is the hollow failure this slice exists
	// to not ship: ports still bound, banner still on screen, API answering nothing.
	if child == nil {
		<-stop
		fmt.Fprintln(os.Stderr, "\nstopping the appliance…")
		return nil
	}

	select {
	case <-stop:
		fmt.Fprintln(os.Stderr, "\nstopping the appliance…")
		// THE CHILD FIRST, AND NOT BY ACCIDENT. It holds the DuckLake catalog lock and a Temporal
		// connection; stopping the server underneath it would turn an orderly shutdown into a
		// worker unwinding against a dead frontend, and the lock is released on ITS exit path.
		// The embedded services stop after this returns, in the deferred calls above.
		if err := child.Stop(appliance.DefaultStopTimeout); err != nil {
			// Reported, not returned: the appliance is still shutting down and the deferred stops
			// must run. An operator who reads this line has an orphan to look at.
			fmt.Fprintf(os.Stderr, "  %v\n", err)
		}
		return nil
	case <-child.Done():
		err := child.Err()
		// Best effort: the process is gone, but anything it started shares its group and is not.
		if stopErr := child.Stop(2 * time.Second); stopErr != nil {
			fmt.Fprintf(os.Stderr, "  %v\n", stopErr)
		}
		fmt.Fprintln(os.Stderr, "\nstopping the appliance…")
		return err
	}
}

// printApplianceReady says what came up and, more usefully, what to point at it. A control plane
// that starts without telling you its address is one `docker inspect` away from being unusable.
func printApplianceReady(w *os.File, srv *temporalsrv.Server, store *objstore.Server, kv *kv.Server, codec *codec.Server, registry *registry.Server) {
	fmt.Fprintf(w, "%-22s %s\n", "Temporal (embedded):", srv.Address())
	fmt.Fprintf(w, "%-22s %s\n", "Temporal persistence:", srv.DatabasePath())
	fmt.Fprintf(w, "%-22s http://%s/metrics\n", "Temporal metrics:", srv.MetricsAddress())
	fmt.Fprintf(w, "%-22s %s\n", "Object store (S3):", store.Endpoint())
	fmt.Fprintf(w, "%-22s %s\n", "Object store data:", filepath.Join(store.DataDir(), "objects"))
	fmt.Fprintf(w, "%-22s %s (%d keys)\n", "State store:", kv.Address(), kv.Keys())
	fmt.Fprintf(w, "%-22s %s\n", "State store data:", kv.LogPath())
	// PRINTED BECAUSE IT IS DERIVED. This is the value that used to be `--ui-codec-endpoint` in
	// docker-compose.yml, kept in step by hand with the codec container's published port; the
	// binary reads it off its own listener. A Temporal UI (opt-in, a later slice) is handed this,
	// and `temporal workflow show --codec-endpoint <this>` decodes an offloaded payload with it.
	fmt.Fprintf(w, "%-22s %s\n", "Payload codec:", codec.Endpoint())
	// The registry gets no `export` line below, deliberately, and it is the only address here that
	// does not: `kontra deploy` and `kontra scale` READ it from this data directory, so there is
	// nothing for an operator to keep in step. KONTRA_REGISTRY exists to point them at a DIFFERENT
	// registry; printing it here would invite someone to pin the one they already have, which is
	// how a moved port becomes a deploy to a registry nobody is serving.
	//
	// The second line names the CAS rather than a volume, which is what `registry-data` leaving
	// compose actually means on disk: layers sit beside every other artifact the appliance holds.
	fmt.Fprintf(w, "%-22s %s\n", "Actor registry:", registry.Address())
	// THE SECOND ADDRESS, PRINTED ONLY WHEN THERE IS ONE. On a bind that is not loopback the
	// registry serves two, and the line above names the one push and pull use — because the
	// Docker daemon refuses plain HTTP to anything else. Saying so here is what stops the pair of
	// addresses reading as a bug.
	if bound := registry.BoundAddress(); bound != registry.Address() {
		fmt.Fprintf(w, "%-22s %s  (the same registry; the line above is what `docker push` will accept)\n", "", bound)
	}
	fmt.Fprintf(w, "%-22s %s\n", "Registry layers:", registry.CASRoot())
	fmt.Fprintf(w, "\n  export KONTRA_ADDRESS=%s\n", srv.Address())
	fmt.Fprintf(w, "  export KONTRA_S3_ENDPOINT=%s\n", store.Endpoint())
	fmt.Fprintf(w, "  export KONTRA_REDIS_HOST=%s\n\n", kv.Address())
}

// applianceDataDir resolves the directory the binary owns.
//
// `$KONTRA_HOME/data` rather than a sibling of the checkout, because KONTRA_HOME is already THE
// answer to "where does this installation keep its things" — config.yaml, workflows/ and actors/
// are there, resolved by the same three-answer rule in kontraRoot, and the orchestrator resolves
// the identical path from TypeScript. A second location for the same installation is a second
// thing to find, back up and get wrong.
// KONTRA_DATA_DIR is the SAME variable `backend/src/data/dataDir.ts` reads, and it is what
// keeps the two halves of one installation pointing at one directory: the binary hands it to the
// orchestrator child, and docker-compose.yml sets it to the container path of the volume. A
// second spelling of "where does this installation keep its things" is the trap `sources.ts`
// already records paying for once.
func applianceDataDir(override string) (string, error) {
	if override == "" {
		override = os.Getenv("KONTRA_DATA_DIR")
	}
	if override != "" {
		abs, err := filepath.Abs(override)
		if err != nil {
			return "", fmt.Errorf("--data-dir %s: %w", override, err)
		}
		return abs, nil
	}
	root, err := kontraRoot()
	if err != nil {
		return "", errors.New("no data directory: " + err.Error() + " (or pass --data-dir)")
	}
	return filepath.Join(root, "data"), nil
}
