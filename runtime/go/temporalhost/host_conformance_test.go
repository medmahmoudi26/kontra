package temporalhost

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// The GO ACTOR-HOST ARM of conformance/queues.json.
//
// THIS IS THE DERIVATION THE FAILURE MODE IS NAMED AFTER. The host binds TaskQueue and polls it;
// the handler builds the same string from its workflow's OWN task queue, because workflow code
// may not read env and stay deterministic. A drift does not error anywhere — the actor registers,
// polls a queue nobody schedules onto, and reports as a healthy idle Worker while every run hangs
// to StartToClose.
//
// WHAT THIS REPLACED. A four-row table with the instruction "keep this table in sync with
// tests/test_queue_congruence.py" — which is a contract enforced by remembering, in a repo where
// the three comments counting these derivations disagreed with each other about how many there
// were. The corpus is the file both sides execute instead.

type queueCase struct {
	Why       string `json:"why"`
	Name      string `json:"name"`
	Version   string `json:"version"`
	SessionID string `json:"session_id"`
	Expect    string `json:"expect"`
}

type queueCorpus struct {
	Sessions struct {
		Cases []queueCase `json:"cases"`
	} `json:"sessions"`
	Session struct {
		Cases   []queueCase `json:"cases"`
		Refusal queueCase   `json:"refuses_without_a_session_id"`
	} `json:"session"`
}

// ../../../conformance/queues.json — temporalhost -> go -> runtime -> <repo root>.
func loadQueueCorpus(t *testing.T) *queueCorpus {
	t.Helper()
	raw, err := os.ReadFile("../../../conformance/queues.json")
	if err != nil {
		t.Fatalf("read the corpus: %v", err)
	}
	var doc queueCorpus
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse the corpus: %v", err)
	}
	if len(doc.Sessions.Cases) < 4 || len(doc.Session.Cases) < 3 {
		t.Fatalf("the corpus shrank: sessions=%d session=%d", len(doc.Sessions.Cases), len(doc.Session.Cases))
	}
	// An empty version is the DIY/CLI path and the row this derivation is most likely to get
	// wrong — `{name}-sessions` instead of `{name}-shared-sessions` reads fine and polls nothing.
	blob := string(raw)
	for _, want := range []string{"-shared-sessions", "-s-", "my actor"} {
		if !strings.Contains(blob, want) {
			t.Errorf("the corpus no longer exercises %q", want)
		}
	}
	return &doc
}

func TestTheHostBindsTheQueuesInTheCorpus(t *testing.T) {
	doc := loadQueueCorpus(t)
	for _, c := range doc.Sessions.Cases {
		t.Run("sessions/"+c.Why, func(t *testing.T) {
			if got := TaskQueue(c.Name, c.Version); got != c.Expect {
				t.Errorf("TaskQueue(%q,%q) = %q, want %q", c.Name, c.Version, got, c.Expect)
			}
		})
	}
	for _, c := range doc.Session.Cases {
		t.Run("session/"+c.Why, func(t *testing.T) {
			if got := SessionTaskQueue(c.Name, c.Version, c.SessionID); got != c.Expect {
				t.Errorf("SessionTaskQueue(%q,%q,%q) = %q, want %q",
					c.Name, c.Version, c.SessionID, got, c.Expect)
			}
		})
	}
}

// The corpus's `refuses_without_a_session_id`, in Go's idiom. OpenSession already refuses a blank
// id with an error; this is the second lock, so no other path can turn one into an address.
func TestNoSessionIDIsNoQueue(t *testing.T) {
	r := loadQueueCorpus(t).Session.Refusal
	if r.SessionID != "" {
		t.Fatalf("the corpus's refusal row carries a session id (%q), so it proves nothing", r.SessionID)
	}
	if got := SessionTaskQueue(r.Name, r.Version, r.SessionID); got != "" {
		t.Errorf("SessionTaskQueue with no id = %q, want the empty string", got)
	}
}
