// archive.go — the tar.gz half of an appliance bundle, written so that its own sha256 is
// an ANSWER rather than a coincidence.
//
// A bundle is content-addressed: the binary that carries it names it by the digest of these
// bytes, hydrate.Store verifies that digest before the bytes are stored, and the whole promise of
// "what is actually running" being answerable from a digest rests on the same inputs producing
// the same bytes. `tar` does NOT do that by default. Four things leak into an archive with no
// help from the files it contains:
//
//	mtime   — every file's modification time, which on a fresh pnpm install is now
//	uid/gid  — and uname/gname, which on this box are root and on CI are something else
//	mode     — a 022 umask and a 077 umask produce different archives from one tree
//	order    — readdir order is the filesystem's, not the alphabet's
//
// Each of them is normalised below. The result is a build whose digest is worth printing: two
// runs over the same staged tree produce byte-identical output, so a digest that CHANGED means an
// input changed, which is the only reading of a content address that is any use.
//
// WHAT IS STILL AN INPUT, and is not pretended otherwise: the Go toolchain. `compress/flate`'s
// output is deterministic for a given version and level, and is not promised to be stable across
// Go releases. So a bundle's digest is reproducible on one Go, not across all of them — which is
// exactly the guarantee the acceptance criterion asks for ("stable across two builds from the
// same inputs on the same platform") and it would be dishonest to claim the wider one.
package bundle

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// bundleEpoch is the timestamp every entry gets.
//
// Not `time.Now()` and not the file's own mtime: both make the archive a function of WHEN it was
// built. The reproducible-builds convention is a fixed epoch, and zero is the one value nobody
// has to agree on. It is a lie about the files, and it is a lie the digest needs — the alternative
// is an archive that differs from itself between two runs a second apart.
var bundleEpoch = time.Unix(0, 0).UTC()

// ManifestName is where the manifest lives inside the archive. A fixed, top-level name so that
// reading it costs one streamed pass and never a full extraction.
const ManifestName = "manifest.json"

// WriteReleaseArchive is {@link writeDeterministicTarGz} for a caller outside this package.
//
// `kontra release` packs a directory that is NOT a bundle — a Go binary, the two bundles and their
// manifests — and it must be packed by the same rules, because the whole release inherits the
// reproducibility claim from the archives inside it. A second writer in `cli/` with its own idea
// of timestamps and sort order is a second thing that has to stay deterministic, and it would drift
// the first time somebody fixed a bug in one of them.
func WriteReleaseArchive(root string, w io.Writer) (string, error) {
	return writeDeterministicTarGz(root, w)
}

// writeDeterministicTarGz archives root (its CONTENTS, not root itself) and returns the sha256 of
// the compressed bytes it wrote.
//
// The digest is taken on the way out rather than by re-reading the file: a bundle whose recorded
// digest came from a second read is a digest of whatever was on disk at the second read.
func writeDeterministicTarGz(root string, w io.Writer) (string, error) {
	sum := sha256.New()
	zw, err := gzip.NewWriterLevel(io.MultiWriter(w, sum), gzip.BestCompression)
	if err != nil {
		return "", err
	}
	// Name and ModTime deliberately left zero — gzip's own header carries a filename and a
	// timestamp, and both would put the build's identity into the compressed envelope where no
	// amount of care about the tar inside it would help.
	tw := tar.NewWriter(zw)

	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		return writeEntry(tw, filepath.ToSlash(rel), path, d)
	})
	if err != nil {
		return "", err
	}
	if err := tw.Close(); err != nil {
		return "", fmt.Errorf("close tar: %w", err)
	}
	if err := zw.Close(); err != nil {
		return "", fmt.Errorf("close gzip: %w", err)
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// writeEntry appends one normalised member.
//
// WALKING IS THE ORDERING. `filepath.WalkDir` reads each directory with `os.ReadDir`, which sorts
// by filename, so the traversal is a deterministic depth-first walk and no explicit sort is
// needed. That is a property of the standard library this file depends on, so it is written down
// here rather than left to be rediscovered by whoever sees an unstable digest.
func writeEntry(tw *tar.Writer, rel, path string, d fs.DirEntry) error {
	info, err := d.Info()
	if err != nil {
		return err
	}

	hdr := &tar.Header{
		Name: rel,
		// PAX for every entry, not "whichever format fits". Go picks the narrowest format a
		// header fits into, so a tree where one path crosses ustar's 100-character limit would be
		// archived in a mixture — deterministic, but for a reason nobody would guess. Forcing one
		// format makes the encoding a property of this file instead of of the deepest path in
		// node_modules. (Go emits an extended-header block only when a field actually overflows,
		// so short names cost nothing.)
		Format:  tar.FormatPAX,
		ModTime: bundleEpoch,
		// Uid/Gid 0 and the NAMES blank. Numeric ids alone are not enough: tar records uname and
		// gname as strings too, so an archive built as `root` and one built as `build` differ in
		// bytes that describe neither the files nor their permissions.
		Uid: 0, Gid: 0, Uname: "", Gname: "",
	}

	switch {
	case d.IsDir():
		hdr.Typeflag = tar.TypeDir
		hdr.Name = rel + "/"
		hdr.Mode = 0o755
	case info.Mode()&fs.ModeSymlink != 0:
		link, err := os.Readlink(path)
		if err != nil {
			return err
		}
		// AN ABSOLUTE SYMLINK IS A MACHINE-SPECIFIC PATH that no content scan would catch — it
		// lives in the tar header, not in any file's bytes. pnpm's `.bin/*` links are relative and
		// stay so; refuse anything else here rather than shipping a bundle with a dangling link to
		// the machine that built it.
		if filepath.IsAbs(link) {
			return fmt.Errorf("%s is an absolute symlink to %s: a bundle may not carry a path from the machine that built it", rel, link)
		}
		hdr.Typeflag = tar.TypeSymlink
		hdr.Linkname = filepath.ToSlash(link)
		hdr.Mode = 0o777
	case info.Mode().IsRegular():
		hdr.Typeflag = tar.TypeReg
		hdr.Size = info.Size()
		// TWO MODES, NOT THE FILE'S. The only bit that matters downstream is +x — `bin/node` and
		// the `.bin` shims must stay executable — and the rest is umask noise that would make the
		// digest depend on the shell the build ran under.
		if info.Mode()&0o111 != 0 {
			hdr.Mode = 0o755
		} else {
			hdr.Mode = 0o644
		}
	default:
		// Sockets, devices, fifos. Nothing in a Node install produces one, and an archive that
		// silently dropped one would be a bundle that is quietly not what was staged.
		return fmt.Errorf("%s is a %s, which a bundle may not contain", rel, info.Mode().Type())
	}

	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("write header %s: %w", rel, err)
	}
	if hdr.Typeflag != tar.TypeReg {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	n, err := io.Copy(tw, f)
	if err != nil {
		return fmt.Errorf("write %s: %w", rel, err)
	}
	if n != hdr.Size {
		// The file changed under the walk. Better a refusal than a corrupt archive whose digest
		// is perfectly stable and describes bytes nobody has.
		return fmt.Errorf("%s changed while it was being archived (%d bytes staged, %d written)", rel, hdr.Size, n)
	}
	return nil
}

// walkArchive streams a bundle, calling fn for each member with a reader valid only for that call.
// Streaming rather than extracting: the archive is several hundred megabytes and every question
// asked of it here — read the manifest, re-digest a member, re-derive a tree digest — is
// answerable in one pass over it.
//
// SYMLINKS ARE MEMBERS TOO, and skipping them was a real bug in the first draft of this: a tree
// digest counts a symlink as a line naming its target, so a verifier blind to them would report
// every tree in the bundle as changed.
func walkArchive(path string, fn func(hdr *tar.Header, r io.Reader) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("%s is not a gzip stream: %w", path, err)
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeSymlink {
			continue
		}
		if err := fn(hdr, tr); err != nil {
			return err
		}
	}
}

// ReadManifest pulls the manifest out of a built bundle without unpacking it.
func ReadManifest(bundlePath string) (*Manifest, error) {
	var m *Manifest
	errFound := errors.New("found")
	err := walkArchive(bundlePath, func(hdr *tar.Header, r io.Reader) error {
		if hdr.Name != ManifestName {
			return nil
		}
		var parsed Manifest
		if err := json.NewDecoder(r).Decode(&parsed); err != nil {
			return fmt.Errorf("%s in %s: %w", ManifestName, bundlePath, err)
		}
		m = &parsed
		return errFound // stop the walk; the manifest is the first member written
	})
	if err != nil && !errors.Is(err, errFound) {
		return nil, err
	}
	if m == nil {
		return nil, fmt.Errorf("%s carries no %s, so it is not a kontra bundle", bundlePath, ManifestName)
	}
	return m, nil
}

// Verify re-derives every digest the manifest claims, from the archive itself.
//
// THE POINT IS THAT THE MANIFEST IS CHECKABLE. A manifest whose digests were only ever written,
// never read back, is documentation — and documentation of a native addon's provenance is exactly
// the thing that goes stale silently. This re-reads the archive, re-hashes each recorded path and
// compares, so `which addon build, which digest` is a claim with a verifier behind it.
//
// It reports EVERY mismatch rather than the first: one wrong path usually means a staging change
// that moved several, and finding them one build at a time is the slow way.
func Verify(bundlePath string) (*Manifest, error) {
	m, err := ReadManifest(bundlePath)
	if err != nil {
		return nil, err
	}

	// TWO KINDS OF CLAIM, both re-derived. A single FILE is one digest to recompute; a DIRECTORY
	// is a treeDigest, which is a function of every member under a prefix — so both are gathered in
	// one pass and compared together, and neither is taken on trust because it was expensive.
	want := map[string]string{}  // archive path → expected sha256
	trees := map[string]string{} // archive prefix → expected treeDigest
	for _, c := range m.Components {
		switch {
		case c.Path != "" && c.SHA256 != "":
			want[c.Path] = c.SHA256
		case c.Path != "" && c.TreeSHA256 != "":
			trees[c.Path] = c.TreeSHA256
		}
	}
	if m.Tree.SHA256 != "" {
		// The whole bundle, minus the manifest — which did not exist when its digest was taken and
		// could not have included itself if it had.
		trees[""] = m.Tree.SHA256
	}
	for _, a := range m.NativeAddons {
		want[a.Path] = a.SHA256
	}

	// SORTED BY PATH, NOT BY LINE, and the difference is not cosmetic. `treeDigest` orders its
	// lines by the RELATIVE PATH; a line begins with the digest, so sorting the assembled strings
	// orders them by hash instead and every tree in a real bundle reports as changed. Caught by
	// running this against a 31,823-file bundle, which is the only size at which the two orderings
	// were ever going to differ visibly.
	type treeLine struct{ rel, text string }
	seen := map[string]bool{}
	lines := map[string][]treeLine{} // tree prefix → the treeDigest lines gathered for it
	var problems []string
	err = walkArchive(bundlePath, func(hdr *tar.Header, r io.Reader) error {
		name := hdr.Name
		line := ""
		if hdr.Typeflag == tar.TypeSymlink {
			line = "symlink:" + hdr.Linkname + "  "
		} else {
			expect, wanted := want[name]
			needDigest := wanted
			for prefix := range trees {
				if underPrefix(name, prefix) {
					needDigest = true
				}
			}
			if !needDigest {
				return nil
			}
			sum := sha256.New()
			if _, err := io.Copy(sum, r); err != nil {
				return err
			}
			got := hex.EncodeToString(sum.Sum(nil))
			if wanted {
				seen[name] = true
				if got != expect {
					problems = append(problems, fmt.Sprintf("  %s\n    manifest: %s\n    archive:  %s", name, expect, got))
				}
			}
			line = got + "  "
		}
		for prefix := range trees {
			if !underPrefix(name, prefix) || name == ManifestName {
				continue
			}
			rel := strings.TrimPrefix(strings.TrimPrefix(name, prefix), "/")
			lines[prefix] = append(lines[prefix], treeLine{rel: rel, text: line + rel + "\n"})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, prefix := range sortedKeys(trees) {
		gathered := lines[prefix]
		// A PREFIX THAT MATCHED NOTHING IS A WRONG PATH, NOT A CHANGED TREE, and saying so is the
		// difference between a two-minute diagnosis and an afternoon. An empty tree digests to
		// e3b0c442…b855 — sha256 of no bytes at all — so the comparison below DOES fail, but it
		// fails as "these two hashes differ", which reads like the dependency tree changed and
		// sends you looking at a 31,825-file archive for the file that moved. What actually
		// happened is that the manifest named a directory the bundle does not have: ADR 0035 moved
		// the repo's `orchestrator/` to `backend/`, one Component.Path came along with it, and the
		// bundle layout it described did not move at all. Four platform jobs each spent five
		// minutes building a correct bundle to report a hash.
		if len(gathered) == 0 && prefix != "" {
			problems = append(problems, fmt.Sprintf(
				"  %s\n    manifest names it as a directory; the archive has nothing under that path\n"+
					"    (a Component.Path is relative to the EXTRACTED BUNDLE, not to the repo)", prefix))
			continue
		}
		sort.Slice(gathered, func(i, j int) bool { return gathered[i].rel < gathered[j].rel })
		h := sha256.New()
		for _, l := range gathered {
			h.Write([]byte(l.text))
		}
		if got := hex.EncodeToString(h.Sum(nil)); got != trees[prefix] {
			label := prefix
			if label == "" {
				label = "(the whole bundle)"
			}
			problems = append(problems, fmt.Sprintf("  %s\n    manifest: %s (tree)\n    archive:  %s (tree)", label, trees[prefix], got))
		}
	}
	for _, name := range sortedKeys(want) {
		if !seen[name] {
			problems = append(problems, fmt.Sprintf("  %s\n    manifest names it; the archive does not contain it", name))
		}
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("%s does not match its own manifest:\n%s", bundlePath, strings.Join(problems, "\n"))
	}
	return m, nil
}

// underPrefix is directory containment on archive paths. The empty prefix is the whole bundle, and
// `orchestrator/dist` must not match `orchestrator/dist-old` — which is what a bare HasPrefix would
// do, silently folding a second directory into a tree digest that then never matches again.
func underPrefix(name, prefix string) bool {
	if prefix == "" {
		return true
	}
	return strings.HasPrefix(name, prefix+"/")
}

// sha256File digests a file on disk.
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}
