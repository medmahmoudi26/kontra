package temporalhost

// Publishing the author's progress onto the RUN's Temporal Workflow Stream.
//
// ── WHY THIS EXISTS BESIDE THE HEARTBEAT AND DOES NOT REPLACE IT ────────────────────────────────
//
// `RecordHeartbeat` in host.go carries the same map and is what keeps `temporal workflow describe`
// and the CLI's `--state` view honest. It is also the liveness signal, so it must keep firing
// whether or not anything is subscribed. What it cannot do is let a BROWSER follow a run: reading
// it means calling DescribeWorkflowExecution and polling, which samples the last beat and misses
// every one in between, and `heartbeat.ts` historically dropped the author's map entirely.
//
// A workflow stream is a log with offsets. A subscriber resumes from where it stopped, a late one
// replays from zero, and neither needs the orchestrator to poll anything.
//
// ── WHY IT ADDRESSES THE RUN AND NOT THIS ACTIVITY'S OWN WORKFLOW ───────────────────────────────
//
// `workflowstreams.NewClientFromActivity` would target `actor-<actor>-<run>-<node>` — the per-node
// workflow this activity happens to be running under. A console subscribes by RUN id, which is the
// address a person has, so the publisher names `req.RunID` explicitly.
//
// ── EVERY FAILURE IS SWALLOWED, DELIBERATELY ────────────────────────────────────────────────────
//
// Same rule as the heartbeat and as `note`: an observability call must never be the thing that
// fails a Batch that already committed. An actor driven outside a hosted Run has no RunID, and a
// workflow that does not host a stream has no handler for the publish signal — both are ordinary,
// not errors, and both land as a nil publisher that every function here tolerates.

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"go.temporal.io/sdk/contrib/workflowstreams"
)

// progressTopic is the one name every kontra run publishes progress on, so a subscriber that has
// never seen a particular run still knows what to ask for. Peer of `kontra.say.PROGRESS_TOPIC`.
const progressTopic = "progress"

// streamFor returns a publisher onto runID's stream, or nil when there is nothing to publish to.
func (h *Activities) streamFor(runID string) *workflowstreams.Client {
	if h == nil || h.tc == nil || runID == "" {
		return nil
	}
	// BatchInterval is left at the library default (2s), which is already the engine's progress
	// tick — so a batch of publishes costs one Signal rather than one per beat. Measured: 40
	// events over 20s cost 1 signal and 17 history events in total.
	return workflowstreams.NewClient(h.tc, runID, workflowstreams.Options{})
}

// publishProgress puts one beat on the topic. `node` and the actor ID travel with it because a
// run is many workers: a pane showing one merged `at` for a six-node fleet would flicker between
// hosts and describe none of them.
//
// `actor` IS THE SESSION'S ID, NOT THE ACTOR'S NAME — the peer of Python's `self._actor_id`. See
// the SetStream call site in host.go for why the name belongs in the topic and nowhere else.
//
// The author's map is SPREAD at the top level rather than nested under `progress`, unlike the
// heartbeat payload — there the nesting exists so an author returning `{"done": …}` cannot
// overwrite the liveness field the orchestrator reads. Here there is no such field to protect, and
// a subscriber wants `program` and `at` as first-class keys rather than one level down.
func publishProgress(pub *workflowstreams.Client, node, actor string, v any, last map[string]any) {
	if pub == nil {
		return
	}
	beat := map[string]any{"node": node, "actor": actor}
	for k, val := range last {
		beat[k] = val
	}
	if m, ok := v.(map[string]any); ok {
		for k, val := range m {
			beat[k] = val
		}
	} else if v != nil {
		beat["progress"] = v
	}
	// Buffered by the client and flushed on its interval; `false` is "do not force a flush", which
	// is what keeps a 2-second beat from costing a Signal apiece.
	pub.Topic(progressTopic).Publish(beat, false)
}

// closeStream flushes what is still buffered and releases the publisher.
//
// THE FLUSH IS THE POINT. Events buffered but unshipped when a process ends are lost — the library
// says so — and a Batch ending is exactly when the last and most interesting beat is still sitting
// inside the two-second window. The deadline is its own context because the activity's may already
// be cancelled, which is the case where this matters most.
func closeStream(ctx context.Context, pub *workflowstreams.Client) {
	if pub == nil {
		return
	}
	flush, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = pub.Flush(flush)
	_ = pub.Close(flush)
}

// methodTopic is `<actor>/<method>` — e.g. `webcrawl/crawl`.
//
// ONE TOPIC PER METHOD, so a console groups a run's streams without being told what any of the
// actors are, and so two actors in one run cannot write over each other. Which WORKER is speaking
// rides in the record (`node`), not in the topic: six crawlers doing one job are one stream with
// six voices, not six streams.
//
// Peer of `_method_topic` in runtime/python/internals/engine.py. The fallbacks matter: an actor
// with no resolved Method would otherwise publish to `<actor>/`, which reads as a typo rather than
// as the sole-Method case it actually is.
func methodTopic(actorName, methodName string) string {
	if actorName == "" {
		actorName = "actor"
	}
	if methodName == "" {
		methodName = "run"
	}
	return actorName + "/" + methodName
}

// publishRecord puts ONE author record on a Method's topic.
//
// A SIBLING OF publishProgress, NOT A PARAMETERISATION OF IT. That one merges the liveness counts
// and nests a non-map under `progress`; both would corrupt a typed record against the schema its
// Method declared with `Streams(...)`. This one does only what Python's publisher does: flatten,
// add `node` and `actor`, publish.
func publishRecord(pub *workflowstreams.Client, topic, node, actor string, v any) {
	if pub == nil {
		return
	}
	pub.Topic(topic).Publish(streamBody(node, actor, v), false)
}

// streamBody is the record as it goes on the wire: the author's fields, plus the engine's routing
// keys where the author has not claimed them.
//
// SPLIT OUT OF publishRecord SO IT CAN BE TESTED. The rest of that function needs a live
// workflowstreams.Client, and a nil one is a no-op — so a test against publishRecord can only ever
// assert that nothing happened. This half is the part that crosses a language boundary and is
// therefore the part worth pinning.
//
// THE AUTHOR WINS A COLLISION. `setdefault`, not assignment, matching Python's publisher: a record
// that genuinely carries its own `node` — a scheduler reporting which worker it PLACED something
// on — means that field, and having the engine silently overwrite it with the worker that happens
// to be publishing would be a lie the author cannot see or prevent.
func streamBody(node, actor string, v any) map[string]any {
	body := asMapping(v)
	if _, ok := body["node"]; !ok {
		body["node"] = node
	}
	if _, ok := body["actor"]; !ok {
		body["actor"] = actor
	}
	return body
}

// asMapping flattens an author's record to a map — the peer of Python's `_as_mapping`.
//
// VIA json.Marshal, DELIBERATELY. The `Streams(...)` schema is reflected from the same struct
// tags, so a round trip through JSON cannot disagree with the shape a console was told to expect;
// reflecting the struct by hand here would be a second implementation of the same mapping, free to
// drift from the first.
//
// A VALUE THAT IS NOT AN OBJECT IS NESTED, NOT DROPPED. `Stream(s, "fetched "+url)` is a natural
// thing to try given the verb's name, and returning an empty map would ship a record carrying only
// the framework's own `node`/`actor` — the author's value gone with nothing said. Python's peer
// makes the same choice for the same reason.
func asMapping(v any) map[string]any {
	if v == nil {
		return map[string]any{}
	}
	if m, ok := v.(map[string]any); ok {
		out := make(map[string]any, len(m))
		for k, val := range m {
			out[k] = val
		}
		return out
	}
	raw, err := json.Marshal(v)
	if err != nil {
		// SAID, NOT SWALLOWED. A record that cannot be marshalled is an author bug, and a
		// silently missing stream is how it would otherwise present.
		log.Printf("kontra.Stream: record dropped, not marshalable: %v", err)
		return map[string]any{}
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil || out == nil {
		return map[string]any{"value": v}
	}
	return out
}

