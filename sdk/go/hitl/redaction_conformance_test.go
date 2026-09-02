package hitl_test

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/medmahmoudi26/kontra/sdk/go/hitl"
	"github.com/medmahmoudi26/kontra/sdk/go/narrate"
)

// The GO ARM of the redaction contract (conformance/redaction.json). The Python arm is
// tests/test_redaction_conformance.py and asserts the same file.
//
// THE ONE PARITY SURFACE WHERE BEING WRONG IS A CREDENTIAL. Everything else these two SDKs must
// agree about is a key, a name or a hash — getting it wrong loses work. Getting redaction wrong
// writes a token into workflow history in the clear, where the claim-check codec leaves anything
// under 128 KiB inline and readable by anyone who can open the run.
//
// The three rules are byte-identical between the SDKs today. This is the guard for the thing the
// Python peer's own header already named and nothing acted on: "a word only one of them knows is
// redacted in one surface and not in the other."
//
// THE TWO KINDS STAY SEPARATE. A whole-key match over an ask's fields is not the same job as a
// word-in-a-sentence match over prose; merging them would either redact "password policy" in a
// narration or miss "db_password" in a form. What this asserts is that the two LANGUAGES agree,
// not that the two RULES should.

type redactionDoc struct {
	SentenceRedacted string `json:"sentence_redacted"`
	ValueRedacted    string `json:"value_redacted"`
	Sentences        []struct {
		Why    string `json:"why"`
		Input  string `json:"input"`
		Expect string `json:"expect"`
	} `json:"sentences"`
	Values []struct {
		Why    string `json:"why"`
		Input  any    `json:"input"`
		Expect any    `json:"expect"`
	} `json:"values"`
}

func loadRedaction(t *testing.T) redactionDoc {
	t.Helper()
	// ../../../conformance/redaction.json — hitl -> go -> sdk -> <repo root>.
	b, err := os.ReadFile("../../../conformance/redaction.json")
	if err != nil {
		t.Fatalf("read the corpus: %v", err)
	}
	var doc redactionDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("parse the corpus: %v", err)
	}
	// A corpus that shrank to its easy half would pass everything below: a rule that redacts
	// EVERYTHING passes every positive case and is useless, so the negatives are asserted present.
	if len(doc.Sentences) < 8 || len(doc.Values) < 6 {
		t.Fatalf("corpus has %d sentences / %d values, want at least 8 / 6", len(doc.Sentences), len(doc.Values))
	}
	blob := strings.ToLower(string(b))
	for _, needed := range []string{"passwordless", "bearer", "basic", "db_password"} {
		if !strings.Contains(blob, needed) {
			t.Errorf("the corpus no longer exercises %q", needed)
		}
	}
	return doc
}

func TestASentenceIsRedactedAsTheCorpusSays(t *testing.T) {
	for _, c := range loadRedaction(t).Sentences {
		t.Run(c.Why, func(t *testing.T) {
			if got := narrate.Redact(c.Input); got != c.Expect {
				t.Errorf("Redact(%q)\n got  %q\n want %q", c.Input, got, c.Expect)
			}
		})
	}
}

func TestAValueIsRedactedAsTheCorpusSays(t *testing.T) {
	for _, c := range loadRedaction(t).Values {
		t.Run(c.Why, func(t *testing.T) {
			// Round-tripped through JSON so the comparison is between two decoded shapes rather
			// than between Go's `any` tree and the corpus's — otherwise every number is a float64
			// on one side and an int on the other and nothing matches for the wrong reason.
			got := hitl.Redact(c.Input)
			gotJSON, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			wantJSON, err := json.Marshal(c.Expect)
			if err != nil {
				t.Fatal(err)
			}
			var g, w any
			_ = json.Unmarshal(gotJSON, &g)
			_ = json.Unmarshal(wantJSON, &w)
			if !reflect.DeepEqual(g, w) {
				t.Errorf("Redact(%v)\n got  %s\n want %s", c.Input, gotJSON, wantJSON)
			}
		})
	}
}

// The marker itself is contract: a side that reworded it would pass every case above only if the
// corpus were regenerated from it, which is the drift this file exists to stop.
func TestTheReplacementStringsAreTheOnesTheCorpusRecorded(t *testing.T) {
	doc := loadRedaction(t)
	if hitl.Redacted != doc.ValueRedacted {
		t.Errorf("hitl marker %q, corpus %q", hitl.Redacted, doc.ValueRedacted)
	}
	if got := narrate.Redact("api_key=abcdef123456"); !strings.Contains(got, doc.SentenceRedacted) {
		t.Errorf("narrate marker: %q does not contain %q", got, doc.SentenceRedacted)
	}
}
