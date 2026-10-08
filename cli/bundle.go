package main

// Building a **Bundle** — the Artifact for a machine Target (infra/CONTEXT.md).
//
// A Bundle is a tarball identified by the sha256 of its own bytes, fetched over the VPC and
// verified on arrival. That verification is what makes it an Artifact rather than a copy of some
// files — "what is actually running" has to be answerable from the deploy command alone, and this
// fleet has answered it wrongly three times.
//
// IT TRAVELS AS AN OCI ARTIFACT (ADR 0036), not as an object in the Controller's store. The tar is
// byte-for-byte what it always was; what changed is the envelope and the addressing, and the
// publishing half of this file is where that lives. An **Image** and a **Bundle** are still
// different bytes built by different paths, but they are now carried, mirrored and addressed by
// one mechanism — which is what makes an airgap a registry mirror instead of an object-store copy
// with kontra-specific tooling.
//
// What goes in: the actor, both Python SDK seams, and the handler binary — that is the whole of it
// since ADR 0018 removed the sidecar. What deliberately does NOT: any credential. The Bundle is
// world-readable on an anonymous registry by design; the Machine gets its secrets from the
// environment the install writes.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/memory"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/retry"

	"github.com/medmahmoudi26/kontra/cli/internal/cliutil"
	"github.com/medmahmoudi26/kontra/cli/internal/ociref"
)

type bundle struct {
	Name    string
	Version string
	Engine  string
	SHA     string // sha256 of the tar.gz — the Bundle's identity, and the artifact's layer digest
	Bytes   []byte
}

// buildBundle assembles the Bundle for `actorDir`. The handler is compiled for linux/amd64
// here rather than on the Machine: a Machine has no Go toolchain and should not need one, and
// compiling once means every Machine in a Fleet runs bytes that were built together.
func buildBundle(actorDir string, progress io.Writer) (*bundle, error) {
	m, err := readManifest(actorDir)
	if err != nil {
		return nil, err
	}
	root, err := cliutil.FindRepoRoot("")
	if err != nil {
		return nil, fmt.Errorf("building a Bundle needs the checkout (handler/ + sdk/ + runtime/): %w", err)
	}
	engine := "py"
	if _, err := os.Stat(filepath.Join(actorDir, "go.mod")); err == nil {
		engine = "go"
	}

	fmt.Fprintf(progress, "compiling the handler for linux/amd64\n")
	handlerBin, err := buildHandler(root)
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(filepath.Dir(handlerBin))

	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	tw := tar.NewWriter(zw)

	add := func(name string, mode int64, body []byte) error {
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: mode, Size: int64(len(body)),
			// A FIXED mtime, so the same inputs produce the same sha. A Bundle whose identity
			// changed every build would redeploy the Fleet on every command.
			ModTime: time.Unix(0, 0), Typeflag: tar.TypeReg,
		}); err != nil {
			return err
		}
		_, err := tw.Write(body)
		return err
	}

	// 1) the actor itself
	if err := addTree(tw, actorDir, "actor/"+m.Name, nil); err != nil {
		return nil, err
	}
	// 2) the two Python seams, mirroring the image's layout so PYTHONPATH is the same in both
	//    Targets and an author's `from kontra import actor` resolves identically. BOTH of them,
	//    not one: `sdk/python` carries the author surface the actor's own code names, and
	//    `runtime/python` carries the engine and the Temporal host that `actor.serve()` hands
	//    control to. A Bundle with only the first imports cleanly and then dies at the handoff.
	//
	//    There is no repo-root `actorkit/__init__.py` to ship beside them any more. It was a shim
	//    that existed only because the old seam directory `actorkit/` shadowed the import name;
	//    `sdk/python/kontra/` is a real package directory and needs nothing pointing at it.
	if engine == "py" {
		skip := func(p string) bool {
			return strings.Contains(p, "__pycache__") || strings.HasSuffix(p, ".pyc") ||
				strings.Contains(p, ".egg-info")
		}
		if err := addTree(tw, filepath.Join(root, "sdk", "python"), "sdk/python", skip); err != nil {
			return nil, err
		}
		if err := addTree(tw, filepath.Join(root, "runtime", "python"), "runtime/python", skip); err != nil {
			return nil, err
		}
	}
	// 2b) A GO ACTOR IS A BINARY, NOT SOURCE, and this is where that was missed. `addTree` above
	// ships the actor directory verbatim, which is right for Python — the Machine has an
	// interpreter and the two Python seams went with it — and wrong for Go: the Machine has no Go
	// toolchain, by the same decision recorded on `buildHandler` one function down. So the
	// systemd unit exec'd `/opt/kontra/actor/<name>/<name>`, found `main.go` and `go.mod` beside
	// no executable at all, and reported "No such file or directory".
	//
	// MEASURED, and this is why it earns a comment rather than a one-liner: the failure surfaces
	// only AFTER `fleet.up` has provisioned four Droplets and placed the Bundle, roughly three
	// minutes into a run, as a systemd exec error inside a Pulumi resource log. Nothing
	// before that point is wrong — the build succeeds, the Bundle uploads, its sha is served, the
	// Machines come up. `kontra deploy`'s Image path had the engine detection all along; the
	// Bundle path, which is what every doc example builds, did not.
	if engine == "go" {
		fmt.Fprintf(progress, "compiling the actor for linux/amd64\n")
		actorBin, aerr := buildGoActorBinary(actorDir, m.Name)
		if aerr != nil {
			return nil, aerr
		}
		defer os.RemoveAll(filepath.Dir(actorBin))
		ab, rerr := os.ReadFile(actorBin)
		if rerr != nil {
			return nil, rerr
		}
		// The name the entrypoint execs: actor/<name>/<name>, beside its own source.
		if err := add("actor/"+m.Name+"/"+m.Name, 0o755, ab); err != nil {
			return nil, err
		}
	}
	// 3) the handler
	hb, err := os.ReadFile(handlerBin)
	if err != nil {
		return nil, err
	}
	if err := add("bin/handler", 0o755, hb); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}

	sum := sha256.Sum256(gz.Bytes())
	return &bundle{
		Name:    m.Name,
		Version: m.Version,
		Engine:  engine,
		SHA:     hex.EncodeToString(sum[:]),
		Bytes:   gz.Bytes(),
	}, nil
}

// addTree walks `dir` into the tar under `prefix`.
func addTree(tw *tar.Writer, dir, prefix string, skip func(string) bool) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(dir, p)
		if rerr != nil {
			return rerr
		}
		if rel == "." {
			return nil
		}
		name := filepath.ToSlash(filepath.Join(prefix, rel))
		if skip != nil && skip(name) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			// __pycache__ in a Bundle is stale bytecode compiled by a different interpreter on
			// a different machine — python will happily prefer it.
			if d.Name() == "__pycache__" || d.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		body, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		mode := int64(0o644)
		if info.Mode()&0o111 != 0 {
			mode = 0o755
		}
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: mode, Size: int64(len(body)),
			ModTime: time.Unix(0, 0), Typeflag: tar.TypeReg,
		}); err != nil {
			return err
		}
		_, werr := tw.Write(body)
		return werr
	})
}

// buildHandler cross-compiles the Temporal handler. GOWORK=off because handler/ is its own
// module; CGO_ENABLED=0 so the binary runs on a Machine whose libc we never chose.
func buildHandler(root string) (string, error) {
	out, err := os.MkdirTemp("", "kontra-handler-")
	if err != nil {
		return "", err
	}
	bin := filepath.Join(out, "handler")
	cmd := exec.Command("go", "build", "-trimpath", "-o", bin, ".")
	cmd.Dir = filepath.Join(root, "runtime", "handler")
	cmd.Env = append(os.Environ(), "GOWORK=off", "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64")
	if b, err := cmd.CombinedOutput(); err != nil {
		os.RemoveAll(out)
		return "", fmt.Errorf("compiling the handler: %v\n%s", err, b)
	}
	return bin, nil
}

// buildGoActorBinary compiles a Go actor for the Machine, for the same reason buildHandler compiles the
// handler: a Machine has no Go toolchain and should not need one, and building once means every
// Machine in a Fleet runs bytes that were built together.
//
// GOWORK=off because an example actor is its own module and is deliberately not in `go.work` —
// with the workspace active, `go build` resolves the repo's modules instead of the actor's own
// go.mod and fails on a checkout that is otherwise fine.
func buildGoActorBinary(actorDir, name string) (string, error) {
	out, err := os.MkdirTemp("", "kontra-actor-")
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(actorDir)
	if err != nil {
		os.RemoveAll(out)
		return "", err
	}
	// Named for the ACTOR, not for its directory: the entrypoint execs <name>/<name>, and an
	// actor whose folder is named something else would build a binary nothing looks for.
	bin := filepath.Join(out, name)
	cmd := exec.Command("go", "build", "-trimpath", "-o", bin, ".")
	cmd.Dir = abs
	cmd.Env = append(os.Environ(), "GOWORK=off", "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64")
	if b, err := cmd.CombinedOutput(); err != nil {
		os.RemoveAll(out)
		return "", fmt.Errorf("compiling the actor %s: %v\n%s", name, err, b)
	}
	return bin, nil
}

// --- publishing: a Bundle is an OCI artifact ------------------------------------------------
//
// WHAT MOVED, AND WHERE EACH PIECE WENT (ADR 0036). The left column is gone from this tree:
//
//	kontra-bundles/<n>/<v>/<sha>.tar.gz   the artifact's one LAYER, named by that same sha256
//	kontra-bundles/<n>/<v>/latest.json    the TAG `<n>:<v>`, resolving to a manifest digest
//	  its `engine` field                  the CONFIG blob, which the manifest digest covers
//	  its `key` field                     nothing: a blob's address IS its digest
//
// THE POINTER'S JOB IS DONE BY A TAG, AND ITS ONE BUG GOES WITH IT. `latest.json` was mutable and
// nothing verified it, while the tarball beside it was verified on arrival — so the engine, the
// single field a listing could not supply, was the one field a Machine took on trust. A pointer
// saying `py` for a Go actor produced a Machine that installed cleanly, started nothing, and
// reported a successful deploy (`control/orchestrator/src/activities/fleet.ts` says the same thing from the
// reading side). In a manifest the engine is one of the bytes the manifest digest is computed
// over, so that lie is now a digest mismatch rather than a silent misplacement.
//
// WHAT DID NOT CHANGE IS THE IDENTITY. `KONTRA_ACTOR_DIGEST` carries `sha256:<hex>` on both
// Targets and nothing parses it (ADR 0032); a Bundle's value is still the sha256 of its own
// tar.gz, which is now also the layer descriptor's digest. The manifest digest is a second, outer
// name for the same content and is what `<n>:<v>` resolves to — see bundleArtifact.
//
// WHY oras-go AND NOT THE `oras` BINARY. A Bundle is pushed by the CLI and pulled by a Machine,
// and a shell-out would have to be installed on both. The Machine's half is ONE anonymous GET of
// one blob followed by `sha256sum` — two commands cloud-init already installs — so putting a
// binary there would add a runtime dependency to every Machine in every Fleet in order to make a
// request `curl` already makes. On this side the library is the cheap half of the trade: oras-go/v2
// is 3.3 MB of module cache and its three dependencies (image-spec, go-digest, x/sync) are already
// in cli/go.sum at the versions it wants, so it adds no module this binary did not already build.
// That is the opposite finding to `cli/internal/testregistry/server.go`'s refusal of `distribution`
// (measured there at 233 MB), and the difference is the whole reason both decisions are written
// down: the server would have had to reimplement a storage driver, the client makes a handful of
// requests and gets the upload-Location handling — absolute versus relative, chunked fallback —
// that is the classic hand-rolled-push bug.
const (
	// bundleArtifactType is what the registry is told this manifest IS. `artifactType` on an OCI
	// 1.1 manifest is the field a mirror, a scanner or a `kontra` reader filters on, and it is why
	// a Bundle cannot be mistaken for a runnable image by anything that pulls by type.
	bundleArtifactType = "application/vnd.kontra.bundle.v1+json"
	// bundleConfigType tags the config blob — what used to be latest.json.
	bundleConfigType = "application/vnd.kontra.bundle.config.v1+json"
	// bundleLayerType tags the tar.gz. `+gzip` because it is one, and because a mirror that
	// understands the suffix will not try to recompress it.
	bundleLayerType = "application/vnd.kontra.bundle.layer.v1.tar+gzip"
)

// bundleRepoPrefix separates an actor's Bundle from its Image in the SAME registry.
//
// `kontra deploy` pushes an Image to `<registry>/<name>:<version>`. A Bundle for the same
// actor@version is different bytes for a different Target, so it needs a name that cannot collide
// with that one — an OCI repository name is a path, and `bundles/nscheck:0.1.0` says which of the
// two it is without a second lookup. ADR 0036 ends with one Target and therefore one repository;
// until then the prefix is what keeps two Artifacts of one actor apart.
const bundleRepoPrefix = "bundles"

// bundleRepo is the OCI repository a Bundle is published to.
//
// A PURE DERIVATION AND NOT A GUARD, because `shared/conformance/bundleref.json` drives it from two
// languages and a validating version would have to be validated identically in both. The grammar
// is checked exactly once, by the OCI reference parser inside `pushBundle` — see the refusal
// there for why an actor name that serves fine cannot always be published.
func bundleRepo(name string) string { return bundleRepoPrefix + "/" + name }

// bundleBlobURL is where the Bundle's BYTES are: the distribution spec's blob endpoint, which is
// the only address a Machine ever fetches from.
//
// The digest is in the path, which is worth saying because it is what lets the caller pair the URL
// with the sha it is about to check — `validateMachineActor` refuses a pair that disagrees. Under
// the object store those were two independent strings and a mismatched pair was a 65 MiB download
// that failed at the end.
//
// THE SCHEME FOLLOWS THE REGISTRY STRING and is not assumed, via `registryBase` — the same helper
// `kontra deploy` resolves an Image's registry with. A bare `host:port` is plain HTTP, which is
// what the Controller serves and what the object store this replaces was; an operator who typed
// `https://mirror` gets https. Hardcoding `http://` here would silently downgrade exactly the
// deployment that bothered to configure TLS, on the one request that leaves the VPC.
//
// IT IS THE CONVENTIONAL DESTINATION'S OWN blobURL, not a second formatter that agrees with it.
// `kontra fleet deploy` has a registry string and an actor name; `kontra build --push` has a whole
// destination, whose repository may not be `bundles/<name>` at all. Those are two callers with two
// inputs and ONE question, and this repo's own habit is to name what a second answer costs: the
// pointer and the tarball that disagreed, the push address and the fetch address that disagreed.
// `shared/conformance/bundleref.json` drives this side, so a divergence would only ever have been caught
// on the path the corpus does not cover.
// The version is passed empty because a BLOB is addressed by its digest and never by a tag — that
// is the whole property `bundleref.json` calls the pairing invariant. Nothing downstream of here
// reads the tag.
func bundleBlobURL(reg, name, sha string) string {
	return conventionalBundleDest(reg, name, "").blobURL(sha)
}

// bundleConfig is the artifact's config blob: what a resolver needs and cannot cliutil.Derive from a
// listing, now inside the digest instead of beside it.
//
// The engine is the reason it exists. A tag already answers "which Bundle is `0.1.0` today", but
// nothing in a digest says whether the Machine execs `/opt/kontra/actor/<n>/<n>` or points python
// at `actor.py`. Name and version ride along so the blob is self-describing when it is read out of
// a mirror with no tag in hand.
type bundleConfig struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Engine  string `json:"engine"`
}

// bundleArtifact is a published Bundle: the two names it now has, and where its bytes are.
//
// TWO DIGESTS, AND THEY ARE NOT INTERCHANGEABLE. `Digest` is the manifest's — the artifact's
// address, what the tag resolves to, and what a `@sha256:…` reference pins. `LayerSHA` is the
// tar.gz's, which is the Bundle's identity in ADR 0032's sense and the value every Machine
// verifies. Keeping both named is deliberate: collapsing them into "the digest" is how a reader
// ends up checking one and reporting the other.
// Registry here is SCHEME-LESS on purpose: an OCI reference may not carry one, so `ref()` and
// `pinned()` would be unusable if it did. A URL may and must, which is why `bundleBlobURL` is
// given the operator's own registry string rather than this field.
type bundleArtifact struct {
	Registry string
	Repo     string
	Tag      string
	Digest   string // the OCI manifest digest, `sha256:<hex>`
	LayerSHA string // sha256 of the tar.gz, hex — what a Machine checks on arrival
}

// ref is the mutable name: `<registry>/bundles/<name>:<version>`.
func (a bundleArtifact) ref() string { return a.Registry + "/" + a.Repo + ":" + a.Tag }

// pinned is the immutable one: `<registry>/bundles/<name>@sha256:…`. This is the reference to
// paste into a mirror command, because it cannot mean something different tomorrow.
func (a bundleArtifact) pinned() string { return a.Registry + "/" + a.Repo + "@" + a.Digest }

// bundleCreated is the `org.opencontainers.image.created` annotation, FIXED at the epoch.
//
// oras writes `time.Now()` there unless it is given one, and that alone would move the manifest
// digest on every build of identical bytes. The tar already sets `ModTime: time.Unix(0, 0)` for
// exactly this reason and says why — "a Bundle whose identity changed every build would redeploy
// the Fleet on every command" — so the envelope has to make the same promise the payload does.
var bundleCreated = time.Unix(0, 0).UTC().Format(time.RFC3339)

// bundleDest is WHERE a Bundle is published: a whole OCI reference, plus whether the registry is
// spoken to over plain HTTP.
//
// IT IS A REFERENCE AND NOT A REGISTRY, and that is slice 07 (ADR 0036: "kontra owns no registry.
// `kontra build --push <ref>` takes any OCI reference"). Until now the repository and the tag were
// DERIVED — `bundles/<name>` and the manifest's version — from one operator input, the registry
// host. That is still the default and still what a Fleet placement resolves, but it can no longer be
// the only expressible answer: the whole point is that an Artifact lands in a registry the customer
// owns, under whatever name their CI already uses.
type bundleDest struct {
	ociref.Ref
	// PlainHTTP is a property of the ADDRESS and not of the reference, which is why it is not on
	// ociref.Ref: an OCI reference may not carry a scheme, and `registryHost` is the one place that
	// reads the operator's scheme and strips it.
	PlainHTTP bool
}

// blobURL is where THIS destination's bytes end up — the distribution spec's blob endpoint, which is
// the only address a Machine ever fetches from.
//
// THE SCHEME FOLLOWS THE DESTINATION and is not assumed. `bundleBlobURL` answers the same question
// from a registry string and an actor name, for the callers that have those and not a destination
// (`kontra fleet deploy`); this one exists so `kontra build --push https://mirror.internal/…` prints
// an https URL rather than silently downgrading the one deployment that bothered to configure TLS,
// on the one request that leaves the VPC.
func (d bundleDest) blobURL(sha string) string {
	scheme := "https://"
	if d.PlainHTTP {
		scheme = "http://"
	}
	return fmt.Sprintf("%s%s/v2/%s/blobs/sha256:%s", scheme, d.Domain, d.Path, sha)
}

// conventionalBundleDest is where a Bundle goes when nobody said otherwise:
// `<registry>/bundles/<name>:<version>`.
//
// THE CONVENTION IS NOT DEAD AND MUST NOT BE, because it is the only address the control plane can
// resolve without being told: `control/orchestrator/src/activities/fleet.ts:resolveBundle` builds
// `<registry>/v2/bundles/<actor>/manifests/<version>` from the actor and the version alone, and
// `shared/conformance/bundleref.json` pins that on both sides. A `--push` to some other repository
// publishes an Artifact that a Fleet placement cannot find — see `cmdBuild`, which prints that
// rather than leaving it to be discovered.
func conventionalBundleDest(reg, name, version string) bundleDest {
	host, plain := registryHost(reg)
	return bundleDest{
		Ref:       ociref.Ref{Domain: host, Path: bundleRepo(name), Tag: version, TagSet: true},
		PlainHTTP: plain,
	}
}

// pushDestination resolves and JUDGES where `kontra build` publishes, and it is the third site that
// names an **Artifact** — the one `.scratch/warden/issues/15-*` warned must not add a fourth
// bespoke message. The grammar comes from cli/internal/ociref/ociref.go; what is added here is the two refusals that
// are true of a DESTINATION and of nothing else.
//
//	--push AND --registry        two answers to one question. `--push` is the whole reference and
//	                             `--registry` is only its first component, so a disagreement would
//	                             be silently resolved by whichever this function read first.
//	a digest-pinned destination  THE DIGEST IS THE PUSH'S OUTPUT. `<repo>@sha256:…` names content
//	                             that does not exist until these bytes are uploaded, so a reference
//	                             carrying one is either a copy-paste of a previous build or a
//	                             misunderstanding, and both deserve a sentence rather than a
//	                             registry-side 400.
//	no registry in the reference THIS IS DOCKER HUB. `nscheck:0.1.0` and `acme/nscheck:0.1.0` are
//	                             legal references that resolve to docker.io, and Docker Hub is
//	                             rate-limited at 100 manifest GETs per hour PER IP — one Fleet
//	                             pulling one actor exhausts it, and the failure lands on every
//	                             unrelated build on the same egress address. Measured on this box.
//
// THE LAST ONE ALSO GUARDS THE DEFAULT PATH, which is why it is a check on the resolved reference
// rather than on the flag. `KONTRA_REGISTRY=myregistry` (no dot, no port, not `localhost`) pushes to
// a host called `myregistry` while every reference kontra then PRINTS —
// `myregistry/bundles/nscheck:0.1.0`, copy-pasteable into `oras`, `skopeo` or `podman` — parses back
// as a Docker Hub repository owned by a user called `myregistry`. The invariant is that a reference
// kontra prints resolves to the place kontra pushed, and that is exactly the distribution spec's own
// "does the first component look like a host" rule.
func pushDestination(pushFlag, registryFlag, controllerFlag, name, version string) (bundleDest, error) {
	push, regFlag := strings.TrimSpace(pushFlag), strings.TrimSpace(registryFlag)

	var dest bundleDest
	switch {
	case push != "" && regFlag != "":
		return dest, fmt.Errorf("--push %q and --registry %q are two answers to one question: "+
			"--push is the WHOLE reference and --registry is only its first component.\n"+
			"  Pass one. `--push %s/%s:%s` is what --registry %s would have produced",
			push, regFlag, regFlag, bundleRepo(name), version, regFlag)

	case push != "":
		// The scheme comes off before the reference is parsed, because an OCI reference may not carry
		// one and `https://mirror/x:1` would otherwise split as host `https:` — a domain-shaped string,
		// so it would pass every check and push to a repository called `/mirror/x`.
		bare, plain := ociref.PushTransport(push)
		r, err := ociref.Parse(bare)
		if err != nil {
			return dest, fmt.Errorf("--push %q is not a destination.\n%w\n"+
				"  Nothing about the Artifact changes this: the reference is the string, and a registry\n"+
				"  cannot be asked for a name the grammar cannot express", push, err)
		}
		if r.Digest != "" {
			return dest, fmt.Errorf("--push %q names a digest, and a digest is what this push COMPUTES: "+
				"`sha256:…` addresses content that does not exist until these bytes are uploaded.\n"+
				"  Push to `%s:%s` and read the pinned reference off the output", push, r.Repository(), version)
		}
		// AN UNTAGGED REFERENCE TAKES THE ACTOR'S VERSION rather than `:latest`. `latest` is the one
		// tag whose meaning is "whatever was pushed last", and a Fleet that resolved it would place
		// whichever Artifact won the race.
		if !r.TagSet {
			r.Tag, r.TagSet = version, true
			if err := r.Check(r.Tagged()); err != nil {
				return dest, fmt.Errorf("--push %q has no tag, so the Artifact's own version would be one, "+
					"and it cannot be.\n%w\n  Fix the version in actor.json, or name a tag on --push", push, err)
			}
		}
		dest = bundleDest{Ref: r, PlainHTTP: plain(r.Domain)}

	default:
		dest = conventionalBundleDest(bundleRegistry(regFlag, controllerFlag), name, version)
		if err := dest.Check(dest.Tagged()); err != nil {
			return dest, fmt.Errorf("this Actor cannot be published as a Bundle.\n%w\n"+
				"  A task queue accepts far more than an OCI repository does (shared/conformance/queues.json), so\n"+
				"  %s@%s serves fine and only its ARTIFACT is unnameable. Rename it in actor.json",
				err, name, version)
		}
	}

	// THE REGISTRY MUST BE ONE, and it must be one that a reader of the printed reference resolves
	// to the same place. See this function's header for the two ways to get here.
	if dest.Domain == "" || !ociref.LooksLikeRegistryHost(dest.Domain) {
		named := dest.Domain
		if named == "" {
			named = "(none)"
		}
		return dest, fmt.Errorf("%q names no registry (first component %s), which in the OCI grammar means "+
			"DOCKER HUB.\n"+
			"  kontra owns no registry and will not make one the default: docker.io rate-limits manifest\n"+
			"  reads at 100 per hour PER IP, so one Fleet pulling one actor exhausts it for every build on\n"+
			"  that address.\n"+
			"  Name a host: `--push ghcr.io/<org>/%s:%s`, a GitLab or Harbor address, or `localhost:5000`\n"+
			"  for the single box (the install serves one on :%d).\n"+
			"  A host with no dot and no port is not one — `myregistry/x` is a Docker Hub user called\n"+
			"  `myregistry`, which is what a reference this command PRINTS would resolve to.",
			dest.Tagged(), named, bundleRepo(name), version, defaultRegistryPort)
	}
	return dest, nil
}

// pushBundle publishes the Bundle to the conventional address for its registry. `kontra fleet
// deploy` and this package's tests are its callers; `kontra build` goes through pushBundleTo, which
// is the same act against a destination an operator may have named.
func pushBundle(ctx context.Context, reg string, b *bundle, progress io.Writer) (*bundleArtifact, error) {
	return pushBundleTo(ctx, conventionalBundleDest(reg, b.Name, b.Version), b, progress)
}

// pushBundleTo publishes the Bundle as an OCI artifact and returns both of its names.
//
// Plain HTTP unless the registry is spelled with a scheme: the Controller's registry is anonymous on
// the VPC, which is also why nothing secret is ever put in a Bundle. The bucket-creation dance the
// object store needed has no equivalent — a registry creates a repository on first push — which is
// one class of first-run failure that simply stops existing.
func pushBundleTo(ctx context.Context, dest bundleDest, b *bundle, progress io.Writer) (*bundleArtifact, error) {
	// ═══ THE GRAMMAR IS ASKED HERE AND NOT ONLY IN pushDestination ═══
	//
	// A QUEUE NAME IS NOT SANITISED AND AN OCI REPOSITORY NAME IS, and the two vocabularies genuinely
	// differ — so an actor that serves perfectly can fail to publish. `shared/conformance/queues.json` carries
	// `my actor`, `café` and `a/b` as adversarial cases precisely because Temporal accepts them; the
	// OCI grammar accepts only the third, so `a/b` publishes to `bundles/a/b` (unambiguous, because a
	// TAG is delimited by `:` and not by a slash count) while the other two are refused before any
	// Machine exists.
	//
	// It is HERE because this is the funnel. `pushDestination` guards `kontra build`, but `kontra
	// fleet deploy` reaches `pushBundle` directly and would otherwise get oras's own `invalid
	// repository "bundles/my actor"` — accurate, and unreadable by the person who chose the name. The
	// judgement itself is cli/internal/ociref/ociref.go's, shared with the pull site and the podman driver; what is
	// added is the sentence oras cannot write: which actor, and that actor.json is where the fix goes.
	if err := dest.Check(dest.Tagged()); err != nil {
		return nil, fmt.Errorf("actor %q cannot be published as a Bundle.\n%w\n"+
			"  Task queues accept far more than an OCI repository does (shared/conformance/queues.json), so this\n"+
			"  actor serves fine and only its ARTIFACT is unnameable. Rename it in actor.json.",
			b.Name, err)
	}

	// Staged in memory first, so the manifest is complete and self-consistent before a single
	// byte reaches the registry. `oras.Copy` then pushes the layer and the config BEFORE the
	// manifest that names them, which is the ordering `putBundlePointer` had to arrange by hand
	// and comment on — a pointer written before its artifact failed the sha check on every
	// Machine at once. Here the registry enforces it: a manifest naming a blob it does not hold
	// is rejected.
	store := memory.New()
	layer, err := oras.PushBytes(ctx, store, bundleLayerType, b.Bytes)
	if err != nil {
		return nil, fmt.Errorf("staging the bundle layer: %w", err)
	}
	cfgJSON, err := json.Marshal(bundleConfig{Name: b.Name, Version: b.Version, Engine: b.Engine})
	if err != nil {
		return nil, err
	}
	cfg, err := oras.PushBytes(ctx, store, bundleConfigType, cfgJSON)
	if err != nil {
		return nil, fmt.Errorf("staging the bundle config: %w", err)
	}
	man, err := oras.PackManifest(ctx, store, oras.PackManifestVersion1_1, bundleArtifactType,
		oras.PackManifestOptions{
			ConfigDescriptor:    &cfg,
			Layers:              []ocispec.Descriptor{layer},
			ManifestAnnotations: map[string]string{ocispec.AnnotationCreated: bundleCreated},
		})
	if err != nil {
		return nil, fmt.Errorf("packing the bundle manifest: %w", err)
	}
	// The staging tag is the DESTINATION's, not `b.Version`: `oras.Copy` is given one name for both
	// ends, so a `--push …:nightly` of a `0.1.0` Bundle stages and uploads under one string rather
	// than under two that happen to agree today.
	if err := store.Tag(ctx, man, dest.Tag); err != nil {
		return nil, err
	}

	// The reference has already been judged above, so this error is the parser disagreeing with
	// cli/internal/ociref/ociref.go — which would be a real finding and not an operator's problem.
	repo, err := remote.NewRepository(dest.Repository())
	if err != nil {
		return nil, fmt.Errorf("oras refuses %q, which cli/internal/ociref/ociref.go accepted — the two grammars have "+
			"drifted and shared/conformance/ociref.json is where that gets pinned: %w", dest.Repository(), err)
	}
	repo.PlainHTTP = dest.PlainHTTP
	// THE SAME OMISSION THE ACTOR PUSH HAD. zot refuses an anonymous write once the install has
	// accounts, so `kontra build --push` to this install's own registry failed with
	// `requested access to the resource is denied` — and a Bundle is what a Fleet Machine fetches.
	// Only for this install's registry (cli/registryauth.go): a Bundle pushed to a third-party
	// registry is the operator's `docker login`, not ours to assume.
	if cred := pushCredential(); !cred.anonymous() && isInstallRegistry(dest.Ref.Domain) {
		repo.Client = &auth.Client{
			Client: retry.DefaultClient,
			Cache:  auth.NewCache(),
			Credential: auth.StaticCredential(dest.Ref.Domain,
				auth.Credential{Username: cred.User, Password: cred.Password}),
		}
	}

	// LayerSHA comes off the DESCRIPTOR, not from `b.SHA`, even though the two are the same hash of
	// the same bytes. The value written here is what a Machine verifies its download against and
	// what the manifest tells it to expect; taking it from the descriptor makes those one string
	// rather than two that agree — and "two independent strings that must match" is the shape of
	// the mismatched (url, sha) pair `validateMachineActor` now has to refuse.
	art := &bundleArtifact{
		Registry: dest.Domain, Repo: dest.Path, Tag: dest.Tag,
		Digest: man.Digest.String(), LayerSHA: layer.Digest.Encoded(),
	}
	fmt.Fprintf(progress, "pushing %.1f MiB to %s\n", float64(len(b.Bytes))/(1<<20), art.ref())
	if _, err := oras.Copy(ctx, store, dest.Tag, repo, dest.Tag, oras.DefaultCopyOptions); err != nil {
		// THE TRANSPORT HINT IS HERE AND NOT IN A RETRY. `--push <host>/…` is https unless the host is
		// loopback (ociref.PushTransport), which is right for ghcr and wrong for a plain-HTTP registry on a
		// VPC — and the failure for that is a TLS error nobody reads as "add a scheme". Retrying over
		// plain HTTP on a handshake failure would be the alternative, and that is a downgrade this
		// process performed on its own, on the one request that leaves the box.
		hint := ""
		if !dest.PlainHTTP {
			hint = fmt.Sprintf("\n  this pushed over HTTPS because %s is not loopback; if that registry speaks "+
				"plain HTTP, say so:\n    --push http://%s", dest.Domain, dest.Tagged())
		}
		return nil, fmt.Errorf("pushing %s: %w\n  a Bundle needs an OCI registry the Machines can also reach; "+
			"the install serves one on :%d, and --push <ref> names another%s",
			art.ref(), err, defaultRegistryPort, hint)
	}
	return art, nil
}

// registryHost splits an operator's registry string into the host:port a reference is built from
// and whether to speak plain HTTP. A bare `host:port` is plain HTTP — that is what every address
// in this system has always been, and it is what the install serves — while an explicit `https://`
// is honoured, because the day a Bundle is pushed to somebody else's registry is slice 07's.
func registryHost(reg string) (host string, plainHTTP bool) {
	switch {
	case strings.HasPrefix(reg, "https://"):
		return strings.TrimRight(strings.TrimPrefix(reg, "https://"), "/"), false
	case strings.HasPrefix(reg, "http://"):
		return strings.TrimRight(strings.TrimPrefix(reg, "http://"), "/"), true
	default:
		return strings.TrimRight(reg, "/"), true
	}
}

// bundleRegistry is ONE address for both halves of a Bundle's round trip, and the halves are on
// DIFFERENT HOSTS — which is why `deploy.go:registryAddress` cannot be reused here even though it
// answers the same question for an Image.
//
// That function's two callers, `kontra deploy` and `kontra scale`, are the same box; a Bundle is
// pushed by the CLI and pulled by every Machine in a Fleet, for which `localhost:5000` is its own
// loopback. So the rung order gains the Controller, exactly as `api.go:s3Endpoint` did for the
// store this replaces, and for the same measured reason:
//
//	--registry           the operator said so
//	KONTRA_REGISTRY      the environment said so
//	<controller>:5000    the Controller the Machines will be told to fetch from
//	localhost:5000       nothing said; say the conventional thing
//
// THE CONTROLLER RUNG CLOSES A LIVE SPLIT. `kontra fleet deploy` pushed to
// `KONTRA_S3_ENDPOINT` or `localhost:8333` while telling Machines to fetch from
// `<controller>:8333`, so running it anywhere but ON the Controller placed a Fleet whose every
// Machine 404s. It only ever worked because nobody ran it from a laptop.
func bundleRegistry(flagVal, controllerFlag string) string {
	if v := strings.TrimSpace(flagVal); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("KONTRA_REGISTRY")); v != "" {
		return v
	}
	if host := explicitController(controllerFlag); host != "" {
		return fmt.Sprintf("%s:%d", host, defaultRegistryPort)
	}
	return defaultRegistry
}

// explicitController is `controllerAddress` MINUS its `10.124.0.2` fallback, and it has to track
// that function rung for rung or the split this whole resolver exists to close reopens: the
// address a Bundle is pushed to and the address the placement tells Machines to fetch from are
// derived here and there from the same two inputs, and they must agree.
//
// The fallback is the one rung it deliberately does not follow. A guess must never select a
// registry — pushing 65 MiB into a host nobody configured is a connection refused, and on a single
// box with no controller at all the conventional loopback address is the right answer instead.
func explicitController(flagVal string) string {
	if v := strings.TrimSpace(flagVal); v != "" {
		return v
	}
	return strings.TrimSpace(os.Getenv("KONTRA_CONTROLLER"))
}
