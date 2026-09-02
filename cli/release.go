package main

// `kontra release` — one file per platform, and the digests that make it installable (issue 17).
//
// WHAT "ONE FILE PER PLATFORM" IS FOR. The PRD's first sentence about installation is "the goal is
// one file: `curl`, or `go install` from GitHub, then `kontra up`". A user who has to fetch a
// binary, then work out which orchestrator bundle matches it, then put that bundle somewhere the
// binary looks, has not installed one file — they have performed an assembly, and every step of it
// is a step that can be done wrong on a machine nobody can see.
//
// So a release artifact is the whole appliance for one platform:
//
//	kontra                                              the Go binary, built for that platform
//	bundles/kontra-orchestrator-<node>-<os>-<arch>.tar.gz  the carried Node orchestrator
//	bundles/kontra-spa.tar.gz                           the browser half, the same on all four
//	manifests/*.manifest.json                           what is inside each, without unpacking
//
// The layout is not arbitrary: `bundles/` is exactly what `findBundles` looks for under
// `$KONTRA_HOME`, so installing is "unpack the binary onto PATH and the rest into $KONTRA_HOME".
// The installer script does that in one command and nothing else.
//
// SHA256SUMS IS THE OTHER HALF OF THE RELEASE, and it is the half that makes the install
// verifiable rather than merely convenient. `install.sh`'s buf step is the shape this repo has
// already learned not to copy — `/releases/latest/download/` with no version and no checksum, so
// nothing anywhere states what was supposed to arrive. A release that publishes its own digests
// lets the installer say `sha256sum -c` and mean it.
//
// CROSS-BUILDING THE GO BINARY IS SUPPORTED AND NOT RELIED ON. `cli/` has no cgo, so `GOOS=darwin
// GOARCH=arm64 go build` works from anywhere and the bundle for that platform cross-builds too
// (see appliance/bundle/toolchain.go). CI still runs this natively on four runners, because a
// cross-built artifact nobody has ever executed is a claim and a natively built one that ran its
// own smoke path is a fact.

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	applbundle "github.com/medmahmoudi26/kontra/cli/appliance/bundle"
)

func cmdRelease(args []string) error {
	fs := flag.NewFlagSet("release", flag.ContinueOnError)
	out := fs.String("out", "", "where to write the release files (default: <repo>/build/release)")
	repo := fs.String("repo", "", "the checkout to build from (default: found by walking up)")
	version := fs.String("version", "", "the release version (default: `git describe`)")
	platforms := fs.String("platform", "", "which platforms to release: a goos/goarch, a comma-separated list, or `all` (default: this host)")
	cache := fs.String("cache", "", "where verified upstream artifacts are kept (default: ~/.cache/kontra/appliance-artifacts)")
	skipSPA := fs.Bool("no-spa", false, "leave the browser bundle out (the API serves, the surfaces do not)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	root, err := findRepoRoot(*repo)
	if err != nil {
		return err
	}
	targets, err := parsePlatformList(*platforms)
	if err != nil {
		return err
	}
	tag, err := releaseVersion(root, *version)
	if err != nil {
		return err
	}
	outDir := *out
	if outDir == "" {
		outDir = filepath.Join(root, "build", "release")
	}
	if outDir, err = filepath.Abs(outDir); err != nil {
		return err
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}

	fmt.Fprintf(stdout, "releasing kontra %s for %s\n", tag, describePlatforms(targets))

	// THE SPA ONCE, BEFORE THE LOOP. It is a vite output — the same bytes on every platform — and
	// rebuilding it four times would produce four identical files and four chances for one of them
	// to be built from a different working tree.
	spa := ""
	if !*skipSPA {
		res, err := applbundle.BuildSPA(applbundle.BuildOptions{
			RepoRoot: root,
			OutDir:   filepath.Join(outDir, ".artifacts"),
			Progress: stdout,
		})
		if err != nil {
			return fmt.Errorf("the browser bundle: %w\n"+
				"  pass --no-spa to release an appliance that serves its API and says the surfaces are missing", err)
		}
		spa = res.BundlePath
	}

	var files []string
	for _, target := range targets {
		fmt.Fprintf(stdout, "\n=== %s ===\n", target)
		path, err := releaseOnePlatform(root, outDir, tag, target, *cache, spa)
		if err != nil {
			return fmt.Errorf("%s: %w", target, err)
		}
		files = append(files, path)
	}

	sums, err := writeSHA256SUMS(outDir, files)
	if err != nil {
		return err
	}

	// THE LOOSE BUNDLES GO, AND ONLY ONCE EVERYTHING SUCCEEDED. Each one is already inside the
	// release tarball that was just written, so keeping both doubles a gigabyte for four platforms
	// — and this repo has twice driven a box to 99% during this branch. On a FAILURE the directory
	// stays: a run that died on the fourth platform has three expensive bundles in it, and
	// deleting those to tidy up would be charging somebody twenty minutes for a cleanup they did
	// not ask for.
	if err := os.RemoveAll(filepath.Join(outDir, ".artifacts")); err != nil {
		return err
	}

	fmt.Fprintf(stdout, "\n%s\n", outDir)
	for _, f := range files {
		st, err := os.Stat(f)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "  %-44s %10s\n", filepath.Base(f), humanSize(st.Size()))
	}
	fmt.Fprintf(stdout, "  %-44s %10s\n", filepath.Base(sums), "digests")
	fmt.Fprintf(stdout, "\nverify a downloaded one:\n  sha256sum -c --ignore-missing SHA256SUMS\n")
	return nil
}

// releaseOnePlatform builds the binary and the bundle for one target and packs them with the SPA.
func releaseOnePlatform(root, outDir, tag string, target applbundle.Platform, cache, spa string) (string, error) {
	artifacts := filepath.Join(outDir, ".artifacts")
	stage := filepath.Join(outDir, ".stage-release-"+target.OS+"-"+target.Arch)
	if err := os.RemoveAll(stage); err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(stage) }()

	// 1. The binary. `-trimpath` because a Go binary otherwise records the directory it was built
	//    in, which makes the release's digest a function of somebody's home directory.
	exe := filepath.Join(stage, "kontra")
	if err := os.MkdirAll(stage, 0o755); err != nil {
		return "", err
	}
	fmt.Fprintf(stdout, "building the kontra binary for %s\n", target)
	build := exec.Command("go", "build", "-trimpath", "-ldflags", "-s -w", "-o", exe, ".")
	build.Dir = filepath.Join(root, "cli")
	build.Env = append(os.Environ(), "GOOS="+target.OS, "GOARCH="+target.Arch, "CGO_ENABLED=0")
	if combined, err := build.CombinedOutput(); err != nil {
		return "", fmt.Errorf("go build for %s failed: %w\n%s\n"+
			"  if this is a cross-build that cannot work here, run the release on a %s runner instead",
			target, err, indentBlock(string(combined)), target)
	}

	// 2. The orchestrator bundle for the same target.
	res, err := applbundle.BuildOrchestrator(applbundle.BuildOptions{
		RepoRoot: root,
		OutDir:   artifacts,
		CacheDir: cache,
		Platform: target,
		Progress: stdout,
	})
	if err != nil {
		return "", err
	}

	// 3. Everything where an installed binary already looks for it. `findBundles` searches
	//    `$KONTRA_HOME/bundles`, so the release lays the tree out that way and installing is a
	//    copy rather than an instruction.
	bundles := filepath.Join(stage, "bundles")
	manifests := filepath.Join(stage, "manifests")
	for _, d := range []string{bundles, manifests} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return "", err
		}
	}
	if err := copyInto(res.BundlePath, bundles); err != nil {
		return "", err
	}
	if err := copyInto(res.ManifestPath, manifests); err != nil {
		return "", err
	}
	if spa != "" {
		if err := copyInto(spa, bundles); err != nil {
			return "", err
		}
		// fileExists (dispatch.go), not a looser "anything is there": this is a sidecar the next
		// line COPIES, and a directory wearing that name would pass a permissive check and fail
		// two statements later with "is a directory".
		if m := strings.TrimSuffix(spa, ".tar.gz") + ".manifest.json"; fileExists(m) {
			if err := copyInto(m, manifests); err != nil {
				return "", err
			}
		}
	}

	name := fmt.Sprintf("kontra_%s_%s_%s.tar.gz", tag, target.OS, target.Arch)
	path := filepath.Join(outDir, name)
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	digest, err := applbundle.WriteReleaseArchive(stage, f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(path)
		return "", err
	}
	fmt.Fprintf(stdout, "%s\n  sha256 %s\n", name, digest)
	return path, nil
}

// writeSHA256SUMS writes the digests in the format `sha256sum -c` reads, sorted by filename so two
// releases of the same files produce the same file.
func writeSHA256SUMS(outDir string, files []string) (string, error) {
	names := make([]string, 0, len(files))
	for _, f := range files {
		names = append(names, filepath.Base(f))
	}
	sort.Strings(names)

	var b strings.Builder
	for _, name := range names {
		sum, err := sha256Path(filepath.Join(outDir, name))
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "%s  %s\n", sum, name)
	}
	path := filepath.Join(outDir, "SHA256SUMS")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// parsePlatformList reads what `--platform` accepts: nothing (the host), `all`, or a list.
//
// `all` READS ITS FOUR OUT OF THE PIN TABLE rather than spelling them here. A second list of
// platforms is a second place to forget one, and the failure of forgetting is silent: a release
// that ships three files and says nothing about the fourth.
func parsePlatformList(spec string) ([]applbundle.Platform, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return []applbundle.Platform{applbundle.HostPlatform()}, nil
	}
	if spec == "all" {
		return applbundle.Platforms(), nil
	}
	var out []applbundle.Platform
	seen := map[string]bool{}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		p, err := applbundle.ParsePlatform(part)
		if err != nil {
			return nil, err
		}
		if seen[p.String()] {
			continue
		}
		seen[p.String()] = true
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil, errors.New("--platform was given nothing to build")
	}
	return out, nil
}

func describePlatforms(in []applbundle.Platform) string {
	out := make([]string, 0, len(in))
	for _, p := range in {
		out = append(out, p.String())
	}
	return strings.Join(out, ", ")
}

// releaseVersion is what the files are named after.
//
// `git describe` BY DEFAULT AND NOT A CONSTANT IN A FILE, because a version constant is a thing
// somebody forgets to bump and then two different releases wear one name. `--version` overrides it
// for a build from an export with no `.git` — and the fallback says what it did rather than
// quietly naming a release `unknown`.
func releaseVersion(root, override string) (string, error) {
	if override != "" {
		return strings.TrimPrefix(strings.TrimSpace(override), "v"), nil
	}
	cmd := exec.Command("git", "describe", "--tags", "--always", "--dirty")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("no --version was given and `git describe` failed in %s: %w\n"+
			"  pass --version <x.y.z>; a release named after nothing is a release nobody can report a bug against", root, err)
	}
	v := strings.TrimPrefix(strings.TrimSpace(string(out)), "v")
	if v == "" {
		return "", errors.New("`git describe` returned nothing; pass --version")
	}
	return v, nil
}

func copyInto(src, dir string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(filepath.Join(dir, filepath.Base(src)), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, st.Mode().Perm())
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

func indentBlock(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = "    " + l
	}
	return strings.Join(lines, "\n")
}
