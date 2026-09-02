// identity.go — WHICH QUEUE DOES THIS CLI TALK TO, answered in one file.
//
// Two kinds of thing get served here and they are addressed by two different rules. An ACTOR's
// queue is `{name}-{version}` off a manifest; a WORKFLOW's is `wf-{name}-{version}` off its own —
// folder's CONTENT. Until this file existed those two lived in `api.go` and `workflow.go`, and
// neither referenced the other — so the one question an operator asks about a stuck run ("what is
// it polling, and why is nothing there") meant reading two files and holding the difference in
// your head. The difference is the interesting part and it is now visible in one place:
//
//	sharedQueue("nscheck", "0.1.0")  ->  nscheck-0.1.0        NAMED   — a free string on a manifest
//	queueForWorkflow("nscheck/…")    ->  wf-nscheck-0f89cbf6  DERIVED — a hash of the bytes
//
// AND THE `wf-` PREFIX IS WHY THEY HAVE TO BE READ TOGETHER. This repo ships an Actor and a
// Workflow both called `nscheck`. The Actor's handler polls its shared queue for WORKFLOW tasks
// too (`runtime/handler/main.go` registers RunWorkflow on it) while knowing only its own type, so a
// `NsCheck` task landing there fails and retries forever. The prefix is the only thing keeping
// those two derivations apart, and a prefix that guards a collision with a function in another
// file is a guard nobody can see.
//
// Everything below the queues is what the queues are derived FROM: the marker that makes a folder
// a workflow, the resolution from what an operator typed to the file python is handed, the
// manifest, the content digests, and the tmux session name. One resolution feeds all of them,
// which is what makes `serve nscheck`, `serve nscheck/`, `serve nscheck.py` and
// `serve nscheck/workflow.py` one worker on one queue in one session instead of four.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// --- the actor's queue -------------------------------------------------------------------------

// sharedQueue is the (name, version) → task-queue contract: "{name}-{version}", or
// "{name}-shared" when version is empty.
//
// UNTIL 2026-08-28 THIS DERIVATION HAD NO CONGRUENCE TEST AT ALL. It appeared in the suite once,
// in workflow_test.go, as the CONTRAST half of an assertion about something else — so the eight
// places that must agree on this string were seven places and a comment. queues_conformance_test.go
// is the CLI's arm of shared/conformance/queues.json now.
func sharedQueue(name, version string) string {
	if version != "" {
		return name + "-" + version
	}
	return name + "-shared"
}

// --- the workflow's queue ----------------------------------------------------------------------

// workflowQueue is the task queue a workflow is served on: DERIVED, UNTYPABLE, and bound to the
// folder's CONTENT, never a name an operator (or an agent) can pass.
//
// A QUEUE A STRING CAN NAME GOES STALE SILENTLY, and the failure has no error in it. `--queue recon`
// was muscle memory from an older name, so a run started, routed to a queue nobody was serving, and
// sat `running` forever — indistinguishable from a worker that had not booted. Temporal counted it
// and nothing here read the count: `no_poller_tasks{taskqueue="recon"} = 12` was on the live cluster
// when this was written. Worse, `--queue dhmonitor` typed onto a LIVE monitor's queue makes a second
// worker a rival that can take its `continue_as_new` and run different code. So the flag is gone: no
// `--queue` on serve or start, and the queue cannot be spelled by anyone.
//
// IT USED TO BE `<digest12>`, A HASH OF THE FOLDER'S BYTES. That argument was real and is kept here
// because this change gives it up deliberately rather than by forgetting it: `(name, version)` is a
// free string on a folder (ADR 0004 / 0011), so editing `workflow.py` without bumping
// `workflow.json` keeps the same queue, and two checkouts of one manifest share it. A content digest
// made the queue BE the code — a re-served edit landed on a NEW queue and could not steal a live run.
//
// WHAT IT COST, MEASURED 2026-08-29. The queue became unreadable, untypable and unstable: three
// edits to one `workflow.py` in a single session produced `wf-nscheck-1b6777a07395`, then
// `-839897c865fe`, then `-12cd1fbaaf48`, and every `start` against a remembered one failed with
// "no worker is serving this folder's code" — accurate, and useless to act on. It also broke the
// product's own vocabulary: an Actor is `nscheck-0.1.0` in `workers list`, in the Monitor and in
// `task-queue describe`, and a Workflow being a hash in that same column made two identities out of
// one idea.
//
// The digest is not lost, it is put where it belongs: it still identifies WHICH BYTES a folder holds
// and is reported beside the queue. Routing identity is the manifest. Conflating the two made
// routing unspeakable in order to answer a question routing was not asking.
//
// `wf-` SURVIVES AND IS NOT NEGOTIABLE — see this file's header. This repo ships an Actor and a
// Workflow both called `nscheck` at 0.1.0; without the prefix both are `nscheck-0.1.0` and Temporal
// hands workflow tasks to the actor's handler, which fails every one of them forever.
func workflowQueue(name, version string) string {
	if name == "" || version == "" {
		return ""
	}
	return "wf-" + name + "-" + version
}

// queueForWorkflow answers the queue to serve `file` on: DERIVED from the folder, with no override.
//
// THERE IS NO OVERRIDE ANY MORE. `--queue` was the hole this closes — a string an operator or an
// agent could type that Temporal accepts for any workflow, going stale onto a queue nobody polls or
// LIVE onto one two workers then fight over. The queue is `wf-<name>-<digest>` and nothing else can
// name it.
//
// EMPTY IS A REFUSAL, not a fallback to the SDK's "default". A workflow served on `default` shares a
// queue with every other workflow anyone forgot to name, which is the same routing accident by a
// different route. A folder with no manifest is one `kontra workflow register --init` away.
func queueForWorkflow(file string) (string, error) {
	dir := filepath.Dir(file)
	if filepath.Base(file) == workflowMarker {
		// FOLDER: the manifest names AND versions it. Both are required — defaulting a missing
		// version would put the workflow on a queue the operator did not choose, which is the
		// fault this shape exists to remove.
		m := readWorkflowManifest(file)
		if q := workflowQueue(m.Name, m.Version); q != "" {
			return q, nil
		}
		return "", fmt.Errorf(
			"%s/%s needs both \"name\" and \"version\" to derive a queue — run `kontra workflow register %s --init`",
			dir, manifestFile("workflow"), dir)
	}
	// FLAT FILE: no manifest, so its stem names it and it carries the stated flat-file version.
	// Not its own bytes: a queue that moves on every edit is the thing being removed, and a flat
	// file is the shape a folder replaces — it does not get a second identity scheme.
	name := strings.TrimSuffix(filepath.Base(file), filepath.Ext(file))
	if q := workflowQueue(name, flatFileVersion); q != "" {
		return q, nil
	}
	return "", fmt.Errorf("%s could not be read to derive a queue from", file)
}

// --- what a workflow IS ------------------------------------------------------------------------

// workflowMarker is the file that makes a FOLDER a workflow, and the peer of
// `control/orchestrator/src/sources.ts:MARKER.workflow` — written independently on this side like every
// other cross-language literal in this repo. A workflow is a folder holding this file and its
// description.md, the same shape an actor already has with actor.json + actor.py.
const workflowMarker = "workflow.py"

// flatFileVersion is what a manifest-less `.py` workflow carries where a folder carries its
// manifest version — the peer of `control/orchestrator/src/workflowControl.ts:FLAT_FILE_VERSION`.
const flatFileVersion = "0.0.0"

// workflowFileOf resolves what an operator typed to the FILE python will be handed.
//
// A FOLDER IS A WORKFLOW NOW, so `serve nscheck` and `serve nscheck/workflow.py` name the same
// thing. Without this a folder reached `fileExists`, which is false for a directory, and the
// command answered "no such file" about a path that is right there — naming the folder rather than
// the file it was missing.
//
// AND SO DOES THE OLD FLAT NAME OF ONE. `serve nscheck.py` is in muscle memory, in the wiki and in
// this repo's own docs, and `nscheck.py` is now `nscheck/workflow.py` — so without the fallback
// below the move turned every remembered command into "no such file" about a workflow that is
// right there. It resolves to the FOLDER rather than aliasing the old path, which is what makes
// `serve nscheck.py` and `serve nscheck` one session and not two: everything downstream, the
// session name included, is derived from the file this returns.
func workflowFileOf(target string) (string, error) {
	abs, err := filepath.Abs(target)
	if err != nil {
		return "", err
	}
	if fi, err := os.Stat(abs); err == nil && fi.IsDir() {
		marker := filepath.Join(abs, workflowMarker)
		if !fileExists(marker) {
			return "", fmt.Errorf("%s has no %s — that is what makes a folder a workflow", abs, workflowMarker)
		}
		return marker, nil
	}
	if !fileExists(abs) {
		// Only when the flat file is NOT there: a real `nscheck.py` sitting beside a `nscheck/`
		// folder is two workflows, and the one that was named is the one that exists.
		if folder := strings.TrimSuffix(abs, ".py"); folder != abs {
			if fi, err := os.Stat(folder); err == nil && fi.IsDir() && fileExists(filepath.Join(folder, workflowMarker)) {
				return filepath.Join(folder, workflowMarker), nil
			}
		}
		return "", fmt.Errorf("%s: no such file", abs)
	}
	return abs, nil
}

// workflowManifest is `workflow.json` — the peer of actorManifest, and of MANIFEST in
// control/orchestrator/src/sources.ts.
type workflowManifest struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	// The @workflow.defn class. `kontra workflow start` takes this exact string.
	Workflow string `json:"workflow"`
	Entry    string `json:"entry"`
}

// readWorkflowManifest reads the `workflow.json` beside a served `workflow.py`. Missing or
// unparseable is not an error here: a bare `.py` file served directly has no folder to hold one,
// and that case still has to work.
func readWorkflowManifest(file string) workflowManifest {
	var m workflowManifest
	body, err := os.ReadFile(filepath.Join(filepath.Dir(file), manifestFile("workflow")))
	if err != nil {
		return m
	}
	_ = json.Unmarshal(body, &m)
	return m
}

// workflowSession is the tmux session a served workflow worker runs in: THE WORKFLOW'S NAME, taken
// from the file it is served from — `enumerate_scope.py` runs in `enumerate_scope`.
//
// IT IS HERE, BESIDE THE QUEUE, AND NOT IN tmux.go, because it is derived from the same resolved
// file the queue is and shares `workflowMarker` with it. `tmuxSession` is an ACTOR's session and
// lives with the tmux mechanics; the two are held together by shared/conformance/queues.json §tmux_session
// rather than by being adjacent.
//
// IT USED TO BE `kontra-wf-<queue>`, and both halves of that were compensating for the same thing.
// The `kontra-` prefix was how `panels/local.ts` found local Terminals at all, so a session named
// anything an operator would recognise was invisible to the Monitor; and `wf-` existed to keep a
// workflow served on a queue named after an actor from colliding with that actor's own session.
// Discovery is the `@kontra` tmux option now, and it carries the KIND — so neither prefix has
// anything left to do, and the collision cannot happen because the two kinds are distinguishable
// without their names being.
//
// THE FILE, NOT THE QUEUE. A queue is a routing decision an operator makes per session; the file is
// what is being served. Naming the session after the queue meant `tmux attach -t kontra-wf-recon`
// for a file called `nscheck.py`, and two files served on one queue still collided — which the old
// comment claimed as the point, but the collision it detects is "two workers on one queue", and
// that is a property of the QUEUE, not something a session name should be spent on.
//
// AND THE FOLDER, when the file is that folder's `workflow.py`. Every workflow folder holds a file
// of that one name, so the stem alone named all of them `workflow`: `nscheck/workflow.py` and
// `ping/workflow.py` served into ONE session, the second serve was refused as "already exists"
// against the first one's worker, and the Monitor — which finds a pane by this name — showed one
// pane for two workflows.
func workflowSession(file string) string {
	base := filepath.Base(file)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	if base == strings.TrimSuffix(workflowMarker, filepath.Ext(workflowMarker)) {
		// The parent of a bare `workflow.py` is `.` and of `/workflow.py` is `/`; neither is a
		// workflow's name, so the stem stands and the fallback below names it.
		if parent := filepath.Base(filepath.Dir(file)); parent != "." && parent != ".." && parent != string(filepath.Separator) {
			base = parent
		}
	}
	// tmux rewrites `.` and `:` to `_` at creation, and a name can carry either — `my.workflow.py`.
	base = tmuxSafeName(base)
	if base == "" || base == "_" {
		return "workflow"
	}
	return base
}

// --- the content digests the workflow queue is built on ------------------------------------------

// notCodeDigest is the byte-identical peer of control/orchestrator/src/sources.ts:NOT_CODE — directories
// that are not the folder's code and must not move its digest. `__pycache__` is the one that forces
// it to exist: Python writes it into whatever directory it imports from, so merely SERVING a
// workflow would otherwise change the folder's digest and hand every run a new queue.
var notCodeDigest = map[string]bool{
	"__pycache__": true, ".git": true, "node_modules": true, ".venv": true,
	".mypy_cache": true, ".pytest_cache": true, ".DS_Store": true,
}

// folderDigest is `sha256:<hex>` over a folder's own files — the byte-identical peer of
// control/orchestrator/src/sources.ts:folderDigest, written independently on this side like every other
// cross-language literal in this repo. It MUST agree with the TS one: the orchestrator stores this
// at registration and the CLI derives the served queue from it, and a drift would put the worker on
// a queue the page reports differently — a run dispatched to a queue nobody serves.
//
// The path is in the hash (relative), the walk is a per-directory-sorted pre-order DFS, and size and
// mtime are excluded — see the TS original for why each of those is load-bearing. conformance is
// pinned by a shared fixture in both test suites.
func folderDigest(dir string) string {
	h := sha256.New()
	for _, rel := range folderDigestWalk(dir, "") {
		h.Write([]byte(rel))
		h.Write([]byte{0})
		if data, err := os.ReadFile(filepath.Join(dir, rel)); err != nil {
			// An unreadable file is recorded as present-but-unreadable, not skipped: skipping would
			// hash an unreadable file and a missing one the same.
			h.Write([]byte("\x00unreadable"))
		} else {
			h.Write(data)
		}
		h.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// folderDigestWalk yields every file under dir as slash-separated relative paths, per-directory
// sorted, DFS, skipping notCodeDigest. os.ReadDir already returns entries sorted by name (byte
// order), which matches the TS `a.name < b.name` for the ASCII names a workflow folder holds.
// Symlinks are neither IsDir nor IsRegular and are skipped on both sides.
func folderDigestWalk(dir, prefix string) []string {
	entries, err := os.ReadDir(filepath.Join(dir, prefix))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if notCodeDigest[e.Name()] {
			continue
		}
		rel := e.Name()
		if prefix != "" {
			rel = prefix + "/" + e.Name()
		}
		switch {
		case e.IsDir():
			out = append(out, folderDigestWalk(dir, rel)...)
		case e.Type().IsRegular():
			out = append(out, rel)
		}
	}
	return out
}

// workflowDigest12 is the short digest that goes in a queue name: the first 12 hex chars of the
// folder's sha256. Twelve is 48 bits — enough that two distinct folders colliding is not a hazard
// worth a longer, uglier queue — and QUEUE_RE-safe.
func workflowDigest12(dir string) string {
	return strings.TrimPrefix(folderDigest(dir), "sha256:")[:12]
}

// fileDigest12 is the same short digest for a FLAT `.py` workflow — one with no folder and no
// manifest, still supported by workflowFileOf. Its identity is its own bytes (raw sha256, the
// simplest possible cross-language contract, matching the TS peer), so a flat file is content-bound
// exactly like a folder: edit it and its queue moves. Unreadable is "" — a refusal, not a guess.
func fileDigest12(file string) string {
	data, err := os.ReadFile(file)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:12]
}
