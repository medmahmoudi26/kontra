// spa.go — the browser half of the control plane, as its own content-addressed artifact
// (ADR 0031 §2, issue 14).
//
// WHY IT IS NOT IN THE ORCHESTRATOR BUNDLE. That artifact is a Node runtime, tsc's output and a
// pnpm-resolved production tree — three things produced by one toolchain for one platform, and
// its manifest says so. The SPA is vite's output: no runtime, no native addon, identical bytes on
// every platform. Folding it in would make a 125 MB per-platform artifact carry 7 MB that is not
// per-platform, and it would tie "I changed a button" to a rebuild of the whole thing.
//
// SEPARATE ALSO MEANS SEPARATELY ANSWERABLE. `kontra up` prints both digests. "Which SPA is this
// control plane serving" is then a question with an exact answer, which is what makes a stale
// deploy visible — this repo has already had a `docker cp` deploy leave 22 versions of one chunk
// in a container and confirm whichever build you grepped for.
//
// The archive is written by the same deterministic tar writer, carries the same `manifest.json`
// in the same place, and is checked by the same `kontra bundle verify`. One shape, two subjects.
package bundle

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// SPAName is what `kontra bundle spa` writes and `kontra up` looks for. No version and no
// platform in the name, deliberately: the digest is the version, the bytes are platform-neutral,
// and a name that never changes is one an operator can put in a script.
const SPAName = "kontra-spa"

// SPARootInBundle is where the served files live inside the archive.
//
// `web/dist` rather than the archive root, so that `manifest.json` is not itself a static file
// this control plane serves. It is not a secret — every digest in it describes a public artifact
// — but a bundle that publishes its own manifest at `/manifest.json` is a surface nobody designed.
const SPARootInBundle = "web/dist"

// BuildSPA archives the built SPA with a manifest.
//
// IT DOES NOT RUN VITE. The build is `pnpm run build` in `frontend`, it needs that
// package's dev dependencies, and it is a 30-second job whose output an operator often already
// has — so this reads `web/dist` and refuses with the command to run when it is not there. The
// same rule `stageOrchestrator` follows for `pnpm install`: this file archives what a toolchain
// produced, it does not become the toolchain.
func BuildSPA(opts BuildOptions) (*BuildResult, error) {
	opts, err := opts.resolve()
	if err != nil {
		return nil, err
	}
	// Its own staging directory, never the orchestrator's: a build of one must not wipe a stage
	// the other is holding with --keep-stage.
	opts.StageDir = filepath.Join(opts.OutDir, ".stage-spa")
	p := progress(opts.Progress)

	src := filepath.Join(opts.RepoRoot, "frontend", "dist")
	if _, err := os.Stat(filepath.Join(src, "index.html")); err != nil {
		return nil, fmt.Errorf("no built SPA at %s (index.html is missing).\n"+
			"  build it first:  pnpm --dir frontend run build", src)
	}

	if err := os.RemoveAll(opts.StageDir); err != nil {
		return nil, fmt.Errorf("clear staging directory %s: %w", opts.StageDir, err)
	}
	if !opts.KeepStage {
		defer func() { _ = os.RemoveAll(opts.StageDir) }()
	}

	dst := filepath.Join(opts.StageDir, filepath.FromSlash(SPARootInBundle))
	p("staging the SPA from %s", src)
	if err := copyTree(src, dst); err != nil {
		return nil, err
	}

	tree, files, bytes, err := treeDigest(dst)
	if err != nil {
		return nil, err
	}
	version, err := packageVersion(filepath.Join(opts.RepoRoot, "frontend", "package.json"))
	if err != nil {
		// A missing version is not worth failing a build over; the digest is the identity.
		version = "unknown"
	}

	m := &Manifest{
		Schema: SchemaID,
		Bundle: "spa",
		// NEUTRAL ON PURPOSE, and it is checked on hydration. The orchestrator bundle refuses to
		// hydrate on a platform it was not built for because it carries native addons; this one
		// has none, so pinning it to the machine that ran vite would be a lie that costs a
		// rebuild on every other machine.
		Platform:  "any",
		Toolchain: map[string]string{},
		Components: []Component{{
			Name:       "orchestrator-spa",
			Kind:       "built",
			Version:    version,
			Path:       SPARootInBundle,
			TreeSHA256: tree,
			Files:      files,
			Bytes:      bytes,
			Note:       "vite output for frontend; served by the API role at its own port",
		}},
	}

	// THE SAME GATE THE ORCHESTRATOR BUNDLE PASSES THROUGH, and it matters more here rather than
	// less: a browser bundle is the one artifact this project has ever deliberately baked a
	// bearer token into (`VITE_KONTRA_EXPLORE_TOKEN`), and ADR 0031 §3 removes the reason for it
	// rather than the mechanism. Nothing below this line can add bytes to the tree.
	p("scanning the staged SPA for credentials and build paths")
	if err := scanStaged(opts.StageDir, opts.machinePaths(nil)); err != nil {
		return nil, err
	}

	summary, err := summarizeTree(opts.StageDir)
	if err != nil {
		return nil, err
	}
	m.Tree = summary

	manifestBytes, err := encodeManifest(m)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(opts.StageDir, ManifestName), manifestBytes, 0o644); err != nil {
		return nil, err
	}

	if err := os.MkdirAll(opts.OutDir, 0o755); err != nil {
		return nil, err
	}
	bundlePath := filepath.Join(opts.OutDir, SPAName+".tar.gz")
	p("writing %s", bundlePath)
	f, err := os.Create(bundlePath)
	if err != nil {
		return nil, err
	}
	digest, err := writeDeterministicTarGz(opts.StageDir, f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(bundlePath)
		return nil, err
	}
	st, err := os.Stat(bundlePath)
	if err != nil {
		return nil, err
	}

	manifestPath := filepath.Join(opts.OutDir, SPAName+".manifest.json")
	if err := os.WriteFile(manifestPath, manifestBytes, 0o644); err != nil {
		return nil, err
	}
	digestPath := bundlePath + ".sha256"
	if err := os.WriteFile(digestPath, []byte(digest+"  "+SPAName+".tar.gz\n"), 0o644); err != nil {
		return nil, err
	}

	return &BuildResult{
		BundlePath:   bundlePath,
		ManifestPath: manifestPath,
		DigestPath:   digestPath,
		SHA256:       digest,
		Bytes:        st.Size(),
		Manifest:     m,
		StageDir:     opts.StageDir,
	}, nil
}

// copyTree copies a directory of plain files. Symlinks are REFUSED rather than followed or
// recreated: vite emits none, and a link in an artifact is either a path off this machine or a
// second name for bytes the digest has already counted once.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o755)
		case d.Type().IsRegular():
			return copyFile(path, target)
		default:
			return fmt.Errorf("%s is %s; a bundle carries plain files", path, d.Type())
		}
	})
}
