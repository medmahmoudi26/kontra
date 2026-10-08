#!/usr/bin/env bash
# parity-gate.sh — does the LOCAL DEVELOPMENT PATH work end to end on the compose install?
#
# Two conditions, and the second one carries the weight:
#
#   1. an actor's own suite green against the control plane
#   2. the local actor path end to end — deploy → serve → dispatch → a Dataset that queries
#
# Condition 1 is not, by itself, evidence of anything: an actor's suite runs in-process against
# `actorkit.testing` stubs and passes on a bare runner with no control plane at all, so IT WOULD GO
# GREEN AGAINST A CONTROL PLANE THAT NEVER STARTED. This script runs it BOTH ways on one commit —
# once with every control-plane variable unset, and once with `KONTRA_ADDRESS` pointing at the
# stack, which turns the suite's `e2e`-marked tests on. Two runs of one suite is the comparison.
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
# ── ISOLATION, WHICH IS STRUCTURAL WHERE IT CAN BE AND REFUSED WHERE IT CANNOT ───────────────
#
# On 2026-08-26 an agent satisfying a "complete a run" criterion ran a workflow against the live
# stack with three isolated QUEUE NAMES. One module pinned a queue constant, the isolation did not
# hold, and the run deleted 223,378 rows of run data
# (docs/incidents/2026-08-26-sweep-deleted-run-data.md). Names are not isolation.
#
# THREE OF THE FOUR AXES ARE STRUCTURAL HERE. A unique COMPOSE PROJECT gives this gate its own
# containers and its own volumes — every volume in docker-compose.yml is declared unnamed, so
# compose prefixes each with the project. Its install directory and workspaces directory are fresh
# `mktemp -d`s. Every published port is chosen outside the defaults and PROVEN FREE before compose
# is invoked, so a collision refuses to start rather than quietly joining something already there.
#
# THE FOURTH AXIS CANNOT BE ISOLATED AND IS THEREFORE REFUSED. `docker-compose.yml:1227-1229`
# declares `networks: default: name: kontra` — a FIXED name, so `-p` does not scope it and two
# projects would share one network. That is deliberate in the compose file (a worker container
# started by `kontra serve --mode docker` attaches to `kontra` by name), and it means this gate
# must not run beside a live stack. So it asks first: if anything is already attached to the
# `kontra` network, it exits 2 and says so, rather than starting a second control plane into it.
#
# ── WHERE THE COMMANDS RUN, WHICH IS THE WHOLE DIFFERENCE FROM THE OLD GATE ──────────────────
#
# Inside the `cli` container, through `docker compose exec -T cli`. That is how the documented
# install is driven and how `.github/workflows/ci.yml`'s "the local docker install" job drives it,
# and it removes a class of failure the previous gate spent a hundred lines on: a CLI on the host
# had to reach a control plane a worker CONTAINER could also reach, which meant binding the docker
# bridge gateway, probing whether the host firewall dropped container→gateway packets, and falling
# back to `--network host` when it did. From inside the network there is one set of addresses —
# compose DNS — and `tests/parity/{actor,workflow}` resolve at the SAME paths in the container as
# out of it, because the compose file mounts the repo read-only at its own path.
#
# It never runs `sweepDatasetsWorkflow`, and it removes exactly the containers it created.
#
# ── THE IMAGES ARE AN INPUT, NOT SOMETHING THIS SCRIPT BUILDS ────────────────────────────────
#
# Building them here would put a ten-minute `docker build` inside a gate, and the thing under test
# is the images built from THIS commit — which CI builds in the same job and `make image` builds
# locally. So the gate REFUSES when they are missing, naming them, rather than pulling `:dev` from
# ghcr and silently gating last week's release.
#
# ── USAGE ────────────────────────────────────────────────────────────────────────────────────
#
#   scripts/parity-gate.sh                 # the whole gate
#   scripts/parity-gate.sh --keep          # leave the stack up afterwards, to poke at it
#   KONTRA_GATE_PORT_BASE=41000 …          # move every published port at once
#   KONTRA_GATE_UNITS=24 …                 # a wider Batch
#   KONTRA_IMAGE=… KONTRA_ORCHESTRATOR_IMAGE=… …   # name the images under test
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
    -h|--help) sed -n '2,80p' "${BASH_SOURCE[0]}"; exit 0 ;;
    *) echo "unknown argument: $arg (try --help)" >&2; exit 2 ;;
  esac
done

UNITS="${KONTRA_GATE_UNITS:-12}"
REPLICAS="${KONTRA_GATE_REPLICAS:-2}"
ACTOR_NAME="paritygate"
ACTOR_VERSION="0.1.0"

GATE_TMP="$(mktemp -d "${TMPDIR:-/tmp}/kontra-parity-XXXXXX")"
INSTALL_DIR="$GATE_TMP/install"
# INSIDE THE INSTALL DIRECTORY, WHICH IS WHAT MAKES IT AN INSTALL. `assert-install-is-self-contained`
# allows a `workspaces/` mount only when it is compose's own default — a directory the install
# CREATED, under the install — and refuses a host path from anywhere else. A workspaces dir beside
# the install is the shape of a clone, which is the thing that check exists to stop this becoming.
WORKSPACES="$INSTALL_DIR/workspaces"
LOGS="$GATE_TMP/logs"
mkdir -p "$INSTALL_DIR" "$WORKSPACES" "$LOGS"

# THE FIXTURES LIVE IN THE WORKSPACE, WHERE AN OPERATOR'S CODE LIVES. The repo is NOT mounted into
# the `cli` container: `docker-compose.yml` mounts `${KONTRA_REPO:-${PWD}}` read-only, and `${PWD}`
# is the install directory because `dc` runs from there — so the container sees the install, not the
# checkout. Copying is therefore not a convenience, it is the only way these are visible at all, and
# it is also what a user does.
ACTOR_DIR="$WORKSPACES/parity/actors/paritygate"
WORKFLOW_DIR="$WORKSPACES/parity/workflows/paritygate"
mkdir -p "$(dirname "$ACTOR_DIR")" "$(dirname "$WORKFLOW_DIR")"
cp -a "$REPO/tests/parity/actor/." "$ACTOR_DIR/"
cp -a "$REPO/tests/parity/workflow/." "$WORKFLOW_DIR/"

# THE PROJECT NAME IS THE ISOLATION, so it carries the pid and a timestamp rather than being a
# constant somebody could run twice. Lower-case and dash-only: compose rejects anything else.
PROJECT="kontra-parity-$$-$(date +%s)"

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
COMPOSE_UP=0

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

# ── THE PORTS. All non-default, all proven free before compose binds anything ────────────────
#
# THE DEFAULTS ARE THE LIVE STACK'S and are refused by name below, not merely avoided by choosing
# other numbers — a base that happened to land on one would otherwise make this script the thing
# the incident report describes.
BASE="${KONTRA_GATE_PORT_BASE:-39100}"
P_TEMPORAL=$((BASE + 1))
P_S3=$((BASE + 2))
P_KV=$((BASE + 3))
P_API=$((BASE + 6))
P_POSTGRES=$((BASE + 8))

# THE REGISTRY IS THE ONE PORT THAT CANNOT MOVE, and that is a property of image references rather
# than an omission here. An actor image reference is resolved by TWO different things that must
# agree on the string: the `cli` container (which reads manifests over the compose network) and the
# HOST's Docker daemon, which `pack` drives through the mounted socket and which is what pulls the
# run image. Shifting the published port to 39105 moved the host's half only — `KONTRA_REGISTRY`
# stayed `127.0.0.1:5000` — and the gate failed with
#
#   invalid run-image '127.0.0.1:5000/kontra-runtimes/python:1@sha256:712b8d2e…':
#   Error response from daemon: Get "http://127.0.0.1:5000/v2/": dial tcp: connection refused
#
# Moving `KONTRA_REGISTRY` to match would break the other half instead: `registryProbeBases`'s
# in-network fallback is `registry:<that port>`, and zot listens on 5000 inside the network whatever
# the host publishes. So the registry keeps the default, and the isolation this gate relies on is
# the `kontra`-network refusal above — which already means no second install is running.
P_REGISTRY=5000

# The numbers a kontra installation uses when nobody says otherwise. Reaching any of them means
# this script is about to talk to something it did not start.
DEFAULT_PORTS=(7233 8233 8333 8088 8090 6379 5000 18234 9333 8888 8085 5432 55432)

# port_free — asked WITHOUT A PIPE, and that is not a style choice.
#
# `set -o pipefail` at the top of this file plus `cmd | grep -q pattern` is a race, and it bit
# here: `grep -q` exits the instant it matches, the writer gets SIGPIPE, and pipefail makes the
# PIPELINE fail even though the match succeeded. Capture first, test the string second.
port_free() {
  # A listener on 0.0.0.0 and one on 127.0.0.1 both take the port from us, and the difference does
  # not matter to a bind that is about to fail — so this asks about the port over any address.
  local listeners
  listeners=$(ss -ltnH "sport = :$1" 2>/dev/null)
  [ -z "$listeners" ]
}

# ── THE IMAGES UNDER TEST ────────────────────────────────────────────────────────────────────
#
# `:ci` rather than `:dev`, because `:dev` is what ghcr serves and `pull_policy: never` below is
# what stops compose fetching it. A gate that pulled would be gating a published release.
export KONTRA_IMAGE="${KONTRA_IMAGE:-kontra:ci}"
export KONTRA_ORCHESTRATOR_IMAGE="${KONTRA_ORCHESTRATOR_IMAGE:-kontra-orchestrator:ci}"
export KONTRA_LOGSHIP_IMAGE="${KONTRA_LOGSHIP_IMAGE:-kontra-logship:ci}"
export KONTRA_PORTER_IMAGE="${KONTRA_PORTER_IMAGE:-kontra-porter:ci}"
export KONTRA_PULL_POLICY=never

say "0 · isolation"
info "project       $PROJECT"
info "install dir   $INSTALL_DIR"
info "workspaces    $WORKSPACES"
info "ports         temporal=$P_TEMPORAL s3=$P_S3 kv=$P_KV registry=$P_REGISTRY api=$P_API postgres=$P_POSTGRES"

# NOTHING MAY ALREADY BE ON THE `kontra` NETWORK. See the header: the network name is fixed by the
# compose file, so this is the one axis `-p` cannot scope, and joining a live stack is the
# incident. Asked of docker rather than assumed from a pid file.
# ABSENT IS NOT "UNKNOWN", AND THE DIFFERENCE IS THE WHOLE CHECK. `docker network inspect` EXITS
# NON-ZERO on a network that does not exist — which is the normal state on a fresh runner, the one
# machine this gate is meant to run on. `… || echo 0` did not save it: the failing command still
# wrote a newline to stdout, so the substitution captured "\n0", which is not the string "0", and
# the gate refused to start because ZERO containers were attached. Measured in CI:
#     REFUSING: \n0 container(s) are already attached to the `kontra` network
#     Error response from daemon: network kontra not found
# Existence is therefore asked FIRST, as its own question, and the count is only read when there is
# something to count — and then stripped to digits so no stray whitespace can make it a non-number.
if docker network inspect kontra >/dev/null 2>&1; then
  ATTACHED=$(docker network inspect kontra --format '{{ len .Containers }}' 2>/dev/null | tr -dc '0-9')
  ATTACHED=${ATTACHED:-0}
else
  ATTACHED=0
fi
if [ "$ATTACHED" != "0" ]; then
  echo "REFUSING: $ATTACHED container(s) are already attached to the \`kontra\` network, so" >&2
  echo "  something is running there and this gate will not start a second control plane into" >&2
  echo "  it. docker-compose.yml fixes the network name, so a compose project cannot scope it." >&2
  docker network inspect kontra --format '{{ range .Containers }}  {{ .Name }}{{ "\n" }}{{ end }}' >&2
  echo "  Stop it first:  docker compose down" >&2
  exit 2
fi
ok "nothing is attached to the \`kontra\` network"

for p in "$P_TEMPORAL" "$P_S3" "$P_KV" "$P_REGISTRY" "$P_API" "$P_POSTGRES"; do
  # The registry is EXEMPT from the default-port refusal and from nothing else: it has to be 5000
  # (see above) and it still has to be free, which is the check that actually protects a live stack.
  if [ "$p" != "$P_REGISTRY" ]; then
    for d in "${DEFAULT_PORTS[@]}"; do
      if [ "$p" = "$d" ]; then
        echo "REFUSING: port $p is a kontra default. Move KONTRA_GATE_PORT_BASE." >&2
        exit 2
      fi
    done
  fi
  if ! port_free "$p"; then
    echo "REFUSING: port $p is already bound. Something is listening there and this gate will not" >&2
    echo "  join it. Move KONTRA_GATE_PORT_BASE to a free block." >&2
    ss -ltnp "sport = :$p" >&2
    exit 2
  fi
done
ok "every port is free and none is a kontra default"

# THE LIST IS DERIVED FROM COMPOSE, NOT RESTATED. `.github/workflows/ci.yml` pays for this lesson
# in its own install job: `KONTRA_PORTER_IMAGE` was missing from a hand-written list, so porter
# alone fell back to a `:dev` tag that has never been published and the `up` failed with no error
# until the very last step. Every `${KONTRA_*_IMAGE:-…}` in the compose file has to be a variable
# this script exported, and a new one fails HERE, by name.
UNSET_IMAGE_VARS=()
while read -r var; do
  [ -n "${!var:-}" ] || UNSET_IMAGE_VARS+=("$var")
done < <(grep -oE '\$\{KONTRA_[A-Z_]*IMAGE:-' "$REPO/docker-compose.yml" | sed 's/^\${//;s/:-$//' | sort -u)
if [ ${#UNSET_IMAGE_VARS[@]} -gt 0 ]; then
  echo "REFUSING: docker-compose.yml names image override(s) this gate does not set:" >&2
  for v in "${UNSET_IMAGE_VARS[@]}"; do echo "    $v" >&2; done
  echo "  Each one needs a line in the exports above and a build in" >&2
  echo "  scripts/build-cluster-images.sh, or the service falls back to a published :dev tag." >&2
  exit 2
fi

# EVERY IMAGE, NAMED, BEFORE COMPOSE IS ASKED FOR ANY OF THEM. `pull_policy: never` turns a
# missing image into `No such image` from the daemon partway through an `up`, which reads like a
# registry problem. This reads like what it is.
MISSING=()
for img in "$KONTRA_IMAGE" "$KONTRA_ORCHESTRATOR_IMAGE" \
           "$KONTRA_LOGSHIP_IMAGE" "$KONTRA_PORTER_IMAGE"; do
  docker image inspect "$img" >/dev/null 2>&1 || MISSING+=("$img")
done
if [ ${#MISSING[@]} -gt 0 ]; then
  echo "REFUSING: this gate does not build images, and these are not on this machine:" >&2
  for m in "${MISSING[@]}"; do echo "    $m" >&2; done
  echo "  Build them from this commit first — \`make image\` locally, or the build step CI runs" >&2
  echo "  before this one — or name others with KONTRA_IMAGE / KONTRA_ORCHESTRATOR_IMAGE / …" >&2
  exit 2
fi
ok "every image under test is present locally"

PYTHON="${PYTHON:-$REPO/.venv/bin/python}"
[ -x "$PYTHON" ] || PYTHON="$(command -v python3 || command -v python)"

# ── THE INSTALL DIRECTORY, HOLDING ONLY WHAT A CURL WOULD PUT THERE ─────────────────────────
#
# The same two files `.github/workflows/ci.yml`'s install job copies, and then the same assertion
# over the result: `scripts/assert-install-is-self-contained.py` is what catches a compose file
# that has grown a bind mount back, which would make the documented install a clone again.
cp "$REPO/docker-compose.quickstart.yml" "$INSTALL_DIR/docker-compose.yml"
cp "$REPO/.env.quickstart" "$INSTALL_DIR/.env"
{
  echo ""
  echo "# ── appended by scripts/parity-gate.sh ──"
  echo "KONTRA_BIND=127.0.0.1"
  echo "KONTRA_API_PORT=$P_API"
  echo "KONTRA_TEMPORAL_PORT=$P_TEMPORAL"
  echo "KONTRA_S3_PORT=$P_S3"
  echo "KONTRA_KV_PORT=$P_KV"
  echo "KONTRA_REGISTRY_PORT=$P_REGISTRY"
  echo "KONTRA_POSTGRES_PORT=$P_POSTGRES"
  echo "KONTRA_IMAGE=$KONTRA_IMAGE"
  echo "KONTRA_ORCHESTRATOR_IMAGE=$KONTRA_ORCHESTRATOR_IMAGE"
  echo "KONTRA_LOGSHIP_IMAGE=$KONTRA_LOGSHIP_IMAGE"
  echo "KONTRA_PORTER_IMAGE=$KONTRA_PORTER_IMAGE"
  echo "KONTRA_PULL_POLICY=never"
  # KONTRA_REPO IS DELIBERATELY NOT SET. docker-compose.yml mounts `${KONTRA_REPO:-${PWD}}` into the
  # `cli` container read-only; naming the checkout here mounts a host path from outside the install,
  # which `assert-install-is-self-contained.py` refuses by design — "an install that comes up healthy
  # and is broken somewhere it does not mention". Unset, `${PWD}` is the install directory, because
  # `dc` runs from there.
  echo "KONTRA_WORKSPACES=$WORKSPACES"
  # THE REGISTRY IS AUTHENTICATED, which is the shape the anonymous default cannot test: `kontra
  # deploy` pushed with an empty credential for as long as the zot accounts existed, and an
  # anonymous registry accepts that. ALL THREE OR NONE — `registry-config` refuses a partial set.
  # Generated per run, written straight into the file, never echoed.
  echo "KONTRA_REGISTRY_PUSH_ACTORS_PASSWORD=$(openssl rand -hex 24)"
  echo "KONTRA_REGISTRY_PUSH_RUNTIMES_PASSWORD=$(openssl rand -hex 24)"
  echo "KONTRA_REGISTRY_PULL_PASSWORD=$(openssl rand -hex 24)"
} >> "$INSTALL_DIR/.env"
if "$PYTHON" "$REPO/scripts/assert-install-is-self-contained.py" "$INSTALL_DIR" \
     >"$LOGS/self-contained.log" 2>&1; then
  ok "the install directory holds nothing a curl would not have"
else
  bad "the install directory is not self-contained — see $LOGS/self-contained.log"
  sed 's/^/     /' "$LOGS/self-contained.log"
  exit 1
fi

# dc — every compose call, with the project and the install directory, in one place.
#
# IT RUNS FROM THE INSTALL DIRECTORY, and that is not cosmetic: compose substitutes `${PWD}` from the
# ENVIRONMENT, not from `--project-directory`, so a `${KONTRA_REPO:-${PWD}}` mount would otherwise
# resolve to wherever this script was invoked — the checkout — and mount it into the container.
dc() { (cd "$INSTALL_DIR" && docker compose -p "$PROJECT" "$@"); }
# kli — a kontra command inside the `cli` container. `-T` because there is no tty in CI and
# compose would otherwise refuse; the repo is mounted at its own path, so `tests/parity/actor` is
# the same string on both sides.
kli() { dc exec -T cli "$@"; }

cleanup() {
  local rc=$?
  [ "$CLEANED" = "1" ] && return
  CLEANED=1
  if [ "$KEEP" = "1" ] && [ ${#FAILURES[@]} -gt 0 ]; then
    say "--keep: leaving $PROJECT up (install dir $INSTALL_DIR)"
    return
  fi
  say "5 · teardown"
  # THE WORKER CONTAINERS FIRST, BY THE LABEL THIS SCRIPT'S DEPLOY PUT ON THEM — never by a name
  # pattern, and never `docker ps` filtered by anything broader. They are not compose's, so
  # `compose down` does not know about them; a gate that skipped this would leak a worker
  # container per run, which on a developer box is how a laptop ends up with forty of them.
  if [ "$COMPOSE_UP" = "1" ]; then
    kli kontra serve --actor "$ACTOR_DIR" --mode docker --replicas 0 >"$LOGS/scale-down.log" 2>&1 \
      && ok "worker containers removed" \
      || info "scale-down reported an error; see $LOGS/scale-down.log"
  fi
  local left
  left=$(docker ps -aq --filter "label=kontra.actor=$ACTOR_NAME@$ACTOR_VERSION" 2>/dev/null | wc -l | tr -d ' ')
  if [ "$left" != "0" ]; then
    docker rm -f $(docker ps -aq --filter "label=kontra.actor=$ACTOR_NAME@$ACTOR_VERSION") >/dev/null 2>&1
    info "force-removed $left leftover worker container(s)"
  fi
  # The stack last: it is what everything above was talking to. `-v` because the volumes are this
  # project's own and nothing else can be holding them.
  if [ "$COMPOSE_UP" = "1" ]; then
    dc down -v --remove-orphans >"$LOGS/compose-down.log" 2>&1 \
      && ok "compose project $PROJECT removed, with its volumes" \
      || info "compose down reported an error; see $LOGS/compose-down.log"
  fi
  teardown_assertions
  if [ "$KEEP" = "1" ]; then
    info "--keep: the install directory and logs are left at $GATE_TMP"
  else
    rm -rf "$GATE_TMP"
  fi
  report
  exit $rc
}

# TEARDOWN IS AN ASSERTION, not a courtesy. "teardown leaves nothing running" is an acceptance
# criterion, and a gate that tore down without checking would pass while leaking.
teardown_assertions() {
  local containers ports_bound=0 project_left volumes_left
  containers=$(docker ps -aq --filter "label=kontra.actor=$ACTOR_NAME@$ACTOR_VERSION" 2>/dev/null | wc -l | tr -d ' ')
  check "worker containers after teardown" "0" "$containers"
  project_left=$(docker ps -aq --filter "label=com.docker.compose.project=$PROJECT" 2>/dev/null | wc -l | tr -d ' ')
  check "project containers after teardown" "0" "$project_left"
  # THE VOLUMES TOO, because `down` without `-v` keeps them and the next run of this gate would
  # then start on the previous run's Postgres. A project-scoped filter: nothing else's volumes can
  # match a name this script invented.
  volumes_left=$(docker volume ls -q --filter "label=com.docker.compose.project=$PROJECT" 2>/dev/null | wc -l | tr -d ' ')
  check "project volumes after teardown" "0" "$volumes_left"
  for p in "$P_TEMPORAL" "$P_S3" "$P_KV" "$P_REGISTRY" "$P_API" "$P_POSTGRES"; do
    port_free "$p" || { ports_bound=$((ports_bound + 1)); info "still bound: $p"; }
  done
  check "published ports still bound after teardown" "0" "$ports_bound"
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
    printf '\n\033[32m%s\033[0m\n' "PARITY GATE PASSES — the compose install serves the local development path."
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
# NOTHING about the stack. That is the point of running it here as well as later: one suite,
# twice, one control plane apart.
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
say "2 · the compose control plane, on its own project"
info "docker compose -p $PROJECT up -d --wait"
if ! dc up -d --wait >"$LOGS/compose-up.log" 2>&1; then
  COMPOSE_UP=1   # some of it may be up; cleanup still has to run
  bad "docker compose up -d --wait failed — see $LOGS/compose-up.log"
  tail -40 "$LOGS/compose-up.log"
  dc ps >>"$LOGS/compose-up.log" 2>&1
  sed 's/^/     /' <(dc ps 2>&1)
  exit 1
fi
COMPOSE_UP=1
ok "every service reports healthy"

# READY IS "THE API ANSWERS", not "compose said --wait". A healthcheck can pass on a process that
# has bound its port and not finished opening its catalog, and everything after this dispatches
# work — so the next step would fail with a connection error that reads like a bug in itself.
UP=0
for _ in $(seq 1 120); do
  if curl -fsS --max-time 2 "http://127.0.0.1:$P_API/api/health" >/dev/null 2>&1; then UP=1; break; fi
  sleep 1
done
if [ "$UP" != "1" ]; then
  bad "the API never answered on http://127.0.0.1:$P_API/api/health"
  dc logs --tail 40 orchestrator-api 2>&1 | sed 's/^/     /'
  exit 1
fi
ok "control plane answering at http://127.0.0.1:$P_API"

# LOOPBACK IS THE SECURITY CONTROL, ASSERTED RATHER THAN ASSUMED. Every `ports:` entry in
# docker-compose.yml is `${KONTRA_BIND:-127.0.0.1}:…`, and a service that grew a bare `- "8088:8088"`
# would publish on every interface — which on a box with a public address is a control plane on the
# internet. The gate would otherwise pass while the install got more exposed, not less.
WIDE=$(ss -ltnH 2>/dev/null | awk -v ports="$P_TEMPORAL $P_S3 $P_KV $P_REGISTRY $P_API $P_POSTGRES" '
  { split($4, a, ":"); p = a[length(a)]; addr = substr($4, 1, length($4) - length(p) - 1)
    if (index(" " ports " ", " " p " ") && (addr == "0.0.0.0" || addr == "*" || addr == "[::]")) print addr ":" p }')
if [ -z "$WIDE" ]; then
  ok "every published port is on 127.0.0.1 — none on 0.0.0.0"
else
  bad "a service ignored KONTRA_BIND and published on every interface: $(echo "$WIDE" | tr '\n' ' ')"
fi

# ═════════════════════════════════════════════════════════════════════════════════════════════
say "3 · condition 2 — the local actor path"

# `--override`, BECAUSE THE WATCHER IS ALREADY DEPLOYING THIS ACTOR. `kontra workspace watch` is the
# `cli` service's main process and it runs `kontra deploy --override` for every actor directory under
# the workspaces tree — which is where this one had to be created for the container to see it. So by
# the time this line runs the watcher may have pushed the version already, and without `--override`
# the gate would be refused by the immutability check it is not here to test. The two deploys no
# longer OVERLAP (cli/deploylock.go serialises them per version); this is about which of them wins.
info "kontra deploy --actor $ACTOR_DIR --override  (inside the cli container)"
if ! kli kontra deploy --actor "$ACTOR_DIR" --override >"$LOGS/deploy.log" 2>&1; then
  bad "kontra deploy failed — see $LOGS/deploy.log"
  tail -30 "$LOGS/deploy.log"
  exit 1
fi
# THE DIGEST IS THE RECEIPT. A push that answered without writing anything is the failure mode a
# tag cannot see (ADR 0032), so the gate asks the registry rather than trusting the summary.
DIGEST=$(grep -Eo 'sha256:[0-9a-f]{64}' "$LOGS/deploy.log" | head -1)
if [ -n "$DIGEST" ]; then ok "pushed $ACTOR_NAME:$ACTOR_VERSION → $DIGEST"; else bad "deploy printed no manifest digest"; fi
# ASKED FROM THE HOST, AT THE PUBLISHED PORT — the same store the push went into, reached by the
# other of its two addresses. If those two ever stop being one registry, this is where it shows.
#
# WITH THE CREDENTIAL, because this gate runs the AUTHENTICATED shape: zot's rendered accessControl
# gives every repository `"defaultPolicy": []`, so an anonymous `tags/list` is 401 and this check
# would report "the registry does not list the version" about a registry that holds it. Read back
# from the file this script wrote it to, and never echoed — `--fail` keeps the body out of the log.
PULL_PW=$(sed -n 's/^KONTRA_REGISTRY_PULL_PASSWORD=//p' "$INSTALL_DIR/.env" | tail -1)
TAGS=$(curl -fsS --max-time 5 -u "pull:$PULL_PW" \
         "http://127.0.0.1:$P_REGISTRY/v2/$ACTOR_NAME/tags/list" 2>/dev/null)
case "$TAGS" in
  *"\"$ACTOR_VERSION\""*) ok "the install's registry holds $ACTOR_NAME:$ACTOR_VERSION" ;;
  *) bad "the registry on :$P_REGISTRY does not list $ACTOR_VERSION (answered: ${TAGS:-nothing})" ;;
esac

info "kontra serve --mode docker --replicas $REPLICAS"
if ! kli kontra serve --actor "$ACTOR_DIR" --mode docker --replicas "$REPLICAS" >"$LOGS/scale.log" 2>&1; then
  bad "kontra serve --mode docker failed — see $LOGS/scale.log"
  tail -30 "$LOGS/scale.log"
  exit 1
fi
sed 's/^/     /' "$LOGS/scale.log"
RUNNING=$(docker ps -q --filter "label=kontra.actor=$ACTOR_NAME@$ACTOR_VERSION" | wc -l | tr -d ' ')
check "worker containers running" "$REPLICAS" "$RUNNING"

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
    kli kontra workers list >"$logfile" 2>&1
    row=$(grep -E "[[:space:]]${queue}[[:space:]]" "$logfile" | head -1)
    case "$row" in
      "" | *"(none"* | *"temporal error"*) sleep 2 ;;
      *) POLLERS_ROW="$row"; return 0 ;;
    esac
  done
  POLLERS_ROW="$row"
  return 1
}

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

# THE WORKFLOW WORKER, DETACHED INSIDE THE CONTAINER, which is what `compose down` later takes
# with it — so there is no host pid to kill and no `pkill -f` pattern that could match this script.
#
# `nohup`, NOT A BARE `&`. The exec session ends the moment this call returns; a backgrounded child
# that still has the exec's terminal as its stdio can be torn down with it, and what that looks
# like is the wait below timing out on a worker that was running a second ago.
info "kontra workflow serve $WORKFLOW_DIR"
kli sh -c "nohup kontra workflow serve '$WORKFLOW_DIR' >/tmp/workflow-worker.log 2>&1 &" >/dev/null 2>&1
# WAITED FOR, NOT SLEPT THROUGH. `kontra workflow start` REFUSES a folder whose derived queue has
# no pollers — deliberately, because a start against an unserved queue sits `running` forever with
# no error. A fixed sleep that is a second short turns that correct refusal into a gate failure
# about the wrong thing, so the gate waits for the worker's own "serving" line.
WF_UP=0
for _ in $(seq 1 90); do
  kli sh -c 'cat /tmp/workflow-worker.log 2>/dev/null' >"$LOGS/workflow-worker.log" 2>&1
  grep -q "\[wfhost\] ParityGate" "$LOGS/workflow-worker.log" 2>/dev/null && { WF_UP=1; break; }
  sleep 1
done
if [ "$WF_UP" != "1" ]; then
  bad "the workflow worker never reported serving ParityGate — see $LOGS/workflow-worker.log"
  tail -30 "$LOGS/workflow-worker.log"
  exit 1
fi
ok "workflow worker up: $(grep -m1 '\[wfhost\]' "$LOGS/workflow-worker.log")"

# A NAME PER RUN. The gate asserts an exact row count, and a name reused across runs would make
# the second run's count the sum of two — which passes `>= units` and hides a leg that produced
# nothing this time.
DATASET="paritygate_$(date +%s)"
info "kontra workflow start $WORKFLOW_DIR --wait --input {\"units\": $UNITS}"
kli kontra workflow start "$WORKFLOW_DIR" --wait --timeout 15m \
  --input "{\"units\": $UNITS, \"dataset\": \"$DATASET\"}" >"$LOGS/run.log" 2>&1
RUN_RC=$?
sed 's/^/     /' "$LOGS/run.log"
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
kli kontra dataset list >"$LOGS/datasets.log" 2>&1
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
#
# EXPORTED INSIDE THE CONTAINER AND READ BACK OUT, because `--export` writes a file and the only
# filesystem both sides share is the read-only repo mount. /tmp in the container, then `cat`.
kli kontra dataset query "$DATASET" \
  --sql "SELECT count(*) AS rows_out, count(DISTINCT id) AS ids, count(DISTINCT worker) AS workers FROM \"$DATASET\"" \
  --export /tmp/rowcount.json >"$LOGS/rowcount.log" 2>&1
kli sh -c 'cat /tmp/rowcount.json 2>/dev/null' >"$LOGS/rowcount.json" 2>/dev/null
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
say "4 · condition 1b — THE E2E LEG AGAINST THIS STACK"
# The e2e leg dispatches to a real Worker, so one has to exist. The fixture actor is served in
# LOCAL mode — the actor process and the Go handler, inside the `cli` container — deliberately,
# and not as a shortcut: the image path is already proven by step 3, and this step is about
# whether the SDK's own contract holds against the control plane rather than how a Worker was
# placed.
info "kontra serve --actor testdata/fixtureactor (local mode, for the e2e leg)"
kli sh -c 'nohup kontra serve --actor testdata/fixtureactor >/tmp/fixture.log 2>&1 &' >/dev/null 2>&1
BEACON_QUEUE="fixtureactor-0.1.0"
if wait_for_pollers "$BEACON_QUEUE" 120 "$LOGS/workers-fixture.log"; then
  ok "the fixture actor is polling $BEACON_QUEUE: $POLLERS_ROW"
else
  bad "the fixture actor never started polling $BEACON_QUEUE — the e2e leg below has nothing to dispatch to"
  sed 's/^/     /' "$LOGS/workers-fixture.log"
  kli sh -c 'tail -25 /tmp/fixture.log 2>/dev/null' 2>&1 | sed 's/^/     /'
fi

# THE SUITE RUNS ON THE HOST, AT THE PUBLISHED PORTS. That is the one thing this leg does from
# outside the network, and it is right: the SDK an actor author installs runs on their machine,
# so the address it is handed has to be the one an operator is given.
#
# `KONTRA_E2E=1` is what turns the `e2e`-marked tests on; they skip themselves when no
# KONTRA_ADDRESS is set, so run 1a above and this run differ in exactly one thing.
E2E_RECEIPT="$LOGS/e2e-receipt"
: >"$E2E_RECEIPT"
KONTRA_E2E_RECEIPT="$E2E_RECEIPT" KONTRA_E2E=1 \
  KONTRA_ADDRESS="127.0.0.1:$P_TEMPORAL" \
  KONTRA_S3_ENDPOINT="http://127.0.0.1:$P_S3" \
  KONTRA_REDIS_HOST="127.0.0.1:$P_KV" \
  KONTRA_ORCHESTRATOR_URL="http://127.0.0.1:$P_API" \
  "$PYTHON" -m pytest -q "$REPO/tests/test_fixture_actor_e2e.py" >"$LOGS/examples-stack.log" 2>&1
E2E_RC=$?
E2E_LINE=$(grep -Eo '[0-9]+ (passed|failed|error|skipped)[^,]*' "$LOGS/examples-stack.log" | tr '\n' ' ')
info "pytest said: ${E2E_LINE:-<no counts printed>}"
# A SUITE THAT SKIPPED EVERY E2E TEST IS NOT A PASS. That is the exact shape of the warning this
# gate opens with — the examples suite would go green against a control plane that never started —
# so the gate refuses a run in which nothing e2e actually executed.
#
# COUNTED FROM A RECEIPT FILE, NOT FROM THE LOG. The first version grepped the run output for a
# line the test prints; pytest CAPTURES stdout and replays it only for failures, so a test that
# PASSED left no line and the gate reported "they all skipped" about the leg that had just proven
# the stack. The test appends to $KONTRA_E2E_RECEIPT instead — a side effect pytest does not own.
E2E_RAN=$(wc -l <"$E2E_RECEIPT" 2>/dev/null | tr -d ' ')
E2E_RAN=${E2E_RAN:-0}
if [ "$E2E_RC" != "0" ]; then
  bad "the e2e leg exited $E2E_RC — see $LOGS/examples-stack.log"
  tail -40 "$LOGS/examples-stack.log"
elif [ "$E2E_RAN" = "0" ]; then
  bad "the e2e leg was green but ran nothing against the stack (it skipped) — see $LOGS/examples-stack.log"
else
  ok "the e2e leg is green, with $E2E_RAN dispatch(es) dialling this stack:"
  sed 's/^/        /' "$E2E_RECEIPT"
fi

# THE LAST LINE, and the only place this is set. See REACHED_END at the top.
REACHED_END=1
exit 0
