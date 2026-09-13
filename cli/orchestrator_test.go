package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	applbundle "github.com/medmahmoudi26/kontra/cli/appliance/bundle"
)

// --- what the child is told -------------------------------------------------------------------

func envMap(t *testing.T, env []string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			t.Fatalf("not a KEY=VALUE entry: %q", kv)
		}
		out[k] = v
	}
	return out
}

func applianceEnvOptions() orchestratorEnvOptions {
	return orchestratorEnvOptions{
		TemporalAddress: "127.0.0.1:17233",
		S3Endpoint:      "http://127.0.0.1:18333",
		S3Bound:         "http://127.0.0.1:18333",
		KVAddress:       "127.0.0.1:16379",
		DataDir:         "/var/lib/kontra",
		Port:            18088,
		KontraHome:      "/root/.kontra",
		Roles:           "api,materializer,infra",
		BindIP:          "127.0.0.1",
	}
}

// THE API'S LISTENER MUST HONOUR `--bind`, and until issue 18 it was the one surface that did not.
//
// The five embedded services close onto the address `kontra up` is given; the child hard-coded
// `0.0.0.0` because that is what it did as a container, where the compose `ports:` entry was the
// control. MEASURED on a droplet with a public interface: `kontra up --bind 172.17.0.1` left an
// unauthenticated orchestrator API answering on every address the box has. A gate that declares
// the binary has replaced compose for local development must not bless a replacement that is more
// exposed than what it replaces.
func TestOrchestratorEnvClosesTheAPIOntoTheBoundAddress(t *testing.T) {
	env, _ := orchestratorEnv(nil, applianceEnvOptions())
	if got := envMap(t, env)["KONTRA_ORCHESTRATOR_BIND"]; got != "127.0.0.1" {
		t.Errorf("the API listener was not told the bind address, got %q", got)
	}

	// A DEFAULT, NOT A FACT. docker-compose.yml describes relocating the `api` role to a worker
	// host as a container of kontra-orchestrator:latest, and a process inside a container that
	// binds its own loopback is unreachable through its published port. An operator who says
	// 0.0.0.0 keeps it.
	env, _ = orchestratorEnv([]string{"KONTRA_ORCHESTRATOR_BIND=0.0.0.0"}, applianceEnvOptions())
	if got := envMap(t, env)["KONTRA_ORCHESTRATOR_BIND"]; got != "0.0.0.0" {
		t.Errorf("the operator's bind address was overwritten: %q", got)
	}
}

// THE ADDRESSES ARE FACTS. An inherited KONTRA_ADDRESS pointing at a compose stack is how a
// control plane ends up serving one installation's SPA over another installation's history — and
// on a developer's box that variable is usually set, because everything else in this CLI needs it.
func TestOrchestratorEnvOverridesTheAppliancesOwnAddresses(t *testing.T) {
	base := []string{
		"KONTRA_ADDRESS=localhost:7233",
		"KONTRA_S3_ENDPOINT=http://localhost:8333",
		"KONTRA_REDIS_HOST=localhost:6379",
		"PATH=/usr/bin",
	}
	env, replaced := orchestratorEnv(base, applianceEnvOptions())
	got := envMap(t, env)

	for k, want := range map[string]string{
		"KONTRA_ADDRESS":         "127.0.0.1:17233",
		"KONTRA_S3_ENDPOINT":     "http://127.0.0.1:18333",
		"KONTRA_REDIS_HOST":      "127.0.0.1:16379",
		"KONTRA_DATA_DIR":        "/var/lib/kontra",
		"KONTRA_ORCHESTRATOR_DB": filepath.Join("/var/lib/kontra", "orchestrator.db"),
	} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}
	if got["PATH"] != "/usr/bin" {
		t.Errorf("an unrelated variable was lost: PATH=%q", got["PATH"])
	}

	// AND IT IS SAID OUT LOUD. A silent override is the same class of bug as `kontra infra up`
	// silently reverting a container to its image.
	joined := strings.Join(replaced, "\n")
	for _, want := range []string{"KONTRA_ADDRESS", "localhost:7233"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the override of %s was not reported: %v", want, replaced)
		}
	}
	if len(replaced) != 3 {
		t.Errorf("expected three reported overrides, got %v", replaced)
	}
}

// An override that changed nothing is not worth a line. Reporting every fact as a replacement is
// how a real one stops standing out.
func TestOrchestratorEnvIsQuietWhenItChangedNothing(t *testing.T) {
	opts := applianceEnvOptions()
	_, replaced := orchestratorEnv([]string{"KONTRA_ADDRESS=" + opts.TemporalAddress}, opts)
	if len(replaced) != 0 {
		t.Errorf("an unchanged value was reported as an override: %v", replaced)
	}
}

// The roles and the port have a right answer for the appliance and a legitimate reason to be set
// differently: a deployment that keeps a compose `orchestrator-infra` beside this process MUST
// leave the infra role out of it (ADR 0034 §1), and it says so with this variable.
func TestOrchestratorEnvDefersToRolesTheOperatorChose(t *testing.T) {
	env, _ := orchestratorEnv([]string{
		"KONTRA_ORCHESTRATOR_ROLES=api,materializer",
		"KONTRA_ORCHESTRATOR_PORT=9999",
	}, applianceEnvOptions())
	got := envMap(t, env)

	if got["KONTRA_ORCHESTRATOR_ROLES"] != "api,materializer" {
		t.Errorf("the operator's role selection was overwritten: %q", got["KONTRA_ORCHESTRATOR_ROLES"])
	}
	if got["KONTRA_ORCHESTRATOR_PORT"] != "9999" {
		t.Errorf("the operator's port was overwritten: %q", got["KONTRA_ORCHESTRATOR_PORT"])
	}
}

func TestOrchestratorEnvDefaultsToAllThreeRoles(t *testing.T) {
	env, _ := orchestratorEnv(nil, applianceEnvOptions())
	if got := envMap(t, env)["KONTRA_ORCHESTRATOR_ROLES"]; got != "api,materializer,infra" {
		t.Errorf("the appliance runs all three roles (it ships no Pulumi engine), got %q", got)
	}
}

// A worker started by the Workflows page's Serve button runs on the HOST and cannot inherit an
// address that only means something inside this process. Its absence has already cost a run: the
// actor booted, registered, polled, took a Batch and spun on a name that resolved to nothing,
// while every surface reported it healthy.
func TestOrchestratorEnvTellsServedWorkersWhereEverythingIs(t *testing.T) {
	env, _ := orchestratorEnv(nil, applianceEnvOptions())
	serve := envMap(t, env)["KONTRA_SERVE_ENV"]
	for _, want := range []string{
		"KONTRA_ADDRESS=127.0.0.1:17233",
		"KONTRA_S3_ENDPOINT=http://127.0.0.1:18333",
		"KONTRA_REDIS_HOST=127.0.0.1:16379",
		"KONTRA_ORCHESTRATOR_URL=http://127.0.0.1:18088",
	} {
		if !strings.Contains(serve, want) {
			t.Errorf("KONTRA_SERVE_ENV does not carry %q: %q", want, serve)
		}
	}
}

// --- which orchestrator ------------------------------------------------------------------------

// fakeCheckout is a tree that looks enough like the repo for the resolver.
func fakeCheckout(t *testing.T, compiled bool) string {
	t.Helper()
	root := t.TempDir()
	if !compiled {
		return root
	}
	for _, rel := range []string{
		"control/orchestrator/dist/src/main.js",
		"control/orchestrator/node_modules/.keep",
		"frontend/dist/index.html",
	} {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("//\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// bundleName is what findBundles looks for, so a test writing a bundle has to agree with it or it
// is testing nothing.
func bundleName() string {
	return fmt.Sprintf("kontra-orchestrator-%s-%s-%s.tar.gz", applbundle.NodeVersion, runtime.GOOS, runtime.GOARCH)
}

// writeBundle writes a real, hydratable bundle: a manifest member first, then the entrypoint.
func writeBundle(t *testing.T, path string, files map[string]string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest, err := json.Marshal(map[string]any{
		"schema":     "kontra.appliance.bundle/v1",
		"bundle":     "orchestrator",
		"platform":   runtime.GOOS + "/" + runtime.GOARCH,
		"entrypoint": []string{"node/bin/node", "orchestrator/dist/src/main.js"},
	})
	if err != nil {
		t.Fatal(err)
	}

	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.New()
	zw := gzip.NewWriter(io.MultiWriter(f, sum))
	tw := tar.NewWriter(zw)
	add := func(name string, body []byte, mode int64) {
		if err := tw.WriteHeader(&tar.Header{Name: name, Size: int64(len(body)), Mode: mode, Format: tar.FormatPAX}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	add("manifest.json", manifest, 0o644)
	for name, body := range files {
		mode := int64(0o644)
		if strings.HasSuffix(name, "/node") {
			mode = 0o755
		}
		add(name, []byte(body), mode)
	}
	for _, closer := range []io.Closer{tw, zw, f} {
		if err := closer.Close(); err != nil {
			t.Fatal(err)
		}
	}
	digest := hex.EncodeToString(sum.Sum(nil))
	if err := os.WriteFile(path+".sha256", []byte(digest+"  "+filepath.Base(path)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func bundleFiles() map[string]string {
	return map[string]string{
		"node/bin/node":                 "#!/bin/sh\necho fake node\n",
		"orchestrator/dist/src/main.js": "console.log('bundle');\n",
	}
}

// THE ACCEPTANCE CRITERION IN ONE TEST. In a compiled checkout `kontra up` runs what the
// developer built — the `kontra infra up` trap is exactly the opposite behaviour — and it says
// what it did NOT use, with the flag that would have chosen it.
func TestResolveOrchestratorPrefersALocalBuildAndSaysWhatItSkipped(t *testing.T) {
	if _, err := os.Stat("/usr/local/bin/node"); err != nil {
		if _, err := os.Stat("/usr/bin/node"); err != nil {
			t.Skip("no node on this machine; the local path is not available to test")
		}
	}
	repo := fakeCheckout(t, true)
	bundle := filepath.Join(repo, "build", "bundles", bundleName())
	writeBundle(t, bundle, bundleFiles())

	src, err := resolveOrchestrator(context.Background(), orchestratorOptions{
		DataDir:  t.TempDir(),
		RepoRoot: repo,
	})
	if err != nil {
		t.Fatal(err)
	}
	if src.Kind != "local" {
		t.Fatalf("a compiled checkout did not win: %+v", src)
	}
	if !strings.Contains(src.Chose, "local build") {
		t.Errorf("the choice does not say what it ran: %q", src.Chose)
	}
	if !strings.Contains(src.Instead, bundle) || !strings.Contains(src.Instead, "--orchestrator=bundle") {
		t.Errorf("the choice does not say what it skipped or how to pick it: %q", src.Instead)
	}
	if src.Entry != filepath.Join(repo, "control", "orchestrator", "dist", "src", "main.js") {
		t.Errorf("the wrong entrypoint: %s", src.Entry)
	}
	// The SPA a local build serves is the checkout's own — nothing hydrated, nothing pointed at.
	if src.SPARoot != filepath.Join(repo, "frontend", "dist") {
		t.Errorf("the local build's SPA was not found: %q", src.SPARoot)
	}
}

// With no local build there is nothing to prefer, so the bundle is hydrated — and the reason the
// other path was not taken travels with the decision.
func TestResolveOrchestratorFallsBackToTheBundleAndSaysWhy(t *testing.T) {
	repo := fakeCheckout(t, false)
	writeBundle(t, filepath.Join(repo, "build", "bundles", bundleName()), bundleFiles())

	src, err := resolveOrchestrator(context.Background(), orchestratorOptions{
		DataDir:  t.TempDir(),
		RepoRoot: repo,
	})
	if err != nil {
		t.Fatal(err)
	}
	if src.Kind != "bundle" {
		t.Fatalf("the bundle was not used: %+v", src)
	}
	if !src.Fresh {
		t.Error("the first hydration did not report itself")
	}
	if !strings.Contains(src.Chose, "hydrated") {
		t.Errorf("the choice does not say what it did: %q", src.Chose)
	}
	// THE REASON, WHATEVER IT IS — not one specific phrasing of it. `fakeCheckout(t, false)` has
	// neither node_modules nor dist, and the resolver names the EARLIEST missing prerequisite
	// (issue #2), so this reads "has no node_modules" now rather than "not compiled". Both are the
	// same fact for this test's purpose: the bundle was hydrated BECAUSE the local build was not
	// usable, and the operator is told which command fixes it.
	if !strings.Contains(src.Instead, "control/orchestrator") || !strings.Contains(src.Instead, "pnpm") {
		t.Errorf("the choice does not say why the local build was not usable: %q", src.Instead)
	}
	if _, err := os.Stat(src.Entry); err != nil {
		t.Errorf("the hydrated entrypoint is not there: %v", err)
	}
}

// --orchestrator=bundle is the developer's way back to the artifact, and it must not quietly do
// something else when a local build is sitting right there.
func TestResolveOrchestratorHonoursAnExplicitBundle(t *testing.T) {
	repo := fakeCheckout(t, true)
	writeBundle(t, filepath.Join(repo, "build", "bundles", bundleName()), bundleFiles())

	src, err := resolveOrchestrator(context.Background(), orchestratorOptions{
		Mode:     "bundle",
		DataDir:  t.TempDir(),
		RepoRoot: repo,
	})
	if err != nil {
		t.Fatal(err)
	}
	if src.Kind != "bundle" {
		t.Fatalf("--orchestrator=bundle ran something else: %+v", src)
	}
	if !strings.Contains(src.Chose, "--orchestrator=bundle") {
		t.Errorf("the choice does not record that it was asked for: %q", src.Chose)
	}
}

// `--orchestrator=none` is how an operator runs the five embedded services beside a control plane
// that is somewhere else — a compose stack mid-migration, or a second appliance.
func TestResolveOrchestratorNoneRunsNoChild(t *testing.T) {
	src, err := resolveOrchestrator(context.Background(), orchestratorOptions{Mode: "none"})
	if err != nil {
		t.Fatal(err)
	}
	if src.Kind != "none" || src.Entry != "" {
		t.Fatalf("--orchestrator=none produced something to run: %+v", src)
	}
	if !strings.Contains(src.Chose, "not started") {
		t.Errorf("the choice does not say the control plane is absent: %q", src.Chose)
	}
}

// Neither one available is the fresh-machine case, and the message has to carry BOTH fixes: an
// operator on an installed binary and a developer in a checkout have the same symptom.
//
// IT ASSERTS `run build` AND NOT `not compiled`, and that is the change issue #2 made. A fresh
// clone has neither node_modules nor dist, and the resolver names the EARLIEST missing
// prerequisite — so the developer half of this message is now the install, with the build named
// after it. Pinning the later phrase was pinning the order that asked for a compile which could
// not run. What the message must carry is a command that works, which is what this checks.
func TestResolveOrchestratorWithNothingToRunExplainsBothWaysOut(t *testing.T) {
	t.Setenv("KONTRA_HOME", t.TempDir())
	_, err := resolveOrchestrator(context.Background(), orchestratorOptions{
		DataDir:  t.TempDir(),
		RepoRoot: fakeCheckout(t, false),
	})
	if err == nil {
		t.Fatal("a machine with no orchestrator started one anyway")
	}
	for _, want := range []string{"kontra bundle orchestrator", "--orchestrator=none", "run build"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the failure does not mention %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "exec tsc") {
		t.Errorf("the failure names `exec tsc`, which fails on a clean checkout: %v", err)
	}
}

// A path is a path: an archive is a bundle and a directory is a build, decided by what the thing
// IS rather than by a second flag that can disagree with it.
func TestResolveOrchestratorTakesADirectoryAsALocalBuild(t *testing.T) {
	if _, err := os.Stat("/usr/local/bin/node"); err != nil {
		if _, err := os.Stat("/usr/bin/node"); err != nil {
			t.Skip("no node on this machine")
		}
	}
	repo := fakeCheckout(t, true)
	src, err := resolveOrchestrator(context.Background(), orchestratorOptions{
		Mode:    filepath.Join(repo, "control", "orchestrator"),
		DataDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if src.Kind != "local" || src.Entry != filepath.Join(repo, "control", "orchestrator", "dist", "src", "main.js") {
		t.Fatalf("a directory was not read as a local build: %+v", src)
	}
}

// A word that is neither a mode nor a path fails with the list, rather than being stat'd into a
// confusing "no such file".
func TestResolveOrchestratorRefusesAWordItDoesNotKnow(t *testing.T) {
	_, err := resolveOrchestrator(context.Background(), orchestratorOptions{Mode: "bundel", DataDir: t.TempDir()})
	if err == nil {
		t.Fatal("a typo was accepted")
	}
	if !strings.Contains(err.Error(), "bundle") || !strings.Contains(err.Error(), "none") {
		t.Errorf("the refusal does not list the words that work: %v", err)
	}
}

// The environment pins an exact artifact, which is what an operator does when the search path
// would find the wrong one.
func TestFindBundlesLetsTheEnvironmentPinTheArtifact(t *testing.T) {
	repo := fakeCheckout(t, false)
	writeBundle(t, filepath.Join(repo, "build", "bundles", bundleName()), bundleFiles())
	pinned := filepath.Join(t.TempDir(), "pinned.tar.gz")
	writeBundle(t, pinned, bundleFiles())
	t.Setenv("KONTRA_ORCHESTRATOR_BUNDLE", pinned)

	bundle, _, err := findBundles(orchestratorOptions{RepoRoot: repo})
	if err != nil {
		t.Fatal(err)
	}
	if bundle != pinned {
		t.Errorf("the environment did not win: %s", bundle)
	}
}

// The SPA is found beside the bundle without being named, because that is where `kontra bundle
// spa` writes it.
func TestFindBundlesPicksUpTheSPABesideTheBundle(t *testing.T) {
	repo := fakeCheckout(t, false)
	dir := filepath.Join(repo, "build", "bundles")
	writeBundle(t, filepath.Join(dir, bundleName()), bundleFiles())
	spa := filepath.Join(dir, applbundle.SPAName+".tar.gz")
	if err := os.WriteFile(spa, []byte("not really an archive"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, found, err := findBundles(orchestratorOptions{RepoRoot: repo})
	if err != nil {
		t.Fatal(err)
	}
	if found != spa {
		t.Errorf("the SPA beside the bundle was not found: %q", found)
	}
}

// THE PRESIGNED-URL HOST HAS THREE SOURCES AND AN ORDER. A URL is signed FOR a host — `kontra
// explore` hands one to an operator's workstation — so a signature computed for the wrong one is
// invalid there. The flag is this appliance speaking; an inherited value is a deployment
// speaking; the bound address is what is true when nobody said anything.
func TestOrchestratorEnvPresignHostFollowsFlagThenEnvironmentThenTheBinding(t *testing.T) {
	opts := applianceEnvOptions()

	env, _ := orchestratorEnv(nil, opts)
	if got := envMap(t, env)["KONTRA_S3_PUBLIC_ENDPOINT"]; got != opts.S3Bound {
		t.Errorf("with nothing set, presigning must use the bound address; got %q", got)
	}

	inherited := []string{"KONTRA_S3_PUBLIC_ENDPOINT=http://workstation.lan:8333"}
	env, _ = orchestratorEnv(inherited, opts)
	if got := envMap(t, env)["KONTRA_S3_PUBLIC_ENDPOINT"]; got != "http://workstation.lan:8333" {
		t.Errorf("an inherited presign host was overwritten by the binding: %q", got)
	}

	opts.S3Public = "http://controller:8333"
	env, replaced := orchestratorEnv(inherited, opts)
	if got := envMap(t, env)["KONTRA_S3_PUBLIC_ENDPOINT"]; got != "http://controller:8333" {
		t.Errorf("--s3-public-endpoint did not win: %q", got)
	}
	if len(replaced) != 1 || !strings.Contains(replaced[0], "workstation.lan") {
		t.Errorf("the override of an inherited presign host was not reported: %v", replaced)
	}
}

// THE SIBLING CHECKOUT, which is where the console actually is (ADR 0038).
//
// The test above pins `frontend/dist` — the LEGACY candidate, kept only so a bundle built from an
// older checkout still resolves. It would keep passing if the sibling lookup were deleted, which
// is the whole failure mode: the layout every developer has would be the one nothing covers.
func TestResolveOrchestratorFindsTheConsoleInASiblingCheckout(t *testing.T) {
	// `repo` is a child of a temp dir, so `../kontra-console` is inside the sandbox.
	parent := t.TempDir()
	repo := filepath.Join(parent, "kontra")
	for _, rel := range []string{
		"kontra/control/orchestrator/dist/src/main.js",
		"kontra/control/orchestrator/node_modules/.keep",
		"kontra-console/dist/index.html",
	} {
		path := filepath.Join(parent, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("//\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	src, err := resolveOrchestrator(context.Background(), orchestratorOptions{
		Mode: "local", RepoRoot: repo, DataDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(repo, "..", "kontra-console", "dist")
	if src.SPARoot != want {
		t.Errorf("the sibling console was not found: got %q, want %q", src.SPARoot, want)
	}
}

// A console named explicitly wins, and a WRONG name is an answer rather than a fallback.
//
// Falling through to the search would hand an operator who mistyped a path a different SPA than
// the one they asked for, and report success — the class of failure `kontra up` prints digests to
// make visible in the first place.
func TestConsoleDistOverrideIsTheOnlyCandidateWhenItIsSet(t *testing.T) {
	parent := t.TempDir()
	repo := filepath.Join(parent, "kontra")
	// A sibling console that WOULD be found, so the assertion is about precedence, not absence.
	for _, rel := range []string{"kontra-console/dist/index.html", "elsewhere/index.html"} {
		path := filepath.Join(parent, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("<html>\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	named := filepath.Join(parent, "elsewhere")
	t.Setenv("KONTRA_CONSOLE_DIST", named)
	if got, _ := applbundle.ConsoleDist(repo); got != named {
		t.Errorf("the named console did not win: got %q, want %q", got, named)
	}

	t.Setenv("KONTRA_CONSOLE_DIST", filepath.Join(parent, "typo"))
	got, tried := applbundle.ConsoleDist(repo)
	if got != "" {
		t.Errorf("a wrong KONTRA_CONSOLE_DIST fell through to %q instead of failing", got)
	}
	if len(tried) != 1 || !strings.Contains(tried[0], "KONTRA_CONSOLE_DIST") {
		t.Errorf("the refusal does not name the variable the operator set: %v", tried)
	}
}

// requireNode skips when there is no interpreter, because `resolveLocal` checks for one FIRST now
// and would refuse for that reason instead of the one under test.
func requireNode(t *testing.T, why string) {
	t.Helper()
	if _, err := osexec.LookPath("node"); err != nil {
		t.Skip("no node on this machine; " + why)
	}
}

// ── issue #2: every refusal names a command that works, in prerequisite order ──────────────────
//
// A FIRST-TIME USER LOST AN EVENING TO THIS. `install.sh` finished clean, printed "run kontra up",
// and `kontra up` died telling them to run `pnpm exec tsc` — which on a clean checkout fails with
// about a hundred errors, because `package.json`'s `build` is `pnpm --filter @kontra/core run build
// && tsc` and the orchestrator imports `@kontra/core`. Bare `tsc` is the second half of a two-step
// build run without the first.
//
// THE ASSERTION IS NEGATIVE AS WELL AS POSITIVE, because "mentions run build" would pass on a
// message that also still said `exec tsc`, and the wrong half is the one somebody copies.

// notCompiled is a checkout with node_modules but no dist — what you have after `pnpm install`.
func notCompiled(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "control", "orchestrator", "node_modules")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".keep"), []byte("\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestNotCompiledNamesTheBuildThatWorksAndNotTheOneThatDoesNot(t *testing.T) {
	requireNode(t, "resolveLocal refuses earlier than the case under test")
	_, err := resolveLocal(orchestratorOptions{}, notCompiled(t), "")
	if err == nil {
		t.Fatal("an uncompiled checkout resolved")
	}
	if !strings.Contains(err.Error(), "run build") {
		t.Errorf("the refusal does not name the command that works: %q", err)
	}
	if strings.Contains(err.Error(), "exec tsc") {
		t.Errorf("the refusal still names `exec tsc`, which fails on a clean checkout: %q", err)
	}
}

func TestAFreshCloneIsToldToInstallBeforeItIsToldToBuild(t *testing.T) {
	requireNode(t, "resolveLocal refuses earlier than the case under test")
	// A fresh clone has NEITHER node_modules NOR dist. This used to check dist first and so asked
	// for a compile that could not possibly run — the earliest missing prerequisite is the one to
	// name.
	_, err := resolveLocal(orchestratorOptions{}, t.TempDir(), "")
	if err == nil {
		t.Fatal("a fresh clone resolved")
	}
	if !strings.Contains(err.Error(), "node_modules") {
		t.Errorf("a fresh clone was not told to install first: %q", err)
	}
	// Non-vacuous partner: the install message still points at the build that follows it, so the
	// two steps are not two separate discoveries.
	if !strings.Contains(err.Error(), "run build") {
		t.Errorf("the install message does not say what comes after it: %q", err)
	}
}

// The compiled case still resolves — a guard that refused everything would pass both tests above.
func TestACompiledCheckoutStillResolves(t *testing.T) {
	requireNode(t, "")
	src, err := resolveLocal(orchestratorOptions{}, fakeCheckout(t, true), "")
	if err != nil {
		t.Fatalf("a compiled checkout was refused: %v", err)
	}
	if src.Kind != "local" {
		t.Errorf("wrong kind: %+v", src)
	}
}
