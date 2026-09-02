// identity_test.go — the derivations in identity.go: an Actor's queue, a Workflow's queue, the
// content digests the second is built on, and the resolution from what an operator typed to the
// file all of them are derived from.
//
// The CROSS-LANGUAGE half of this file is not here. `sharedQueue` is driven against
// conformance/queues.json by queues_conformance_test.go, and the folder digest is pinned against
// backend/src/sources.test.ts by the shared fixture below. What is here is the local half: the
// collision the `wf-` prefix exists to prevent, the refusals, and the one-file-one-worker rule.
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// THE QUEUE IS DERIVED, AND IT MUST NOT COLLIDE WITH AN ACTOR'S.
//
// A queue a string can name goes stale silently: `--queue recon` outlived the name, so a run
// started, routed to a queue nobody served, and reported `running` forever. Temporal counted it —
// `no_poller_tasks{taskqueue="recon"} = 12`, read off the live cluster — and no surface here read
// that number. Deriving from the folder's CONTENT DIGEST is what makes the queue untypable and
// unable to drift from the code, replacing the earlier `(name, version)` which was still a free
// string on a folder (edit the code, keep the version, keep the queue).
//
// The `wf-` prefix is the part with teeth. See workflowQueue's comment: this repo ships an actor
// and a workflow both named `nscheck`, the actor's handler polls its shared queue for workflow
// tasks, and without separate namespaces a `NsCheck` task lands on a worker that cannot run it.
// The two derivations are in ONE file now, which is what makes this assertion readable as
// something other than a coincidence between two packages.
func TestWorkflowQueueIsDerivedAndCannotCollideWithAnActor(t *testing.T) {
	const digest12 = "0f89cbf60a37"
	if got, want := workflowQueue("nscheck", digest12), "wf-nscheck-"+digest12; got != want {
		t.Fatalf("workflowQueue = %q, want %q", got, want)
	}

	// The collision this prefix exists for, spelled out: same name, two kinds. An actor's shared
	// queue can never wear the `wf-` prefix, so no digest value can make the two collide.
	if workflowQueue("nscheck", digest12) == sharedQueue("nscheck", "0.1.0") {
		t.Fatal("a workflow and an actor of the same name resolved to ONE queue — " +
			"the actor's handler polls that queue for workflow tasks and knows only its own type, " +
			"so every run of this workflow would fail its task and retry forever")
	}

	// Neither half alone is a queue: no name, or no digest, is a refusal — never `wf--x` or `wf-n-`.
	if workflowQueue("", digest12) != "" {
		t.Fatal("no name must not produce a queue")
	}
	if workflowQueue("ping", "") != "" {
		t.Fatal("no digest must not produce a queue — an unserved/unhashable folder is a refusal")
	}
}

// The queue is derived from the folder as it is on disk — the manifest names it, the CONTENT
// digests it, and there is no override to pass. The shared fixture pins Go's digest byte-for-byte
// against the TS peer in backend/src/sources.test.ts.
func TestQueueForWorkflowIsDerivedFromFolderContent(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The exact bytes hashed by the cross-language fixture — DO NOT edit without recomputing the
	// digest in both suites. See writeFixture below.
	writeFixture(t, dir)
	file := filepath.Join(dir, "workflow.py")

	got, err := queueForWorkflow(file)
	if err != nil {
		t.Fatalf("queueForWorkflow: %v", err)
	}
	// THE MANIFEST NAMES AND VERSIONS IT. This was `wf-canary-0f89cbf60a37`, the first 12 hex of
	// the folder's content digest, until the queue stopped being a hash — see the header on
	// `workflowQueue`. The fixture's manifest says version 0.2.0, and that is now the whole answer.
	if want := "wf-canary-0.2.0"; got != want {
		t.Fatalf("derived %q, want %q", got, want)
	}

	// NO MANIFEST IS A SENTENCE, NOT A FALLBACK. Serving on the SDK's `default` would share one
	// queue with every workflow anyone forgot to name — the same routing accident by another route.
	bare := filepath.Join(t.TempDir(), "workflow.py")
	if err := os.WriteFile(bare, []byte("# no manifest beside me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := queueForWorkflow(bare); err == nil {
		t.Fatal("a folder with no workflow.json must refuse, not fall back to `default`")
	}
}

// writeFixture lays down the SHARED cross-language digest fixture: two files plus a nested one, so
// the walk's per-directory sort and its separators are all exercised. Its digest is pinned in this
// suite and in backend/src/sources.test.ts; the two MUST agree or a served worker lands on a
// queue the orchestrator reports differently.
func writeFixture(t *testing.T, dir string) {
	t.Helper()
	files := map[string]string{
		filepath.Join("sub", "inner.py"): "x = 1\n",
		"workflow.json":                  `{"name":"canary","version":"0.2.0","workflow":"Canary"}`,
		"workflow.py":                    "# caller\n",
	}
	for rel, body := range files {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestFolderDigestMatchesTheCrossLanguageFixture pins the digest itself — the whole queue derivation
// rests on Go and TS producing the SAME sha256. `__pycache__` is written into the folder and must
// NOT move the digest, because serving a workflow writes it and a queue that moved on serve would
// hand every run a fresh, unpolled queue.
func TestFolderDigestMatchesTheCrossLanguageFixture(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir)
	const want = "sha256:0f89cbf60a37772e8eba8995e5d37b432b0ea7724d55fdbbf0acbb8eb3aae424"
	if got := folderDigest(dir); got != want {
		t.Fatalf("folderDigest = %q, want %q", got, want)
	}
	// __pycache__ appears merely from importing/serving; it must be invisible to the digest.
	if err := os.MkdirAll(filepath.Join(dir, "__pycache__"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "__pycache__", "workflow.pyc"), []byte("bytecode"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := folderDigest(dir); got != want {
		t.Fatalf("__pycache__ moved the digest to %q — serving would then re-queue every run", got)
	}
}

// TestEditingTheFolderChangesTheDerivedQueue is the live-monitor guarantee, pinned. A long-running
// worker stays on the queue it started on; editing the folder moves the digest, so a fresh serve
// gets a NEW queue and cannot steal the live run — and `start` on the edited folder derives that new
// queue, finds no poller, and refuses rather than dispatching onto the live monitor's queue.
// EDITING THE FOLDER MUST NOT MOVE THE QUEUE, which is the exact inverse of what this test used to
// assert. The digest queue was chosen so an edited re-serve landed somewhere new and could not steal
// a live run; the cost, measured 2026-08-29, was three queues from three edits in one session and a
// `start` that failed against every remembered one. It cost a live run too: killing the worker to
// re-serve after a build timed out that run's workflow task, and it recovered ONLY because the
// re-served worker came back to the same queue. Under the old rule the edit in between would have
// stranded it with no way to reattach.
func TestEditingTheFolderDoesNotMoveTheQueue(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir)
	file := filepath.Join(dir, "workflow.py")

	before, err := queueForWorkflow(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("# caller, edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	after, err := queueForWorkflow(file)
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("editing workflow.py moved the queue %q -> %q — a re-served worker would poll somewhere the live run cannot reach", before, after)
	}

	// BUMPING THE VERSION IS THE WAY TO MOVE IT, and that is deliberate: it is an act, not a
	// side effect of saving a file.
	if err := os.WriteFile(filepath.Join(dir, "workflow.json"),
		[]byte(`{"name":"canary","version":"0.3.0","workflow":"Canary"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	bumped, err := queueForWorkflow(file)
	if err != nil {
		t.Fatal(err)
	}
	if bumped == after {
		t.Fatalf("bumping the manifest version left the queue at %q", bumped)
	}
}

func TestWorkflowSessionIsTheContract(t *testing.T) {
	// THE WORKFLOW'S NAME, from the file it is served from — what an operator would type at a
	// `tmux attach`. It was `kontra-wf-<queue>`, which named a routing decision rather than the
	// thing being served, so `nscheck.py` on the `recon` queue ran in `kontra-wf-recon`.
	cases := map[string]string{
		"examples/python/workflows/dnssweep.py": "dnssweep",
		"enumerate_scope.py":                    "enumerate_scope",
		"/abs/path/to/sweep.py":                 "sweep",
		// A file with no extension is still a file.
		"recon": "recon",

		// THE FOLDER, and this is the trap the folder layout sets. A workflow is a folder holding
		// `workflow.py`, so the stem — all this used to take — is the same for every workflow there
		// is: every one of them would serve into a session called `workflow`, the second serve would
		// be refused as "already exists" against the first one's worker, and the Monitor discovers
		// panes by this name.
		"nscheck/workflow.py":                         "nscheck",
		"/home/me/.kontra/workflows/ping/workflow.py": "ping",
		// The folder on its own is the same workflow and the same session: `kontra workflow pause
		// nscheck` has to reach what `serve nscheck` created, with or without the trailing slash a
		// shell's tab-completion adds.
		"nscheck":  "nscheck",
		"nscheck/": "nscheck",
		// Nothing to be named after: a bare `workflow.py` has `.` for a parent and `/workflow.py`
		// has `/`, and a session called `.` or `` cannot be attached to.
		"workflow.py":  "workflow",
		"/workflow.py": "workflow",
		// Not every stem with the word in it is the marker — a flat file keeps its own name.
		"recon/my.workflow.py": "my_workflow",
	}
	for file, want := range cases {
		if got := workflowSession(file); got != want {
			t.Errorf("workflowSession(%q) = %q, want %q", file, got, want)
		}
	}
	if workflowSession("nscheck/workflow.py") == workflowSession("ping/workflow.py") {
		t.Error("two workflow folders must not share a session — the second serve would be refused")
	}

	// A workflow and an actor CAN now share a session name, and that is fine: they are told apart
	// by `@kontra`, which carries the kind. The old `wf-` prefix existed only because a name was
	// the only signal there was.
	if kontraWorkflowTag("nscheck") == kontraSessionTag("nscheck", "0.1.0") {
		t.Error("a workflow tag is indistinguishable from an actor tag")
	}
}

// TestWorkflowFileOfResolvesAFolder covers what an operator may hand `serve` and `resume` now that
// a workflow is a folder. A directory reached `fileExists`, which is false for one, so serving a
// registered folder failed with "no such file" about a path that is right there.
func TestWorkflowFileOfResolvesAFolder(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "nscheck")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "workflow.py")
	if err := os.WriteFile(marker, []byte("# caller\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	flat := filepath.Join(root, "ping.py")
	if err := os.WriteFile(flat, []byte("# caller\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Both spellings of the same workflow reach the same file — and therefore the same session.
	for _, target := range []string{dir, marker} {
		got, err := workflowFileOf(target)
		if err != nil {
			t.Fatalf("workflowFileOf(%q): %v", target, err)
		}
		if got != marker {
			t.Errorf("workflowFileOf(%q) = %q, want %q", target, got, marker)
		}
	}

	// A FLAT FILE STILL SERVES. The folder layout is added beside the old one, not in place of it.
	if got, err := workflowFileOf(flat); err != nil || got != flat {
		t.Errorf("workflowFileOf(%q) = (%q,%v), want the file itself", flat, got, err)
	}

	// THE OLD FLAT NAME OF A MOVED WORKFLOW. `serve nscheck.py` is in muscle memory, in the wiki
	// and in this repo's own docs, and `.kontra/workflows/nscheck.py` is now `nscheck/workflow.py`
	// — so without the fallback the move turns every remembered command into "no such file" about
	// a workflow that is right there. It resolves to the FOLDER, so the session name and every
	// other thing derived from the file agree with `serve nscheck`.
	legacy := filepath.Join(root, "nscheck.py")
	if got, err := workflowFileOf(legacy); err != nil || got != marker {
		t.Errorf("workflowFileOf(%q) = (%q,%v), want the folder's %q", legacy, got, err, marker)
	}
	for _, spelling := range []string{legacy, dir, marker} {
		file, err := workflowFileOf(spelling)
		if err != nil {
			t.Fatalf("workflowFileOf(%q): %v", spelling, err)
		}
		if got := workflowSession(file); got != "nscheck" {
			t.Errorf("serve %q lands in session %q, want nscheck — one workflow, one worker", spelling, got)
		}
	}

	// A REAL FLAT FILE WINS. `ping.py` exists beside no folder here; if a `ping/` folder were
	// added the named file is still the one that exists, and the fallback must not shadow it.
	pingDir := filepath.Join(root, "ping")
	if err := os.MkdirAll(pingDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pingDir, "workflow.py"), []byte("# folder\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := workflowFileOf(flat); err != nil || got != flat {
		t.Errorf("workflowFileOf(%q) = (%q,%v), want the flat file that exists", flat, got, err)
	}

	// A directory that is not a workflow must say what would make it one. "no such file" about a
	// folder that plainly exists sends an operator looking for a typo they did not make.
	empty := filepath.Join(root, "notyet")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := workflowFileOf(empty)
	if err == nil || !strings.Contains(err.Error(), "workflow.py") {
		t.Errorf("a folder with no marker must be refused by name, got %v", err)
	}

	if _, err := workflowFileOf(filepath.Join(root, "nope.py")); err == nil {
		t.Error("a missing file must still be refused")
	}
}
