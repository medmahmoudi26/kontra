package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/medmahmoudi26/kontra/cli/internal/cliutil"
)

// The invocation is asserted without running a build, because the four flags below are each a build
// that fails — or worse, succeeds insecurely — when they are missing, and a real build is minutes.

func TestUntrustedBuilderIsNotOptional(t *testing.T) {
	// In untrusted mode the lifecycle runs its phases in separate containers and only the exporter
	// gets registry credentials, so an actor's own package install script cannot read a push
	// credential. An actor is third-party code by construction; this is what makes building it on the
	// controller defensible, so there is deliberately no option to turn it off.
	argv := strings.Join(packArgv(packOpts{Image: "r/a:1", ActorDir: "."}), " ")
	if !strings.Contains(argv, "--trust-builder=false") {
		t.Fatalf("argv lost the credential boundary: %s", argv)
	}
}

func TestPlainHTTPGetsInsecureRegistryAndHTTPSDoesNot(t *testing.T) {
	// kontra's registry is plain HTTP by construction. Without the flag the exporter reports a TLS
	// error that reads like a certificate problem; WITH it against an https registry it would be a
	// silent downgrade on the one request that leaves the box.
	plain := strings.Join(packArgv(packOpts{Image: "r/a:1", Registry: "127.0.0.1:5000"}), " ")
	if !strings.Contains(plain, "--insecure-registry 127.0.0.1:5000") {
		t.Errorf("plain HTTP registry got no --insecure-registry: %s", plain)
	}
	secure := strings.Join(packArgv(packOpts{Image: "r/a:1", Registry: "https://ghcr.io"}), " ")
	if strings.Contains(secure, "--insecure-registry") {
		t.Errorf("an https registry was downgraded: %s", secure)
	}
}

func TestTheBuildContextIsTheActorDirectory(t *testing.T) {
	// The whole reason a Go actor's build becomes cheap: the old path used the repo ROOT because the
	// `replace` directives pointed outside it, so an edit anywhere invalidated every actor's cache.
	argv := packArgv(packOpts{Image: "r/a:1", ActorDir: "/w/actors/demo"})
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "--path /w/actors/demo") {
		t.Errorf("build context: %s", joined)
	}
}

func TestPublishAndCacheAreOnlyAskedForWhenWanted(t *testing.T) {
	bare := strings.Join(packArgv(packOpts{Image: "r/a:1"}), " ")
	if strings.Contains(bare, "--publish") || strings.Contains(bare, "--cache-image") {
		t.Errorf("--no-push build asked to publish: %s", bare)
	}
	full := strings.Join(packArgv(packOpts{Image: "r/a:1", Publish: true, CacheImage: "r/a-cache:latest", Network: "kontra"}), " ")
	for _, want := range []string{"--publish", "--cache-image r/a-cache:latest", "--network kontra"} {
		if !strings.Contains(full, want) {
			t.Errorf("missing %q: %s", want, full)
		}
	}
}

func TestTheDigestIsReadFromPacksOwnReport(t *testing.T) {
	d := strings.Repeat("a", 64)
	m := publishedDigest.FindStringSubmatch("*** Images (sha256:" + d + "):")
	if m == nil || m[1] != "sha256:"+d {
		t.Fatalf("did not read pack's digest line: %v", m)
	}
	// And nothing else in a build log looks like one. A false match here would have the CLI confirm a
	// digest the registry never saw.
	for _, line := range []string{"Adding layer 'heroku/python:venv'", "sha256:" + d, "*** Images:"} {
		if publishedDigest.MatchString(line) {
			t.Errorf("matched a line that is not the report: %q", line)
		}
	}
}

func TestAnOutsideReplaceIsNamedWithItsLine(t *testing.T) {
	// A `replace ../../sdk/go` makes the actor unbuildable anywhere but inside this checkout. The
	// refusal has to name the LINE, because a go.mod with four replaces otherwise sends the author
	// looking through all of them.
	mod := "module demo\n\ngo 1.26\n\nrequire github.com/x/y v1.0.0\n\nreplace github.com/x/y => ../../sdk/go\n"
	d, line := outsideReplace(mod)
	if line != 7 || !strings.Contains(d, "../../sdk/go") {
		t.Errorf("got line=%d directive=%q", line, d)
	}
}

func TestAReplaceInsideTheActorDirectoryIsFine(t *testing.T) {
	// `./vendored` is inside the build context, so it travels with the actor. That is exactly what
	// `--dev` produces by vendoring, and refusing it would refuse the escape hatch.
	if d, line := outsideReplace("module demo\n\nreplace github.com/x/y => ./vendored\n"); line != 0 {
		t.Errorf("an in-directory replace was refused: line=%d %q", line, d)
	}
}

func TestAModuleToModuleReplaceIsNotAPathAtAll(t *testing.T) {
	// `replace a => b v1.2.3` has no filesystem path and is common and portable.
	if _, line := outsideReplace("module demo\n\nreplace github.com/a/b => github.com/c/d v1.2.3\n"); line != 0 {
		t.Errorf("a module-to-module replace was refused at line %d", line)
	}
}

func TestAReplaceBlockIsReadToo(t *testing.T) {
	mod := "module demo\n\nreplace (\n\tgithub.com/x/y => ../../sdk/go\n)\n"
	if _, line := outsideReplace(mod); line != 4 {
		t.Errorf("a replace( ) block was missed: line=%d", line)
	}
}

func TestACommentedReplaceIsNotADirective(t *testing.T) {
	if _, line := outsideReplace("module demo\n\n// replace github.com/x/y => ../../sdk/go\n"); line != 0 {
		t.Errorf("a commented-out replace was refused at line %d", line)
	}
}

func TestDeployShellDeprecationHasThreeStates(t *testing.T) {
	dir := t.TempDir()
	if got := deployShellCheck(dir, ""); got != deployShellAbsent {
		t.Errorf("no deploy.sh should be absent, got %v", got)
	}
	if err := writeFileForTest(dir+"/deploy.sh", "#!/bin/sh\n"); err != nil {
		t.Fatal(err)
	}
	if got := deployShellCheck(dir, ""); got != deployShellWarn {
		t.Errorf("default should warn, got %v", got)
	}
	if got := deployShellCheck(dir, "refuse"); got != deployShellRefuse {
		t.Errorf("KONTRA_DEPLOY_SH=refuse should refuse, got %v", got)
	}
	// The message must name the replacement, or an author whose build just stopped installing
	// something has nowhere to go.
	for _, want := range []string{"runtime", "actor.json"} {
		if !strings.Contains(deployShellMessage, want) {
			t.Errorf("the message should mention %q: %s", want, deployShellMessage)
		}
	}
}

// A DOCKERFILE IN AN ACTOR'S FOLDER IS REFUSED, UNDER BOTH NAMES THE OLD PATH READ. Nothing on the
// buildpack path reads either, so an actor that kept one used to deploy clean and fail at run time
// missing what the file installed. The message has to name where those things live now — the
// `runtime` field and kontra-runtimes — or the author's next move is looking for a flag to turn it off.
func TestAnActorDockerfileIsRefusedNotIgnored(t *testing.T) {
	if got := actorDockerfiles(t.TempDir()); len(got) != 0 {
		t.Fatalf("an empty actor folder reported %v", got)
	}

	for _, name := range []string{"Dockerfile", "runtime.Dockerfile"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := writeFileForTest(filepath.Join(dir, name), "FROM scratch\n"); err != nil {
				t.Fatal(err)
			}
			got := actorDockerfiles(dir)
			if len(got) != 1 || got[0] != name {
				t.Fatalf("a folder holding %s reported %v", name, got)
			}
			msg := actorDockerfileError(dir, got).Error()
			for _, want := range []string{name, `actor.json`, `"runtime"`, "kontra-runtimes", dir} {
				if !strings.Contains(msg, want) {
					t.Errorf("the refusal should mention %q:\n%s", want, msg)
				}
			}
		})
	}

	// BOTH AT ONCE ARE BOTH NAMED. Reporting the first would send the author back for a second refusal.
	dir := t.TempDir()
	for _, name := range []string{"runtime.Dockerfile", "Dockerfile"} {
		if err := writeFileForTest(filepath.Join(dir, name), "FROM scratch\n"); err != nil {
			t.Fatal(err)
		}
	}
	if got := actorDockerfiles(dir); strings.Join(got, ",") != "Dockerfile,runtime.Dockerfile" {
		t.Errorf("both files present, reported %v", got)
	}

	// A DIRECTORY CALLED Dockerfile IS NOT ONE; nothing would ever have built from it.
	dir = t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "Dockerfile"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := actorDockerfiles(dir); len(got) != 0 {
		t.Errorf("a directory named Dockerfile was refused as a file: %v", got)
	}
}

// THE REFUSAL IS WIRED INTO `kontra deploy`, AND IT LANDS BEFORE THE BUILD. Asserting on
// `actorDockerfiles` alone would stay green with the call deleted from runDeploy — a refusal that is
// written and tested and never called. So runDeploy runs, against a stand-in registry, with a `pack`
// that only records whether it was reached.
//
// THE CONTROL CASE IS WHAT MAKES THE REFUSAL CASE MEAN SOMETHING. The same folder WITHOUT the
// Dockerfile must reach `pack`; otherwise "pack was never called" could be any earlier failure — an
// unresolvable runtime, a missing handler — passing for this one.
func TestDeployRefusesAnActorDockerfileBeforeBuilding(t *testing.T) {
	const runtimeDigest = "sha256:" + "ab" + "cdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	t.Setenv("KONTRA_REGISTRY", fakeRegistry(t, http.StatusOK, runtimeDigest))
	t.Setenv("KONTRA_HANDLER_BIN", fakeHandler(t))

	built := false
	old := runPackBuild
	runPackBuild = func(_ context.Context, _ packOpts) (string, error) {
		built = true
		return runtimeDigest, nil
	}
	t.Cleanup(func() { runPackBuild = old })

	deployFolder := func(t *testing.T, extra string) error {
		t.Helper()
		dir := t.TempDir()
		if err := writeFileForTest(filepath.Join(dir, "actor.json"), `{"name":"echo","version":"0.1.0"}`); err != nil {
			t.Fatal(err)
		}
		if extra != "" {
			if err := writeFileForTest(filepath.Join(dir, extra), "FROM scratch\nRUN apt-get install -y chromium\n"); err != nil {
				t.Fatal(err)
			}
		}
		built = false
		_, err := runDeploy(context.Background(), io.Discard, deployOpts{actorDir: dir, hostOnly: true})
		return err
	}

	if err := deployFolder(t, ""); err != nil {
		t.Fatalf("the control deploy (no Dockerfile) failed, so nothing below can tell a refusal from "+
			"an unrelated failure: %v", err)
	}
	if !built {
		t.Fatal("the control deploy (no Dockerfile) never reached pack; the refusal cases below would pass vacuously")
	}

	for _, name := range []string{"Dockerfile", "runtime.Dockerfile"} {
		t.Run(name, func(t *testing.T) {
			err := deployFolder(t, name)
			if err == nil {
				t.Fatalf("an actor carrying %s deployed; it should have been refused", name)
			}
			if built {
				t.Fatalf("pack ran for an actor carrying %s — the refusal has to come before the build", name)
			}
			if msg := err.Error(); !strings.Contains(msg, name) || !strings.Contains(msg, "kontra-runtimes") {
				t.Fatalf("refused, but not by the Dockerfile check:\n%s", msg)
			}
		})
	}
}

func writeFileForTest(path, body string) error {
	return os.WriteFile(path, []byte(body), 0o644)
}

// THE BUILDER PIN IS COMPARED TO THE CONFORMANCE CORPUS, not merely written down twice.
//
// `shared/conformance/catalog.json` carries the `builderDigest` a worker is expected to register,
// and `cli/packbuild.go` carries the digest a build is actually run against. Those are the two ends
// of one fact: if they drift, actors are registered against a builder the corpus says they were not
// built with, and nothing fails — the catalog simply records a value no fixture agrees with.
//
// THE CORPUS IS THE SOURCE AND THIS IS THE IMPLEMENTATION, which is the repo's rule for anything
// with more than one implementation. A bump therefore starts in the JSON.
func TestTheBuilderDigestMatchesTheConformanceCorpus(t *testing.T) {
	root, err := cliutil.FindRepoRoot("")
	if err != nil {
		t.Skipf("not in a checkout, so the corpus is not here: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "shared", "conformance", "catalog.json"))
	if err != nil {
		t.Fatalf("reading the catalog corpus: %v", err)
	}
	var corpus struct {
		Expect struct {
			BuilderDigest string `json:"builderDigest"`
		} `json:"expect"`
	}
	if err := json.Unmarshal(raw, &corpus); err != nil {
		t.Fatalf("the catalog corpus is not valid JSON: %v", err)
	}
	// THE SHAPE IS ASSERTED BEFORE THE VALUE IS COMPARED. That file's `schemas` block DESCRIBES
	// `builderDigest` in prose and its `expect` block carries the value, so a reader that picked the
	// wrong one would compare a sentence to a digest — or, if the key moved, compare "" to "" and
	// pass. Verified: this guard fired on the first version of this test, which read `cases[]`.
	got := corpus.Expect.BuilderDigest
	if !strings.HasPrefix(got, "sha256:") {
		t.Fatalf("shared/conformance/catalog.json's `expect.builderDigest` is %q, not a digest — "+
			"this comparison matched nothing and would pass whatever the pin was", got)
	}
	if got != builderDigest {
		t.Errorf("the corpus expects builderDigest %s and cli/packbuild.go pins %s", got, builderDigest)
	}
	if !strings.HasSuffix(pinnedBuilder(), "@"+builderDigest) {
		t.Errorf("pinnedBuilder() = %q, which does not end in the pinned digest", pinnedBuilder())
	}
}

// THE `pack` VERSION THIS CLI EXPECTS IS THE ONE THE IMAGE INSTALLS.
//
// `packBinary`'s refusal names `packVersion` as the version to install, and the `kontra` image
// fetches `PACK_VERSION` and checksums it. If those two drift, the CLI tells an operator to install
// a version the image it is running inside does not have — advice that is wrong in the one place it
// is read. Both move together or this fails.
func TestThePackVersionMatchesTheImageThatShipsIt(t *testing.T) {
	root, err := cliutil.FindRepoRoot("")
	if err != nil {
		t.Skipf("not in a checkout: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "control", "images", "Dockerfile.selfcontained"))
	if err != nil {
		t.Fatalf("reading the image that ships pack: %v", err)
	}
	m := regexp.MustCompile(`(?m)^ARG PACK_VERSION=(\S+)$`).FindSubmatch(raw)
	if m == nil {
		t.Fatal("no `ARG PACK_VERSION=` in Dockerfile.selfcontained — this comparison matched " +
			"nothing and would pass whatever the constant said")
	}
	if got := string(m[1]); got != packVersion {
		t.Errorf("the image installs pack %s and cli/packbuild.go expects %s", got, packVersion)
	}
}
