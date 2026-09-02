package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/medmahmoudi26/kontra/cli/appliance/registry"
)

// A Go actor's Bundle must carry the actor as an EXECUTABLE, because the Machine's systemd unit
// execs `/opt/kontra/actor/<name>/<name>` and a Machine has no Go toolchain.
//
// This is a regression test for a failure that cost four Droplets to observe. The bundle shipped
// `main.go` and `go.mod` — right for Python, wrong for Go — and nothing noticed until systemd
// tried to exec the directory's source three minutes into a run, reporting only
// "No such file or directory". Every earlier step reported success.
func TestGoBundleCarriesACompiledActor(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain")
	}
	root, err := findRepoRoot("")
	if err != nil {
		t.Skip("not in a checkout")
	}
	actorDir := filepath.Join(root, "examples", "go", "nscheck")
	if _, err := os.Stat(filepath.Join(actorDir, "go.mod")); err != nil {
		t.Skip("examples/go/nscheck not present")
	}

	b, err := buildBundle(actorDir, io.Discard)
	if err != nil {
		t.Fatalf("buildBundle: %v", err)
	}
	if b.Engine != "go" {
		t.Fatalf("engine = %q, want go — go.mod is right there", b.Engine)
	}

	names := map[string]int64{}
	zr, err := gzip.NewReader(bytes.NewReader(b.Bytes))
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar: %v", err)
		}
		names[h.Name] = h.Mode
	}

	// The exact path the entrypoint execs, and it has to be executable.
	const want = "actor/nscheck/nscheck"
	mode, ok := names[want]
	if !ok {
		t.Fatalf("%s missing from the Bundle — a Machine would find source and no binary", want)
	}
	if mode&0o111 == 0 {
		t.Errorf("%s mode %o is not executable", want, mode)
	}
	// The handler travels with it; both are built together on purpose.
	if _, ok := names["bin/handler"]; !ok {
		t.Error("bin/handler missing from the Bundle")
	}
}

// --- publishing -----------------------------------------------------------------------------

// testBundle is a Bundle's shape without the minutes it takes to compile one. Every test below is
// about the ENVELOPE — the manifest, the digests, the addresses — and none of them cares what is
// inside the tar.gz, which the test above is the test for.
func testBundle(engine string, payload string) *bundle {
	body := []byte(payload)
	sum := sha256.Sum256(body)
	return &bundle{
		Name: "nscheck", Version: "0.1.0", Engine: engine,
		SHA: hex.EncodeToString(sum[:]), Bytes: body,
	}
}

// startTestRegistry is the appliance's own OCI registry, which is what a Bundle is now published
// to. A real registry rather than a stub httptest server: the whole claim of this slice is that a
// Bundle travels over the distribution API, and a fake that answers whatever the client asks
// proves nothing about that.
// A FREE port, not `Port: 0` — that means the default 5000, and a suite run on a controller that
// is already serving one would talk to the wrong registry and report it as a pass
// (deploy_registry_test.go makes the same choice for the same reason).
func startTestRegistry(t *testing.T) *registry.Server {
	t.Helper()
	srv, err := registry.Start(registry.Options{
		DataDir: t.TempDir(), Port: freeTestPort(t), Logf: func(string, ...any) {},
	})
	if err != nil {
		t.Fatalf("start the registry: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })
	return srv
}

// A MACHINE'S HALF OF THE PULL, and this test is the reason a Machine needs no new tooling.
//
// `machineInstall` fetches the Bundle with `curl -fsSL "$BUNDLE_URL"` and checks it with
// `sha256sum` — two commands cloud-init already installs — so what has to be true is that the blob
// endpoint the control plane hands it serves exactly the bytes whose sha it was also handed. That
// is asserted here the way the shell does it: fetch the URL, hash the body, compare. No OCI client
// on this side of the assertion, because there is none on a Machine either.
func TestAMachineFetchesTheBundleFromTheBlobEndpointAndTheShaMatches(t *testing.T) {
	srv := startTestRegistry(t)
	b := testBundle("go", "the bundle bytes")

	art, err := pushBundle(context.Background(), srv.Address(), b, io.Discard)
	if err != nil {
		t.Fatalf("pushBundle: %v", err)
	}
	if art.LayerSHA != b.SHA {
		t.Fatalf("LayerSHA = %q, want the Bundle's own sha %q", art.LayerSHA, b.SHA)
	}

	url := bundleBlobURL(art.Registry, b.Name, art.LayerSHA)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: HTTP %d — a Machine would report a curl failure and place nothing", url, resp.StatusCode)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(got)
	if hex.EncodeToString(sum[:]) != b.SHA {
		t.Fatalf("the blob endpoint served bytes hashing to %x, not %s — every Machine would exit 1 on the sha check",
			sum, b.SHA)
	}
	if !bytes.Equal(got, b.Bytes) {
		t.Fatal("the served bytes are not the Bundle's")
	}
}

// THE CONTROL PLANE'S HALF: a tag resolves to a manifest, and the manifest carries the engine.
//
// This is what `resolveBundle` does on the other side of the language boundary, asserted here
// against the same registry so the Go writer and the TypeScript reader are pinned to one shape.
// The engine is the field the whole resolution exists for — `latest.json` carried it beside the
// Artifact where nothing verified it, and a wrong one produces a Machine that installs cleanly,
// starts nothing and reports a healthy deploy.
func TestTheManifestATagResolvesToCarriesTheEngineAndTheLayer(t *testing.T) {
	srv := startTestRegistry(t)
	b := testBundle("go", "the bundle bytes")
	art, err := pushBundle(context.Background(), srv.Address(), b, io.Discard)
	if err != nil {
		t.Fatalf("pushBundle: %v", err)
	}

	base := "http://" + srv.Address() + "/v2/" + bundleRepo(b.Name)
	body, man, reported := resolveTag(t, srv, b.Name, b.Version)

	// The reader verifies the manifest against the digest the registry reports, and against the
	// digest the push returned. Both, because they answer different questions: the first says the
	// body was not mangled in transit, the second says push and resolve agree on what `0.1.0` is.
	sum := sha256.Sum256(body)
	if want := "sha256:" + hex.EncodeToString(sum[:]); want != art.Digest {
		t.Fatalf("the manifest body hashes to %s, but the push reported %s", want, art.Digest)
	}
	if reported != art.Digest {
		t.Fatalf("Docker-Content-Digest = %q, want %q", reported, art.Digest)
	}

	if man.ArtifactType != bundleArtifactType {
		t.Errorf("artifactType = %q, want %q — a mirror filters on this", man.ArtifactType, bundleArtifactType)
	}
	if len(man.Layers) != 1 {
		t.Fatalf("%d layers, want exactly 1: a Bundle is one tar.gz and a reader takes layers[0]", len(man.Layers))
	}
	if got := man.Layers[0].Digest.String(); got != "sha256:"+b.SHA {
		t.Fatalf("layers[0].digest = %q, want sha256:%s", got, b.SHA)
	}
	if man.Layers[0].MediaType != bundleLayerType {
		t.Errorf("layer mediaType = %q, want %q", man.Layers[0].MediaType, bundleLayerType)
	}

	// The config blob is where the engine went, and it is inside the manifest digest — which is
	// the whole improvement over a pointer file that sat beside the Artifact unverified.
	cfgResp, err := http.Get(base + "/blobs/" + man.Config.Digest.String())
	if err != nil {
		t.Fatalf("GET the config blob: %v", err)
	}
	defer cfgResp.Body.Close()
	var cfg bundleConfig
	if err := json.NewDecoder(cfgResp.Body).Decode(&cfg); err != nil {
		t.Fatalf("parse the config blob: %v", err)
	}
	if cfg.Engine != "go" || cfg.Name != b.Name || cfg.Version != b.Version {
		t.Fatalf("config = %+v, want the actor's identity and engine", cfg)
	}
}

// THE SAME BYTES MUST PUBLISH TO THE SAME DIGEST, and this is not free.
//
// `oras.PackManifest` stamps `org.opencontainers.image.created` with `time.Now()` unless it is
// given one, so an unattended manifest moves on every single build — and a Bundle whose identity
// changed every build would redeploy the whole Fleet on every command, which is exactly why
// `buildBundle` pins the tar's mtime to the epoch. The payload's promise is worthless if the
// envelope does not make the same one.
//
// THE DIGEST COMPARISON ALONE IS A GUARD THAT CANNOT FAIL, and that is why the annotation is
// asserted directly underneath it. `time.Now().Format(time.RFC3339)` has SECOND resolution: two
// pushes in one test land in the same second, so the two digests agree whether or not anything
// pins the field. Measured — with the annotation deleted from pushBundle, the comparison below
// still passed. What catches the regression is reading the value out of the published manifest.
func TestPublishingTheSameBundleTwiceProducesTheSameDigest(t *testing.T) {
	srv := startTestRegistry(t)

	a1, err := pushBundle(context.Background(), srv.Address(), testBundle("py", "identical inputs"), io.Discard)
	if err != nil {
		t.Fatalf("first push: %v", err)
	}
	a2, err := pushBundle(context.Background(), srv.Address(), testBundle("py", "identical inputs"), io.Discard)
	if err != nil {
		t.Fatalf("second push: %v", err)
	}
	if a1.Digest != a2.Digest {
		t.Fatalf("two builds of identical bytes published as %s and %s — every converge would replace the Fleet",
			a1.Digest, a2.Digest)
	}

	_, man, _ := resolveTag(t, srv, "nscheck", "0.1.0")
	got := man.Annotations[ocispec.AnnotationCreated]
	if got != bundleCreated {
		t.Fatalf("%s = %q, want the fixed %q — a wall-clock stamp moves the manifest digest on every "+
			"build of identical bytes, and the digests above cannot see it inside one second",
			ocispec.AnnotationCreated, got, bundleCreated)
	}

	// And it must still MOVE when the content does, or it is not an identity at all.
	changed, err := pushBundle(context.Background(), srv.Address(), testBundle("py", "different inputs"), io.Discard)
	if err != nil {
		t.Fatalf("third push: %v", err)
	}
	if changed.Digest == a1.Digest {
		t.Fatal("different bytes published to the same digest")
	}
}

// resolveTag is the read `backend/src/activities/fleet.ts:resolveBundle` makes: GET the manifest
// by TAG and take the digest the registry reports for it.
func resolveTag(t *testing.T, srv *registry.Server, name, tag string) ([]byte, ocispec.Manifest, string) {
	t.Helper()
	url := "http://" + srv.Address() + "/v2/" + bundleRepo(name) + "/manifests/" + tag
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Accept", ocispec.MediaTypeImageManifest)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("resolve %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("resolving %s: HTTP %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var man ocispec.Manifest
	if err := json.Unmarshal(body, &man); err != nil {
		t.Fatalf("parse the manifest: %v", err)
	}
	return body, man, resp.Header.Get("Docker-Content-Digest")
}

// A CHANGE OF ENGINE ALONE MOVES THE DIGEST, which is the property the pointer file could not have.
//
// `latest.json` was mutable and unverified, so the one field a listing could not supply was the
// one field a Machine took on trust. Here the engine is inside the bytes the manifest digest is
// computed over: the same lie is a digest mismatch, not a silent misplacement.
func TestTheEngineIsInsideTheManifestDigest(t *testing.T) {
	srv := startTestRegistry(t)
	const same = "the bundle bytes"
	py, err := pushBundle(context.Background(), srv.Address(), testBundle("py", same), io.Discard)
	if err != nil {
		t.Fatalf("push py: %v", err)
	}
	golang, err := pushBundle(context.Background(), srv.Address(), testBundle("go", same), io.Discard)
	if err != nil {
		t.Fatalf("push go: %v", err)
	}
	if py.LayerSHA != golang.LayerSHA {
		t.Fatalf("the two pushes did not share a layer (%s vs %s); the test is not measuring what it claims",
			py.LayerSHA, golang.LayerSHA)
	}
	if py.Digest == golang.Digest {
		t.Fatal("the same tar.gz published as py and as go got ONE digest — the engine is not covered by it, " +
			"and a Machine could be handed the wrong host with nothing to check it against")
	}
}

// bundleRegistry is one resolution for two halves that are on DIFFERENT HOSTS, and the Controller
// rung is the one `deploy.go:registryAddress` does not have.
//
// The bug it closes was live: `kontra fleet deploy` pushed to `localhost:8333` while telling every
// Machine to fetch from `<controller>:8333`, so running it from anywhere but ON the Controller
// placed a Fleet whose every Machine 404s. It survived only because nobody ran it from a laptop.
func TestTheBundleRegistryIsTheControllerTheMachinesWillFetchFrom(t *testing.T) {
	t.Setenv("KONTRA_REGISTRY", "")
	t.Setenv("KONTRA_CONTROLLER", "")

	if got := bundleRegistry("", "10.124.0.2"); got != "10.124.0.2:5000" {
		t.Errorf("--controller ignored: bundleRegistry(\"\", %q) = %q", "10.124.0.2", got)
	}
	t.Setenv("KONTRA_CONTROLLER", "10.116.0.9")
	if got := bundleRegistry("", ""); got != "10.116.0.9:5000" {
		t.Errorf("KONTRA_CONTROLLER ignored: %q", got)
	}
	if got := bundleRegistry("", "10.124.0.2"); got != "10.124.0.2:5000" {
		t.Errorf("--controller lost to the environment: %q", got)
	}
	// An explicit --registry or KONTRA_REGISTRY beats the Controller: an operator pointing at a
	// mirror is the airgap case, and it must not be overridden by a controller address.
	t.Setenv("KONTRA_REGISTRY", "mirror.internal:5000")
	if got := bundleRegistry("", "10.124.0.2"); got != "mirror.internal:5000" {
		t.Errorf("KONTRA_REGISTRY lost to the controller: %q", got)
	}
	if got := bundleRegistry("other:5000", "10.124.0.2"); got != "other:5000" {
		t.Errorf("--registry lost: %q", got)
	}

	// AND A GUESS MUST NEVER SELECT ONE. `controllerAddress` falls back to a hardcoded
	// `10.124.0.2`, which is right for placing a Fleet and wrong for choosing where 65 MiB gets
	// uploaded — on a single box that named no controller, that address belongs to nobody.
	t.Setenv("KONTRA_REGISTRY", "")
	t.Setenv("KONTRA_CONTROLLER", "")
	if got := bundleRegistry("", ""); strings.HasPrefix(got, "10.124.0.2") {
		t.Errorf("bundleRegistry fell through to controllerAddress's guess: %q", got)
	}
}

// AN ACTOR NAME THAT SERVES IS NOT ALWAYS AN ACTOR NAME THAT PUBLISHES, and this test is driven
// from the OTHER corpus on purpose.
//
// `shared/conformance/queues.json` states that a queue name is NOT sanitised — `my actor` and `café` reach
// Temporal verbatim, and it carries `a/b` as an adversarial case — so the set of nameable Actors is
// strictly larger than the set of nameable OCI repositories. Asserting that here, against the very
// names that corpus already thought were dangerous, is what stops this from being a guard written
// against the three names somebody imagined.
//
// The three outcomes below are all correct and all different, which is why each is pinned:
//
//   - `a/b` PUBLISHES, to `bundles/a/b`. A repository name is a path, and a tag is delimited by
//     `:` and not by a slash count, so the extra component is unambiguous. (This is exactly where
//     the process driver's label parse had a bug — three `/`-joined fields split left-to-right made
//     `a/b` unrepresentable — and it does not recur here because nothing counts separators.)
//   - `my actor` and `café` are REFUSED, at build time, before a Machine exists.
//   - the refusal NAMES THE ACTOR. `invalid repository "bundles/my actor"` is what oras says on its
//     own, and an author reading it has to work out that `bundles/` is ours and that the fix is in
//     actor.json.
func TestAnActorNameThatCannotBeARepositoryIsRefusedByName(t *testing.T) {
	srv := startTestRegistry(t)
	push := func(name string) error {
		b := testBundle("py", "x")
		b.Name = name
		_, err := pushBundle(context.Background(), srv.Address(), b, io.Discard)
		return err
	}

	// Legal, including the slash — `bundles/a/b` is a repository, not a repository plus a stray
	// segment, and `queues.json` carries this name because it breaks naive parsers.
	for _, name := range []string{"nscheck", "http-fuzzer", "crawl4ai", "a/b"} {
		if err := push(name); err != nil {
			t.Errorf("actor %q is servable and should be publishable: %v", name, err)
		}
	}

	// Illegal, and the refusal has to be readable by the person who chose the name.
	for _, name := range []string{"my actor", "café", "MyActor"} {
		err := push(name)
		if err == nil {
			t.Errorf("actor %q published; an OCI repository name cannot contain it", name)
			continue
		}
		if !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), "actor.json") {
			t.Errorf("the refusal for %q names neither the actor nor the fix: %v", name, err)
		}
	}
}

// A registry spelled with a scheme is honoured, because slice 07 pushes to somebody else's.
func TestRegistryHostKeepsTheOperatorsScheme(t *testing.T) {
	for _, c := range []struct {
		in    string
		host  string
		plain bool
	}{
		{"localhost:5000", "localhost:5000", true},
		{"http://10.124.0.2:5000", "10.124.0.2:5000", true},
		{"https://ghcr.io", "ghcr.io", false},
		{"https://ghcr.io/", "ghcr.io", false},
	} {
		host, plain := registryHost(c.in)
		if host != c.host || plain != c.plain {
			t.Errorf("registryHost(%q) = (%q,%v), want (%q,%v)", c.in, host, plain, c.host, c.plain)
		}
	}
}
