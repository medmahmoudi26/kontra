package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// The CLI ARM of shared/conformance/queues.json — and it is the arm that did not exist.
//
// `sharedQueue` in api.go carried the comment "byte-for-byte the peer of
// handler/internal/identity.SharedQueue and orchestrator taskQueue()", and nothing in this suite
// ever ran it against either. Its ONE appearance was the contrast half of an assertion about
// workflow queues (`workflowQueue(...) == sharedQueue(...)` must be false), which proves the two
// differ and says nothing about whether this one is right. So the eight-way contract was a
// seven-way contract with a comment on the eighth, in the binary an operator actually types.
//
// The tmux session name is here for the same reason. `kontra serve --tmux` mints it, the Monitor
// looks a pane up by it, and the browser derives it a third time — and when they disagree the
// Worker runs perfectly while its tile says NO SESSION, which is the exact thing ADR 0020 says a
// tile may never say.

type queueCase struct {
	Why       string `json:"why"`
	Name      string `json:"name"`
	Version   string `json:"version"`
	SessionID string `json:"session_id"`
	Expect    string `json:"expect"`
}

type tmuxSessionCase struct {
	Why     string `json:"why"`
	Actor   string `json:"actor"`
	Version string `json:"version"`
	Tag     string `json:"tag"`
	Worker  string `json:"worker"`
	Machine string `json:"machine"`
}

type queueCorpus struct {
	Shared struct {
		Cases []queueCase `json:"cases"`
	} `json:"shared"`
	TmuxSession struct {
		Cases []tmuxSessionCase `json:"cases"`
	} `json:"tmux_session"`
}

// ../shared/conformance/queues.json — cli -> <repo root>.
func loadQueueCorpus(t *testing.T) *queueCorpus {
	t.Helper()
	raw, err := os.ReadFile("../shared/conformance/queues.json")
	if err != nil {
		t.Fatalf("read the corpus: %v", err)
	}
	var doc queueCorpus
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse the corpus: %v", err)
	}
	if len(doc.Shared.Cases) < 6 || len(doc.TmuxSession.Cases) < 8 {
		t.Fatalf("the corpus shrank: shared=%d tmux=%d",
			len(doc.Shared.Cases), len(doc.TmuxSession.Cases))
	}
	blob := string(raw)
	for _, want := range []string{"my actor", "-shared", "web crawl; reboot", `"fleet"`, `"actor"`} {
		if !strings.Contains(blob, want) {
			t.Errorf("the corpus no longer exercises %q", want)
		}
	}
	return &doc
}

func TestTheSharedQueueMatchesTheCorpus(t *testing.T) {
	for _, c := range loadQueueCorpus(t).Shared.Cases {
		t.Run(c.Why, func(t *testing.T) {
			if got := sharedQueue(c.Name, c.Version); got != c.Expect {
				t.Errorf("sharedQueue(%q,%q) = %q, want %q", c.Name, c.Version, got, c.Expect)
			}
		})
	}
}

// The two tmux derivations this binary owns, against the corpus's two answers per row.
//
// `worker` is an ACTOR's Worker — a name and a version and nothing else, and a fallback that
// exists only so the string stays attachable. `machine` is a fleet MACHINE's session, which is
// also given the fleet's tag, because a `fleet up` with no Actor placed on it still has
// Terminals. The two fallbacks differ ON PURPOSE and the corpus says why; what must not differ
// is either one of them from its peer across the language boundary.
func TestTheTmuxSessionNamesMatchTheCorpus(t *testing.T) {
	for _, c := range loadQueueCorpus(t).TmuxSession.Cases {
		t.Run(c.Why, func(t *testing.T) {
			if got := tmuxSession(c.Actor, c.Version); got != c.Worker {
				t.Errorf("tmuxSession(%q,%q) = %q, want %q", c.Actor, c.Version, got, c.Worker)
			}
			if got := fleetSessionName(c.Actor, c.Version, c.Tag); got != c.Machine {
				t.Errorf("fleetSessionName(%q,%q,%q) = %q, want %q",
					c.Actor, c.Version, c.Tag, got, c.Machine)
			}
		})
	}
}

// A Machine's session name reaches a command run as root over ssh, so every answer this binary
// produces must survive the whitelist the backend applies before it interpolates one. Asserted
// over the corpus rather than on the three rows somebody thought of.
func TestEveryMachineSessionNameIsSafeToInterpolate(t *testing.T) {
	for _, c := range loadQueueCorpus(t).TmuxSession.Cases {
		if !safeSessionName.MatchString(c.Machine) {
			t.Errorf("%s: %q would be refused by panels/ids.ts:assertSafe('session')", c.Why, c.Machine)
		}
	}
}
