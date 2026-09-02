package main

// `kontra build` — produce an actor's **Artifact** and publish it.
//
// ═══ ONE TARGET, SO NO --target ═══
//
// This command used to take `--target machine|container`, because the domain had exactly two
// Targets. ADR 0036 collapses that axis to one — "actor code runs in a container everywhere it
// runs" — and says what replaces the flag: *"The `--target` flag loses its meaning and `kontra
// build` takes `--push <ref>` instead."* `machine` survives as a DRIVER (`process`, which runs from
// source and produces no Artifact at all), which is a property of the **Warden**'s environment and
// not something a build can be asked for.
//
// The flag is REFUSED rather than ignored. A retired flag that is silently accepted is a script
// that keeps running and quietly builds the other thing; `flag` would otherwise answer `flag
// provided but not defined`, which reads as a typo. `--target container` in particular has a real
// destination — `kontra deploy` — and the refusal names it.
//
// ═══ kontra OWNS NO REGISTRY ═══
//
// ADR 0036: *"`kontra build --push <ref>` takes any OCI reference; the control plane stores the
// digest, never the bytes. `localhost:5000` survives as a convenience for the single-box case and
// becomes one configured endpoint among many. What kontra keeps is meaning — which digest a version
// currently names — not storage or transport."*
//
// So `--push` is a whole reference and it may name anything speaking the distribution spec: GitHub
// Container Registry, GitLab, Harbor, ECR, a mirror in an airgap, or the appliance's own port on
// loopback. There is no kontra registry, no kontra versioning scheme and no kontra deployment store
// to be inside of. `pushDestination` (cli/bundle.go) is where the reference is resolved and judged,
// and cli/ociref.go is the grammar it consults — the same one the pull site and the podman driver
// consult, which is `.scratch/warden/issues/15-*`'s requirement for the third site that names an
// **Artifact**.
//
// ═══ WHAT IT PRINTS, AND THE ONE THING THE PRINT HAS TO ADMIT ═══
//
// A **Fleet** placement resolves a Bundle by CONVENTION and not from a stored reference:
// `control/orchestrator/src/activities/fleet.ts:resolveBundle` builds `<registry>/v2/bundles/<actor>/manifests/
// <version>` out of the actor and the version alone, pinned on both sides by
// `shared/conformance/bundleref.json`. A `--push` to any other repository therefore publishes an Artifact
// that is perfectly good, perfectly mirrorable, and NOT findable by `kontra fleet deploy`. That is a
// real gap and the output says so rather than leaving it to be discovered as a 404 three minutes
// into a run — which is precisely the failure `bundleref.json` was written after.
//
// Build produces an Artifact; deploy places one. `kontra fleet deploy` is the only thing that
// places, and it never builds. That split is what makes "what is actually running" answerable: a
// verb that both built and placed could never say which of the two a given sha came from.
//
// `kontra deploy` remains as the container-Image spelling, because it is what every existing script
// and every operator's muscle memory says.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"strings"
)

// buildOutput is what `--json` prints: one document, on stdout, with nothing else on that channel.
//
// IT EXISTS SO A CI WRAPPER DOES NOT GREP. `kontra/build-actor` (the GitHub Action) and the GitLab
// template both need exactly one value out of a build — the digest — and the alternative is a `sed`
// against a human sentence, which is the "source scrape" shared/conformance/README.md records as one of the
// two drift shapes this repo has already paid for: "They break on any refactor that preserves
// behaviour, and they pass while the values drift."
//
// `placeable` is the field that is not about the Artifact but about what can be done with it: a
// Fleet placement resolves `bundles/<name>` at its own registry by convention
// (`shared/conformance/bundleref.json`), so an Artifact pushed anywhere else is publishable, mirrorable and
// not placeable. A wrapper that wants to gate on that can, instead of reading a note meant for a
// human.
type buildOutput struct {
	Actor      string `json:"actor"`
	Version    string `json:"version"`
	Engine     string `json:"engine"`
	Reference  string `json:"reference"`
	Pinned     string `json:"pinned"`
	Repository string `json:"repository"`
	Digest     string `json:"digest"`
	SHA256     string `json:"sha256"`
	Blob       string `json:"blob"`
	Placeable  bool   `json:"placeable"`
}

// buildFlags is `kontra build`'s flag set together with the pointers it fills.
//
// IT IS A FUNCTION AND NOT A LITERAL INSIDE cmdBuild so that a test can enumerate THE SAME SET the
// command parses, rather than a list written beside it. `fleet_documented_flags_test.go` is the
// precedent and its header says what a list beside it costs: three flags this repo told people to
// type had been gone for months, in copy-pasteable position, with nothing red anywhere.
type buildFlags struct {
	fs         *flag.FlagSet
	actorDir   *string
	push       *string
	controller *string
	registry   *string
	target     *string
	asJSON     *bool
}

func buildFlagSet() *buildFlags {
	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	return &buildFlags{
		fs:         fs,
		actorDir:   fs.String("actor", "", "actor directory (contains actor.json)"),
		push:       fs.String("push", "", "OCI reference to publish the Artifact to (default: <registry>/bundles/<name>:<version>)"),
		controller: fs.String("controller", "", "Controller whose registry the Artifact is published to (default: KONTRA_CONTROLLER)"),
		registry:   fs.String("registry", "", "registry host only, when the rest of the reference is conventional (default: the Controller's, else the running appliance's)"),
		// RETIRED, AND DECLARED SO IT CAN BE REFUSED BY NAME. See this file's header.
		target: fs.String("target", "", "retired (ADR 0036) — see --push"),
		asJSON: fs.Bool("json", false, "print the published Artifact as JSON (for CI: the digest is a field, not a line to grep)"),
	}
}

func cmdBuild(args []string) error {
	f := buildFlagSet()
	actorDir, push, controller, registryFlag, target, asJSON := f.actorDir, f.push, f.controller, f.registry, f.target, f.asJSON
	if err := f.fs.Parse(args); err != nil {
		return err
	}
	if *actorDir == "" {
		return errors.New("usage: kontra build --actor <dir> [--push <ref>]")
	}
	if t := strings.TrimSpace(*target); t != "" {
		return retiredTarget(t)
	}

	m, err := readManifest(*actorDir)
	if err != nil {
		return err
	}
	// THE DESTINATION IS RESOLVED AND JUDGED BEFORE THE BUILD, not after it. A Bundle is tens of MiB
	// and a handler cross-compile; discovering that the reference cannot name an Artifact after
	// spending both is the shape of failure this repo keeps finding late. Nothing about the bytes
	// changes the verdict, so nothing about the verdict has to wait for them.
	dest, err := pushDestination(*push, *registryFlag, *controller, m.Name, m.Version)
	if err != nil {
		return err
	}

	// PROGRESS GOES TO STDERR UNDER --json, and stdout carries one JSON document and nothing else. A
	// CI wrapper that had to strip build noise out of the channel it parses is a wrapper that breaks
	// the first time the build prints something new — which is the shape of the "source scrape" that
	// shared/conformance/README.md names as one of the two things this repo already paid for.
	progress := stdout
	if *asJSON {
		progress = stderr
	}

	b, err := buildBundle(*actorDir, progress)
	if err != nil {
		return err
	}
	art, err := pushBundleTo(context.Background(), dest, b, progress)
	if err != nil {
		return err
	}

	// PLACEABLE IS THE REPOSITORY *AND* THE TAG, because `resolveBundle` derives both — it asks for
	// `bundles/<actor>` at `<version>`, so `--push …/bundles/probe:nightly` is exactly as unfindable
	// as `--push …/acme/probe:0.1.0` and reporting the first as placeable would be the more expensive
	// of the two lies. The REGISTRY is deliberately not part of it: a Fleet's registry is configured
	// on the Fleet, and this process cannot know it.
	placeable := art.Repo == bundleRepo(b.Name) && art.Tag == b.Version

	if *asJSON {
		return json.NewEncoder(stdout).Encode(buildOutput{
			Actor: b.Name, Version: b.Version, Engine: b.Engine,
			Reference: art.ref(), Pinned: art.pinned(), Repository: art.Repo,
			Digest: art.Digest, SHA256: art.LayerSHA, Blob: dest.blobURL(art.LayerSHA),
			Placeable: placeable,
		})
	}

	fmt.Fprintf(stdout, "\nartifact: %s\n  digest %s\n  sha256 %s (the bytes a Machine verifies)\n  fetched from %s\n",
		art.ref(), art.Digest, art.LayerSHA, dest.blobURL(art.LayerSHA))
	// THE PINNED REFERENCE, because it is the one that means the same thing tomorrow, and because an
	// airgap is now a registry mirror with no kontra-specific step in it: this line is copy-pasteable
	// into `oras`, `skopeo` or `crane` and there is nothing else to sync for this Artifact. See
	// infra/README.md for what else a mirror needs.
	fmt.Fprintf(stdout, "\nmirror it:\n  oras copy %s <mirror>/%s\n", art.pinned(), art.Repo)

	if placeable {
		fmt.Fprintf(stdout, "\nplace it:\n  kontra fleet deploy --tag <t> --actor %s\n", *actorDir)
	} else {
		// See this file's header: the resolver derives the address, so a custom repository is
		// publishable and not placeable. Said here, once, at the moment the choice is made.
		fmt.Fprintf(stdout, "\nnote: a Fleet placement resolves %s:%s at its own registry, not %s.\n"+
			"  This Artifact is published and mirrorable; `kontra fleet deploy` will not find it until\n"+
			"  it is also copied there.\n",
			bundleRepo(b.Name), b.Version, art.ref())
	}
	return nil
}

// retiredTarget is the refusal for a flag that used to mean something, and it names where each of
// the two values went.
//
// A RETIRED FLAG IS NOT A TYPO AND MUST NOT READ AS ONE. `flag provided but not defined: -target`
// sends an operator to check their spelling; this sends them to the verb that does what they meant.
// The same reasoning `cli/fleet_documented_flags_test.go` was written under: a flag somebody was
// told to type has to either work or explain itself.
func retiredTarget(t string) error {
	switch t {
	case "container":
		return fmt.Errorf("--target is retired (ADR 0036: there is one Target, and it is a container).\n" +
			"  `--target container` built an Image through `kontra deploy`, which is still the verb:\n" +
			"    kontra deploy --actor <dir> [--registry <host:port>]")
	case "machine":
		return fmt.Errorf("--target is retired (ADR 0036: there is one Target, and it is a container).\n" +
			"  `--target machine` is what `kontra build` does now, and where it publishes is `--push`:\n" +
			"    kontra build --actor <dir> --push ghcr.io/<org>/bundles/<name>:<version>\n" +
			"  A Machine that runs the actor natively is the `process` DRIVER, not a Target — it runs\n" +
			"  from source and produces no Artifact, so nothing can be built for it.")
	default:
		return fmt.Errorf("--target is retired (ADR 0036: there is one Target, and it is a container), "+
			"and %q was never one of its values.\n"+
			"  `kontra build --push <ref>` publishes the Artifact; `kontra deploy` builds an Image", t)
	}
}
