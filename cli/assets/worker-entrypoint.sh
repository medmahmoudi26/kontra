#!/bin/sh
# Self-contained kontra worker entrypoint: TWO processes, one container.
#
#   the actor    a Temporal activity worker, polling {actor}-{version}-sessions
#   the handler  the Go workflow worker, polling {actor}-{version}
#
# ALWAYS two, because there is one deployed kind (ADR 0023 §9). The single-process branch here
# served the retired Activity bundle, which had no backing workflow for a handler to own.
#
# TWO, not more: no broker, no placement service and no bundled Redis, because a container
# holding its own state is an island of it. Both processes reach OUT to the Controller: Temporal
# (KONTRA_ADDRESS), Redis (KONTRA_REDIS_HOST), S3 (KONTRA_S3_ENDPOINT) and the catalog
# (KONTRA_ORCHESTRATOR_URL).
#
#   docker run -e KONTRA_ADDRESS=<ctrl>:7233 -e KONTRA_ORCHESTRATOR_URL=http://<ctrl>:8088 \
#              -e KONTRA_S3_ENDPOINT=http://<ctrl>:8333 -e KONTRA_REDIS_HOST=<ctrl>:6379 <image>
set -eu

: "${KONTRA_ACTOR_NAME:?worker image is missing KONTRA_ACTOR_NAME (rebuild via kontra deploy)}"

log() { echo "[worker] $*" >&2; }
log "actor=${KONTRA_ACTOR_NAME}@${KONTRA_ACTOR_VERSION:-?} temporal=${KONTRA_ADDRESS:-unset} redis=${KONTRA_REDIS_HOST:-unset}"

# LOG MODE. Both processes write to $LOG_DIR. The default is /tmp — inside the container, so it
# dies with it. Set KONTRA_LOG_DIR to a MOUNTED path (cli/scale.go --logs binds
# <hostdir>/<worker>:/kontra/logs) and the files survive the container, which is what makes a
# long scan inspectable after the fact. Deliberately NOT /tmp-as-the-mount: an actor's own
# scratch also lands in /tmp and would fill the host disk.
LOG_DIR="${KONTRA_LOG_DIR:-/tmp}"
mkdir -p "$LOG_DIR"

# OpenTelemetry (roadmap platform-x100 #03). Exported into the environment BOTH processes see;
# there is no third component with its own config file any more, which is what made this a rewrite
# rather than an export before. Only the handler acts on it — neither SDK's actor host installs a
# tracer — so OTEL_SERVICE_NAME below is read by nothing today and is kept for the process that
# grows one.
#
# NO DEFAULT, and that is the point (ADR 0031): kontra runs no collector, so an unset
# KONTRA_OTEL_ENDPOINT means the handler builds no exporter at all rather than retrying an
# address nobody listens on. `cli/scale.go` used to pass `jaeger:4317` here for every managed
# worker; that service no longer exists.
if [ -n "${KONTRA_OTEL_ENDPOINT:-}" ]; then
  export OTEL_EXPORTER_OTLP_ENDPOINT="http://${KONTRA_OTEL_ENDPOINT}"
  export OTEL_SERVICE_NAME="kontra-actor-${KONTRA_ACTOR_NAME}"
  export KONTRA_OTEL_SAMPLING="${KONTRA_OTEL_SAMPLING:-1.0}"
  log "otel -> ${KONTRA_OTEL_ENDPOINT} (sampling ${KONTRA_OTEL_SAMPLING})"
fi

# A worker image built before ADR 0023 §9 carries KONTRA_ACTOR_KIND=activity and an entrypoint
# that no longer exists here. REFUSE it rather than booting it as an actor: its entry module is
# `activities.py`, so the two-process path below would launch a handler beside a file that never
# calls actor.serve(), and the container would idle looking healthy.
if [ "${KONTRA_ACTOR_KIND:-actor}" = "activity" ]; then
  log "this image is kind=activity, retired with ADR 0023 §9 — rebuild it with kontra deploy"
  exit 1
fi

# BOTH DESTINATIONS, VIA A FIFO. The redirect used to be `>"$LOG_DIR/host.log" 2>&1`, which sent
# every line the actor wrote to a file INSIDE the container and nothing at all to stdout — so
# `docker logs <worker>` showed the two `[worker]` banner lines above and then silence, for the
# whole life of a scan. The only way to watch a running actor was to know the path and
# `docker exec … tail -f /tmp/host.log`, which is not something an operator should have to know,
# and is impossible for a Machine in a Fleet.
#
# It must be BOTH and not just stdout: the file is the documented reason $LOG_DIR exists (mount
# it and a long scan stays inspectable after the container is gone), and it is what
# `cli/scale.go --logs` binds.
#
# `tee` reads the FIFO rather than sitting in a pipeline, because `cmd | tee … &` would make `$!`
# the pid of TEE — and $HOST_PID is what the supervise loop below watches and what the trap
# kills. A pipeline here would leave this script watching the wrong process and reporting a dead
# actor as alive.
pipe_to_both() { # <fifo> <logfile>
  [ -p "$1" ] || { rm -f "$1"; mkfifo "$1"; }
  tee -a "$2" <"$1" &
}
pipe_to_both "$LOG_DIR/host.pipe" "$LOG_DIR/host.log"
pipe_to_both "$LOG_DIR/handler.pipe" "$LOG_DIR/handler.log"

# WHERE THE TWO HALVES LIVE, AND WHY IT IS NOT HARDCODED ANY MORE. The generated Dockerfile put
# the actor at `/actor/<name>/` and the handler at `/kontra/handler`; the CNB lifecycle stages an
# app wherever the platform says and runs the process with that directory as the working dir, so
# an absolute path is the one thing a buildpack-built image cannot promise. Both default to the
# old locations, so an image built before the buildpack path keeps working unchanged.
ACTOR_ROOT="${KONTRA_ACTOR_ROOT:-/actor/${KONTRA_ACTOR_NAME}}"
HANDLER_BIN="${KONTRA_HANDLER_BIN:-/kontra/handler}"
[ -x "$HANDLER_BIN" ] || { log "no handler at $HANDLER_BIN (set KONTRA_HANDLER_BIN)"; exit 1; }

# 1) the actor — a Python actor is run by Python; a Go actor is a compiled binary beside its
#    manifest. KONTRA_ACTOR_ENGINE is baked by `kontra deploy`; it defaults to py so an older
#    worker image keeps working.
#
# PYTHON IS UNBUFFERED (-u) because its stdout is now a pipe, not a tty, so the interpreter
# switches to block buffering and a progress line written every few seconds arrives in 4 KB
# lumps — which turns "logging as it progresses" back into silence, just with a different cause.
if [ "${KONTRA_ACTOR_ENGINE:-py}" = "go" ]; then
  "${ACTOR_ROOT}/${KONTRA_ACTOR_NAME}" >"$LOG_DIR/host.pipe" 2>&1 &
else
  python3 -u "${ACTOR_ROOT}/${KONTRA_ACTOR_ENTRY:-actor.py}" >"$LOG_DIR/host.pipe" 2>&1 &
fi
HOST_PID=$!

# 2) the handler — the workflow half.
"$HANDLER_BIN" >"$LOG_DIR/handler.pipe" 2>&1 &
HANDLER_PID=$!

log "started: host=$HOST_PID handler=$HANDLER_PID"

# Supervise: exit non-zero the moment either dies so the container restarts. A half-dead worker
# is worse than a dead one — it keeps its Temporal lease and units time out one by one.
term() {
  kill "$HOST_PID" "$HANDLER_PID" 2>/dev/null || true
  exit 0
}
trap term TERM INT
while kill -0 "$HOST_PID" 2>/dev/null && kill -0 "$HANDLER_PID" 2>/dev/null; do
  sleep 3
done
log "a process exited — tearing down (see ${LOG_DIR}/{host,handler}.log)"
kill "$HOST_PID" "$HANDLER_PID" 2>/dev/null || true
exit 1
