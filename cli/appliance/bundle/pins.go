// pins.go — THE PINS the orchestrator bundle is built from, and the one fetch that redeems them
// (ADR 0031 §2, issues 13 and 17).
//
// DELIBERATELY NOT SPLIT, AND THIS PARAGRAPH IS THE DECISION. `Pin`, `fetchPinned` and
// `extractMember` are the ALREADY-CONCENTRATED version of four fetches that used to be four:
// Node's tarball, pnpm's single-file bundle from the npm registry, the SPA, and Temporal's own
// ui-server release. They differ in vendor, in URL shape, in digest ALGORITHM (nodejs.org
// publishes sha256 hex; npm publishes an SRI `sha512-<base64>` integrity string) and in whether
// one member or a whole tree is wanted — and they agree on the only thing that matters, which is
// that bytes are streamed through their hash and are never written under the pin's address unless
// the whole body hashed to it. Four copies of that rule is four places for one of them to skip a
// verify on a cache hit. Do not "fix" this into per-consumer helpers; a fifth pinned artifact is
// meant to arrive as a table row and a `Pin`, not as a fifth downloader.
//
// FOUR PLATFORMS AND FIVE TABLES, which is the shape issue 17 gave this file and the reason it is
// worth reading before editing. A platform is buildable only when every one of these agrees about
// it, and each belongs to a different vendor's vocabulary:
//
//	nodeDigests      nodejs.org's sha256 for that artifact set
//	nodePlatform     Node's own naming     linux-x64, darwin-arm64
//	rustTriple       @temporalio/core-bridge's  x86_64-unknown-linux-gnu, aarch64-apple-darwin
//	npmArchitecture  pnpm's os/cpu/libc    darwin, arm64, (no libc)
//	Platforms()      the list itself
//
// Adding a fifth platform means five edits, and forgetting one of them fails differently and
// usually late — a 404, a URL with an empty segment, a prune that deletes the binary the runtime
// is about to ask for, or, worst, a tree resolved for the machine that ran the build.
// `TestEveryShippedPlatformResolvesEveryTable` walks {@link Platforms} and asks all five, so the
// failure is a test naming the table rather than a bundle nobody can run.
//
// Same shape as install.sh's Go step, which is the one of its three fetches that is reproducible:
// a version, a URL built from it, and a checksum per platform, all bumped together. The buf step
// in that file — `/releases/latest/download/` with no version and no checksum — is the shape this
// deliberately does not copy.
//
// ONE DIFFERENCE FROM install.sh, AND IT IS THE POINT OF THIS FILE. There, the version and the
// digests are three independent shell variables:
//
//	GO_VERSION="1.26.8"
//	GO_SHA256_amd64=1153d3d5...
//
// Editing the first and not the second is a one-character mistake that still fetches, still runs,
// and fails at `sha256sum -c` with `FAILED` and no statement of what was expected of whom. Here
// the digests are keyed BY VERSION, so bumping {@link NodeVersion} without adding its row cannot
// reach the network at all: {@link NodePin} refuses with the version, the platform and the URL of
// the checksum file to copy the digest out of. A pin you cannot half-bump is the whole reason to
// write the table this way round.
//
// THE DIGESTS ARE UPSTREAM'S OWN, from https://nodejs.org/dist/v<version>/SHASUMS256.txt. Not
// computed from a tarball somebody already had on disk: a checksum taken from the file you
// already fetched checks the copy, not the artifact.
//
// SECOND HOME FOR THE SAME PIN, deliberately and with a test. `runtime/handler/internal/hydrate` pins the
// Node runtime for the appliance's fetch path and cannot be imported from here — it is under
// `runtime/handler/internal/`, so Go's own visibility rule stops the cli module reading it. Two files
// therefore name one Node version, and `pins_test.go` reads the other one off disk and fails if
// they have drifted. Two Nodes in one appliance is the failure that test exists to make
// impossible: the binary hydrates one and the bundle carries the other, and nothing says so.
package bundle

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// NodeVersion is the Node the carried orchestrator runs on.
//
// `control/orchestrator/package.json` says `engines.node: ">=22.13.0"`. The appliance ships an EXACT
// version, because ">=" is a constraint on somebody else's machine and this is our machine — and
// because a bundle whose runtime floats is a bundle whose digest describes nothing.
const NodeVersion = "22.13.0"

// nodeDigests is version → "goos/goarch" → sha256 of the upstream `.tar.gz`.
//
// Keyed by version FIRST so a bump lands in a row that does not exist yet. Old rows are kept
// rather than deleted: they cost four lines, and they are what makes rolling a bundle back to a
// previous runtime an edit of one constant instead of an archaeology exercise in the git log.
var nodeDigests = map[string]map[string]string{
	"22.13.0": {
		"linux/amd64":  "9a33e89093a0d946c54781dcb3ccab4ccf7538a7135286528ca41ca055e9b38f",
		"linux/arm64":  "e0cc088cb4fb2e945d3d5c416c601e1101a15f73e0f024c9529b964d9f6dce5b",
		"darwin/amd64": "cfaaf5edde585a15547f858f5b3b62a292cf5929a23707b6f1e36c29a32487be",
		"darwin/arm64": "bc1e374e7393e2f4b20e5bbc157d02e9b1fb2c634b2f992136b38fb8ca2023b7",
	},
}

// nodePlatform maps a GOOS/GOARCH pair to Node's own naming for its artifact sets. The four ADR
// 0031 §6 commits to; issue 13 built the one it ran on, issue 17 builds all four.
var nodePlatform = map[string]string{
	"linux/amd64":  "linux-x64",
	"linux/arm64":  "linux-arm64",
	"darwin/amd64": "darwin-x64",
	"darwin/arm64": "darwin-arm64",
}

// PnpmVersion is the package manager the dependency tree is resolved by.
//
// IT IS ALSO `control/orchestrator/package.json`'s `packageManager` FIELD, and a test holds the two
// together. Corepack resolves that field to decide which pnpm a developer's `pnpm` really runs;
// this constant decides which pnpm the BUILD runs. Two different mechanisms reading two different
// files is how a bundle gets resolved by a pnpm nobody chose — so they are checked against each
// other rather than merely both being written down.
const PnpmVersion = "10.33.2"

// pnpmDigests is version → the registry's own Subresource Integrity string for the npm tarball.
//
// ONE ROW PER VERSION AND NO PLATFORM AXIS, because pnpm ships as JavaScript. That is the reason
// this build fetches the npm tarball rather than one of pnpm's per-platform standalone
// executables: a package manager that runs on the Node we already pin is one artifact and one
// digest for all four targets, and the standalone route would be four more binaries to pin for no
// gain.
//
// sha512 IN SRI FORM, NOT sha256, and that is deliberate. Everything else here is checksummed
// with the digest UPSTREAM PUBLISHES — `SHASUMS256.txt` for Node — and what npm publishes is
// `dist.integrity`, the same string pnpm itself checks every package in the lockfile against. A
// sha256 would have to be computed from a tarball somebody already downloaded, which checks the
// copy rather than the artifact. Read it back with:
//
//	curl -s https://registry.npmjs.org/pnpm/<version> | jq -r .dist.integrity
var pnpmDigests = map[string]string{
	"10.33.2": "sha512-qQ+vb+6rca1sblf5Tg/hoS9dzCLNdU20CulZPraj4LaxLjVAIYuzeuCDQEsfLObbKkEh6XmCm0r/lLmfSdoc+A==",
}

// Platforms are the four the appliance ships (ADR 0031 §6, PRD locked decision 7), in a fixed
// order so a release's file list and a CI matrix read the same way every time.
//
// THE LIST LIVES HERE AND NOWHERE ELSE. `nodeDigests` is keyed by version first, so it cannot be
// the list; a CI matrix is YAML and cannot be read by the build; a `--platform all` loop that
// wrote its own four would be a fifth place to forget one. A test walks this slice and asserts
// every entry resolves a Node pin, a Node naming, a Rust triple and an npm architecture — which
// makes adding a platform a single edit that fails loudly until all four tables agree.
func Platforms() []Platform {
	return []Platform{
		{OS: "linux", Arch: "amd64"},
		{OS: "linux", Arch: "arm64"},
		{OS: "darwin", Arch: "amd64"},
		{OS: "darwin", Arch: "arm64"},
	}
}

// Platform is a build target, in Go's vocabulary rather than Node's or Rust's. Go's is the one
// the binary that will carry this bundle already speaks, and every other naming in the bundle
// (Node's `linux-x64`, Rust's `x86_64-unknown-linux-gnu`) is derived from it in exactly one place.
type Platform struct {
	OS   string // GOOS
	Arch string // GOARCH
}

func (p Platform) String() string { return p.OS + "/" + p.Arch }

// ParsePlatform reads a "goos/goarch" pair.
func ParsePlatform(s string) (Platform, error) {
	os, arch, ok := strings.Cut(strings.TrimSpace(s), "/")
	if !ok || os == "" || arch == "" {
		return Platform{}, fmt.Errorf("platform %q is not goos/goarch (for example linux/amd64)", s)
	}
	return Platform{OS: os, Arch: arch}, nil
}

// Pin is a fetchable artifact whose identity is its digest. The URL says where to look; the
// digest says what counts as having found it.
//
// TWO DIGEST FIELDS AND EXACTLY ONE IS SET, because the two upstreams this build fetches from
// publish different things. nodejs.org publishes `SHASUMS256.txt`; the npm registry publishes
// `dist.integrity`, an sha512 in Subresource Integrity form. Taking each upstream's OWN digest is
// the rule — the alternative is normalising them to one algorithm, which means computing a digest
// from a tarball we already have, and a checksum of the copy you fetched checks the fetch and not
// the artifact. {@link Pin.hasher} enforces that one and only one is present, so a pin can never
// be added with neither.
type Pin struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	URL     string `json:"url"`
	Digest  string `json:"sha256,omitempty"`

	// Integrity is an SRI string — `sha512-<base64>` — verbatim from the npm registry.
	Integrity string `json:"integrity,omitempty"`
}

// hasher returns the hash this pin is checked with.
//
// A PIN WITH NO DIGEST IS A BUG IN THIS FILE, not a fetch that skips verification, so it is an
// error here rather than a permissive default. Nothing downstream has to remember to check.
func (p Pin) hasher() (hash.Hash, error) {
	switch {
	case p.Digest != "" && p.Integrity != "":
		return nil, fmt.Errorf("%s pins both a sha256 and an integrity string; one artifact has one upstream digest", p.Name)
	case p.Digest != "":
		return sha256.New(), nil
	case p.Integrity != "":
		alg, _, ok := strings.Cut(p.Integrity, "-")
		if !ok || alg != "sha512" {
			return nil, fmt.Errorf("%s: integrity %q is not the sha512-<base64> form npm publishes", p.Name, p.Integrity)
		}
		return sha512.New(), nil
	default:
		return nil, fmt.Errorf("%s %s has no digest, so fetching it would verify nothing", p.Name, p.Version)
	}
}

// expected is the pin's digest written the way {@link Pin.observed} writes what arrived, so the
// two are comparable as strings and an error message can show them one above the other.
func (p Pin) expected() string {
	if p.Digest != "" {
		return p.Digest
	}
	return p.Integrity
}

// observed renders a finished hash in the same notation the pin uses: bare hex for sha256, and
// `sha512-<base64>` for an SRI pin. A mismatch then prints two strings of the same shape rather
// than asking the reader to convert one into the other before they can tell them apart.
func (p Pin) observed(h hash.Hash) string {
	if p.Digest != "" {
		return hex.EncodeToString(h.Sum(nil))
	}
	return "sha512-" + base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// cacheName is what this pin's artifact is called in the content-addressed cache.
//
// Hex for a sha256 pin — unchanged, so a cache filled before this file learned a second algorithm
// still hits — and `sha512-<hex>` for an SRI one, because base64 contains `/` and `+` and a digest
// used verbatim as a filename would scatter artifacts into directories nobody created.
func (p Pin) cacheName() (string, error) {
	if p.Digest != "" {
		return p.Digest, nil
	}
	_, raw, err := p.integrityBytes()
	if err != nil {
		return "", err
	}
	return "sha512-" + hex.EncodeToString(raw), nil
}

func (p Pin) integrityBytes() (string, []byte, error) {
	alg, b64, ok := strings.Cut(p.Integrity, "-")
	if !ok {
		return "", nil, fmt.Errorf("%s: integrity %q is not the <alg>-<base64> form npm publishes", p.Name, p.Integrity)
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "", nil, fmt.Errorf("%s: integrity %q does not decode as base64: %w", p.Name, p.Integrity, err)
	}
	return alg, raw, nil
}

// digestFile re-derives this pin's digest from a file already on disk, in the pin's own notation.
// It is what makes a cache HIT verify rather than merely exist.
func (p Pin) digestFile(path string) (string, error) {
	h, err := p.hasher()
	if err != nil {
		return "", err
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return p.observed(h), nil
}

// NodePin resolves the pinned Node runtime for a platform, or explains precisely which edit is
// missing.
//
// TWO DIFFERENT FAILURES, SAID DIFFERENTLY, because they have different fixes. An unknown VERSION
// means somebody bumped the constant and stopped there — the fix is to paste four digests out of
// upstream's checksum file, and the message carries its URL. An unknown PLATFORM means this
// version has rows but not that one — the fix is a single line, and the message lists what is
// pinned so a typo (`linux/x86_64`) reads as a typo.
func NodePin(p Platform) (Pin, error) { return nodePin(NodeVersion, p) }

// nodePin is NodePin with the version as an argument, so a test can ask what happens to a version
// nobody has added digests for. That question cannot be asked of a `const`, and it is the exact
// failure the table shape exists to produce — an invariant with no way to test it is an invariant
// that stops holding quietly.
func nodePin(version string, p Platform) (Pin, error) {
	byPlatform, ok := nodeDigests[version]
	if !ok {
		return Pin{}, fmt.Errorf(
			"node %s has no pinned digests: NodeVersion was bumped without its row in nodeDigests.\n"+
				"  add it from https://nodejs.org/dist/v%s/SHASUMS256.txt (pinned versions: %s)",
			version, version, strings.Join(pinnedNodeVersions(), ", "))
	}
	digest, ok := byPlatform[p.String()]
	if !ok {
		return Pin{}, fmt.Errorf(
			"node %s is not pinned for %s; pinned platforms are %s.\n"+
				"  add the digest from https://nodejs.org/dist/v%s/SHASUMS256.txt",
			version, p, strings.Join(sortedKeys(byPlatform), ", "), version)
	}
	naming, ok := nodePlatform[p.String()]
	if !ok {
		// Reachable only by adding a digest row without the naming row beside it. Say which of
		// the two tables is short rather than producing a 404 URL from an empty string.
		return Pin{}, fmt.Errorf("node %s: %s has a digest but no entry in nodePlatform", version, p)
	}
	return Pin{
		Name:    "node-runtime",
		Version: version,
		URL:     fmt.Sprintf("https://nodejs.org/dist/v%s/node-v%s-%s.tar.gz", version, version, naming),
		Digest:  digest,
	}, nil
}

// nodeTarballPrefix is the single top-level directory Node's tarballs carry. The bundle takes one
// file out of it, so the path is built here rather than by string concatenation at the call site.
func nodeTarballPrefix(version string, p Platform) string {
	return fmt.Sprintf("node-v%s-%s", version, nodePlatform[p.String()])
}

// PnpmPin resolves the pinned package manager, and refuses a half-bump the same way NodePin does.
//
// The message names the one command that produces the missing line, because the fix is a paste
// and the only hard part is knowing where from.
func PnpmPin() (Pin, error) { return pnpmPin(PnpmVersion) }

func pnpmPin(version string) (Pin, error) {
	integrity, ok := pnpmDigests[version]
	if !ok {
		return Pin{}, fmt.Errorf(
			"pnpm %s has no pinned digest: PnpmVersion was bumped without its row in pnpmDigests.\n"+
				"  add it with `curl -s https://registry.npmjs.org/pnpm/%s | jq -r .dist.integrity` (pinned versions: %s)",
			version, version, strings.Join(sortedKeys(pnpmDigests), ", "))
	}
	return Pin{
		Name:      "pnpm",
		Version:   version,
		URL:       fmt.Sprintf("https://registry.npmjs.org/pnpm/-/pnpm-%s.tgz", version),
		Integrity: integrity,
	}, nil
}

// npmArchitecture is what pnpm's `--os`, `--cpu` and `--libc` want, so the build can resolve a
// dependency tree for a machine it is not running on.
//
// THIS IS THE WHOLE OF CROSS-PLATFORM, AND IT IS ONE LINE OF FLAGS. `@duckdb/node-bindings-*` and
// `@swc/core-*` are separate npm packages carrying `os`/`cpu`/`libc` fields, and pnpm installs the
// ones matching the CURRENT process unless it is told otherwise — which is why a bundle built on
// linux was a linux bundle no matter what `--platform` said. These three values move that decision
// off `process.platform` and onto the target, and the committed lockfile already holds every
// variant's integrity hash, so nothing about the pinning weakens.
//
// libc IS ANSWERED FOR LINUX AND WITHHELD FOR macOS, and the asymmetry is the point. glibc and
// musl are two different `@duckdb/node-bindings-linux-x64*` packages, so a linux target that does
// not say which gets whichever the BUILDING machine runs — correct by accident on this box and
// wrong the first time CI builds linux on a macOS runner. Darwin packages declare no libc at all,
// and passing one there would filter against a field that does not exist.
func npmArchitecture(p Platform) (osName, cpu, libc string, err error) {
	osName, ok := map[string]string{"linux": "linux", "darwin": "darwin", "windows": "win32"}[p.OS]
	if !ok {
		return "", "", "", fmt.Errorf("no npm `os` naming for %q; pnpm cannot resolve a tree for it", p.OS)
	}
	cpu, ok = map[string]string{"amd64": "x64", "arm64": "arm64"}[p.Arch]
	if !ok {
		return "", "", "", fmt.Errorf("no npm `cpu` naming for %q; pnpm cannot resolve a tree for it", p.Arch)
	}
	if p.OS == "linux" {
		libc = "glibc"
	}
	return osName, cpu, libc, nil
}

// HostPlatform is the machine the build is running ON, which from issue 17 onwards is a different
// question from the machine it is building FOR.
func HostPlatform() Platform { return Platform{OS: runtime.GOOS, Arch: runtime.GOARCH} }

// rustTriple is the target name @temporalio/core-bridge files its prebuilt `.node` under. Its
// `common.js` derives it from `os.arch()`/`os.platform()` at require time — this is the same
// mapping, read off that file, so the prune below keeps the one binary the runtime will ask for.
func rustTriple(p Platform) (string, error) {
	arch, ok := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[p.Arch]
	if !ok {
		return "", fmt.Errorf("no @temporalio/core-bridge prebuild naming for arch %q", p.Arch)
	}
	sys, ok := map[string]string{"linux": "unknown-linux-gnu", "darwin": "apple-darwin", "windows": "pc-windows-msvc"}[p.OS]
	if !ok {
		return "", fmt.Errorf("no @temporalio/core-bridge prebuild naming for os %q", p.OS)
	}
	return arch + "-" + sys, nil
}

func pinnedNodeVersions() []string { return sortedKeys(nodeDigests) }

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// --- fetching -----------------------------------------------------------------------------

// fetchPinned returns a local path to the pinned artifact, downloading it only if the cache does
// not already hold bytes that match the digest.
//
// VERIFIED BEFORE IT IS USED, AND VERIFIED AGAIN ON A CACHE HIT. The second is not paranoia about
// attackers: a cache is a directory on a developer's laptop, and the cheapest way to produce a
// bundle whose manifest is a lie is to have a truncated download sitting in one.
func fetchPinned(pin Pin, cacheDir string, p func(string, ...any)) (string, error) {
	// NAMED BY THE DIGEST, WHICH IS NOT ALWAYS A FILENAME. A sha256 pin is 64 hex characters and
	// can be a path as it stands; an npm integrity string is base64 and carries `/` and `+`, so
	// using one verbatim would put an artifact in a directory nobody asked for. {@link Pin.cacheName}
	// is the one place that is decided, and it keeps the hex form for sha256 pins so a cache
	// populated before this file gained a second algorithm still hits.
	dir := filepath.Join(cacheDir, "sha256")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	name, err := pin.cacheName()
	if err != nil {
		return "", err
	}
	dst := filepath.Join(dir, name)

	if _, err := os.Stat(dst); err == nil {
		got, err := pin.digestFile(dst)
		if err != nil {
			return "", err
		}
		if got == pin.expected() {
			p("%s %s: cached, digest verified", pin.Name, pin.Version)
			return dst, nil
		}
		p("%s: cached copy has the wrong digest, refetching", pin.Name)
		if err := os.Remove(dst); err != nil {
			return "", err
		}
	}

	sum, err := pin.hasher()
	if err != nil {
		return "", err
	}

	p("fetching %s", pin.URL)
	resp, err := http.Get(pin.URL)
	if err != nil {
		return "", fmt.Errorf("fetch %s: %w", pin.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch %s: %s", pin.URL, resp.Status)
	}

	// ROOM FOR THIS ARTIFACT, NOT FOR THE WHOLE BUILD. The first version of this asked for
	// {@link minFreeBytes} — a floor sized for a 450 MB staged tree — before downloading a 50 MB
	// tarball, and its own unit tests failed on a box with 760 MB free while fetching a
	// seventeen-byte fixture. A precondition that refuses work it could have done is not a safety
	// check, it is an outage with a helpful message; the build-sized floor belongs where the
	// build-sized writes are, and this one asks about the download in front of it.
	//
	// Asked AFTER the response headers, because that is when the size is known. `Content-Length` is
	// absent on a chunked response, and then there is nothing to compute a requirement from — so
	// fall back to a figure comfortably above any artifact this pins rather than skipping the check.
	if err := requireFreeSpace(dir, fetchNeed(resp.ContentLength)); err != nil {
		return "", err
	}

	// Downloaded to a temporary name and renamed only after the digest matches, so a killed build
	// leaves a leftover and never a wrong artifact wearing the right name — the same rule
	// `runtime/handler/internal/hydrate` follows, for the same reason.
	tmp, err := os.CreateTemp(dir, ".fetching-")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()

	if _, err := io.Copy(io.MultiWriter(tmp, sum), resp.Body); err != nil {
		tmp.Close()
		return "", fmt.Errorf("download %s: %w", pin.URL, err)
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if got := pin.observed(sum); got != pin.expected() {
		// THE EXPECTATION AND WHAT ARRIVED, both, in the message. `sha256sum -c` prints `FAILED`
		// and leaves you to work out which of the two numbers was supposed to be right.
		return "", fmt.Errorf("%s does not match its pin:\n  url:      %s\n  expected: %s\n  received: %s\n"+
			"  either the pin is wrong (bump the digest with the version) or the download is not what upstream published",
			pin.Name, pin.URL, pin.expected(), got)
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return "", err
	}
	return dst, nil
}

// fetchNeed is how much room one pinned download asks for. Split out of {@link fetchPinned} so the
// rule can be asserted directly: the bug it replaced was a precondition that asked for the whole
// build's working space before fetching anything, and no test of a FETCH could have caught that —
// only a test of the SIZING can.
func fetchNeed(contentLength int64) int64 {
	if contentLength <= 0 {
		return unknownFetchSize + fetchHeadroom
	}
	return contentLength + fetchHeadroom
}

// extractMember pulls exactly one file out of a .tar.gz and returns its size.
func extractMember(archive, member, dst string) (int64, error) {
	f, err := os.Open(archive)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return 0, err
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return 0, fmt.Errorf("%s contains no %s: the archive's layout is not what the pin assumes", archive, member)
		}
		if err != nil {
			return 0, err
		}
		if strings.TrimPrefix(hdr.Name, "./") != member || hdr.Typeflag != tar.TypeReg {
			continue
		}
		out, err := os.Create(dst)
		if err != nil {
			return 0, err
		}
		n, err := io.Copy(out, tr)
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return 0, err
		}
		return n, nil
	}
}
