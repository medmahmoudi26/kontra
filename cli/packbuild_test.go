package main

import (
	"os"
	"strings"
	"testing"
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

func writeFileForTest(path, body string) error {
	return os.WriteFile(path, []byte(body), 0o644)
}
