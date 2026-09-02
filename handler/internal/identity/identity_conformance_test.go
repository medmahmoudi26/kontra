package identity

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// The HANDLER ARM of shared/conformance/queues.json.
//
// WHY A CORPUS REPLACED THE TABLE THAT WAS HERE. The Actor's task queue was derived in eight
// places across four languages, the endpoint in four, the session queue in five — and three
// hand-copied golden tables held them, each one keeping itself "in sync" with the others by
// somebody remembering. The bookkeeping had already drifted before the code did: this file's
// comment said one thing, sdk/go/catalog said "a SIXTH independent derivation",
// backend/src/panels/pollers.ts said "a fourth", and none of the three was right.
//
// A drift has no loud failure mode. The actor registers, polls a queue nobody schedules onto, and
// reports as a healthy idle Worker while every run hangs to StartToClose.
//
// The corpus is the file both sides execute. handler/internal is not importable from the SDK
// modules and the SDKs are not importable from here — the decoupling rule — so the derivations
// stay separate and the FIXTURE is what makes them one contract.

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

// loadQueueCorpus reads shared/conformance/queues.json and refuses a corpus that shrank.
//
// The path is ../../../shared/conformance/queues.json — identity -> internal -> handler -> <repo root>.
// Built as ONE string on purpose: a path assembled from separate arguments is invisible to a
// regex sweep, which is how three drivers in this restructure quietly stopped finding their
// fixture. tests/test_conformance_tree.py checks both directions of that.
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
	// A corpus that silently shrank to nothing passes every case in it.
	if len(doc.Shared.Cases) < 6 || len(doc.Sessions.Cases) < 4 ||
		len(doc.Session.Cases) < 3 || len(doc.Endpoint.Cases) < 10 {
		t.Fatalf("the corpus shrank: shared=%d sessions=%d session=%d endpoint=%d",
			len(doc.Shared.Cases), len(doc.Sessions.Cases), len(doc.Session.Cases), len(doc.Endpoint.Cases))
	}
	// The inputs that BREAK are the point of the file. An empty version is the DIY path's whole
	// contract; a space and a non-ASCII rune are what separate an unsanitised queue name from a
	// sanitised endpoint name. A corpus of easy rows is the guard this one replaced.
	blob := string(raw)
	for _, want := range []string{"my actor", "café", "naïve", "\U0001f4e6", "-shared", "-sessions", "-s-"} {
		if !strings.Contains(blob, want) {
			t.Errorf("the corpus no longer exercises %q", want)
		}
	}
	if !hasEmptyVersion(doc.Shared.Cases) {
		t.Error("the corpus no longer has an empty-version case, which is the DIY/CLI path")
	}
	return &doc
}

func hasEmptyVersion(cases []queueCase) bool {
	for _, c := range cases {
		if c.Version == "" {
			return true
		}
	}
	return false
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
			// The workflow's own path: it holds only its task queue, so this is the derivation
			// that actually schedules RunBatch. Both spellings, one answer.
			if got := SessionsQueueOf(SharedQueue(c.Name, c.Version)); got != c.Expect {
				t.Errorf("SessionsQueueOf(%q) = %q, want %q", SharedQueue(c.Name, c.Version), got, c.Expect)
			}
		})
	}

	for _, c := range doc.Session.Cases {
		t.Run("session/"+c.Why, func(t *testing.T) {
			// SessionQueue takes the SHARED queue, because its only caller is workflow code
			// reading its own task queue off the context.
			if got := SessionQueue(SharedQueue(c.Name, c.Version), c.SessionID); got != c.Expect {
				t.Errorf("SessionQueue(%q,%q) = %q, want %q",
					SharedQueue(c.Name, c.Version), c.SessionID, got, c.Expect)
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

// The Go idiom for the corpus's `refuses_without_a_session_id`: the empty string, which no caller
// can mistake for a queue. Python raises instead; what both sides must agree on is that
// "{shared}-s-" is never produced, because it is a real queue every Session would share.
func TestNoSessionIDIsNoQueue(t *testing.T) {
	doc := loadQueueCorpus(t)
	r := doc.Session.Refusal
	if r.SessionID != "" {
		t.Fatalf("the corpus's refusal row carries a session id (%q), so it proves nothing", r.SessionID)
	}
	if got := SessionQueue(SharedQueue(r.Name, r.Version), r.SessionID); got != "" {
		t.Errorf("SessionQueue with no id = %q, want the empty string", got)
	}
}

// The two planes must not collide. One Session's queue is polled by exactly one worker; the
// actor's sessions queue is polled by all of them — so a call that landed on the wrong one runs
// against a process that never activated the Session and holds none of its state, and REPORTS
// SUCCESS. Asserted as a property over the corpus rather than on one hand-picked pair.
func TestASessionQueueIsNeverTheSharedSessionsQueue(t *testing.T) {
	doc := loadQueueCorpus(t)
	for _, c := range doc.Session.Cases {
		shared := SharedQueue(c.Name, c.Version)
		if SessionQueue(shared, c.SessionID) == SessionsQueueOf(shared) {
			t.Errorf("%s: a Session's queue collides with the actor's shared sessions queue", c.Why)
		}
	}
}
