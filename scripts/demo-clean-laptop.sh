#!/usr/bin/env bash
# The README's "Five minutes" on a clean laptop (D280, D282, D298; the M4 plan's gate run B2, the gate item "demo
# from a clean laptop against staging"). A fresh container stands in for the laptop: test/demo/Dockerfile (Go 1.26.10
# and Node 22, a user with an empty home, nothing else), built from `git archive HEAD` with the docker driver's
# builder, run on the default bridge (internet egress only) with no host mount and no secret. Inside it,
# test/demo/laptop.sh runs README.md's block between <!-- demo:begin --> and <!-- demo:end --> (HEAD's README) as
# written, one command at a time in one shell, with exactly these substitutions:
#   (a) before the commit is on GitHub (the default, --source): `go install kontra.sh/kontra/cmd/kontra@latest` runs as
#       `(cd /src && go install ./cmd/kontra)`, /src being `git archive HEAD`. With --published it runs as written, the
#       image has no /src, and `kontra version` must name --expect <commit> (installed again, a minute apart, for up to
#       30 minutes while proxy.golang.org's @latest lags);
#   (b) `anon-<…>` becomes the slug kontra connect printed, in the commands after it.
# The runner checks each command (laptop.sh's header lists what each client must print), then writes the slug to its
# stdout and holds the node connected. This host then checks, with no credential, while the node is still connected:
# the public card (GET …/agents/<slug>/<public service>/.well-known/agent-card.json names that URL) and a JSON-RPC
# SendMessage "hi" to the public URL, which must answer Hello World (and `a2a send` to the public card, when a2a is on
# this host's PATH). Then it stops the laptop (a stop file copied in: laptop.sh stops its node and exits 0) and checks
# that it exited 0. Everything is written to a transcript (--transcript; default ./kontra-demo-<UTC time>.log).
#
# Teardown, in a trap on EXIT, INT and TERM (printed before anything starts):
#   docker rm -f kontra-demo-<sha>; docker image rm kontra-demo:<sha> (unless --keep-image); rm -rf <its temporary dir>.
# Nothing on the gate is deleted: the anonymous tenant expires by itself after 2 h and is purged a day later
# (spec/tunnel.md §6.1). Each run opens one anonymous session from this host's network, and a network holds at most 3
# live ones (§6.2): at most 3 runs per 2 h from one /24.
#
# Usage: scripts/demo-clean-laptop.sh [--published --expect <commit>] [--transcript <file>] [--keep-image]
#        scripts/demo-clean-laptop.sh --dry-run | --build-only [--published]
#   --dry-run      print the commands, the substitutions, the checks and the teardown, and change nothing
#   --build-only   build kontra-demo:<sha> and keep it (test/demo checks what it holds)
#   --readme FILE  the README to read instead of HEAD's (tests)
#   Rehearsal against a local gate (test/demo, KONTRA_KIT_TESTS=1), never for the gate run:
#   --network NET  the laptop joins this docker network instead of the default bridge
#   --ca-file PEM  the laptop and this host's checks also trust this CA
#   --resolve ADDR this host's checks reach the gate's name at ADDR (curl --resolve)
# Prerequisites: docker (with a running daemon), git and curl; run from the repository. Exit 2 when one is missing.
set -euo pipefail

usage() { sed -n '/^# Usage:/,/^# Prerequisites:/p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 2; }

mode=source expect="" transcript="" dry=0 build_only=0 keep_image=0 readme="" network="" ca_file="" resolve=""
while [ $# -gt 0 ]; do
  case "$1" in
    --published) mode=published ;;
    --source) mode=source ;;
    --expect) expect=${2:?--expect needs a commit}; shift ;;
    --transcript) transcript=${2:?--transcript needs a file}; shift ;;
    --dry-run) dry=1 ;;
    --build-only) build_only=1 ;;
    --keep-image) keep_image=1 ;;
    --readme) readme=${2:?--readme needs a file}; shift ;;
    --network) network=${2:?--network needs a docker network}; shift ;;
    --ca-file) ca_file=${2:?--ca-file needs a PEM file}; shift ;;
    --resolve) resolve=${2:?--resolve needs an address}; shift ;;
    -h | --help) usage ;;
    *) echo "demo-clean-laptop: unknown argument $1" >&2; usage ;;
  esac
  shift
done
if [ "$mode" = published ] && [ -z "$expect" ] && [ "$dry" = 0 ] && [ "$build_only" = 0 ]; then
  echo "demo-clean-laptop: --published needs --expect <the merge commit>" >&2
  exit 2
fi
if [ -n "$expect" ] && ! [[ "$expect" =~ ^[0-9a-f]{12,40}$ ]]; then
  echo "demo-clean-laptop: --expect $expect is not a commit (12 to 40 hex digits)" >&2
  exit 2
fi
if [ -n "$ca_file" ] && [ ! -r "$ca_file" ]; then
  echo "demo-clean-laptop: --ca-file $ca_file is not readable" >&2
  exit 2
fi

# Prerequisites, all checked before anything is built or run.
missing=()
for tool in docker git curl; do
  command -v "$tool" > /dev/null 2>&1 || missing+=("$tool")
done
if [ ${#missing[@]} -gt 0 ]; then
  echo "demo-clean-laptop: missing prerequisites: ${missing[*]} (docker with a running daemon, git, curl)" >&2
  exit 2
fi
if ! docker info > /dev/null 2>&1; then
  echo "demo-clean-laptop: docker cannot reach its daemon (docker info failed): start it, or run where it runs" >&2
  exit 2
fi
repo=$(git rev-parse --show-toplevel 2> /dev/null) || { echo "demo-clean-laptop: run it from the repository" >&2; exit 2; }
cd "$repo"
sha=$(git rev-parse --short=12 HEAD)
image="kontra-demo:$sha"
name="kontra-demo-$sha"
target=laptop-src
[ "$mode" = published ] && target=laptop

# The block: HEAD's README (what `git archive HEAD` holds), or --readme.
if [ -n "$readme" ]; then
  text=$(cat "$readme")
else
  text=$(git show HEAD:README.md)
fi
begins=$(grep -c '^<!-- demo:begin -->$' <<< "$text" || true)
ends=$(grep -c '^<!-- demo:end -->$' <<< "$text" || true)
if [ "$begins" != 1 ] || [ "$ends" != 1 ]; then
  echo "demo-clean-laptop: README.md needs exactly one <!-- demo:begin --> and one <!-- demo:end --> line" >&2
  exit 2
fi
commands=()
cur=""
while IFS= read -r line; do
  case "$line" in '```'*) continue ;; esac
  if [[ "$line" == *\\ ]]; then
    cur+="${line%\\}"
    continue
  fi
  cur+="$line"
  trimmed="${cur#"${cur%%[![:space:]]*}"}"
  if [ -n "$trimmed" ] && [[ "$trimmed" != '#'* ]]; then
    commands+=("$trimmed")
  fi
  cur=""
done < <(sed -n '/^<!-- demo:begin -->$/,/^<!-- demo:end -->$/p' <<< "$text" | sed '1d;$d')
install_line='go install kontra.sh/kontra/cmd/kontra@latest'
installs=0
for c in "${commands[@]}"; do [ "$c" = "$install_line" ] && installs=$((installs + 1)); done
if [ "$installs" != 1 ]; then
  echo "demo-clean-laptop: the block must have the command '$install_line' exactly once (substitution (a))" >&2
  exit 2
fi
all="${commands[*]}"
[[ "$all" =~ --gate[[:space:]]+(https://[a-z0-9.-]+) ]] || { echo "demo-clean-laptop: no --gate https://… in the block" >&2; exit 2; }
gate=${BASH_REMATCH[1]}
[[ "$all" =~ --public[[:space:]]+([a-z0-9-]+) ]] || { echo "demo-clean-laptop: no --public <service> in the block" >&2; exit 2; }
public=${BASH_REMATCH[1]}
gatehost=${gate#https://}

kept=""
[ "$keep_image" = 0 ] || kept=" (skipped: --keep-image)"
teardown_text="docker rm -f $name; docker image rm $image$kept; rm -rf <the run's temporary directory>"
echo "demo-clean-laptop: $(date -u +%FT%TZ), commit $sha, mode $mode (target $target), gate $gate"
echo "teardown, in a trap on EXIT/INT/TERM: $teardown_text"
echo "commands (README.md, between the demo markers):"
i=0
for c in "${commands[@]}"; do i=$((i + 1)); printf '  %2d  %s\n' "$i" "$c"; done
echo "substitutions:"
if [ "$mode" = source ]; then
  echo "  (a) $install_line  ->  (cd /src && go install ./cmd/kontra)"
else
  echo "  (a) none: $install_line runs as written; kontra version must name ${expect:-<--expect>}"
fi
echo "  (b) anon-<…>  ->  the slug kontra connect prints"
echo "host checks, no credential, while the node is connected: GET $gate/agents/<slug>/$public/.well-known/agent-card.json;"
echo "  JSON-RPC SendMessage \"hi\" to $gate/agents/<slug>/$public (Hello World)"
if [ -n "$network$ca_file$resolve" ]; then
  echo "rehearsal: network ${network:-default bridge}, CA ${ca_file:-system}, $gatehost at ${resolve:-its DNS address}"
fi
if [ "$dry" = 1 ]; then
  echo "dry run: nothing built, run or removed"
  exit 0
fi
if [ "$build_only" = 0 ] && docker container inspect "$name" > /dev/null 2>&1; then
  echo "demo-clean-laptop: a container $name exists (another run, or a crashed one: docker rm -f $name)" >&2
  exit 2
fi

tmp=$(mktemp -d "${TMPDIR:-/tmp}/kontra-demo.XXXXXX")
follower=""
cleanup() {
  local rc=$?
  set +e
  [ -n "$follower" ] && kill "$follower" 2> /dev/null
  if [ "$build_only" = 0 ]; then
    docker rm -f "$name" > /dev/null 2>&1
    [ "$keep_image" = 1 ] || docker image rm "$image" > /dev/null 2>&1
  fi
  rm -rf "$tmp"
  echo "demo-clean-laptop: torn down ($teardown_text); exit $rc"
  exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

if [ "$build_only" = 0 ]; then
  [ -n "$transcript" ] || transcript="$PWD/kontra-demo-$(date -u +%Y%m%dT%H%M%SZ).log"
  exec > >(tee -a "$transcript") 2>&1
  echo "transcript: $transcript"
fi

mkdir -p "$tmp/ctx" "$tmp/payload"
# The archive's modes, not this host's umask: -p keeps them for a user who is not root too (root's tar always does),
# so /src is the laptop's to read.
git archive --format=tar HEAD | tar -x -p -C "$tmp/ctx"
chmod 0755 "$tmp/ctx"
echo "building $image (target $target) from git archive $sha"
BUILDX_BUILDER=default docker build -q --target "$target" -t "$image" -f "$tmp/ctx/test/demo/Dockerfile" "$tmp/ctx"
if [ "$build_only" = 1 ]; then
  echo "built $image (kept)"
  exit 0
fi

printf '%s\n' "${commands[@]}" > "$tmp/payload/commands"
cp "$tmp/ctx/test/demo/laptop.sh" "$tmp/payload/laptop.sh"
env_args=(-e "KONTRA_DEMO_MODE=$mode" -e "KONTRA_DEMO_EXPECT=$expect")
net_args=()
[ -n "$network" ] && net_args=(--network "$network")
if [ -n "$ca_file" ]; then
  cp "$ca_file" "$tmp/payload/extra-ca.pem"
  env_args+=(-e KONTRA_DEMO_EXTRA_CA=/opt/kontra-demo/extra-ca.pem)
fi
# docker cp keeps these modes (root becomes the owner) and the image runs as laptop (uid 1000): the payload must be
# world-readable whatever this host's umask, or the laptop cannot read its runner.
chmod 0755 "$tmp/payload"
chmod 0644 "$tmp/payload"/*
docker create --name "$name" --init "${net_args[@]}" "${env_args[@]}" "$image" bash /opt/kontra-demo/laptop.sh > /dev/null
docker cp "$tmp/payload/." "$name:/opt/kontra-demo" > /dev/null
docker start "$name" > /dev/null
# The log exists before the loop reads it: the follower opens it only once it runs, which under load may come after
# the loop's first read.
: > "$tmp/laptop.log"
docker logs -f "$name" >> "$tmp/laptop.log" 2>&1 &
follower=$!
started=$SECONDS
shown=0
slug=""
while :; do
  lines=$(wc -l < "$tmp/laptop.log")
  if [ "$lines" -gt "$shown" ]; then
    sed -n "$((shown + 1)),${lines}p" "$tmp/laptop.log" | sed 's/^/  laptop | /'
    shown=$lines
  fi
  if grep -q '^kontra-demo: FAIL' "$tmp/laptop.log"; then
    echo "demo-clean-laptop: FAIL in the laptop: $(grep '^kontra-demo: FAIL' "$tmp/laptop.log" | head -1)"
    exit 1
  fi
  if grep -q '^kontra-demo: ready$' "$tmp/laptop.log"; then
    slug=$(sed -n 's/^kontra-demo: slug \(anon-[0-9a-f]\{16\}\)$/\1/p' "$tmp/laptop.log" | tail -1)
    break
  fi
  if [ "$(docker container inspect -f '{{.State.Running}}' "$name" 2> /dev/null)" != true ]; then
    sleep 1
    sed -n "$((shown + 1)),\$p" "$tmp/laptop.log" | sed 's/^/  laptop | /'
    echo "demo-clean-laptop: FAIL: the laptop stopped before it was ready"
    exit 1
  fi
  if [ $((SECONDS - started)) -gt 2700 ]; then
    echo "demo-clean-laptop: FAIL: the laptop was not ready within 45 minutes"
    exit 1
  fi
  sleep 2
done
[[ "$slug" =~ ^anon-[0-9a-f]{16}$ ]] || { echo "demo-clean-laptop: FAIL: no slug from the laptop"; exit 1; }
echo "demo-clean-laptop: the laptop is ready ($((SECONDS - started)) s), slug $slug; checking from this host"

curl_args=(-sS --max-time 60 --proto '=https')
[ -n "$resolve" ] && curl_args+=(--resolve "$gatehost:443:$resolve")
[ -n "$ca_file" ] && curl_args+=(--cacert "$ca_file")
url="$gate/agents/$slug/$public"
card=$(curl "${curl_args[@]}" -f "$url/.well-known/agent-card.json") || { echo "demo-clean-laptop: FAIL: GET $url/.well-known/agent-card.json"; exit 1; }
echo "GET $url/.well-known/agent-card.json: $(head -c 300 <<< "$card")…"
if ! grep -qF "\"$url\"" <<< "$card"; then
  echo "demo-clean-laptop: FAIL: the public card does not name $url"
  exit 1
fi
body=$(printf '{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{"message":{"messageId":"kontra-demo-%s-%s","role":"ROLE_USER","parts":[{"text":"hi"}]}}}' "$sha" "$RANDOM$RANDOM")
answer=$(curl "${curl_args[@]}" -X POST -H 'Content-Type: application/json' -H 'A2A-Version: 1.0' --data "$body" "$url") ||
  { echo "demo-clean-laptop: FAIL: SendMessage to $url"; exit 1; }
echo "SendMessage to $url: $(head -c 600 <<< "$answer")"
if ! grep -qF 'Hello World' <<< "$answer"; then
  echo "demo-clean-laptop: FAIL: the public URL did not answer Hello World"
  exit 1
fi
if command -v a2a > /dev/null 2>&1 && [ -z "$resolve" ]; then
  out=$(a2a send -a "$url/.well-known/agent-card.json" "hi" 2>&1) || { echo "$out"; echo "demo-clean-laptop: FAIL: a2a send"; exit 1; }
  echo "a2a send -a $url/.well-known/agent-card.json \"hi\": $out"
  grep -qF 'Hello World' <<< "$out" || { echo "demo-clean-laptop: FAIL: a2a send did not print Hello World"; exit 1; }
fi

echo "demo-clean-laptop: host checks passed; stopping the laptop"
: > "$tmp/stop"
chmod 0644 "$tmp/stop"
docker cp "$tmp/stop" "$name:/opt/kontra-demo/stop" > /dev/null
if ! timeout 60 docker wait "$name" > /dev/null; then
  echo "demo-clean-laptop: the laptop did not stop within 60 s of its stop file; docker stop"
  docker stop -t 10 "$name" > /dev/null
fi
code=$(docker container inspect -f '{{.State.ExitCode}}' "$name")
sleep 1
sed -n "$((shown + 1)),\$p" "$tmp/laptop.log" | sed 's/^/  laptop | /'
if [ "$code" != 0 ]; then
  echo "demo-clean-laptop: FAIL: the laptop exited $code"
  exit 1
fi
echo "demo-clean-laptop: PASS (slug $slug, $((SECONDS - started)) s)"
