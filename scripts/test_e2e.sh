#!/usr/bin/env bash
# End-to-end tests: `go test -p 1` over test/e2e only (spec/relay.md §16, D80; the M2 tunnel tests,
# spec/tunnel.md §16). The harness starts, per test, a Temporal development server from $KONTRA_TEMPORAL_CLI
# (the pinned CLI 1.9.1, Server 1.32.0), two kontra-gate replicas as processes, the tunnel tests' nodes
# (`kontra connect` processes of the binary built once from ./cmd/kontra) and, for TestHelloworldTunnel, the
# a2a-samples helloworld seller (examples/sellers/helloworld/run.sh; $KONTRA_A2A_SAMPLES names its checkout,
# else the test clones the pinned commit), and a database on $KONTRA_TEST_PG (a server where the user may
# create databases) or a postgres:17-alpine container when it is unset; it removes all of them when the
# test ends, pass or fail. Every process it starts carries KONTRA_E2E_RUN and this script's
# KONTRA_E2E_SESSION in its environment, every container the label kontra-e2e-session, every database this
# session's id in its name, and every file it writes (the built gate and node; per test the kontra.yaml that
# holds the Postgres URL and its password, the nodes' keys, and the logs) lives under
# $TMPDIR/kontra-e2e-<session> (test/e2e TempRoot): after the run this script looks for all four, and for
# any process still running a binary built there (a kontra connect or a gate, whatever its environment),
# removes what it finds (a test binary that died, a go test -timeout panic or a crash, cannot clean up), and
# fails the run if there was anything. Extra arguments go to `go test` (e.g. -run TestRelayResume -v).
set -euo pipefail
cd "$(dirname "$0")/.."
GO="${GO:-go}"

if [[ -z "${KONTRA_TEMPORAL_CLI:-}" ]]; then
  echo "test-e2e: set KONTRA_TEMPORAL_CLI to the pinned temporal CLI (1.9.1, Server 1.32.0)" >&2
  exit 2
fi

# This run's processes and containers carry this session id (the harness passes it on), so a check never
# touches another run's.
export KONTRA_E2E_SESSION="e2e-$$-$RANDOM"
status=0
"$GO" test -p 1 -count=1 -timeout "${KONTRA_E2E_TIMEOUT:-60m}" "$@" ./test/e2e/... || status=$?

leaked=0
dir="${TMPDIR:-/tmp}/kontra-e2e-$KONTRA_E2E_SESSION"
# what names a leftover process for the report: a gate replica, a node, the seller, Temporal, or other.
what() {
  local cmd="$1"
  case "$cmd" in
    *kontra-gate\ serve*) echo "a gate replica" ;;
    *kontra\ connect*) echo "a node (kontra connect)" ;;
    *helloworld*|*__main__.py*|*uv\ run*) echo "the helloworld seller" ;;
    *temporal*) echo "the Temporal server" ;;
    *) echo "a process" ;;
  esac
}
for environ in /proc/[0-9]*/environ; do
  pid="${environ#/proc/}"
  pid="${pid%/environ}"
  [[ "$pid" == "$$" ]] && continue
  env="$({ tr '\0' '\n' <"$environ"; } 2>/dev/null || true)" # a process may end while the loop runs
  exe="$(readlink "/proc/$pid/exe" 2>/dev/null || true)"
  if { grep -q '^KONTRA_E2E_RUN=' <<<"$env" && grep -qx "KONTRA_E2E_SESSION=$KONTRA_E2E_SESSION" <<<"$env"; } ||
    [[ "$exe" == "$dir/"* ]]; then
    cmd="$({ tr '\0' ' ' <"/proc/$pid/cmdline"; } 2>/dev/null | cut -c1-160)"
    echo "test-e2e: $(what "$cmd") outlived the run (pid $pid): $cmd" >&2
    kill -9 "$pid" 2>/dev/null || true
    leaked=1
  fi
done
if command -v docker >/dev/null 2>&1; then
  while read -r name; do
    [[ -z "$name" ]] && continue
    echo "test-e2e: container $name outlived the run" >&2
    docker rm -f "$name" >/dev/null 2>&1 || true
    leaked=1
  done < <(docker ps -a --filter "label=kontra-e2e-session=$KONTRA_E2E_SESSION" --format '{{.Names}}' 2>/dev/null || true)
fi
# A test binary that died (a timeout, a crash) could not drop its databases on $KONTRA_TEST_PG; their
# names start with kontra_e2e_ and this session's id (test/e2e DatabasePrefix).
if [[ -n "${KONTRA_TEST_PG:-}" ]] && command -v psql >/dev/null 2>&1; then
  prefix="kontra_e2e_$(tr 'A-Z' 'a-z' <<<"$KONTRA_E2E_SESSION" | tr -c 'a-z0-9\n' '_')_"
  while read -r db; do
    [[ -z "$db" ]] && continue
    echo "test-e2e: database $db outlived the run" >&2
    psql "$KONTRA_TEST_PG" -Atqc "drop database if exists \"$db\" with (force)" >/dev/null || true
    leaked=1
  done < <(psql "$KONTRA_TEST_PG" -Atqc "select datname from pg_database where starts_with(datname, '$prefix')" 2>/dev/null || true)
fi
# The test binary removes its directory when it ends; one that died could not.
if [[ -e "$dir" ]]; then
  echo "test-e2e: directory $dir outlived the run" >&2
  rm -rf -- "$dir"
  leaked=1
fi
if [[ $leaked -ne 0 ]]; then
  echo "test-e2e: the run left processes, containers, databases or files behind (removed now)" >&2
  [[ $status -eq 0 ]] && status=1
fi
exit $status
