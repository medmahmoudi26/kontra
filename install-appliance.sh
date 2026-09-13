#!/usr/bin/env sh
# Install the kontra appliance: one command, one file, no Docker (ADR 0031, issue 17).
#
#   curl -fsSL https://raw.githubusercontent.com/medmahmoudi26/kontra/main/install-appliance.sh | sh
#
# NOT install.sh. That one sets up a CHECKOUT for development — a venv, an editable SDK, the buf
# toolchain, proto codegen — and it needs Go, Docker and a clone. This one installs a RELEASE: one
# tarball for your platform, holding the `kontra` binary, the orchestrator bundle it carries and
# the browser bundle it serves. Nothing is compiled and nothing is cloned.
#
# WHAT IT VERIFIES, AND WHAT IT DOES NOT. Every download is checked against the SHA256SUMS file
# published with that exact release, and the release is named by a TAG rather than by
# `/releases/latest/download/` — the shape this repo has already written down as a mistake, because
# a URL with no version has nothing to state a checksum about. Be clear about the limit of that:
# the digests come from the release you are installing, so they detect a corrupted or truncated
# download and a mirror serving different bytes. They are not a signature and they do not establish
# that the release itself is one you should trust; that is what pinning KONTRA_VERSION to a tag you
# have decided on is for.
#
#   KONTRA_VERSION=v0.2.0   install that tag rather than resolving the newest one
#   KONTRA_HOME=~/.kontra   where the bundles go (default ~/.kontra)
#   KONTRA_BIN=/usr/local/bin   where `kontra` goes
#   KONTRA_BASE_URL=...     where the release files live (default: this repo's GitHub release)
#
# KONTRA_BASE_URL EXISTS SO THIS FILE CAN BE RUN BEFORE IT IS PUBLISHED. Every other path here was
# unreachable until a tag existed, which meant the installer's own correctness could only be learned
# from the first person to use it — and an installer is the one program whose first run is always on
# a machine you do not control. Point it at a directory served over HTTP holding a
# `kontra_<v>_<os>_<arch>.tar.gz` and a `SHA256SUMS`, and every step below runs unchanged. That is
# how it is tested, and the test is the reason the variable is here rather than a convenience.
set -eu

# THE PUBLIC REPOSITORY, WHICH IS WHERE THE RELEASES ARE. This said `kontra-local` — the private
# development repository — which has no releases and never will: a stranger who ran the line above
# got a 404 about a repository they could not have seen existed either way. `KONTRA_REPO` is still
# here for a mirror or a fork.
REPO="${KONTRA_REPO:-medmahmoudi26/kontra}"
KONTRA_HOME="${KONTRA_HOME:-$HOME/.kontra}"
KONTRA_BIN="${KONTRA_BIN:-/usr/local/bin}"

say() { printf '%s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

need() {
  command -v "$1" >/dev/null 2>&1 || die "$1 is required and is not on PATH"
}

need curl
need tar

# --- which machine is this ---------------------------------------------------------------------
#
# THE SAME FOUR THE BUILD PINS, in the same vocabulary. `uname` answers in its own words
# (`x86_64`, `Darwin`), the release files are named in Go's (`amd64`, `darwin`), and the mapping
# lives here in one place. An unrecognised machine is refused BY NAME rather than left to fail on a
# 404, because "no such file" says nothing about which of the four you needed.
os=$(uname -s)
arch=$(uname -m)
case "$os" in
  Linux)  goos=linux ;;
  Darwin) goos=darwin ;;
  *) die "kontra ships linux and macOS; this is $os" ;;
esac
case "$arch" in
  x86_64|amd64)  goarch=amd64 ;;
  arm64|aarch64) goarch=arm64 ;;
  *) die "kontra ships amd64 and arm64; this is $arch" ;;
esac

# --- which release -------------------------------------------------------------------------------
#
# RESOLVED ONCE AND THEN PINNED. `KONTRA_VERSION` names a tag outright. Without one, the newest
# release is looked up through the API and the ANSWER is used for every URL below — so the tarball
# and the checksums come from one release even if a new one is published mid-install. A
# `/releases/latest/download/` URL cannot promise that.
version="${KONTRA_VERSION:-}"
if [ -z "$version" ]; then
  say "resolving the newest kontra release"
  # `/releases/latest` DOES NOT MEAN "the newest tag": GitHub excludes drafts and pre-releases from
  # it, so a repo whose only releases are pre-releases answers 404 here and a repo with none answers
  # 404 too. Both are the same sentence to a reader — "there is nothing to install" — and both are
  # fixed the same way, so the message says how rather than only what.
  version=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" 2>/dev/null \
    | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1) || true
  [ -n "$version" ] || die "no published release found for $REPO.
  If the only releases are pre-releases, \`latest\` does not see them — name one:
      KONTRA_VERSION=v0.2.0 sh install-appliance.sh"
fi
bare=${version#v}

base="${KONTRA_BASE_URL:-https://github.com/$REPO/releases/download/$version}"
file="kontra_${bare}_${goos}_${goarch}.tar.gz"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

say "kontra $version for $goos/$goarch"
curl -fSL --progress-bar -o "$tmp/$file" "$base/$file" \
  || die "no $file in release $version — that platform was not published for this release"
curl -fsSL -o "$tmp/SHA256SUMS" "$base/SHA256SUMS" \
  || die "release $version publishes no SHA256SUMS; refusing to install bytes nothing describes"

# --- verify before unpacking ----------------------------------------------------------------------
#
# BEFORE, not after. An archive is a program's worth of files, and checking it once it is already
# on disk is checking something that has already happened.
# `grep -F`, BECAUSE THE FILENAME IS FULL OF DOTS. `kontra_0.2.0_linux_amd64.tar.gz` as a REGEX
# matches `kontra_0X2Y0_linux_amd64Ztar.gz` too — every `.` is a wildcard. That is not a hypothetical
# on a line whose whole job is to pick one filename out of a list of four: a version string is
# nothing but digits and dots, and the wrong line here means checking the wrong file's digest.
# The trailing anchor still has to be a regex, so it is a second grep rather than a cleverer one.
say "verifying $file"
( cd "$tmp" && grep -F " $file" SHA256SUMS | grep "[[:space:]]$file\$" > SHA256SUMS.one ) \
  || die "SHA256SUMS in release $version does not mention $file"
[ -s "$tmp/SHA256SUMS.one" ] || die "SHA256SUMS in release $version does not mention $file"
if command -v sha256sum >/dev/null 2>&1; then
  ( cd "$tmp" && sha256sum -c SHA256SUMS.one ) || die "$file does not match the digest the release published"
elif command -v shasum >/dev/null 2>&1; then
  # macOS has no sha256sum. `shasum -a 256 -c` reads the same format.
  ( cd "$tmp" && shasum -a 256 -c SHA256SUMS.one ) || die "$file does not match the digest the release published"
else
  die "neither sha256sum nor shasum is available; refusing to install an unverified download"
fi

# --- place it ---------------------------------------------------------------------------------
#
# THE LAYOUT IS NOT A CHOICE MADE HERE. `$KONTRA_HOME/bundles` is where the binary's own
# `findBundles` looks when there is no checkout, so installing is a copy into a path the program
# already knows and never an environment variable somebody has to be told about.
mkdir -p "$tmp/unpacked" "$KONTRA_HOME/bundles"
tar -xzf "$tmp/$file" -C "$tmp/unpacked"

# A GLOB THAT MATCHES NOTHING IS A STRING, NOT AN ERROR, and `sh` will hand that string to the next
# command as though it were a filename. This repo has been bitten by it before, which is why the
# shape is checked rather than assumed: a release whose `bundles/` is empty would otherwise reach
# `cp` as a literal `*.tar.gz`, fail with a message about a file that was never meant to exist, and
# say nothing about the actual fault — that the tarball is not a release.
[ -f "$tmp/unpacked/kontra" ] || die "$file has no kontra binary at its root; this is not a release tarball"
set -- "$tmp/unpacked"/bundles/*.tar.gz
[ -f "$1" ] || die "$file carries no bundles/; this is not a release tarball"
cp "$@" "$KONTRA_HOME/bundles/"
if [ -d "$tmp/unpacked/manifests" ]; then
  mkdir -p "$KONTRA_HOME/manifests"
  cp "$tmp/unpacked"/manifests/*.json "$KONTRA_HOME/manifests/" 2>/dev/null || true
fi

# sudo ONLY IF THE DIRECTORY NEEDS IT, and never silently: an installer that reaches for root
# because it always has is an installer nobody can run without deciding to trust it first.
# `-w` ON A DIRECTORY THAT DOES NOT EXIST IS FALSE, which reads identically to "exists and is not
# writable" and sends this to sudo — where `install` then fails on the missing directory rather than
# on permissions. A minimal container image is exactly where `/usr/local/bin` is absent, and exactly
# where somebody is most likely to be running this for the first time.
if [ ! -d "$KONTRA_BIN" ]; then
  mkdir -p "$KONTRA_BIN" 2>/dev/null \
    || { command -v sudo >/dev/null 2>&1 && sudo mkdir -p "$KONTRA_BIN"; } \
    || true
fi
if [ -w "$KONTRA_BIN" ]; then
  install -m 0755 "$tmp/unpacked/kontra" "$KONTRA_BIN/kontra"
elif [ -d "$KONTRA_BIN" ] && command -v sudo >/dev/null 2>&1; then
  say "$KONTRA_BIN is not writable; using sudo to install the binary there"
  sudo install -m 0755 "$tmp/unpacked/kontra" "$KONTRA_BIN/kontra"
else
  KONTRA_BIN="$KONTRA_HOME/bin"
  mkdir -p "$KONTRA_BIN"
  install -m 0755 "$tmp/unpacked/kontra" "$KONTRA_BIN/kontra"
  say ""
  say "installed to $KONTRA_BIN — add it to PATH:"
  say "    export PATH=\"\$PATH:$KONTRA_BIN\""
fi

say ""
say "kontra $version installed"
say "  binary   $KONTRA_BIN/kontra"
say "  bundles  $KONTRA_HOME/bundles"

# THE BUNDLE VERIFIED BY NAME, NOT BY GLOB. `$KONTRA_HOME/bundles` may already hold bundles from a
# previous version — that is the point of a digest-named store — and `kontra bundle verify` takes
# exactly one archive, so a glob over that directory is an install that starts failing the day
# somebody upgrades. The names come from the tarball that was just unpacked.
#
# AND A FAILURE HERE IS FATAL. The SHA256SUMS check above proved the DOWNLOAD arrived intact; this
# opens it and re-derives every digest the manifest inside it claims, which is a different question
# and the one that catches a release built from a tree nobody checked.
# AND THE SAME EMPTY-GLOB RULE AS ABOVE, for a sharper reason: an unmatched pattern here does not
# fail, it VERIFIES NOTHING and says "verified". A loop over no bundles is the exact shape of a
# check that reports success by having had no work to do, so the count is asserted rather than the
# loop trusted.
set -- "$tmp/unpacked"/bundles/kontra-orchestrator-*.tar.gz
[ -f "$1" ] || die "$file carries no orchestrator bundle; there is nothing for \`kontra up\` to run"
for b in "$@"; do
  name=$(basename "$b")
  say "  checking $name (re-deriving every digest in its manifest)"
  "$KONTRA_BIN/kontra" bundle verify "$KONTRA_HOME/bundles/$name" >/dev/null \
    || die "$name does not match its own manifest; the release is not what it says it is"
  say "  bundle   $name verified"
done

say ""
say "start it:"
say "    kontra up"
