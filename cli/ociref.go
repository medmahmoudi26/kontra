package main

// ociref.go — THE ONE ANSWER to "can this string name an **Artifact**", for every site that names
// one.
//
// ═══ WHY THIS FILE EXISTS AND IS NOT THREE REGEXPS ═══
//
// kontra names **Actors** more freely than OCI names repositories. `shared/conformance/queues.json` pins
// `a/b`, `my actor` and `café` as names that must keep working and states why — "A QUEUE NAME IS
// NOT SANITISED… A derivation that sanitised the queue would route to a queue nobody polls, which
// is silent." Temporal accepts all three; the OCI grammar accepts one. So the set of nameable
// **Actors** is strictly larger than the set of nameable **Artifacts**, which ADR 0036 records as a
// consequence it deliberately does not close.
//
// THE COST OF THAT GAP IS NOT THE REFUSAL, IT IS THE DIAGNOSIS. `.scratch/warden/issues/15-*`
// measured one error message serving two opposite truths:
//
//	a/b     "does not hold a/b:1.0.0 … Either it was never deployed…"   TRUE — deploying fixes it
//	café    IDENTICAL                                                   FALSE — nothing fixes it
//
// An operator with a `café` actor was told to deploy, forever. The issue's requirement is exactly
// the thing this file is: "One shared answer, consulted by build, pull and the driver — proven by
// breaking it in one place and watching all three change."
//
// ═══ THE SITES, AND WHAT EACH ADDS ═══
//
//	cli/build.go     `--push <ref>`   the DESTINATION an Artifact is published to (slice 07)
//	cli/deploy.go    runDeploy        the Image reference `kontra deploy` tags and pushes
//	cli/scale.go     pullFailure      the reference `kontra serve --mode docker` handed docker
//	cli/driver_podman.go  imageDigest the reference a **Machine**'s runtime is handed (slice 02)
//
// The issue counted three and there were four: the push half of `deploy` had been folded into
// "deploy/pull" and had the same bug, refusing `café` only after a full image build and then as a
// "push/pull address mismatch". No total is written down anywhere — shared/conformance/README.md calls a
// count in a comment "the least reliable kind of documentation there is" — the corpus driver's list
// is the truth.
//
// Each one appends its own CONSEQUENCE — where the string came from and what, if anything, the
// operator can do — and none of them re-derives the grammar. A second answer to "what is a legal
// repository" is how a build succeeds and the push it authorised is then rejected by the registry.
//
// ═══ IT PARSES THE REFERENCE, NEVER THE ACTOR'S NAME ═══
//
// Issue 15 names the trap in the obvious fix: **there are two references in play and they are not
// the same string.**
//
//	localhost:5000/café:0.1.0    the REMOTE form, what `kontra serve --mode docker` pulls
//	kontra/café-worker:0.1.0     the LOCAL form — different prefix AND suffix
//
// A check aimed at the actor's NAME, or at either form alone, passes and lets the pull die later on
// podman's or docker's own `invalid reference format` — the same bug moved one step earlier, which
// is worse than leaving it. So every entry point here takes a whole reference, and each call site
// passes the exact string its runtime is about to receive.
//
// ═══ WHAT IS NOT JUDGED HERE ═══
//
// The DIGEST. `localhost/nscheck@md5:…` and `…@sha256:<63 hex>` are refusals about *pinning*, which
// is a rule the podman driver owns (a tag is a mutable binding; two Machines in one **Fleet** would
// not be running the same code) and which `kontra build --push` deliberately does not share — a
// build's destination must NOT be digest-pinned, because the digest is what the push computes. Two
// sites, two opposite rules about the same field, so the field is left to them.

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ═══ THE TWO REFUSALS, BECAUSE THERE ARE TWO OPPOSITE TRUTHS ═══
//
//	errImageUnpinned         the reference is WELL FORMED but names a tag. Pinning it fixes it.
//	errImageUnrepresentable  the reference is not an OCI reference AT ALL. Nothing fixes it.
//
// THE SECOND IS NOT A TYPO, IT IS A CLASS OF ACTOR — see this file's header. They are sentinels
// rather than strings because the thing a caller has to be able to do is TELL THEM APART
// (`errors.Is`), and a message is not a thing you can branch on.
var (
	errImageUnpinned        = errors.New("image is not digest-pinned")
	errImageUnrepresentable = errors.New("image reference is not expressible in the OCI grammar")
)

// The OCI reference grammar, transcribed from the distribution spec.
//
// The asymmetry between the first two is the whole point and is not a mistake in either: a DOMAIN
// is a hostname and may carry uppercase, a PATH COMPONENT may not — which is why
// `localhost:5000/Foo` fails on `Foo` and not on `localhost`.
//
// The TAG is the one this file added, and it is not decoration: `shared/conformance/queues.json` carries
// `1:2` as an adversarial VERSION, which Temporal accepts verbatim and which cannot be a tag at all
// — `<registry>/bundles/nscheck:1:2` reads back as repository `bundles/nscheck:1`, tag `2`, so
// without this rule the refusal would blame the wrong half of the string.
var (
	ociPathComponent = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*$`)
	ociDomain        = regexp.MustCompile(`^[a-zA-Z0-9](?:[a-zA-Z0-9-]*[a-zA-Z0-9])?(?:\.[a-zA-Z0-9](?:[a-zA-Z0-9-]*[a-zA-Z0-9])?)*(?::[0-9]+)?$`)
	ociDigest        = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	ociTag           = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}$`)
)

// ociRef is a reference taken APART, without judgement. Splitting and judging are separate because
// a refusal has to name WHICH part is wrong — "invalid reference format" is the message this whole
// file exists to stop being the answer.
type ociRef struct {
	// Domain is the registry. EMPTY IS A REAL AND LEGAL VALUE and it means Docker Hub: `nscheck`
	// and `kontra/nscheck-worker` are perfectly good references that resolve there. That is fine
	// for a LOCAL image and fatal for a push destination, which is why the two are separate checks.
	Domain string
	// Path is the repository path with the domain removed: `bundles/nscheck`, `a/b`.
	Path string
	// Tag, and whether one was written at all. A reference with no tag means `:latest`; a reference
	// ending in a bare `:` is a different thing and has to be refused rather than read as neither.
	Tag    string
	TagSet bool
	// Digest is whatever followed `@`, VERBATIM and unjudged — `md5:…` and a 63-hex sha256 both land
	// here. See this file's header for why the judgement belongs to the call sites.
	Digest string
}

// splitOCIRef takes a reference apart. It never fails: every string is *some* shape, and saying
// which part of it is wrong needs the parts.
func splitOCIRef(ref string) ociRef {
	var r ociRef
	rest := strings.TrimSpace(ref)

	// The digest first, at the FIRST `@`: everything after it is opaque here, so a second `@`
	// inside it is the digest's problem and not the repository's.
	if repo, digest, ok := strings.Cut(rest, "@"); ok {
		rest, r.Digest = repo, digest
	}

	// THE TAG IS THE LAST `:` THAT HAS NO `/` AFTER IT, which is what keeps the `:5000` in
	// `localhost:5000/nscheck` from being mistaken for one.
	if i := strings.LastIndex(rest, ":"); i >= 0 && !strings.Contains(rest[i+1:], "/") {
		r.Tag, r.TagSet = rest[i+1:], true
		rest = rest[:i]
	}

	// THE FIRST COMPONENT IS A DOMAIN ONLY IF IT LOOKS LIKE ONE — it carries a dot or a port, or it
	// is literally `localhost`. That is the registry's own disambiguation rule and not a heuristic
	// invented here; without it `a/b` would be read as host `a` and would wrongly pass a hostname
	// check, which is the same class of error as failing it.
	if i := strings.Index(rest, "/"); i >= 0 && looksLikeRegistryHost(rest[:i]) {
		r.Domain, rest = rest[:i], rest[i+1:]
	}
	r.Path = rest
	return r
}

// looksLikeRegistryHost is the distribution spec's disambiguation between "this first component is
// a registry" and "this first component is the first segment of a repository path". It is a
// question about SHAPE, not about reachability.
func looksLikeRegistryHost(s string) bool {
	return strings.ContainsAny(s, ".:") || s == "localhost"
}

// repository is the reference's name with no tag and no digest: what `/v2/<here>/manifests/…`
// addresses.
func (r ociRef) repository() string {
	if r.Domain == "" {
		return r.Path
	}
	return r.Domain + "/" + r.Path
}

// tagged is the reference put back together — `<domain>/<path>:<tag>`, and just the repository when
// no tag was written. It is here rather than on bundleArtifact so that a reference an OPERATOR typed
// and one kontra derived print identically; a refusal that quoted the operator's string for one and
// a rebuilt string for the other would be two spellings of one address.
func (r ociRef) tagged() string {
	if !r.TagSet {
		return r.repository()
	}
	return r.repository() + ":" + r.Tag
}

// check is THE judgement. It is on the parts rather than on the string so that the refusal can name
// the part, and it is one method rather than one per site so that breaking it breaks every site at
// once — which is `.scratch/warden/issues/15-*`'s acceptance criterion, stated as a test in
// ociref_conformance_test.go.
func (r ociRef) check(ref string) error {
	if r.Domain != "" && !ociDomain.MatchString(r.Domain) {
		return ociRefRefusal("registry host", r.Domain, ref)
	}
	if r.Path == "" {
		return ociRefRefusal("repository path", r.Path, ref)
	}
	for _, p := range strings.Split(r.Path, "/") {
		if !ociPathComponent.MatchString(p) {
			return ociRefRefusal("repository path component", p, ref)
		}
	}
	if r.TagSet && !ociTag.MatchString(r.Tag) {
		return ociRefRefusal("tag", r.Tag, ref)
	}
	return nil
}

// parseOCIRef is split-then-judge, and it is what a caller with a STRING uses.
func parseOCIRef(ref string) (ociRef, error) {
	r := splitOCIRef(ref)
	if err := r.check(ref); err != nil {
		return r, err
	}
	return r, nil
}

// checkOCIRef is parseOCIRef for a caller that only wants the verdict — the shape most call sites
// use, since each one already has the reference it is about to hand to something else.
func checkOCIRef(ref string) error {
	_, err := parseOCIRef(ref)
	return err
}

// ociRefRefusal is THE SENTENCE, written once.
//
// Every word of it is load-bearing and was chosen against the two failures issue 15 measured. It
// names the part rather than the whole string, because "invalid reference format" is exactly the
// answer that sent an operator to look for a typo in the wrong half. It states the OTHER grammar —
// Temporal's — because the reader's actor demonstrably works, and a refusal that does not explain
// why the working thing is refused reads as a bug in kontra. And it closes the door explicitly:
// NO REBUILD, REDEPLOY OR PIN. Those are the three remedies the surrounding messages offer, all of
// them unreachable here, and an operator who is not told so will try all three.
//
// WHAT IT MUST NOT CONTAIN is a remedy. Each call site adds the one true consequence for its own
// path, and `driver_podman_test.go` asserts that the pinning remedy never reaches this class of
// refusal.
func ociRefRefusal(part, bad, ref string) error {
	return fmt.Errorf("%w: %s %q in %q.\n"+
		"  kontra names Actors more freely than OCI names repositories — shared/conformance/queues.json pins\n"+
		"  `a/b`, `my actor` and `café` as names that must keep working, and Temporal accepts all three.\n"+
		"  An OCI path component is lowercase alphanumeric separated by `.`, `_` or `-`; a tag is\n"+
		"  [A-Za-z0-9_][A-Za-z0-9._-]{0,127}; a registry host is a hostname with an optional :port.\n"+
		"  NO REBUILD, REDEPLOY OR PIN CHANGES THIS — the string itself cannot name an Artifact.",
		errImageUnrepresentable, part, bad, ref)
}
