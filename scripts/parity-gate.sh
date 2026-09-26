#!/usr/bin/env bash
# parity-gate.sh — does the binary replace compose for LOCAL DEVELOPMENT? Run it and find out.
#
# ADR 0031 §5 locks two conditions, and says the second one carries the weight:
#
#   1. an actor's own suite green against the binary
#   2. the local actor path end to end — deploy → scale → dispatch → a Dataset that queries
#
# It also says why condition 1 is not, by itself, evidence of anything: an actor's suite runs
# in-process against `actorkit.testing` stubs and passes on a bare runner with no control plane,
# so IT WOULD GO GREEN AGAINST A BINARY THAT NEVER STARTED. This script runs it
# BOTH ways on one commit — once with no control plane (the SDK-side contract, exactly as CI has
# always run it) and once with `KONTRA_ADDRESS` pointing at the appliance, which turns the suite's
# `e2e`-marked tests on. Two runs of one suite is the comparison the ADR asks for; see the
# `## What this does NOT compare` note at the bottom for the leg that no longer exists.
#
# ── WHAT IT ASSERTS, AND WHY IT IS NOT "THE RUN SAID COMPLETED" ──────────────────────────────
#
# Every failure this gate is aimed at reported `completed`:
#
#   · a chained dispatch isolated every Unit on a fleet and was invisible on one box;
#   · a wiped batch came back as a clean `completed` in seven minutes;
#   · a node whose Units all failed reported success with empty output.
#
# A gate that checked terminal status would have passed while all three were happening. So this
# one compares INTEGERS — units in, records out, Units dropped — for four calls whose right
# answers are known, and it fails if `boom` (every Unit isolated) and `silent` (every Unit fine,
# nothing found) come back looking the same. A negative test needs a control: "the Dataset is
# empty" proves nothing unless the same path can be shown to fill it, which is what the `echo`
# leg does two calls earlier.
#
# ── ISOLATION, WHICH IS STRUCTURAL AND NOT A NAMING CONVENTION ───────────────────────────────
#
# On 2026-08-26 an agent satisfying a "complete a run" criterion ran a workflow against the live
# stack with three isolated QUEUE NAMES. One module pinned a queue constant, the isolation did not
# hold, and the run deleted 223,378 rows of run data
# (docs/incidents/2026-08-26-sweep-deleted-run-data.md).
#
# Names are not isolation. This script's isolation is that the appliance it starts OWNS ITS OWN
# EVERYTHING: its own data directory (a fresh mktemp -d), its own embedded Temporal and its
# database, its own object store, its own catalog and ledger, its own KV, its own registry. There
# is no shared server for a stray constant to reach. On top of that, every port is chosen outside
# the defaults and PROVEN FREE before anything binds — so a collision refuses to start rather than
# quietly joining something that is already running — and `--data-dir` is exported as
# KONTRA_DATA_DIR, which is the same variable `kontra deploy`, `kontra serve` and the orchestrator
# child all resolve, so the whole command set points at this instance by construction.
#
# It never runs `sweepDatasetsWorkflow`, and it stops, restarts and removes exactly the containers
# it created, found by the label it created them with.
#
# ── USAGE ────────────────────────────────────────────────────────────────────────────────────
#
#   scripts/parity-gate.sh                 # the whole gate
#   scripts/parity-gate.sh --keep          # leave the appliance up afterwards, to poke at it
#   KONTRA_GATE_PORT_BASE=41000 …          # move every port at once
#   KONTRA_GATE_UNITS=24 …                 # a wider Batch
#   KONTRA_GATE_WORKER_NETWORK=host …      # force the worker network; the gate otherwise probes
#                                          #   the bridge and only falls back when it is blocked
#
# Exit 0 means the gate passes. Anything else prints which condition failed and what it measured.

set -uo pipefail

# ── where we are, and where our things go ───────────────────────────────────────────────────
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO" || exit 1

KEEP=0
for arg in "$@"; do
  case "$arg" in
    --keep) KEEP=1 ;;
    -h|--help) sed -n '2,60p' "${BASH_SOURCE[0]}"; exit 0 ;;
    *) echo "unknown argument: $arg (try --help)" >&2; exit 2 ;;
  esac
done

UNITS="${KONTRA_GATE_UNITS:-12}"
REPLICAS="${KONTRA_GATE_REPLICAS:-2}"
ACTOR_DIR="tests/parity/actor"
ACTOR_NAME="paritygate"
ACTOR_VERSION="0.1.0"
WORKFLOW_DIR="tests/parity/workflow"

GATE_RUN="parity-$(date +%s)-$$"
GATE_TMP="$(mktemp -d "${TMPDIR:-/tmp}/kontra-parity-XXXXXX")"
DATA_DIR="$GATE_TMP/data"
KONTRA_HOME_DIR="$GATE_TMP/home"
LOGS="$GATE_TMP/logs"
mkdir -p "$DATA_DIR" "$KONTRA_HOME_DIR" "$LOGS"

FAILURES=()
# REACHED_END IS THE HALF A FAILURE LIST CANNOT COVER, and it is here because this script got it
# wrong first. Interrupted in the middle of step 3 — before a single count had been compared — the
# EXIT trap ran, found FAILURES empty, and printed PARITY GATE PASSES. That is precisely the shape
# of failure the whole file is aimed at: a run that did not do the work reporting success. An
# absence of failures is not evidence; only the last line of the script sets this.
REACHED_END=0
# The trap is armed for EXIT and for INT/TERM, so it fires twice on a Ctrl-C — which tore down
# twice and printed the verdict twice. Once.
CLEANED=0

say()  { printf '\n\033[1m== %s\033[0m\n' "$*"; }
info() { printf '   %s\n' "$*"; }
ok()   { printf '   \033[32mok\033[0m   %s\n' "$*"; }
bad()  { printf '   \033[31mFAIL\033[0m %s\n' "$*"; FAILURES+=("$*"); }

# check <description> <expected> <actual> — the whole assertion vocabulary. It prints both numbers
# on failure, always, because "assertion failed" without the measurement is how a gate becomes a
# thing people re-run instead of read.
check() {
  local what="$1" want="$2" got="$3"
  if [ "$want" = "$got" ]; then
    ok "$what = $got"
  else
    bad "$what: expected $want, measured $got"
  fi
}

# wait_for_pollers <queue> <attempts> <logfile> — is anything actually POLLING that queue?
#
# A CONTAINER THAT IS UP IS NOT A WORKER THAT IS POLLING, and the difference is most of the point
# of this gate. `docker ps` shows a healthy container for one pointed at a control plane that does
# not exist; only Temporal knows whether anybody is listening.
#
# IT READS THE WHOLE ROW, NOT A COLUMN, and that is a correction rather than a preference. The
# first version took awk's `$4` and compared it against the string `(none)`; what the table
# actually prints is `(none — registered but no live worker)`, so `$4` is `(none` — which is not
# equal to `(none)`, so the loop broke on its first pass and reported a live worker that was not
# there. A gate printing a false green about the exact thing it exists to check.
POLLERS_ROW=""
wait_for_pollers() {
  local queue="$1" attempts="$2" logfile="$3" row=""
  for _ in $(seq 1 "$attempts"); do
    "$KONTRA" workers list >"$logfile" 2>&1
    row=$(grep -E "[[:space:]]${queue}[[:space:]]" "$logfile" | head -1)
    case "$row" in
      "" | *"(none"* | *"temporal error"*) sleep 2 ;;
      *) POLLERS_ROW="$row"; return 0 ;;
    esac
  done
  POLLERS_ROW="$row"
  return 1
}

# ── THE PORTS. All non-default, all proven free before anything binds ────────────────────────
#
# THE DEFAULTS ARE THE LIVE STACK'S and are refused by name below, not merely avoided by choosing
# other numbers — a base that happened to land on one would otherwise make this script the thing
# the incident report describes.
BASE="${KONTRA_GATE_PORT_BASE:-39100}"
P_TEMPORAL=$((BASE + 1))
P_S3=$((BASE + 2))
P_KV=$((BASE + 3))
P_CODEC=$((BASE + 4))
P_REGISTRY=$((BASE + 5))
P_API=$((BASE + 6))
P_METRICS=$((BASE + 7))

# The numbers a kontra installation uses when nobody says otherwise. Reaching any of them means
# this script is about to talk to something it did not start.
DEFAULT_PORTS=(7233 8233 8333 8088 8090 6379 5000 18234 9333 8888 8085 5432 55432)

# port_free / has_address — two questions asked WITHOUT A PIPE, and that is not a style choice.
#
# `set -o pipefail` at the top of this file plus `cmd | grep -q pattern` is a race, and it bit
# here: `grep -q` exits the instant it matches, the writer gets SIGPIPE, and pipefail makes the
# PIPELINE fail even though the match succeeded. `ip -o addr show | grep -q " inet 172.17.0.1/"`
# therefore reported "not an address on this machine" — intermittently, and more often the more
# interfaces the box had, because a longer write is likelier to still be in flight. Docker creates
# a veth per container, so the gate got less reliable exactly as it started doing its job.
#
# Capture first, test the string second. There is nothing to signal.
port_free() {
  # A listener on 0.0.0.0 and one on 127.0.0.1 both take the port from us, and the difference does
  # not matter to a bind that is about to fail — so this asks about the port over any address.
  local listeners
  listeners=$(ss -ltnH "sport = :$1" 2>/dev/null)
  [ -z "$listeners" ]
}

has_address() {
  local addrs
  addrs=$(ip -o addr show 2>/dev/null)
  case "$addrs" in *" inet $1/"*) return 0 ;; *) return 1 ;; esac
}

say "0 · isolation"
info "data dir      $DATA_DIR"
info "KONTRA_HOME   $KONTRA_HOME_DIR"
info "ports         temporal=$P_TEMPORAL s3=$P_S3 kv=$P_KV codec=$P_CODEC registry=$P_REGISTRY api=$P_API"

for p in "$P_TEMPORAL" "$P_S3" "$P_KV" "$P_CODEC" "$P_REGISTRY" "$P_API" "$P_METRICS"; do
  for d in "${DEFAULT_PORTS[@]}"; do
    if [ "$p" = "$d" ]; then
      echo "REFUSING: port $p is a kontra default. Move KONTRA_GATE_PORT_BASE." >&2
      exit 2
    fi
  done
  if ! port_free "$p"; then
    echo "REFUSING: port $p is already bound. Something is listening there and this gate will not" >&2
    echo "  join it. Move KONTRA_GATE_PORT_BASE to a free block." >&2
    ss -ltnp "sport = :$p" >&2
    exit 2
  fi
done
ok "every port is free and none is a kontra default"

# THE BIND ADDRESS. Loopback is the appliance's default and is right for every surface an operator
# touches (ADR 0031 §3) — and it is wrong for exactly one customer, which is the one this gate
# needs: an actor Worker is a CONTAINER (ADR 0031 §2), and a container's loopback is its own. So
# the gate binds the docker bridge gateway, which is reachable from this box and from containers
# on it and from nowhere else. `kontra serve --mode docker` refuses a loopback-bound appliance by
# name rather than starting workers that poll nothing, so this is not a detail the gate may skip.
#
# ASKED OF DOCKER, NOT HARD-CODED. `172.17.0.1` is the usual answer and it is not the only one: a
# daemon configured with `bip`, a second bridge, or a CI runner with a different default all move
# it, and a gate that assumed the number would refuse to start on a machine where everything is
# fine. The daemon knows its own gateway; ask it.
BIND="${KONTRA_GATE_BIND:-$(docker network inspect bridge --format '{{ (index .IPAM.Config 0).Gateway }}' 2>/dev/null)}"
BIND="${BIND:-172.17.0.1}"
if ! has_address "$BIND"; then
  echo "REFUSING: $BIND is not an address on this machine, so a worker container could not reach" >&2
  echo "  a control plane bound to it — and it cannot reach loopback either, which is why this" >&2
  echo "  gate needs a bridge-visible address at all." >&2
  echo "  docker says its bridge gateway is: $(docker network inspect bridge --format '{{ (index .IPAM.Config 0).Gateway }}' 2>&1)" >&2
  echo "  this machine has:" >&2
  ip -o addr show 2>/dev/null | sed 's/^/    /' >&2
  echo "  Set KONTRA_GATE_BIND to the right one." >&2
  exit 2
fi
ok "bind $BIND is a local address a worker container can reach"

# ── THE BINARY. Built from this checkout, not whatever is on PATH ────────────────────────────
KONTRA="$GATE_TMP/kontra"
if [ -n "${KONTRA_GATE_BINARY:-}" ]; then
  KONTRA="$KONTRA_GATE_BINARY"
  info "using the binary given in KONTRA_GATE_BINARY: $KONTRA"
else
  say "0b · build the binary under test"
  info "go build ./cli → $KONTRA"
  if ! (cd "$REPO/cli" && go build -o "$KONTRA" .); then
    echo "the binary did not build; there is nothing to gate." >&2
    exit 1
  fi
  ok "built $KONTRA ($(stat -c %s "$KONTRA" 2>/dev/null || echo ?) bytes)"
fi

PYTHON="${PYTHON:-$REPO/.venv/bin/python}"
[ -x "$PYTHON" ] || PYTHON="$(command -v python3 || command -v python)"

# ── the environment every command in this script inherits ────────────────────────────────────
#
# ONE PLACE, EXPORTED ONCE. `KONTRA_DATA_DIR` is what `kontra deploy` reads to find this
# appliance's registry, what `kontra serve --mode docker` reads to find its Temporal, and what the
# orchestrator child resolves its catalog and ledger from — so a single variable is what makes
# every command in this file belong to one installation. `KONTRA_HOME` is separate and also ours:
# it is where `config.yaml` would live, and an inherited one carrying `controller:` would send
# every address resolution at a machine this gate does not own.
export KONTRA_DATA_DIR="$DATA_DIR"
export KONTRA_HOME="$KONTRA_HOME_DIR"
export KONTRA_ADDRESS="$BIND:$P_TEMPORAL"
export KONTRA_S3_ENDPOINT="http://$BIND:$P_S3"
export KONTRA_REDIS_HOST="$BIND:$P_KV"
export KONTRA_ORCHESTRATOR_URL="http://$BIND:$P_API"
# KONTRA_REGISTRY IS DELIBERATELY NOT SET, and this is not an omission — it was set here once and
# it broke the gate. `registryAddress` resolves the flag, then this variable, then THE ADDRESS THE
# RUNNING APPLIANCE PUBLISHED, and the third rung is the one under test: the appliance publishes
# its LOOPBACK registry address because the Docker daemon will not push plain HTTP anywhere else
# (appliance/registry.go). Naming the bind address here overrode that with the very address the
# daemon refuses, and `kontra deploy` died on
#     http: server gave HTTP response to HTTPS client
# — a gate that had pinned the wrong half of its own mechanism. Unset, so the resolution is
# exercised rather than bypassed.
unset KONTRA_REGISTRY
# THE QUERY SURFACE STILL NEEDS A TOKEN, AND THIS IS THE ONE PLACE THAT SAYS SO.
#
# `kontra dataset query` prefers the orchestrator (~25× faster: it already holds the catalog open)
# and that route is bearer-gated, failing CLOSED — `auth.ts` returns 503 with nothing configured
# rather than defaulting open. With no token the CLI falls back to running DuckDB here, and that
# path is REFUSED too, for a different reason: the catalog is a file now (ADR 0031 §1b), DuckDB
# takes an exclusive lock on it, and the control plane is holding it. So on a fresh `kontra up`
# both routes to a Dataset are shut, and the second one reports a lock conflict naming a PID.
#
# Exported BEFORE `kontra up`, so the child inherits the same value the CLI will send. That is the
# whole mechanism — one variable, both sides — and it is why this is not `--explore-token`.
export KONTRA_EXPLORE_TOKEN="parity-gate-$(head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n')"
# Not inherited from the operator's shell: a controller name in the environment is exactly how a
# "isolated" run reaches a stack it did not start.
unset KONTRA_NAMESPACE KONTRA_S3_BUCKET KONTRA_ORCHESTRATOR_DB KONTRA_MATERIALIZATION_DB KONTRA_DUCKLAKE_CATALOG

APPLIANCE_PID=""
WF_PID=""
BEACON_PID=""
# WORKER_NETWORK is empty for "let `resolveWorkerPlane` decide", which is the documented default
# and resolves to the docker bridge against a running appliance. The pre-flight in step 3 sets it
# to `host` only when it has PROVEN the bridge cannot reach this appliance.
WORKER_NETWORK="${KONTRA_GATE_WORKER_NETWORK:-}"
WORKER_TOPOLOGY="worker containers on the docker bridge, dialling the appliance's bound address"

cleanup() {
  local rc=$?
  [ "$CLEANED" = "1" ] && return
  CLEANED=1
  if [ "$KEEP" = "1" ] && [ ${#FAILURES[@]} -gt 0 ]; then
    say "--keep: leaving the appliance up at $DATA_DIR (pid $APPLIANCE_PID)"
    return
  fi
  say "5 · teardown"
  # The two host workers first: each holds a Temporal connection. Killed by PID, never by
  # `pkill -f` — a pattern that matches this script's own command line kills the script (exit
  # 144), and the bracket trick does not save you.
  for pid_var in WF_PID BEACON_PID; do
    pid="${!pid_var}"
    if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
      # SIGTERM to the PID, and only the PID. `kontra serve` and `kontra workflow serve` both run
      # their children under a `signal.NotifyContext`, so the cancel travels down; killing a
      # process GROUP from here would kill this script, which shares it, and a `pkill -f` pattern
      # matches this script's own command line.
      kill -TERM "$pid" 2>/dev/null
      for _ in $(seq 1 20); do kill -0 "$pid" 2>/dev/null || break; sleep 0.5; done
      kill -0 "$pid" 2>/dev/null && kill -KILL "$pid" 2>/dev/null
      ok "$pid_var stopped"
    fi
  done
  # The worker containers, BY THE LABEL THIS SCRIPT'S DEPLOY PUT ON THEM — never by a name
  # pattern, and never `docker ps` filtered by anything broader. Ten containers on this box hold
  # real data.
  if [ -n "$APPLIANCE_PID" ] && kill -0 "$APPLIANCE_PID" 2>/dev/null; then
    "$KONTRA" serve --actor "$ACTOR_DIR" --mode docker --replicas 0 >"$LOGS/scale-down.log" 2>&1 \
      && ok "worker containers removed" \
      || info "scale-down reported an error; see $LOGS/scale-down.log"
  fi
  local left
  left=$(docker ps -aq --filter "label=kontra.actor=$ACTOR_NAME@$ACTOR_VERSION" 2>/dev/null | wc -l | tr -d ' ')
  if [ "$left" != "0" ]; then
    docker rm -f $(docker ps -aq --filter "label=kontra.actor=$ACTOR_NAME@$ACTOR_VERSION") >/dev/null 2>&1
    info "force-removed $left leftover worker container(s)"
  fi
  # The appliance last: it is what the two above were talking to.
  if [ -n "$APPLIANCE_PID" ] && kill -0 "$APPLIANCE_PID" 2>/dev/null; then
    kill -TERM "$APPLIANCE_PID" 2>/dev/null
    for _ in $(seq 1 40); do kill -0 "$APPLIANCE_PID" 2>/dev/null || break; sleep 0.5; done
    kill -0 "$APPLIANCE_PID" 2>/dev/null && kill -KILL "$APPLIANCE_PID" 2>/dev/null
    ok "appliance stopped"
  fi
  teardown_assertions
  if [ "$KEEP" = "1" ]; then
    info "--keep: the data directory is left at $GATE_TMP"
  else
    rm -rf "$GATE_TMP"
  fi
  report
  exit $rc
}

# TEARDOWN IS AN ASSERTION, not a courtesy. "teardown leaves nothing running" is an acceptance
# criterion, and a gate that tore down without checking would pass while leaking a worker
# container per run — which on a developer box is how a laptop ends up with forty of them.
teardown_assertions() {
  local containers ports_bound=0
  containers=$(docker ps -aq --filter "label=kontra.actor=$ACTOR_NAME@$ACTOR_VERSION" 2>/dev/null | wc -l | tr -d ' ')
  check "worker containers after teardown" "0" "$containers"
  for p in "$P_TEMPORAL" "$P_S3" "$P_KV" "$P_CODEC" "$P_REGISTRY" "$P_API"; do
    port_free "$p" || { ports_bound=$((ports_bound + 1)); info "still bound: $p"; }
  done
  check "appliance ports still bound after teardown" "0" "$ports_bound"
  if [ -n "$APPLIANCE_PID" ]; then
    if kill -0 "$APPLIANCE_PID" 2>/dev/null; then
      check "appliance process after teardown" "gone" "still running (pid $APPLIANCE_PID)"
    else
      ok "appliance process after teardown = gone"
    fi
  fi
}

report() {
  if [ "$REACHED_END" != "1" ]; then
    printf '\n\033[31m%s\033[0m\n' "PARITY GATE INCOMPLETE — it stopped before the end, so it proved nothing."
    printf '  %s\n' "Whatever passed above is real; everything after the last line printed was never run."
    if [ ${#FAILURES[@]} -gt 0 ]; then
      printf '  it had already failed %s:\n' "${#FAILURES[@]}"
      for f in "${FAILURES[@]}"; do printf '  · %s\n' "$f"; done
    fi
    printf '\nlogs: %s\n' "$LOGS"
  elif [ ${#FAILURES[@]} -eq 0 ]; then
    printf '\n\033[32m%s\033[0m\n' "PARITY GATE PASSES — the binary serves the local development path."
    printf '  %s\n' "worker topology proved: $WORKER_TOPOLOGY"
  else
    printf '\n\033[31m%s\033[0m\n' "PARITY GATE FAILS (${#FAILURES[@]}):"
    for f in "${FAILURES[@]}"; do printf '  · %s\n' "$f"; done
    printf '\nlogs: %s\n' "$LOGS"
  fi
}
trap cleanup EXIT INT TERM

# ═════════════════════════════════════════════════════════════════════════════════════════════
say "1 · condition 1a — THE SAME LEG WITH NO CONTROL PLANE (the SDK-side contract)"
# THE CONTROL FOR 1b, on this commit. The same test file runs against `actorkit.testing`'s stubs
# with every control-plane variable unset, so it says the actor-facing API has not moved and says
# NOTHING about the appliance. That is the point of running it here as well as later: one suite,
# twice, one control plane apart.
#
# IT USED TO BE `make test-examples-python`, which ran four real actors' suites. ADR 0038 moved
# every actor to kontra-actors, so what is comparable in BOTH halves is the fixture leg and only
# that. The comparison is narrower and it is still a comparison; the four-actor coverage lives in
# kontra-actors' CI, against its own control plane.
#
# The exit code is read directly, never from a pipeline — a pipeline reports its LAST stage's
# status, so `pytest … | tail` would print failures and exit 0.
env -u KONTRA_ADDRESS -u KONTRA_S3_ENDPOINT -u KONTRA_REDIS_HOST \
  "$PYTHON" -m pytest -q "$REPO/tests/test_fixture_actor_e2e.py" >"$LOGS/examples-stub.log" 2>&1
STUB_RC=$?
STUB_LINE=$(grep -Eo '[0-9]+ (passed|failed|error)[^,]*' "$LOGS/examples-stub.log" | tr '\n' ' ')
info "pytest said: ${STUB_LINE:-<no counts printed>}"
if [ "$STUB_RC" = "0" ]; then
  ok "the fixture leg with no control plane is green (it skipped, which is the control)"
else
  bad "the fixture leg with no control plane exited $STUB_RC — see $LOGS/examples-stub.log"
  tail -30 "$LOGS/examples-stub.log"
fi

# ═════════════════════════════════════════════════════════════════════════════════════════════
say "2 · the appliance, on its own everything"
"$KONTRA" up \
  --data-dir "$DATA_DIR" \
  --bind "$BIND" \
  --temporal-port "$P_TEMPORAL" \
  --metrics-port "$P_METRICS" \
  --s3-port "$P_S3" \
  --kv-port "$P_KV" \
  --codec-port "$P_CODEC" \
  --registry-port "$P_REGISTRY" \
  --api-port "$P_API" \
  --orchestrator "${KONTRA_GATE_ORCHESTRATOR:-auto}" \
  >"$LOGS/appliance.log" 2>&1 &
APPLIANCE_PID=$!
info "kontra up pid $APPLIANCE_PID → $LOGS/appliance.log"

# READY IS "THE API ANSWERS", not "the process is alive". Everything after this dispatches work,
# and a control plane that has bound its ports but has not finished hydrating its child would fail
# the next step with a connection error that reads like a bug in the next step.
UP=0
for _ in $(seq 1 240); do
  if ! kill -0 "$APPLIANCE_PID" 2>/dev/null; then break; fi
  if curl -fsS --max-time 2 "http://$BIND:$P_API/api/health" >/dev/null 2>&1; then UP=1; break; fi
  sleep 1
done
if [ "$UP" != "1" ]; then
  bad "the appliance never answered on http://$BIND:$P_API/api/health"
  tail -40 "$LOGS/appliance.log"
  exit 1
fi
ok "control plane answering at http://$BIND:$P_API"
if [ -f "$DATA_DIR/endpoints.json" ]; then
  ok "endpoints published: $(tr -d '\n ' < "$DATA_DIR/endpoints.json")"
else
  bad "no $DATA_DIR/endpoints.json — deploy and scale cannot find this appliance"
fi

# EVERY LISTENER HONOURS `--bind`, ASSERTED RATHER THAN ASSUMED. ADR 0031 §3's whole security
# argument is that there is no remote caller, and a listener on 0.0.0.0 is a remote caller's
# address on a box with a public interface. The API was the one surface that ignored the flag
# (`server.ts` hard-coded `0.0.0.0`, a habit from when it was a container behind a `ports:` entry),
# and the gate would otherwise declare compose retired in favour of something more exposed than
# what it replaces.
WIDE=$(ss -ltnH 2>/dev/null | awk -v ports="$P_TEMPORAL $P_S3 $P_KV $P_CODEC $P_REGISTRY $P_API $P_METRICS" '
  { split($4, a, ":"); p = a[length(a)]; addr = substr($4, 1, length($4) - length(p) - 1)
    if (index(" " ports " ", " " p " ") && (addr == "0.0.0.0" || addr == "*" || addr == "[::]")) print addr ":" p }')
if [ -z "$WIDE" ]; then
  ok "every appliance listener is on $BIND — none on 0.0.0.0"
else
  bad "an appliance listener ignored --bind and is on every interface: $(echo "$WIDE" | tr '\n' ' ')"
fi

# ═════════════════════════════════════════════════════════════════════════════════════════════
say "3 · condition 2 — the local actor path"

info "kontra deploy --actor $ACTOR_DIR"
if ! "$KONTRA" deploy --actor "$ACTOR_DIR" >"$LOGS/deploy.log" 2>&1; then
  bad "kontra deploy failed — see $LOGS/deploy.log"
  tail -30 "$LOGS/deploy.log"
  exit 1
fi
# THE DIGEST IS THE RECEIPT. A push that answered without writing anything is the failure mode a
# tag cannot see (ADR 0032), so the gate asks the registry rather than trusting the summary.
DIGEST=$(grep -Eo 'sha256:[0-9a-f]{64}' "$LOGS/deploy.log" | head -1)
if [ -n "$DIGEST" ]; then ok "pushed $ACTOR_NAME:$ACTOR_VERSION → $DIGEST"; else bad "deploy printed no manifest digest"; fi
# ASKED AT THE ADDRESS THE APPLIANCE PUBLISHED, which is the same one `kontra deploy` just
# resolved — asking anywhere else would be a different registry's answer.
REG_ADDR=$("$PYTHON" -c 'import json,sys; print(json.load(open(sys.argv[1]))["registry"])' "$DATA_DIR/endpoints.json" 2>/dev/null || echo "$BIND:$P_REGISTRY")
TAGS=$(curl -fsS --max-time 5 "http://$REG_ADDR/v2/$ACTOR_NAME/tags/list" 2>/dev/null)
case "$TAGS" in
  *"\"$ACTOR_VERSION\""*) ok "the appliance's own registry at $REG_ADDR holds $ACTOR_NAME:$ACTOR_VERSION" ;;
  *) bad "the registry at $REG_ADDR does not list $ACTOR_VERSION (answered: ${TAGS:-nothing})" ;;
esac

# ── CAN A CONTAINER REACH THIS APPLIANCE AT ALL? ASKED BEFORE ANY WORKER IS STARTED ─────────
#
# THE ANSWER IS NOT ALWAYS YES, AND WHEN IT IS NO NOTHING SAYS SO. MEASURED on this repo's own
# development box: ufw active, `Default: deny (incoming)`. A container's packets to the bridge
# GATEWAY traverse the host's INPUT chain — unlike a published port, whose packets go through
# FORWARD and Docker's DOCKER-USER chain, which is the case everyone knows about — so every
# connection from a worker to `172.17.0.1:<appliance port>` timed out. What that looked like:
# `kontra deploy` fine, `kontra serve --mode docker` fine, two containers Up, BOTH processes
# alive inside them, `/tmp/host.log` and `/tmp/handler.log` zero bytes, no actor in the catalog,
# no poller on the queue, and `docker ps` reporting two healthy replicas. Port 22 was reachable
# from the same container, which is what pins the cause on the firewall rather than on docker.
#
# So it is a probe, not a hope, and it runs in a CONTAINER because that is the only thing that
# answers the question a container is about to ask. The Python host is the base image `kontra
# deploy` has just guaranteed exists, and it carries a python3.
#
# RESOLVED THE SAME WAY `cli/deploy.go:hostImage()` RESOLVES IT, not hardcoded. This said
# `kontra-host:1`, which was the name `deploy` looked for as long as nothing else said otherwise —
# and `docker-compose.yml` now sets `KONTRA_HOST_IMAGE=ghcr.io/medmahmoudi26/kontra-host:dev`, so the
# image `deploy` guarantees and the image this line ran were two different names. Docker resolves by
# name, so the guarantee stopped covering the probe, and the failure would have read as "a container
# cannot reach Temporal" — a firewall verdict — when the truth was a missing image.
HOST_IMAGE="${KONTRA_HOST_IMAGE:-kontra-host:1}"
info "can a container reach $BIND:$P_TEMPORAL?"
if docker run --rm --network bridge "$HOST_IMAGE" python3 -c "
import socket, sys
s = socket.socket(); s.settimeout(5)
try:
    s.connect(('$BIND', $P_TEMPORAL))
except Exception as e:
    print(e); sys.exit(1)
" >"$LOGS/reach.log" 2>&1; then
  ok "a bridge container reaches the appliance — the documented topology"
else
  WORKER_NETWORK="host"
  bad_note=$(cat "$LOGS/reach.log")
  info ""
  info "!! A CONTAINER ON THE DOCKER BRIDGE CANNOT REACH THIS APPLIANCE ($bad_note)."
  info "   This host's firewall drops it: a container's packets to the bridge gateway go through"
  info "   the INPUT chain, not FORWARD/DOCKER-USER, so a default 'deny (incoming)' blocks them."
  info "   Nothing in kontra reports this — the worker container comes up and polls nothing."
  info ""
  info "   Two remedies, and this gate takes the second because it changes no system state:"
  info "     1. allow it:  ufw allow in on docker0 to $BIND port $P_TEMPORAL proto tcp   (and each other port)"
  info "     2. --network host for the workers, which is what follows. It costs the network"
  info "        namespace, so it is a development answer and not a default."
  info ""
  info "   THE BRIDGE TOPOLOGY IS THEREFORE NOT PROVEN BY THIS RUN. Everything below is."
fi

info "kontra serve --mode docker --replicas $REPLICAS${WORKER_NETWORK:+ --network $WORKER_NETWORK}"
if ! "$KONTRA" serve --actor "$ACTOR_DIR" --mode docker --replicas "$REPLICAS" \
      ${WORKER_NETWORK:+--network "$WORKER_NETWORK"} >"$LOGS/scale.log" 2>&1; then
  bad "kontra serve --mode docker failed — see $LOGS/scale.log"
  tail -30 "$LOGS/scale.log"
  exit 1
fi
cat "$LOGS/scale.log" | sed 's/^/     /'
RUNNING=$(docker ps -q --filter "label=kontra.actor=$ACTOR_NAME@$ACTOR_VERSION" | wc -l | tr -d ' ')
check "worker containers running" "$REPLICAS" "$RUNNING"
if [ -n "$WORKER_NETWORK" ]; then
  WORKER_TOPOLOGY="worker containers on --network $WORKER_NETWORK (the bridge path is blocked on this host; see step 3)"
fi

# A CONTAINER THAT IS UP IS NOT A WORKER THAT IS POLLING, and the difference is the whole reason
# this block exists. `docker ps` shows a healthy container for one pointed at a control plane that
# does not exist; only Temporal knows whether anything is polling the queue.
QUEUE="$ACTOR_NAME-$ACTOR_VERSION"
if wait_for_pollers "$QUEUE" 90 "$LOGS/workers.log"; then
  ok "$QUEUE has live pollers: $POLLERS_ROW"
else
  bad "no worker is polling $QUEUE — the containers are up and nothing is listening, which is the failure this gate exists to catch"
  sed 's/^/     /' "$LOGS/workers.log"
  for c in $(docker ps -q --filter "label=kontra.actor=$ACTOR_NAME@$ACTOR_VERSION"); do
    info "--- docker logs $c ---"
    docker logs --tail 25 "$c" 2>&1 | sed 's/^/     /'
  done
fi

info "kontra workflow serve $WORKFLOW_DIR"
"$KONTRA" workflow serve "$WORKFLOW_DIR" >"$LOGS/workflow-worker.log" 2>&1 &
WF_PID=$!
# WAITED FOR, NOT SLEPT THROUGH. `kontra workflow start` REFUSES a folder whose derived queue has
# no pollers — deliberately, because a start against an unserved queue sits `running` forever with
# no error. A fixed sleep that is a second short turns that correct refusal into a gate failure
# about the wrong thing, so the gate waits for the worker's own "serving" line.
WF_UP=0
for _ in $(seq 1 90); do
  kill -0 "$WF_PID" 2>/dev/null || break
  grep -q "\[wfhost\] ParityGate" "$LOGS/workflow-worker.log" 2>/dev/null && { WF_UP=1; break; }
  sleep 1
done
if [ "$WF_UP" != "1" ]; then
  bad "the workflow worker never reported serving ParityGate — see $LOGS/workflow-worker.log"
  tail -30 "$LOGS/workflow-worker.log"
  exit 1
fi
ok "workflow worker up (pid $WF_PID): $(grep -m1 '\[wfhost\]' "$LOGS/workflow-worker.log")"

# A NAME PER RUN. The gate asserts an exact row count, and a name reused across runs would make
# the second run's count the sum of two — which passes `>= units` and hides a leg that produced
# nothing this time.
DATASET="paritygate_$(date +%s)"
info "kontra workflow start $WORKFLOW_DIR --wait --input {\"units\": $UNITS}"
"$KONTRA" workflow start "$WORKFLOW_DIR" --wait --timeout 15m \
  --input "{\"units\": $UNITS, \"dataset\": \"$DATASET\"}" >"$LOGS/run.log" 2>&1
RUN_RC=$?
cat "$LOGS/run.log" | sed 's/^/     /'
if [ "$RUN_RC" != "0" ]; then
  bad "the run did not complete (exit $RUN_RC) — see $LOGS/run.log"
fi

# ── THE COUNTS. This is the gate ────────────────────────────────────────────────────────────
#
# The workflow's return value is JSON on stdout after the two `started …` lines. Everything below
# reads integers out of it; nothing below looks at a status word.
RESULT_JSON=$("$PYTHON" - "$LOGS/run.log" <<'PY'
import json, sys
raw = open(sys.argv[1]).read()
start = raw.find('{')
while start != -1:
    try:
        obj = json.JSONDecoder().raw_decode(raw[start:])[0]
    except ValueError:
        start = raw.find('{', start + 1)
        continue
    if isinstance(obj, dict) and 'echo' in obj:
        print(json.dumps(obj))
        break
    start = raw.find('{', start + 1)
PY
)
if [ -z "$RESULT_JSON" ]; then
  bad "the run returned no counts — a run that completes and hands back nothing is exactly the shape of failure this gate exists to catch"
else
  info "counts: $RESULT_JSON"
  get() { printf '%s' "$RESULT_JSON" | "$PYTHON" -c 'import json,sys; d=json.load(sys.stdin); print(d[sys.argv[1]][sys.argv[2]])' "$1" "$2" 2>/dev/null || echo "?"; }

  # 1. THE STRAIGHT LINE. Units in must equal records out, and nothing may be dropped.
  check "echo   · units"   "$UNITS" "$(get echo units)"
  check "echo   · out"     "$UNITS" "$(get echo out)"
  check "echo   · dropped" "0"      "$(get echo dropped)"

  # 2. THE CHAIN — Method → Method by ref. The leg that isolated every Unit on a fleet while being
  #    invisible locally: it fails here as `dropped == units` with every other leg green.
  check "chain  · units"   "$UNITS" "$(get chain units)"
  check "chain  · out"     "$UNITS" "$(get chain out)"
  check "chain  · dropped" "0"      "$(get chain dropped)"

  # 3. EVERY UNIT FAILS. The call still returns — nothing fails a Batch on the framework's
  #    judgement (ADR 0023 §14) — so the ONLY signal is the drop count.
  check "boom   · out"     "0"      "$(get boom out)"
  check "boom   · dropped" "$UNITS" "$(get boom dropped)"

  # 4. THE CONTROL. Same empty output, same clean status, zero drops.
  check "silent · out"     "0"      "$(get silent out)"
  check "silent · dropped" "0"      "$(get silent dropped)"

  # 5. AND THE TWO MUST NOT LOOK THE SAME. Stated as its own assertion rather than inferred from
  #    the four above, because this is the acceptance criterion in its own words: a run whose
  #    Units all failed has to be distinguishable from one that legitimately found nothing.
  if [ "$(get boom dropped)" = "$(get silent dropped)" ]; then
    bad "a run that dropped every Unit is INDISTINGUISHABLE from one that found nothing (both dropped=$(get boom dropped))"
  else
    ok "lost-everything ($(get boom dropped) dropped) is distinguishable from found-nothing ($(get silent dropped) dropped)"
  fi
fi

# ── THE DATASET. Written, listed, and queried ───────────────────────────────────────────────
info "kontra dataset list"
"$KONTRA" dataset list >"$LOGS/datasets.log" 2>&1
if grep -q "$DATASET" "$LOGS/datasets.log"; then
  ok "Dataset $DATASET is listed"
else
  bad "Dataset $DATASET is not in \`kontra dataset list\` — see $LOGS/datasets.log"
  tail -20 "$LOGS/datasets.log"
fi
# QUERIED, NOT JUST LISTED, AND COUNTED RATHER THAN EYEBALLED. A named row in a catalog with no
# rows behind it is the empty-Dataset failure wearing a name; `LIMIT 100` would have looked fine
# for the seven-minute run that lost 15,814 targets.
# EXPORTED AS DATA AND PARSED, NOT SCRAPED OFF THE RENDERED TABLE. The first version took the last
# integer in the output — which is the `1` in `1 row · 13ms` — so a Dataset holding exactly 12
# rows measured as 1 and the gate failed on its own arithmetic. An assertion that is wrong in the
# lenient direction is worse than no assertion; `--export` writes the answer as JSON.
#
# ONE QUERY, THREE AGGREGATES. Two queries against a Dataset a run may still be writing is the
# divide-two-counts trap: one scan cannot disagree with itself.
"$KONTRA" dataset query "$DATASET" \
  --sql "SELECT count(*) AS rows_out, count(DISTINCT id) AS ids, count(DISTINCT worker) AS workers FROM \"$DATASET\"" \
  --export "$LOGS/rowcount.json" >"$LOGS/rowcount.log" 2>&1
COUNTS=$("$PYTHON" "$REPO/scripts/lib/one-row.py" "$LOGS/rowcount.json" rows_out ids workers 2>/dev/null || echo "? ? ?")
read -r ROWS IDS WORKERS_SEEN <<<"$COUNTS"
check "rows queryable in Dataset $DATASET" "$UNITS" "${ROWS:-?}"
# AND THE ROWS ARE THE ROWS THAT WENT IN. A count alone would pass on a Dataset full of the wrong
# actor's output, or one where every record is the same Unit written N times.
check "distinct Unit ids in the Dataset" "$UNITS" "${IDS:-?}"
if [ "${ROWS:-?}" != "$UNITS" ] || [ "${IDS:-?}" != "$UNITS" ]; then
  sed 's/^/     /' "$LOGS/rowcount.log"
  head -c 500 "$LOGS/rowcount.json" 2>/dev/null | sed 's/^/     /'
fi
# WHICH WORKER PRODUCED THE ROWS — a fact, not a fan-out assertion.
#
# ONE BATCH IS ONE DISPATCH, so one of the replicas takes it and `workers` is 1 even with two of
# them up. Asserting 2 here would be asserting a distribution kontra does not promise. What IS
# worth checking is that the column is populated at all: an empty producer means the ref's meta
# did not survive the round trip, which is the same missing-metadata class as a lost drop count.
case "${WORKERS_SEEN:-?}" in
  ""|"?"|0) bad "the Dataset records no producing worker — the ref's meta did not survive; see $LOGS/rowcount.json" ;;
  *) info "produced by $WORKERS_SEEN distinct worker(s) out of $REPLICAS replica(s) — one Batch is one dispatch, so 1 is the expected answer" ;;
esac

# ═════════════════════════════════════════════════════════════════════════════════════════════
say "4 · condition 1b — THE E2E LEG AGAINST THE BINARY"
# The e2e leg dispatches to a real Worker, so one has to exist. `beacon` is served in LOCAL mode —
# the actor process and the Go handler on this box — deliberately, and not as a shortcut: the
# image path is already proven by step 3, and this step is about whether the SDK's own contract
# holds against the appliance rather than about how the Worker was placed.
info "kontra serve --actor testdata/fixtureactor (local mode, for the e2e leg)"
"$KONTRA" serve --actor testdata/fixtureactor >"$LOGS/beacon.log" 2>&1 &
BEACON_PID=$!
BEACON_QUEUE="fixtureactor-0.1.0"
if wait_for_pollers "$BEACON_QUEUE" 120 "$LOGS/workers-beacon.log"; then
  ok "beacon is polling $BEACON_QUEUE: $POLLERS_ROW"
else
  bad "beacon never started polling $BEACON_QUEUE — the e2e leg below has nothing to dispatch to"
  sed 's/^/     /' "$LOGS/workers-beacon.log"
  tail -25 "$LOGS/beacon.log"
fi

# The same suite, on the same commit, with a control plane under it. `KONTRA_E2E=1` is what turns
# the `e2e`-marked tests on; they skip themselves when no KONTRA_ADDRESS is set, so run 1a above
# and this run differ in exactly one thing.
E2E_RECEIPT="$LOGS/e2e-receipt"
: >"$E2E_RECEIPT"
KONTRA_E2E_RECEIPT="$E2E_RECEIPT" KONTRA_E2E=1 "$PYTHON" -m pytest -q \
  "$REPO/tests/test_fixture_actor_e2e.py" >"$LOGS/examples-binary.log" 2>&1
E2E_RC=$?
E2E_LINE=$(grep -Eo '[0-9]+ (passed|failed|error|skipped)[^,]*' "$LOGS/examples-binary.log" | tr '\n' ' ')
info "pytest said: ${E2E_LINE:-<no counts printed>}"
# A SUITE THAT SKIPPED EVERY E2E TEST IS NOT A PASS. That is the exact shape of ADR 0031 §5's
# warning — the examples suite would go green against a binary that never started — so the gate
# refuses a run in which nothing e2e actually executed.
#
# COUNTED FROM A RECEIPT FILE, NOT FROM THE LOG. The first version grepped the run output for a
# line the test prints; pytest CAPTURES stdout and replays it only for failures, so a test that
# PASSED left no line and the gate reported "they all skipped" about the leg that had just proven
# the binary. The test appends to $KONTRA_E2E_RECEIPT instead — a side effect pytest does not own.
E2E_RAN=$(wc -l <"$E2E_RECEIPT" 2>/dev/null | tr -d ' ')
E2E_RAN=${E2E_RAN:-0}
if [ "$E2E_RC" != "0" ]; then
  bad "the e2e leg exited $E2E_RC — see $LOGS/examples-binary.log"
  tail -40 "$LOGS/examples-binary.log"
elif [ "$E2E_RAN" = "0" ]; then
  bad "the e2e leg was green but ran nothing against the binary (it skipped) — see $LOGS/examples-binary.log"
else
  ok "the e2e leg is green, with $E2E_RAN dispatch(es) dialling the appliance:"
  sed 's/^/        /' "$E2E_RECEIPT"
fi

# THE LAST LINE, and the only place this is set. See REACHED_END at the top.
REACHED_END=1
exit 0

# ── What this does NOT compare, said here rather than left to be assumed ─────────────────────
#
# Issue 18 asks for `make test-examples` "against the binary, in CI, on the same commit that runs
# it against compose — so parity is a comparison rather than an assertion". THE COMPOSE HALF OF
# THAT COMPARISON NO LONGER EXISTS. By the time this gate was written, docker-compose.yml had lost
# Temporal, the object store, the KV, the codec, the registry and orchestrator-api to ADR 0031's
# earlier slices; what it still defines is `orchestrator-infra` and `orchestrator-probe`, neither
# of which is a control plane. There is nothing left to run the suite against, which is not a gap
# in this script — it is the migration having finished. The comparison that IS runnable is the one
# above: one suite, one commit, with and without the appliance under it.
