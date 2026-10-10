// Package hydrate turns a pinned, checksummed URL into a working directory. It is the
// appliance's first-run path: an artifact is named by its digest, fetched once, verified
// BEFORE it is stored, and materialized copy-on-write out of the store (ADR 0031 §2). The
// customers are the ones that ADR names — the Node runtime the carried orchestrator runs
// on, its native addons, the built SPA, and the opt-in Temporal Web UI.
//
// IT IS A SEPARATE PACKAGE FROM cas, DELIBERATELY. cas owns the address, the bytes and the
// copy-on-write primitive, and it has two customers: this one and the embedded registry.
// The registry never fetches from a pinned URL — its layers arrive by push — so an HTTP
// client, a pin-shaped URL policy and a tar expander inside cas would be dead weight it
// has to compile and reason about. The rule from the ADR is "do not build a second store",
// not "put everything in one package", and the way to keep a shared store shared is to
// stop it growing customers' policies. The store is shared; the fetch policy is not.
//
// COPY-ON-WRITE IS NOT A SECURITY BOUNDARY. See the cas package header. A hydrated working
// directory deduplicates bytes and removes a copy from a cold start; it is not isolation,
// and nothing untrusted should be run against one. Actors get containers.
//
// TWO ENTRY POINTS, AND THE DIFFERENCE IS WHAT THEY PROMISE. Hydrate is store-if-absent all
// the way down: absent bytes are fetched, an absent tree is expanded, an absent working
// directory is cloned, and anything that is already there is left alone WITHOUT being looked
// at. EnsureHydrated (verify.go) is the one the appliance calls, because the appliance's
// next act is to exec what it was handed: it verifies the working copy against the
// artifact's own inventory, adopts it if it is whole, discards and rebuilds it if it is not,
// and hydrates once more before giving up.
//
// WHAT THIS FILE GIVES THAT ONE TO BUILD ON is that nothing incomplete is ever visible under
// a name a caller reads — every publish here is a link or a rename, so a killed process
// leaves a leftover temporary and never a half-built artifact wearing the real name. What it
// cannot give is that nothing ELSE ever produces one, which is why verify.go exists.
package hydrate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/medmahmoudi26/kontra/runtime/handler/internal/cas"
)

// Kind is what the fetched bytes are. A single file is materialized as one working copy; a
// tar.gz is expanded once into a golden tree and each working directory is a copy-on-write
// clone of that tree.
type Kind string

const (
	KindFile  Kind = "file"
	KindTarGz Kind = "tar.gz"
)

// Artifact is a pin. Every field except Size is part of the identity of what gets installed,
// and the digest is the only one that is authoritative — the URL says where to look, the
// digest says what counts as having found it.
type Artifact struct {
	Name   string // for messages: "node-runtime", "duckdb-cli"
	URL    string // pinned; a moving URL is refused by Validate
	Digest string // sha256, lower hex, of the bytes at URL
	Size   int64  // optional; 0 means unknown. A cross-check on the pin, not on the download.
	Kind   Kind

	// StripComponents drops leading path elements from every tar entry, the way
	// `tar --strip-components` does. Upstream tarballs almost always carry a single
	// version-named top directory, and hydrating into a directory whose name repeats the
	// version is how a caller ends up building paths out of string concatenation.
	StripComponents int

	// Executable applies to KindFile only: the working copy needs +x. It forces the
	// materialization to Writable, because the stored object is 0444 and a Shared
	// materialization may hardlink it — and a chmod on a hardlink is a chmod on the store.
	Executable bool

	// MaxExpanded caps the total bytes a tar may expand to. 0 uses defaultMaxExpanded. A
	// compressed artifact that expands without limit fills the disk, and this repo has
	// already learned what a full disk costs when nothing names it.
	MaxExpanded int64
}

const defaultMaxExpanded = 8 << 30 // 8 GiB: far above any artifact the appliance ships

// fetchHeadroom is what a fetch wants free ON TOP of the artifact it is about to write.
//
// A FETCH IS THE ONE STEP WHOSE COST IS BOTH KNOWN AND UNAVOIDABLE, which is why it is the
// only one this package checks in advance. The size is the Content-Length (or the local
// file's length) and every one of those bytes is going to be written, so a disk that cannot
// hold them will not somehow hold them by being asked. Everything downstream is different:
// an expansion's size is not knowable until it is read, and a copy-on-write clone of an
// expanded tree costs NOTHING on a filesystem with reflink or on any Shared hydration that
// hardlinks — so a preflight there would refuse hydrations that were about to succeed for
// free. Those steps get cas.DiskError instead, which names the disk when the disk is what
// went wrong.
//
// The headroom itself is small and deliberately so: it is not a fudge factor on the artifact
// (whose size is exact), it is a refusal to drive a filesystem to literally zero, which on
// this system is where a storage backend starts answering a bare 500 to every write.
const fetchHeadroom = 16 << 20

// Validate refuses a pin that is not one.
func (a Artifact) Validate() error {
	if strings.TrimSpace(a.Name) == "" {
		return errors.New("artifact has no name; the name is what its failures will be reported under")
	}
	if err := cas.ValidateDigest(a.Digest); err != nil {
		return fmt.Errorf("artifact %q: %w", a.Name, err)
	}
	switch a.Kind {
	case KindFile, KindTarGz:
	default:
		return fmt.Errorf("artifact %q: unknown kind %q", a.Name, a.Kind)
	}
	u, err := url.Parse(a.URL)
	if err != nil {
		return fmt.Errorf("artifact %q: unparseable URL %q: %w", a.Name, a.URL, err)
	}
	switch u.Scheme {
	case "https":
	case "file":
		// AN ARTIFACT THIS MACHINE ALREADY HAS, and it is not a loophole in the pin — it is
		// the appliance's own bundle before there is a release to fetch it from. `kontra
		// bundle orchestrator` writes a tar.gz into the checkout; issue 14 hydrates that file,
		// and issue 17 is what gives the same bytes a URL. Everything the digest promises is
		// unchanged: the bytes are streamed through the same hash into the same store, so a
		// file that is not what it claims to be is refused before anything can exec it.
		//
		// Absolute only. A relative `file:` URL would be resolved against whatever directory
		// the process happened to be started in, which is the one thing a pin must never be a
		// function of.
		if u.Host != "" && u.Host != "localhost" {
			return fmt.Errorf("artifact %q: %q names host %q; a file URL is local to this machine (file:///path)", a.Name, a.URL, u.Host)
		}
		if !strings.HasPrefix(u.Path, "/") || u.Opaque != "" {
			return fmt.Errorf("artifact %q: %q is not an absolute path (file:///path, three slashes)", a.Name, a.URL)
		}
	case "http":
		// Loopback only, for tests and for a mirror on the same machine. Everywhere else
		// the digest would be the ONLY thing standing between an operator and whatever a
		// network wanted to hand them, and a plaintext fetch of a pinned artifact is a
		// downgrade nobody chose.
		if !isLoopback(u.Hostname()) {
			return fmt.Errorf("artifact %q: plain http is only allowed to loopback, not %q", a.Name, u.Host)
		}
	default:
		return fmt.Errorf("artifact %q: URL scheme %q is not https", a.Name, u.Scheme)
	}
	// A PIN, NOT A POINTER. ADR 0031 finding 6: scripts/dev-setup.sh fetches buf from
	// `/releases/latest/download/` with no version and no checksum, and that is the one of
	// its three fetches that is not reproducible at all. A digest against a moving URL is
	// not reproducibility either — it is an install that works until upstream cuts a
	// release and then fails for everyone at once, with a message about integrity rather
	// than about the URL. Refuse it here, where the message can say what is actually wrong.
	for _, seg := range strings.Split(u.EscapedPath(), "/") {
		if seg == "latest" {
			return fmt.Errorf("artifact %q: URL %q is not pinned (a %q path segment moves under you)", a.Name, a.URL, "latest")
		}
	}
	return nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Store is the appliance's artifact store: a cas.Local for the fetched bytes, plus the
// expanded golden trees those bytes unpack to.
type Store struct {
	cas    *cas.Local
	trees  string
	client *http.Client

	// progress is nil unless a caller asked for it. See progress.go — the phases are only
	// visible from in here, and a first run that prints nothing for thirty seconds is
	// indistinguishable from a hung one.
	progress func(Progress)
}

// Open opens (and creates) the store under root. The layout is the store's business, but it
// is stable enough to name here because an operator will look:
//
//	<root>/cas/<ab>/<digest>   the fetched artifact, immutable, 0444
//	<root>/tmp/                in-flight writes; anything here is debris
//	<root>/trees/<digest>/     the golden expansion of a tar.gz artifact
func Open(root string) (*Store, error) {
	c, err := cas.NewLocal(root)
	if err != nil {
		return nil, err
	}
	trees := filepath.Join(c.Root(), "trees")
	if err := os.MkdirAll(trees, 0o755); err != nil {
		return nil, fmt.Errorf("create tree directory %s: %w", trees, err)
	}
	return &Store{
		cas:   c,
		trees: trees,
		// No global default client: a hydration that hangs forever on a stalled mirror is
		// indistinguishable from a slow one, and `kontra up` would simply never finish.
		// Generous, because 54 MB over a bad link is slow but not broken.
		client: &http.Client{Timeout: 30 * time.Minute},
	}, nil
}

// CAS is the shared store. The embedded registry (issue 12) points its storage driver at
// this, rather than opening a second one.
func (s *Store) CAS() *cas.Local { return s.cas }

// Root is the directory the store owns.
func (s *Store) Root() string { return s.cas.Root() }

// SetHTTPClient replaces the fetch client. For tests, and for an installation that has to
// go through a proxy.
func (s *Store) SetHTTPClient(c *http.Client) { s.client = c }

// Result is what happened, in the terms an operator asks the question: did we download, or
// did the store already have it; and did this machine give us copy-on-write or a full copy.
type Result struct {
	Artifact Artifact
	Digest   string
	Dest     string

	// Fetched is false when the store already held the artifact — the second install, or
	// the second machine restore.
	Fetched bool
	// Existing is true when dest was already there and was left completely alone. Whether
	// what is there is CORRECT is Hydrate's caller's question; EnsureHydrated is the call
	// that answers it.
	Existing bool

	// Verified is true when dest was checked against the artifact's own inventory during
	// THIS call. Hydrate never sets it; EnsureHydrated never returns without it.
	Verified bool
	// Adopted is true when dest was already complete but had no receipt, and was vouched
	// for rather than rebuilt. It is what an upgrade to a verifying binary looks like.
	Adopted bool
	// Repaired is true when something was found wrong and dest was hydrated again. Damage
	// is what was found — the thing an operator needs to see, since a repair that says
	// nothing is a machine quietly re-downloading 125 MB on every start.
	Repaired bool
	Damage   error

	// Method is the WEAKEST rung any file needed. Reporting the first or the best would
	// let one copied file hide behind ten thousand reflinked ones.
	Method   cas.Method
	Files    int
	Bytes    int64
	Fetching time.Duration
	Elapsed  time.Duration
}

// Fetch makes sure the artifact's bytes are in the store, verified. It returns whether it
// had to go to the network.
func (s *Store) Fetch(ctx context.Context, a Artifact) (bool, error) {
	if err := a.Validate(); err != nil {
		return false, err
	}
	has, err := s.cas.Has(a.Digest)
	if err != nil {
		return false, err
	}
	if has {
		return false, nil
	}

	body, total, err := s.open(ctx, a)
	if err != nil {
		return false, err
	}
	defer body.Close()

	// SAY IT BEFORE SPENDING TWO MINUTES DISCOVERING IT. A transfer that fills the disk at
	// 60 MB of 125 costs the download, leaves the artifact unstored, and — before ENOSPC was
	// classified — reported a write error about a temporary file nobody has heard of. When
	// the size is known in advance, so is the answer.
	if total > 0 {
		if err := cas.RequireSpace("hydrating "+a.Name, s.cas.Root(), total+fetchHeadroom); err != nil {
			return false, err
		}
	}

	// Streamed through the hash into the store. Nothing is ever written under the
	// artifact's address unless the whole body hashed to the pin — a truncated transfer and
	// a substituted file fail identically here, before there is anything to materialize.
	rep := s.reportFor(a.Name, PhaseFetch, total)
	size, err := s.cas.PutExpecting(&countingReader{r: body, p: rep}, a.Digest, a.URL)
	rep.done()
	if err != nil {
		return false, fmt.Errorf("hydrate %s: %w", a.Name, err)
	}
	if a.Size > 0 && size != a.Size {
		// The digest matched, so the bytes are the right ones and the SIZE in the pin is
		// wrong. Worth saying, because a pin that is half-wrong is a pin somebody edited.
		return true, fmt.Errorf("artifact %q: pinned size %d but %d bytes hashed to the pinned digest; fix the pin", a.Name, a.Size, size)
	}
	return true, nil
}

// open acquires the artifact's bytes and, when anything knows it, their total size.
//
// TWO SOURCES, ONE VERIFICATION. An https URL is somebody else's machine and a file URL is
// this one; the difference ends at this function, because what makes either safe is the
// digest the bytes are streamed through afterwards, not where they came from.
func (s *Store) open(ctx context.Context, a Artifact) (io.ReadCloser, int64, error) {
	if path, ok := localPath(a.URL); ok {
		f, err := os.Open(path)
		if err != nil {
			return nil, 0, fmt.Errorf("hydrate %s from %s: %w", a.Name, path, err)
		}
		size := int64(0)
		if st, err := f.Stat(); err == nil {
			size = st.Size()
		}
		return f, size, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("fetch %s: %w", a.Name, err)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("fetch %s from %s: %w", a.Name, a.URL, err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, 0, fmt.Errorf("fetch %s from %s: HTTP %s", a.Name, a.URL, resp.Status)
	}
	total := resp.ContentLength
	if total < 0 {
		total = 0 // chunked: a caller must not draw a percentage out of -1
	}
	return resp.Body, total, nil
}

// localPath answers the filesystem path of a `file:` artifact, and false for anything else.
// Validate has already refused the shapes that are not absolute, so this is a reader rather
// than a second policy.
func localPath(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "file" {
		return "", false
	}
	return u.Path, true
}

// FileURL is the pin for an artifact that is already on this machine. It exists so callers
// build the URL in one place rather than pasting "file://" in front of a path that might be
// relative — which parses, hydrates the wrong file, and blames the digest.
func FileURL(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return (&url.URL{Scheme: "file", Path: abs}).String(), nil
}

// Hydrate ensures the artifact is in the store and materializes it at dest.
//
// It is store-if-absent for the working directory as well as for the bytes: if dest already
// exists it is not touched, and Result.Existing says so. Two `kontra up` invocations racing
// a first run therefore converge on one store and one working directory, whichever of them
// got there first — see cas.Local.put for why that needs no lock.
func (s *Store) Hydrate(ctx context.Context, a Artifact, dest string, mode cas.Mode) (Result, error) {
	started := time.Now()
	res := Result{Artifact: a, Digest: a.Digest, Dest: dest}
	if err := a.Validate(); err != nil {
		return res, err
	}

	fetchStart := time.Now()
	fetched, err := s.Fetch(ctx, a)
	if err != nil {
		return res, err
	}
	res.Fetched, res.Fetching = fetched, time.Since(fetchStart)

	switch _, err := os.Lstat(dest); {
	case err == nil:
		res.Existing, res.Elapsed = true, time.Since(started)
		return res, nil
	case !errors.Is(err, fs.ErrNotExist):
		return res, fmt.Errorf("inspect %s: %w", dest, err)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return res, fmt.Errorf("create %s: %w", filepath.Dir(dest), err)
	}

	switch a.Kind {
	case KindFile:
		// A FILE ARTIFACT IS THE ONE THAT LEAVES THE STORE WITHOUT BEING READ. A reflink
		// shares extents and a hardlink shares the inode; neither looks at a byte, so an
		// object that changed under the store would be materialized, exec'd, and never
		// questioned. An archive gets this for free because expansion reads it (tree.go);
		// this is the same guarantee, paid for explicitly.
		//
		// Skipped when the fetch we just did put the bytes there: they went through the
		// hash on their way in, seconds ago, and hashing them again proves nothing about
		// anything except this process's own memory.
		if !res.Fetched {
			if err := s.cas.Verify(a.Digest); err != nil {
				return res, fmt.Errorf("materialize %s: %w", a.Name, err)
			}
		}
		fileMode := mode
		if a.Executable {
			// Cannot be Shared: the stored object is 0444 and Shared may hardlink it, so
			// the chmod that makes the working copy runnable would make the STORE runnable
			// and writable-adjacent. Writable still reflinks where the filesystem can.
			fileMode = cas.Writable
		}
		method, err := s.cas.Materialize(a.Digest, dest, fileMode)
		if err != nil {
			return res, fmt.Errorf("materialize %s: %w", a.Name, err)
		}
		if a.Executable {
			if err := os.Chmod(dest, 0o755); err != nil {
				return res, fmt.Errorf("make %s executable: %w", dest, err)
			}
		}
		st, err := os.Stat(dest)
		if err != nil {
			return res, err
		}
		res.Method, res.Files, res.Bytes = method, 1, st.Size()

	case KindTarGz:
		tree, _, err := s.ensureTree(a)
		if err != nil {
			return res, err
		}
		rep := s.reportFor(a.Name, PhaseMaterialize, 0)
		method, files, bytes, existing, err := cloneTree(tree, dest, mode, rep)
		rep.done()
		if err != nil {
			return res, fmt.Errorf("materialize %s at %s: %w", a.Name, dest, err)
		}
		res.Method, res.Files, res.Bytes, res.Existing = method, files, bytes, existing

	default:
		return res, fmt.Errorf("artifact %q: unknown kind %q", a.Name, a.Kind)
	}

	res.Elapsed = time.Since(started)
	return res, nil
}
