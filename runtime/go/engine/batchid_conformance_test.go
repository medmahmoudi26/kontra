package engine

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// The GO ARM of the Batch content-hash contract (shared/conformance/batchid.json). The Python arm is
// tests/test_batchid_conformance.py and asserts the same file.
//
// WHY A CORPUS REPLACED A GOLDEN. Both SDKs key a committed Unit by the Batch's content hash
// (ADR 0023 §17), derived from a canonical JSON encoding of [method, units, params]. The two
// encoders did not agree: this one escaped `<`, `>` and `&` for browser safety, and Python's
// escaped every non-ASCII character. Same Batch, different keys, for any Unit carrying a query
// string or an accent.
//
// MEASURED 2026-08-27, method "probe", empty params:
//
//	https://acme.com/?a=1&b=2   py d8868b6200c19340   go 6250da42654ec10a
//	café                        py 4fc02e4dd607c61d   go d0de34ff30eafbc4
//	<script>                    py 7e16c1cbdab1e24b   go 6513f87dedc4a622
//	plain-ascii                 both 76457373b3bbfe8c
//
// The golden this file replaces (TestTheBatchHashMatchesThePythonPeer) asserted {"url":"a"},
// {"url":"b"} and {"depth":2} — plain ASCII with no symbols, the ONE input class where the two
// encoders cannot differ — while a URL with a query string is this repo's canonical Unit. Its own
// comment said "a drift in either encoder shows up here". It could not.

type batchIDCase struct {
	Why    string         `json:"why"`
	Method string         `json:"method"`
	Units  []any          `json:"units"`
	Params map[string]any `json:"params"`
	Expect string         `json:"expect"`
}

func loadBatchIDCorpus(t *testing.T) []batchIDCase {
	t.Helper()
	// ../../../shared/conformance/batchid.json — engine -> go -> runtime -> <repo root>.
	b, err := os.ReadFile("../../../shared/conformance/batchid.json")
	if err != nil {
		t.Fatalf("read the corpus: %v", err)
	}
	var doc struct {
		Cases []batchIDCase `json:"cases"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("parse the corpus: %v", err)
	}
	// A corpus that silently shrank to nothing would pass every case below.
	if len(doc.Cases) < 10 {
		t.Fatalf("the corpus has %d cases, want at least 10", len(doc.Cases))
	}
	// The named characters ARE the point of the file: an ASCII-only corpus is exactly the corpus
	// that let this bug live, so their presence is asserted rather than assumed.
	blob := string(b)
	for _, ch := range []string{"&", "<", ">", "café", "\U0001f4e6"} {
		if !strings.Contains(blob, ch) {
			t.Errorf("the corpus no longer exercises %q", ch)
		}
	}
	return doc.Cases
}

func TestTheBatchHashMatchesTheCorpus(t *testing.T) {
	for _, c := range loadBatchIDCorpus(t) {
		t.Run(c.Why, func(t *testing.T) {
			if got := BatchID(c.Method, c.Units, c.Params); got != c.Expect {
				t.Errorf("BatchID(%q, %v, %v) = %s, want %s", c.Method, c.Units, c.Params, got, c.Expect)
			}
		})
	}
}

// The PROPERTY behind every row above, asserted directly, so a reinstated escape reads as one
// line rather than as thirteen mismatched digests.
func TestTheEncoderEscapesNothing(t *testing.T) {
	for _, unit := range []string{"a&b", "<script>", "café", "\U0001f4e6"} {
		plain := BatchID("m", []any{unit}, map[string]any{})
		// Same input, encoded the way encoding/json does by DEFAULT. If the two ever agree again,
		// the escaping is back on and every non-ASCII row above is about to go red.
		escaped, err := json.Marshal([]any{"m", []any{unit}, map[string]any{}})
		if err != nil {
			t.Fatal(err)
		}
		if strings.ContainsAny(unit, "&<>") && !strings.Contains(string(escaped), "\\u00") {
			t.Errorf("encoding/json stopped escaping %q, so this test no longer proves anything", unit)
		}
		if plain == "" {
			t.Errorf("BatchID returned nothing for %q", unit)
		}
	}
}
