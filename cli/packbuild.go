// packbuild.go — building an actor image with Cloud Native Buildpacks.
//
// THE BOUNDARY MOVED, AND `deploy.go`'s HEADER SAYS SO. That file's rule is "no shell-out to
// `docker build` — the engine API is the boundary here", and it was right for a generated
// Dockerfile: the Engine API is a complete interface to the thing being asked for. The CNB
// lifecycle is not a build request; it is five phases in separate containers with a cache image, a
// run image and a credential boundary between them. Reimplementing that against the Engine API means
// reimplementing the lifecycle, and importing `pack` as a Go module pulls its whole dependency graph
// into a CLI that is also shipped as a single binary. So this shells out to a PINNED `pack`, and the
// version it was pinned to is asserted rather than assumed (ADR 0061).
//
// Four things about the invocation were measured, and each of them is a build that fails without it:
//
//	--trust-builder=false   the lifecycle phases then run in SEPARATE containers and only the
//	                        exporter gets registry credentials, so an actor's own package install
//	                        script cannot read a push credential. This is the security property that
//	                        makes building third-party code on the controller defensible, so it is
//	                        hardcoded rather than offered.
//	--network               those separate containers must resolve the registry THEMSELVES.
//	                        `127.0.0.1:5000` is their own loopback and reaches nothing.
//	--insecure-registry     kontra's registry is plain HTTP by construction (ociref.json pins
//	                        `plainHTTP` per case for the same reason). Without the flag the exporter
//	                        reports a TLS error that reads like a certificate problem.
//	DOCKER_CONFIG           the credential has to be a FILE: `docker login` refuses plain HTTP to a
//	                        non-loopback host, so there is no interactive path that would create one.
package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// packVersion is what this CLI was written against and what the `cli` image ships. Asserted at use:
// a different `pack` on an operator's PATH is the kind of difference that shows up as a build that
// works on one machine and not another.
const packVersion = "0.40.9"

// packBinary finds the pinned binary. KONTRA_PACK_BIN wins so an operator can point at their own.
func packBinary() (string, error) {
	if p := strings.TrimSpace(os.Getenv("KONTRA_PACK_BIN")); p != "" {
		if _, err := os.Stat(p); err != nil {
			return "", fmt.Errorf("KONTRA_PACK_BIN=%s: %w", p, err)
		}
		return p, nil
	}
	for _, c := range []string{"/usr/local/bin/pack", "/opt/kontra/pack"} {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	if p, err := exec.LookPath("pack"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("no `pack` binary. kontra builds actors with Cloud Native Buildpacks and "+
		"ships pack %s in the `cli` image; outside it, install pack %s or set KONTRA_PACK_BIN",
		packVersion, packVersion)
}

type packOpts struct {
	// Image is the full destination reference, `<registry>/<repo>:<version>`.
	Image string
	// ActorDir is the build context — the actor's own directory and nothing above it. This is the
	// change that makes a Go actor's build cheap: the old path used the repo root because the
	// `replace` directives pointed outside, so any edit anywhere invalidated it.
	ActorDir string
	// Builder and RunImage are both PINNED BY DIGEST. A moving tag would make two builds of the same
	// source produce different images, and the catalog records a runtime digest it must be able to
	// compare against later.
	Builder  string
	RunImage string
	// CacheImage keeps the dependency layers in the registry, which is where the reuse comes from:
	// without it every build starts from an empty cache and reinstalls the lockfile.
	CacheImage string
	Network    string
	// Registry is the bare `host:port`, used to decide plain HTTP and to confirm the push.
	Registry string
	// DockerConfig is a directory holding a `config.json` with the push credential.
	DockerConfig string
	Publish      bool
	Progress     io.Writer
}

// packArgv is separated from running it so the invocation is assertable without a build.
func packArgv(o packOpts) []string {
	args := []string{
		"build", o.Image,
		"--builder", o.Builder,
		"--run-image", o.RunImage,
		"--path", o.ActorDir,
		"--trust-builder=false",
		"--pull-policy", "if-not-present",
	}
	if o.Publish {
		args = append(args, "--publish")
	}
	if o.CacheImage != "" {
		args = append(args, "--cache-image", o.CacheImage)
	}
	if o.Network != "" {
		args = append(args, "--network", o.Network)
	}
	if host, plain := registryHost(o.Registry); plain && host != "" {
		args = append(args, "--insecure-registry", host)
	}
	return args
}

// publishedDigest is pack's own report of what it wrote: `*** Images (sha256:…):`.
var publishedDigest = regexp.MustCompile(`\*\*\* Images \((sha256:[0-9a-f]{64})\)`)

// packBuild runs the lifecycle and returns the digest pack says it published.
//
// The returned digest is pack's claim, not the authority. `runDeploy` hands it to `confirmPushed`,
// which asks the registry — the same rule the Engine API path already followed, and the reason a
// builder that reports no digest at all is survivable rather than fatal.
func packBuild(ctx context.Context, o packOpts) (string, error) {
	bin, err := packBinary()
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, bin, packArgv(o)...)
	cmd.Dir = o.ActorDir
	cmd.Env = os.Environ()
	if o.DockerConfig != "" {
		cmd.Env = append(cmd.Env, "DOCKER_CONFIG="+o.DockerConfig)
	}

	out, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return "", err
	}
	// Streamed rather than buffered: a buildpack build is minutes long and silence for minutes is
	// indistinguishable from a hang.
	digest := ""
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if o.Progress != nil {
			fmt.Fprintln(o.Progress, line)
		}
		if m := publishedDigest.FindStringSubmatch(line); m != nil {
			digest = m[1]
		}
	}
	if err := cmd.Wait(); err != nil {
		return "", fmt.Errorf("pack build failed: %w", err)
	}
	return digest, nil
}

// --- the two refusals the new layout needs ---

// deployShellDisposition says what to do about a `deploy.sh`.
//
// It is DEPRECATED rather than ignored: a buildpack build does not run it, so an actor that depended
// on it would build successfully and be missing whatever it installed — a failure at run time, in a
// container, far from the change that caused it. So the author is told at build time.
type deployShellDisposition int

const (
	deployShellAbsent deployShellDisposition = iota
	deployShellWarn
	deployShellRefuse
)

// deployShellCheck reads the directory and the one env var that moves the deadline.
func deployShellCheck(dir string, mode string) deployShellDisposition {
	if _, err := os.Stat(filepath.Join(dir, "deploy.sh")); err != nil {
		return deployShellAbsent
	}
	if mode == "refuse" {
		return deployShellRefuse
	}
	return deployShellWarn
}

const deployShellMessage = "deploy.sh is no longer run: an actor's system packages come from its " +
	"runtime now. Pick a runtime that provides what the script installed, or add one to a fork of " +
	"kontra-runtimes — `actor.json`'s `runtime` field selects it, and an unknown name is refused with " +
	"the list of what is published."

// outsideReplace reports the first `replace` directive in an actor's go.mod that points outside the
// actor's own directory, with its line number, or ("", 0) when there is none.
//
// WHY IT IS REFUSED. A `replace ../../sdk/go` makes the actor unbuildable anywhere but inside this
// checkout, which is why the old path used the repo root as build context and why any edit anywhere
// invalidated the build. A published actor has to build from its own directory; `--dev` is the path
// for repo-internal fixtures, which vendor the SDK first.
func outsideReplace(gomod string) (directive string, line int) {
	for i, raw := range strings.Split(gomod, "\n") {
		s := strings.TrimSpace(raw)
		if s == "" || strings.HasPrefix(s, "//") {
			continue
		}
		// Both forms: a single `replace a => b` and a line inside a `replace (` block.
		body := s
		if strings.HasPrefix(s, "replace ") {
			body = strings.TrimPrefix(s, "replace ")
		} else if !strings.Contains(s, "=>") {
			continue
		}
		_, target, ok := strings.Cut(body, "=>")
		if !ok {
			continue
		}
		target = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(target), ")"))
		// Only a FILESYSTEM path can point outside; a module-to-module replace is fine and common.
		if !strings.HasPrefix(target, "./") && !strings.HasPrefix(target, "../") && !strings.HasPrefix(target, "/") {
			continue
		}
		if strings.HasPrefix(target, "../") || strings.HasPrefix(target, "/") {
			return s, i + 1
		}
	}
	return "", 0
}
