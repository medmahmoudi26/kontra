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

# 1) the actor — a Python actor is run by Python; a Go actor is a compiled binary beside its
#    manifest. KONTRA_ACTOR_ENGINE is baked by `kontra deploy`; it defaults to py so an older
#    worker image keeps working.
if [ "${KONTRA_ACTOR_ENGINE:-py}" = "go" ]; then
  "/actor/${KONTRA_ACTOR_NAME}/${KONTRA_ACTOR_NAME}" >"$LOG_DIR/host.log" 2>&1 &
else
  python3 "/actor/${KONTRA_ACTOR_NAME}/${KONTRA_ACTOR_ENTRY:-actor.py}" >"$LOG_DIR/host.log" 2>&1 &
fi
HOST_PID=$!

# 2) the handler — the workflow half.
/kontra/handler >"$LOG_DIR/handler.log" 2>&1 &
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
