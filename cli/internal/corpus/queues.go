// Package corpus loads the conformance corpora for the Go suites that drive them.
//
// IT IS A PACKAGE BECAUSE TWO SUITES DRIVE THE SAME FILE. `queues.json` is executed by the CLI's
// own arm and by the Warden's — the tmux session name is derived on the Machine — and a loader in
// one `_test.go` is reachable from neither of the others. Copying it would give the two arms two
// readings of one corpus, which is the drift a corpus exists to prevent.
//
// THE PATH IS RESOLVED FROM THIS FILE, not from the caller's directory. A relative
// `../shared/conformance/…` is correct in `cli/` and wrong in `cli/warden/`, and that is a class of
// bug this repository has hit five times — see tests/test_conformance_tree.py.
package corpus

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func path() string {
	_, self, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(self), "..", "..", "..", "shared", "conformance", "queues.json")
}

// tile may never say.

type Case struct {
	Why       string `json:"why"`
	Name      string `json:"name"`
	Version   string `json:"version"`
	SessionID string `json:"session_id"`
	Expect    string `json:"expect"`
}

type TmuxCase struct {
	Why     string `json:"why"`
	Actor   string `json:"actor"`
	Version string `json:"version"`
	Tag     string `json:"tag"`
	Worker  string `json:"worker"`
	Machine string `json:"machine"`
}

type Queues struct {
	Shared struct {
		Cases []Case `json:"cases"`
	} `json:"shared"`
	TmuxSession struct {
		Cases []TmuxCase `json:"cases"`
	} `json:"tmux_session"`
}

// ../shared/conformance/queues.json — cli -> <repo root>.
func LoadQueues(t *testing.T) *Queues {
	t.Helper()
	raw, err := os.ReadFile(path())
	if err != nil {
		t.Fatalf("read the corpus: %v", err)
	}
	var doc Queues
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
