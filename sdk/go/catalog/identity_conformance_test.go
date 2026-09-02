package catalog

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// The GO CALLER-SIDE ARM of shared/conformance/queues.json.
//
// WHAT THIS REPLACED. A table of goldens "taken verbatim from the Python peer", kept correct by
// somebody copying them across a language boundary and remembering to do it again. Its own header
// called this package "a SIXTH independent derivation"; the peer comments in
// backend/src/panels/pollers.ts and backend/src/nexusRegistry.ts said "a fourth" and "THE FIFTH".
// Three counts of one thing, none of them right, and nothing that could ever fail because of it.
//
// A drift here does not error anywhere. The dispatch goes to a task queue nobody polls and the
// workflow waits until ScheduleToStart fires, which presents as a hung scope rather than as an
// error naming a queue.

type queueCase struct {
	Why       string `json:"why"`
	Name      string `json:"name"`
	Version   string `json:"version"`
	SessionID string `json:"session_id"`
	Expect    string `json:"expect"`
}

type queueCorpus struct {
	Shared struct {
		Cases []queueCase `json:"cases"`
	} `json:"shared"`
	Sessions struct {
		Cases []queueCase `json:"cases"`
	} `json:"sessions"`
	Session struct {
		Cases   []queueCase `json:"cases"`
		Refusal queueCase   `json:"refuses_without_a_session_id"`
	} `json:"session"`
	Endpoint struct {
		Servable string      `json:"servable"`
		Cases    []queueCase `json:"cases"`
	} `json:"endpoint"`
}

// ../../../shared/conformance/queues.json — catalog -> go -> sdk -> <repo root>. One string, not a
// filepath.Join argument list: a path built from separate arguments is invisible to a regex sweep,
// which is how three drivers in this restructure quietly stopped finding their fixture.
func loadQueueCorpus(t *testing.T) *queueCorpus {
	t.Helper()
	raw, err := os.ReadFile("../../../shared/conformance/queues.json")
	if err != nil {
		t.Fatalf("read the corpus: %v", err)
	}
	var doc queueCorpus
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse the corpus: %v", err)
	}
	if len(doc.Shared.Cases) < 6 || len(doc.Sessions.Cases) < 4 ||
		len(doc.Session.Cases) < 3 || len(doc.Endpoint.Cases) < 10 {
		t.Fatalf("the corpus shrank: shared=%d sessions=%d session=%d endpoint=%d",
			len(doc.Shared.Cases), len(doc.Sessions.Cases), len(doc.Session.Cases), len(doc.Endpoint.Cases))
	}
	// The rows that BREAK are the point of the file: an empty version (the DIY path), a space and
	// a non-ASCII rune (which the queue keeps and the endpoint mangles), and an emoji, whose two
	// UTF-16 code units are where the TypeScript peer could have counted differently.
	blob := string(raw)
	for _, want := range []string{"my actor", "café", "naïve", "\U0001f4e6", "-shared", "-sessions", "-s-"} {
		if !strings.Contains(blob, want) {
			t.Errorf("the corpus no longer exercises %q", want)
		}
	}
	return &doc
}

func TestTheQueuesMatchTheCorpus(t *testing.T) {
	doc := loadQueueCorpus(t)

	for _, c := range doc.Shared.Cases {
		t.Run("shared/"+c.Why, func(t *testing.T) {
			if got := SharedQueue(c.Name, c.Version); got != c.Expect {
				t.Errorf("SharedQueue(%q,%q) = %q, want %q", c.Name, c.Version, got, c.Expect)
			}
		})
	}
	for _, c := range doc.Sessions.Cases {
		t.Run("sessions/"+c.Why, func(t *testing.T) {
			if got := SessionsQueue(c.Name, c.Version); got != c.Expect {
				t.Errorf("SessionsQueue(%q,%q) = %q, want %q", c.Name, c.Version, got, c.Expect)
			}
		})
	}
	for _, c := range doc.Session.Cases {
		t.Run("session/"+c.Why, func(t *testing.T) {
			if got := SessionQueue(c.Name, c.Version, c.SessionID); got != c.Expect {
				t.Errorf("SessionQueue(%q,%q,%q) = %q, want %q",
					c.Name, c.Version, c.SessionID, got, c.Expect)
			}
		})
	}
	for _, c := range doc.Endpoint.Cases {
		t.Run("endpoint/"+c.Why, func(t *testing.T) {
			if got := EndpointName(c.Name, c.Version); got != c.Expect {
				t.Errorf("EndpointName(%q,%q) = %q, want %q", c.Name, c.Version, got, c.Expect)
			}
		})
	}
}

// The corpus's `refuses_without_a_session_id`, in Go's idiom. Empty rather than a queue named
// `…-s-`, which would be a real queue that every Session of this actor would share.
func TestASessionQueueNeedsASessionID(t *testing.T) {
	r := loadQueueCorpus(t).Session.Refusal
	if r.SessionID != "" {
		t.Fatalf("the corpus's refusal row carries a session id (%q), so it proves nothing", r.SessionID)
	}
	if got := SessionQueue(r.Name, r.Version, r.SessionID); got != "" {
		t.Errorf("SessionQueue with no id = %q, want empty", got)
	}
}

// Every endpoint the corpus names must be one the cluster will actually accept. The regex is the
// corpus's, not this file's: the dev server enforces it, and a name that fails it is refused at
// create time with a message about the NAME rather than about the missing version.
func TestEveryEndpointNameIsServable(t *testing.T) {
	doc := loadQueueCorpus(t)
	if doc.Endpoint.Servable == "" {
		t.Fatal("the corpus no longer states the pattern the cluster enforces")
	}
	for _, c := range doc.Endpoint.Cases {
		if !servable(c.Expect) {
			t.Errorf("%s: %q is not a name the cluster will accept", c.Why, c.Expect)
		}
	}
}

// servable is `^[a-zA-Z][a-zA-Z0-9-]*[a-zA-Z0-9]$` without a regexp dependency in the SDK's test
// binary — the pattern is short enough that reading it beats importing it.
func servable(s string) bool {
	if len(s) < 2 {
		return false
	}
	alpha := func(b byte) bool { return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') }
	digit := func(b byte) bool { return b >= '0' && b <= '9' }
	if !alpha(s[0]) || !(alpha(s[len(s)-1]) || digit(s[len(s)-1])) {
		return false
	}
	for i := 1; i < len(s)-1; i++ {
		if !alpha(s[i]) && !digit(s[i]) && s[i] != '-' {
			return false
		}
	}
	return true
}
