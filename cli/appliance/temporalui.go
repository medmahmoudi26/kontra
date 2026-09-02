// temporalui.go — Temporal's own Web UI, opt-in, hydrated from the CAS (ADR 0031, issue 16).
//
// Embedding `go.temporal.io/server` gives gRPC and nothing else: the Web UI is a SEPARATE server
// in a separate repository (temporalio/ui-server), with its own release artifacts and its own
// embedded asset bundle. `temporal server start-dev` runs both and looks like one thing; the
// library does not. So the UI is not "a flag on the embedded server" — it is a second program,
// and this file is what fetches, verifies, places and supervises it.
//
// WHY IT IS OPT-IN RATHER THAN GONE, and the reason is specific enough to be worth writing down.
// kontra's own surfaces answer "what is this run doing" better than Temporal's do, and four of
// them exist. What they do NOT yet answer is the failure this repo has already paid for: a node
// whose Units all failed reporting `completed` with empty output, visible only through
// `temporal workflow show`. Stack traces, pending-activity detail and a manual signal/terminate
// console are the raw view, and until the transcript work makes those facts first-class the raw
// view is the fallback. OFF by default, because the common path should stay one process; ON with
// one flag, because the day you need it is not the day to be building it.
//
// WHY IT IS AN ARTIFACT AND NOT A LINKED LIBRARY. `ui-server` could be imported — it is Go. It
// would drag its own `go.temporal.io/api` and `go.temporal.io/server` constraints into a module
// graph that is already pinned to a 1.32 pre-release for reasons temporalsrv/temporal.go's
// header explains at length, it would put ~26 MB of embedded browser assets in every kontra
// binary whether or not
// anyone ever asks for them, and it would make the UI's release cadence a reason to rebuild the
// control plane. A pinned tarball costs one table of digests and is fetched once, ever, on the
// machines that want it.
//
// THE CODEC ADDRESS IS THE FRAGILE PART, AND IT IS HANDED OVER RATHER THAN CONFIGURED. The UI
// calls the codec FROM THE BROWSER, so what it needs is a browser-reachable address and not an
// internal one. In compose that was two values in two files — the codec container's published
// port and `--ui-codec-endpoint` on the temporal service — which had to be kept equal BY HAND,
// and whose drift showed up as the UI silently rendering `$ref` blobs nobody could read. Here
// {@link TemporalUIOptions} takes the *codec.Server itself: the endpoint is read off the listener
// that is actually bound (codec.Server.Endpoint()), and there is no field on this struct into which
// a second, disagreeing address could be typed.
//
// THE OTHER HALF OF THAT WIRE IS CORS, and it is derived from the same two values. The codec
// admits exactly one browser origin; that origin is where the UI is served from, which is
// {@link TemporalUIOrigin}(bind, port) — the same string this file prints and the same string
// `kontra up` hands codec.Start. One derivation, one printed URL, one admitted origin.
//
// BOTH PORTS BIND LOOPBACK (ADR 0031 §3). The UI is an unauthenticated read-write console over
// every workflow in the namespace — its signal/terminate buttons are the point of it — so the
// posture that makes it safe is that there is no remote caller at all.
package appliance

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/medmahmoudi26/kontra/cli/appliance/bundle"
	"github.com/medmahmoudi26/kontra/cli/appliance/codec"
	"github.com/medmahmoudi26/kontra/cli/appliance/temporalsrv"
	"github.com/medmahmoudi26/kontra/runtime/handler/hydratestore"
)

// DefaultTemporalUIPort is 8233, which is where `temporal server start-dev` serves its UI.
//
// The number is not ours and that is the whole reason to keep it: a bookmark, a tunnel and
// `codecserver.DefaultUIOrigin` — which spells out `http://localhost:8233` as the origin the
// codec admits — all already point here. A different port would be a working UI that every
// existing habit misses, and a CORS default that silently stopped matching.
const DefaultTemporalUIPort = 8233

// TemporalUIVersion is the pinned release of temporalio/ui-server.
//
// It is NOT tied to the embedded server's version and does not need to be: the UI speaks the
// public gRPC API, which is versioned separately and compatibly. Bump it on its own schedule.
const TemporalUIVersion = "2.53.3"

// temporalUIBinary is the executable inside the release tarball. The archive is flat — LICENSE,
// README.md and this — so there is nothing to strip.
const temporalUIBinary = "ui-server"

// temporalUIDigests is version → "goos/goarch" → sha256 of the upstream release tarball.
//
// SAME SHAPE, SAME REASON as nodeDigests in bundle/pins.go: keyed by VERSION FIRST, so
// bumping TemporalUIVersion without pasting its row cannot reach the network at all. The failure
// mode this shape exists to prevent is the one install.sh has — a version bumped in one variable
// and a digest left in another, which still fetches, still runs, and fails at the checksum with
// no statement of what was expected of whom.
//
// THE DIGESTS ARE UPSTREAM'S OWN, from the `ui-server_<version>_checksums.txt` asset on the
// release. Not computed from a tarball somebody already had on disk: a checksum taken from the
// file you already fetched checks the copy, not the artifact.
//
// Four platforms, matching ADR 0031 §6. Upstream also publishes windows/amd64 and windows/arm64;
// they are left out because the appliance does not target Windows, and a pin nobody runs is a pin
// nobody notices has gone stale.
var temporalUIDigests = map[string]map[string]string{
	"2.53.3": {
		"linux/amd64":  "ec160cab0235f300444e66a68ca90599e21419c4294c6f11553fad4c60f634ad",
		"linux/arm64":  "371905d70bcfe2e724bed8f549e15449aa073b8b9991a0530c6eb0617da9e856",
		"darwin/amd64": "91d6c0a236258cd6873856cc9e8c7f4b90036d9b2898767bab60d87cea2d2927",
		"darwin/arm64": "64904efd4d72a0038eb4917628b713ccfa50d20028eb242c3e7283b7f1f090e9",
	},
}

// TemporalUIPin resolves the pinned UI server for a platform, or explains which edit is missing.
func TemporalUIPin(p bundle.Platform) (bundle.Pin, error) { return temporalUIPin(TemporalUIVersion, p) }

// temporalUIPin is TemporalUIPin with the version as an argument, so a test can ask what happens
// to a version nobody has added digests for — a question that cannot be asked of a `const`, and
// the exact failure the table shape exists to produce.
func temporalUIPin(version string, p bundle.Platform) (bundle.Pin, error) {
	byPlatform, ok := temporalUIDigests[version]
	if !ok {
		return bundle.Pin{}, fmt.Errorf(
			"temporal ui-server %s has no pinned digests: TemporalUIVersion was bumped without its row in temporalUIDigests.\n"+
				"  add it from %s (pinned versions: %s)",
			version, temporalUIChecksumsURL(version), strings.Join(sortedKeys(temporalUIDigests), ", "))
	}
	digest, ok := byPlatform[p.String()]
	if !ok {
		return bundle.Pin{}, fmt.Errorf(
			"temporal ui-server %s is not pinned for %s; pinned platforms are %s.\n"+
				"  add the digest from %s",
			version, p, strings.Join(sortedKeys(byPlatform), ", "), temporalUIChecksumsURL(version))
	}
	return bundle.Pin{
		Name:    "temporal-ui-server",
		Version: version,
		// UPSTREAM NAMES ITS ARTIFACTS IN GO'S OWN VOCABULARY — `linux_amd64`, `darwin_arm64` —
		// so unlike Node (`linux-x64`) there is no second naming to map. Written as GOOS_GOARCH
		// rather than through a table because inventing one here would be a table with nothing in
		// it to get wrong.
		URL: fmt.Sprintf("https://github.com/temporalio/ui-server/releases/download/v%s/ui-server_%s_%s_%s.tar.gz",
			version, version, p.OS, p.Arch),
		Digest: digest,
	}, nil
}

func temporalUIChecksumsURL(version string) string {
	return fmt.Sprintf("https://github.com/temporalio/ui-server/releases/download/v%s/ui-server_%s_checksums.txt", version, version)
}

// TemporalUIOrigin is the browser origin the UI is served from — the ONE string that is printed
// to the operator, admitted by the codec's CORS, and dialled by a readiness check.
//
// LOOPBACK IS SPELLED `localhost`, NOT `127.0.0.1`, and the two are different origins to a
// browser. The address bar is what sets the `Origin` header, `codecserver.DefaultUIOrigin` is
// already `http://localhost:8233`, and `temporal server start-dev` prints `localhost` for the
// same listener — so this is the spelling every existing bookmark and every existing default
// already agrees on. The LISTENER is still 127.0.0.1; this is a name for it, not a second bind.
//
// A non-loopback bind (a controller an operator browses to over the VPC) keeps its address as
// written, because there is no name for it that both halves would resolve the same way.
func TemporalUIOrigin(bindIP string, port int) string {
	host := strings.TrimSpace(bindIP)
	if host == "" || host == "127.0.0.1" {
		host = "localhost"
	}
	if port == 0 {
		port = DefaultTemporalUIPort
	}
	return "http://" + net.JoinHostPort(host, strconv.Itoa(port))
}

// TemporalUIOptions configures the opt-in Web UI.
//
// NOTHING HERE IS A CODEC ADDRESS, deliberately — see the file header. Codec is the server
// itself, so the endpoint the browser is told to call is read off the listener that is bound.
type TemporalUIOptions struct {
	// DataDir is the appliance's data directory: the CAS the artifact is hydrated from and into,
	// shared with the registry and the orchestrator bundle (ADR 0031 §2). Required.
	DataDir string

	// TemporalAddress is the embedded server's frontend, host:port — temporalsrv.Server.Address().
	// Required: a UI with no server behind it starts, serves a page, and answers every query with
	// a transport error, which is a worse failure than not starting.
	TemporalAddress string

	// BindIP is the address the UI listens on. Empty means 127.0.0.1 (ADR 0031 §3).
	BindIP string

	// Port is the UI's HTTP port; 0 means DefaultTemporalUIPort.
	Port int

	// Codec is the appliance's codec server, or nil for a UI that cannot decode offloaded
	// payloads. Handed over rather than configured: the endpoint comes off its listener.
	Codec *codec.Server

	// Namespace is the one the UI opens on. Empty means temporalsrv.DefaultNamespace.
	Namespace string

	// Store is an already-open artifact store. When nil, one is opened on DataDir — `kontra up`
	// passes its own so the registry, the orchestrator bundle and this address one set of bytes.
	Store *hydratestore.Store

	// Progress is where a first run reports its download. nil is silent.
	Progress io.Writer

	// Log is where the UI process's own output goes, tagged. nil discards.
	Log io.Writer

	// ReadyTimeout bounds the wait for the UI's first answer; 0 means temporalUIReadyTimeout.
	ReadyTimeout time.Duration
}

// temporalUIReadyTimeout is how long the UI gets to answer its own settings endpoint. It is a
// Go binary with embedded assets and no schema to migrate, so a healthy start is well under a
// second; the margin is for a cold page cache on a loaded box.
const temporalUIReadyTimeout = 30 * time.Second

// HydratedTemporalUI is a placed UI server: an executable and the digest that names it.
type HydratedTemporalUI struct {
	// Root is the working directory the artifact was materialized into, named by its digest.
	Root   string
	Digest string

	// Binary is the absolute path of the `ui-server` executable inside Root. Checked to exist
	// before this struct is returned — a release whose layout changed has to fail here, not
	// later as an exec error nobody can read.
	Binary string

	// Version is the pin this came from, and Pin is where it was fetched from.
	Version string
	Pin     bundle.Pin

	// Fresh is true when THIS call did the work; false is the second `kontra up`, which does
	// none at all.
	Fresh   bool
	Method  hydratestore.Method
	Files   int
	Bytes   int64
	Elapsed time.Duration
}

// TemporalUI is a running Web UI.
type TemporalUI struct {
	child     *Child
	address   string
	origin    string
	codec     string
	namespace string
	temporal  string
	config    string
	art       *HydratedTemporalUI
}

// Address is host:port — the bound listener.
func (u *TemporalUI) Address() string { return u.address }

// URL is what an operator opens, and Origin is the same string. They are one value because the
// codec admits exactly one origin and it has to be the one the browser will send; printing a URL
// that differs from the admitted origin by so much as `127.0.0.1` vs `localhost` is the drift
// this whole slice was told to remove.
func (u *TemporalUI) URL() string    { return u.origin }
func (u *TemporalUI) Origin() string { return u.origin }

// CodecEndpoint is the address the UI's browser calls to decode offloaded payloads, or "" when
// the UI was started without a codec. Read off the codec's listener, never configured.
func (u *TemporalUI) CodecEndpoint() string { return u.codec }

// TemporalAddress is the server this UI reads, and Namespace is the one it opens on.
func (u *TemporalUI) TemporalAddress() string { return u.temporal }
func (u *TemporalUI) Namespace() string       { return u.namespace }

// ConfigPath is the file the UI was started with. Named so an operator can read what it was
// actually told, rather than what this file says it tells it.
func (u *TemporalUI) ConfigPath() string { return u.config }

// Artifact is the hydration this UI runs out of: the digest, the working directory, and whether
// this start had to do any work.
func (u *TemporalUI) Artifact() *HydratedTemporalUI { return u.art }

// PID is the UI process.
func (u *TemporalUI) PID() int { return u.child.PID() }

// Done closes when the UI process has exited; Err is why.
//
// A DEAD UI DOES NOT STOP THE APPLIANCE, and that is the one place this child differs from the
// orchestrator. The orchestrator IS the control plane and `kontra up` outliving it is a hollow
// failure; the UI is a diagnostic window onto a control plane that keeps working without it. Its
// death is logged, loudly, and the appliance carries on.
func (u *TemporalUI) Done() <-chan struct{} { return u.child.Done() }
func (u *TemporalUI) Err() error            { return u.child.Err() }

// Stop shuts the UI down. Safe to call twice, and safe after it has already exited.
func (u *TemporalUI) Stop(timeout time.Duration) error { return u.child.Stop(timeout) }

// StartTemporalUI hydrates the pinned UI server and starts it, returning once it answers.
func StartTemporalUI(ctx context.Context, opts TemporalUIOptions) (*TemporalUI, error) {
	if opts.DataDir == "" {
		return nil, errors.New("appliance: the Temporal UI needs a data directory (it is hydrated into the appliance's CAS)")
	}
	if strings.TrimSpace(opts.TemporalAddress) == "" {
		return nil, errors.New("appliance: the Temporal UI needs the embedded server's address (a UI with nothing behind it answers every query with a transport error)")
	}
	if opts.BindIP == "" {
		opts.BindIP = "127.0.0.1"
	}
	if net.ParseIP(opts.BindIP) == nil {
		return nil, fmt.Errorf("appliance: bind address %q is not an IP (use 127.0.0.1, not a hostname)", opts.BindIP)
	}
	if opts.Port == 0 {
		opts.Port = DefaultTemporalUIPort
	}
	if opts.Namespace == "" {
		opts.Namespace = temporalsrv.DefaultNamespace
	}
	if opts.ReadyTimeout <= 0 {
		opts.ReadyTimeout = temporalUIReadyTimeout
	}

	// THE PORT CHECK IS THE ERROR MESSAGE, exactly as it is for the frontend port. 8233 is
	// `temporal server start-dev`'s UI port, so the process most likely to be holding it is
	// another Temporal — and "the appliance is broken" and "you are running two of these" are one
	// sentence apart.
	if err := checkPortFree(opts.BindIP, opts.Port); err != nil {
		return nil, fmt.Errorf("appliance: %s:%d is already listening — another Temporal Web UI (a `temporal server start-dev`, or an older compose stack?) has it: %w",
			opts.BindIP, opts.Port, err)
	}

	art, err := HydrateTemporalUI(ctx, opts)
	if err != nil {
		return nil, err
	}

	address := hostPort(opts.BindIP, opts.Port)
	origin := TemporalUIOrigin(opts.BindIP, opts.Port)
	codecEndpoint := ""
	if opts.Codec != nil {
		// THE DERIVATION, IN ONE EXPRESSION. Not a field, not an option, not a second default:
		// the address the browser is told to call is the address the codec's listener is bound to.
		codecEndpoint = opts.Codec.Endpoint()
	}

	runDir := filepath.Join(opts.DataDir, "temporal-ui", "run")
	configPath, err := writeTemporalUIConfig(runDir, temporalUIConfig{
		TemporalGRPCAddress: opts.TemporalAddress,
		Host:                opts.BindIP,
		Port:                opts.Port,
		EnableUI:            true,
		DefaultNamespace:    opts.Namespace,
		// NO NEWS FETCH. The UI otherwise calls out to temporal.io on load to ask whether a newer
		// version exists — an outbound request from a control plane that may be on a private
		// network, made on behalf of an operator who asked for a local console.
		DisableNewsFetch: true,
		Codec:            temporalUICodecConfig{Endpoint: codecEndpoint},
	})
	if err != nil {
		return nil, err
	}

	child, err := StartChild(ChildOptions{
		Name: "temporal-ui",
		Path: art.Binary,
		// `--root` is the directory `--config` is resolved against and `--env` picks the file
		// inside it. Three flags rather than three environment variables, because a flag cannot
		// be shadowed by whatever the operator happens to have exported.
		Args: []string{"--root", runDir, "--config", temporalUIConfigDir, "--env", temporalUIConfigEnv, "start"},
		Dir:  runDir,
		Env:  temporalUIEnv(runDir),
		Log:  opts.Log,
	})
	if err != nil {
		return nil, err
	}

	ui := &TemporalUI{
		child:     child,
		address:   address,
		origin:    origin,
		codec:     codecEndpoint,
		namespace: opts.Namespace,
		temporal:  opts.TemporalAddress,
		config:    configPath,
		art:       art,
	}
	if err := waitForTemporalUI(ctx, ui, opts.ReadyTimeout); err != nil {
		_ = child.Stop(2 * time.Second)
		return nil, err
	}
	return ui, nil
}

// HydrateTemporalUI puts the pinned UI server on disk and returns where.
//
// SAME STORE, SAME VERIFICATION, SAME LAYOUT as the orchestrator bundle: the bytes are streamed
// through their sha256 into the CAS and are never written under the pin's address unless the
// whole body hashed to it, the archive is expanded once into a golden tree, and the working
// directory is a copy-on-write clone named `<data>/temporal-ui/<digest>`. A different pin is a
// different directory, so bumping TemporalUIVersion cannot silently keep running the old UI.
func HydrateTemporalUI(ctx context.Context, opts TemporalUIOptions) (*HydratedTemporalUI, error) {
	started := time.Now()
	if opts.DataDir == "" {
		return nil, errors.New("appliance: hydrating the Temporal UI needs a data directory")
	}
	here := bundle.Platform{OS: runtime.GOOS, Arch: runtime.GOARCH}
	pin, err := TemporalUIPin(here)
	if err != nil {
		return nil, err
	}

	root := filepath.Join(opts.DataDir, "temporal-ui", pin.Digest)
	out := &HydratedTemporalUI{
		Root:    root,
		Digest:  pin.Digest,
		Binary:  filepath.Join(root, temporalUIBinary),
		Version: pin.Version,
		Pin:     pin,
	}

	store := opts.Store
	if store == nil {
		store, err = hydratestore.Open(opts.DataDir)
		if err != nil {
			return nil, err
		}
	}

	art := hydratestore.Artifact{
		Name:   pin.Name,
		URL:    pin.URL,
		Digest: pin.Digest,
		Kind:   hydratestore.KindTarGz,
	}

	// THE FAST PATH SAYS NOTHING, and that is the same discipline the orchestrator bundle keeps.
	// Everything below runs once per pin, ever; the second `kontra up --temporal-ui` is silent,
	// because a line announcing a download that is not happening is a line that teaches an
	// operator to skim past this one.
	//
	// IT IS NO LONGER `stat(root)` (issue 15). This artifact is a BINARY that gets exec'd, and a
	// working directory that lost half of itself to a disk that filled still passes a stat — so
	// the fast path asks the only question that separates a whole artifact from most of one,
	// which is whether every file of its inventory is there at the right length.
	fresh := store.Check(art, root, hydratestore.Structure) != nil

	report := bundle.NewHydrationReport(opts.Progress)
	if fresh {
		// SAID BEFORE THE WAIT, NOT AFTER IT. A first `--temporal-ui` fetches ~9 MB from GitHub
		// and a binary that goes quiet on a slow link is indistinguishable from a hung one.
		report.Say("hydrating Temporal's Web UI %s from %s", pin.Version, pin.URL)
		store.SetProgress(report.Observe)
		defer store.SetProgress(nil)
	}

	// SHARED, and the promise holds for the same reason it does for the orchestrator: the tree is
	// read and exec'd and never written. Everything the UI needs to write — nothing, today —
	// would go to the run directory beside it, never into the artifact.
	res, err := store.EnsureHydrated(ctx, art, root, hydratestore.Shared)
	if err != nil {
		return nil, err
	}
	if fresh {
		report.Done(res)
	}

	out.Fresh = !res.Existing || res.Repaired
	out.Method, out.Files, out.Bytes = res.Method, res.Files, res.Bytes
	out.Elapsed = time.Since(started)

	// THE CHEAPEST CHECK OF THE CLAIM, and the same one resolveEntrypoint makes about the
	// orchestrator bundle: the file we are about to exec has to be there and has to be
	// executable. A release whose archive layout changed fails here, by name, instead of as
	// `fork/exec: no such file or directory` from inside StartChild.
	st, err := os.Stat(out.Binary)
	if err != nil {
		return nil, fmt.Errorf("the hydrated Temporal UI at %s has no %s, so there is nothing to start: %w", root, temporalUIBinary, err)
	}
	if st.Mode().Perm()&0o111 == 0 {
		return nil, fmt.Errorf("%s hydrated without its execute bit (mode %s); the release tarball's modes did not survive", out.Binary, st.Mode().Perm())
	}
	return out, nil
}

// --- the configuration the UI is started with -------------------------------------------------

// temporalUIConfigDir and temporalUIConfigEnv name the file `ui-server` reads:
// `<root>/<dir>/<env>.yaml`. The environment is called `kontra` rather than upstream's
// `development` so that a file found in a data directory is unambiguously ours.
const (
	temporalUIConfigDir = "config"
	temporalUIConfigEnv = "kontra"
)

// temporalUIConfig is the subset of ui-server's configuration the appliance sets. Every field it
// does not set stays at upstream's default on purpose — this is a config file for a program we do
// not own, and a value written here is a value that has to be maintained here.
type temporalUIConfig struct {
	TemporalGRPCAddress string                `yaml:"temporalGrpcAddress"`
	Host                string                `yaml:"host"`
	Port                int                   `yaml:"port"`
	EnableUI            bool                  `yaml:"enableUi"`
	DefaultNamespace    string                `yaml:"defaultNamespace"`
	DisableNewsFetch    bool                  `yaml:"disableNewsFetch"`
	Codec               temporalUICodecConfig `yaml:"codec"`
}

type temporalUICodecConfig struct {
	Endpoint string `yaml:"endpoint"`
}

// writeTemporalUIConfig writes the file and answers its path.
//
// IT IS REWRITTEN ON EVERY START, and it lives BESIDE the hydrated artifact rather than inside
// it. Inside would be a write into a Shared working copy, whose files are hardlinks to 0444
// objects in the store — a chmod or a write there is a write to the store itself. Beside it, the
// artifact stays exactly the bytes its digest describes and the configuration stays a function of
// this boot's addresses.
func writeTemporalUIConfig(runDir string, cfg temporalUIConfig) (string, error) {
	dir := filepath.Join(runDir, temporalUIConfigDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("appliance: the Temporal UI's configuration directory %s: %w", dir, err)
	}
	body, err := yaml.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("appliance: rendering the Temporal UI's configuration: %w", err)
	}
	path := filepath.Join(dir, temporalUIConfigEnv+".yaml")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return "", fmt.Errorf("appliance: writing %s: %w", path, err)
	}
	return path, nil
}

// temporalUIEnv is the child's WHOLE environment, and it is short on purpose.
//
// `ui-server` reads TEMPORAL_ROOT, TEMPORAL_CONFIG_DIR and TEMPORAL_ENVIRONMENT for the three
// values passed above as flags. An inherited TEMPORAL_CONFIG_DIR — perfectly plausible on a box
// where somebody runs `temporal` by hand — is a UI reading somebody else's configuration and
// pointing at somebody else's server, which is the worst possible outcome for a window whose only
// job is to show you the truth. They are set here to the same values the flags carry, so neither
// spelling can disagree with the other.
func temporalUIEnv(runDir string) []string {
	env := []string{
		"TEMPORAL_ROOT=" + runDir,
		"TEMPORAL_CONFIG_DIR=" + temporalUIConfigDir,
		"TEMPORAL_ENVIRONMENT=" + temporalUIConfigEnv,
	}
	// PATH and HOME are carried because a Go binary that resolves a CA bundle or a temporary
	// directory without them behaves differently in ways that have nothing to do with kontra.
	for _, k := range []string{"PATH", "HOME", "TMPDIR", "TZ"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	sort.Strings(env)
	return env
}

// --- readiness --------------------------------------------------------------------------------

// waitForTemporalUI returns once the UI answers, or explains why it never will.
//
// IT POLLS THE SETTINGS ENDPOINT RATHER THAN THE PORT. A bound socket says the process is alive;
// `/api/v1/settings` is the document the browser reads before it renders anything, and it is
// where the codec endpoint appears — so an answer here is the first moment the wiring in this
// file is known to have arrived. It also watches the child: a UI that died on a bad config is a
// failure to report immediately, not thirty seconds of polling a socket nobody will bind.
func waitForTemporalUI(ctx context.Context, ui *TemporalUI, timeout time.Duration) error {
	url := "http://" + ui.address + temporalUISettingsPath
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(timeout)
	var last error
	for {
		select {
		case <-ui.child.Done():
			if err := ui.child.Err(); err != nil {
				return fmt.Errorf("appliance: the Temporal UI exited before it served: %w", err)
			}
			return errors.New("appliance: the Temporal UI exited before it served")
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			last = fmt.Errorf("%s answered HTTP %s", url, resp.Status)
		} else {
			last = err
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("appliance: the Temporal UI did not answer %s within %s: %w", url, timeout, last)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// temporalUISettingsPath is the document the browser reads first, and the one that carries the
// codec endpoint it will call. Named here because two things depend on it: the readiness check
// above, and the test that proves the endpoint the browser is handed is the one the codec bound.
const temporalUISettingsPath = "/api/v1/settings"

// --- three helpers this file keeps a copy of ---------------------------------------------------
//
// Each has exactly one caller above, and the import that would remove the copy would be this
// package reaching into a role for a HELPER rather than for the role — which is the coupling the
// split exists to remove. Where a copy has an original, it says so.

// hostPort is net.JoinHostPort with an int, as every listening role in this tree spells it.
func hostPort(ip string, port int) string { return net.JoinHostPort(ip, strconv.Itoa(port)) }

// checkPortFree reports whether we could bind ip:port right now. temporalsrv asks the same
// question of the Temporal frontend port, for the same reason the caller above gives: the refusal
// has to name the collision.
func checkPortFree(ip string, port int) error {
	l, err := net.Listen("tcp", hostPort(ip, port))
	if err != nil {
		return err
	}
	return l.Close()
}

// sortedKeys is what makes a "which versions ARE pinned?" error deterministic rather than a map
// iteration; bundle/pins.go has the same five lines, for the same table.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
