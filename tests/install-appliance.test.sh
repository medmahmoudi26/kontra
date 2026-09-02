#!/usr/bin/env bash
# What `install-appliance.sh` does when the release is not what it claims to be.
#
# WHY THIS EXISTS. Until it was tested, every path in that installer was unreachable: it resolves a
# GitHub release, and this repo has never published one. So its correctness could only ever have
# been learned from the first person to run it — and an installer is the one program whose first run
# is always on a machine you do not control, where a wrong branch is a half-installed appliance and
# a confusing sentence.
#
# `KONTRA_BASE_URL` is what makes it testable at all: point the installer at a directory served over
# HTTP and every step below runs unchanged, against the real script, with no mocking of anything
# inside it.
#
# WHAT THIS FILE COVERS: the refusals. Each case builds a release that is broken in exactly one way
# and asserts the installer both FAILS and SAYS WHY. The happy path needs a genuine ~124 MB
# orchestrator bundle and lives in `install-appliance.happy.sh`, which is run by hand when there is
# disk for it — the two are split so the cheap half can run anywhere.
#
# The assertions are on the MESSAGE, not just the exit code, and that is the point. A non-zero exit
# tells an operator that something broke; these cases are all shapes where the naive failure message
# names the wrong thing entirely — `cp: '*.tar.gz': No such file` for a release with no bundles, or
# a 404 for a repo whose only releases are pre-releases.
set -uo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ROOT=$(cd "$HERE/.." && pwd)
SCRIPT="$ROOT/install-appliance.sh"

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"; [ -n "${SRV:-}" ] && kill "$SRV" 2>/dev/null' EXIT

pass=0; fail=0
ok()   { printf '  ok    %s\n' "$1"; pass=$((pass+1)); }
bad()  { printf '  FAIL  %s\n     %s\n' "$1" "$2"; fail=$((fail+1)); }

# One server over a directory we rewrite between cases. `--bind 127.0.0.1` because this box has
# ports reachable from off-box (docker bypasses ufw) and a test server is not a thing to publish.
mkdir -p "$WORK/serve"
( cd "$WORK/serve" && exec python3 -m http.server 8731 --bind 127.0.0.1 ) >/dev/null 2>&1 &
SRV=$!
for _ in $(seq 1 50); do
  curl -fsS -o /dev/null "http://127.0.0.1:8731/" 2>/dev/null && break
  sleep 0.1
done

VER=0.0.0-test
FILE="kontra_${VER}_$(uname -s | tr 'A-Z' 'a-z')_amd64.tar.gz"
case "$(uname -m)" in x86_64|amd64) ARCH=amd64 ;; arm64|aarch64) ARCH=arm64 ;; esac
case "$(uname -s)" in Linux) OS=linux ;; Darwin) OS=darwin ;; esac
FILE="kontra_${VER}_${OS}_${ARCH}.tar.gz"

# Build a release tarball from a directory laid out however the caller wants, and publish a
# SHA256SUMS that actually describes it — so a case that is meant to test the bundle check is not
# accidentally testing the checksum check instead.
publish() {
  local stage="$1"
  rm -f "$WORK/serve/"*.tar.gz "$WORK/serve/SHA256SUMS"
  ( cd "$stage" && tar -czf "$WORK/serve/$FILE" . )
  ( cd "$WORK/serve" && sha256sum "$FILE" > SHA256SUMS )
}

# A STUB THAT IS A SCRIPT, NOT AN EMPTY FILE, and the difference is not stylistic. `: > kontra`
# with the exec bit set EXITS 0 for every argument list on Linux — the kernel falls back to running
# a zero-length file as a shell script with no commands in it. A stub like that succeeds at
# `bundle verify` on a bundle that is an empty tar, which makes every assertion downstream of it
# vacuous. This one records its argv and refuses, which is what the real binary would do.
stub_kontra() {
  cat > "$1" <<'STUB'
#!/bin/sh
printf '%s\n' "$*" >> "${KONTRA_STUB_LOG:-/dev/null}"
[ "$1" = "bundle" ] && [ "$2" = "verify" ] && {
  echo "error: $3 does not match its own manifest" >&2; exit 1; }
exit 0
STUB
  chmod +x "$1"
}

run_installer() {
  env KONTRA_VERSION="$VER" \
      KONTRA_BASE_URL="http://127.0.0.1:8731" \
      KONTRA_HOME="$WORK/home" \
      KONTRA_BIN="$WORK/bin" \
      KONTRA_STUB_LOG="$WORK/stub.log" \
      sh "$SCRIPT" 2>&1
}

expect_fail() {
  local name="$1" needle="$2" out
  rm -rf "$WORK/home" "$WORK/bin"
  out=$(run_installer)
  if [ $? -eq 0 ]; then
    bad "$name" "the installer SUCCEEDED on a release that is broken"
    return
  fi
  case "$out" in
    *"$needle"*) ok "$name" ;;
    *) bad "$name" "message does not mention '$needle':
     $(printf '%s' "$out" | tail -3)" ;;
  esac
}

echo "install-appliance.sh — the refusals"

# 1. A release whose tarball has no bundles/. The naive failure is `cp: cannot stat '*.tar.gz'`,
#    which names a file that was never supposed to exist and says nothing about the real fault.
stage="$WORK/s1"; mkdir -p "$stage"; stub_kontra "$stage/kontra"
publish "$stage"
expect_fail "a tarball with no bundles/ is refused by name" "carries no bundles/"

# 2. A release with bundles but no orchestrator bundle. The old loop would have iterated zero times
#    and printed nothing, leaving "verified" as the last word on an appliance that cannot start.
stage="$WORK/s2"; mkdir -p "$stage/bundles"; stub_kontra "$stage/kontra"
( cd "$stage/bundles" && tar -czf kontra-spa.tar.gz -T /dev/null )
publish "$stage"
expect_fail "no orchestrator bundle is refused rather than silently verified" "no orchestrator bundle"

# 3. A tarball that is not a release at all — no binary at its root.
stage="$WORK/s3"; mkdir -p "$stage/bundles"
( cd "$stage/bundles" && tar -czf kontra-orchestrator-x.tar.gz -T /dev/null )
publish "$stage"
expect_fail "a tarball with no kontra binary is refused" "no kontra binary at its root"

# 4. A corrupted download: the bytes do not match the digest the release published.
stage="$WORK/s4"; mkdir -p "$stage/bundles"; stub_kontra "$stage/kontra"
( cd "$stage/bundles" && tar -czf kontra-orchestrator-x.tar.gz -T /dev/null )
publish "$stage"
printf 'corrupted' >> "$WORK/serve/$FILE"        # after SHA256SUMS was written
expect_fail "a tarball that does not match its digest is refused" "does not match the digest"

# 5. SHA256SUMS that does not mention this platform's file at all.
publish "$stage"
echo "$(printf 'x%.0s' $(seq 1 64))  kontra_${VER}_plan9_mips.tar.gz" > "$WORK/serve/SHA256SUMS"
expect_fail "a SHA256SUMS not naming this file is refused" "does not mention"

# 6. THE REGEX HAZARD. A version is nothing but digits and dots, and every `.` in an unanchored
#    grep pattern is a wildcard — so `kontra_0.0.0-test_linux_amd64.tar.gz` as a REGEX also matches
#    a line for `kontra_0X0Y0-test_linux_amd64Ztar.gz`. If the installer picks that line it checks
#    the wrong file's digest and reports a mismatch on a download that was fine. `grep -F` is why
#    this passes; the case exists so the day somebody "simplifies" it back, this says what broke.
#
#    THE ASSERTION HAS TO NAME A LATER STAGE, not merely the absence of the digest complaint. A
#    404, a missing server, any earlier failure at all would also "not say does not match the
#    digest" — so an absence-only check passes for every reason except the one it is about, which
#    is the vacuous-guard shape this repo keeps finding in its own tests. What proves the grep
#    worked is that execution REACHED the bundle check, and with a stub bundle that stage has its
#    own distinct refusal.
publish "$stage"
real=$(cd "$WORK/serve" && sha256sum "$FILE" | cut -d' ' -f1)
decoy="kontra_$(printf '%s' "$VER" | tr '.' 'X')_${OS}_${ARCH}Xtar.gz"
{ printf '%s  %s\n' "$(printf 'a%.0s' $(seq 1 64))" "$decoy"
  printf '%s  %s\n' "$real" "$FILE"; } > "$WORK/serve/SHA256SUMS"
rm -rf "$WORK/home" "$WORK/bin"
out=$(run_installer)
case "$out" in
  *"does not match the digest"*)
    bad "a dotted filename is matched literally, not as a regex" \
        "it matched the decoy line and blamed the download" ;;
  *"does not match its own manifest"*)
    ok "a dotted filename is matched literally, not as a regex" ;;
  *)
    bad "a dotted filename is matched literally, not as a regex" \
        "never reached the bundle check, so the checksum step proves nothing here:
     $(printf '%s' "$out" | tail -2)" ;;
esac

# 7. No release published at all, which is this repo's state today. GitHub's `latest` also excludes
#    pre-releases, so the same 404 covers two very different situations and the message has to say
#    what to do about both.
rm -rf "$WORK/home" "$WORK/bin"
out=$(env KONTRA_REPO="medmahmoudi26/definitely-not-a-repo-$$" \
          KONTRA_HOME="$WORK/home" KONTRA_BIN="$WORK/bin" sh "$SCRIPT" 2>&1)
case "$out" in
  *"KONTRA_VERSION"*) ok "no published release says how to name one instead" ;;
  *) bad "no published release says how to name one instead" "$(printf '%s' "$out" | tail -2)" ;;
esac

echo
echo "  $pass passed, $fail failed"
[ "$fail" -eq 0 ]
