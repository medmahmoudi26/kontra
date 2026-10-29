#!/usr/bin/env bash
# The runner inside the clean laptop (test/demo/Dockerfile). scripts/demo-clean-laptop.sh copies it, with the README
# block's commands, to /opt/kontra-demo before the container starts (never a mount). It runs the commands one at a time
# in this one shell, as typed: `export` and `cd` carry over, a command ending in `&` runs in the background. It makes
# exactly the substitutions the host script lists, and says so when it makes one:
#   (a) KONTRA_DEMO_MODE=source: the command `go install kontra.sh/kontra/cmd/kontra@latest` is run as
#       `(cd /src && go install ./cmd/kontra)`; KONTRA_DEMO_MODE=published runs it as written, again a minute apart
#       for up to 30 minutes until `kontra version` names KONTRA_DEMO_EXPECT (proxy.golang.org's @latest may lag);
#   (b) `anon-<…>` is the slug kontra connect printed; a command naming it waits until it did (3 minutes at most).
# Each command's output is captured and printed. Checked: every command exits 0, a background one is still running a
# second later, and the clients answer what the README says (a client command is tried 3 times, 15 s apart, since
# the first npx run of each package downloads it and the node starts server-filesystem through npx on first use):
#   a2a send … "hi"                 Hello World
#   inspector … --method tools/list read_file
#   inspector … --tool-name hire    Hello World
#   curl … agent-card.json          the card, naming the slug
# Then it prints the slug and "ready" for the host's checks, and holds the node connected until it is stopped (TERM,
# docker stop): it then stops what it started and exits 0.
#
# Lines for the host, on stdout: "kontra-demo: slug <slug>", "kontra-demo: ready", "kontra-demo: FAIL <why>".
# KONTRA_DEMO_EXTRA_CA (a rehearsal against a local gate, test/demo): a CA certificate to trust besides the system's,
# through SSL_CERT_FILE (Go), CURL_CA_BUNDLE and NODE_EXTRA_CA_CERTS; nothing of the block changes.
set -u

here=/opt/kontra-demo
work=/tmp/kontra-demo
mode=${KONTRA_DEMO_MODE:-source}
expect=${KONTRA_DEMO_EXPECT:-}
install_line='go install kontra.sh/kontra/cmd/kontra@latest'
placeholder='anon-<…>'
slug_re='/agents/(anon-[0-9a-f]{16})/'
pids=()

say() { printf 'kontra-demo: %s\n' "$*"; }

stop_all() {
  local p
  for p in "${pids[@]}"; do kill -TERM "$p" 2>/dev/null; done
  for p in "${pids[@]}"; do
    for _ in 1 2 3 4 5 6 7 8 9 10; do kill -0 "$p" 2>/dev/null || break; sleep 0.5; done
    kill -KILL "$p" 2>/dev/null
  done
}

fail() {
  say "FAIL $*"
  stop_all
  exit 1
}

trap 'say "stopped"; stop_all; exit 0' TERM INT

mkdir -p "$work" || fail "cannot create $work"
if [ -n "${KONTRA_DEMO_EXTRA_CA:-}" ]; then
  cat /etc/ssl/certs/ca-certificates.crt "$KONTRA_DEMO_EXTRA_CA" > "$work/ca.pem" || fail "cannot read $KONTRA_DEMO_EXTRA_CA"
  export SSL_CERT_FILE="$work/ca.pem" CURL_CA_BUNDLE="$work/ca.pem" NODE_EXTRA_CA_CERTS="$KONTRA_DEMO_EXTRA_CA"
  say "rehearsal: trusting $KONTRA_DEMO_EXTRA_CA besides the system's roots"
fi

mapfile -t cmds < "$here/commands" || fail "no commands"
say "$(id -un) on $(uname -m), $(go version), node $(node --version), ${#cmds[@]} commands, mode $mode"

slug=""
connect_out=""
connect_pid=""

# wait_slug reads the slug from kontra connect's output, once it printed its welcome.
wait_slug() {
  local i
  [ -n "$connect_out" ] || fail "a command names anon-<…> before kontra connect ran"
  for ((i = 0; i < 180; i++)); do
    if [[ "$(cat "$connect_out" 2>/dev/null)" =~ $slug_re ]]; then
      slug=${BASH_REMATCH[1]}
      say "slug $slug"
      return
    fi
    kill -0 "$connect_pid" 2>/dev/null || { cat "$connect_out"; fail "kontra connect exited before printing a slug"; }
    sleep 1
  done
  cat "$connect_out"
  fail "kontra connect printed no slug within 3 minutes"
}

# expectation is what a client command must print (empty: only its exit status is checked).
expectation() {
  case "$1" in
    "a2a send "*) echo "Hello World" ;;
    *"--method tools/list"*) echo "read_file" ;;
    *"--tool-name hire"*) echo "Hello World" ;;
    "curl "*agent-card.json*) echo "$slug" ;;
    *) echo "" ;;
  esac
}

# installed checks, in published mode, that kontra version names the expected commit.
installed() {
  local v
  v=$("$(go env GOPATH)/bin/kontra" version 2>&1)
  say "kontra version: $v"
  [ -z "$expect" ] || [[ "$v" == *"${expect:0:12}"* ]]
}

n=0
for cmd in "${cmds[@]}"; do
  n=$((n + 1))
  out="$work/out.$n"
  run=$cmd
  if [ "$cmd" = "$install_line" ] && [ "$mode" = source ]; then
    run='(cd /src && go install ./cmd/kontra)'
    say "substitution (a): $cmd  ->  $run"
  fi
  if [[ "$run" == *"$placeholder"* ]]; then
    [ -n "$slug" ] || wait_slug
    run=${run//"$placeholder"/$slug}
    say "substitution (b): anon-<…>  ->  $slug"
  fi
  want=$(expectation "$cmd")
  printf '$ %s\n' "$run"

  if [[ "$run" =~ \&[[:space:]]*(#.*)?$ ]]; then
    eval "$run" > "$out" 2>&1
    pids+=("$!")
    if [[ "$run" == "kontra connect "* ]]; then
      connect_out=$out
      connect_pid=$!
    fi
    sleep 1
    kill -0 "$!" 2>/dev/null || { cat "$out"; fail "command $n exited at once: $run"; }
    continue
  fi

  case "$cmd" in
    "a2a send "* | "npx "* | "curl "*)
      ok=""
      for attempt in 1 2 3; do
        timeout 300 bash -c "$run" > "$out" 2>&1
        rc=$?
        cat "$out"
        if [ "$rc" -eq 0 ] && { [ -z "$want" ] || grep -qF -- "$want" "$out"; }; then
          ok=1
          break
        fi
        say "command $n, attempt $attempt: exit $rc${want:+, want \"$want\" in its output}"
        [ "$attempt" -lt 3 ] && sleep 15
      done
      [ -n "$ok" ] || fail "command $n: $run"
      say "ok $n${want:+: \"$want\"}"
      ;;
    *)
      eval "$run" > "$out" 2>&1
      rc=$?
      cat "$out"
      [ "$rc" -eq 0 ] || fail "command $n exited $rc: $run"
      if [ "$cmd" = "$install_line" ]; then
        if [ "$mode" = published ]; then
          deadline=$((SECONDS + 1800))
          until installed; do
            [ "$SECONDS" -lt "$deadline" ] || fail "kontra version does not name $expect after 30 minutes"
            say "@latest does not name $expect yet: installing again in a minute"
            sleep 60
            eval "$run" > "$out" 2>&1 || { cat "$out"; fail "command $n exited: $run"; }
          done
        else
          installed || true
        fi
      fi
      say "ok $n"
      ;;
  esac
done

[ -n "$slug" ] || fail "the block never named the slug"
say "slug $slug"
say "ready"
sleep infinity &
wait $!
