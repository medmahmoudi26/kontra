#!/bin/sh
# canary — serve the canary actor and workflow, then start a run you can watch.
#
# ── WHY THIS SCRIPT EXISTS ──────────────────────────────────────────────────────────────────────
#
# The canary needs THREE processes and six environment variables, and getting any of them wrong
# fails silently in a way that looks like the console is broken:
#
#   * no workflow server  -> the run is created, Temporal queues the task, and the run sits at
#                            RUNNING publishing nothing. The console shows an open stream with no
#                            records, which is indistinguishable from a backend fault. This is the
#                            failure it actually shipped with.
#   * no actor server     -> the workflow dispatches and blocks on a Nexus operation nobody serves.
#                            There are TWO actors — one Python, one Go — because the two hosts
#                            publish through entirely different code and a green run of one is no
#                            evidence about the other. `--language python` or `go` runs one leg.
#   * no KONTRA_S3_*      -> the claim-check codec runs in PASSTHROUGH. The workflow server SAYS SO
#                            at startup, in a line easy to scroll past, and then the first batch's
#                            result cannot be decoded and the run hangs with no error anywhere.
#
# A canary whose own setup has three silent failure modes is not a canary. This is the one command.
#
# ── WHY THE ENDPOINTS ARE 127.0.0.1 AND NOT THE COMPOSE NAMES ───────────────────────────────────
#
# These processes run on the HOST, not in the compose network, so `seaweed:8333` and `redis:6379`
# do not resolve. Both are published to loopback by docker-compose, which is what makes a
# host-served actor possible at all.
set -eu

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo=$(CDPATH= cd -- "$here/../.." && pwd)
cd "$repo"

export KONTRA_S3_ENDPOINT="${KONTRA_S3_ENDPOINT:-http://127.0.0.1:8333}"
export KONTRA_S3_BUCKET="${KONTRA_S3_BUCKET:-kontra}"
export KONTRA_S3_ACCESS_KEY="${KONTRA_S3_ACCESS_KEY:-kontra}"
export KONTRA_S3_SECRET_KEY="${KONTRA_S3_SECRET_KEY:-kontra}"
export KONTRA_S3_REGION="${KONTRA_S3_REGION:-us-east-1}"
export KONTRA_REDIS_HOST="${KONTRA_REDIS_HOST:-127.0.0.1:6379}"
export KONTRA_NODE="${KONTRA_NODE:-canary-local}"

log="${TMPDIR:-/tmp}/kontra-canary"
mkdir -p "$log"

# ONE OF EACH, ALWAYS. Two workers polling one task queue steal each other's batches and supersede
# each other's sessions, which presents as a run that completes in seventeen seconds having done
# nothing. Leftovers from a previous invocation are the usual source, so they go first.
for pid in $(ps -eo pid,args 2>/dev/null | awk '/actors\/canary\/actor\.py|actors\/gocanary\/gocanary|workflows\/canary\/workflow\.py/ && !/awk/ {print $1}'); do
  kill "$pid" 2>/dev/null || true
done
sleep 2

# BUILD THE GO ACTOR, because `kontra serve` does not.
#
# For a Go actor it execs `<actor-dir>/<name>` — a COMPILED BINARY, not a source tree — and the
# failure when it is missing or stale is `no such file or directory` at exec time, or worse, a
# silently OLD binary that serves the previous version of the code. The Python leg has no
# equivalent step and that asymmetry is the whole reason this is easy to forget.
#
# GOWORK=off because the actor is its own module and the repo root's go.work does not list it:
# with the workspace active the build fails with "directory prefix . does not contain modules
# listed in go.work", which reads as a broken checkout rather than as a missing flag.
echo "[canary] building gocanary…"
( cd workspaces/scraping/actors/gocanary && GOWORK=off go build -o gocanary . ) || {
  echo "[canary] gocanary failed to build; the Go leg cannot run" >&2
  exit 1
}

echo "[canary] serving actor  (python) -> $log/actor.log"
setsid nohup kontra serve --actor workspaces/scraping/actors/canary > "$log/actor.log" 2>&1 < /dev/null &
echo "[canary] serving actor  (go)     -> $log/goactor.log"
# ITS OWN METRICS PORT. Both hosts default to 9110, so whichever loses the race logs
# `metrics listener on 0.0.0.0:9110 stopped: Address already in use` and serves no metrics. It is
# not fatal — the actor still runs — but it is an alarming line to leave in a canary's log, and
# the whole point of this script is that its output can be read at face value.
setsid env KONTRA_METRICS_ADDR=0.0.0.0:9111 nohup kontra serve --actor workspaces/scraping/actors/gocanary > "$log/goactor.log" 2>&1 < /dev/null &
echo "[canary] serving flow            -> $log/workflow.log"
setsid nohup kontra workflow serve workspaces/scraping/workflows/canary > "$log/workflow.log" 2>&1 < /dev/null &

# WAIT FOR ALL THREE TO REGISTER before starting anything. Starting a run against a queue nobody
# polls yet is the exact failure this script exists to prevent, and a fixed sleep would reintroduce
# it on a slow box. The Go leg is waited on for the same reason as the Python one: the workflow
# dispatches to both by default, and a leg whose worker is not up yet blocks on a Nexus operation
# nobody serves — which looks identical to a hung run.
i=0
while [ "$i" -lt 60 ]; do
  # THE TWO HOSTS ANNOUNCE THEMSELVES DIFFERENTLY, and matching the wrong string is a readiness
  # check that never passes against a worker that is up. Python logs
  #   [host] canary@0.1.1 serving on canary-0.1.1-sessions
  # and Go logs
  #   [host] gocanary@0.1.0 serving OpenSession/RunBatch/Close on gocanary-0.1.0-sessions
  # — so "serving on" does not appear in the Go log at all, and this script exited saying a server
  # had not come up while all three were running happily.
  grep -q "serving on" "$log/actor.log" 2>/dev/null &&
    grep -q "serving OpenSession" "$log/goactor.log" 2>/dev/null &&
    grep -q "wf-canary" "$log/workflow.log" 2>/dev/null && break
  i=$((i + 1))
  sleep 1
done
if [ "$i" -ge 60 ]; then
  echo "[canary] a server did not come up; see $log/actor.log, $log/goactor.log and $log/workflow.log" >&2
  exit 1
fi

if grep -q "PASSTHROUGH" "$log/workflow.log" 2>/dev/null; then
  echo "[canary] REFUSING: the workflow server has no object store, so the claim-check codec is" >&2
  echo "         PASSTHROUGH and the first batch's result will not decode. Check KONTRA_S3_*." >&2
  exit 1
fi

echo "[canary] both serving. starting a run…"
kontra workflow start workspaces/scraping/workflows/canary "$@"
echo
echo "[canary] watch it: console -> workspace 'scraping' -> Workflows -> canary -> the RUNNING run"
echo "[canary] expect THREE topics: progress (the workflow), canary/tick (python), gocanary/tick (go)"
echo "[canary] stop the servers:  pkill -f 'canary/(actor|workflow)\\.py|gocanary/gocanary'"
