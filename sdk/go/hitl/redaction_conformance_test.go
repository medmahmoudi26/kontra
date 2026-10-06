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

// The GO ARM of the redaction contract (shared/conformance/redaction.json). The Python arm is
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
	// A SECTION THIS STRUCT DOES NOT NAME IS SILENTLY DROPPED by encoding/json, which is how a new
	// corpus section can be added and leave both existing arms green while asserting nothing about
	// it. `http_headers` arrived with the report renderer; this field and the floor below are what
	// make it actually exercised here.
	HTTPHeaders []struct {
		Why    string `json:"why"`
		Input  string `json:"input"`
		Expect string `json:"expect"`
	} `json:"http_headers"`
}

func loadRedaction(t *testing.T) redactionDoc {
	t.Helper()
	// ../../../shared/conformance/redaction.json — hitl -> go -> sdk -> <repo root>.
	b, err := os.ReadFile("../../../shared/conformance/redaction.json")
	if err != nil {
		t.Fatalf("read the corpus: %v", err)
	}
	var doc redactionDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("parse the corpus: %v", err)
	}
	// A corpus that shrank to its easy half would pass everything below: a rule that redacts
	// EVERYTHING passes every positive case and is useless, so the negatives are asserted present.
	if len(doc.Sentences) < 8 || len(doc.Values) < 6 || len(doc.HTTPHeaders) < 12 {
		t.Fatalf("corpus has %d sentences / %d values / %d http_headers, want at least 8 / 6 / 12",
			len(doc.Sentences), len(doc.Values), len(doc.HTTPHeaders))
	}
	blob := strings.ToLower(string(b))
	for _, needed := range []string{"passwordless", "bearer", "basic", "db_password", "set-cookie", "hunter2"} {
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

// TestHTTPMessageRedactionMatchesTheCorpus drives the third rule.
//
// NO GO CALLER EXERCISES `RedactHTTP` IN PRODUCTION — the report renderer that needs it is
// TypeScript, in the orchestrator. This arm is the whole reason the Go implementation exists: a rule
// with one implementation drifts the moment a second appears, and the corpus is where the three
// languages are held to each other. `sdk/python/kontra/redaction.py` has carried its sentence rule
// with no production caller on the same reasoning since narration was removed.
//
// THE CASES INCLUDE A NON-BREAKING SPACE, deliberately. Go's regexp `\s` is ASCII-only while
// Python's and JavaScript's include U+00A0, and the rule runs over a latin-1 view of raw bytes where
// byte 0xA0 IS U+00A0. The sentence rule already diverges because of it — `token:\xa0abc123def456`
// redacts to `token:[redacted]` here and `token:\xa0[redacted]` in Python, which no case in this
// corpus catches — so the HTTP rule spells its whitespace `[ \t]` and that case proves it.
func TestHTTPMessageRedactionMatchesTheCorpus(t *testing.T) {
	doc := loadRedaction(t)
	for _, c := range doc.HTTPHeaders {
		why := c.Why
		if len(why) > 80 {
			why = why[:80]
		}
		t.Run(why, func(t *testing.T) {
			if got := narrate.RedactHTTP(c.Input); got != c.Expect {
				t.Errorf("RedactHTTP(%q)\n got  %q\n want %q", c.Input, got, c.Expect)
			}
		})
	}
}

// TestHTTPRedactionIsIdempotent pins what a re-rendered snapshot depends on: applying the rule to its
// own output changes nothing, because the marker carries brackets no credential pattern accepts.
func TestHTTPRedactionIsIdempotent(t *testing.T) {
	doc := loadRedaction(t)
	for _, c := range doc.HTTPHeaders {
		once := narrate.RedactHTTP(c.Input)
		if twice := narrate.RedactHTTP(once); twice != once {
			t.Errorf("not idempotent for %q:\n once  %q\n twice %q", c.Input, once, twice)
		}
	}
}
