#!/usr/bin/env bash
# The pinned A2A TCK against the gate (spec/relay.md §0, §16 tck_jsonrpc, tck_rest, tck_behind_caddy; D18, DM4-12):
# `make tck VARIANT=direct|tunnel|caddy|auth`, or scripts/tck.sh <variant>.
#
#   direct  the `tck` service on the direct backend, the TCK talking to the gate itself (tck_jsonrpc, tck_rest)
#   tunnel  the same seller hosted through `kontra connect` (tck_jsonrpc, tck_rest on the tunnel backend)
#   caddy   the direct backend behind examples/front/caddy, TLS terminated by Caddy (tck_behind_caddy)
#   auth    the direct backend behind a loopback Caddy that adds `Authorization: Bearer <a tck API key>` to every
#           request, so the TCK runs as an API key's principal and reaches ListTasks and push configs, which the
#           anonymous principal is refused (D20; the TCK sends no credential, a2a-tck#163); nightly only
#
# One run: a fresh database on $KONTRA_TEST_PG (a server where the user may create databases) or, when it is
# unset, a postgres:17-alpine container; a Temporal development server from $KONTRA_TEMPORAL_CLI (the pinned CLI
# 1.9.1, Server 1.32) on free loopback ports; kontra-gate, the seller (examples/sellers/tck) and, for tunnel,
# kontra (`kontra connect`, enrolled with a service token) built from this checkout; Caddy ($KONTRA_CADDY, the
# pinned 2.11.7) for caddy and auth. Every process runs from the run's directory and in a process group of its
# own. Then the plugin's self-test (test/tck/test_kontra_xfail.py), and the TCK once per binding (--transport
# jsonrpc, then http_json), each with test/tck/kontra_xfail.py enforcing test/tck/expected-failures.txt for the
# variant (TCK_VARIANT; KONTRA_TCK_EXPECTED_FAILURES names another file, to try one). $KONTRA_TCK_DIR is the a2a-tck checkout at the pinned commit, its tracked files
# unmodified; the reports land in its reports/ directory: <variant>-<binding>.xml (JUnit), .html (pytest-html),
# -compatibility.json/.html (the TCK's own), and <variant>-logs/ (the gate's, the seller's, the node's,
# Caddy's and Temporal's logs).
#
# The trap kills every process group the run started, drops the database (or removes the container) and removes
# the run's directory; then the run fails if any process whose command line names that directory is still alive
# (pgrep), and kills it. The exit status is non-zero when either binding's run failed.
set -euo pipefail
cd "$(dirname "$0")/.."
repo="$PWD"
GO="${GO:-go}"

TCK_PIN=d4d6fae7887f88ef607fb461c13e3b0e1802f19b
CADDY_VERSION=v2.11.7

variant="${1:-${VARIANT:-direct}}"
case "$variant" in
  direct | tunnel | caddy | auth) ;;
  *) echo "tck: VARIANT $variant: want direct, tunnel, caddy or auth" >&2; exit 2 ;;
esac
read -r -a bindings <<<"${KONTRA_TCK_BINDINGS:-jsonrpc http_json}"

die() { echo "tck: $*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "$1 not found: $2"; }

# --- prerequisites, before anything starts ---
[[ -n "${KONTRA_TEMPORAL_CLI:-}" ]] || die "set KONTRA_TEMPORAL_CLI to the pinned temporal CLI (1.9.1, Server 1.32)"
"$KONTRA_TEMPORAL_CLI" --version | grep -q 'Server 1\.32\.' ||
  die "$KONTRA_TEMPORAL_CLI is not the pinned temporal CLI (Server 1.32): $("$KONTRA_TEMPORAL_CLI" --version)"
[[ -n "${KONTRA_TCK_DIR:-}" ]] || die "set KONTRA_TCK_DIR to an a2a-tck checkout at $TCK_PIN"
tck_dir="$(cd "$KONTRA_TCK_DIR" && pwd)"
[[ "$(git -C "$tck_dir" rev-parse HEAD)" == "$TCK_PIN" ]] ||
  die "$tck_dir is at $(git -C "$tck_dir" rev-parse HEAD), not the pinned $TCK_PIN (spec/relay.md §0)"
git -C "$tck_dir" diff --quiet HEAD -- ||
  die "$tck_dir has modified tracked files ($(git -C "$tck_dir" diff --name-only HEAD | tr '\n' ' ')): the TCK runs as pinned (uv run --frozen never rewrites uv.lock)"
need uv "the TCK runs under uv (D18)"
need python3 "free ports are found with python3"
need curl "readiness is checked with curl"
need setsid "every process runs in a group of its own"
need pgrep "leftovers are found with pgrep"
caddy=""
if [[ "$variant" == caddy || "$variant" == auth ]]; then
  caddy="${KONTRA_CADDY:-$(command -v caddy || true)}"
  [[ -n "$caddy" && -x "$caddy" ]] || die "VARIANT=$variant needs Caddy $CADDY_VERSION: set KONTRA_CADDY or put caddy on PATH"
  "$caddy" version | grep -q "^$CADDY_VERSION " || die "$caddy is not Caddy $CADDY_VERSION: $("$caddy" version)"
fi
if [[ -n "${KONTRA_TEST_PG:-}" ]]; then
  need psql "KONTRA_TEST_PG needs psql to create and drop the run's database"
else
  need docker "without KONTRA_TEST_PG the run's database is a postgres:17-alpine container"
fi

run="$(mktemp -d "${TMPDIR:-/tmp}/kontra-tck-XXXXXX")"
id="$(basename "$run" | tr '[:upper:]' '[:lower:]' | tr -c 'a-z0-9\n' '_')"
reports="$tck_dir/reports"
declare -A pids=()
db_drop="" container=""

# start <name> <command...>: runs the command in a session (and so a process group) of its own, its stdout in
# $run/<name>.out and its stderr in $run/<name>.log; every process carries KONTRA_TCK_RUN.
start() {
  local name="$1"
  shift
  KONTRA_TCK_RUN="$run" setsid "$@" >"$run/$name.out" 2>"$run/$name.log" </dev/null &
  pids[$name]=$!
}

alive() { kill -0 "$1" 2>/dev/null; }

# shellcheck disable=SC2317 # cleanup runs from the trap
cleanup() {
  local status=$? name pid
  trap - EXIT INT TERM
  set +e
  for name in "${!pids[@]}"; do
    kill -TERM -- "-${pids[$name]}" 2>/dev/null
  done
  # The gate drains on the first SIGTERM (drain_timeout 2s in its configuration); the rest stop at once.
  for _ in $(seq 50); do
    local any=0
    for pid in "${pids[@]}"; do alive "$pid" && any=1; done
    [[ $any -eq 0 ]] && break
    sleep 0.2
  done
  for name in "${!pids[@]}"; do
    kill -KILL -- "-${pids[$name]}" 2>/dev/null
  done
  wait 2>/dev/null
  if [[ -d "$reports" ]]; then
    mkdir -p "$reports/$variant-logs"
    for f in "$run"/*.log "$run"/*.out; do [[ -f "$f" ]] && cp "$f" "$reports/$variant-logs/"; done
  fi
  if [[ -n "$db_drop" ]]; then
    psql "$KONTRA_TEST_PG" -Atqc "drop database if exists \"$db_drop\" with (force)" >/dev/null ||
      { echo "tck: could not drop the database $db_drop" >&2; status=1; }
  fi
  if [[ -n "$container" ]]; then
    docker rm -f "$container" >/dev/null 2>&1 || { echo "tck: could not remove the container $container" >&2; status=1; }
  fi
  local left
  left="$(pgrep -af -- "$run/" || true)"
  if [[ -n "$left" ]]; then
    echo "tck: processes outlived the run (killed now):" >&2
    echo "$left" >&2
    pkill -KILL -f -- "$run/" || true
    [[ $status -eq 0 ]] && status=1
  fi
  rm -rf -- "$run"
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# free_ports <n>: n distinct free loopback ports.
free_ports() {
  python3 -c '
import socket, sys
ss = [socket.socket() for _ in range(int(sys.argv[1]))]
for s in ss: s.bind(("127.0.0.1", 0))
print(" ".join(str(s.getsockname()[1]) for s in ss))
for s in ss: s.close()' "$1"
}

# wait_for <what> <seconds> <pid> <command...>: runs the command until it succeeds; fails when <pid> (the process
# being waited on) exits or the time is up, with the tail of its log.
wait_for() {
  local what="$1" secs="$2" pid="$3" name
  shift 3
  for _ in $(seq $((secs * 4))); do
    "$@" >/dev/null 2>&1 && return 0
    if ! alive "$pid"; then
      for name in "${!pids[@]}"; do [[ "${pids[$name]}" == "$pid" ]] && tail -n 40 "$run/$name.log" "$run/$name.out" >&2; done
      die "$what: the process exited"
    fi
    sleep 0.25
  done
  for name in "${!pids[@]}"; do [[ "${pids[$name]}" == "$pid" ]] && tail -n 40 "$run/$name.log" "$run/$name.out" >&2; done
  die "$what: not within $secs s"
}

# --- binaries, run from the run's directory (so pgrep on it finds every process the run started) ---
mkdir -p "$run/bin" "$reports"
"$GO" build -o "$run/bin/kontra-gate" ./cmd/kontra-gate
"$GO" build -o "$run/bin/kontra" ./cmd/kontra
"$GO" build -o "$run/bin/tck" ./examples/sellers/tck
ln -s "$(command -v "$KONTRA_TEMPORAL_CLI")" "$run/bin/temporal"
[[ -n "$caddy" ]] && ln -s "$(command -v "$caddy")" "$run/bin/caddy"

# --- the database ---
if [[ -n "${KONTRA_TEST_PG:-}" ]]; then
  db="kontra_tck_$id"
  psql "$KONTRA_TEST_PG" -Atqc "create database \"$db\"" >/dev/null
  db_drop="$db"
  postgres="$(python3 -c '
import sys, urllib.parse as u
p = u.urlsplit(sys.argv[1])
print(u.urlunsplit(p._replace(path="/" + sys.argv[2])))' "$KONTRA_TEST_PG" "$db")"
else
  container="kontra-tck-$id"
  docker run -d --name "$container" --label "kontra-tck-run=$run" -e POSTGRES_USER=kontra -e POSTGRES_PASSWORD=kontra \
    -e POSTGRES_DB=kontra -p 127.0.0.1::5432 postgres:17-alpine >/dev/null
  pg_port="$(docker port "$container" 5432/tcp | head -1 | sed 's/.*://')"
  for _ in $(seq 120); do
    docker exec "$container" pg_isready -U kontra -h 127.0.0.1 >/dev/null 2>&1 && break
    sleep 0.5
  done
  postgres="postgres://kontra:kontra@127.0.0.1:$pg_port/kontra?sslmode=disable"
fi

# --- Temporal: the development server, in memory, with the settings spec/relay.md §14.2 asks of the gate's ---
read -r t_port t_http t_metrics <<<"$(free_ports 3)"
start temporal "$run/bin/temporal" server start-dev --headless --ip 127.0.0.1 --port "$t_port" --http-port "$t_http" \
  --metrics-port "$t_metrics" --namespace kontra --log-level warn \
  --dynamic-config-value frontend.WorkerCommandsEnabled=true \
  --dynamic-config-value system.enableCancelActivityWorkerCommand=true
wait_for "the Temporal development server on 127.0.0.1:$t_port" 60 "${pids[temporal]}" \
  "$run/bin/temporal" operator namespace describe --address "127.0.0.1:$t_port" --namespace kontra

# --- the seller ---
start seller "$run/bin/tck" --host 127.0.0.1 --port 0
wait_for "the TCK seller" 20 "${pids[seller]}" grep -q '^http://' "$run/seller.out"
seller="$(head -1 "$run/seller.out")"

# --- the gate, from its template (test/tck/gate-<variant>.yaml) ---
read -r g_port f_port f_http <<<"$(free_ports 3)"
gate="http://127.0.0.1:$g_port"
case "$variant" in
  direct | tunnel) public="$gate" ;;
  caddy) public="https://127.0.0.1:$f_port" ;;
  auth) public="http://127.0.0.1:$f_port" ;;
esac
sed -e "s|@LISTEN@|127.0.0.1:$g_port|" -e "s|@PUBLIC_URL@|$public|" -e "s|@POSTGRES@|$postgres|" \
  -e "s|@TEMPORAL@|127.0.0.1:$t_port|" -e "s|@SELLER@|$seller|" "test/tck/gate-$variant.yaml" >"$run/kontra.yaml"
chmod 600 "$run/kontra.yaml"
if grep -q '@[A-Z_]*@' "$run/kontra.yaml"; then die "test/tck/gate-$variant.yaml has a placeholder this script does not fill"; fi
"$run/bin/kontra-gate" migrate -f "$run/kontra.yaml" up >"$run/migrate.log" 2>&1 || { cat "$run/migrate.log" >&2; die "migrate failed"; }
key=""
if [[ "$variant" == auth ]]; then
  key="$("$run/bin/kontra-gate" enroll -f "$run/kontra.yaml" --api-key tck --prefix tck 2>>"$run/enroll.log")"
fi
start gate "$run/bin/kontra-gate" serve -f "$run/kontra.yaml" --log-level info
wait_for "the gate on $gate" 90 "${pids[gate]}" curl -fsS -o /dev/null "$gate/healthz"

# --- the node (tunnel): kontra connect, enrolled for tck with a service token, its ~/.kontra in the run ---
if [[ "$variant" == tunnel ]]; then
  token="$("$run/bin/kontra-gate" enroll -f "$run/kontra.yaml" tck 2>>"$run/enroll.log")"
  mkdir -p "$run/node-home"
  HOME="$run/node-home" start node "$run/bin/kontra" connect --gate "$public" --name tck-node --log json \
    --listen 127.0.0.1:0 --serve "a2a:tck=$seller" --enroll "$token"
  unset token
  wait_for "kontra connect" 60 "${pids[node]}" grep -q '^kontra connect: connected to ' "$run/node.out"
fi

# --- the front (caddy: examples/front/caddy as an operator runs it; auth: test/tck/front-auth.Caddyfile) ---
curl_ca=()
if [[ "$variant" == caddy ]]; then
  KONTRA_FRONT_SITE="$public" KONTRA_GATE="127.0.0.1:$g_port" KONTRA_CADDY_ADMIN=off KONTRA_CADDY_BIND=127.0.0.1 \
    KONTRA_CADDY_HTTP_PORT="$f_http" \
    KONTRA_CADDY_HTTPS_PORT="$f_port" XDG_DATA_HOME="$run/caddy-data" XDG_CONFIG_HOME="$run/caddy-config" \
    start caddy "$run/bin/caddy" run --adapter caddyfile --config "$repo/examples/front/caddy/Caddyfile"
  # Caddy's local CA (tls internal, which Caddy uses for an IP site) signs the front's certificate; the TCK's
  # httpx and curl trust it through SSL_CERT_FILE, nothing is installed in the system's store.
  ca="$run/caddy-data/caddy/pki/authorities/local/root.crt"
  wait_for "Caddy's local CA" 30 "${pids[caddy]}" test -s "$ca"
  curl_ca=(--cacert "$ca")
  export SSL_CERT_FILE="$ca"
elif [[ "$variant" == auth ]]; then
  KONTRA_FRONT_PORT="$f_port" KONTRA_GATE="127.0.0.1:$g_port" KONTRA_CADDY_HTTP_PORT="$f_http" KONTRA_TCK_KEY="$key" \
    XDG_DATA_HOME="$run/caddy-data" XDG_CONFIG_HOME="$run/caddy-config" \
    start caddy "$run/bin/caddy" run --adapter caddyfile --config "$repo/test/tck/front-auth.Caddyfile"
fi
unset key

# --- the card, where the TCK reads it ---
sut="$public/agents/tck"
wait_pid="${pids[caddy]:-${pids[gate]}}"
wait_for "the card at $sut/.well-known/agent-card.json" 60 "$wait_pid" \
  curl -fsS "${curl_ca[@]}" -o /dev/null "$sut/.well-known/agent-card.json"
echo "tck: $variant: the gate serves $sut (seller $seller, Temporal 127.0.0.1:$t_port)"

# --- the TCK, its own environment pinned by its uv.lock (D18) ---
export PYTHONDONTWRITEBYTECODE=1
(cd "$tck_dir" && uv sync --frozen --python "${KONTRA_TCK_PYTHON:-3.12}" --quiet)
export PYTHONPATH="$repo/test/tck"
export TCK_EXPECTED_FAILURES="${KONTRA_TCK_EXPECTED_FAILURES:-$repo/test/tck/expected-failures.txt}"
export TCK_VARIANT="$variant"
(cd "$tck_dir" && uv run --frozen python -m pytest -p pytester -p no:cacheprovider -q \
  -o asyncio_default_fixture_loop_scope=function "$repo/test/tck/test_kontra_xfail.py") ||
  die "the self-test of test/tck/kontra_xfail.py failed"

status=0
for b in "${bindings[@]}"; do
  echo "tck: $variant: --transport $b"
  (cd "$tck_dir" && uv run --frozen ./run_tck.py --sut-host "$sut" --transport "$b" -v -- \
    -p kontra_xfail -p no:cacheprovider -o asyncio_default_fixture_loop_scope=function --webhook-host 127.0.0.1 \
    --junitxml="reports/$variant-$b.xml" --html="reports/$variant-$b.html" --self-contained-html \
    --compatibility-report="reports/$variant-$b-compatibility") || status=1
done
exit $status
