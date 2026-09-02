package catalog

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// The GO ARM of the Run-id slug contract (conformance/slug.json). The Python arm is
// tests/test_slug_conformance.py and asserts the same file.
//
// WHAT THIS REPLACES. TestTempSlugKeepsARunIDOutOfThePath asserted
// "runs_2026-08-19T14_49_20_00_00_sweep" with the comment "want the same mapping Python's _slug
// makes" — a value hand-copied out of the Python implementation. On the other side of that pair,
// _slug's docstring cited `tests/test_temp_dataset.py` as pinning it. That file does not exist, so
// the Python half of a three-writer derivation was asserted by nothing at all and the Go half was
// asserted against a transcription of it. ADR 0035 rule two: a contract with two writers gets a
// corpus, not a hand-copied golden and not a source scrape.
//
// THE THIRD WRITER is the orchestrator's `backend/src/data/parquet.ts:safeName`, which sanitises
// the composed name again. tempSlug's own comment claimed it applies "the same rule"; it does not,
// and the corpus's `measured` field records the three places it differs and why none of them is
// live. The `safe_name` column is that side's answer, asserted by
// backend/src/data/slug.conformance.test.ts.

type slugCase struct {
	Why      string `json:"why"`
	In       string `json:"in"`
	Slug     string `json:"slug"`
	SafeName string `json:"safe_name"`
}

type slugCorpus struct {
	Bound int         `json:"bound"`
	Empty string      `json:"empty"`
	Cases []slugCase  `json:"cases"`
	Raw   []byte      `json:"-"`
}

func loadSlugCorpus(t *testing.T) slugCorpus {
	t.Helper()
	// ../../../conformance/slug.json — catalog -> go -> sdk -> <repo root>.
	b, err := os.ReadFile("../../../conformance/slug.json")
	if err != nil {
		t.Fatalf("read the corpus: %v", err)
	}
	var doc slugCorpus
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("parse the corpus: %v", err)
	}
	// A corpus that silently shrank to nothing would pass every case below.
	if len(doc.Cases) < 12 {
		t.Fatalf("the corpus has %d cases, want at least 12", len(doc.Cases))
	}
	// The named inputs ARE the point of the file: a corpus of `nscheck-1787150959` and nothing else
	// is satisfied by every wrong implementation of this function. Searched over the DECODED
	// inputs, never the file's text — `́` and a literal combining acute are the same input and
	// only one of them survives an editor.
	var ins strings.Builder
	for _, c := range doc.Cases {
		ins.WriteString(c.In)
		ins.WriteByte('\n')
	}
	for _, ch := range []string{"/", ":", "'", ";", "é", "\U0001f4e6", "́", "."} {
		if !strings.Contains(ins.String(), ch) {
			t.Errorf("the corpus no longer exercises %q", ch)
		}
	}
	doc.Raw = b
	return doc
}

func TestTheSlugMatchesTheCorpus(t *testing.T) {
	doc := loadSlugCorpus(t)
	for _, c := range doc.Cases {
		t.Run(c.Why, func(t *testing.T) {
			if got := tempSlug(c.In); got != c.Slug {
				t.Errorf("tempSlug(%q) = %q, want %q", c.In, got, c.Slug)
			}
		})
	}
}

// The two SDK-only rules, taken from the corpus rather than from a literal here — the Python arm
// asserts its own constant against the same two fields, which is what makes "the SDKs agree on the
// bound" a fact rather than a coincidence of two 64s.
func TestTheBoundAndTheFallbackAreWhatTheCorpusDeclares(t *testing.T) {
	doc := loadSlugCorpus(t)
	if tempSlugMax != doc.Bound {
		t.Errorf("tempSlugMax = %d, the corpus declares %d", tempSlugMax, doc.Bound)
	}
	if got := tempSlug(""); got != doc.Empty {
		t.Errorf("tempSlug(\"\") = %q, the corpus declares %q", got, doc.Empty)
	}
	// The bound is measured in the SAME unit on both sides. Go counts bytes into a strings.Builder
	// and Python slices code points, which agree only because every byte this function writes is
	// ASCII — so an over-long id must come back at exactly the bound, not merely under it.
	if n := len(tempSlug(strings.Repeat("n", 500))); n != doc.Bound {
		t.Errorf("an over-long id produced %d characters, want exactly %d", n, doc.Bound)
	}
}

// The composed name is what the orchestrator actually receives, and it is the reason the three
// divergences the corpus records are not live: `tmp_` in front makes every one of them
// unreachable. Asserted here rather than left in prose, because "always starts with tmp_" is the
// whole of the argument.
func TestTheComposedNameIsAlwaysASafeIdentifier(t *testing.T) {
	doc := loadSlugCorpus(t)
	for _, c := range doc.Cases {
		name := "tmp_" + tempSlug(c.In) + "_deadbeef"
		if !strings.HasPrefix(name, "tmp_") {
			t.Errorf("%q does not start with tmp_", name)
		}
		for _, r := range name {
			ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
				r == '_' || r == '.' || r == '-'
			if !ok {
				t.Errorf("the composed name %q carries %q, which safeName would rewrite", name, r)
			}
		}
	}
}
