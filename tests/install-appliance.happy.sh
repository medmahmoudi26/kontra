#!/usr/bin/env bash
# The install that WORKS, against a release this repo actually built.
#
# SPLIT FROM `install-appliance.test.sh` FOR ONE REASON: cost. The refusals need tarballs of a few
# hundred bytes and run anywhere in about ten seconds. This one needs a genuine
# `kontra-orchestrator-*.tar.gz` — 124 MB, and producing it means a pnpm production install — so it
# is a script you run deliberately rather than a test that runs itself. Running the expensive half
# by hand is a real gap and it is named here rather than papered over: until somebody runs this, the
# installer's SUCCESS path is the one path nothing has executed.
#
# WHAT IT PROVES THAT THE REFUSALS CANNOT. Every case in the other file ends in `die`, so all of
# them together still never reach: the copy into `$KONTRA_HOME/bundles`, the binary landing on PATH,
# `kontra bundle verify` doing real work on a real manifest, or the layout contract holding. A suite
# made only of refusals is a suite that has never seen the program work.
#
#   ./tests/install-appliance.happy.sh                # builds the bundle if it has to
#   KONTRA_TEST_BUNDLE=/path/to/kontra-orchestrator-*.tar.gz ./tests/install-appliance.happy.sh
#
# The second form is the one to use on a box that is short on disk: point it at a bundle that
# already exists and nothing is built.
set -uo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ROOT=$(cd "$HERE/.." && pwd)
SCRIPT="$ROOT/install-appliance.sh"
KONTRA_BIN_SRC="${KONTRA_BIN_SRC:-$(command -v kontra || echo /usr/local/bin/kontra)}"

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"; [ -n "${SRV:-}" ] && kill "$SRV" 2>/dev/null' EXIT

pass=0; fail=0
ok()  { printf '  ok    %s\n' "$1"; pass=$((pass+1)); }
bad() { printf '  FAIL  %s\n     %s\n' "$1" "$2"; fail=$((fail+1)); }
die() { printf 'cannot run: %s\n' "$*" >&2; exit 2; }

[ -x "$KONTRA_BIN_SRC" ] || die "no kontra binary at $KONTRA_BIN_SRC (set KONTRA_BIN_SRC)"

# --- a genuine bundle, built or borrowed -------------------------------------------------------
bundle="${KONTRA_TEST_BUNDLE:-}"
if [ -z "$bundle" ]; then
  free_kb=$(df -Pk "$WORK" | awk 'NR==2 {print $4}')
  [ "$free_kb" -gt 1500000 ] || die "building a bundle needs ~1.5 GB free; $((free_kb/1024)) MB available.
  Free some, or point KONTRA_TEST_BUNDLE at a bundle that already exists."
  echo "building an orchestrator bundle (no Go build: using $KONTRA_BIN_SRC)"
  ( cd "$ROOT/cli" && "$KONTRA_BIN_SRC" bundle orchestrator --repo .. --out "$WORK/bundles" ) \
    || die "bundle orchestrator failed"
  bundle=$(ls "$WORK/bundles"/kontra-orchestrator-*.tar.gz 2>/dev/null | head -1)
fi
[ -f "$bundle" ] || die "no orchestrator bundle to install"
echo "using bundle: $(basename "$bundle") ($(du -h "$bundle" | cut -f1))"

# --- assemble a release in the layout `kontra release` documents -------------------------------
#
# THE LAYOUT IS A CONTRACT WITH TWO WRITERS and, as of this writing, no gate — `cli/release.go`
# creates it and `install-appliance.sh` reads it, and nothing asserts they agree. That is the exact
# shape ADR 0035 rule two is about. This script is the closest thing to a gate today: it lays the
# tarball out the way `release.go`'s own header specifies and the installer either copes or does
# not. If you are reading this because it broke, the fix is a corpus, not a patch here.
stage="$WORK/stage"
mkdir -p "$stage/bundles" "$stage/manifests"
cp "$KONTRA_BIN_SRC" "$stage/kontra"
cp "$bundle" "$stage/bundles/"
[ -f "${bundle}.manifest.json" ] && cp "${bundle}.manifest.json" "$stage/manifests/"
for extra in "$ROOT"/../*/kontra-spa.tar.gz "$WORK/bundles"/kontra-spa.tar.gz; do
  [ -f "$extra" ] && cp "$extra" "$stage/bundles/"
done

case "$(uname -m)" in x86_64|amd64) ARCH=amd64 ;; arm64|aarch64) ARCH=arm64 ;; *) die "unsupported arch" ;; esac
case "$(uname -s)" in Linux) OS=linux ;; Darwin) OS=darwin ;; *) die "unsupported os" ;; esac
VER=0.0.0-happy
FILE="kontra_${VER}_${OS}_${ARCH}.tar.gz"

mkdir -p "$WORK/serve"
( cd "$stage" && tar -czf "$WORK/serve/$FILE" . )
( cd "$WORK/serve" && sha256sum "$FILE" > SHA256SUMS )

( cd "$WORK/serve" && exec python3 -m http.server 8732 --bind 127.0.0.1 ) >/dev/null 2>&1 &
SRV=$!
for _ in $(seq 1 50); do curl -fsS -o /dev/null "http://127.0.0.1:8732/" 2>/dev/null && break; sleep 0.1; done

echo "install-appliance.sh — the success path"
out=$(env KONTRA_VERSION="$VER" KONTRA_BASE_URL="http://127.0.0.1:8732" \
          KONTRA_HOME="$WORK/home" KONTRA_BIN="$WORK/bin" sh "$SCRIPT" 2>&1)
status=$?

[ $status -eq 0 ] && ok "the installer exits 0 on a genuine release" \
  || bad "the installer exits 0 on a genuine release" "exit $status:
     $(printf '%s' "$out" | tail -5)"

[ -x "$WORK/bin/kontra" ] && ok "the binary lands on PATH and is executable" \
  || bad "the binary lands on PATH and is executable" "no $WORK/bin/kontra"

# `findBundles` looks here when there is no checkout, which is what makes the install a copy into a
# path the program already knows rather than an environment variable somebody has to be told about.
ls "$WORK/home/bundles"/kontra-orchestrator-*.tar.gz >/dev/null 2>&1 \
  && ok "the bundle lands where findBundles looks" \
  || bad "the bundle lands where findBundles looks" "nothing in $WORK/home/bundles"

case "$out" in
  *"verified"*) ok "the installed bundle was verified against its own manifest" ;;
  *) bad "the installed bundle was verified against its own manifest" \
         "no verification line in the output" ;;
esac

# The installed binary runs out of where it was put — the narrow question a cross-build cannot
# answer, and the one the release workflow asks with `--help` for the same reason.
if "$WORK/bin/kontra" help >/dev/null 2>&1 || [ $? -le 1 ]; then
  ok "the installed binary executes"
else
  bad "the installed binary executes" "it does not run from $WORK/bin"
fi

echo
echo "  $pass passed, $fail failed"
[ "$fail" -eq 0 ]
