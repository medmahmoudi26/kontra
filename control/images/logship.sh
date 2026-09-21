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
# ── THE STREAM FIELDS ARE A CONTRACT ────────────────────────────────────────────────────────────
#
# `routes/logs.ts::scopedQuery` filters `_stream:{tenant="<namespace>"}`, so a line ingested
# without `tenant` is invisible to the console no matter what else is right about it. `actor`,
# `machine` and `unit` match what the existing rows carry, so old and new lines sit in one stream
# set rather than two shapes of the same thing.
set -eu

LOGS_URL="${KONTRA_LOGS_URL:-http://victorialogs:9428}"
TENANT="${KONTRA_NAMESPACE:-default}"
# Containers whose stdout is worth shipping. Workers are the point; the infrastructure containers
# have their own logs and would drown the rail.
MATCH="${KONTRA_LOGSHIP_MATCH:-kontra-desync|kontra-webcrawl|kontra-nuclei|kontra-subfinder}"

INGEST="$LOGS_URL/insert/jsonline?_stream_fields=tenant,actor,machine,unit&_msg_field=msg&_time_field=_time"

say() { echo "[logship] $*" >&2; }
say "-> $LOGS_URL tenant=$TENANT match=$MATCH"

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

# follow ships one container's stdout until the container stops.
#
# ONE curl PER LINE, deliberately. A streaming upload (`curl -T -`) would be fewer processes, but
# it holds one connection per container and a worker that goes quiet for minutes — which is the
# normal state between batches — leaves that connection idle until something times it out and the
# tail is lost silently. Worker log volume is a few lines a second; correctness is worth the forks.
follow() {
  name="$1"
  actor="$(echo "$name" | sed -E 's/^kontra-([a-z0-9]+).*/\1/')"
  say "following $name (actor=$actor)"
  # `--tail 0` so a restart of THIS service does not re-ship a container's whole history as if it
  # had just happened; the timestamps would be right but the rail would fill with a replay.
  docker logs -f --tail 0 "$name" 2>&1 | while IFS= read -r line; do
    [ -n "$line" ] || continue
    printf '%s' "$line" \
      | python3 -c '
import json,re,sys,datetime
msg = sys.stdin.read()
# THE RUN ID, WHEN THE LINE NAMES ONE. `run/logs.ts::fetchLogs` queries `run_id:"<id>"`, so a line
# without this field is invisible to the per-run rail no matter how right everything else is —
# which is exactly why the rail read "No logs for this Run" while the data sat in VictoriaLogs.
#
# It is EXTRACTED rather than configured because one shipper follows every worker and a worker
# serves many runs over its life. `<workflow>-<epoch>` is the id shape, and it appears in batch
# ids (`batch hunt-1789865677/desync-…`) and in Temporal WorkflowIDs
# (`actor-desync-hunt-1789742802-…`). The 9-digit floor is what keeps `webcrawl-0.2.3-sessions`
# from matching.
#
# A LINE WITH NO RUN ID STILL SHIPS, with the field absent — it belongs in the global stream even
# though the per-run rail cannot claim it. Lines like `progress: {"contexts": 4}` are in that
# class, and closing that properly means the engine stamping its own RunID, which it knows.
m = re.search(r"\b([a-z][a-z0-9_]*-\d{9,})\b", msg)
print(json.dumps({
    "_time": datetime.datetime.now(datetime.timezone.utc).isoformat().replace("+00:00","Z"),
    "tenant": sys.argv[1], "actor": sys.argv[2], "machine": sys.argv[3], "unit": sys.argv[3],
    **({"run_id": m.group(1)} if m else {}),
    # The level is what the rail filters on. Inferred rather than parsed: these lines come from
    # three different loggers (Go `log`, Python logging, the temporal SDK) and guessing wrong
    # costs a filter chip, while refusing to guess costs the whole filter.
    "level": ("error" if ("ERROR" in msg or "Traceback" in msg or "panic" in msg)
              else "warn" if ("WARN" in msg or "warning" in msg)
              else "info"),
    "msg": msg,
}))' "$TENANT" "$actor" "$name" \
      | curl -s -m 5 -X POST -H 'Content-Type: application/stream+json' --data-binary @- "$INGEST" >/dev/null || true
  done
  say "$name ended"
}

# Poll for containers rather than subscribing to docker events: a worker is started and stopped by
# `kontra deploy` and by hand, and a five-second poll that can never miss one permanently is worth
# more here than an event stream that needs its own reconnect logic.
while :; do
  for c in $(docker ps --format '{{.Names}}' 2>/dev/null | grep -E "$MATCH" || true); do
    if [ ! -f "/tmp/following-$c" ]; then
      : > "/tmp/following-$c"
      ( follow "$c"; rm -f "/tmp/following-$c" ) &
    fi
  done
  sleep 5
done
