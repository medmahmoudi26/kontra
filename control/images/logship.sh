#!/bin/sh
# logship — put every worker container's stdout into VictoriaLogs, so the console's logs rail has
# something to show.
#
# ── WHY THIS EXISTS AS A SERVICE ────────────────────────────────────────────────────────────────
#
# `LogsRail.svelte` says "Lines reach here from every Machine's vlagent", and on a Fleet Machine
# they do. A worker running as a CONTAINER on the control plane has no vlagent, so the rail was
# empty for every local run — and an empty rail is indistinguishable from a Run that logged
# nothing, which is the exact confusion `routes/logs.ts::unreachable` was written to prevent.
#
# It had been solved before by patching a running container, which is why it stopped working the
# next time one was recreated. A compose service survives that; a `docker exec` does not.
#
# ── SELECTION IS BY LABEL, AND THE LABEL ALREADY EXISTED ────────────────────────────────────────
#
# This used to ship four container NAMES:
#
#     kontra-desync|kontra-webcrawl|kontra-nuclei|kontra-subfinder
#
# An enumeration in an open world. A fifth actor's logs were absent and nothing said so — the rail
# simply showed no lines for it, which reads as a Run that logged nothing, which is the one
# reading this whole file exists to prevent. It also excluded every control-plane container, so
# `kontra-api`, `kontra-infra` and `kontra-probe` — the processes that make the authorisation
# decisions — were the ones with no retained logs at all.
#
# So it selects on labels, and neither of them is new:
#
#   • `KONTRA_WORKER` is stamped on every Worker half the Warden starts (`cli/warden/driver.go`,
#     `driver_docker.go:runFlags`), and the Warden's own reconcile loop already filters on it. A
#     new actor is therefore covered the moment it exists, by construction rather than by somebody
#     remembering to add it here.
#   • `kontra.logs=true` is set in `docker-compose.yml` on the control-plane services. That one IS
#     new, and it is an opt-IN on a small fixed set of services this repository owns, not an
#     enumeration of an open set it does not.
#
# The infrastructure containers (postgres, temporal, seaweed, the registry) stay unlabelled and
# unshipped. They have their own logs and would drown the rail.
#
# ── THE LINE IS PARSED, NOT GUESSED AT ──────────────────────────────────────────────────────────
#
# The previous version recovered the run id from the message TEXT with a regular expression, and
# inferred the level by looking for the substring "ERROR". Both were reconstructing structure that
# had been thrown away one layer up — the engine holds the run id (`engine.py`, `logs.bind_run`)
# and the logger holds the level.
#
# It was doing that because NOTHING EVER SET `KONTRA_LOG_FORMAT=json`. `internals/logs.py` has
# shipped structured, identity-stamped records since ADR 0050 §1 and no deployment asked for them.
# `cli/warden/warden_ca.go::workerDefaults` now does, so a Worker emits JSON and this reads it.
#
# Three shapes arrive and all three are RECOGNISED rather than sniffed at:
#
#   kontra   `_msg` + `_time`           — internals/logs.py::JsonFormatter, passed through whole
#   pino     `msg` + numeric `level`    — the orchestrator's Fastify logger
#   text     anything else              — a line from a process that is not logging structurally
#
# A TEXT LINE GETS NO RUN ID. Not a regex-recovered one: a wrong correlation is worse than an
# absent one, because a line attributed to the wrong Run is evidence pointing at an innocent
# execution. The fix for a text line is at the EMITTER, and it is one environment variable.
#
# ── THE TIMESTAMP BELONGS TO THE EVENT, NOT TO THE TRANSPORT ────────────────────────────────────
#
# This used to stamp `datetime.now()` — the SHIPPER's clock, at the moment it happened to read the
# line. Under backpressure, or when `docker logs` replays a burst, that orders the rail by when
# this loop got around to it rather than by when anything happened. Any forensic timeline built on
# it is wrong in exactly the situation somebody would be building one. The emitter's own timestamp
# is used whenever the line carries one; the shipper's clock is the fallback and is MARKED as such
# with `ship_time=true`, so a reader can tell which kind of ordering they are looking at.
set -eu

LOGS_URL="${KONTRA_LOGS_URL:-http://victorialogs:9428}"
TENANT="${KONTRA_NAMESPACE:-default}"

# THE STREAM FIELDS ARE A CONTRACT, AND `worker` IS DELIBERATELY NOT ONE.
#
# `routes/logs.ts::scopedQuery` filters `_stream:{tenant="<namespace>"}`, so a line ingested
# without `tenant` is invisible to the console no matter what else is right about it. `actor`,
# `machine` and `unit` match what the existing rows carry, so old and new lines sit in one stream
# set rather than two shapes of the same thing.
#
# `worker` is `<pid>@<host>@<queue>` and the pid changes on every restart. As a STREAM field that
# would mint a new log stream per restart — unbounded cardinality in the index for a value nobody
# groups by. It rides as an ordinary field, which is filterable and costs nothing.
INGEST="$LOGS_URL/insert/jsonline?_stream_fields=tenant,actor,machine,unit&_msg_field=msg&_time_field=_time"

say() { echo "[logship] $*" >&2; }
say "-> $LOGS_URL tenant=$TENANT"

# THE MARKERS ARE ABOUT CHILDREN OF *THIS* PROCESS, so they must not outlive it.
#
# `docker restart` keeps the container filesystem, so `/tmp/following-<name>` written by the
# previous process is still there when this one starts — and the poll loop below reads a marker as
# "already following". After any restart the shipper therefore followed NOTHING, forever, while
# still printing its startup banner and looking healthy. MEASURED: `ps` inside the container showed
# only `/bin/sh /logship.sh` and not one `docker logs -f`, with both marker files present.
#
# That is the worst shape a logging bug can take: it is silent, it survives a restart meant to fix
# it, and an empty rail reads as "this run logged nothing".
rm -f /tmp/following-* 2>/dev/null || true

# THE PARSER IS ITS OWN FILE, AND THAT IS THE WHOLE REASON IT CAN BE TESTED.
#
# The version this replaces embedded forty lines of Python inside a shell function inside a compose
# entrypoint, where nothing could import it and a syntax error surfaced as a silent absence of
# logs. `tests/test_logline.py` runs it directly.
PARSER="${KONTRA_LOGSHIP_PARSER:-/logline.py}"
if [ ! -f "$PARSER" ]; then
  # FAIL LOUD AND STAY DOWN. A shipper that keeps running without a parser produces exactly what a
  # correctly-running one produces when nothing logged: nothing. Compose restarts this, the message
  # repeats, and `docker compose logs logship` names the missing mount.
  say "FATAL: parser not found at $PARSER — is control/images/logline.py mounted?"
  exit 1
fi

# follow ships one container's stdout until the container stops.
#
# ONE curl PER LINE, deliberately. A streaming upload (`curl -T -`) would be fewer processes, but
# it holds one connection per container and a worker that goes quiet for minutes — which is the
# normal state between batches — leaves that connection idle until something times it out and the
# tail is lost silently. Worker log volume is a few lines a second; correctness is worth the forks.
follow() {
  name="$1"
  # THE LABEL, NOT THE NAME. `kontra-([a-z0-9]+)` guessed the actor from the container name, which
  # is a convention rather than a fact and says nothing about version or half.
  worker_label="$(docker inspect -f '{{index .Config.Labels "KONTRA_WORKER"}}' "$name" 2>/dev/null || true)"
  actor="$(echo "$name" | sed -E 's/^kontra-([a-z0-9]+).*/\1/')"
  say "following $name (actor=$actor worker=${worker_label:-none})"
  # `--tail 0` so a restart of THIS service does not re-ship a container's whole history as if it
  # had just happened; the timestamps would be right but the rail would fill with a replay.
  docker logs -f --tail 0 "$name" 2>&1 \
    | python3 "$PARSER" "$TENANT" "$actor" "$name" "$name" "$worker_label" \
    | while IFS= read -r doc; do
        printf '%s' "$doc" \
          | curl -s -m 5 -X POST -H 'Content-Type: application/stream+json' \
                 --data-binary @- "$INGEST" >/dev/null || true
      done
  say "$name ended"
}

# Poll for containers rather than subscribing to docker events: a worker is started and stopped by
# `kontra deploy` and by hand, and a five-second poll that can never miss one permanently is worth
# more here than an event stream that needs its own reconnect logic.
#
# TWO LABEL FILTERS, UNIONED. `docker ps` ANDs multiple `--filter label=`, so asking for both in
# one call would match nothing — the two sets are disjoint by design.
while :; do
  for c in $( { docker ps --filter label=KONTRA_WORKER --format '{{.Names}}' 2>/dev/null || true
                docker ps --filter label=kontra.logs=true --format '{{.Names}}' 2>/dev/null || true
              } | sort -u ); do
    if [ ! -f "/tmp/following-$c" ]; then
      : > "/tmp/following-$c"
      ( follow "$c"; rm -f "/tmp/following-$c" ) &
    fi
  done
  sleep 5
done
