package temporalhost

import (
	"encoding/json"
	"reflect"
	"testing"
)

// THE TOPIC IS THE ADDRESS A CONSOLE GROUPS BY, and it is built from two strings either of which
// can be empty in an ordinary run.
func TestMethodTopic(t *testing.T) {
	for _, c := range []struct{ actor, method, want string }{
		{"webcrawl", "crawl", "webcrawl/crawl"},
		{"gocanary", "tick", "gocanary/tick"},
		// An actor with ONE Method is dispatched with no name on the wire (ADR 0023 §16), so the
		// resolved name arrives empty. `<actor>/` would read as a typo rather than as the
		// sole-Method case it is.
		{"desync", "", "desync/run"},
		// No actor name at all should still address something a subscriber can ask for.
		{"", "crawl", "actor/crawl"},
	} {
		if got := methodTopic(c.actor, c.method); got != c.want {
			t.Errorf("methodTopic(%q, %q) = %q, want %q", c.actor, c.method, got, c.want)
		}
	}
}

// `actor` MEANS THE SESSION'S ID IN BOTH LANGUAGES. Python's publisher has always sent
// `self._actor_id`; this host sent `h.name`, so one run's stream carried the same key meaning two
// different things depending on which leg wrote the record — `"actor": "c5eaf2b6a275"` from the
// Python canary and `"actor": "gocanary"` from the Go one, measured on canary-1789943224.
//
// The name is not lost by this: it is in the TOPIC, which is where a subscriber reads it from.
func TestTheRecordCarriesTheSessionIDNotTheActorName(t *testing.T) {
	body := streamBody("node-1", "c5eaf2b6a275", map[string]any{"label": "unit-3"})
	if body["actor"] != "c5eaf2b6a275" {
		t.Fatalf("actor must be the session id, got %v", body["actor"])
	}
	if body["node"] != "node-1" {
		t.Fatalf("node lost: %v", body["node"])
	}
	if body["label"] != "unit-3" {
		t.Fatalf("the author's field was dropped: %+v", body)
	}
}

func TestAnAuthorsOwnNodeFieldIsNotOverwritten(t *testing.T) {
	// A record that genuinely carries `node` means it. Overwriting it with the publishing worker
	// would be a lie the author cannot see.
	body := streamBody("worker-7", "sess", map[string]any{"node": "placed-on-9"})
	if body["node"] != "placed-on-9" {
		t.Fatalf("the author's node was overwritten: %v", body["node"])
	}
}

// A STRUCT IS THE ORDINARY CASE, and it must flatten to the same keys the catalog's `stream`
// schema declares — both come from these json tags, which is the whole reason asMapping goes
// through encoding/json rather than reflecting the struct by hand.
func TestAStructFlattensToItsJSONTags(t *testing.T) {
	type CrawlProgress struct {
		At       string `json:"at"`
		Contexts int    `json:"contexts"`
		Skipped  bool   `json:"skipped,omitempty"`
	}
	body := streamBody("n1", "s1", CrawlProgress{At: "https://example.com", Contexts: 2})
	want := map[string]any{
		"at": "https://example.com", "contexts": float64(2), "node": "n1", "actor": "s1",
	}
	if !reflect.DeepEqual(body, want) {
		t.Fatalf("got %+v, want %+v", body, want)
	}
}

// A VALUE THAT IS NOT AN OBJECT IS NESTED, NOT DROPPED. `kontra.Stream(s, "fetched "+url)` is a
// natural thing to try given the verb's name; returning the framework's two routing keys and
// nothing else would ship a record with the author's value silently gone.
func TestANonObjectRecordIsKeptUnderValue(t *testing.T) {
	body := streamBody("n1", "s1", "fetched https://example.com")
	if body["value"] != "fetched https://example.com" {
		t.Fatalf("a scalar record was lost: %+v", body)
	}
}

// EVERY RECORD MUST SURVIVE THE CONVERTER, because the publish is buffered and flushed later: a
// value that cannot be encoded fails at flush time, which is after the Method returned and
// nowhere near the line that wrote it.
func TestTheRecordSerialises(t *testing.T) {
	type TickProgress struct {
		Label string  `json:"label"`
		Done  int     `json:"done"`
		Slept float64 `json:"slept"`
	}
	raw, err := json.Marshal(streamBody("n1", "s1", TickProgress{Label: "unit-3", Done: 3, Slept: 4}))
	if err != nil {
		t.Fatalf("the stream record does not serialise: %v", err)
	}
	var back map[string]any
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back["label"] != "unit-3" || back["done"] != float64(3) {
		t.Fatalf("fields lost in transit: %s", raw)
	}
}

// A nil publisher is the ordinary case for an actor driven outside a hosted Run, and must be a
// no-op rather than a panic — an observability call cannot be the thing that fails a Batch.
func TestPublishingToNothingIsSafe(t *testing.T) {
	publishRecord(nil, "a/b", "n1", "s1", map[string]any{"x": 1})
	publishProgress(nil, "n1", "s1", map[string]any{"x": 1}, nil)
}
