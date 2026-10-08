// image.go — the three helpers that turn an image reference into something runnable.
//
// THEY CAME FROM TWO PLACES THAT SHOULD NOT HAVE OWNED THEM. `ImageRef` and `PinnedDigest` were in
// `driver_podman.go` and `PushTransport` in `bundle.go`, and the trust policy called all three —
// so verifying a signature reached into a container driver and into the bundle builder. They are
// about an OCI reference and nothing else, so they live with the rest of that grammar.
package ociref

import (
	"fmt"
	"strings"
)

// ImageRef is the spec's Image taken apart by the SHARED grammar, plus the two things that are this
// driver's own: the empty spec, and the consequence sentence for a reference no image driver can use.
//
// IT IS THE ONE DOOR, AND `trust.Policy.admit` COMES THROUGH IT TOO. Both of the additions below are
// easy to leave out of a second caller and neither failure is loud: a spec with no Image at all would
// be reported as "the empty string is not a repository path", and an unnameable one would arrive
// without the sentence that says which driver CAN run that Actor. Found by writing `admit` against
// `Parse` directly and losing the first of the two.
func ImageRef(ref string) (Ref, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return Ref{}, fmt.Errorf("a podman Worker needs an Image and this spec has none " +
			"(ADR 0036: one Target, and it is a container)")
	}

	// THE GRAMMAR IS ASKED ONCE, AND NOT HERE. `Parse` splits the tag off before judging the
	// repository — or every tagged reference would be reported as ungrammatical, the loudest possible
	// wrong answer, since it tells an operator their Actor can never run when all they had to do was
	// pin it. What this site adds is THE CONSEQUENCE, which is the shape cli/go's header
	// describes ("Each one appends its own CONSEQUENCE… and none of them re-derives the grammar").
	r, err := Parse(ref)
	if err != nil {
		return Ref{}, fmt.Errorf("%w\n"+
			"  If that string carries the Actor's name, this Actor cannot be placed by `podman` at all;\n"+
			"  `process` is the driver that runs it, from source, producing no Artifact and needing no\n"+
			"  reference", err)
	}
	return r, nil
}

// PinnedDigest is THE PINNING RULE, which is this driver's alone and is not in cli/go for the
// reason recorded there: a `--push` destination refuses the very digest this requires, so the field
// is left to the two sites that hold opposite rules about it.
func PinnedDigest(r Ref, ref string) (string, error) {
	digest, pinned := r.Digest, r.Digest != ""
	if !pinned || !Digest.MatchString(digest) {
		return "", fmt.Errorf("%w: %q — podman Workers run `<repo>@sha256:<64 hex>` and nothing else, "+
			"because a tag is a mutable binding and two Machines in one Fleet would not be running the "+
			"same code (ADR 0032: the digest is the identity, the tag is the convenience).\n"+
			"  the repository itself is fine; `kontra build --push` prints the digest to pin",
			ErrImageUnpinned, strings.TrimSpace(ref))
	}
	return digest, nil
}

// PushTransport takes the scheme off a `--push` reference and decides whether the registry is spoken
// to over plain HTTP, returning the decision as a function of the host so the caller can apply it
// once the host is known.
//
// ═══ A --push DESTINATION IS HTTPS BY DEFAULT, AND registryHost'S IS NOT ═══
//
// The asymmetry is deliberate and it is the day `registryHost`'s comment was written for: "an
// explicit `https://` is honoured, because the day a Bundle is pushed to somebody else's registry is
// slice 07's." Two different addresses, two different defaults:
//
//	the CONVENTIONAL address    kontra's own registry — the Controller's :5000, the install's bound
//	                            port. Plain HTTP by construction, anonymous on the VPC, and it is what
//	                            every address in this system has always been. registryHost keeps that.
//	a --push DESTINATION        somebody else's, reached over the internet. Defaulting it to plain HTTP
//	                            would silently downgrade every push to ghcr, GitLab or Harbor — and a
//	                            downgrade on the ONE request that carries an Artifact out of the box is
//	                            not a convenience, it is a credential and an artifact in the clear.
//
// LOOPBACK IS THE EXCEPTION AND IS NAMED RATHER THAN GUESSED. `localhost:5000` is the single-box
// convenience ADR 0036 explicitly keeps, and there is no TLS on it; a private-range heuristic would
// be a third rule to be wrong about, so an operator with a plain-HTTP registry on a VPC writes
// `http://` and says so.
func PushTransport(ref string) (bare string, plain func(domain string) bool) {
	switch {
	case strings.HasPrefix(ref, "https://"):
		return strings.TrimPrefix(ref, "https://"), func(string) bool { return false }
	case strings.HasPrefix(ref, "http://"):
		return strings.TrimPrefix(ref, "http://"), func(string) bool { return true }
	default:
		return ref, LoopbackHost
	}
}

// LoopbackHost is "this box", for the one case that has no TLS and needs none.
func LoopbackHost(domain string) bool {
	h := domain
	if i := strings.LastIndex(h, ":"); i >= 0 {
		h = h[:i]
	}
	return h == "localhost" || h == "127.0.0.1" || h == "::1" || h == "[::1]" || strings.HasPrefix(h, "127.")
}

// ImageDigest returns the digest of a digest-pinned reference, or refuses with the reason that is
// actually true.
//
// A TAG IS NOT A WEAKER PIN, IT IS A DIFFERENT THING. ADR 0032 decided one identity field carrying
// `sha256:<hex>` and that "nothing parses it, nothing branches on which Target produced it"; ADR 0036
// keeps it as what makes the reversal cheap. A tag is a NAME with a mutable binding — two Machines in
// one **Fleet** that pull `nscheck:0.1.0` an hour apart can be running different code, and every
// downstream fact (which digest a version currently names, what a Run recorded) becomes a claim about
// when you looked. deploy.go says it already: "THE DIGEST IS THE IDENTITY (ADR 0032); the tag above is
// the convenience."
//
// So this refuses rather than resolves. Resolving a tag here would put the moment-of-lookup inside the
// driver, which is the same bug wearing a helpful face.
//
// IT PARSES THE REFERENCE THAT REACHES THE RUNTIME, NOT THE ACTOR'S NAME. There are two references in
// play for one Actor and they are not the same string — `localhost:5000/café:0.1.0` remotely and
// `kontra/café-worker:0.1.0` locally, differing in BOTH prefix and suffix — so a check aimed at the
// name, or at either form alone, passes and lets the pull die later with podman's own wording. What
// arrives here is `spec.Image`, which is the exact string handed to `podman run`, so that is the
// string checked.
// IT IS TWO HALVES SO THAT TRUST POLICY CAN SIT BETWEEN THEM. `cli/internal/trustpolicy/trustpolicy.go` runs the
// allowlist after the grammar and before the pin, and it does that by calling the same two functions
// this one does rather than by re-deriving either. See `trustpolicy.Policy.admit` for why the allowlist goes
// in the middle: it is the refusal whose remedy a pin cannot reach.
func ImageDigest(ref string) (string, error) {
	r, err := ImageRef(ref)
	if err != nil {
		return "", err
	}
	return PinnedDigest(r, ref)
}
