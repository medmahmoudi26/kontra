package unitstore

import (
	"encoding/json"
	"os"
	"testing"
)

// The blob layout is a CROSS-SDK contract (ADR 0015): the Go and Python hosts must produce the
// same key for the same inputs, or the reader silently sees two layouts and half the data goes
// missing. They have already drifted once — the isolation counters shipped Go-only, leaving the
// crawler (the actor that actually lost ~1,400 seeds) uninstrumented.
//
// Both suites assert against conformance/blobkey.json, so a divergence is a failing
// test rather than something discovered months later in production data.
func TestBlobKeyMatchesCrossSDKFixture(t *testing.T) {
	b, err := os.ReadFile("../../../conformance/blobkey.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var fx struct {
		DT    string `json:"dt"`
		Cases []struct {
			Why    string `json:"why"`
			Actor  string `json:"actor"`
			Run    string `json:"run"`
			Node   string `json:"node"`
			Unit   int    `json:"unit"`
			Sha    string `json:"sha"`
			Expect string `json:"expect"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(b, &fx); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	when := fx.DT
	if len(fx.Cases) == 0 {
		t.Fatal("fixture is empty — a vacuously passing conformance test is worse than none")
	}
	for _, c := range fx.Cases {
		got := BlobKey(when, c.Actor, c.Run, c.Node, c.Unit, c.Sha)
		if got != c.Expect {
			t.Errorf("%s:\n  got  %s\n  want %s", c.Why, got, c.Expect)
		}
	}
}
