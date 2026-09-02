package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// The CLI ARM of conformance/bundleref.json — where a Bundle lives once it is an OCI artifact.
//
// THIS BINARY IS THE WRITER AND IT HAS NEVER BEEN THE TESTED ONE. The address a Bundle is
// published to is assembled here out of four separate arguments (`bundleRepoPrefix`, the actor's
// name, the version, the layer sha) and re-assembled in `backend/src/activities/fleet.ts` out of
// the same four — and, before this corpus, the two were pinned by a Go test asserting one literal
// and a vitest asserting another, neither aware of the other. That is precisely the arrangement
// `conformance/README.md` calls a hand-copied golden, and the object-store layout it replaces
// drifted exactly that way once already.
//
// The manifest URL has ONE implementation and it is TypeScript's: only the control plane resolves
// a tag, and this side lets oras build the URL. It is asserted over there and recorded here, the
// way `output_dataset.json` records being a two-of-three corpus, rather than mirrored into a
// second Go helper that nothing would call.

type bundleRepoCase struct {
	Why    string `json:"why"`
	Name   string `json:"name"`
	Expect string `json:"expect"`
}

type bundleRefCase struct {
	Why      string `json:"why"`
	Registry string `json:"registry"`
	Name     string `json:"name"`
	Version  string `json:"version"`
	Digest   string `json:"digest"`
	Tagged   string `json:"tagged"`
	Pinned   string `json:"pinned"`
}

type bundleURLCase struct {
	Why      string `json:"why"`
	Registry string `json:"registry"`
	Name     string `json:"name"`
	Version  string `json:"version"`
	SHA      string `json:"sha"`
	Manifest string `json:"manifest"`
	Blob     string `json:"blob"`
}

type bundleRefCorpus struct {
	Repo struct {
		Cases []bundleRepoCase `json:"cases"`
	} `json:"repo"`
	Reference struct {
		Cases []bundleRefCase `json:"cases"`
	} `json:"reference"`
	URLs struct {
		Cases []bundleURLCase `json:"cases"`
	} `json:"urls"`
	MediaTypes struct {
		Artifact string `json:"artifact"`
		Config   string `json:"config"`
		Layer    string `json:"layer"`
	} `json:"media_types"`
}

// ../conformance/bundleref.json — cli -> <repo root>.
func loadBundleRefCorpus(t *testing.T) *bundleRefCorpus {
	t.Helper()
	raw, err := os.ReadFile("../conformance/bundleref.json")
	if err != nil {
		t.Fatalf("read the corpus: %v", err)
	}
	var doc bundleRefCorpus
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse the corpus: %v", err)
	}
	// A corpus that silently shrank to nothing passes every assertion in this file.
	if len(doc.Repo.Cases) < 3 || len(doc.Reference.Cases) < 2 || len(doc.URLs.Cases) < 4 {
		t.Fatalf("the corpus shrank: repo=%d reference=%d urls=%d",
			len(doc.Repo.Cases), len(doc.Reference.Cases), len(doc.URLs.Cases))
	}
	// The inputs that BREAK, named so a corpus edited down to the easy cases goes red here rather
	// than passing quietly: a name that prefixes another, a tag carrying both a dot and a hyphen,
	// an all-zero digest that a truthiness check would read as absent, and a registry with a
	// scheme, which is the airgap-mirror case and the one a hardcoded `http://` downgrades.
	blob := string(raw)
	for _, want := range []string{"nscheck-2", "0.2.0-rc1", strings.Repeat("0", 64), "https://"} {
		if !strings.Contains(blob, want) {
			t.Errorf("the corpus no longer exercises %q", want)
		}
	}
	return &doc
}

func TestTheBundleRepositoryMatchesTheCorpus(t *testing.T) {
	for _, c := range loadBundleRefCorpus(t).Repo.Cases {
		t.Run(c.Why, func(t *testing.T) {
			if got := bundleRepo(c.Name); got != c.Expect {
				t.Errorf("bundleRepo(%q) = %q, want %q", c.Name, got, c.Expect)
			}
		})
	}
}

// The two names a published Bundle has. `tagged` is what a version resolves through and moves;
// `pinned` is what a mirror copies and never does.
func TestTheBundleReferencesMatchTheCorpus(t *testing.T) {
	for _, c := range loadBundleRefCorpus(t).Reference.Cases {
		t.Run(c.Why, func(t *testing.T) {
			art := bundleArtifact{
				Registry: c.Registry, Repo: bundleRepo(c.Name), Tag: c.Version, Digest: c.Digest,
			}
			if got := art.ref(); got != c.Tagged {
				t.Errorf("ref() = %q, want %q", got, c.Tagged)
			}
			if got := art.pinned(); got != c.Pinned {
				t.Errorf("pinned() = %q, want %q", got, c.Pinned)
			}
		})
	}
}

// The blob URL is the ONLY address that reaches a Machine, and `machineInstall` curls it with no
// further parsing — so a drift here is a Fleet that installs nothing and says nothing.
func TestTheBundleBlobURLMatchesTheCorpus(t *testing.T) {
	for _, c := range loadBundleRefCorpus(t).URLs.Cases {
		t.Run(c.Why, func(t *testing.T) {
			if got := bundleBlobURL(c.Registry, c.Name, c.SHA); got != c.Blob {
				t.Errorf("bundleBlobURL(%q,%q,%q) = %q, want %q", c.Registry, c.Name, c.SHA, got, c.Blob)
			}
		})
	}
}

// THE PAIRING INVARIANT, asserted over the corpus rather than over the rows somebody thought of.
//
// A URL and the sha a Machine checks it against are two values that travel together through
// `resolveBundle`, the stack args and `MachineActor`. Under the object store they were
// independent strings and nothing could tell a mismatched pair from a correct one until 65 MiB
// had been downloaded; a blob URL ends with its own digest, so the pair is now self-checking and
// `validateMachineActor` refuses one that is not.
func TestABlobURLEndsWithTheDigestItServes(t *testing.T) {
	for _, c := range loadBundleRefCorpus(t).URLs.Cases {
		if !strings.HasSuffix(c.Blob, "sha256:"+c.SHA) {
			t.Errorf("%s: %q does not end with the sha a Machine verifies (%s)", c.Why, c.Blob, c.SHA)
		}
	}
}

// The media types are the manifest's, and a mirror or a scanner filters on the first of them.
func TestTheBundleMediaTypesMatchTheCorpus(t *testing.T) {
	m := loadBundleRefCorpus(t).MediaTypes
	for _, c := range []struct{ got, want, name string }{
		{bundleArtifactType, m.Artifact, "artifactType"},
		{bundleConfigType, m.Config, "config"},
		{bundleLayerType, m.Layer, "layer"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
}
