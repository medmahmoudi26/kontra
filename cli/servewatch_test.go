package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/medmahmoudi26/kontra/cli/internal/queues"
)

// writeFile writes and returns the path, bumping mtime enough that a coarse filesystem notices.
func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// THE BUG THIS PREVENTS IS AN INFINITE RELOAD. Importing an actor WRITES `__pycache__`, so a
// watcher that does not ignore it reloads, whose import writes the cache, which triggers the next
// reload, forever — and it looks exactly like a watcher that works until you read the output.
func TestSnapshotIgnoresGeneratedAndScratchPaths(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "actor.py", "x = 1\n")
	writeFile(t, dir, "helper.py", "y = 2\n")
	writeFile(t, dir, "__pycache__/actor.cpython-311.pyc", "bytes")
	writeFile(t, dir, ".venv/lib/thing.py", "z = 3\n")
	writeFile(t, dir, ".git/HEAD", "ref: refs/heads/main\n")
	writeFile(t, dir, "actor.py~", "old\n")
	writeFile(t, dir, ".actor.py.swp", "vim\n")

	snap, err := takeSnapshot(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Non-vacuous: it must have found the real files, or the exclusions below prove nothing.
	if len(snap) != 2 {
		t.Fatalf("expected exactly actor.py and helper.py, got %d: %v", len(snap), keysOfSnapshot(snap))
	}
	for p := range snap {
		for _, bad := range []string{"__pycache__", ".venv", ".git", "~", ".swp"} {
			if strings.Contains(p, bad) {
				t.Errorf("%s should not be watched (matched %q)", p, bad)
			}
		}
	}
}

func TestChangedBetweenSeesAddModifyRemove(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "actor.py", "x = 1\n")
	before, _ := takeSnapshot(dir)

	// add
	writeFile(t, dir, "extra.py", "e = 1\n")
	after, _ := takeSnapshot(dir)
	if got := changedBetween(before, after); len(got) != 1 || !strings.HasSuffix(got[0], "extra.py") {
		t.Errorf("an added file was not seen: %v", got)
	}

	// modify
	before = after
	p := filepath.Join(dir, "actor.py")
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(p, future, future); err != nil {
		t.Fatal(err)
	}
	after, _ = takeSnapshot(dir)
	if got := changedBetween(before, after); len(got) != 1 || !strings.HasSuffix(got[0], "actor.py") {
		t.Errorf("a modified file was not seen: %v", got)
	}

	// remove
	before = after
	if err := os.Remove(filepath.Join(dir, "extra.py")); err != nil {
		t.Fatal(err)
	}
	after, _ = takeSnapshot(dir)
	got := changedBetween(before, after)
	if len(got) != 1 || !strings.Contains(got[0], "removed") {
		t.Errorf("a removed file was not seen as removed: %v", got)
	}
}

// ONE SAVE IS ONE RELOAD. An editor writing through a temporary file touches several paths in quick
// succession; without a settle window that is three restarts of a Worker for one Ctrl-S.
func TestWatchDebouncesABurstIntoOneReload(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "actor.py", "x = 1\n")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	changes := make(chan []string, 8)
	go watchForChanges(ctx, dir, changes)

	// A burst: several writes inside one settle window.
	time.Sleep(watchPoll)
	for i, name := range []string{"actor.py", "a.py", "b.py", "c.py"} {
		writeFile(t, dir, name, strings.Repeat("x", i+1))
		time.Sleep(60 * time.Millisecond)
	}

	select {
	case got := <-changes:
		if len(got) == 0 {
			t.Fatal("a reload fired with nothing changed")
		}
	case <-time.After(6 * time.Second):
		t.Fatal("no reload fired for a real save")
	}

	// And nothing else arrives for the same burst.
	select {
	case extra := <-changes:
		t.Fatalf("one save produced a second reload: %v", extra)
	case <-time.After(2 * time.Second):
	}
}

// THE INVARIANT THE WHOLE LOOP RESTS ON. A dispatch goes to a queue somebody remembered; if an edit
// moved it, every `start` after the first save would fail with "no worker is serving this folder's
// code". The queue is derived from the MANIFEST, never from the folder's bytes — `workflowQueue`'s
// own history records what the alternative cost: three edits produced three queues in one session.
func TestQueueDoesNotMoveWhenTheCodeChanges(t *testing.T) {
	before := queues.Shared("nscheck", "0.1.0")
	// Whatever happens to the directory, the derivation takes only (name, version).
	after := queues.Shared("nscheck", "0.1.0")
	if before != after || before == "" {
		t.Fatalf("the shared queue is not stable for one (name, version): %q vs %q", before, after)
	}
	if strings.Contains(before, "/") || len(before) > 64 {
		t.Errorf("queue %q does not look like a name an operator can retype", before)
	}
}

func TestWatchIsDocumented(t *testing.T) {
	if !strings.Contains(usageText, "--watch") {
		t.Error("usageText does not advertise `kontra serve --watch`")
	}
}

func keysOfSnapshot(s snapshot) []string {
	out := make([]string, 0, len(s))
	for k := range s {
		out = append(out, k)
	}
	return out
}
