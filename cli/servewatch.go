package main

// servewatch.go — `kontra serve --actor <dir> --watch`: save a line, and the next dispatch runs it.
//
// WHAT STANDS BETWEEN A SAVED LINE AND A RUN, and it is not a build. `--mode local` executes
// `python <dir>/actor.py` straight from the directory; the Go handler is generic and parameterised
// by `KONTRA_ACTOR_NAME`/`_VERSION`, so it is never rebuilt per actor; and the task queue is
// `<name>-<version>` off the manifest, so an edit does not move it. The only thing that has to
// happen is the pair re-execing. That is a file watcher, not a pipeline.
//
// ── WHY THIS RESTARTS THE WORKER WHEN `workflow serve --watch` DELIBERATELY DOES NOT ────────────
//
// `internals/wfwatch.py` re-registers a workflow's contract on save and says, in capitals, that the
// worker is never touched: "running the new code is still an explicit re-serve". That is correct
// FOR A WORKFLOW. Workflow code is replayed against its own history, so swapping it under a live
// execution is how a non-determinism error is manufactured.
//
// AN ACTOR IS AN ACTIVITY WORKER, and activity code is never replayed. A new attempt simply runs
// whatever the worker now holds. So a restart here is safe in the way it is not there, and the two
// commands differ on purpose rather than by oversight.
//
// ── THE PAIR RESTARTS AS A PAIR ─────────────────────────────────────────────────────────────────
//
// `serve.go`'s header states the invariant: "Whichever half dies takes the other with it. A
// half-dead worker is worse than a dead one: it keeps its Temporal lease and units time out one by
// one." A reload that restarted the actor and left the handler would manufacture exactly that, on
// every save. `Stop` takes the pair down and `Start` brings the pair up; there is no path here that
// touches one.
//
// ── mtime POLLING, NO NEW DEPENDENCY ────────────────────────────────────────────────────────────
//
// The same discipline `wfwatch.py` keeps for the same job: "a one-second poll against a handful of
// files is not a cost worth a watchdog for". An actor directory is small, and fsnotify would be a
// dependency, a platform matrix and an editor-specific event storm in exchange for latency nobody
// can feel.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/medmahmoudi26/kontra/cli/internal/cliio"
	"github.com/medmahmoudi26/kontra/cli/warden"
)

// watchPoll is how often the directory is re-stat'd, and watchSettle is how long it must stop
// changing before a restart. An editor writing through a temporary file produces several events per
// save — a settle window turns those into one reload instead of three.
const (
	watchPoll   = 700 * time.Millisecond
	watchSettle = 400 * time.Millisecond
)

// watchIgnored is the set a save must never trigger on.
//
// `__pycache__` is the one that matters: importing the actor WRITES it, so without this the first
// reload's own import triggers the second, which triggers the third. A watcher that reloads forever
// looks exactly like a watcher that works, until you read the log.
var watchIgnored = map[string]bool{
	"__pycache__": true, ".venv": true, "venv": true, ".git": true,
	".pytest_cache": true, ".mypy_cache": true, ".ruff_cache": true,
	"node_modules": true, ".idea": true, ".vscode": true,
}

// snapshot is path → modification time for everything under an actor directory that counts as code.
type snapshot map[string]time.Time

func takeSnapshot(root string) (snapshot, error) {
	out := snapshot{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // a file that vanished mid-walk is a save in progress, not a failure
		}
		if d.IsDir() {
			if watchIgnored[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		// Editor scratch files: vim's `4913`, `.swp`, emacs `#file#`, and anything ending `~`.
		name := d.Name()
		if strings.HasSuffix(name, "~") || strings.HasSuffix(name, ".swp") ||
			strings.HasPrefix(name, ".#") || (strings.HasPrefix(name, "#") && strings.HasSuffix(name, "#")) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		out[p] = info.ModTime()
		return nil
	})
	return out, err
}

// changedBetween names what moved, for the line printed on reload. An operator who sees "reloading"
// with no reason cannot tell a real save from a watcher misfiring on its own cache writes.
func changedBetween(before, after snapshot) []string {
	var changed []string
	for p, t := range after {
		if old, ok := before[p]; !ok || !old.Equal(t) {
			changed = append(changed, p)
		}
	}
	for p := range before {
		if _, ok := after[p]; !ok {
			changed = append(changed, p+" (removed)")
		}
	}
	sort.Strings(changed)
	return changed
}

// watchForChanges polls `root` and sends the changed paths once the directory has settled.
func watchForChanges(ctx context.Context, root string, out chan<- []string) {
	last, _ := takeSnapshot(root)
	ticker := time.NewTicker(watchPoll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		cur, err := takeSnapshot(root)
		if err != nil || len(changedBetween(last, cur)) == 0 {
			continue
		}
		// Settle: keep re-snapshotting until nothing moves for a full window, so one save is one
		// reload however many files the editor touched on its way.
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(watchSettle):
			}
			next, err := takeSnapshot(root)
			if err != nil {
				break
			}
			if len(changedBetween(cur, next)) == 0 {
				break
			}
			cur = next
		}
		changed := changedBetween(last, cur)
		last = cur
		select {
		case out <- changed:
		case <-ctx.Done():
			return
		}
	}
}

// serveWatchLoop supervises the pair and re-execs it on every settled change, until ctx is done.
//
// Returns the same thing the non-watching path returns: a half exiting on its own is still the end
// of the command, because that is a crash rather than a reload and hiding it behind a restart would
// turn a broken actor into an infinite loop nobody is told about.
func serveWatchLoop(
	ctx context.Context,
	drv *warden.ProcessDriver,
	spec warden.Spec,
	actorDir string,
	announce func(),
) error {
	h, err := drv.Start(ctx, spec)
	if err != nil {
		return err
	}
	announce()
	fmt.Fprintf(cliio.Stdout, "watching %s — save to reload\n", actorDir)

	changes := make(chan []string, 1)
	go watchForChanges(ctx, actorDir, changes)

	// down records that the last reload failed to come back up, so the next save is reported as a
	// recovery attempt rather than as an ordinary reload — and so the operator is told, once, that
	// nothing is serving right now.
	down := false

	for {
		select {
		case err := <-drv.Exited(h):
			if down {
				// Nothing was running; this is the stale exit of the pair we already reported.
				continue
			}
			_ = drv.Stop(context.Background(), h, 0)
			return err

		case <-ctx.Done():
			fmt.Fprintln(cliio.Stdout, "\nstopping…")
			if !down {
				if err := drv.Stop(context.Background(), h, serveDrain); err != nil {
					fmt.Fprintf(cliio.Stderr, "note: %v\n", err)
				}
			}
			return nil

		case changed := <-changes:
			fmt.Fprintf(cliio.Stdout, "\n── reload: %s\n", summarise(changed, actorDir))
			if !down {
				// DRAIN, not kill. `serveDrain` is what a Ctrl-C gets, and a Unit that is mid-flight
				// deserves the same on a save — a reload that drops work teaches the wrong thing
				// about what the runtime guarantees.
				if err := drv.Stop(context.Background(), h, serveDrain); err != nil {
					fmt.Fprintf(cliio.Stderr, "  note: %v\n", err)
				}
			}
			next, err := drv.Start(ctx, spec)
			if err != nil {
				// A Go actor's compile error lands here. Say so and KEEP WATCHING: the next save is
				// very likely the fix, and exiting would make the operator restart the command to
				// recover from a typo.
				down = true
				fmt.Fprintf(cliio.Stderr, "  the worker did NOT come back: %v\n", err)
				fmt.Fprintln(cliio.Stderr, "  nothing is serving this actor until the next save fixes it")
				continue
			}
			h, down = next, false
			announce()
		}
	}
}

// summarise keeps the reload line short: paths relative to the actor directory, and a count once
// there are more than a handful.
func summarise(changed []string, root string) string {
	rel := make([]string, 0, len(changed))
	for _, p := range changed {
		if r, err := filepath.Rel(root, p); err == nil && !strings.HasPrefix(r, "..") {
			rel = append(rel, r)
		} else {
			rel = append(rel, p)
		}
	}
	if len(rel) > 3 {
		return fmt.Sprintf("%s and %d more", strings.Join(rel[:3], ", "), len(rel)-3)
	}
	return strings.Join(rel, ", ")
}
