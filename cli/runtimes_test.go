package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/medmahmoudi26/kontra/cli/internal/trustpolicy"
)

// A runtime reference is the third kind of reference this CLI resolves, after an actor image and a
// Bundle, and the failure it must not have is the one the other two already guard against: resolving
// to something plausible and wrong. A build pinned to the wrong run image produces an actor that
// works on the author's machine and not on a Machine.

func TestADeclarationIsANameAndAMajor(t *testing.T) {
	t.Setenv("KONTRA_RUNTIMES_PREFIX", "")
	name, major, ref, err := parseRuntimeDeclaration("python-browser:1", "127.0.0.1:5000")
	if err != nil {
		t.Fatal(err)
	}
	if name != "python-browser" || major != 1 {
		t.Errorf("got name=%q major=%d", name, major)
	}
	if want := "127.0.0.1:5000/kontra-runtimes/python-browser:1"; ref != want {
		t.Errorf("ref:\n  got  %s\n  want %s", ref, want)
	}
}

func TestAFullyQualifiedReferenceIsNotPrefixed(t *testing.T) {
	// The whole point of §3.4: a fork publishes to its own registry. Prefixing a reference that
	// already names a host would produce `127.0.0.1:5000/kontra-runtimes/registry.example.com/...`,
	// which is a real repository path and resolves to nothing.
	t.Setenv("KONTRA_RUNTIMES_PREFIX", "")
	name, major, ref, err := parseRuntimeDeclaration("registry.example.com/team/runtimes/gpu:2", "127.0.0.1:5000")
	if err != nil {
		t.Fatal(err)
	}
	if name != "gpu" || major != 2 {
		t.Errorf("got name=%q major=%d", name, major)
	}
	if want := "registry.example.com/team/runtimes/gpu:2"; ref != want {
		t.Errorf("ref: got %s want %s", ref, want)
	}
}

func TestTheRuntimesPrefixIsOverridable(t *testing.T) {
	t.Setenv("KONTRA_RUNTIMES_PREFIX", "ghcr.io/acme/rt/")
	_, _, ref, err := parseRuntimeDeclaration("python:1", "127.0.0.1:5000")
	if err != nil {
		t.Fatal(err)
	}
	// Trailing slash trimmed: an operator writing one must not produce a double slash, which some
	// registries accept as a DIFFERENT repository.
	if want := "ghcr.io/acme/rt/python:1"; ref != want {
		t.Errorf("ref: got %s want %s", ref, want)
	}
}

func TestAnInterpreterVersionIsNotAMajor(t *testing.T) {
	// `python:3.12` is the mistake an author will actually make, because every other Python image
	// they have ever used is tagged that way. The refusal has to say where the interpreter version
	// really goes or they will try `3.12` again.
	_, _, _, err := parseRuntimeDeclaration("python:3.12", "127.0.0.1:5000")
	if err == nil {
		t.Fatal("python:3.12 was accepted as a major")
	}
	for _, want := range []string{"MAJOR", ".python-version"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal should mention %q: %v", want, err)
		}
	}
}

func TestAMissingMajorIsRefusedWithTheFix(t *testing.T) {
	_, _, _, err := parseRuntimeDeclaration("python-browser", "127.0.0.1:5000")
	if err == nil {
		t.Fatal("a runtime with no major was accepted")
	}
	// Unpinned would mean "whatever is newest", which is the one thing a build must not resolve to.
	if !strings.Contains(err.Error(), "python-browser:1") {
		t.Errorf("the refusal should name the fix: %v", err)
	}
}

func TestAPathThatIsNotAHostIsRefusedRatherThanPrefixed(t *testing.T) {
	// `myregistry/rt/gpu:2` has a slash but no host, and ociref.json pins that `myregistry` is not a
	// registry host. Prefixing it would silently build against a repository nobody meant.
	_, _, _, err := parseRuntimeDeclaration("myregistry/rt/gpu:2", "127.0.0.1:5000")
	if err == nil {
		t.Fatal("a slashed non-host was accepted")
	}
	if !strings.Contains(err.Error(), "myregistry") {
		t.Errorf("the refusal should name the part it blamed: %v", err)
	}
}

func TestAnEmptyDeclarationIsRefusedHereAndDefaultedAbove(t *testing.T) {
	// parseRuntimeDeclaration refuses it; resolveRuntime supplies the default. Keeping the default
	// out of the parser is what lets the parser be the one place that says what a reference may be.
	if _, _, _, err := parseRuntimeDeclaration("", "127.0.0.1:5000"); err == nil {
		t.Error("an empty declaration was accepted by the parser")
	}
	if got := defaultRuntime("py"); got != "python:1" {
		t.Errorf("python default: got %q", got)
	}
	if got := defaultRuntime("go"); got != "base:1" {
		t.Errorf("go default: got %q — a Go binary needs an OS, not a Python", got)
	}
}

func TestPinnedIsTheDigestForm(t *testing.T) {
	// `pack --run-image` is handed the digest, not the moving major tag: the tag can advance between
	// resolution and the lifecycle's own pull, and a build that silently used a different run image
	// than the one recorded in the catalog is a rebase that can never be detected.
	r := resolvedRuntime{Ref: "reg/kontra-runtimes/python:1", Digest: "sha256:" + strings.Repeat("a", 64)}
	if want := "reg/kontra-runtimes/python:1@sha256:" + strings.Repeat("a", 64); r.Pinned() != want {
		t.Errorf("got %s", r.Pinned())
	}
}

func TestCutLastSplitsOnTheTagAndNotThePort(t *testing.T) {
	// `127.0.0.1:5000/a/b:1.0.0` has two colons and only the second is a tag. Splitting on the first
	// yields host `127.0.0.1` and a "tag" of `5000/a/b:1.0.0`.
	before, after, ok := cutLast("127.0.0.1:5000/a/b:1.0.0", ":")
	if !ok || before != "127.0.0.1:5000/a/b" || after != "1.0.0" {
		t.Errorf("got before=%q after=%q ok=%v", before, after, ok)
	}
	if _, _, ok := cutLast("nocolon", ":"); ok {
		t.Error("a string with no separator reported a split")
	}
}

func TestNextRegistryLink(t *testing.T) {
	// A registry that pages against a client that does not hides the runtimes on page two, and the
	// symptom is "available: python:1" on an install that has four.
	cases := map[string]string{
		`</v2/_catalog?n=200&last=x>; rel="next"`: "/v2/_catalog?n=200&last=x",
		`</v2/_catalog?n=200>; rel="prev"`:        "",
		``:                                        "",
		`</a>; rel="prev", </b?n=1>; rel="next"`:  "/b?n=1",
	}
	for header, want := range cases {
		if got := nextRegistryLink(header); got != want {
			t.Errorf("%q: got %q want %q", header, got, want)
		}
	}
}

// ── THE RUNTIME GOES THROUGH THE TRUST GATE, OR IT IS SAID THAT IT DID NOT ─────────────────────────
//
// A Machine refuses an actor image whose registry is not allowed or whose signature it cannot accept.
// A runtime is the BASE of every actor built on it and nothing was asking the same question, because
// `pack` pulls it and the Warden never sees it as a reference.

func TestAnUnconfiguredTrustPolicyIsANoteAndNotAPass(t *testing.T) {
	// The zero policy admits NOTHING, so gating unconditionally would refuse every build on an install
	// that has not configured trust. Silently skipping would let an operator believe it was checked.
	for _, k := range []string{
		"KONTRA_TRUST_REGISTRIES", "KONTRA_TRUST_UNSIGNED", "KONTRA_TRUST_KEY",
		"KONTRA_TRUST_IDENTITY", "KONTRA_TRUST_ISSUER",
	} {
		t.Setenv(k, "")
	}
	var say strings.Builder
	r := resolvedRuntime{Ref: "127.0.0.1:5000/kontra-runtimes/python:1", Digest: "sha256:" + strings.Repeat("a", 64)}
	if err := admitRuntime(context.Background(), r, &say); err != nil {
		t.Fatalf("an unconfigured policy refused the build: %v", err)
	}
	for _, want := range []string{"NOT checked", "KONTRA_TRUST_REGISTRIES", r.Pinned()} {
		if !strings.Contains(say.String(), want) {
			t.Errorf("the note should mention %q: %s", want, say.String())
		}
	}
}

func TestAnAllowedUnsignedRuntimeIsAdmitted(t *testing.T) {
	// The shipped quickstart posture: kontra's own registry is allowed and its artifacts are accepted
	// unsigned, because nothing signs an actor image yet.
	t.Setenv("KONTRA_TRUST_REGISTRIES", "127.0.0.1:5000")
	t.Setenv("KONTRA_TRUST_UNSIGNED", "127.0.0.1:5000")
	t.Setenv("KONTRA_TRUST_KEY", "")
	t.Setenv("KONTRA_TRUST_IDENTITY", "")
	t.Setenv("KONTRA_TRUST_ISSUER", "")
	r := resolvedRuntime{Ref: "127.0.0.1:5000/kontra-runtimes/python:1", Digest: "sha256:" + strings.Repeat("b", 64)}
	var say strings.Builder
	if err := admitRuntime(context.Background(), r, &say); err != nil {
		t.Fatalf("the install's own registry was refused: %v", err)
	}
	if say.Len() != 0 {
		t.Errorf("a configured policy should say nothing on success: %s", say.String())
	}
}

func TestARuntimeFromAnUnallowedRegistryIsRefused(t *testing.T) {
	// The case the gate exists for: a fork points KONTRA_RUNTIMES_PREFIX somewhere this install does
	// not trust, and that runtime would be layered under every actor on the fleet.
	t.Setenv("KONTRA_TRUST_REGISTRIES", "127.0.0.1:5000")
	t.Setenv("KONTRA_TRUST_UNSIGNED", "127.0.0.1:5000")
	t.Setenv("KONTRA_TRUST_KEY", "")
	t.Setenv("KONTRA_TRUST_IDENTITY", "")
	t.Setenv("KONTRA_TRUST_ISSUER", "")
	r := resolvedRuntime{Ref: "evil.example.com/rt/python:1", Digest: "sha256:" + strings.Repeat("c", 64)}
	err := admitRuntime(context.Background(), r, nil)
	if err == nil {
		t.Fatal("a runtime from a registry this install does not allow was admitted")
	}
	// The refusal has to say WHY it matters, or it reads as a typo in a reference.
	for _, want := range []string{"evil.example.com", "base of every actor"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal should mention %q: %v", want, err)
		}
	}
}

func TestARuntimeThatCannotBeVerifiedIsNotReportedAsUnsigned(t *testing.T) {
	// trustpolicy's own distinction, and the reason it exists: "one is a decision somebody made and
	// the other is a question nobody could ask". An operator told "unsigned" would go and sign an image
	// that would then still be refused.
	t.Setenv("KONTRA_TRUST_REGISTRIES", "127.0.0.1:5000")
	t.Setenv("KONTRA_TRUST_UNSIGNED", "") // a signature is required
	t.Setenv("KONTRA_TRUST_KEY", "")
	t.Setenv("KONTRA_TRUST_IDENTITY", "")
	t.Setenv("KONTRA_TRUST_ISSUER", "")
	r := resolvedRuntime{Ref: "127.0.0.1:5000/kontra-runtimes/python:1", Digest: "sha256:" + strings.Repeat("d", 64)}
	err := admitRuntime(context.Background(), r, nil)
	if err == nil {
		t.Fatal("a runtime requiring a signature was admitted with no verifier configured")
	}
	if errors.Is(err, trustpolicy.ErrUnsigned) {
		t.Errorf("reported as UNSIGNED when the question could not be asked: %v", err)
	}
}
