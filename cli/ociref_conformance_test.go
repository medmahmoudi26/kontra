package main

// ociref_conformance_test.go — `shared/conformance/ociref.json`, driven from the ONE implementation and
// from all THREE sites that consult it.
//
// The acceptance criterion in `.scratch/warden/issues/15-*` is not "there is a shared function". It
// is: *"One shared answer, consulted by build, pull and the driver — proven by breaking it in one
// place and watching all three change."* `TestOneAnswerReachesAllThreeSitesThatNameAnArtifact` is
// that proof: it drives `cli/build.go`, `cli/scale.go` and `cli/driver_podman.go` over the same
// corpus rows and asserts each one carries the shared sentence. Editing the grammar in
// `cli/ociref.go` moves all three; adding a fourth bespoke message fails the row rather than passing
// quietly.
//
// MEASURED RED, each of the four drivers below, by breaking exactly one thing:
//
//	ociPathComponent widened to allow `é`      → verdict, pin, push AND the three-sites test fail
//	the tag rule deleted                        → the `1.0.0+build.7` and `:` rows fail
//	pullFailure's check removed                 → the three-sites test fails on the pull site alone,
//	                                              with `café` told to re-run `kontra deploy` again
//
// See shared/conformance/README.md §"Adding one" for the rules this driver obeys: every case carries a
// `why`, the inputs that BREAK are in the corpus rather than only the easy ones, and the driver
// asserts the corpus is not empty and still holds its interesting inputs.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/image"
)

type ociRefCorpus struct {
	Why   string `json:"why"`
	Split struct {
		Cases []struct {
			Why    string `json:"why"`
			Ref    string `json:"ref"`
			Domain string `json:"domain"`
			Path   string `json:"path"`
			Tag    string `json:"tag"`
			TagSet bool   `json:"tagSet"`
			Digest string `json:"digest"`
		} `json:"cases"`
	} `json:"split"`
	Verdict struct {
		Cases []struct {
			Why     string `json:"why"`
			Ref     string `json:"ref"`
			Verdict string `json:"verdict"`
			Part    string `json:"part"`
			Bad     string `json:"bad"`
		} `json:"cases"`
	} `json:"verdict"`
	Pin struct {
		Cases []struct {
			Why     string `json:"why"`
			Ref     string `json:"ref"`
			Verdict string `json:"verdict"`
			Digest  string `json:"digest"`
		} `json:"cases"`
	} `json:"pin"`
	Push struct {
		Actor struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"actor"`
		Cases []struct {
			Why       string `json:"why"`
			Ref       string `json:"ref"`
			Verdict   string `json:"verdict"`
			Expect    string `json:"expect"`
			PlainHTTP bool   `json:"plainHTTP"`
		} `json:"cases"`
	} `json:"push"`
	// Allow is slice 14's section: the Machine's trust policy, asked of the SAME `ociRef` fields the
	// grammar above produces. It is here rather than in a corpus of its own because a second split of
	// a reference is not an inconsistency, it is a bypass — see the section's own `why`.
	Allow struct {
		Rules struct {
			Cases []struct {
				Why     string   `json:"why"`
				Entry   string   `json:"entry"`
				Verdict string   `json:"verdict"`
				Host    string   `json:"host"`
				Path    []string `json:"path"`
			} `json:"cases"`
		} `json:"rules"`
		Cases []struct {
			Why        string   `json:"why"`
			Allow      []string `json:"allow"`
			Unsigned   []string `json:"unsigned"`
			Ref        string   `json:"ref"`
			Verdict    string   `json:"verdict"`
			Repository string   `json:"repository"`
			Digest     string   `json:"digest"`
		} `json:"cases"`
	} `json:"allow"`
}

func loadOCIRefCorpus(t *testing.T) ociRefCorpus {
	t.Helper()
	b, err := os.ReadFile("../shared/conformance/ociref.json")
	if err != nil {
		t.Fatalf("reading the corpus: %v", err)
	}
	var c ociRefCorpus
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatalf("parsing the corpus: %v", err)
	}

	// A CORPUS THAT SILENTLY SHRANK TO NOTHING PASSES (shared/conformance/README.md, rule 3). Every loop
	// below is over a slice out of this file, so an empty one — a renamed key, a bad merge, a JSON
	// shape that stopped matching these structs — would report success over zero assertions.
	if n := len(c.Split.Cases); n < 8 {
		t.Fatalf("split has %d cases; the corpus is not being read", n)
	}
	if n := len(c.Verdict.Cases); n < 12 {
		t.Fatalf("verdict has %d cases; the corpus is not being read", n)
	}
	if n := len(c.Pin.Cases); n < 6 {
		t.Fatalf("pin has %d cases; the corpus is not being read", n)
	}
	if n := len(c.Push.Cases); n < 8 {
		t.Fatalf("push has %d cases; the corpus is not being read", n)
	}
	if c.Push.Actor.Name == "" || c.Push.Actor.Version == "" {
		t.Fatal("push names no actor, so every --push case would be resolved against an empty name")
	}
	if n := len(c.Allow.Cases); n < 12 {
		t.Fatalf("allow has %d cases; the corpus is not being read", n)
	}
	if n := len(c.Allow.Rules.Cases); n < 8 {
		t.Fatalf("allow.rules has %d cases; the corpus is not being read", n)
	}
	return c
}

// TestOCIRefCorpusStillHoldsTheInterestingInputs is the guard on the guard. A corpus of the easy
// cases is "the shape of every guard in this list that failed to guard anything"
// (shared/conformance/README.md), and the three names below are the ones the whole thing exists for:
// `café` cannot be an Artifact, `a/b` can and must stay legal, and `1:2` is a VERSION that cannot be
// a tag. Losing any of them leaves a corpus that passes while the bug is back.
func TestOCIRefCorpusStillHoldsTheInterestingInputs(t *testing.T) {
	c := loadOCIRefCorpus(t)
	all := ""
	for _, k := range c.Verdict.Cases {
		all += k.Ref + "\n"
	}
	for _, k := range c.Pin.Cases {
		all += k.Ref + "\n"
	}
	for _, k := range c.Push.Cases {
		all += k.Ref + "\n"
	}
	for _, want := range []string{"café", "a/b", "1:2", "my actor", "Foo", "+build"} {
		if !strings.Contains(all, want) {
			t.Errorf("the corpus no longer carries %q — shared/conformance/queues.json's adversarial names are "+
				"the reason this file exists, and a corpus without them proves nothing", want)
		}
	}

	// AND THE POLICY SECTION'S OWN ADVERSARIAL INPUTS. Every one of these is a row that a plausible,
	// wrong implementation passes every other row with: a `strings.HasPrefix` on the host, a
	// `strings.HasPrefix` on the path, a port ignored, an empty Domain read as "no registry to check".
	// A corpus that lost them would be a corpus of the cases nobody gets wrong.
	policy := ""
	for _, k := range c.Allow.Cases {
		policy += k.Ref + "\n"
	}
	for _, k := range c.Allow.Rules.Cases {
		policy += k.Entry + "\n"
	}
	for _, want := range []string{"ghcr.io.evil.example", "acme-evil", "localhost:5001", "myregistry", "café"} {
		if !strings.Contains(policy, want) {
			t.Errorf("the allow corpus no longer carries %q — it is one of the inputs a widened match "+
				"admits silently, and without it the section proves only that the easy cases work", want)
		}
	}
	// A row whose reference has no digest at all, or the whole `unpinned`/`unrepresentable` ordering
	// evidence is gone and the section stops saying anything about the ORDER of the gates.
	seen := map[string]bool{}
	for _, k := range c.Allow.Cases {
		seen[k.Verdict] = true
	}
	for _, want := range []string{"ok", "registry", "unpinned", "unrepresentable", "signature"} {
		if !seen[want] {
			t.Errorf("no allow case has verdict %q, so that gate is asserted about nothing", want)
		}
	}
}

// TestOCIRefSplitMatchesTheCorpus drives the taking-apart. It is separate from the verdict because a
// refusal has to name WHICH part is wrong, and it can only do that if the parts are right.
func TestOCIRefSplitMatchesTheCorpus(t *testing.T) {
	c := loadOCIRefCorpus(t)
	for _, k := range c.Split.Cases {
		got := splitOCIRef(k.Ref)
		if got.Domain != k.Domain || got.Path != k.Path || got.Tag != k.Tag ||
			got.TagSet != k.TagSet || got.Digest != k.Digest {
			t.Errorf("splitOCIRef(%q) =\n  domain %q path %q tag %q tagSet %v digest %q\nwant\n"+
				"  domain %q path %q tag %q tagSet %v digest %q\n  (%s)",
				k.Ref, got.Domain, got.Path, got.Tag, got.TagSet, got.Digest,
				k.Domain, k.Path, k.Tag, k.TagSet, k.Digest, k.Why)
		}
	}
}

// TestOCIRefVerdictMatchesTheCorpus drives the judgement, including which PART each refusal blames.
func TestOCIRefVerdictMatchesTheCorpus(t *testing.T) {
	c := loadOCIRefCorpus(t)
	for _, k := range c.Verdict.Cases {
		err := checkOCIRef(k.Ref)
		switch k.Verdict {
		case "ok":
			if err != nil {
				t.Errorf("checkOCIRef(%q) refused a legal reference: %v\n  (%s)", k.Ref, err, k.Why)
			}
		case "unrepresentable":
			if !errors.Is(err, errImageUnrepresentable) {
				t.Errorf("checkOCIRef(%q) = %v, want errImageUnrepresentable\n  (%s)", k.Ref, err, k.Why)
				continue
			}
			// THE PART, because naming the wrong half of a string is the failure issue 15 measured.
			// An operator told the tag is bad when the path is bad edits the wrong thing.
			if !strings.Contains(err.Error(), k.Part) {
				t.Errorf("checkOCIRef(%q) does not blame the %s:\n%v\n  (%s)", k.Ref, k.Part, err, k.Why)
			}
			if k.Bad != "" && !strings.Contains(err.Error(), k.Bad) {
				t.Errorf("checkOCIRef(%q) does not quote the offending %q:\n%v", k.Ref, k.Bad, err)
			}
		default:
			t.Fatalf("case %q has verdict %q, which this driver does not implement", k.Ref, k.Verdict)
		}
	}
}

// TestOCIRefPinMatchesTheCorpus drives the PODMAN DRIVER over the corpus — one of the three sites.
// The rule on top of the grammar is the driver's own: `<repo>@sha256:<64 hex>` and nothing else.
func TestOCIRefPinMatchesTheCorpus(t *testing.T) {
	c := loadOCIRefCorpus(t)
	for _, k := range c.Pin.Cases {
		digest, err := imageDigest(k.Ref)
		switch k.Verdict {
		case "pinned":
			if err != nil {
				t.Errorf("imageDigest(%q) refused a pinned reference: %v\n  (%s)", k.Ref, err, k.Why)
				continue
			}
			if digest != k.Digest {
				t.Errorf("imageDigest(%q) = %q, want %q", k.Ref, digest, k.Digest)
			}
		case "unpinned":
			if !errors.Is(err, errImageUnpinned) {
				t.Errorf("imageDigest(%q) = %v, want errImageUnpinned\n  (%s)", k.Ref, err, k.Why)
			}
			if errors.Is(err, errImageUnrepresentable) {
				t.Errorf("imageDigest(%q) told the operator their Actor can never run, when all it "+
					"needs is a digest", k.Ref)
			}
		case "unrepresentable":
			if !errors.Is(err, errImageUnrepresentable) {
				t.Errorf("imageDigest(%q) = %v, want errImageUnrepresentable\n  (%s)", k.Ref, err, k.Why)
			}
			if errors.Is(err, errImageUnpinned) {
				t.Errorf("imageDigest(%q) reported BOTH refusals, so a caller cannot tell them apart", k.Ref)
			}
		default:
			t.Fatalf("case %q has verdict %q, which this driver does not implement", k.Ref, k.Verdict)
		}
	}
}

// TestOCIRefPushMatchesTheCorpus drives `kontra build --push` — the site slice 07 added, and the
// reason the shared answer had to exist before it did.
func TestOCIRefPushMatchesTheCorpus(t *testing.T) {
	c := loadOCIRefCorpus(t)
	name, version := c.Push.Actor.Name, c.Push.Actor.Version
	for _, k := range c.Push.Cases {
		dest, err := pushDestination(k.Ref, "", "", name, version)
		switch k.Verdict {
		case "ok":
			if err != nil {
				t.Errorf("pushDestination(%q) refused a destination: %v\n  (%s)", k.Ref, err, k.Why)
				continue
			}
			if got := dest.tagged(); got != k.Expect {
				t.Errorf("pushDestination(%q) = %q, want %q\n  (%s)", k.Ref, got, k.Expect, k.Why)
			}
			if dest.PlainHTTP != k.PlainHTTP {
				t.Errorf("pushDestination(%q) plainHTTP = %v, want %v — a downgraded push carries an "+
					"Artifact and a credential in the clear\n  (%s)", k.Ref, dest.PlainHTTP, k.PlainHTTP, k.Why)
			}
		case "unrepresentable":
			if !errors.Is(err, errImageUnrepresentable) {
				t.Errorf("pushDestination(%q) = %v, want errImageUnrepresentable — this is the THIRD site "+
					"that names an Artifact and it must not answer for itself\n  (%s)", k.Ref, err, k.Why)
			}
		case "no-registry":
			if err == nil {
				t.Errorf("pushDestination(%q) accepted a reference with no registry, which means Docker "+
					"Hub\n  (%s)", k.Ref, k.Why)
				continue
			}
			// The refusal has to name the destination it is protecting, or it reads as a typo.
			for _, want := range []string{"DOCKER HUB", "100 per hour"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("pushDestination(%q) does not say %q:\n%v", k.Ref, want, err)
				}
			}
		case "digest-pinned":
			if err == nil {
				t.Errorf("pushDestination(%q) accepted a digest as a destination; a digest is what the "+
					"push COMPUTES\n  (%s)", k.Ref, k.Why)
				continue
			}
			if !strings.Contains(err.Error(), "COMPUTES") {
				t.Errorf("pushDestination(%q) does not say why a digest cannot be a destination:\n%v", k.Ref, err)
			}
		default:
			t.Fatalf("case %q has verdict %q, which this driver does not implement", k.Ref, k.Verdict)
		}
	}
}

// ═══ THE PROOF ISSUE 15 ASKS FOR ═══
//
// "One shared answer, consulted by build, pull and the driver — proven by breaking it in one place
// and watching all three change." This is that test. It takes every `unrepresentable` row in the
// corpus and asks every site, and it asserts three things of each answer:
//
//  1. it is errImageUnrepresentable, so a caller can BRANCH on it;
//  2. it carries the shared sentence, which is what makes "one place" checkable — a site that
//     wrote its own message would pass (1) and fail here; and
//  3. it does NOT offer a remedy that cannot work. That is the actual bug: `café` was told to
//     re-run `kontra deploy`, forever.
//
// And the fourth assertion is the one that keeps the fix honest: `a/b` STILL reports "never
// deployed" at the pull site, because for `a/b` that is true.
// THE COUNT IS NOT WRITTEN DOWN, and that is deliberate. The issue said three; slice 07 found FOUR,
// because `kontra deploy` names an Artifact on the PUSH side with a string it builds itself, and the
// issue had folded that into "deploy/pull". shared/conformance/README.md names a count in a comment as "the
// least reliable kind of documentation there is" — three comments in this repo once told the reader
// how many peers a derivation had and none of them counted the same set. So the list below is the
// truth and no sentence anywhere claims a total.
func TestOneAnswerReachesEverySiteThatNamesAnArtifact(t *testing.T) {
	c := loadOCIRefCorpus(t)

	// The sentence that proves the answer came from cli/ociref.go and not from a copy. Two fragments,
	// not one, so a site that happened to quote the grammar cannot pass by accident.
	shared := []string{"shared/conformance/queues.json", "NO REBUILD, REDEPLOY OR PIN CHANGES THIS"}

	// The remedies each site used to offer, none of which reaches this class of reference.
	unreachable := []string{"was never deployed", "kontra deploy --actor", "kontra build --push"}

	asserted, deployed := 0, 0
	for _, k := range c.Verdict.Cases {
		if k.Verdict != "unrepresentable" || k.Ref == "" {
			continue
		}
		asserted++

		// Every site, each given the exact string ITS runtime would receive.
		sites := map[string]error{
			"driver (cli/driver_podman.go)": secondOf(imageDigest(k.Ref)),
			"pull   (cli/scale.go)":         pullFailure("127.0.0.1:1", "café", "0.1.0", k.Ref, errors.New("invalid reference format")),
			"build  (cli/build.go)":         destErr(pushDestination(k.Ref, "", "", "café", "0.1.0")),
		}
		// deploy's push half is driven THROUGH runDeploy rather than by calling checkOCIRef here — a
		// row that asserted the shared function about itself would be the vacuous half of this test.
		// It runs only for rows this site can actually EXPRESS (see deployableRow); a row it cannot
		// express is SKIPPED rather than approximated, because an approximated row is a row that
		// stopped testing what it names.
		if deployableRow(k.Ref) {
			deployed++
			sites["deploy (cli/deploy.go)"] = deployRefusal(t, k.Ref)
		}
		for site, err := range sites {
			if !errors.Is(err, errImageUnrepresentable) {
				t.Errorf("%s: %q = %v\n  want errImageUnrepresentable — this is a class of Actor, not a "+
					"typo, and a site that cannot say so sends the operator to fix something else",
					site, k.Ref, err)
				continue
			}
			for _, want := range shared {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("%s: %q does not carry the shared answer (%q missing), so this site has its "+
						"OWN message and cli/ociref.go is not the one place:\n%v", site, k.Ref, want, err)
				}
			}
			for _, no := range unreachable {
				if strings.Contains(err.Error(), no) {
					t.Errorf("%s: %q offers %q, which is exactly the unreachable advice issue 15 "+
						"measured:\n%v", site, k.Ref, no, err)
				}
			}
		}
	}
	if asserted == 0 {
		t.Fatal("no unrepresentable rows in the corpus, so every site was asserted about nothing")
	}
	if deployed == 0 {
		t.Fatal("the deploy site was driven over zero rows — deployableRow no longer matches anything " +
			"in the corpus, and this test silently covers one site fewer than it names")
	}
}

// TestTheOtherTruthSurvives is the other half of issue 15's "Done when": `a/b` is a legal repository
// that really was never deployed, so it must STILL be told to deploy. A fix that refused it would be
// exactly as wrong as the bug — one message for two truths, with the two truths swapped.
func TestTheOtherTruthSurvives(t *testing.T) {
	reg := fakeRegistry(t, http.StatusNotFound, "")
	img := reg + "/a/b:1.0.0"

	err := pullFailure(reg, "a/b", "1.0.0", img, errors.New("manifest unknown"))
	if err == nil {
		t.Fatal("a pull of an undeployed image was not a failure")
	}
	if errors.Is(err, errImageUnrepresentable) {
		t.Fatalf("`a/b` was told its name can never be an image; a slash is a legal path separator "+
			"and deploying really does fix this:\n%v", err)
	}
	for _, want := range []string{"was never deployed", "kontra deploy --actor"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal for `a/b` no longer offers the remedy that WORKS (%q missing):\n%v", want, err)
		}
	}
}

// --- slice 14: the Machine's trust policy, over the same corpus -------------------------------------

// TestTrustRulesMatchTheCorpus drives the ALLOWLIST ENTRY through the same three rules the grammar is
// made of. An entry is a registry, and the day it is judged by a fourth rule written beside it is the
// day `ghcr.io/café` becomes allowlistable and the reference it admits is refused by the driver two
// gates later, for a reason that names the wrong file.
func TestTrustRulesMatchTheCorpus(t *testing.T) {
	c := loadOCIRefCorpus(t)
	for _, k := range c.Allow.Rules.Cases {
		got, err := parseTrustRule(k.Entry)
		switch k.Verdict {
		case "ok":
			if err != nil {
				t.Errorf("parseTrustRule(%q) refused a legal entry: %v\n  (%s)", k.Entry, err, k.Why)
				continue
			}
			if got.Host != k.Host {
				t.Errorf("parseTrustRule(%q).Host = %q, want %q\n  (%s)", k.Entry, got.Host, k.Host, k.Why)
			}
			if strings.Join(got.Path, "/") != strings.Join(k.Path, "/") {
				t.Errorf("parseTrustRule(%q).Path = %v, want %v\n  (%s)", k.Entry, got.Path, k.Path, k.Why)
			}
		case "refused":
			if err == nil {
				t.Errorf("parseTrustRule(%q) accepted an entry that does not name a registry (%v)\n  (%s)",
					k.Entry, got, k.Why)
			}
		default:
			t.Fatalf("entry %q has verdict %q, which this driver does not implement", k.Entry, k.Verdict)
		}
	}
}

// TestTrustPolicyAdmitMatchesTheCorpus drives `trustPolicy.admit` — the ONE gate a container driver
// calls before it pulls.
//
// IT ASSERTS THE ORDER AND NOT ONLY THE VERDICT, which is the whole reason the recording verifier
// exists. `errRegistryNotAllowed` returned AFTER a signature fetch would be the same error value and
// a materially different program: the refused registry would have learned this Machine's address, its
// clock and the fact that something told it to go there. So every row that should be refused early
// also asserts that nothing was ever asked of the network, and every row that reaches the verifier
// asserts the repository and digest it was asked about — the ones the container would have run.
func TestTrustPolicyAdmitMatchesTheCorpus(t *testing.T) {
	c := loadOCIRefCorpus(t)
	for _, k := range c.Allow.Cases {
		v := &recordingVerifier{}
		p, err := loadTrustPolicy(trustOptions{
			Registries: strings.Join(k.Allow, ","),
			Unsigned:   strings.Join(k.Unsigned, ","),
			// A key is named so `loadTrustPolicy` has a root to build a verifier from; it is replaced
			// below, because what these rows test is the policy and not cosign.
			Key: "/dev/null",
		})
		if err != nil {
			t.Errorf("the policy for %q does not load: %v\n  (%s)", k.Ref, err, k.Why)
			continue
		}
		p.Verifier = v

		digest, err := p.admit(context.Background(), k.Ref)
		switch k.Verdict {
		case "ok":
			if err != nil {
				t.Errorf("admit(%q) refused an image from an allowlisted registry marked unsigned: %v\n  (%s)",
					k.Ref, err, k.Why)
			}
			if v.calls != 0 {
				t.Errorf("admit(%q) asked the verifier about a registry the operator marked "+
					"--trust-unsigned, so the exception does nothing\n  (%s)", k.Ref, k.Why)
			}
		case "registry":
			if !errors.Is(err, errRegistryNotAllowed) {
				t.Errorf("admit(%q) = %v, want errRegistryNotAllowed\n  (%s)", k.Ref, err, k.Why)
			}
			if v.calls != 0 {
				t.Errorf("admit(%q) contacted the registry before deciding it was not allowed — the "+
					"allowlist is a string check and a refused registry must not learn this Machine "+
					"exists\n  (%s)", k.Ref, k.Why)
			}
		case "unpinned":
			if !errors.Is(err, errImageUnpinned) {
				t.Errorf("admit(%q) = %v, want errImageUnpinned\n  (%s)", k.Ref, err, k.Why)
			}
			if v.calls != 0 {
				t.Errorf("admit(%q) fetched a signature for a reference it was going to refuse for free\n  (%s)",
					k.Ref, k.Why)
			}
		case "unrepresentable":
			if !errors.Is(err, errImageUnrepresentable) {
				t.Errorf("admit(%q) = %v, want errImageUnrepresentable — the shared grammar is asked "+
					"before any policy\n  (%s)", k.Ref, err, k.Why)
			}
			// The shared sentence, so the policy path cannot become a fifth site with its own message.
			for _, want := range []string{"shared/conformance/queues.json", "NO REBUILD, REDEPLOY OR PIN CHANGES THIS"} {
				if err != nil && !strings.Contains(err.Error(), want) {
					t.Errorf("admit(%q) does not carry the shared answer (%q missing), so cli/ociref.go is "+
						"not the one place:\n%v", k.Ref, want, err)
				}
			}
		case "signature":
			if !errors.Is(err, errImageUnsigned) {
				t.Errorf("admit(%q) = %v, want the verifier's refusal — this reference is legal, "+
					"allowlisted and pinned, so the only question left is who signed it\n  (%s)",
					k.Ref, err, k.Why)
			}
			if v.calls != 1 {
				t.Errorf("admit(%q) asked the verifier %d times, want 1\n  (%s)", k.Ref, v.calls, k.Why)
				continue
			}
			if v.repo != k.Repository || v.digest != k.Digest {
				t.Errorf("admit(%q) asked about %s@%s, want %s@%s — a signature checked against a "+
					"different reference than the one that runs is not a check\n  (%s)",
					k.Ref, v.repo, v.digest, k.Repository, k.Digest, k.Why)
			}
			if digest != "" {
				t.Errorf("admit(%q) returned a digest alongside a refusal (%q)", k.Ref, digest)
			}
		default:
			t.Fatalf("case %q has verdict %q, which this driver does not implement", k.Ref, k.Verdict)
		}
	}
}

// recordingVerifier answers "no" and remembers being asked. Answering NO rather than yes is the
// deliberate half: a verifier that said yes would make "the verifier was reached" and "the image was
// admitted" the same observation, and the corpus needs them apart.
type recordingVerifier struct {
	calls        int
	repo, digest string
}

func (v *recordingVerifier) verify(_ context.Context, repo, digest string) error {
	v.calls++
	v.repo, v.digest = repo, digest
	return fmt.Errorf("%w: %s@%s (the corpus's verifier, which signs nothing)", errImageUnsigned, repo, digest)
}

func (v *recordingVerifier) root() string { return "a test verifier" }

// secondOf drops a two-value call's first result, so the three sites can be written as one map.
func secondOf(_ string, err error) error { return err }

// destErr does the same for pushDestination, whose first result is a struct.
func destErr(_ bundleDest, err error) error { return err }

// deployableRow says whether `runDeploy`'s site can be driven with this reference AT ALL.
//
// TWO CONSTRAINTS, AND THE SECOND IS ABOUT THIS BOX RATHER THAN ABOUT THE CODE.
//
//  1. `runDeploy` builds `<registry>/<name>:<version>` from three inputs, so a row is expressible
//     only if splitting it back into those three and rejoining them yields the row EXACTLY. A row
//     with no tag, or with a digest, cannot be expressed — and approximating one (padding a missing
//     tag with `0.0.0`, say) turns an unrepresentable reference into a legal one.
//
//  2. THE REGISTRY MUST BE LOOPBACK. `runDeploy` reaches the network two lines past the check under
//     test, so a REGRESSION — not the passing case, which never gets there — would dial whatever
//     host the row names. A test whose FAILURE mode is an outbound request to a third party is not
//     a test to leave in a suite. That is not hypothetical: the first version of this helper did
//     both things wrong at once, padded `ghcr.io/acme/nscheck:` into a legal reference, and sent an
//     anonymous `GET https://ghcr.io/v2/` (401, no credential, nothing published) from this box.
//     A red run stays on this machine.
func deployableRow(ref string) bool {
	r := splitOCIRef(ref)
	if r.Domain == "" || r.Digest != "" || !r.TagSet || r.Tag == "" || r.Path == "" {
		return false
	}
	if r.Domain+"/"+r.Path+":"+r.Tag != ref {
		return false
	}
	return loopbackHost(r.Domain)
}

// deployRefusal drives `runDeploy` for an actor whose Artifact reference is EXACTLY `ref`, and
// returns the error it produced.
//
// IT ASSERTS THE REFUSAL IS BEFORE THE BUILD, not merely that one happened. The fake engine fails
// the test if anything asks it to build: an image build for an actor that can never be pushed is a
// minute and a half spent to reach a message the first string comparison could have produced, and
// "it errors eventually" is the behaviour this replaced.
func deployRefusal(t *testing.T, ref string) error {
	t.Helper()
	r := splitOCIRef(ref)
	t.Setenv("KONTRA_REGISTRY", r.Domain)

	dir := t.TempDir()
	manifest := fmt.Sprintf("{%q:%q,%q:%q}", "name", r.Path, "version", r.Tag)
	if err := os.WriteFile(filepath.Join(dir, "actor.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	old := newDocker
	newDocker = func() (imageAPI, error) { return refusingDocker{t: t}, nil }
	t.Cleanup(func() { newDocker = old })

	_, err := runDeploy(context.Background(), io.Discard, deployOpts{actorDir: dir})
	return err
}

// refusingDocker is an imageAPI that fails the test if a build, tag or push is attempted. The base
// images read as present so nothing is built for THAT reason either.
type refusingDocker struct{ t *testing.T }

func (refusingDocker) ImageList(context.Context, image.ListOptions) ([]image.Summary, error) {
	return []image.Summary{{}}, nil
}
func (d refusingDocker) ImageBuild(context.Context, io.Reader, types.ImageBuildOptions) (types.ImageBuildResponse, error) {
	d.t.Error("runDeploy started an image build for an Actor whose Artifact can never be named — " +
		"the refusal has to come before the build, not after it")
	return types.ImageBuildResponse{Body: io.NopCloser(strings.NewReader(""))}, nil
}
func (d refusingDocker) ImageTag(_ context.Context, _, target string) error {
	d.t.Errorf("runDeploy tagged %q for an unnameable Artifact", target)
	return nil
}
func (d refusingDocker) ImagePush(_ context.Context, ref string, _ image.PushOptions) (io.ReadCloser, error) {
	d.t.Errorf("runDeploy pushed %q for an unnameable Artifact", ref)
	return io.NopCloser(strings.NewReader("")), nil
}
