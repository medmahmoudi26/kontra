package detect_test

import (
	"testing"

	"github.com/medmahmoudi26/kontra-actors/go/desync/detect"
	"github.com/medmahmoudi26/kontra-actors/go/desync/technique"
)

// THE AXIS STRINGS ARE A JOIN KEY, AND A BROKEN ONE IS SILENT.
//
// `techniques.axis` (what the corpus publishes) and `observations.axis` (what a scan writes) are
// joined by an operator asking "which techniques ever fire?". When they disagreed —
// `framing`/`injection` on one side, `frame`/`fold` on the other — that join returned zero rows
// and raised nothing, because `'framing' = 'frame'` is simply false in SQL. Nothing in either
// package could have caught it: both sides compiled, both sides were internally consistent.
//
// This test is the only thing that makes the two constants one fact.
func TestAxisVocabularyIsShared(t *testing.T) {
	if string(technique.AxisSplit) != detect.AxisSplit {
		t.Errorf("split axis differs: corpus %q vs observations %q",
			technique.AxisSplit, detect.AxisSplit)
	}
	if string(technique.AxisSmuggle) != detect.AxisSmuggle {
		t.Errorf("smuggle axis differs: corpus %q vs observations %q",
			technique.AxisSmuggle, detect.AxisSmuggle)
	}
}

// The values are the FIELD's words, not this repo's. Renaming them to something internal again
// (`fold`, `framing`, `injection`) would make the `axis` column unreadable to anyone who has not
// read this codebase — which is every reader of a report it produces.
func TestAxisNamesAreTheFieldsNames(t *testing.T) {
	if detect.AxisSplit != "split" {
		t.Errorf("want the industry term %q for CWE-444 request splitting, got %q",
			"split", detect.AxisSplit)
	}
	if detect.AxisSmuggle != "smuggle" {
		t.Errorf("want the industry term %q for request smuggling, got %q",
			"smuggle", detect.AxisSmuggle)
	}
}
