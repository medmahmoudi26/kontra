package main

// trustpolicy_test.go — the two gates of slice 14, and the ways each one could quietly stop being one.
//
// ═══ EVERY TEST HERE IS A NEGATIVE TEST, AND THAT IS THE POINT ═══
//
// A signed image starting proves nothing: it passes identically against a verifier that returns nil
// unconditionally, against a policy that allows every registry, and against a gate that was deleted.
// So what is asserted is the refusals, and each one carries a positive control in the same run so
// that "it was refused" cannot be "everything is refused".
//
// ═══ COSIGN IS NOT INSTALLED ON THIS BOX ═══
//
// Measured while writing this file: `which cosign` → not found. That makes the "cannot verify" path
// the DEFAULT path here rather than a corner to design for later, and it is why
// `TestAMissingCosignIsARefusalAndNotAPass` is the first test in the file. A verification that
// degrades to always-allow when its tool is missing is `infra/watchdog.sh`, which shipped in this
// repo for years reporting healthy whenever it could not read what it was judging.
//
// The stub below is how the rest is tested. It is a shell script that records its argv and exits with
// whatever it was told to, which is enough to pin the three things kontra owns: that a non-zero exit
// is a refusal, that the reference handed over is the PINNED one, and that the trust root reaches the
// command line. What it deliberately does not test is sigstore — kontra does not implement it and
// must not.
//
// ═══ NOTHING HERE CONTACTS A REGISTRY, STRUCTURALLY ═══
//
// Slice 07 reached ghcr.io from a test helper by accident: a row was padded into a legal reference
// and `runDeploy` dialled two lines past the check under test. The fix was to make the helper
// structurally incapable of it, and the same rule is applied from the start here — every test that
// builds a REAL `cosignVerifier` goes through `loopbackVerifier`, which fails the test if the
// reference is not loopback. A red run stays on this machine.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/medmahmoudi26/kontra/cli/internal/ociref"
	"github.com/medmahmoudi26/kontra/cli/internal/trustpolicy"
	"github.com/medmahmoudi26/kontra/cli/internal/trustpolicy/cosignstub"
)

// --- the tool is missing ---------------------------------------------------------------------------

// A MISSING VERIFIER IS A REFUSAL. This is the single most important assertion in the file.
//
// The failure it prevents is not hypothetical and not novel: `infra/watchdog.sh` judged a Machine
// healthy whenever it could not read the logs it judged from, for years, on every Machine, and looked
// exactly like a working watchdog the entire time. A signature gate that passes when cosign is absent
// is the same program — worse, because "verified" is a word an operator acts on.
//
// AND IT MUST NOT SAY "UNSIGNED". `trustpolicy.ErrUnverifiable` and `trustpolicy.ErrUnsigned` are two different facts
// about two different things: one is about this Machine, one is about the Artifact. An operator told
// "unsigned" would go and sign the image, push it, place it again and be refused identically — which
// is `.scratch/warden/issues/15-*`'s finding, arriving in the gate this slice adds.
func TestAMissingCosignIsARefusalAndNotAPass(t *testing.T) {
	// A PATH WITH NOTHING ON IT, so this test does not depend on cosign being absent from the box —
	// it would otherwise turn green-and-vacuous the day somebody installs it.
	t.Setenv("PATH", t.TempDir())

	p := loopbackPolicy(t, "localhost:5000", "", trustpolicy.Options{Key: "/nonexistent/cosign.pub"})
	_, err := p.Admit(context.Background(), loopbackRef(t, "localhost:5000/bundles/nscheck"))
	if err == nil {
		t.Fatal("an image was admitted with no way to verify its signature — this is the always-allow " +
			"degradation infra/watchdog.sh shipped for years")
	}
	if !errors.Is(err, trustpolicy.ErrUnverifiable) {
		t.Errorf("admit = %v, want trustpolicy.ErrUnverifiable", err)
	}
	if errors.Is(err, trustpolicy.ErrUnsigned) {
		t.Errorf("a Machine that cannot ASK reported the image as unsigned, so an operator would go and "+
			"sign it and be refused again identically:\n%v", err)
	}
	// The remedy has to be about this Machine, not about the Artifact.
	if !strings.Contains(err.Error(), "not installed") {
		t.Errorf("the refusal does not say what is missing from this Machine:\n%v", err)
	}

	// THE POSITIVE CONTROL, in the same run and through the same code path: with a cosign on PATH that
	// exits 0, the identical reference is admitted. Without this the assertion above holds against a
	// policy that refuses everything, which is the shape of a guard that guards nothing.
	cosignstub.Stub(t, 0, "")
	if _, err := p.Admit(context.Background(), loopbackRef(t, "localhost:5000/bundles/nscheck")); err != nil {
		t.Fatalf("with a cosign that verifies, the same reference was still refused: %v", err)
	}
}

// --- the tool says no ------------------------------------------------------------------------------

// AN UNSIGNED IMAGE IS REFUSED, AND THE REFUSAL NAMES WHY. The slice's first "Done when".
//
// A NON-ZERO EXIT IS THE WHOLE CONTRACT, and cosign's text is quoted rather than parsed. Branching on
// its wording would be a promise about a binary this repo does not version; what is asserted is that
// kontra treats "cosign said no" as a refusal and repeats what it said.
func TestAnUnsignedImageIsRefusedAndTheRefusalNamesWhy(t *testing.T) {
	cosignstub.Stub(t, 1, "Error: no matching signatures")

	p := loopbackPolicy(t, "localhost:5000", "", trustpolicy.Options{Key: "/keys/release.pub"})
	ref := loopbackRef(t, "localhost:5000/bundles/nscheck")
	digest, err := p.Admit(context.Background(), ref)
	if err == nil {
		t.Fatalf("an unsigned image was admitted as %q", digest)
	}
	if !errors.Is(err, trustpolicy.ErrUnsigned) {
		t.Fatalf("admit = %v, want trustpolicy.ErrUnsigned", err)
	}
	if errors.Is(err, trustpolicy.ErrUnverifiable) {
		t.Errorf("cosign ran and answered, so this is a fact about the Artifact and not about the "+
			"Machine; reporting both leaves a caller unable to branch:\n%v", err)
	}
	// WHY, in the three parts an operator needs: which image, what it was checked against, and what
	// the tool said. A refusal that names none of them is `invalid reference format` in a new place.
	for _, want := range []string{"bundles/nscheck", "/keys/release.pub", "no matching signatures"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q:\n%v", want, err)
		}
	}

	// THE CONTROL: the same policy, the same reference, a cosign that exits 0 — admitted. So the
	// refusal above is about the exit status and not about the reference, the policy or the stub.
	cosignstub.Stub(t, 0, "")
	if _, err := p.Admit(context.Background(), ref); err != nil {
		t.Fatalf("with a cosign that verifies, the same image was refused: %v", err)
	}
}

// COSIGN IS ASKED ABOUT THE DIGEST, NEVER THE TAG, and about the root the operator named.
//
// THE TAG IS A TIME-OF-CHECK/TIME-OF-USE GAP WIDE ENOUGH TO DRIVE AN IMAGE THROUGH. Verifying
// `repo:1.0.0` proves something about whatever that tag pointed at when cosign looked; podman pulls a
// moment later and a tag is a mutable binding (driver_podman.go's whole reason for `ociref.ErrImageUnpinned`).
// The two would agree almost always, which is what makes it the kind of bug that ships.
func TestCosignIsAskedAboutThePinnedReferenceAndTheConfiguredRoot(t *testing.T) {
	argv := cosignstub.Stub(t, 0, "")

	const digest = "sha256:d216f83bc7887c5dc03bb07853f4dd28c0694a5a72509d575f83386cb602da40"
	p := loopbackPolicy(t, "localhost:5000", "", trustpolicy.Options{
		Identity: "https://github.com/acme/actors/.github/workflows/release.yml@refs/heads/main",
		Issuer:   "https://token.actions.githubusercontent.com",
	})
	if _, err := p.Admit(context.Background(), "localhost:5000/bundles/nscheck:0.9.9@"+digest); err != nil {
		t.Fatalf("admit: %v", err)
	}

	got := argv(t)
	for _, want := range []string{
		"verify",
		"--certificate-identity",
		"https://github.com/acme/actors/.github/workflows/release.yml@refs/heads/main",
		"--certificate-oidc-issuer",
		"https://token.actions.githubusercontent.com",
		"localhost:5000/bundles/nscheck@" + digest,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("cosign was not given %q; it was run as:\n%s", want, got)
		}
	}
	// THE TAG MUST NOT BE THE THING VERIFIED. `:0.9.9` was in the reference kontra was handed and the
	// repository was rebuilt by the shared grammar (`ociref.Ref.Repository()`), so it must be gone.
	if strings.Contains(got, "0.9.9") {
		t.Errorf("cosign was asked about a TAG, which can point somewhere else by the time podman "+
			"pulls:\n%s", got)
	}
	// A KEYLESS POLICY MUST NOT ALSO PASS A KEY, or cosign resolves two roots by argument order.
	if strings.Contains(got, "--key") {
		t.Errorf("a keyless policy passed --key as well:\n%s", got)
	}
}

// --- the trust root -------------------------------------------------------------------------------

// A ROOT THE ATTACKER ALSO CONTROLS IS WORSE THAN NO ROOT, because it reads as safety.
//
// `cosign verify --certificate-identity x` with no issuer accepts a signature from ANY OIDC provider
// that ever issued a certificate for a subject spelled `x`, and anyone can stand one up. `cosign
// verify --certificate-oidc-issuer https://token.actions.githubusercontent.com` with no identity
// accepts a signature from EVERY repository on GitHub. Both configurations run, both print
// "Verified OK", and neither is a check — so both are refused at load rather than at use.
func TestATrustRootThatProvesNothingIsRefusedAtLoad(t *testing.T) {
	for _, k := range []struct {
		why  string
		opts trustpolicy.Options
		says string
	}{
		{
			why:  "an identity with no issuer: anyone can obtain a certificate for a subject",
			opts: trustpolicy.Options{Registries: "ghcr.io", Identity: "https://github.com/acme/actors/.github/workflows/release.yml@refs/heads/main"},
			says: "ANY OIDC provider",
		},
		{
			why:  "an issuer with no identity: every workload that issuer vouches for, which is all of GitHub",
			opts: trustpolicy.Options{Registries: "ghcr.io", Issuer: "https://token.actions.githubusercontent.com"},
			says: "EVERY workload",
		},
		{
			why:  "a key AND an identity: two roots, resolved silently by argument order",
			opts: trustpolicy.Options{Registries: "ghcr.io", Key: "/keys/release.pub", Identity: "someone"},
			says: "two answers to one question",
		},
		{
			why:  "a registry that requires a signature and no root at all to check it against",
			opts: trustpolicy.Options{Registries: "ghcr.io"},
			says: "no trust root",
		},
	} {
		_, err := trustpolicy.Load(k.opts)
		if err == nil {
			t.Errorf("trustpolicy.Load accepted a root that proves nothing: %s", k.why)
			continue
		}
		if !strings.Contains(err.Error(), k.says) {
			t.Errorf("%s: the refusal does not say %q:\n%v", k.why, k.says, err)
		}
	}

	// THE CONTROLS. Both complete roots load, or every assertion above holds against a function that
	// refuses every configuration — which would be a Warden that can never place anything.
	for _, k := range []trustpolicy.Options{
		{Registries: "ghcr.io", Key: "/keys/release.pub"},
		{Registries: "ghcr.io", Identity: "acme", Issuer: "https://token.actions.githubusercontent.com"},
		// …and a Machine whose whole allowlist is unsigned needs no root, because nothing will ask.
		{Registries: "localhost:5000", Unsigned: "localhost:5000"},
	} {
		if _, err := trustpolicy.Load(k); err != nil {
			t.Errorf("trustpolicy.Load(%+v) refused a complete configuration: %v", k, err)
		}
	}
}

// AN EXCEPTION TO A RULE THAT DOES NOT EXIST IS A LINE THAT DOES NOTHING, and the operator who wrote
// it believes their images are being accepted from that registry. They are not — the allowlist refuses
// them one gate earlier — so the diagnosis is a pull failure with no obvious cause.
func TestAnUnsignedExceptionMustNameAnAllowlistedRegistry(t *testing.T) {
	_, err := trustpolicy.Load(trustpolicy.Options{Registries: "localhost:5000", Unsigned: "ghcr.io"})
	if err == nil {
		t.Fatal("--trust-unsigned accepted a registry that is not on --trust-registries")
	}
	if !strings.Contains(err.Error(), "ghcr.io") || !strings.Contains(err.Error(), trustpolicy.RegistriesEnv) {
		t.Errorf("the refusal does not name the entry and the list it is missing from:\n%v", err)
	}
	// The control: the same entry, spelled the same way, on both lists.
	if _, err := trustpolicy.Load(trustpolicy.Options{Registries: "localhost:5000", Unsigned: "localhost:5000"}); err != nil {
		t.Errorf("an exception naming an allowlisted registry was refused: %v", err)
	}
}

// A POLICY WITH NO VERIFIER AT ALL MUST NOT BE A PASS. This is the branch a nil check gets wrong: a
// `trustpolicy.Policy` built by hand — by a test, by a future caller, by a driver constructed with a partial
// literal — has a nil Verifier, and `if p.Verifier != nil { verify }` reads as correct and admits
// everything.
func TestAPolicyWithNoVerifierRefusesRatherThanSkipping(t *testing.T) {
	p := trustpolicy.Policy{Rules: []trustpolicy.Rule{{Host: "localhost:5000"}}}
	_, err := p.Admit(context.Background(), loopbackRef(t, "localhost:5000/bundles/nscheck"))
	if !errors.Is(err, trustpolicy.ErrUnverifiable) {
		t.Fatalf("admit = %v, want trustpolicy.ErrUnverifiable — a nil verifier is 'the question was never asked', "+
			"never 'the answer was yes'", err)
	}
	// The control: the same rule marked unsigned admits, so the refusal is about the missing verifier
	// and not about the rule.
	p.Rules[0].Unsigned = true
	if _, err := p.Admit(context.Background(), loopbackRef(t, "localhost:5000/bundles/nscheck")); err != nil {
		t.Fatalf("a registry the operator marked unsigned was still refused: %v", err)
	}
}

// A SPEC WITH NO IMAGE MUST STILL SAY SO. `admit` is the door the podman driver now goes through, and
// the first version of it called `ociref.Parse` directly — which reported an empty spec as "the empty
// string is not a repository path", sending an operator to look for a typo in a field that is blank.
// The refusal has to name the Target, because the Warden's mistake is upstream of the reference.
func TestASpecWithNoImageIsRefusedAsAMissingImage(t *testing.T) {
	p, err := trustpolicy.Load(trustpolicy.Options{Registries: "localhost:5000", Unsigned: "localhost:5000"})
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"", "   "} {
		_, err := p.Admit(context.Background(), ref)
		if err == nil {
			t.Fatalf("admit(%q) accepted a spec with no Image", ref)
		}
		if !strings.Contains(err.Error(), "needs an Image and this spec has none") {
			t.Errorf("admit(%q) does not say the spec has no Image at all:\n%v", ref, err)
		}
	}
}

// THE ZERO VALUE ADMITS NOTHING. Every `&podmanDriver{…}` literal in this package that forgets a
// policy gets this one, and it must start no Workers rather than start every one.
func TestTheZeroTrustPolicyAdmitsNothing(t *testing.T) {
	var p trustpolicy.Policy
	for _, ref := range []string{
		loopbackRef(t, "localhost:5000/bundles/nscheck"),
		loopbackRef(t, "localhost/probe"),
		loopbackRef(t, "127.0.0.1:5000/a/b"),
	} {
		if _, err := p.Admit(context.Background(), ref); !errors.Is(err, trustpolicy.ErrRegistryNotAllowed) {
			t.Errorf("the zero policy admitted %q (%v)", ref, err)
		}
	}
	// AND IT SAYS SO, rather than reporting an empty list as though a list had been configured. A
	// Machine that places nothing looks identical to a Fleet nobody has placed anything on.
	_, err := p.Admit(context.Background(), loopbackRef(t, "localhost:5000/bundles/nscheck"))
	if !strings.Contains(err.Error(), "no trust policy is configured") {
		t.Errorf("the refusal does not say the policy is unconfigured:\n%v", err)
	}
	if !strings.Contains(strings.Join(p.Describe(), "\n"), "NO registry at all") {
		t.Errorf("describe() does not say the allowlist is empty:\n%s", strings.Join(p.Describe(), "\n"))
	}
}

// --- the cost of each gate --------------------------------------------------------------------------

// BenchmarkTrustGatesCostNothing is the measurement cli/internal/trustpolicy/trustpolicy.go's header cites for "steps 1–3
// cost nothing measurable and reach nothing".
//
// IT IS A BENCHMARK RATHER THAN A SENTENCE because the claim it backs is an ordering decision — the
// cheap refusal is the early one — and an ordering justified by an unmeasured intuition is the kind
// that gets reversed in a refactor.
//
// TWO ARMS, AND THE SPLIT BETWEEN THEM IS THE INTERESTING PART. The `refused` arm is dominated by
// `fmt.Errorf` building a nine-line sentence, not by the match, which is why the two differ by an
// order of magnitude — and it is still the right thing to measure, because a refusal that is not
// formatted is a refusal an operator cannot act on.
func BenchmarkTrustGatesCostNothing(b *testing.B) {
	p, err := trustpolicy.Load(trustpolicy.Options{Registries: "localhost:5000", Unsigned: "localhost:5000"})
	if err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	const pinned = "localhost:5000/bundles/nscheck@sha256:d216f83bc7887c5dc03bb07853f4dd28c0694a5a72509d575f83386cb602da40"

	// The three gates on a reference that passes all of them: what admitting costs.
	b.Run("admitted", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := p.Admit(ctx, pinned); err != nil {
				b.Fatal(err)
			}
		}
	})
	// …and on one the allowlist refuses: what NOT contacting a registry costs.
	b.Run("refused", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_, _ = p.Admit(ctx, "ghcr.io/acme/bundles/nscheck:0.1.0")
		}
	})
}

// --- helpers ----------------------------------------------------------------------------------------

// loopbackRef is the structural guard slice 07 added after the fact and this file has from the start:
// a test that builds a real `cosignVerifier` can only name a reference on this box.
//
// IT FAILS THE TEST RATHER THAN SKIPPING, because the hazard is a REGRESSION reaching the network, not
// a missing dependency. Slice 07's helper sent an anonymous `GET https://ghcr.io/v2/` from this box
// when a row it could not express was padded into one it could; the assertion that would have caught
// it is this one.
func loopbackRef(t *testing.T, repo string) string {
	t.Helper()
	r := ociref.Split(repo)
	if !ociref.LoopbackHost(r.Domain) {
		t.Fatalf("this test would name %q, which is not on this box — a test whose FAILURE mode is an "+
			"outbound request to a third party is not a test to leave in a suite", repo)
	}
	return repo + "@sha256:d216f83bc7887c5dc03bb07853f4dd28c0694a5a72509d575f83386cb602da40"
}

// loopbackPolicy builds a policy whose every allowlist entry is on this box.
func loopbackPolicy(t *testing.T, registries, unsigned string, root trustpolicy.Options) trustpolicy.Policy {
	t.Helper()
	for _, e := range strings.Split(registries, ",") {
		r, err := trustpolicy.ParseRule(e)
		if err != nil {
			t.Fatalf("trustpolicy.ParseRule(%q): %v", e, err)
		}
		if !ociref.LoopbackHost(r.Host) {
			t.Fatalf("this test would allowlist %q, which is not on this box", e)
		}
	}
	root.Registries, root.Unsigned = registries, unsigned
	p, err := trustpolicy.Load(root)
	if err != nil {
		t.Fatalf("trustpolicy.Load(%+v): %v", root, err)
	}
	return p
}

// stubCosign puts a `cosign` on PATH that records its argv and exits with `code`, and returns a
// function that reads the recording back.
