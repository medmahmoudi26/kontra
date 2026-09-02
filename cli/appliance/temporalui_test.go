package appliance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	commonpb "go.temporal.io/api/common/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
	"gopkg.in/yaml.v3"

	"github.com/medmahmoudi26/kontra/cli/appliance/bundle"
	"github.com/medmahmoudi26/kontra/cli/appliance/codec"
	"github.com/medmahmoudi26/kontra/cli/appliance/temporalsrv"
	"github.com/medmahmoudi26/kontra/handler/codecserver"
	"github.com/medmahmoudi26/kontra/handler/hydratestore"
)

// --- the pin table ----------------------------------------------------------------------------

// A HALF-BUMPED PIN MUST NOT REACH THE NETWORK, which is the entire reason the table is keyed by
// version rather than by platform. Asked for a version nobody has pasted digests for, the lookup
// has to refuse with the URL of the file to paste them out of — not fetch `latest`, and not fall
// back to a version somebody else chose.
func TestTemporalUIPinRefusesAVersionWithNoDigests(t *testing.T) {
	_, err := temporalUIPin("99.99.99", bundle.Platform{OS: "linux", Arch: "amd64"})
	if err == nil {
		t.Fatal("a version with no digest row must be refused, not fetched")
	}
	for _, want := range []string{"99.99.99", "temporalUIDigests", "checksums.txt", TemporalUIVersion} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must contain %q so the fix is a paste and not an investigation, got:\n%v", want, err)
		}
	}
}

// The other half-bump: the version is pinned but this platform is not. Different fix, so a
// different sentence — and it lists what IS pinned, so `linux/x86_64` reads as the typo it is.
func TestTemporalUIPinRefusesAnUnpinnedPlatform(t *testing.T) {
	_, err := temporalUIPin(TemporalUIVersion, bundle.Platform{OS: "plan9", Arch: "amd64"})
	if err == nil {
		t.Fatal("an unpinned platform must be refused")
	}
	for _, want := range []string{"plan9/amd64", "linux/amd64", "darwin/arm64"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must contain %q, got:\n%v", want, err)
		}
	}
}

// Every platform ADR 0031 §6 commits to resolves a pin, and each pin's URL is built from the same
// version its digest is keyed by. The URL/digest pairing is the thing that goes wrong silently:
// a row copied from another version fetches real bytes that hash to something else, and the only
// place that can be caught cheaply is here.
func TestTemporalUIPinsResolveForEveryShippedPlatform(t *testing.T) {
	for _, p := range []bundle.Platform{
		{OS: "linux", Arch: "amd64"}, {OS: "linux", Arch: "arm64"},
		{OS: "darwin", Arch: "amd64"}, {OS: "darwin", Arch: "arm64"},
	} {
		pin, err := TemporalUIPin(p)
		if err != nil {
			t.Errorf("%s: %v", p, err)
			continue
		}
		if pin.Version != TemporalUIVersion {
			t.Errorf("%s: pin version %q, want %q", p, pin.Version, TemporalUIVersion)
		}
		// Upstream names its artifacts in Go's own vocabulary, so the platform appears in the URL
		// verbatim. A pin whose URL does not name its own platform is a copied row.
		if want := fmt.Sprintf("ui-server_%s_%s_%s.tar.gz", TemporalUIVersion, p.OS, p.Arch); !strings.HasSuffix(pin.URL, want) {
			t.Errorf("%s: URL is %q, which does not end in %q", p, pin.URL, want)
		}
		if !strings.Contains(pin.URL, "/v"+TemporalUIVersion+"/") {
			t.Errorf("%s: URL %q does not name the pinned version's release", p, pin.URL)
		}
		if len(pin.Digest) != 64 {
			t.Errorf("%s: digest %q is not a sha256", p, pin.Digest)
		}
	}
}

// FOUR DIGESTS, FOUR DIFFERENT DIGESTS. A row pasted twice is the mistake this table's shape
// cannot catch on its own — it resolves, it fetches, and it fails at the hash on one platform
// only, which is to say on somebody else's machine.
func TestTemporalUIDigestsAreDistinctPerPlatform(t *testing.T) {
	for version, byPlatform := range temporalUIDigests {
		seen := map[string]string{}
		for p, d := range byPlatform {
			if other, dup := seen[d]; dup {
				t.Errorf("temporal ui-server %s: %s and %s carry the same digest %s", version, other, p, d)
			}
			seen[d] = p
		}
	}
}

// --- the derived addresses --------------------------------------------------------------------

// ONE STRING FOR BOTH HALVES OF THE CORS WIRE. What the operator opens and what the codec admits
// have to be character-identical, because a browser treats `localhost` and `127.0.0.1` as
// different origins — and the codec answers with exactly one Allow-Origin. A mismatch is not an
// error anywhere; it is a page full of `$ref` blobs that will not open.
func TestTemporalUIOriginIsTheStringTheCodecAlreadyDefaultsTo(t *testing.T) {
	// The loopback spelling is `localhost`, which is what codecserver's own default says and what
	// `temporal server start-dev` prints for the same listener.
	if got := TemporalUIOrigin("127.0.0.1", DefaultTemporalUIPort); got != codecserver.DefaultUIOrigin {
		t.Errorf("TemporalUIOrigin(127.0.0.1, %d) = %q, but the codec admits %q by default",
			DefaultTemporalUIPort, got, codecserver.DefaultUIOrigin)
	}
	if got := TemporalUIOrigin("", 0); got != codecserver.DefaultUIOrigin {
		t.Errorf("the zero values must agree with the codec's default too, got %q", got)
	}
	// A non-loopback bind keeps its address: there is no name for it that both halves would
	// resolve the same way.
	if got, want := TemporalUIOrigin("10.124.0.2", 9999), "http://10.124.0.2:9999"; got != want {
		t.Errorf("TemporalUIOrigin(10.124.0.2, 9999) = %q, want %q", got, want)
	}
}

// THE OPTIONS STRUCT OFFERS NO WAY TO TYPE A CODEC ADDRESS, and that is the acceptance criterion
// "derived rather than configured twice" expressed as a fact about the API rather than as a
// comment. In compose the endpoint was a string in a second file that had to be kept equal by
// hand; here the only way to give the UI a codec is to hand it the running server, so the value
// is READ off the listener that is bound.
func TestTemporalUIOptionsCannotCarryACodecAddress(t *testing.T) {
	tp := reflect.TypeOf(TemporalUIOptions{})
	field, ok := tp.FieldByName("Codec")
	if !ok {
		t.Fatal("TemporalUIOptions has no Codec field")
	}
	if field.Type != reflect.TypeOf((*codec.Server)(nil)) {
		t.Errorf("TemporalUIOptions.Codec is %s; it must be *codec.Server, so the endpoint is read off the listener", field.Type)
	}
	for i := 0; i < tp.NumField(); i++ {
		f := tp.Field(i)
		if f.Type.Kind() == reflect.String && strings.Contains(strings.ToLower(f.Name), "codec") {
			t.Errorf("TemporalUIOptions.%s is a string naming the codec; that is the second place the address could drift", f.Name)
		}
	}
}

// --- refusals ---------------------------------------------------------------------------------

func TestStartTemporalUIRefusesWithoutADataDirectory(t *testing.T) {
	_, err := StartTemporalUI(context.Background(), TemporalUIOptions{TemporalAddress: "127.0.0.1:1"})
	if err == nil || !strings.Contains(err.Error(), "data directory") {
		t.Fatalf("a UI with nowhere to hydrate must be refused by name, got: %v", err)
	}
}

// A UI with no server behind it starts, serves a page, and answers every query with a transport
// error — which is a worse failure than not starting, because the page looks fine.
func TestStartTemporalUIRefusesWithoutAServerAddress(t *testing.T) {
	_, err := StartTemporalUI(context.Background(), TemporalUIOptions{DataDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "address") {
		t.Fatalf("a UI with no Temporal behind it must be refused by name, got: %v", err)
	}
}

func TestStartTemporalUIRefusesAHostnameAsBindAddress(t *testing.T) {
	_, err := StartTemporalUI(context.Background(), TemporalUIOptions{
		DataDir: t.TempDir(), TemporalAddress: "127.0.0.1:1", BindIP: "localhost",
	})
	if err == nil || !strings.Contains(err.Error(), "not an IP") {
		t.Fatalf("a hostname bind address must be refused by name, got: %v", err)
	}
}

// 8233 is `temporal server start-dev`'s UI port, so the process most likely to be holding it is
// another Temporal. The refusal has to say so, and it has to come BEFORE anything is fetched:
// nine megabytes downloaded to serve a port we cannot bind is a slow way to learn nothing.
func TestStartTemporalUINamesThePortCollision(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	dir := t.TempDir()
	_, err = StartTemporalUI(context.Background(), TemporalUIOptions{
		DataDir: dir, TemporalAddress: "127.0.0.1:1", Port: port,
	})
	if err == nil {
		t.Fatal("starting the UI on an occupied port must fail")
	}
	for _, want := range []string{"already listening", "Web UI"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the collision error must contain %q, got: %v", want, err)
		}
	}
	if _, statErr := os.Stat(filepath.Join(dir, "temporal-ui")); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("the port was refused but something was hydrated into %s anyway (%v)", dir, statErr)
	}
}

// --- the whole thing, against a real server -----------------------------------------------------

// THE ACCEPTANCE TEST. A real embedded Temporal, a real worker, a run that completes and a run
// that fails, the real pinned ui-server hydrated out of the CAS — and then the two questions the
// escape hatch exists to answer, asked through the UI's own HTTP API rather than through the SDK:
// what happened to this run, and what does the browser get told about the codec.
//
// THE ARTIFACT IS CACHED BETWEEN RUNS, in `$KONTRA_TEST_ARTIFACT_CACHE` or the user cache
// directory, so the 9 MB release is fetched once per machine and every later run is offline. On a
// machine with neither the cache nor the network this skips, naming the artifact it wanted —
// which is the honest answer, because there is no stand-in for "the real UI reads real history".
func TestTemporalUIReadsTheEmbeddedServersHistory(t *testing.T) {
	store, pin := cachedTemporalUIArtifact(t)

	dir := t.TempDir()
	srv, _ := startForTest(t, temporalsrv.Options{DataDir: dir, Port: freeTestPort(t)})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// Two runs with two ends. The failed one is the whole point: `completed` with empty output is
	// this repo's documented blind spot, and the raw view is where the failure is legible.
	done, failed := runOneOfEach(ctx, t, srv.Address())

	// THE PORT IS CHOSEN ONCE AND BOTH HALVES ARE DERIVED FROM IT, which is exactly what cmdUp
	// does: the UI's listener is bound with it, and the origin the codec admits is computed from
	// the same two values rather than typed a second time.
	uiPort := freeTestPort(t)
	cdcSrv := startTestCodec(t, startTestS3(t), func(o *codec.Options) {
		o.UIOrigin = TemporalUIOrigin("127.0.0.1", uiPort)
	})

	ui, err := StartTemporalUI(ctx, TemporalUIOptions{
		DataDir:         dir,
		TemporalAddress: srv.Address(),
		Port:            uiPort,
		Codec:           cdcSrv,
		Store:           store,
		Progress:        uiLogWriter{t},
		Log:             uiLogWriter{t},
	})
	if err != nil {
		t.Fatalf("starting the Temporal UI: %v", err)
	}
	t.Cleanup(func() {
		if err := ui.Stop(10 * time.Second); err != nil {
			t.Errorf("stopping the Temporal UI: %v", err)
		}
	})
	t.Logf("ui        %s (pid %d) from %s", ui.URL(), ui.PID(), ui.Artifact().Root)
	t.Logf("artifact  ui-server %s sha256:%s fresh=%v", ui.Artifact().Version, pin.Digest[:12], ui.Artifact().Fresh)

	// WHAT IT WAS ACTUALLY TOLD, on disk, where an operator can read it rather than taking this
	// file's word for it. The run directory is beside the artifact, never inside it.
	if _, err := os.Stat(ui.ConfigPath()); err != nil {
		t.Errorf("the UI reports its configuration at %s and it is not there: %v", ui.ConfigPath(), err)
	}
	if !strings.HasPrefix(ui.ConfigPath(), filepath.Join(dir, "temporal-ui", "run")) {
		t.Errorf("the configuration is at %s, which is not the run directory beside the artifact", ui.ConfigPath())
	}
	if ui.TemporalAddress() != srv.Address() {
		t.Errorf("the UI reads %q; the embedded server is at %q", ui.TemporalAddress(), srv.Address())
	}

	// ---- the codec address the BROWSER is handed is the one the codec bound -------------------
	//
	// Read from the settings document the UI serves to the page, not from our own config file:
	// what matters is what arrives at the browser, and a config the UI ignored would pass a
	// file-content check and fail this one.
	var settings struct {
		Codec struct{ Endpoint string } `json:"Codec"`
	}
	getJSON(ctx, t, "http://"+ui.Address()+temporalUISettingsPath, &settings)
	if settings.Codec.Endpoint != cdcSrv.Endpoint() {
		t.Errorf("the UI tells the browser to decode at %q; the codec is listening on %q",
			settings.Codec.Endpoint, cdcSrv.Endpoint())
	}
	if ui.CodecEndpoint() != cdcSrv.Endpoint() {
		t.Errorf("TemporalUI.CodecEndpoint() = %q, cdcSrv.Endpoint() = %q", ui.CodecEndpoint(), cdcSrv.Endpoint())
	}
	// …and the other half of that wire: the origin the codec admits is the URL we printed.
	if ui.Origin() != cdcSrv.UIOrigin() {
		t.Errorf("the UI is served from %q and the codec admits %q; a browser would be refused", ui.Origin(), cdcSrv.UIOrigin())
	}

	// ---- and that endpoint, called the way the page calls it, really decodes -------------------
	assertBrowserDecodes(t, ui)

	// ---- the histories ------------------------------------------------------------------------
	completedEvents := uiHistory(ctx, t, ui, done)
	if !hasEventType(completedEvents, "WorkflowExecutionCompleted") {
		t.Errorf("the UI's history for the completed run has no completion event; types were %v", eventTypes(completedEvents))
	}
	failedEvents := uiHistory(ctx, t, ui, failed)
	if !hasEventType(failedEvents, "WorkflowExecutionFailed") {
		t.Errorf("the UI's history for the failed run has no failure event; types were %v", eventTypes(failedEvents))
	}
	// THE REASON, NOT JUST THE VERDICT. "It failed" is what every kontra surface already says; the
	// message is the thing that was only ever visible through `temporal workflow show`.
	if body := fmt.Sprint(failedEvents); !strings.Contains(body, boomMessage) {
		t.Errorf("the failed run's history does not carry its failure message %q", boomMessage)
	}

	// ---- both ports are loopback ---------------------------------------------------------------
	assertLoopbackOnly(t, "the Temporal UI", uiPort)
	_, codecPortStr, err := net.SplitHostPort(cdcSrv.Address())
	if err != nil {
		t.Fatal(err)
	}
	assertLoopbackOnly(t, "the codec", atoiOrFatal(t, codecPortStr))
}

// A SECOND START DOES NO WORK, which is the property that makes the flag cheap enough to leave on.
// Same store, same data directory, same digest: the second call must find the working directory
// already there rather than expand the archive again.
func TestTemporalUIHydratesOncePerDigest(t *testing.T) {
	store, pin := cachedTemporalUIArtifact(t)
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	first, err := HydrateTemporalUI(ctx, TemporalUIOptions{DataDir: dir, Store: store})
	if err != nil {
		t.Fatalf("first hydration: %v", err)
	}
	if !first.Fresh {
		t.Error("the first hydration into an empty data directory reports Fresh=false")
	}
	if want := filepath.Join(dir, "temporal-ui", pin.Digest); first.Root != want {
		t.Errorf("hydrated into %s, want %s (the working directory must be named by the digest)", first.Root, want)
	}

	second, err := HydrateTemporalUI(ctx, TemporalUIOptions{DataDir: dir, Store: store})
	if err != nil {
		t.Fatalf("second hydration: %v", err)
	}
	if second.Fresh {
		t.Error("the second hydration of the same digest did work; it must find the directory already there")
	}
	if second.Root != first.Root || second.Binary != first.Binary {
		t.Errorf("the second hydration answered a different place: %s vs %s", second.Root, first.Root)
	}
}

// THE CONFIG IS WRITTEN BESIDE THE ARTIFACT, NEVER INTO IT. A Shared working copy's files are
// hardlinks to 0444 objects in the store, so a write inside the hydrated tree is a write to the
// store itself — which would corrupt every other working copy of the same digest.
func TestTemporalUIConfigLivesOutsideTheHydratedArtifact(t *testing.T) {
	dir := t.TempDir()
	path, err := writeTemporalUIConfig(filepath.Join(dir, "temporal-ui", "run"), temporalUIConfig{
		TemporalGRPCAddress: "127.0.0.1:1234",
		Host:                "127.0.0.1",
		Port:                8233,
		EnableUI:            true,
		DefaultNamespace:    temporalsrv.DefaultNamespace,
		DisableNewsFetch:    true,
		Codec:               temporalUICodecConfig{Endpoint: "http://127.0.0.1:18234"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "temporal-ui", "run", "config", "kontra.yaml"); path != want {
		t.Errorf("config written to %s, want %s", path, want)
	}
	// It must not be under any digest-named directory, which is what "beside, not inside" means
	// on disk.
	entries, err := os.ReadDir(filepath.Join(dir, "temporal-ui"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if len(e.Name()) == 64 {
			t.Errorf("the configuration landed inside a digest-named directory (%s)", e.Name())
		}
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var back temporalUIConfig
	if err := yaml.Unmarshal(body, &back); err != nil {
		t.Fatalf("the file we wrote is not the YAML ui-server will read: %v\n%s", err, body)
	}
	if back.Codec.Endpoint != "http://127.0.0.1:18234" {
		t.Errorf("codec.endpoint round-tripped as %q", back.Codec.Endpoint)
	}
	if !back.DisableNewsFetch {
		t.Error("disableNewsFetch did not survive; a local console must not call out to temporal.io on load")
	}
}

// The child's environment is short ON PURPOSE: an inherited TEMPORAL_CONFIG_DIR — plausible on a
// box where somebody runs `temporal` by hand — would point the UI at somebody else's config and
// somebody else's server, which is the worst possible outcome for a window whose only job is to
// show you the truth.
func TestTemporalUIEnvironmentCannotBeShadowed(t *testing.T) {
	t.Setenv("TEMPORAL_CONFIG_DIR", "/somebody/elses/config")
	t.Setenv("TEMPORAL_ADDRESS", "temporal.example.com:7233")
	t.Setenv("KONTRA_S3_BUCKET", "not-inherited")

	env := temporalUIEnv("/data/temporal-ui/run")
	byKey := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		byKey[k] = v
	}
	if got := byKey["TEMPORAL_CONFIG_DIR"]; got != temporalUIConfigDir {
		t.Errorf("TEMPORAL_CONFIG_DIR = %q, want %q — the operator's value must not survive", got, temporalUIConfigDir)
	}
	if got := byKey["TEMPORAL_ROOT"]; got != "/data/temporal-ui/run" {
		t.Errorf("TEMPORAL_ROOT = %q", got)
	}
	if _, ok := byKey["KONTRA_S3_BUCKET"]; ok {
		t.Error("the UI child inherited KONTRA_S3_BUCKET; its environment is meant to be the short one")
	}
}

// THE SPELLING OF LOOPBACK IS LOAD-BEARING, which is why TemporalUIOrigin exists at all instead
// of a hostPort call at each of the two call sites.
//
// `localhost` and `127.0.0.1` are one listener and TWO ORIGINS. The codec answers exactly one
// `Access-Control-Allow-Origin`, so a page served from the other spelling has every decode dropped
// by the browser while the codec answers 200 the entire time — the exact silence compose's two
// hand-synced values used to produce. Deriving both halves from one expression is what removes it,
// and this test is the demonstration that the halves are not interchangeable.
func TestTheOtherSpellingOfLoopbackIsRefusedByTheBrowser(t *testing.T) {
	const port = DefaultTemporalUIPort
	derived := TemporalUIOrigin("127.0.0.1", port)
	drifted := "http://127.0.0.1:" + strconv.Itoa(port)
	if derived == drifted {
		t.Fatalf("TemporalUIOrigin now answers %q, so this test no longer measures anything", derived)
	}

	// A codec configured with the IP spelling, and a page served from the derived one.
	wrong := startTestCodec(t, startTestS3(t), func(o *codec.Options) { o.UIOrigin = drifted })
	if allow := preflightFrom(t, wrong.Endpoint(), derived); allow == derived {
		t.Errorf("a codec admitting %q also admitted %q; the two spellings must not be interchangeable", drifted, derived)
	}

	// And the derived pairing, which is what `kontra up` builds: admitted.
	right := startTestCodec(t, startTestS3(t), func(o *codec.Options) { o.UIOrigin = derived })
	if allow := preflightFrom(t, right.Endpoint(), derived); allow != derived {
		t.Errorf("the codec answers Allow-Origin %q to a page served from %q", allow, derived)
	}
}

// assertBrowserDecodes runs the whole browser path minus the browser: a claim-check offloaded and
// read back THROUGH THE ENDPOINT THE UI HANDS THE PAGE, with the page's own Origin on the request.
//
// The settings check above proves the right string reached the browser; this proves the string is
// good for the thing the browser does with it. Both are needed — an endpoint that is correct and
// unreachable, and one that is reachable and never handed over, fail identically on screen.
func assertBrowserDecodes(t *testing.T, ui *TemporalUI) {
	t.Helper()
	cdc := converter.NewRemotePayloadCodec(converter.RemotePayloadCodecOptions{
		Endpoint: ui.CodecEndpoint(),
		ModifyRequest: func(r *http.Request) error {
			r.Header.Set("Origin", ui.Origin())
			return nil
		},
	})
	data := bytes.Repeat([]byte("k"), codecserver.DefaultThreshold+1)
	meta := map[string][]byte{"encoding": []byte("json/plain")}

	offloaded, err := cdc.Encode([]*commonpb.Payload{{Metadata: meta, Data: data}})
	if err != nil {
		t.Fatalf("encoding a claim-check through the endpoint the UI hands the browser (%s): %v", ui.CodecEndpoint(), err)
	}
	if enc := string(offloaded[0].GetMetadata()["encoding"]); enc != claimCheckMarker(t) {
		t.Fatalf("the payload stayed inline (encoding %q), so there is no claim-check for the UI to decode", enc)
	}
	back, err := cdc.Decode(offloaded)
	if err != nil {
		t.Fatalf("decoding through %s: %v", ui.CodecEndpoint(), err)
	}
	if !bytes.Equal(back[0].GetData(), data) {
		t.Errorf("the payload the page would render is %d bytes; %d were offloaded", len(back[0].GetData()), len(data))
	}
	// The preflight the browser sends BEFORE that POST. Without a matching Allow-Origin the
	// response above never reaches the page, and nothing on the page says why.
	if allow := preflightFrom(t, ui.CodecEndpoint(), ui.Origin()); allow != ui.Origin() {
		t.Errorf("the codec answers Allow-Origin %q to a page served from %q; the browser drops every decode", allow, ui.Origin())
	}
}

// preflightFrom asks the codec what it would tell a browser at `origin`, and answers with the one
// header the browser decides on.
//
// It takes the origin as an ARGUMENT, unlike codec_test.go's preflight, which sends the server its
// own configured origin and therefore can only ever agree with itself. The question here is what
// happens to a page served from somewhere else.
func preflightFrom(t *testing.T, endpoint, origin string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodOptions, endpoint+"/decode", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", origin)
	req.Header.Set("Access-Control-Request-Method", "POST")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("preflight %s from %s: %v", endpoint, origin, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("preflight %s: HTTP %d (the browser will block the POST)", endpoint, resp.StatusCode)
	}
	return resp.Header.Get("Access-Control-Allow-Origin")
}

// --- helpers ------------------------------------------------------------------------------------

const (
	uiTestQueue = "appliance-temporal-ui-test"
	boomMessage = "the units all failed and the node still said completed"
)

// echoWorkflow is the smallest thing that produces a history worth reading: one workflow task, a
// completed execution, a result. Five words, and temporalsrv's own suite has the same five for
// the same reason — a shared fixture between two packages' tests would be a third thing to keep
// in step for no gain.
func echoWorkflow(_ workflow.Context, in string) (string, error) { return "echo:" + in, nil }

// boomWorkflow is a run that FAILS, which is the one this slice exists for. A returned error ends
// the execution as failed and puts the reason in the last history event — the fact that was only
// ever visible through `temporal workflow show`.
func boomWorkflow(workflow.Context) error { return errors.New(boomMessage) }

type execution struct{ id, run string }

// runOneOfEach starts a worker, runs a workflow to completion and another to failure, and returns
// both executions.
func runOneOfEach(ctx context.Context, t *testing.T, address string) (done, failed execution) {
	t.Helper()
	c, err := client.DialContext(ctx, client.Options{HostPort: address, Namespace: temporalsrv.DefaultNamespace})
	if err != nil {
		t.Fatalf("dialing the embedded server at %s: %v", address, err)
	}
	t.Cleanup(c.Close)

	w := worker.New(c, uiTestQueue, worker.Options{})
	w.RegisterWorkflow(echoWorkflow)
	w.RegisterWorkflow(boomWorkflow)
	if err := w.Start(); err != nil {
		t.Fatalf("starting the worker: %v", err)
	}
	t.Cleanup(w.Stop)

	ok, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: "ui-completed", TaskQueue: uiTestQueue}, echoWorkflow, "hello")
	if err != nil {
		t.Fatalf("starting the completing workflow: %v", err)
	}
	var got string
	if err := ok.Get(ctx, &got); err != nil {
		t.Fatalf("waiting for the completing workflow: %v", err)
	}

	bad, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: "ui-failed", TaskQueue: uiTestQueue}, boomWorkflow)
	if err != nil {
		t.Fatalf("starting the failing workflow: %v", err)
	}
	if err := bad.Get(ctx, nil); err == nil {
		t.Fatal("the failing workflow succeeded; the test needs a failed run to look at")
	}
	return execution{ok.GetID(), ok.GetRunID()}, execution{bad.GetID(), bad.GetRunID()}
}

// uiHistory reads one execution's history THROUGH THE UI — the same request the browser makes.
func uiHistory(ctx context.Context, t *testing.T, ui *TemporalUI, e execution) []map[string]any {
	t.Helper()
	url := fmt.Sprintf("http://%s/api/v1/namespaces/%s/workflows/%s/history?execution.runId=%s",
		ui.Address(), ui.Namespace(), e.id, e.run)
	var resp struct {
		History struct {
			Events []map[string]any `json:"events"`
		} `json:"history"`
	}
	getJSON(ctx, t, url, &resp)
	if len(resp.History.Events) == 0 {
		t.Fatalf("the UI returned no history events for %s/%s", e.id, e.run)
	}
	return resp.History.Events
}

// eventTypes lists what came back, for a failure message that says what WAS there.
func eventTypes(events []map[string]any) []string {
	out := make([]string, 0, len(events))
	for _, ev := range events {
		if s, ok := ev["eventType"].(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// hasEventType matches across the two spellings the JSON may use — `WorkflowExecutionCompleted`
// and `EVENT_TYPE_WORKFLOW_EXECUTION_COMPLETED` are the same event, and which one arrives is the
// UI's marshalling choice rather than anything this slice controls.
func hasEventType(events []map[string]any, want string) bool {
	norm := func(s string) string { return strings.ToLower(strings.ReplaceAll(s, "_", "")) }
	for _, got := range eventTypes(events) {
		if strings.Contains(norm(got), norm(want)) {
			return true
		}
	}
	return false
}

func getJSON(ctx context.Context, t *testing.T, url string, into any) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: HTTP %s\n%s", url, resp.Status, body)
	}
	if err := json.Unmarshal(body, into); err != nil {
		t.Fatalf("GET %s returned unreadable JSON: %v\n%s", url, err, body)
	}
}

// assertLoopbackOnly proves the port is not reachable from off-box (ADR 0031 §3).
//
// IT DIALS THE MACHINE'S OWN LAN ADDRESS rather than reading the listener's address string,
// because the string is what we asked for and the socket is what we got. This repo has already
// measured the difference once: ufw was active, the bindings were what mattered, and ten ports
// answered from a second droplet anyway.
func assertLoopbackOnly(t *testing.T, what string, port int) {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Skipf("cannot enumerate interfaces: %v", err)
	}
	tried := 0
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() || ipnet.IP.To4() == nil {
			continue
		}
		tried++
		target := net.JoinHostPort(ipnet.IP.String(), fmt.Sprint(port))
		conn, err := net.DialTimeout("tcp", target, 2*time.Second)
		if err == nil {
			conn.Close()
			t.Errorf("%s answered on %s; it must bind loopback only (ADR 0031 §3)", what, target)
		}
	}
	if tried == 0 {
		t.Logf("%s: no non-loopback IPv4 on this machine, so the off-box half is unprovable here", what)
	}
}

func atoiOrFatal(t *testing.T, s string) int {
	t.Helper()
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		t.Fatalf("port %q: %v", s, err)
	}
	return n
}

// cachedTemporalUIArtifact answers a store that already holds the pinned release, fetching it
// once per machine.
//
// THE STORE OUTLIVES THE TEST AND THE WORKING DIRECTORY DOES NOT. That split is deliberate: the
// bytes are cached so a suite run is not a 9 MB download, while every test still hydrates into a
// fresh data directory and therefore still exercises expansion, materialization and the execute
// bit. Point KONTRA_TEST_ARTIFACT_CACHE somewhere else to move the cache.
func cachedTemporalUIArtifact(t *testing.T) (*hydratestore.Store, bundle.Pin) {
	t.Helper()
	pin, err := TemporalUIPin(bundle.Platform{OS: runtime.GOOS, Arch: runtime.GOARCH})
	if err != nil {
		t.Skipf("no Temporal UI is pinned for this machine: %v", err)
	}

	root := os.Getenv("KONTRA_TEST_ARTIFACT_CACHE")
	if root == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			base = os.TempDir()
		}
		root = filepath.Join(base, "kontra", "test-artifacts")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := hydratestore.Open(root)
	if err != nil {
		t.Fatalf("opening the test artifact cache at %s: %v", root, err)
	}

	art := hydratestore.Artifact{Name: pin.Name, URL: pin.URL, Digest: pin.Digest, Kind: hydratestore.KindTarGz}
	has, err := store.CAS().Has(pin.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if !has {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		t.Logf("fetching %s (once per machine, cached in %s)", pin.URL, root)
		if _, err := store.Fetch(ctx, art); err != nil {
			t.Skipf("the pinned Temporal UI is not cached and could not be fetched: %v\n  wanted %s", err, pin.URL)
		}
	}
	return store, pin
}

// uiLogWriter routes the UI child's output and a hydration's progress into the test log, where
// they are attached to the test that produced them instead of interleaved on stdout.
type uiLogWriter struct{ t *testing.T }

func (w uiLogWriter) Write(p []byte) (int, error) {
	w.t.Logf("%s", strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// claimCheckMarker is the marker THE CORPUS RECORDS, read rather than restated.
//
// It was `const claimCheckMarker = "binary/claim-check-v1"`, under a header saying it was
// "restated here rather than imported so a drift in the moved code shows up as a failing test and
// not as a test that agrees with the bug — which is also why it is restated in
// appliance/codec/codec_test.go rather than shared with this file". Two copies of one string, each
// justified by the other. shared/conformance/codec/fixtures.json is where that string is defined for
// every implementation of this codec, so reading it keeps the whole of the drift-detection — this
// test still fails if the served codec stops stamping the marker — and leaves nothing to keep in
// step (ADR 0035 §3).
func claimCheckMarker(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("../../shared/conformance/codec/fixtures.json")
	if err != nil {
		t.Fatalf("read the codec corpus: %v", err)
	}
	var fx struct {
		WireFormat struct {
			Marker string `json:"marker"`
		} `json:"wireFormat"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("parse the codec corpus: %v", err)
	}
	if fx.WireFormat.Marker == "" {
		t.Fatal("the corpus no longer records wireFormat.marker")
	}
	return fx.WireFormat.Marker
}
