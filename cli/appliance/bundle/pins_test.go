package bundle

import (
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestNodePinIsAPinAndNotAPointer(t *testing.T) {
	pin, err := NodePin(Platform{OS: "linux", Arch: "amd64"})
	if err != nil {
		t.Fatalf("NodePin: %v", err)
	}
	if !strings.Contains(pin.URL, "v"+NodeVersion+"/") {
		t.Errorf("the URL does not name the pinned version: %s", pin.URL)
	}
	// ADR 0031 finding 6, and the thing `hydrate.Artifact.Validate` refuses outright: a `latest`
	// segment is an install that works until upstream cuts a release and then fails for everyone
	// at once, with a message about integrity rather than about the URL.
	if strings.Contains(pin.URL, "/latest/") {
		t.Errorf("the URL moves under us: %s", pin.URL)
	}
	if !strings.HasPrefix(pin.URL, "https://") {
		t.Errorf("a pinned artifact is fetched over https, not %s", pin.URL)
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(pin.Digest) {
		t.Errorf("digest is not a lower-hex sha256: %q", pin.Digest)
	}
}

// THE ACCEPTANCE CRITERION, as a test: "bumping a pinned version WITHOUT its digest must fail the
// build". It has to fail BEFORE the network — a build that downloads for forty seconds and then
// prints `FAILED` from `sha256sum -c` has told you that two numbers differ and not which of them
// you were supposed to edit.
func TestBumpingTheVersionWithoutItsDigestsFails(t *testing.T) {
	_, err := nodePin("99.9.9", Platform{OS: "linux", Arch: "amd64"})
	if err == nil {
		t.Fatal("an unpinned version resolved; a bundle would then be built from unverified bytes")
	}
	for _, want := range []string{"99.9.9", "nodeDigests", "SHASUMS256.txt"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the message does not name %q, so it does not say what to edit:\n%v", want, err)
		}
	}
}

func TestUnpinnedPlatformListsWhatIsPinned(t *testing.T) {
	_, err := nodePin(NodeVersion, Platform{OS: "linux", Arch: "riscv64"})
	if err == nil {
		t.Fatal("an unpinned platform resolved")
	}
	if !strings.Contains(err.Error(), "linux/amd64") {
		t.Errorf("the message does not list the pinned platforms, so a typo does not read as one:\n%v", err)
	}
}

func TestEveryPinnedPlatformResolves(t *testing.T) {
	for platform := range nodeDigests[NodeVersion] {
		p, err := ParsePlatform(platform)
		if err != nil {
			t.Fatalf("nodeDigests key %q: %v", platform, err)
		}
		pin, err := NodePin(p)
		if err != nil {
			// The specific bug this catches: a digest added without the matching `nodePlatform`
			// row, which would otherwise build a URL with an empty platform segment and 404.
			t.Errorf("%s has a digest but does not resolve: %v", platform, err)
			continue
		}
		if strings.Contains(pin.URL, "--") || strings.HasSuffix(pin.URL, "-.tar.gz") {
			t.Errorf("%s produced a URL with an empty segment: %s", platform, pin.URL)
		}
	}
}

// TWO FILES PIN ONE NODE, so this reads the other one.
//
// `runtime/handler/internal/hydrate/artifacts.go` is under `internal/`, which is Go's own way of saying
// the cli module may not import it — so the pin is duplicated, and a duplicated pin drifts. What
// drift COSTS here is specific: the binary would hydrate one Node and the bundle would carry
// another, both correctly checksummed, with nothing anywhere reporting two runtimes.
//
// Parsed off disk rather than compared against a copied constant, because a copied constant is a
// third place to update.
func TestNodePinAgreesWithTheHydratePackage(t *testing.T) {
	const other = "../../../handler/internal/hydrate/artifacts.go"
	src, err := os.ReadFile(other)
	if err != nil {
		t.Skipf("%s is not in this checkout: %v", other, err)
	}

	version := regexp.MustCompile(`NodeVersion\s*=\s*"([^"]+)"`).FindSubmatch(src)
	if version == nil {
		t.Fatalf("%s no longer declares NodeVersion; this test can no longer see the other pin", other)
	}
	if got := string(version[1]); got != NodeVersion {
		t.Errorf("two files pin the Node runtime and they disagree:\n  %s says %s\n  %s says %s\n"+
			"  the appliance would hydrate one and the bundle would carry the other",
			"cli/appliance/bundle/pins.go", NodeVersion, other, got)
	}

	// The digests too, not just the version: a matching version with a mismatched digest is the
	// worse half of the same failure, because both sides verify successfully against different
	// bytes and only one of them is what upstream published.
	rows := regexp.MustCompile(`"(linux|darwin)/(amd64|arm64)":\s*"([0-9a-f]{64})"`).FindAllSubmatch(src, -1)
	if len(rows) == 0 {
		t.Fatalf("%s no longer carries per-platform digests in a shape this test can read", other)
	}
	for _, row := range rows {
		platform := string(row[1]) + "/" + string(row[2])
		theirs := string(row[3])
		ours, ok := nodeDigests[NodeVersion][platform]
		if !ok {
			t.Errorf("%s pins %s and this file does not", other, platform)
			continue
		}
		if ours != theirs {
			t.Errorf("%s digest disagrees:\n  here:  %s\n  there: %s", platform, ours, theirs)
		}
	}
}

// The prune that makes the bundle per-platform depends on this mapping matching the one
// `@temporalio/core-bridge/common.js` uses at require time. Get it wrong and the build deletes
// the binary the runtime is about to ask for.
func TestRustTripleMatchesCoreBridgesOwnNaming(t *testing.T) {
	cases := map[string]string{
		"linux/amd64":  "x86_64-unknown-linux-gnu",
		"linux/arm64":  "aarch64-unknown-linux-gnu",
		"darwin/amd64": "x86_64-apple-darwin",
		"darwin/arm64": "aarch64-apple-darwin",
	}
	for in, want := range cases {
		p, _ := ParsePlatform(in)
		got, err := rustTriple(p)
		if err != nil {
			t.Errorf("%s: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("%s: got %q, core-bridge files it under %q", in, got, want)
		}
	}
	if _, err := rustTriple(Platform{OS: "plan9", Arch: "amd64"}); err == nil {
		t.Error("an unknown OS produced a triple; the prune would then delete every prebuild")
	}
}

func TestParsePlatformRefusesJunk(t *testing.T) {
	for _, in := range []string{"", "linux", "/amd64", "linux/", "   "} {
		if _, err := ParsePlatform(in); err == nil {
			t.Errorf("ParsePlatform(%q) was accepted", in)
		}
	}
	p, err := ParsePlatform(" linux/amd64 ")
	if err != nil || p.String() != "linux/amd64" {
		t.Errorf("ParsePlatform trimmed input: %v %v", p, err)
	}
}

// --- four platforms (issue 17) ------------------------------------------------------------

// THE ACCEPTANCE CRITERION "bundles build for linux/amd64, linux/arm64, darwin/amd64 and
// darwin/arm64", asked of the tables rather than of a build.
//
// Four tables have to agree for a platform to be buildable — a Node digest, Node's own naming for
// its artifact set, the Rust triple `@temporalio/core-bridge` files its prebuilds under, and npm's
// os/cpu/libc vocabulary — and they are four separate maps in two files because each one belongs
// to a different vendor. A platform added to one and forgotten in another fails somewhere specific
// and late: a 404, an empty URL segment, a prune that deletes the binary the runtime wants. This
// walks {@link Platforms} and asks all four, so the failure is here and names the missing map.
func TestEveryShippedPlatformResolvesEveryTable(t *testing.T) {
	shipped := Platforms()
	if len(shipped) != 4 {
		t.Fatalf("the appliance ships four platform artifact sets (PRD decision 7); Platforms() has %d", len(shipped))
	}
	for _, p := range shipped {
		if _, err := NodePin(p); err != nil {
			t.Errorf("%s: %v", p, err)
		}
		if nodePlatform[p.String()] == "" {
			t.Errorf("%s has no entry in nodePlatform, so its download URL would have an empty segment", p)
		}
		if _, err := rustTriple(p); err != nil {
			t.Errorf("%s: %v", p, err)
		}
		osName, cpu, _, err := npmArchitecture(p)
		if err != nil {
			t.Errorf("%s: %v", p, err)
			continue
		}
		if osName == "" || cpu == "" {
			t.Errorf("%s resolved an empty npm architecture (%q/%q); pnpm would resolve the host's tree", p, osName, cpu)
		}
	}
}

// glibc AND musl ARE TWO DIFFERENT PACKAGES, so a linux target that does not say which one it
// wants gets whichever the BUILDING machine runs. Correct by accident on a glibc box and wrong the
// first time CI builds a linux bundle on a macOS runner, where there is no libc to read.
//
// Darwin is the other half of the same rule: its packages declare no libc at all, and filtering on
// a field that does not exist is how a correct build resolves an empty tree.
func TestNpmArchitectureAnswersLibcForLinuxAndWithholdsItForMacOS(t *testing.T) {
	for _, p := range Platforms() {
		_, _, libc, err := npmArchitecture(p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		switch p.OS {
		case "linux":
			if libc != "glibc" {
				t.Errorf("%s asks pnpm for libc %q; the appliance ships glibc builds", p, libc)
			}
		case "darwin":
			if libc != "" {
				t.Errorf("%s asks pnpm for libc %q, and darwin packages declare none", p, libc)
			}
		}
	}
}

func TestNpmArchitectureRefusesWhatPnpmCannotResolve(t *testing.T) {
	if _, _, _, err := npmArchitecture(Platform{OS: "plan9", Arch: "amd64"}); err == nil {
		t.Error("an unknown OS resolved an npm architecture")
	}
	if _, _, _, err := npmArchitecture(Platform{OS: "linux", Arch: "riscv64"}); err == nil {
		t.Error("an unknown arch resolved an npm architecture")
	}
}

// The same half-bump refusal as Node's, for the other program the build fetches. Without it, the
// build's own package manager would be the one artifact in the appliance that floats.
func TestBumpingPnpmWithoutItsDigestFails(t *testing.T) {
	_, err := pnpmPin("99.9.9")
	if err == nil {
		t.Fatal("an unpinned pnpm resolved; the dependency tree would be resolved by unverified bytes")
	}
	for _, want := range []string{"99.9.9", "pnpmDigests", "registry.npmjs.org"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the message does not name %q, so it does not say what to edit:\n%v", want, err)
		}
	}
}

// THE THIRD PLACE ONE PNPM VERSION IS WRITTEN DOWN. `control/orchestrator/package.json`'s `packageManager`
// field is what corepack resolves when a developer types `pnpm`; {@link PnpmVersion} is what the
// BUILD runs. Two mechanisms reading two files is how a bundle gets resolved by a pnpm nobody
// chose — and the symptom is a lockfile-shaped diff appearing under an install that "should have
// changed nothing".
func TestPnpmPinAgreesWithThePackageManagerField(t *testing.T) {
	const other = "../../../backend/package.json"
	src, err := os.ReadFile(other)
	if err != nil {
		t.Skipf("%s is not in this checkout: %v", other, err)
	}
	m := regexp.MustCompile(`"packageManager"\s*:\s*"pnpm@([^"]+)"`).FindSubmatch(src)
	if m == nil {
		t.Fatalf("%s no longer declares a pnpm packageManager; this test can no longer see the other pin", other)
	}
	if got := string(m[1]); got != PnpmVersion {
		t.Errorf("two files name the package manager and they disagree:\n  cli/appliance/bundle/pins.go says %s\n  %s says %s\n"+
			"  the build would resolve the dependency tree with one and a developer with the other",
			PnpmVersion, other, got)
	}
}

// A DIGEST THAT IS NOT A FILENAME. npm's integrity strings are base64, so they carry `/` and `+`;
// used verbatim as a cache entry's name, one artifact would land in a directory nobody created and
// the next build would miss the cache and refetch forever.
func TestCacheNameIsAFilenameForEitherAlgorithm(t *testing.T) {
	node, err := NodePin(Platform{OS: "linux", Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	name, err := node.cacheName()
	if err != nil {
		t.Fatal(err)
	}
	// Unchanged for sha256 pins, so a cache filled before this file learned a second algorithm
	// still hits rather than silently refetching 50 MB.
	if name != node.Digest {
		t.Errorf("a sha256 pin's cache name moved from %q to %q", node.Digest, name)
	}

	pnpm, err := PnpmPin()
	if err != nil {
		t.Fatal(err)
	}
	name, err = pnpm.cacheName()
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(name, "/+=") {
		t.Errorf("an integrity pin's cache name is not a filename: %q", name)
	}
	if !strings.HasPrefix(name, "sha512-") {
		t.Errorf("an integrity pin's cache name does not say which algorithm it is: %q", name)
	}
}

// A pin with no digest is a fetch that verifies nothing, and a pin with two is a claim nobody
// checks both halves of. Both are bugs in the pin table rather than permissive defaults, so both
// are refused before anything reaches the network.
func TestAPinMustCarryExactlyOneDigest(t *testing.T) {
	if _, err := (Pin{Name: "x", Version: "1"}).hasher(); err == nil {
		t.Error("a pin with no digest was accepted")
	}
	both := Pin{Name: "x", Version: "1", Digest: strings.Repeat("a", 64), Integrity: "sha512-AAAA"}
	if _, err := both.hasher(); err == nil {
		t.Error("a pin with two digests was accepted")
	}
	if _, err := (Pin{Name: "x", Version: "1", Integrity: "sha1-AAAA"}).hasher(); err == nil {
		t.Error("an integrity string in an algorithm npm does not publish was accepted")
	}
}

// The digest a pin checks a CACHE HIT with has to be the same notation it checks a download with,
// or every hit reads as a mismatch and the build refetches on every run.
func TestDigestFileSpeaksThePinsOwnNotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "artifact")
	if err := os.WriteFile(path, []byte("kontra"), 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("kontra"))
	byHex := Pin{Name: "x", Version: "1", Digest: hex.EncodeToString(sum[:])}
	got, err := byHex.digestFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != byHex.expected() {
		t.Errorf("a sha256 pin re-derived %q, want %q", got, byHex.expected())
	}

	sum512 := sha512.Sum512([]byte("kontra"))
	bySRI := Pin{Name: "x", Version: "1", Integrity: "sha512-" + base64.StdEncoding.EncodeToString(sum512[:])}
	got, err = bySRI.digestFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != bySRI.expected() {
		t.Errorf("an integrity pin re-derived %q, want %q", got, bySRI.expected())
	}
}

// THE FIFTH PLACE TO FORGET A PLATFORM IS A CI MATRIX, and it is the worst one: a matrix that is
// short by an entry does not fail, it simply never builds that platform, and the release ships
// three files with nothing anywhere saying so.
//
// Read with a regex rather than a YAML parser, deliberately — the cli module has no YAML
// dependency and adding one so a test can read a workflow file would be a strange trade. The same
// approach `TestNodePinAgreesWithTheHydratePackage` takes with the other pin: parse the other
// file off disk, and fail loudly if its shape has moved rather than passing on an empty match.
func TestTheCIMatrixBuildsEveryPlatformTheAppliancePins(t *testing.T) {
	for _, workflow := range []string{"../../../.github/workflows/appliance.yml", "../../../.github/workflows/release.yml"} {
		src, err := os.ReadFile(workflow)
		if err != nil {
			t.Skipf("%s is not in this checkout: %v", workflow, err)
		}
		found := map[string]bool{}
		for _, m := range regexp.MustCompile(`(?m)^\s*-\s*platform:\s*(\S+)\s*$`).FindAllSubmatch(src, -1) {
			found[string(m[1])] = true
		}
		if len(found) == 0 {
			t.Errorf("%s no longer declares a `- platform:` matrix in a shape this test can read; it can no longer see whether CI builds all four", workflow)
			continue
		}
		for _, p := range Platforms() {
			if !found[p.String()] {
				t.Errorf("%s does not build %s.\n"+
					"  A matrix short by one entry does not fail — it silently never builds that platform,\n"+
					"  and the release ships three files with nothing anywhere saying which one is missing", workflow, p)
			}
			delete(found, p.String())
		}
		for extra := range found {
			t.Errorf("%s builds %s, which the appliance does not pin (see Platforms())", workflow, extra)
		}
	}
}
