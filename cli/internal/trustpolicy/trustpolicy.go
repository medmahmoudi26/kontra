package trustpolicy

// trustpolicy.go — WHOSE bytes a **Machine** will run, and from WHERE.
//
// Digest-pinning (cli/warden/driver_podman.go, `ociref.ErrImageUnpinned`) already answers *"are these the bytes
// that were there before"*. It cannot answer *"who produced them"*: a digest computed over an
// attacker's image is a perfectly valid digest, and a **Warden** told to place it will place it
// forever, immutably, exactly as asked. This file is the other half — a registry allowlist and a
// signature — and ADR 0036 is why it is needed at all: "actor code runs in a container everywhere it
// runs", so what a Machine pulls is a stranger's code by construction.
//
// ═══ THE ORDER IS THE DESIGN, AND IT IS ORDERED BY COST ═══
//
// An allowlist is a question about a STRING. A signature is a question about BYTES that must first
// be fetched. That asymmetry is not a detail, it is the whole reason there are two gates and not one,
// and it fixes the order:
//
//	1. the grammar     cli/internal/ociref/ociref.go        pure string, no syscall        can this even name an Artifact
//	2. the allowlist   allow()              pure string, no syscall        may this Machine pull from there
//	3. the pin         ociref.PinnedDigest()       pure string, no syscall        is this a mutable pointer
//	4. the signature   Verifier    N registry round trips + crypto   who produced these bytes
//	   ── and only then ──
//	5. the pull        `podman run`         the layers. Measured below.
//
// MEASURED ON THIS BOX, not reasoned about, because the ordering is the design and an ordering
// justified by intuition is the kind that gets reversed in a refactor:
//
//	gates 1-3, admitted        3.8 - 8.1 µs   BenchmarkTrustGatesCostNothing/admitted
//	gates 1-3, refused         4.8 - 10.5 µs  …/refused — the extra is fmt.Errorf building the sentence
//	ONE signature request      15 - 21 ms     GET of a `<digest>.sig` manifest, local registry, loopback
//	a 20 MiB pull              0.40 - 0.54 s  same registry, same loopback, cold
//	a real actor image         19 s           commit 208a6e0, and ADR 0036 sizes one at ~900 MB
//
// Three orders of magnitude between the string gates and the CHEAPEST possible signature request, and
// five between them and the cheapest possible pull. (The µs figures are wide because this box has two
// cores and other work on it; the gap they are being used to argue is not close.)
//
// SO A NON-ALLOWLISTED REGISTRY IS NEVER CONTACTED — which is a security property before it is a
// saving. The DNS lookup and the TLS ClientHello a pull makes are themselves a signal to whoever owns
// that name: a **Machine**'s address, its clock, and the fact that something told it to go there. An
// allowlist that fired after the request would leak all three on every refusal.
//
// AND AN UNSIGNED IMAGE IS NECESSARILY REFUSED LATE. There is no way around it: the signature lives
// in the registry, so learning that it is absent costs a request. What this file does is make that
// request the SMALL one — cosign reads a signature manifest and a ~1 KiB payload blob, not the layers
// — so an unsigned image is refused before its bytes land on the Machine's disk.
//
// ═══ WHAT A SIGNATURE PROVES HERE, AND WHAT IT DOES NOT ═══
//
// It is worth being exact, because "verified" is a word that reads as safety whether or not it is.
//
// WHAT IT PROVES. That the digest this Machine is about to run was signed by a key, or by a workload
// identity, that the OPERATOR NAMED IN THIS MACHINE'S OWN CONFIGURATION — and by nothing else. With
// `--trust-key`, that is a public key on this Machine's disk. With `--trust-identity` +
// `--trust-issuer`, it is a Fulcio certificate whose SAN is that identity and whose OIDC issuer is
// that issuer, which is what "keyless signing via CI OIDC" in the slice means: a GitHub Actions
// workflow signs with an ephemeral key bound to its own repository and ref, and the certificate says
// which workflow it was.
//
// WHAT IT DOES NOT PROVE, and each of these has bitten somebody:
//
//   - IT IS NOT PROOF THE CODE IS GOOD. It says who built it. A signed backdoor verifies.
//   - IT IS ONLY AS GOOD AS THE ROOT. `cosign verify` with no `--certificate-identity` accepts a
//     signature from ANY identity Fulcio ever issued a certificate to, which is everyone. That is
//     why an identity with no issuer, or an issuer with no identity, is refused at CONFIG LOAD here
//     rather than accepted and quietly widened — a verification against a root the attacker also
//     controls is worse than no verification, because it reads as safety.
//   - IT IS NOT PROOF ABOUT THE TRANSPARENCY LOG. This file shells out to `cosign` and inherits
//     whatever Rekor policy that binary applies. It does not implement sigstore and must not: rolling
//     one's own signature verification is the same class of mistake as rolling one's own TLS.
//   - THE POLICY IS LOCAL TO THE MACHINE. It comes from this Warden's flags and environment, so
//     anyone who is already root on the Machine can rewrite it. This defends against a compromised
//     or substituted REGISTRY, and against a control plane that asks for the wrong image. It does not
//     defend against a compromised Machine, and nothing on a Machine can. Delivering the policy in
//     the enrolment credential (slice 08 mints one; it does not carry this) is the change that would,
//     and it is not made here.
//
// ═══ THE ZERO VALUE REFUSES EVERYTHING, ON PURPOSE ═══
//
// `Policy{}` admits nothing. That is the single most important line in this file, because the
// failure this repo has already shipped is the opposite one: `infra/watchdog.sh` degraded to
// always-healthy whenever it could not read what it was judging, for years, and looked fine the whole
// time. A gate whose unconfigured state is "allow" is not a gate; it is a comment.
//
// So there is no default allowlist, `newPodmanDriver` cannot be constructed without a policy, and
// every way of failing to verify — cosign missing, cosign failing, no verifier configured at all —
// lands on a REFUSAL. The cost of that choice is that a Warden with no trust configuration places no
// Workers, which is loud, immediate, and says exactly what to set.
//
// ═══ IT EXTENDS cli/internal/ociref/ociref.go, IT DOES NOT ANSWER FOR ITSELF ═══
//
// `.scratch/warden/issues/15-*` measured what four sites answering "what is a legal reference" cost,
// and cli/internal/ociref/ociref.go is the one answer they now share. Policy asks a DIFFERENT question, but it asks
// it of the SAME field — "which registry is this?" — and a second derivation of that field is a
// bypass rather than a mere inconsistency:
//
//	strings.HasPrefix(ref, "localhost:5000")   admits localhost:5000.evil.com/x
//	strings.Split(ref, "/")[0]                 admits a/b as though `a` were a registry
//
// Both look right. So nothing here splits a reference: `ociref.Ref.Domain` and `ociref.Ref.Path` are the only
// inputs to the match, and the ALLOWLIST ENTRY is checked against the same `ociref.LooksLikeRegistryHost`,
// `ociref.Domain` and `ociref.PathComponent` the grammar is built from. Widening any of those three moves
// this file's verdicts too — `shared/conformance/ociref.json`'s `allow` section is where that is a test
// rather than a claim.

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/medmahmoudi26/kontra/cli/internal/cliutil"
	"github.com/medmahmoudi26/kontra/cli/internal/ociref"
)

// ═══ THE THREE REFUSALS THIS FILE ADDS, AND WHY THEY ARE THREE ═══
//
//	ErrRegistryNotAllowed  the registry is not one this Machine pulls from. Costs nothing to find out,
//	                       and no request was made. Remedy: allowlist it, or push to one already on it.
//	ErrUnsigned       the registry was allowed and ASKED — one signature-manifest request, not
//	                       the layers — and no signature this Machine accepts was found. Remedy: sign
//	                       it, with the key or identity the policy names.
//	ErrUnverifiable        the question could not be ASKED — cosign is not installed, or the policy
//	                       names no root. THIS IS NOT "unsigned" AND MUST NOT COLLAPSE INTO IT: one is
//	                       a fact about the Artifact and one is a fact about this Machine, and an
//	                       operator told "unsigned" would go and sign an image that would then be
//	                       refused identically.
//
// Sentinels rather than strings for cli/internal/ociref/ociref.go's reason: what a caller has to be able to do is tell
// them apart with `errors.Is`, and a message is not a thing you can branch on.
var (
	ErrRegistryNotAllowed = errors.New("image registry is not on this Machine's allowlist")
	ErrUnsigned           = errors.New("image carries no signature this Machine accepts")
	ErrUnverifiable       = errors.New("this Machine cannot verify a signature")
)

// Rule is ONE allowlist entry, taken apart: a registry, and optionally how far into it.
//
// AN ENTRY IS A REGISTRY, AND A REGISTRY IS NOT A NAMESPACE. `ghcr.io` on the allowlist admits every
// account on ghcr.io, including one an attacker opened this morning — which is exactly why the
// signature gate is not optional and why `Unsigned` has to be stated per entry rather than globally.
// The path prefix narrows it (`ghcr.io/acme`), and it is matched COMPONENT-WISE: a `strings.HasPrefix`
// on the path would let `ghcr.io/acme` admit `ghcr.io/acme-evil/x`, which is a repository anyone can
// create.
type Rule struct {
	// Host is the registry as `ociref.Ref.Domain` spells it — `ghcr.io`, `localhost:5000`. Compared with
	// EqualFold because a hostname is case-insensitive and `ociref.Domain` permits uppercase in it (the
	// asymmetry cli/internal/ociref/ociref.go documents); the port is part of the string and is compared exactly,
	// because :5000 and :5001 are two registries.
	Host string

	// Path is the entry's path components, empty for a whole-registry entry.
	Path []string

	// Unsigned says the operator explicitly accepted UNSIGNED images from this registry.
	//
	// IT IS PER-ENTRY AND HAS NO GLOBAL SPELLING, deliberately. `localhost:5000` on a single box is a
	// registry the operator both writes to and reads from, and requiring a signature there would make
	// the first thing anyone does with this feature be to turn it off — which is how a control becomes
	// decoration. Naming it per registry keeps the blast radius at one address, keeps it in the line
	// the Warden prints on startup, and keeps `ghcr.io` from inheriting the exception.
	Unsigned bool
}

// Policy is what this Machine will run. THE ZERO VALUE ADMITS NOTHING — see this file's header.
type Policy struct {
	// Rules is the allowlist. Empty means no registry is allowed, which is the safe reading of "not
	// configured" and the only one that does not silently become "all of them".
	Rules []Rule

	// Verifier answers "who produced these bytes". NIL IS NOT "no signature required" — a rule that
	// requires a signature and a policy with no verifier is ErrUnverifiable, because the alternative
	// is a nil check that reads as a pass.
	Verifier Verifier
}

// Verifier is the seam an implementation of "who signed this" plugs into. An interface so
// that a test can drive `admit` without a signing infrastructure, and so that the ONE production
// implementation — `cosignVerifier` — is a value the Warden prints rather than a branch inside a
// function.
type Verifier interface {
	// verify returns nil only if the digest is signed by the root this verifier pins. Any other
	// outcome is an error, and the two that matter are told apart: ErrUnsigned (asked, answered
	// no) and ErrUnverifiable (could not ask).
	Verify(ctx context.Context, repo, digest string) error

	// root is what this verifier pins to, in one line, for the refusal and for the Warden's startup
	// log. An operator who cannot see the root has no way to judge whether "verified" means anything.
	Root() string
}

// --- the allowlist --------------------------------------------------------------------------------

// ParseRule reads one allowlist entry, USING THE SHARED GRAMMAR AND NOT A SECOND ONE.
//
// It does not call `ociref.Split`, and that is not an oversight: an entry is a registry and a registry
// is not a reference. `ociref.Split("localhost:5000")` reads `5000` as a TAG and `localhost` as a
// repository path, correctly, because that string IS a tagged Docker Hub reference when it appears
// where a reference is expected. What is shared is the three rules the grammar is made of —
// `ociref.LooksLikeRegistryHost`, `ociref.Domain`, `ociref.PathComponent` — so widening any of them widens this too,
// which `shared/conformance/ociref.json`'s `allow` section pins.
//
// A HOST WITH NO DOT AND NO PORT IS NOT A HOST, and refusing it here is the same rule
// `pushDestination` applies to `KONTRA_REGISTRY=myregistry`: `myregistry` names a Docker Hub USER.
// An allowlist entry that silently meant Docker Hub would be the widest possible entry wearing the
// narrowest possible name.
func ParseRule(entry string) (Rule, error) {
	e := strings.TrimSpace(entry)
	if e == "" {
		return Rule{}, errors.New("an empty allowlist entry names no registry")
	}
	// A scheme is stripped by the same function that strips it for a push destination, so an operator
	// who wrote `http://10.124.0.2:5000` in one place and the other means the same thing in both.
	e, _ = ociref.PushTransport(e)
	e = strings.TrimSuffix(e, "/")

	host, rest, _ := strings.Cut(e, "/")
	if !ociref.LooksLikeRegistryHost(host) {
		return Rule{}, fmt.Errorf("allowlist entry %q does not name a registry: %q has no dot and no "+
			"port and is not `localhost`, so a reference beginning with it resolves to a DOCKER HUB user "+
			"of that name.\n"+
			"  An entry is `<host>[:<port>][/<path prefix>]` — `ghcr.io`, `ghcr.io/acme`, `localhost:5000`",
			entry, host)
	}
	if !ociref.Domain.MatchString(host) {
		return Rule{}, fmt.Errorf("allowlist entry %q: %w", entry, ociref.Refusal("registry host", host, entry))
	}

	r := Rule{Host: host}
	if rest != "" {
		for _, p := range strings.Split(rest, "/") {
			if !ociref.PathComponent.MatchString(p) {
				return Rule{}, fmt.Errorf("allowlist entry %q: %w", entry,
					ociref.Refusal("repository path component", p, entry))
			}
			r.Path = append(r.Path, p)
		}
	}
	return r, nil
}

// String is the entry as an operator wrote it, rebuilt — so the Warden's startup line and every
// refusal quote the same spelling.
func (r Rule) String() string {
	if len(r.Path) == 0 {
		return r.Host
	}
	return r.Host + "/" + strings.Join(r.Path, "/")
}

// matches asks whether a reference falls inside this rule. It reads `ociref.Ref`'s fields and derives
// nothing — see this file's header for the two hand-rolled versions of this that are bypasses.
func (r Rule) matches(ref ociref.Ref) bool {
	// AN EMPTY DOMAIN IS NOT A MISSING FIELD, IT IS DOCKER HUB (cli/internal/ociref/ociref.go), and no rule matches it.
	// Left un-normalised on purpose: mapping "" to `docker.io` would need docker's own `library/`
	// insertion rule to be right about the path, which would be a second answer to a question the
	// grammar deliberately does not answer. `allow` says so in the refusal instead.
	if ref.Domain == "" || !strings.EqualFold(ref.Domain, r.Host) {
		return false
	}
	if len(r.Path) == 0 {
		return true
	}
	got := strings.Split(ref.Path, "/")
	if len(got) < len(r.Path) {
		return false
	}
	for i, want := range r.Path {
		// COMPONENT-WISE, NOT A PREFIX OF THE STRING. `ghcr.io/acme` must not admit `ghcr.io/acme-evil/x`.
		if got[i] != want {
			return false
		}
	}
	return true
}

// allow is GATE 2: may this Machine pull from there at all. Pure string work, no syscall, no request.
//
// It returns the rule that matched, because the NEXT gate needs it — whether a signature is required
// here is a property of the entry, not of the policy.
//
// IT TAKES THE PARSED REFERENCE AND THE OPERATOR'S STRING, and quotes the second. The judgement is on
// the first, always — that is the whole anti-drift property — but a refusal that quoted a REBUILT
// reference would print a different address than the one in the Warden's assignment, and cli/internal/ociref/ociref.go
// already records why those must read identically: "a refusal that quoted the operator's string for
// one and a rebuilt string for the other would be two spellings of one address."
func (p Policy) allow(ref ociref.Ref, raw string) (Rule, error) {
	for _, r := range p.Rules {
		if r.matches(ref) {
			return r, nil
		}
	}
	where := ref.Domain
	if where == "" {
		where = "no registry at all, which means DOCKER HUB"
	}
	return Rule{}, fmt.Errorf("%w: %s (from %q).\n"+
		"  This Machine pulls from %s and nothing was contacted — the allowlist is a string check and it\n"+
		"  runs before anything is fetched, so a refused registry never learns this Machine exists.\n"+
		"  Either publish the Artifact to a registry on the list, or add this one to the Warden's\n"+
		"  --trust-registries (KONTRA_TRUST_REGISTRIES). An entry is a REGISTRY, so `ghcr.io` admits every\n"+
		"  account on ghcr.io; `ghcr.io/<org>` is the narrower thing you probably mean.",
		ErrRegistryNotAllowed, where, strings.TrimSpace(raw), p.registriesLine())
}

// registriesLine is the allowlist as one readable phrase, and it says "nothing" out loud rather than
// printing an empty list — an unconfigured policy is the state this file most needs to be legible in.
func (p Policy) registriesLine() string {
	if len(p.Rules) == 0 {
		return "NO registry at all (no trust policy is configured on this Machine)"
	}
	names := make([]string, 0, len(p.Rules))
	for _, r := range p.Rules {
		s := r.String()
		if r.Unsigned {
			s += " (unsigned accepted)"
		}
		names = append(names, s)
	}
	return strings.Join(names, ", ")
}

// --- the whole judgement --------------------------------------------------------------------------

// admit is the ONE ENTRY POINT a driver calls before it pulls anything, and it returns the digest the
// caller was going to need anyway.
//
// THE ORDER IS THE POINT AND IS ARGUED IN THIS FILE'S HEADER. One ordering decision inside it is
// worth stating separately, because both gates are free and cost cannot choose between them:
//
//	THE ALLOWLIST IS CHECKED BEFORE THE PIN.
//
// A reference that is both unpinned AND from a refused registry gets `ErrRegistryNotAllowed`. The
// other order would tell the operator to pin it — a remedy that WORKS, produces a new reference, and
// then fails again on the registry. That is issue 15's finding in a new place: the refusal to give is
// the one whose remedy actually gets you somewhere, and "pin it" does not when the address is wrong.
func (p Policy) Admit(ctx context.Context, ref string) (string, error) {
	// 1. THE GRAMMAR — the shared answer, asked once, by everyone (cli/internal/ociref/ociref.go), through the driver's
	// own door so that the empty spec and the consequence sentence are not lost. Calling `ociref.Parse`
	// here directly was the first version of this line and it dropped the first of those two silently.
	r, err := ociref.ImageRef(ref)
	if err != nil {
		return "", err
	}

	// 2. THE ALLOWLIST — a string, and the last gate before anything could be contacted.
	rule, err := p.allow(r, ref)
	if err != nil {
		return "", err
	}

	// 3. THE PIN — this driver's own rule, still a string.
	digest, err := ociref.PinnedDigest(r, ref)
	if err != nil {
		return "", err
	}

	// 4. THE SIGNATURE — the first gate that costs a request.
	if rule.Unsigned {
		return digest, nil
	}
	if p.Verifier == nil {
		return "", fmt.Errorf("%w: %s requires a signature and this Warden has no verifier configured "+
			"at all.\n"+
			"  This is NOT the same as an unsigned image: the question was never asked. Configure a trust\n"+
			"  root (--trust-key, or --trust-identity with --trust-issuer), or mark %s as\n"+
			"  --trust-unsigned if that registry really is one this Machine trusts without proof.",
			ErrUnverifiable, rule, rule)
	}
	if err := p.Verifier.Verify(ctx, r.Repository(), digest); err != nil {
		return "", err
	}
	return digest, nil
}

// describe is the two lines a **Warden** prints on startup and `warden status` prints on demand. It
// exists because a policy nobody can read is a policy nobody can check, and because the state this
// file most needs to be loud about — nothing configured, therefore nothing placeable — is otherwise
// indistinguishable from a quiet Fleet.
//
// TWO LINES RATHER THAN ONE FORMATTED STRING, so that each caller supplies its own left margin
// instead of one of them string-replacing the other's.
func (p Policy) Describe() []string {
	root := "none configured — every registry above must be marked (unsigned accepted) or nothing starts"
	if p.Verifier != nil {
		root = p.Verifier.Root()
	}
	return []string{"registries " + p.registriesLine(), "signatures " + root}
}

// --- cosign ------------------------------------------------------------------------------------

// cosignVerifier shells out to `cosign verify`.
//
// SHELLING OUT IS THE DESIGN, NOT A SHORTCUT. sigstore verification is a certificate chain, a
// transparency-log inclusion proof and a signed timestamp; an in-process reimplementation of it would
// be a second, worse answer to a question with a maintained one, and the failure mode of a subtly
// wrong verifier is a green "verified" — the exact thing this file's header calls worse than nothing.
// So kontra owns the POLICY (which registries, which identity) and cosign owns the CRYPTO.
//
// THE COST OF THAT IS A DEPENDENCY, AND THE DEPENDENCY IS NOT INSTALLED ON THE BOX THIS WAS WRITTEN
// ON. `cosign` was absent from PATH here, so the "cannot verify" path is not a hypothetical to be
// designed for later — it is the path this machine takes, and it is a REFUSAL. See ErrUnverifiable.
type cosignVerifier struct {
	// bin is the cosign executable name or path. A field so a test can point at a stub that behaves
	// like cosign — including by not existing.
	bin string

	// Exactly one of these two is set; `newCosignVerifier` refuses both and neither.
	key      string // --key: a public key on this Machine's disk
	identity string // --certificate-identity: the SAN of the Fulcio certificate that signed
	issuer   string // --certificate-oidc-issuer: which OIDC provider vouched for that identity
}

var _ Verifier = cosignVerifier{}

// newCosignVerifier builds the verifier, and REFUSES EVERY CONFIGURATION THAT WOULD VERIFY AGAINST A
// ROOT THAT PROVES NOTHING.
//
// Three refusals, and the middle one is the one that matters:
//
//	neither key nor identity     nothing to verify against
//	identity without issuer      `cosign verify --certificate-identity x` with no issuer accepts a
//	   (or issuer without         signature from ANY OIDC provider that ever issued a certificate for
//	    identity)                 a subject spelled `x`. Anyone can obtain one; the check reads as a
//	                              check and is not one. THIS IS THE FAILURE THIS SLICE IS ABOUT.
//	both key and identity        two answers to one question, silently resolved by argument order —
//	                              the same shape `pushDestination` refuses for --push and --registry.
func newCosignVerifier(key, identity, issuer string) (cosignVerifier, error) {
	v := cosignVerifier{bin: "cosign", key: strings.TrimSpace(key),
		identity: strings.TrimSpace(identity), issuer: strings.TrimSpace(issuer)}
	switch {
	case v.key == "" && v.identity == "" && v.issuer == "":
		return cosignVerifier{}, errors.New("no trust root: a signature is only as good as what it is " +
			"checked against.\n" +
			"  --trust-key <public key>                            a key you hold, or\n" +
			"  --trust-identity <san> --trust-issuer <oidc issuer> keyless, signed by CI")
	case v.key != "" && (v.identity != "" || v.issuer != ""):
		return cosignVerifier{}, fmt.Errorf("--trust-key %q and --trust-identity/--trust-issuer are two "+
			"answers to one question. Pass one: a key, or a keyless identity", v.key)
	case v.key == "" && v.identity != "" && v.issuer == "":
		return cosignVerifier{}, fmt.Errorf("--trust-identity %q with no --trust-issuer would accept a "+
			"signature from ANY OIDC provider that has ever issued a certificate for that subject, and "+
			"anyone can obtain one.\n"+
			"  That is not a weaker check, it is no check that reads as one. Name the issuer:\n"+
			"    --trust-issuer https://token.actions.githubusercontent.com", v.identity)
	case v.key == "" && v.identity == "" && v.issuer != "":
		return cosignVerifier{}, fmt.Errorf("--trust-issuer %q with no --trust-identity would accept a "+
			"signature from EVERY workload that issuer vouches for — every repository on GitHub, in the "+
			"case of Actions.\n  Name the identity that is allowed to sign for this Machine", v.issuer)
	}
	return v, nil
}

func (v cosignVerifier) Root() string {
	if v.key != "" {
		return "cosign --key " + v.key
	}
	return fmt.Sprintf("cosign keyless, identity %s issued by %s", v.identity, v.issuer)
}

// verify runs `cosign verify` against the PINNED reference and refuses on anything but success.
//
// IT IS HANDED `<repo>@<digest>` AND NEVER THE TAG. The digest is what gate 3 established and what the
// container will run; verifying a tag would verify whatever that tag pointed at at the moment cosign
// looked, which can differ from what podman pulls a second later — a time-of-check/time-of-use gap
// wide enough to drive an image through. `repo` comes from `ociref.Ref.Repository()` so the string is
// rebuilt by the shared grammar rather than sliced here.
//
// A MISSING BINARY IS ErrUnverifiable AND NOT A PASS, and the LookPath is done here rather than only
// at load time because a Warden runs for weeks and the thing it depends on can be removed under it.
// It costs one stat of a cached PATH entry per start, against a pull; it is not a cost worth
// optimising into a stale answer.
func (v cosignVerifier) Verify(ctx context.Context, repo, digest string) error {
	bin, err := exec.LookPath(v.bin)
	if err != nil {
		return fmt.Errorf("%w: %s is not installed on this Machine, so %s@%s cannot be checked against "+
			"%s.\n"+
			"  This is a REFUSAL and not a warning: a verification that degrades to always-allow is the\n"+
			"  failure this gate exists to prevent. Install cosign on the Machine, or mark the registry\n"+
			"  --trust-unsigned and accept what that means.", ErrUnverifiable, v.bin, repo, digest, v.Root())
	}

	args := []string{"verify"}
	if v.key != "" {
		args = append(args, "--key", v.key)
	} else {
		args = append(args, "--certificate-identity", v.identity, "--certificate-oidc-issuer", v.issuer)
	}
	// `--quiet` keeps the verified payload off stdout; the exit status is the answer and the output is
	// only ever quoted into a refusal.
	args = append(args, "--quiet", repo+"@"+digest)

	out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput()
	if err == nil {
		return nil
	}
	// COSIGN'S OWN TEXT IS QUOTED, TRUNCATED, AND NOT PARSED. Branching on its wording would be a
	// promise about a binary this repo does not version; what is asserted is only that a non-zero exit
	// is a refusal. The distinction that IS made is the one a caller can act on: a cosign that failed
	// to RUN at all (a permission problem, a corrupt binary) is unverifiable, and a cosign that ran and
	// said no is unsigned.
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return fmt.Errorf("%w: %s could not be run (%v), so %s@%s was not checked against %s",
			ErrUnverifiable, bin, err, repo, digest, v.Root())
	}
	return fmt.Errorf("%w: %s@%s is not signed by %s.\n  cosign: %s\n"+
		"  A digest proves the bytes did not change; it does not say who made them. Sign the Artifact\n"+
		"  with the root above, or mark the registry --trust-unsigned and accept what that means.",
		ErrUnsigned, repo, digest, v.Root(), cliutil.FirstLine(string(out)))
}

// --- configuration ----------------------------------------------------------------------------

// Options is the operator's five inputs, before any of them is judged. A struct so that the
// flags on `kontra warden serve` and the environment variables behind them are one set rather than
// two lists that drift.
type Options struct {
	Registries string // --trust-registries / KONTRA_TRUST_REGISTRIES
	Unsigned   string // --trust-unsigned   / KONTRA_TRUST_UNSIGNED
	Key        string // --trust-key        / KONTRA_TRUST_KEY
	Identity   string // --trust-identity   / KONTRA_TRUST_IDENTITY
	Issuer     string // --trust-issuer     / KONTRA_TRUST_ISSUER
}

// The environment names, beside the struct they fill, so the refusals below can quote them.
const (
	RegistriesEnv = "KONTRA_TRUST_REGISTRIES"
	UnsignedEnv   = "KONTRA_TRUST_UNSIGNED"
	KeyEnv        = "KONTRA_TRUST_KEY"
	IdentityEnv   = "KONTRA_TRUST_IDENTITY"
	IssuerEnv     = "KONTRA_TRUST_ISSUER"
)

// Load turns the operator's five strings into the policy, and REFUSES A CONFIGURATION THAT
// WOULD NOT MEAN WHAT IT SAYS.
//
// The refusals are at LOAD rather than at start, so a Warden that cannot enforce what it was asked for
// says so once, at the moment an operator is watching, instead of once per placement in a log nobody
// is reading. Every one of them is also re-checked at `admit` — a policy value can be built by hand,
// and a check that only exists at the boundary is a check the next caller walks around.
//
// AN EMPTY CONFIGURATION IS NOT AN ERROR HERE. It produces the zero policy, which admits nothing, and
// the Warden prints that. Refusing to START would be the wrong shape: `warden status`, telemetry and
// the pane stream are all still worth having on a Machine that has been told to place nothing.
func Load(o Options) (Policy, error) {
	var p Policy

	entries, err := parseTrustList(o.Registries)
	if err != nil {
		return Policy{}, fmt.Errorf("--trust-registries (%s): %w", RegistriesEnv, err)
	}
	p.Rules = entries

	unsigned, err := parseTrustList(o.Unsigned)
	if err != nil {
		return Policy{}, fmt.Errorf("--trust-unsigned (%s): %w", UnsignedEnv, err)
	}
	// AN EXCEPTION MUST NAME SOMETHING THE RULE ALREADY ALLOWS. `--trust-unsigned ghcr.io` with
	// `ghcr.io` absent from the allowlist is not a widening — it is a line the operator believes is
	// doing something, doing nothing, which is the shape of every dead security setting ever written.
	for _, u := range unsigned {
		matched := false
		for i := range p.Rules {
			if strings.EqualFold(p.Rules[i].Host, u.Host) && samePath(p.Rules[i].Path, u.Path) {
				p.Rules[i].Unsigned, matched = true, true
			}
		}
		if !matched {
			return Policy{}, fmt.Errorf("--trust-unsigned names %q, which is not on --trust-registries "+
				"(%s = %q).\n"+
				"  An exception to a rule that does not exist is a line that does nothing. Add %q to the\n"+
				"  allowlist first, spelled the same way", u, RegistriesEnv, o.Registries, u)
		}
	}

	// A TRUST ROOT IS REQUIRED IF, AND ONLY IF, SOMETHING ACTUALLY NEEDS ONE. A Machine whose entire
	// allowlist is marked unsigned has made a choice this file disagrees with and states plainly; it
	// has not made a mistake, and demanding a key it will never use would be ceremony.
	needsRoot := false
	for _, r := range p.Rules {
		if !r.Unsigned {
			needsRoot = true
		}
	}
	rootSet := o.Key != "" || o.Identity != "" || o.Issuer != ""
	if !needsRoot && !rootSet {
		return p, nil
	}
	v, err := newCosignVerifier(o.Key, o.Identity, o.Issuer)
	if err != nil {
		return Policy{}, fmt.Errorf("%w\n  (%s, %s, %s)", err, KeyEnv, IdentityEnv, IssuerEnv)
	}
	p.Verifier = v
	return p, nil
}

// parseTrustList splits an operator's list — commas or whitespace, because both get typed — and parses
// every entry. It refuses the WHOLE list if any entry is bad rather than dropping the bad one: a
// silently discarded allowlist entry is a Machine that refuses a registry the operator believes is
// allowed, diagnosed as a network problem.
func parseTrustList(s string) ([]Rule, error) {
	var out []Rule
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' }) {
		r, err := ParseRule(f)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

func samePath(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
