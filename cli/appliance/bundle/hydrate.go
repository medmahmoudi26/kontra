// hydrate.go — turning the bundle into something the binary can exec (ADR 0031 §2,
// issue 14).
//
// Issue 13 built the artifact and stopped there, deliberately: "building is separate from
// placing". This is the placing. A bundle is a tar.gz whose identity is the sha256 of its own
// bytes; hydrating it means putting those bytes in the CAS, expanding them ONCE into a golden
// tree, and cloning that tree copy-on-write into a working directory the orchestrator runs out of.
//
// EVERY PATH HERE IS NAMED BY A DIGEST, AND THAT IS THE WHOLE CACHE DESIGN. The working directory
// is `<data>/orchestrator/<digest>`, so "which bundle is this" is answered by a path and never by
// a comparison of timestamps or version strings. Two consequences worth stating:
//
//   - The second `kontra up` does no HYDRATING. It never opens the archive, never hashes 125 MB
//     and never touches the store; since issue 15 it does pay one lstat per file of the bundle's
//     own inventory, because "the directory is there" turned out not to be a check — see
//     hydratestore.Store.Check, and the measurement in the hydrate package.
//   - A DIFFERENT bundle is a DIFFERENT directory. Rebuilding the orchestrator and starting the
//     appliance again cannot silently run the previous build, which is the failure `kontra infra
//     up` has — it reverts a container to its image and says nothing — and the one this slice was
//     told not to reproduce.
//
// THE SPA IS A SEPARATE ARTIFACT, and it is hydrated separately. The orchestrator bundle's own
// manifest says so: it carries compiled JavaScript, a Node runtime and node_modules, and the
// browser bundle is neither compiled by tsc nor resolved by pnpm. It gets its own digest, its own
// golden tree and its own working directory, and the orchestrator finds it through a symlink at
// the path `server.ts:defaultWebRoot` walks up to. A symlink rather than a copy INTO the working
// directory, because a copy would make "which SPA is being served" a question no digest could
// answer: rebuilding only the SPA would leave the previous one in place and every surface would
// look correct.
package bundle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/medmahmoudi26/kontra/runtime/handler/casstore"
	"github.com/medmahmoudi26/kontra/runtime/handler/hydratestore"
)

// HydrateOptions is one hydration of the control plane's artifacts.
type HydrateOptions struct {
	// DataDir is the appliance's data directory — the CAS root, shared with the embedded
	// registry (ADR 0031 §2: do not build a second store).
	DataDir string

	// Bundle is the orchestrator bundle, a `.tar.gz` written by `kontra bundle orchestrator`.
	Bundle string

	// SPA is the browser bundle, a `.tar.gz` written by `kontra bundle spa`. Optional: without
	// one the API still serves, and the caller is told that the surfaces will not.
	SPA string

	// Store is an already-open artifact store. When nil, one is opened on DataDir. `kontra up`
	// passes its own so the registry and the hydrator address one set of bytes.
	Store *hydratestore.Store

	// Progress is where a first run reports itself. nil is silent, which is right for a test and
	// wrong for an operator waiting thirty seconds.
	Progress io.Writer
}

// HydratedOrchestrator is a working directory that can be exec'd.
type HydratedOrchestrator struct {
	// Root is the working directory: the bundle's contents, laid out as they were staged.
	Root   string
	Digest string

	// Node and Entry are the two halves of the manifest's entrypoint, made absolute. Both are
	// checked to exist before this struct is returned — a manifest naming a file the archive
	// does not contain has to fail here, not later as `Cannot find module`.
	Node  string
	Entry string

	// Manifest is what the bundle says about itself. Read from the working directory — which has
	// been verified against the bundle's inventory by the time it is read — so it costs a small
	// file read rather than a pass over the archive, and does not need the archive to exist.
	Manifest *Manifest

	// SPARoot is the browser bundle that will be served, or "" when there is none.
	SPARoot   string
	SPADigest string

	// Fresh is true when THIS call did the hydrating. It is what the caller prints, and what a
	// test asserts to prove the second run skipped the work.
	Fresh bool

	// Repaired is true when a working directory was found and thrown away, and Damage says what
	// was wrong with it. It is surfaced rather than swallowed because a repair is a fact about
	// the machine: an appliance that silently re-hydrates 450 MB on every start is a disk that
	// is quietly failing, and the only way anyone finds out is if it says so.
	Repaired bool
	Damage   error

	Method  hydratestore.Method
	Files   int
	Bytes   int64
	Elapsed time.Duration
}

// hydrationHeadroom is what a hydration wants free ON TOP of the tree it is about to write.
//
// The expanded size is known exactly — the manifest carries it — so this is not a guess about
// the artifact; it is room for the archive already in the store, for the temporary directory the
// expansion builds in, and for whatever else the box is doing. A full disk is not a loud failure
// on this system (a storage backend answered a bare 500 on every write and no counter noticed),
// so the number is checked before the work starts rather than discovered halfway through it.
const hydrationHeadroom = 512 << 20

// HydrateOrchestrator makes the bundle runnable and returns where.
func HydrateOrchestrator(ctx context.Context, opts HydrateOptions) (*HydratedOrchestrator, error) {
	started := time.Now()
	if opts.DataDir == "" {
		return nil, errors.New("appliance: hydration needs a data directory")
	}
	if opts.Bundle == "" {
		return nil, errors.New("appliance: hydration needs an orchestrator bundle")
	}
	bundle, err := filepath.Abs(opts.Bundle)
	if err != nil {
		return nil, err
	}

	digest, err := bundleDigest(bundle)
	if err != nil {
		return nil, err
	}

	store, err := hydrationStore(opts)
	if err != nil {
		return nil, err
	}
	root := filepath.Join(opts.DataDir, "orchestrator", digest)
	out := &HydratedOrchestrator{Root: root, Digest: digest}

	// THE FAST PATH, AND IT IS THE COMMON ONE. Everything below this branch runs once per bundle,
	// ever; this side of it is one lstat per file of the bundle's own inventory and a small read.
	//
	// IT USED TO BE A SINGLE `stat manifest.json`, AND THAT IS THE BUG ISSUE 15 EXISTS FOR. A
	// working directory that died between the first and the last file of a copy-on-write clone
	// still has a manifest.json — it is the first thing the archive stages — so the check passed
	// on exactly the artifact it most needed to refuse, and the failure arrived thirty seconds
	// later as a Node stack trace about a module path. "Does it exist" is not a check; "is every
	// one of its 31,823 parts there, at the right length" is.
	if err := store.Check(bundleArtifact(bundle, digest), root, hydratestore.Structure); err != nil {
		res, err := hydrateBundle(ctx, opts, store, bundle, digest, root)
		if err != nil {
			return nil, err
		}
		out.Fresh = !res.Existing || res.Repaired
		out.Method, out.Files, out.Bytes = res.Method, res.Files, res.Bytes
		out.Repaired, out.Damage = res.Repaired, res.Damage
	}

	// THE MANIFEST IS READ FROM THE WORKING DIRECTORY, ALWAYS, and never from the archive. It is
	// a member of the tree, the tree has just been verified against the bundle's own inventory,
	// and a manifest read from the archive instead would be a second source of truth about a
	// directory that is right here. It also means the archive is not needed on this path at all,
	// which is what keeps `kontra up` working on a machine that kept its data directory and threw
	// the 125 MB tarball away.
	m, err := readManifestFile(filepath.Join(root, ManifestName))
	if err != nil {
		return nil, fmt.Errorf("the hydrated orchestrator at %s has no %s: %w", root, ManifestName, err)
	}
	out.Manifest = m

	if err := out.resolveEntrypoint(); err != nil {
		return nil, err
	}
	if err := hydrateSPA(ctx, opts, out); err != nil {
		return nil, err
	}
	out.Elapsed = time.Since(started)
	return out, nil
}

// bundleArtifact is the pin for a bundle this machine already has: its own bytes, addressed by
// their sha256. One place, because the fast path's verification and the hydration itself have to
// be talking about the same artifact or the check checks nothing.
func bundleArtifact(bundle, digest string) hydratestore.Artifact {
	url, err := hydratestore.FileURL(bundle)
	if err != nil {
		// FileURL fails only when the working directory cannot be resolved, and the caller has
		// already made the path absolute. An unusable URL still verifies correctly — a check
		// never opens it — and hydration will report it.
		url = "file://" + bundle
	}
	return hydratestore.Artifact{
		Name:   "orchestrator-bundle",
		URL:    url,
		Digest: digest,
		Kind:   hydratestore.KindTarGz,
	}
}

// hydrateBundle is the run that does work: check what the archive claims, check the disk, then
// hydrate. It is reached on a first run, and on any run where the working directory did not
// verify.
func hydrateBundle(ctx context.Context, opts HydrateOptions, store *hydratestore.Store, bundle, digest, root string) (hydratestore.Result, error) {
	var res hydratestore.Result

	// THE MANIFEST BEFORE THE BYTES. It is the first member of the archive, so reading it costs
	// one streamed pass over a few kilobytes — and it answers the two questions that are cheap
	// now and expensive in thirty seconds' time: is this one of ours, and is it for this machine.
	//
	// WHEN THERE IS AN ARCHIVE TO ASK. A machine can reach here holding a data directory and a
	// digest sidecar and no tarball at all — the ordinary shape of the first start after
	// verification shipped, on an appliance whose 125 MB archive was cleaned up months ago — and
	// there is nothing for these checks to read and nothing for them to protect: the store
	// already holds the bytes, so EnsureHydrated will vouch for what is there or say why it
	// cannot. A refusal here would be this binary declining to start over a file it does not
	// need.
	m, err := readBundleClaims(bundle)
	if err != nil {
		return res, err
	}

	report := NewHydrationReport(opts.Progress)
	store.SetProgress(report.Observe)
	defer store.SetProgress(nil)

	if m != nil {
		if m.Tree.Bytes > 0 {
			if err := requireFreeSpaceFor(opts.DataDir, m.Tree.Bytes+hydrationHeadroom, "hydration"); err != nil {
				return res, err
			}
		}
		report.Say("hydrating the orchestrator from %s", filepath.Base(bundle))
		// "Once per bundle" rather than "first run only": this path is also where a working
		// directory that was found damaged gets rebuilt, and a line promising an operator that
		// this happens once would be a lie on exactly the run they most need to read.
		report.Say("  %s to store, %s over %d files to expand — once per bundle", humanBytes(fileSize(bundle)), humanBytes(m.Tree.Bytes), m.Tree.Files)
	}

	// ENSUREHYDRATED, NOT HYDRATE, AND THE DIFFERENCE IS THE WHOLE OF ISSUE 15. Hydrate is
	// store-if-absent: it leaves an existing working directory alone, whatever is in it. This
	// verifies what is there, adopts it if it is whole, discards and rebuilds it if it is not,
	// and hydrates once more before giving up — so the only way out of this call other than an
	// error is a working directory that was checked against the bundle's own inventory.
	//
	// SHARED, WHICH IS A PROMISE THIS CALLER CAN KEEP. The bundle is read and exec'd and never
	// edited: the orchestrator's own writes go to the data directory, not to its installation.
	// That promise is what lets a filesystem without reflink hardlink 31,823 files instead of
	// copying 450 MB of them.
	res, err = store.EnsureHydrated(ctx, bundleArtifact(bundle, digest), root, hydratestore.Shared)
	if err != nil {
		return res, err
	}
	report.Done(res)
	return res, nil
}

// readBundleClaims reads what the archive says about itself and refuses the two shapes that
// hydrate perfectly and then fail somewhere else. A nil manifest with a nil error means there is
// no archive on this machine to ask, which is not the same as an archive that will not answer.
func readBundleClaims(bundle string) (*Manifest, error) {
	if _, err := os.Stat(bundle); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	m, err := ReadManifest(bundle)
	if err != nil {
		return nil, err
	}
	if m.Schema != SchemaID {
		return nil, fmt.Errorf("%s says its schema is %q and this binary hydrates %q", bundle, m.Schema, SchemaID)
	}
	here := Platform{OS: runtime.GOOS, Arch: runtime.GOARCH}
	if m.Platform != here.String() {
		// A NATIVE ADDON IS WHY THIS IS A REFUSAL AND NOT A WARNING. The bundle carries a Rust
		// `.node`, a libduckdb and an swc binary built for one platform; the wrong one hydrates
		// perfectly and then dies at its first `require` with a message about an ELF header.
		return nil, fmt.Errorf("%s was built for %s and this machine is %s.\n"+
			"  Build one here with `kontra bundle orchestrator`", bundle, m.Platform, here)
	}
	return m, nil
}

// hydrateSPA places the browser bundle and points the orchestrator at it.
func hydrateSPA(ctx context.Context, opts HydrateOptions, out *HydratedOrchestrator) error {
	// `server.ts:defaultWebRoot` walks up from `dist/src` looking for a `web/dist/index.html`.
	// This is the first candidate on that walk that is not inside the compiled output.
	link := filepath.Join(out.Root, "orchestrator", "web", "dist")

	// A BUNDLE THAT ALREADY CARRIES ITS SPA WINS, and this is why the check is by TYPE and not by
	// existence: a later bundle schema may stage `frontend/dist` itself, and replacing a
	// real directory from the archive with a symlink of ours would serve something other than
	// what that bundle's digest describes.
	if st, err := os.Lstat(link); err == nil && st.IsDir() {
		out.SPARoot = link
		return nil
	}

	if opts.SPA == "" {
		return nil
	}
	spa, err := filepath.Abs(opts.SPA)
	if err != nil {
		return err
	}
	digest, err := bundleDigest(spa)
	if err != nil {
		return err
	}
	dest := filepath.Join(opts.DataDir, "spa", digest)
	served := filepath.Join(dest, filepath.FromSlash(SPARootInBundle))

	store, err := hydrationStore(opts)
	if err != nil {
		return err
	}
	art := hydratestore.Artifact{Name: "spa", URL: "", Digest: digest, Kind: hydratestore.KindTarGz}
	if art.URL, err = hydratestore.FileURL(spa); err != nil {
		return err
	}

	// THE SAME CHECK THE ORCHESTRATOR GETS, for the same reason. `stat index.html` was the old
	// test, and a browser bundle whose index.html arrived before the process died is exactly the
	// artifact it would have passed: the appliance would serve an index that then 404s on the
	// JavaScript it names, which is the failure mode the surfaces-do-not-load bug already cost
	// this repo once.
	if err := store.Check(art, dest, hydratestore.Structure); err != nil {
		// WHICH SUBJECT IS THIS. Two flags name two archives of the same shape, and handing the
		// orchestrator bundle to --spa would otherwise hydrate 450 MB of Node into a static file
		// root and serve it. One streamed read of the first member answers it.
		m, err := ReadManifest(spa)
		if err != nil {
			return err
		}
		if m.Schema != SchemaID || m.Bundle != "spa" {
			return fmt.Errorf("%s is a %q bundle (schema %q); --spa wants one built by `kontra bundle spa`", spa, m.Bundle, m.Schema)
		}
		if m.Tree.Bytes > 0 {
			if err := requireFreeSpaceFor(opts.DataDir, m.Tree.Bytes+hydrationHeadroom, "hydration"); err != nil {
				return err
			}
		}

		report := NewHydrationReport(opts.Progress)
		store.SetProgress(report.Observe)
		defer store.SetProgress(nil)
		report.Say("hydrating the SPA from %s (%s)", filepath.Base(spa), humanBytes(fileSize(spa)))

		res, err := store.EnsureHydrated(ctx, art, dest, hydratestore.Shared)
		if err != nil {
			return err
		}
		report.Done(res)
		out.Fresh = out.Fresh || !res.Existing || res.Repaired
		if _, err := os.Stat(filepath.Join(served, "index.html")); err != nil {
			return fmt.Errorf("%s hydrated without %s/index.html, so there is nothing to serve: %w", spa, SPARootInBundle, err)
		}
	}

	if err := pointAt(link, served); err != nil {
		return err
	}
	out.SPARoot, out.SPADigest = served, digest
	return nil
}

// pointAt publishes a symlink at link naming dest, replacing whatever was there.
//
// SYMLINK-THEN-RENAME, because the alternative is a window in which the path does not exist. If
// the orchestrator is restarted during that window it serves no SPA and reports nothing wrong —
// a control plane whose five surfaces 404 for one boot in a hundred is worse than one that never
// serves them, because nobody believes the bug report.
func pointAt(link, dest string) error {
	if current, err := os.Readlink(link); err == nil && current == dest {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		return err
	}
	tmp := link + ".pointing"
	_ = os.Remove(tmp)
	if err := os.Symlink(dest, tmp); err != nil {
		return fmt.Errorf("point %s at %s: %w", link, dest, err)
	}
	if err := os.Rename(tmp, link); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("publish %s: %w", link, err)
	}
	return nil
}

// resolveEntrypoint turns the manifest's entrypoint into two absolute paths, and refuses if
// either is not there.
//
// THE MANIFEST IS A CLAIM AND THIS IS THE CHEAPEST CHECK OF IT. Verifying every digest in a
// hydrated tree is issue 15's job and costs a pass over 450 MB; checking that the two files the
// binary is about to exec actually exist costs two stats, and it is the difference between "the
// bundle is broken" and a Node stack trace about a module path.
func (h *HydratedOrchestrator) resolveEntrypoint() error {
	m := h.Manifest
	if m == nil || len(m.Entrypoint) < 2 {
		return fmt.Errorf("the bundle at %s names no entrypoint (manifest.entrypoint is %v)", h.Root, m.entrypointOrNil())
	}
	h.Node = filepath.Join(h.Root, filepath.FromSlash(m.Entrypoint[0]))
	h.Entry = filepath.Join(h.Root, filepath.FromSlash(m.Entrypoint[1]))
	for _, p := range []string{h.Node, h.Entry} {
		if _, err := os.Stat(p); err != nil {
			return fmt.Errorf("the hydrated bundle at %s is missing %s, which its manifest names as part of the entrypoint: %w",
				h.Root, strings.TrimPrefix(p, h.Root+string(os.PathSeparator)), err)
		}
	}
	return nil
}

func (m *Manifest) entrypointOrNil() []string {
	if m == nil {
		return nil
	}
	return m.Entrypoint
}

// hydrationStore opens the artifact store, or uses the one the caller already has.
func hydrationStore(opts HydrateOptions) (*hydratestore.Store, error) {
	if opts.Store != nil {
		return opts.Store, nil
	}
	return hydratestore.Open(opts.DataDir)
}

// bundleDigest is the artifact's address: the sha256 of its own bytes.
//
// THE SIDECAR IS PREFERRED AND IT IS NOT A SHORTCUT. `kontra bundle orchestrator` writes
// `<bundle>.tar.gz.sha256` beside the archive in the format `sha256sum -c` reads, and reading it
// is what makes a second `kontra up` free: the working directory is named by the digest, so
// knowing the digest without opening the archive is knowing that the work is already done.
//
// AND IT IS STILL CHECKED. The digest from the sidecar is what the store is told to EXPECT, and
// the bytes are hashed on their way in — so a sidecar that disagrees with its archive is refused
// by name, on the first run, rather than believed.
//
// Without a sidecar the archive is hashed here. That costs a pass over 125 MB on every start,
// which is why the builder writes one; a bundle copied to a machine without its sidecar still
// works, it just pays.
func bundleDigest(bundle string) (string, error) {
	if sum, err := readDigestSidecar(bundle + ".sha256"); err == nil {
		return sum, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	if _, err := os.Stat(bundle); err != nil {
		return "", fmt.Errorf("no orchestrator bundle at %s: %w", bundle, err)
	}
	return sha256File(bundle)
}

// readDigestSidecar reads `<hex>  <name>` — the shape `sha256sum` writes and `sha256sum -c`
// reads. Anything else is a file somebody wrote by hand, and guessing at it would be how a
// truncated digest becomes a working directory named after half a hash.
func readDigestSidecar(path string) (string, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	first, _, _ := strings.Cut(strings.TrimSpace(string(body)), "\n")
	sum, _, _ := strings.Cut(strings.TrimSpace(first), " ")
	if err := casstore.ValidateDigest(sum); err != nil {
		return "", fmt.Errorf("%s does not hold a sha256 digest: %w", path, err)
	}
	return sum, nil
}

func readManifestFile(path string) (*Manifest, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &m, nil
}

func fileSize(path string) int64 {
	st, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return st.Size()
}

// --- what a first run says about itself -------------------------------------------------------

// HydrationReport turns the store's progress events into lines a human reads while waiting.
//
// EXPORTED FOR THE SECOND ARTIFACT, not for the shape of it. Temporal's Web UI is hydrated out of
// the SAME CAS by the same store (appliance/temporalui.go) and a first `--temporal-ui` waits on a
// ~9 MB download; a private copy of these lines there is how the two halves of one wait end up
// printing in two formats. It is the only thing this package exports that is not about a bundle.
//
// TWO FLOORS, ONE ABOVE THE OTHER, and they are not redundant. The store coalesces to one event
// every 500 ms so that 31,823 files do not become 31,823 callbacks; this coalesces AGAIN to one
// LINE every couple of seconds, because a terminal that scrolls sixty lines during a hydration
// has told the operator less than one that scrolls fifteen. A phase change always prints,
// whatever the floor says — the phases are the shape of the wait.
type HydrationReport struct {
	w     io.Writer
	start time.Time
	last  time.Time
	phase hydratestore.Phase
}

const hydrationLineFloor = 2 * time.Second

func NewHydrationReport(w io.Writer) *HydrationReport {
	return &HydrationReport{w: w, start: time.Now()}
}

func (r *HydrationReport) Say(format string, args ...any) {
	if r.w == nil {
		return
	}
	fmt.Fprintf(r.w, "%s\n", fmt.Sprintf(format, args...))
}

func (r *HydrationReport) Observe(p hydratestore.Progress) {
	if r.w == nil {
		return
	}
	// A FINAL REPORT ALWAYS PRINTS. It is the only one that carries the phase's whole total, and
	// suppressing it leaves the reader looking at whatever fraction the floor let through.
	if !p.Final && p.Phase == r.phase && time.Since(r.last) < hydrationLineFloor {
		return
	}
	r.phase, r.last = p.Phase, time.Now()
	fmt.Fprintf(r.w, "  [%5.1fs] %-12s %s\n", time.Since(r.start).Seconds(), p.Phase, hydrationScale(p))
}

func hydrationScale(p hydratestore.Progress) string {
	out := humanBytes(p.Bytes)
	if p.Total > 0 {
		out += fmt.Sprintf(" of %s", humanBytes(p.Total))
	}
	if p.Files > 0 {
		out += fmt.Sprintf(" over %d files", p.Files)
	}
	return out
}

// done is the last word on a hydration: what it cost, and which rung the filesystem gave us.
//
// THE METHOD IS PRINTED BECAUSE IT IS A FACT ABOUT THE OPERATOR'S DISK, not about kontra. On
// btrfs or XFS with reflink this is free and instant; on ext4 it is 31,823 hardlinks, which is
// still nearly free; if it says `copy`, this machine just wrote 450 MB and the next hydration
// will too, and that is worth knowing before wondering where the disk went.
func (r *HydrationReport) Done(res hydratestore.Result) {
	if r.w == nil {
		return
	}
	// A REPAIR IS NEVER SILENT. It means something on this disk was found not to be the artifact
	// — a half-written working directory, a truncated file, bytes that changed under the store —
	// and the appliance has just spent a first run's worth of work putting it right. An operator
	// who is told will look at the disk; one who is not will notice that starts got slow.
	if res.Repaired && res.Damage != nil {
		fmt.Fprintf(r.w, "  [%5.1fs] hydrated again: %v\n", time.Since(r.start).Seconds(), res.Damage)
	}
	switch {
	case res.Adopted:
		fmt.Fprintf(r.w, "  [%5.1fs] verified %s over %d files that were already here\n",
			time.Since(r.start).Seconds(), humanBytes(res.Bytes), res.Files)
	case res.Existing && !res.Repaired:
		fmt.Fprintf(r.w, "  [%5.1fs] already hydrated by another process; verified and using it\n", time.Since(r.start).Seconds())
	default:
		fmt.Fprintf(r.w, "  [%5.1fs] hydrated %s over %d files by %s\n",
			time.Since(r.start).Seconds(), humanBytes(res.Bytes), res.Files, res.Method)
	}
}
