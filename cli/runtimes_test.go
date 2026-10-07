package main

import (
	"strings"
	"testing"
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
		``:                                       "",
		`</a>; rel="prev", </b?n=1>; rel="next"`: "/b?n=1",
	}
	for header, want := range cases {
		if got := nextRegistryLink(header); got != want {
			t.Errorf("%q: got %q want %q", header, got, want)
		}
	}
}
