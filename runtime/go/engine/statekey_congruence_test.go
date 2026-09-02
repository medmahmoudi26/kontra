package engine

import (
	"strings"
	"testing"
)

// Cross-SDK congruence guard for the COMMIT KEY (ADR 0023 §17). Both SDKs key a committed Unit by
// the Batch's content hash plus its index — Python's internals/engine.py `unit_slot(batch_id(...),
// i)`, this host's unitSlot(BatchID(...), i) — and both hang the Unit's resume scratch off that
// same slot with a `-ckpt` suffix (ADR 0015).
//
// The two SDKs share no code (the decoupling rule), so this pins the Go side; keep in sync with
// the Python peer in tests/test_state_key_congruence.py.
func TestTheCommitKeyIsTheBatchHashPlusTheIndex(t *testing.T) {
	slot := unitSlot("deadbeefdeadbeef", 3)
	if slot != "deadbeefdeadbeef-u3" {
		t.Errorf("Go commit key = %q, want deadbeefdeadbeef-u3", slot)
	}
	if key := slot + ckptSuffix; key != "deadbeefdeadbeef-u3-ckpt" {
		t.Errorf("Go scratch key = %q, want deadbeefdeadbeef-u3-ckpt", key)
	}
	if !strings.HasSuffix(slot+ckptSuffix, "-ckpt") {
		t.Error("the scratch key must carry the shared -ckpt suffix")
	}
}

// THE HASH GOLDEN THAT USED TO SIT HERE IS GONE. It asserted two digests over plain-ASCII
// inputs and claimed "a drift in either encoder shows up here", which was false: plain ASCII is
// the one input class where Go's HTML escaping and Python's non-ASCII escaping cannot differ.
// shared/conformance/batchid.json replaced it, driven from both SDKs — see batchid_conformance_test.go.

// §17's two properties, stated as behaviour rather than as a string: the same Batch hashes the
// same on every attempt (which is what makes a retry a resume), and any difference in what was
// asked for is a different Batch (which is what stops one replaying the other's outputs).
func TestTheBatchHashIsStableAcrossRetriesAndDistinctAcrossBatches(t *testing.T) {
	units := []any{map[string]any{"url": "a"}}
	params := map[string]any{"depth": 2}
	base := BatchID("crawl", units, params)

	if again := BatchID("crawl", units, params); again != base {
		t.Errorf("a retry of the same Batch hashed %s then %s — the commit map would never skip", base, again)
	}
	for _, c := range []struct {
		why    string
		method string
		units  []any
		params map[string]any
	}{
		{"a different Method", "extract", units, params},
		{"different Units", "crawl", []any{map[string]any{"url": "z"}}, params},
		{"different params", "crawl", units, map[string]any{"depth": 3}},
	} {
		if got := BatchID(c.method, c.units, c.params); got == base {
			t.Errorf("%s hashed to the same Batch (%s) — its Units would replay the first's outputs", c.why, got)
		}
	}
}
