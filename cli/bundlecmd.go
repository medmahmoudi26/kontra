package main

// `kontra bundle` — build and check the artifacts the appliance binary carries (ADR 0031 §2).
//
// NOT THE SAME NOUN AS `cli/bundle.go`, and the overlap is worth naming rather than renaming
// around. That file builds an ACTOR Bundle: the actor, the SDK + runtime seams and the handler, fetched by a
// fleet Machine from the Controller's object store. This one builds an APPLIANCE bundle: a Node
// runtime, the compiled orchestrator and its native addons, hydrated by the kontra binary itself.
// They share the idea that makes both work — a tarball whose identity is the sha256 of its own
// bytes, verified on arrival — and they carry different things for different readers. One verb
// with a subject (`kontra bundle orchestrator`) says which, where a second verb would leave two
// commands nobody can tell apart.
//
// BUILDING IS SEPARATE FROM PLACING, exactly as `kontra build` is separate from
// `kontra fleet deploy`. This command writes a bundle and its manifest to a directory and stops.
// Hydrating one into a working orchestrator is issue 14's, and publishing one to a store is the
// step after that; keeping them apart is what lets a bundle be built, inspected and diffed before
// anything runs it.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	applbundle "github.com/medmahmoudi26/kontra-local/cli/appliance/bundle"
)

func cmdBundle(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: kontra bundle orchestrator|spa [--out <dir>] | kontra bundle verify <bundle.tar.gz>")
	}
	switch args[0] {
	case "orchestrator":
		return cmdBundleOrchestrator(args[1:])
	case "spa":
		return cmdBundleSPA(args[1:])
	case "verify":
		return cmdBundleVerify(args[1:])
	default:
		return fmt.Errorf("kontra bundle: no such subject %q (orchestrator, spa, verify)", args[0])
	}
}

// cmdBundleOrchestrator builds one bundle per platform asked for.
//
// ONE COMMAND, N PLATFORMS, AND THEY ARE BUILT ONE AFTER ANOTHER RATHER THAN AT ONCE. A staged
// tree is ~450 MB before it is compressed, so four in parallel is 1.8 GB of working space plus
// four archives — on a box that has twice hit 99% during this branch, that is not a speed-up, it
// is a different failure. Sequential also means a failure on the third platform leaves the first
// two on disk and usable, which is what you want when the third is the one whose prebuild moved.
func cmdBundleOrchestrator(args []string) error {
	fs := flag.NewFlagSet("bundle orchestrator", flag.ContinueOnError)
	out := fs.String("out", "", "where to write the bundle (default: <repo>/build/bundles)")
	repo := fs.String("repo", "", "the checkout to build from (default: found by walking up)")
	platform := fs.String("platform", "", "which platforms to build: a goos/goarch, a comma-separated list, or `all` (default: this host)")
	cache := fs.String("cache", "", "where verified upstream artifacts are kept (default: ~/.cache/kontra/appliance-artifacts)")
	keepStage := fs.Bool("keep-stage", false, "leave the staged tree on disk, to look at what went in")
	if err := fs.Parse(args); err != nil {
		return err
	}

	root, err := findRepoRoot(*repo)
	if err != nil {
		return err
	}
	targets, err := parsePlatformList(*platform)
	if err != nil {
		return err
	}

	fmt.Fprintf(stdout, "building the orchestrator bundle from %s for %s\n", root, describePlatforms(targets))
	results := make([]*applbundle.BuildResult, 0, len(targets))
	for _, target := range targets {
		if len(targets) > 1 {
			fmt.Fprintf(stdout, "\n=== %s ===\n", target)
		}
		res, err := applbundle.BuildOrchestrator(applbundle.BuildOptions{
			RepoRoot:  root,
			OutDir:    *out,
			CacheDir:  *cache,
			Platform:  target,
			Progress:  stdout,
			KeepStage: *keepStage,
		})
		if err != nil {
			return fmt.Errorf("%s: %w", target, err)
		}
		printBundle(res)
		results = append(results, res)
	}

	// THE LIST AT THE END, when there was more than one. Four bundles scrolled past as four blocks
	// of progress is four digests somebody has to scroll back for, and the digests are the whole
	// output of this command.
	if len(results) > 1 {
		fmt.Fprintf(stdout, "\n%d bundles\n", len(results))
		for _, res := range results {
			fmt.Fprintf(stdout, "  %-14s %s  %s\n", res.Manifest.Platform, res.SHA256, filepath.Base(res.BundlePath))
		}
	}
	return nil
}

func printBundle(res *applbundle.BuildResult) {
	m := res.Manifest
	fmt.Fprintf(stdout, "\n%s\n", res.BundlePath)
	fmt.Fprintf(stdout, "  sha256    %s\n", res.SHA256)
	fmt.Fprintf(stdout, "  size      %s over %d files\n", humanSize(res.Bytes), m.Tree.Files)
	fmt.Fprintf(stdout, "  platform  %s\n", m.Platform)
	fmt.Fprintf(stdout, "  manifest  %s\n", res.ManifestPath)
	fmt.Fprintf(stdout, "\ncomponents\n")
	for _, c := range m.Components {
		fmt.Fprintf(stdout, "  %-26s %-22s %s\n", c.Name, c.Version, c.Path)
	}
	// THE ADDONS GET THEIR OWN BLOCK because they are what makes this artifact per-platform, and
	// "which addon build, which digest" is the question a bundle is supposed to answer without
	// anybody unpacking it.
	fmt.Fprintf(stdout, "\nnative addons (%s)\n", m.Platform)
	for _, a := range m.NativeAddons {
		build := a.Build
		if build == "" {
			build = "-"
		}
		fmt.Fprintf(stdout, "  %-38s %-20s %10s  %s\n", a.Package, build, humanSize(a.Bytes), a.SHA256[:16])
		if len(a.ExcludedBuilds) > 0 {
			fmt.Fprintf(stdout, "  %-38s dropped: %v\n", "", a.ExcludedBuilds)
		}
	}
	fmt.Fprintf(stdout, "\nrun it:\n  tar -xzf %s -C <dir> && cd <dir> && %s %s\n",
		filepath.Base(res.BundlePath), m.Entrypoint[0], m.Entrypoint[1])
}

// cmdBundleSPA builds the browser half. Its own subject rather than a flag on `orchestrator`,
// because the two are built from different toolchains at different moments: the SPA changes when
// a page does, and rebuilding 450 MB of Node to ship a 7 MB vite output is the coupling this
// separation exists to avoid (see appliance/bundle/spa.go).
func cmdBundleSPA(args []string) error {
	fs := flag.NewFlagSet("bundle spa", flag.ContinueOnError)
	out := fs.String("out", "", "where to write the bundle (default: <repo>/build/bundles)")
	repo := fs.String("repo", "", "the checkout to build from (default: found by walking up)")
	keepStage := fs.Bool("keep-stage", false, "leave the staged tree on disk, to look at what went in")
	if err := fs.Parse(args); err != nil {
		return err
	}

	root, err := findRepoRoot(*repo)
	if err != nil {
		return err
	}
	res, err := applbundle.BuildSPA(applbundle.BuildOptions{
		RepoRoot:  root,
		OutDir:    *out,
		Progress:  stdout,
		KeepStage: *keepStage,
	})
	if err != nil {
		return err
	}
	c := res.Manifest.Components[0]
	fmt.Fprintf(stdout, "\n%s\n", res.BundlePath)
	fmt.Fprintf(stdout, "  sha256    %s\n", res.SHA256)
	fmt.Fprintf(stdout, "  size      %s over %d files\n", humanSize(res.Bytes), c.Files)
	fmt.Fprintf(stdout, "  version   %s\n", c.Version)
	fmt.Fprintf(stdout, "  manifest  %s\n", res.ManifestPath)
	fmt.Fprintf(stdout, "\n`kontra up` hydrates this beside the orchestrator bundle and serves it.\n")
	return nil
}

func cmdBundleVerify(args []string) error {
	fs := flag.NewFlagSet("bundle verify", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: kontra bundle verify <bundle.tar.gz>")
	}
	path := fs.Arg(0)

	// THE ARCHIVE'S OWN DIGEST FIRST. Every other check is against what the manifest says about
	// the bundle's parts, and all of them are worthless if the container has been swapped: a
	// manifest travels inside the thing it describes.
	sum, err := sha256Path(path)
	if err != nil {
		return err
	}
	m, err := applbundle.Verify(path)
	if err != nil {
		return err
	}
	st, err := os.Stat(path)
	if err != nil {
		return err
	}

	fmt.Fprintf(stdout, "%s\n", path)
	fmt.Fprintf(stdout, "  sha256    %s\n", sum)
	fmt.Fprintf(stdout, "  size      %s\n", humanSize(st.Size()))
	fmt.Fprintf(stdout, "  schema    %s\n", m.Schema)
	fmt.Fprintf(stdout, "  platform  %s\n", m.Platform)
	for _, c := range m.Components {
		digest := c.SHA256
		if digest == "" {
			digest = c.TreeSHA256 + " (tree)"
		}
		fmt.Fprintf(stdout, "\n  %s %s\n    %s\n    %s\n", c.Name, c.Version, c.Path, digest)
		if c.Upstream != nil {
			fmt.Fprintf(stdout, "    from %s\n    pinned sha256 %s\n", c.Upstream.URL, c.Upstream.Digest)
		}
	}
	fmt.Fprintf(stdout, "\n  %d native addons, every recorded digest re-derived from the archive and matching\n", len(m.NativeAddons))
	return nil
}

// sha256Path digests a file by streaming it. A bundle is hundreds of megabytes; reading one into
// memory to hash it is the kind of thing that works until the box it runs on is the 4 GB
// controller.
func sha256Path(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
