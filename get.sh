#!/usr/bin/env sh
# get.sh — the install, in one line (ADR 0052 §3, issue 09).
#
#   curl -fsSL https://raw.githubusercontent.com/medmahmoudi26/kontra/main/get.sh | sh
#   kontra up
#
# Served out of the repository rather than a domain, so it works the day it lands: no DNS, no TLS,
# nothing to host. `get.kontra.run` becomes a redirect to this file and changes nothing here.
#
# THERE ARE THREE INSTALLER-SHAPED FILES AT THIS ROOT AND THIS IS THE ONE FOR A STRANGER.
#   install.sh            sets up a CHECKOUT for development: `.venv`, an editable SDK, the buf
#                         toolchain, proto codegen, `go build`. Needs Go, Node, pnpm and a clone.
#                         Nothing here touches it and it still does what it did.
#   install-appliance.sh  installs the RELEASE for the appliance that ADR 0052 §2 removes. THIS FILE
#                         SUPERSEDES IT. Issue 08 deletes it; until then the root carries two
#                         one-line installers, which is one too many. Its mechanics are reused here
#                         deliberately — the tarball name, the `grep -F` before `sha256sum -c`, the
#                         sudo ladder, `KONTRA_BASE_URL` — because those were right answers to
#                         questions that have not changed.
#   get.sh                this one. Resolves a released `kontra` for this platform, resolves or
#                         installs `pulumi`, checks Docker, and then gets out of the way.
#
# WHY PULUMI IS HERE AT ALL. `kontra up` is no longer a compose file; it converges the Pulumi program
# at `control/pulumi/Pulumi.yaml` — 13 containers, 11 volumes, one network — by shelling to the host
# `pulumi` (ADR 0052 §1-§3). That program is YAML, which resolves providers as binary plugins and
# needs no Node and no `@pulumi/*` package on the host (ADR 0052 fact 2) — but it does need the
# `docker` resource plugin, which is NOT part of a stock Pulumi install. `control/pulumi/README.md`
# assigns that step to this file: "`get.sh` installs it; `kontra doctor` reports it."
#
# IT REFUSES RATHER THAN FALLING BACK, and every refusal names a failure rather than a fact: an
# unsupported platform, a missing Docker, a Docker daemon that does not answer, a Pulumi older than
# the one the program was measured against, a Pulumi configuration that would reach Pulumi Cloud, a
# download whose digest does not match, a binary that does not report the version it was supposed to
# be. They are collected and reported together, and all of them are decided BEFORE anything is
# written to disk — a partial install that mostly works is the outcome this script exists to prevent.
#
# INTERACTIVE ON A TTY, STRICTLY NON-INTERACTIVE WITHOUT ONE, which is what makes the quickstart path
# and the CI path the same path (ADR 0047 §4's rule, kept by 0052 §3). The only question it ever asks
# is whether to install Pulumi, it is asked once, before any work, and with no terminal the answer is
# yes without asking. `KONTRA_BOOTSTRAP=no` — install.sh's spelling, the same meaning — reports the
# bill and installs no prerequisite.
#
# WHAT THE CHECKSUMS PROVE AND WHAT THEY DO NOT (install-appliance.sh:11-18, unchanged and worth
# repeating). Every download is checked against a digest published with that exact release, and the
# release is named by a VERSION rather than by `/releases/latest/download/` — the shape this repo has
# twice written down as a mistake (release.yml:13-15, cli/release.go:22-26), because a URL with no
# version has nothing to state a checksum about. The limit: those digests come from the same host as
# the bytes, so they catch a truncated download and a mirror serving something else. They are not a
# signature. `KONTRA_VERIFY_SIGNATURE=1` is the stronger check and is opt-in below.
#
#   KONTRA_VERSION=v0.2.0        install that tag rather than resolving the newest release
#   KONTRA_HOME=~/.kontra        bundles, manifests and (later) the Pulumi state directory
#   KONTRA_BIN=/usr/local/bin    where `kontra` goes
#   KONTRA_BASE_URL=...          where the release files live — see below
#   KONTRA_REPO=owner/name       a fork or a mirror
#   KONTRA_BOOTSTRAP=no          report the prerequisites, install none of them
#   KONTRA_PULUMI_VERSION=3.x.y  which Pulumi to install if one is needed
#   KONTRA_PULUMI_BASE_URL=...   where the Pulumi release files live
#   KONTRA_VERIFY_SIGNATURE=1    additionally verify the release's Sigstore bundles (needs cosign)
#   KONTRA_FORCE=1               re-download even when the wanted version is already installed
#
# KONTRA_BASE_URL EXISTS SO THIS FILE CAN BE RUN BEFORE IT IS PUBLISHED, which is the same reasoning
# install-appliance.sh:25-33 gives and it still holds: `git tag` in this repository returns zero tags
# today, so every URL below is unreachable until the first one exists, and an installer is the one
# program whose first run is always on a machine you do not control. Point it at any directory served
# over HTTP holding a `kontra_<version>_<os>_<arch>.tar.gz` and a `SHA256SUMS` and every step runs
# unchanged. KONTRA_PULUMI_BASE_URL is there for the same reason and one more: it is how the Pulumi
# half gets exercised without a 125 MB download.
set -eu

REPO="${KONTRA_REPO:-medmahmoudi26/kontra}"
KONTRA_HOME="${KONTRA_HOME:-$HOME/.kontra}"
KONTRA_BIN="${KONTRA_BIN:-/usr/local/bin}"

# THE PULUMI THIS PROGRAM WAS MEASURED AGAINST, and the floor it may not go under.
# `control/pulumi/README.md` records ">= 3.244.0 measured here"; `infra/workspace.ts:13` names 3.244.0
# and 3.256.0 as the two the backend findings were established on. 3.256.0 is installed rather than
# the floor because it is also what `control/orchestrator/package.json` pins for the OTHER engine
# (`@pulumi/pulumi` ^3.256.0), so a fresh machine starts with the host-CLI-vs-SDK skew that
# ADR 0052's Consequences make a real axis at zero rather than at twelve minor versions.
PULUMI_VERSION="${KONTRA_PULUMI_VERSION:-3.256.0}"
PULUMI_FLOOR=3.244.0

# 4.11.2 AND NOT `latest`. It is the version ADR 0052 fact 1 was established against, the version
# `control/orchestrator/package.json` pins for the infra engine, and the version
# `control/pulumi/Pulumi.yaml` pins on an explicit `pulumi:providers:docker` resource in 38 places so
# that both engines agree about what a `Container` is. A floating plugin here is a topology whose
# health gating changes under it.
DOCKER_PLUGIN_VERSION=4.11.2

# THE SIGNING WORKFLOW, SPELLED ONCE. The Fulcio SAN on a keyless GitHub Actions signature is
# `https://github.com/$GITHUB_WORKFLOW_REF`, which `.github/workflows/publish.yml:618` derives rather
# than types. This side cannot derive it — there is no `GITHUB_WORKFLOW_REF` on an operator's machine
# — so it is reconstructed, and it is a variable because a rename of that workflow would otherwise
# turn every signature into a mismatch, which is the worst way for an installer to be wrong.
SIGNING_WORKFLOW="${KONTRA_SIGNING_WORKFLOW:-.github/workflows/publish.yml}"
SIGNING_ISSUER="${KONTRA_SIGNING_ISSUER:-https://token.actions.githubusercontent.com}"

# workspace.ts:75 sets this on the container engine and the reason applies here: an update-check line
# in the middle of an install is noise, and it is one network call this script does not need.
PULUMI_SKIP_UPDATE_CHECK=true
export PULUMI_SKIP_UPDATE_CHECK

say()  { printf '%s\n' "$*"; }
step() { printf '\n==> %s\n' "$*"; }
die()  { printf 'error: %s\n' "$*" >&2; exit 1; }

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

# --- is anybody there -----------------------------------------------------------------------------
#
# INTERACTIVITY IS DECIDED ON STDOUT AND READ FROM /dev/tty, and both halves are load-bearing.
# MEASURED, on dash 0.5.12 and on `bash --posix`, under the documented `curl … | sh`:
#
#     piped to sh, under a pty:   fd0=not-tty  fd1=tty      fd2=tty
#     piped to sh, no pty:        fd0=not-tty  fd1=not-tty  fd2=not-tty
#
# So STDIN IS THE SCRIPT. `[ -t 0 ]` is false in a terminal on the one path this file exists to be
# run on, which means a prompt gated on it never fires for the reader it was written for — and a
# `read` from stdin would eat the rest of this file instead of an answer.
#
# AND THE OPENABILITY PROBE MUST BE IN A SUBSHELL. `{ : < /dev/tty; } 2>/dev/null` on a machine with
# no controlling terminal does not return false: POSIX says a redirection error on a simple command
# ends a non-interactive shell, and it does — measured under `setsid`, dash exits 2 and `bash
# --posix` exits 1, at that line, with `2>/dev/null` swallowing the only clue. The symptom is an
# installer that prints one line and stops on exactly the machines this probe is for: a container, a
# CI runner, a cron job. `( … )` contains the exit.
interactive=no
if [ -t 1 ] && ( : < /dev/tty ) 2>/dev/null; then
  interactive=yes
fi

# Asked on the terminal, never on stdin, for the reason above. Only ever called when interactive=yes.
# EOF answers the same way an empty line does, which is the same way no terminal at all does: yes.
# Three routes to one behaviour beats three behaviours.
ask() {
  printf '%s [Y/n] ' "$1" > /dev/tty
  ask_answer=''
  read -r ask_answer < /dev/tty || ask_answer=''
  case "$ask_answer" in
    '' | y | Y | yes | YES | Yes) return 0 ;;
    *) return 1 ;;
  esac
}

# --- the refusal ledger ---------------------------------------------------------------------------
#
# COLLECTED, NOT DIED ON, and this is install.sh:27-42's lesson carried over: "find everything
# missing at once … a first run that breaks the second is worse than one that refuses to start." A
# machine with no Docker AND an ambient Pulumi Cloud token should learn both facts in one run, not in
# two. Every entry is added before anything is written, so the report can end by saying that nothing
# was installed — which is the sentence that makes a refusal cheap to act on.
refusals=0
refuse() {
  refusals=$((refusals + 1))
  printf '\n  %s. %s\n' "$refusals" "$1" >> "$tmp/refusals"
}

need() { command -v "$1" >/dev/null 2>&1; }

# 3.244.0 vs 3.256.0 WITHOUT `sort -V`. install.sh:53 can use it because it is a Debian script;
# macOS `sort` is BSD and has no -V, and this file has to answer the same question there. Three
# integer comparisons on a padded triple, in parameter expansion, no subprocess. A component that is
# not a number returns 2 rather than comparing as zero, because a Pulumi that reports something
# unparseable must be NAMED and not quietly treated as ancient.
#
# Answers into vs_a/vs_b/vs_c rather than one joined string: a joined string has to be re-split, and
# an unquoted re-split is the one thing in this file that would be splitting on purpose in a place
# where every other expansion must not.
vs_a=0; vs_b=0; vs_c=0
version_split() {
  vsv=${1#v}
  vsv=${vsv%%-*}                       # 3.256.0-alpha.1 -> 3.256.0
  vsv=${vsv%%+*}                       # 3.256.0+dirty   -> 3.256.0
  vsv="$vsv.0.0"                       # `3` and `3.2` answer as 3.0.0 and 3.2.0
  vs_a=${vsv%%.*}; vsv=${vsv#*.}
  vs_b=${vsv%%.*}; vsv=${vsv#*.}
  vs_c=${vsv%%.*}
  for vsp in "$vs_a" "$vs_b" "$vs_c"; do
    case "$vsp" in
      '' | *[!0-9]*) return 2 ;;
    esac
  done
}

# version_at_least HAVE WANT -> 0 if HAVE >= WANT, 1 if older, 2 if either is unparseable.
version_at_least() {
  version_split "$1" || return 2
  val_a=$vs_a; val_b=$vs_b; val_c=$vs_c
  version_split "$2" || return 2
  [ "$val_a" -gt "$vs_a" ] && return 0
  [ "$val_a" -lt "$vs_a" ] && return 1
  [ "$val_b" -gt "$vs_b" ] && return 0
  [ "$val_b" -lt "$vs_b" ] && return 1
  [ "$val_c" -lt "$vs_c" ] && return 1
  return 0
}

# --- which machine is this ------------------------------------------------------------------------
#
# THREE VOCABULARIES FOR THE SAME TWO CHIPS, and the mapping lives here once. `uname` answers
# `x86_64` and `Darwin`; the kontra release files are named in Go's words, `amd64` and `darwin`
# (cli/release.go:222); Pulumi's own assets say `x64` — measured on the v3.256.0 release, which
# publishes `pulumi-v3.256.0-linux-x64.tar.gz` and `pulumi-v3.256.0-darwin-arm64.tar.gz`. A mapping
# that gets two of the three right fails as a 404 from the third, and "no such file" says nothing
# about which of the four platforms you needed.
#
# THIS ONE IS FATAL IMMEDIATELY rather than collected: with no platform there is no download to
# check the rest against, and the four are the four `cli/appliance/bundle/pins.go:146-153` pins.
os=$(uname -s)
arch=$(uname -m)
case "$os" in
  Linux)  goos=linux ;;
  Darwin) goos=darwin ;;
  *) die "kontra ships linux and macOS; this is $os" ;;
esac
case "$arch" in
  x86_64 | amd64)  goarch=amd64; pulumi_arch=x64 ;;
  arm64 | aarch64) goarch=arm64; pulumi_arch=arm64 ;;
  *) die "kontra ships amd64 and arm64; this is $arch" ;;
esac

step "kontra installer — $goos/$goarch, $( [ "$interactive" = yes ] && echo 'interactive' || echo 'non-interactive' )"

# --- preflight: the tools this file itself needs ---------------------------------------------------
for tool in curl tar; do
  need "$tool" || refuse "$tool is required and is not on PATH."
done
# macOS ships `shasum` and no `sha256sum`; Linux ships `sha256sum`. Both read `sha256sum -c` format,
# so one line of the file feeds either. This is not a refusal about convenience: with no digest
# checker there is no verified install to fall back to, so there is nothing to continue with.
# A FUNCTION RATHER THAN A COMMAND IN A VARIABLE, so the two-word form never has to be expanded
# unquoted — the shape that works until somebody's directory has a space in it.
if need sha256sum; then
  sha_verify() { sha256sum -c "$1"; }
elif need shasum; then
  sha_verify() { shasum -a 256 -c "$1"; }
else
  sha_verify() { die "internal: no sha256 checker; preflight should have refused this run"; }
  refuse "neither sha256sum nor shasum is on PATH, so a download cannot be verified.
     Refusing to install bytes nothing describes."
fi

# --- preflight: Docker ----------------------------------------------------------------------------
#
# THE CHECK IS THAT A DAEMON ANSWERED, NOT THAT THE CLI EXISTS, and the difference is measured.
# On docker 24.0.5 with DOCKER_HOST pointed at a socket that does not exist:
#
#     docker version --format '{{.Server.Version}}'   exit=1  stdout empty
#     docker info    --format '{{.ServerVersion}}'    exit=0  stdout empty
#
# So the `info` spelling of this check PASSES on a machine with no daemon. That is the shape of a
# check that reports success by never having asked the question, and it is why this asks with
# `version` and then also requires the answer to be non-empty: the exit code and the answer are two
# facts, and across Docker versions only one of them has been stable.
#
# NOT `docker compose`. The compose file is what ADR 0052 §2 replaces — `kontra up` converges a
# Pulumi program against the Docker API — so compose is deliberately not a prerequisite any more,
# and requiring it here would be requiring the thing that was removed.
docker_server=''
if ! need docker; then
  refuse "Docker is required and is not on PATH.
     kontra's control plane is 13 containers; there is no in-process mode to fall back to.
         Linux   https://docs.docker.com/engine/install/
         macOS   https://docs.docker.com/desktop/install/mac-install/
     Then re-run this installer."
else
  docker_server=$(docker version --format '{{.Server.Version}}' 2>"$tmp/docker.err") || docker_server=''
  if [ -z "$docker_server" ]; then
    if grep -qi 'permission denied' "$tmp/docker.err" 2>/dev/null; then
      # The one Docker failure whose fix is not "start Docker", and the one a first-time reader is
      # most likely to hit on their own Linux box.
      refuse "the Docker CLI is installed but this user may not talk to the daemon:
$(sed 's/^/       /' "$tmp/docker.err")
     On Linux that is group membership, and it needs a new login to take effect:
         sudo usermod -aG docker \"\$USER\" && newgrp docker"
    else
      refuse "the Docker CLI is installed but no daemon answered:
$(sed 's/^/       /' "$tmp/docker.err")
     Start Docker (\`sudo systemctl start docker\`, or open Docker Desktop) and re-run this."
    fi
  fi
fi

# --- preflight: a Pulumi that would reach Pulumi Cloud --------------------------------------------
#
# THE SHARPEST TRAP IN THE TOOL, AND IT IS NOT HYPOTHETICAL ON A CLEAN MACHINE.
# `infra/workspace.ts:16` records it for the container's engine: "A failed `pulumi login` does not
# stop the CLI — it silently creates an ephemeral Pulumi Cloud account and deploys THERE, state
# included." ADR 0052 fact 4 asks for the same check on the host. Re-measured for this file on the
# host CLI 3.244.0, in a non-interactive shell, with an empty PULUMI_HOME and NO token of any kind:
#
#     warning: failed to get user account details: this command requires logging in
#     PULUMI_EPHEMERAL_AGENT_ACCOUNT
#     CLAIM_URL=https://app.pulumi.com/claim/01a0dee8-…
#     EPHEMERAL_ACCOUNT_ACCESS_EXPIRES_IN=2d23h59m
#
# An account was created, server-side, out of nothing. So the trap needs no ambient token to spring,
# and the only thing that keeps a converge local is an explicit backend — `kontra up`'s step 1,
# `pulumi login file://$KONTRA_HOME/state` (control/pulumi/README.md, "What `kontra up` will run
# against it"). This script never logs in: it is not its state directory to create, and rewriting an
# operator's current backend on the way past would be an installer deciding something it was not
# asked about.
#
# WHAT IT CHECKS INSTEAD is the three ambient settings that survive that login or beat it, each
# measured on 3.244.0 with `pulumi whoami --verbose`:
#
#   PULUMI_BACKEND_URL wins over everything. With a hosted URL exported, step 1 cannot save the
#     converge. With `file://…` exported AND a bogus PULUMI_ACCESS_TOKEN set, `whoami` reported
#     `Backend URL: file:///…` — the URL beat the token.
#   PULUMI_ACCESS_TOKEN with no URL set goes straight to the cloud: "Logging in using access token
#     from PULUMI_ACCESS_TOKEN" then `Unauthorized` for a bogus one. Its real danger is second
#     order: it turns a FAILED step 1 from a stop into a silent success somewhere else.
#   credentials.json's `current` is what a machine that is logged in to Pulumi Cloud reads back, and
#     it is what a hand-run `pulumi -C control/pulumi up` — the invocation in that README — would
#     use. Only a hosted URL is refused here; a DIY backend belongs to whatever else the operator
#     does with Pulumi (this box's own `current` is `s3://kontra-pulumi?endpoint=localhost:8333`).
#
# file:// and s3:// are the accepted pair because those are the two `workspace.ts:93` accepts, and a
# second, different policy for the same question is how two engines start disagreeing.
pulumi_home="${PULUMI_HOME:-$HOME/.pulumi}"
backend_url="${PULUMI_BACKEND_URL:-}"
if [ -n "$backend_url" ]; then
  case "$backend_url" in
    file://* | s3://*) ;;
    *) refuse "PULUMI_BACKEND_URL is \`$backend_url\`, which is not a self-managed backend.
     An exported backend URL beats every other setting, including a later \`pulumi login\`, so
     \`kontra up\` would converge kontra's whole control plane there — 13 containers and their
     state. workspace.ts:93 accepts file:// and s3:// and refuses the rest for the same reason.
         unset PULUMI_BACKEND_URL
         # or point it at kontra's own state directory:
         export PULUMI_BACKEND_URL=file://$KONTRA_HOME/state" ;;
  esac
elif [ -n "${PULUMI_ACCESS_TOKEN:-}" ]; then
  refuse "PULUMI_ACCESS_TOKEN is set in this environment and no PULUMI_BACKEND_URL is.
     That is the configuration \`pulumi\` resolves to Pulumi Cloud, and it is what turns a failed
     \`pulumi login file://$KONTRA_HOME/state\` from an error into a converge into somebody's
     hosted account — state included. See workspace.ts:16 and ADR 0052 fact 4.
         env -u PULUMI_ACCESS_TOKEN sh get.sh
         # or state the backend, which wins over the token (measured):
         export PULUMI_BACKEND_URL=file://$KONTRA_HOME/state"
elif [ -f "$pulumi_home/credentials.json" ]; then
  # The same `sed` shape install-appliance.sh:84 uses to read a tag out of an API response: one
  # field, no JSON parser, and `[^"]*` so an escaped `\u0026` in a query string survives intact.
  pulumi_current=$(sed -n 's/.*"current"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' \
    "$pulumi_home/credentials.json" 2>/dev/null | head -1) || pulumi_current=''
  case "$pulumi_current" in
    http://* | https://*)
      refuse "this machine is logged in to a hosted Pulumi backend:
       $pulumi_home/credentials.json  current = $pulumi_current
     \`kontra up\` logs in to file://$KONTRA_HOME/state before it converges, but anything run by
     hand out of control/pulumi/ would go to that account instead — and per workspace.ts:16 a
     login that fails does not stop the CLI. Point Pulumi at kontra's state, or say so explicitly
     for this shell:
         pulumi login file://$KONTRA_HOME/state
         # or:
         export PULUMI_BACKEND_URL=file://$KONTRA_HOME/state" ;;
  esac
fi

# --- preflight: is there a usable pulumi, and if not, may we install one ---------------------------
#
# A VERSION CHECK, NOT A PRESENCE CHECK — install.sh:45-47's reason, in a second tool: "a distro Go
# that cannot build the modules is a harder failure to read than no Go at all — it fails later,
# inside a build, naming a language feature instead of a toolchain." A Pulumi below the floor fails
# inside a converge, naming a resource property, after ten containers already exist.
pulumi_bin=''
pulumi_have=''
install_pulumi=no
if need pulumi; then
  pulumi_bin=$(command -v pulumi)
  pulumi_have=$("$pulumi_bin" version 2>/dev/null | head -1 | tr -d '\r') || pulumi_have=''
  # `cmd || var=$?` AND NOT A BARE CALL. Under `set -e` a function that returns non-zero as a
  # statement of its own ends the script, so the three-way answer has to be captured on the left of
  # an `||`, where errexit does not apply.
  pulumi_cmp=0
  version_at_least "$pulumi_have" "$PULUMI_FLOOR" || pulumi_cmp=$?
  case "$pulumi_cmp" in
    0) : ;;
    1) refuse "pulumi $pulumi_have is at $pulumi_bin and the control program needs >= $PULUMI_FLOOR.
     control/pulumi/README.md records that floor as measured, and \`waitTimeout\` — the property
     every health gate in the program sets — is the kind of thing an older provider API ignores
     rather than rejects. Upgrade it, or take it off PATH and let this installer place $PULUMI_VERSION:
         pulumi version                     # what you have
         curl -fsSL https://get.pulumi.com | sh -s -- --version $PULUMI_VERSION" ;;
    *) refuse "\`$pulumi_bin version\` answered \`$pulumi_have\`, which is not a version this can
     compare against $PULUMI_FLOOR. Refusing to guess: a pulumi whose version cannot be read is
     also one whose behaviour cannot be predicted." ;;
  esac
else
  if [ "${KONTRA_BOOTSTRAP:-yes}" = no ]; then
    refuse "pulumi is not on PATH and KONTRA_BOOTSTRAP=no says not to install prerequisites.
     \`kontra up\` shells to it for every converge. Install $PULUMI_VERSION or newer and re-run:
         curl -fsSL https://get.pulumi.com | sh -s -- --version $PULUMI_VERSION"
  else
    install_pulumi=yes
  fi
fi

# cosign is required rather than probed when the signature check is asked for. A verification that
# quietly turns itself off when a tool is absent is the divergence this script is supposed not to
# have: the CI path and the quickstart path would then be doing different amounts of checking while
# printing the same lines.
if [ "${KONTRA_VERIFY_SIGNATURE:-}" = 1 ] && ! need cosign; then
  refuse "KONTRA_VERIFY_SIGNATURE=1 asks for the Sigstore bundles to be verified and cosign is not
     on PATH. Install it (https://docs.sigstore.dev/cosign/installation/) or unset the variable —
     the SHA256SUMS check still runs either way."
fi

# --- the report, and nothing has been written yet -------------------------------------------------
if [ "$refusals" -gt 0 ]; then
  printf '\nget.sh refuses to continue — %s %s:\n' "$refusals" \
    "$( [ "$refusals" = 1 ] && echo problem || echo problems )" >&2
  cat "$tmp/refusals" >&2
  printf '\n  Nothing has been installed. Fix these and run the same line again.\n' >&2
  exit 1
fi

say "  docker   $docker_server"
if [ "$install_pulumi" = yes ]; then
  say "  pulumi   not installed — $PULUMI_VERSION will be placed in $pulumi_home/bin"
else
  say "  pulumi   $pulumi_have ($pulumi_bin)"
fi

# THE ONE QUESTION, ASKED ONCE, BEFORE ANY WORK. With no terminal it is not asked and the answer is
# yes: that is what keeps this file's CI path and its quickstart path the same path (ADR 0052 §3).
# Declining is a refusal and not a partial install — the kontra binary is not placed either, because
# a `kontra` whose `up` cannot run is the "mostly works" outcome this script exists to prevent.
if [ "$install_pulumi" = yes ] && [ "$interactive" = yes ]; then
  say ""
  say "  Pulumi is how \`kontra up\` converges the control plane (ADR 0052 §1)."
  say "  It goes in $pulumi_home/bin — nothing outside your home directory."
  if ! ask "  install pulumi $PULUMI_VERSION?"; then
    die "declined. \`kontra up\` cannot run without pulumi >= $PULUMI_FLOOR, so nothing was installed.
  Install it yourself and re-run this line, or set KONTRA_BOOTSTRAP=no to see the whole bill:
      curl -fsSL https://get.pulumi.com | sh -s -- --version $PULUMI_VERSION"
  fi
fi

# --- which kontra release -------------------------------------------------------------------------
#
# RESOLVED ONCE, THEN PINNED, so the tarball and the digests come from ONE release even if a new one
# is published mid-install — which a `/releases/latest/download/` URL cannot promise.
step "resolving the release"
version="${KONTRA_VERSION:-}"
if [ -z "$version" ]; then
  # `/releases/latest` DOES NOT MEAN "the newest tag": GitHub excludes drafts and pre-releases, so a
  # repository whose only releases are pre-releases answers 404 here, and so does one with none.
  # Both read as "there is nothing to install" and both are fixed by naming a version, so the
  # message says how and not only what. This is also the state of the repository right now — zero
  # tags — which makes this the first thing a reader of this file will see it do.
  version=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" 2>/dev/null \
    | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1) || true
  [ -n "$version" ] || die "no published release found for $REPO.
  If the only releases are pre-releases, \`latest\` does not see them — name one:
      KONTRA_VERSION=v0.2.0 sh get.sh
  Or point this at a directory of release files you have already built:
      KONTRA_VERSION=0.0.0-dev KONTRA_BASE_URL=http://localhost:8000 sh get.sh"
fi

# The tag keeps its `v` (publish.yml creates `v0.1.0`); the FILENAME is bare, because
# cli/release.go:222 builds `kontra_%s_%s_%s.tar.gz` from the bare version. One `#v` in one place.
bare=${version#v}
base="${KONTRA_BASE_URL:-https://github.com/$REPO/releases/download/$version}"
file="kontra_${bare}_${goos}_${goarch}.tar.gz"
say "  kontra $version -> $file"

# --- already installed? ---------------------------------------------------------------------------
#
# IDEMPOTENT BY ASKING THE BINARY, NOT BY LOOKING FOR A FILE. `kontra version` prints
# `buildinfo.Version()` bare (cli/main.go:272) — an exact string, `0.1.0`, for a release build, and
# `dev (abc123, dirty)` for anything else. So string equality answers "is the wanted version already
# here" and also refuses to be satisfied by a checkout build that install.sh left at the same path.
# Both candidate paths are checked because the sudo ladder below may have chosen the second one on
# an earlier run.
#
# AND THE SKIP DOES NOT APPLY WHEN A SIGNATURE CHECK WAS ASKED FOR. This was a real bug in the first
# draft of this file, found by running it: `KONTRA_VERIFY_SIGNATURE=1` on an already-installed
# version took this branch, never downloaded anything, never called cosign — and printed the same
# closing summary as a verified install. A cosign that returns 1 was not detected because cosign was
# never reached. The signature covers the TARBALL, so there is no way to check it without fetching
# one; an operator who asked for that check gets the download.
installed_at=''
for candidate in "$KONTRA_BIN/kontra" "$KONTRA_HOME/bin/kontra"; do
  if [ -x "$candidate" ] && [ "$("$candidate" version 2>/dev/null || true)" = "$bare" ]; then
    installed_at=$candidate
    break
  fi
done
if [ -n "$installed_at" ] && [ "${KONTRA_FORCE:-}" != 1 ] && [ "${KONTRA_VERIFY_SIGNATURE:-}" != 1 ]; then
  say "  $bare is already at $installed_at — skipping the download (KONTRA_FORCE=1 to redo it)"
else
  # --- download and verify before unpacking -------------------------------------------------------
  #
  # BEFORE, not after. An archive is a program's worth of files and checking it once it is already on
  # disk is checking something that has already happened.
  step "downloading kontra $version"
  curl -fSL --progress-bar -o "$tmp/$file" "$base/$file" \
    || die "no $file at $base — that platform was not published for this release"
  curl -fsSL -o "$tmp/SHA256SUMS" "$base/SHA256SUMS" \
    || die "release $version publishes no SHA256SUMS; refusing to install bytes nothing describes"

  # `grep -F`, BECAUSE THE FILENAME IS FULL OF DOTS. `kontra_0.2.0_linux_amd64.tar.gz` as a REGEX
  # also matches `kontra_0X2Y0_linux_amd64Ztar.gz` — every `.` is a wildcard — and a version string
  # is nothing but digits and dots. On a line whose whole job is to pick one filename out of four,
  # the wrong match means verifying a different file's digest and reporting success. The trailing
  # anchor has to be a regex, so it is a second grep rather than a cleverer one.
  step "verifying $file"
  ( cd "$tmp" && grep -F " $file" SHA256SUMS | grep "[[:space:]]$file\$" > SHA256SUMS.one ) \
    || die "SHA256SUMS in release $version does not mention $file"
  [ -s "$tmp/SHA256SUMS.one" ] || die "SHA256SUMS in release $version does not mention $file"
  ( cd "$tmp" && sha_verify SHA256SUMS.one ) \
    || die "$file does not match the digest release $version published"

  # THE SIGNATURE, WHEN IT IS ASKED FOR. The digests above came from the same host as the bytes;
  # this is the check that says who built them. publish.yml signs each tarball and SHA256SUMS
  # keyless and leaves a `.sigstore.json` beside each, so one `verify-blob` per file is the whole
  # protocol. The identity carries the tag — `…/publish.yml@refs/tags/v0.1.0` — so it is rebuilt per
  # release rather than pinned; that is also the gap publish.yml's own header records for the
  # Warden, whose `--certificate-identity` is exact and has no regexp form (trustpolicy.go:489).
  if [ "${KONTRA_VERIFY_SIGNATURE:-}" = 1 ]; then
    # `v$bare` AND NOT `$version`: the ref in the SAN is the git tag, which always carries the `v`
    # (`include-component-in-tag: false` in release-please-config.json makes the tag `v<version>`),
    # while `$version` is whatever was typed — `KONTRA_VERSION=0.1.0` is accepted everywhere else in
    # this file and would otherwise build `@refs/tags/0.1.0`, a ref that has never existed, and
    # report it as a signature mismatch.
    identity="https://github.com/$REPO/$SIGNING_WORKFLOW@refs/tags/v$bare"
    say "  identity $identity"
    for signed in "$file" SHA256SUMS; do
      curl -fsSL -o "$tmp/$signed.sigstore.json" "$base/$signed.sigstore.json" \
        || die "release $version publishes no $signed.sigstore.json.
  A release made before signing landed has no bundles — unset KONTRA_VERIFY_SIGNATURE, or pin a
  KONTRA_VERSION that publish.yml built."
      ( cd "$tmp" && cosign verify-blob \
          --certificate-identity "$identity" \
          --certificate-oidc-issuer "$SIGNING_ISSUER" \
          --bundle "$signed.sigstore.json" "$signed" ) \
        || die "$signed does not verify against $identity.
  If that workflow was renamed, KONTRA_SIGNING_WORKFLOW names the new path. Otherwise these are
  not the bytes that workflow signed, and the install stops here."
      say "  verified $signed"
    done
  fi

  # --- place it ---------------------------------------------------------------------------------
  #
  # A GLOB THAT MATCHES NOTHING IS A STRING, NOT AN ERROR, and `sh` hands that string to the next
  # command as though it were a filename. So the shape of the tarball is asserted rather than
  # assumed — a release whose root has no `kontra` would otherwise fail at `install` with a message
  # about a file that was never meant to exist, and say nothing about the actual fault.
  #
  # BUNDLES ARE COPIED IF THEY ARE THERE AND ARE NOT REQUIRED, which is a change from
  # install-appliance.sh:140 and the reason is dated: that file refused a tarball with no
  # `bundles/` because the appliance WAS the bundle. ADR 0052 §2 removes the appliance, so a release
  # cut after issue 08 may carry the binary alone, and an installer that requires the old shape
  # would refuse the first correct release. `$KONTRA_HOME/bundles` is still where the binary's own
  # `findBundles` looks, so when they are present the install is a copy into a path the program
  # already knows.
  step "installing"
  mkdir -p "$tmp/unpacked" "$KONTRA_HOME"
  tar -xzf "$tmp/$file" -C "$tmp/unpacked"
  [ -f "$tmp/unpacked/kontra" ] \
    || die "$file has no kontra binary at its root; this is not a release tarball"

  bundle_count=0
  set -- "$tmp/unpacked"/bundles/*.tar.gz
  if [ -f "$1" ]; then
    mkdir -p "$KONTRA_HOME/bundles"
    cp "$@" "$KONTRA_HOME/bundles/"
    bundle_count=$#
  fi
  if [ -d "$tmp/unpacked/manifests" ]; then
    mkdir -p "$KONTRA_HOME/manifests"
    cp "$tmp/unpacked"/manifests/*.json "$KONTRA_HOME/manifests/" 2>/dev/null || true
  fi

  # sudo ONLY IF THE DIRECTORY NEEDS IT, and never silently: an installer that reaches for root
  # because it always has is one nobody can run without deciding to trust it first.
  # `-w` ON A DIRECTORY THAT DOES NOT EXIST IS FALSE, which reads identically to "exists and is not
  # writable" and sends this to sudo, where `install` then fails on the missing directory rather
  # than on permissions. A minimal container image is exactly where /usr/local/bin is absent and
  # exactly where somebody is most likely to be running this for the first time.
  #
  # AND `sudo -n` WHEN THERE IS NO TERMINAL. sudo asks for a password on /dev/tty; with no terminal
  # that is not a prompt, it is a hang — in CI, a job that sits there until the 6-hour timeout with
  # no output. `-n` turns the same situation into an immediate failure, which falls through to the
  # home-directory path below with a PATH line to add. That is the one place this file chooses a
  # different location rather than refusing, and it is a choice about WHERE, never about whether the
  # bytes were verified.
  if ! need sudo; then
    as_root() { return 1; }
  elif [ "$interactive" = yes ]; then
    as_root() { sudo "$@"; }
  else
    as_root() { sudo -n "$@"; }
  fi
  if [ ! -d "$KONTRA_BIN" ]; then
    mkdir -p "$KONTRA_BIN" 2>/dev/null || as_root mkdir -p "$KONTRA_BIN" 2>/dev/null || true
  fi
  if [ -w "$KONTRA_BIN" ]; then
    install -m 0755 "$tmp/unpacked/kontra" "$KONTRA_BIN/kontra"
    installed_at="$KONTRA_BIN/kontra"
  elif [ -d "$KONTRA_BIN" ] && as_root true 2>/dev/null; then
    say "  $KONTRA_BIN is not writable; using sudo to install the binary there"
    as_root install -m 0755 "$tmp/unpacked/kontra" "$KONTRA_BIN/kontra"
    installed_at="$KONTRA_BIN/kontra"
  else
    mkdir -p "$KONTRA_HOME/bin"
    install -m 0755 "$tmp/unpacked/kontra" "$KONTRA_HOME/bin/kontra"
    installed_at="$KONTRA_HOME/bin/kontra"
    say "  $KONTRA_BIN was not writable — installed to $KONTRA_HOME/bin instead"
  fi
  say "  binary   $installed_at"

  # THE BUNDLE VERIFIED BY NAME, NOT BY GLOB over $KONTRA_HOME/bundles: that directory may already
  # hold bundles from an earlier version, and `kontra bundle verify` takes exactly one archive. The
  # names come from the tarball that was just unpacked.
  #
  # AND THE ABSENCE IS PRINTED. An unmatched glob here does not fail — it verifies NOTHING and says
  # "verified", which is the exact shape of a check that reports success by having had no work to
  # do. So the count is reported either way and the two outcomes read differently.
  if [ "$bundle_count" -gt 0 ]; then
    set -- "$tmp/unpacked"/bundles/kontra-orchestrator-*.tar.gz
    if [ -f "$1" ]; then
      for b in "$@"; do
        name=$(basename "$b")
        "$installed_at" bundle verify "$KONTRA_HOME/bundles/$name" >/dev/null \
          || die "$name does not match its own manifest; the release is not what it says it is"
        say "  bundle   $name verified (every digest in its manifest re-derived)"
      done
    else
      say "  bundles  $bundle_count copied, none of them an orchestrator bundle"
    fi
  else
    say "  bundles  none in this release (expected after ADR 0052 §2 removes the appliance)"
  fi
fi

# --- pulumi ---------------------------------------------------------------------------------------
#
# A PULUMI INSTALL IS A DIRECTORY, NOT A BINARY, and this is the mistake worth guarding. Measured:
# the first entry in `pulumi-v3.256.0-linux-x64.tar.gz` is `pulumi/pulumi-language-dotnet`, and a
# finished install on this box is 14 executables in `~/.pulumi/bin` — `pulumi` itself plus
# `pulumi-language-yaml` at 36 MB and the rest of the language hosts. ADR 0052 fact 2 says a YAML
# program "needs no language host", and that means no Node and no Python RUNTIME on the host; the
# `pulumi-language-yaml` plugin is still the thing that loads `control/pulumi/Pulumi.yaml`. Copy only
# `pulumi` and you get a CLI that reports its version happily and fails at `kontra up` with a message
# about a language plugin, which reads as a program error and gets debugged as one.
#
# It goes under $PULUMI_HOME so the CLI and the plugins it will install share one root — the layout
# pulumi's own installer produced on this box. No symlink into $KONTRA_BIN: whether the CLI finds
# its own directory through a symlink is a claim about os.Executable on two operating systems that
# this script cannot test on either, and pulumi's own installer also settles for a PATH line.
if [ "$install_pulumi" = yes ]; then
  step "installing pulumi $PULUMI_VERSION"
  pulumi_base="${KONTRA_PULUMI_BASE_URL:-https://github.com/pulumi/pulumi/releases/download/v$PULUMI_VERSION}"
  pulumi_tar="pulumi-v$PULUMI_VERSION-$goos-$pulumi_arch.tar.gz"
  # THE CHECKSUMS FILE IS NAMED WITH THE BARE VERSION AND THE TARBALL WITH THE `v`. Measured on the
  # v3.256.0 release: `pulumi-3.256.0-checksums.txt` beside `pulumi-v3.256.0-linux-x64.tar.gz`. The
  # same split kontra's own release has (tag `v0.1.0`, file `kontra_0.1.0_…`), in the other order.
  pulumi_sums="pulumi-$PULUMI_VERSION-checksums.txt"

  curl -fSL --progress-bar -o "$tmp/$pulumi_tar" "$pulumi_base/$pulumi_tar" \
    || die "no $pulumi_tar at $pulumi_base — is $PULUMI_VERSION a published Pulumi release?"
  curl -fsSL -o "$tmp/$pulumi_sums" "$pulumi_base/$pulumi_sums" \
    || die "Pulumi $PULUMI_VERSION publishes no $pulumi_sums; refusing to unpack an archive nothing describes"

  say "  verifying $pulumi_tar"
  ( cd "$tmp" && grep -F " $pulumi_tar" "$pulumi_sums" | grep "[[:space:]]$pulumi_tar\$" > pulumi.one ) \
    || die "$pulumi_sums does not mention $pulumi_tar"
  [ -s "$tmp/pulumi.one" ] || die "$pulumi_sums does not mention $pulumi_tar"
  ( cd "$tmp" && sha_verify pulumi.one ) \
    || die "$pulumi_tar does not match the digest Pulumi published for $PULUMI_VERSION"

  tar -xzf "$tmp/$pulumi_tar" -C "$tmp"
  [ -x "$tmp/pulumi/pulumi" ] || die "$pulumi_tar has no pulumi/pulumi; this is not a Pulumi release"
  # The one plugin whose absence is invisible until `kontra up` — asserted by name, not counted.
  [ -x "$tmp/pulumi/pulumi-language-yaml" ] \
    || die "$pulumi_tar carries no pulumi-language-yaml, which is what loads control/pulumi/Pulumi.yaml"

  mkdir -p "$pulumi_home/bin"
  cp -f "$tmp/pulumi"/* "$pulumi_home/bin/"
  pulumi_bin="$pulumi_home/bin/pulumi"
  pulumi_have=$("$pulumi_bin" version 2>/dev/null | head -1 | tr -d '\r') || pulumi_have=''
  version_at_least "$pulumi_have" "$PULUMI_FLOOR" \
    || die "installed pulumi reports \`$pulumi_have\`, which is not >= $PULUMI_FLOOR"
  say "  pulumi   $pulumi_have -> $pulumi_bin"

  # ON PATH FOR THIS PROCESS, BECAUSE THE PLUGIN INSTALL BELOW IS IN IT, and reported for the
  # operator's next shell rather than written into one.
  #
  # NO /etc/profile.d SNIPPET, AND install.sh:81-85 IS NOT THE PRECEDENT IT LOOKS LIKE. That file
  # writes `/etc/profile.d/go.sh` for `/usr/local/go/bin` — a system path, the same for every user on
  # the machine. `$HOME/.pulumi/bin` is ONE user's home directory, and putting it in a system-wide
  # profile would hand every other account a PATH entry into somebody else's home, which is a worse
  # bug than a missing PATH line and a silent one. Nor is a `~/.bashrc` edit taken: this script does
  # not know which of four shells is in use, and an installer that rewrites dotfiles is one people
  # read before running for exactly that reason. So it prints the line, which is also what
  # install-appliance.sh:167-169 settles for and what pulumi's own installer does.
  case ":$PATH:" in
    *":$pulumi_home/bin:"*) ;;
    *)
      PATH="$pulumi_home/bin:$PATH"
      export PATH
      pulumi_path_todo=yes ;;
  esac
fi

# --- the docker provider plugin -------------------------------------------------------------------
#
# NOT PART OF A STOCK PULUMI INSTALL, and `control/pulumi/README.md` has the evidence: before that
# work, `pulumi plugin ls` on this box listed `digitalocean` and `random` and nothing else, and
# `find /root/.pulumi -name '*docker*'` returned nothing. A YAML program that declares
# `docker:index:Container` with no plugin fails at resource registration, which reads as a program
# error rather than as a missing binary — so that README assigns this step here by name.
#
# THE INSTALL IS IDEMPOTENT AND THE ASSERTION IS SEPARATE. Measured: re-running it for an already
# present 4.11.2 is a 0.33 s no-op that prints nothing. But "install exited 0" and "the plugin is
# there" are two different facts, and the second is the one a converge depends on, so it is asked
# afterwards. It lands in $PULUMI_HOME/plugins — measured, `resource-docker-v4.11.2/
# pulumi-resource-docker` — which is the same root the CLI above was installed under, deliberately.
step "pulumi plugin: docker $DOCKER_PLUGIN_VERSION"
"$pulumi_bin" plugin install resource docker "$DOCKER_PLUGIN_VERSION" \
  || die "could not install the docker provider plugin $DOCKER_PLUGIN_VERSION.
  \`kontra up\` cannot converge without it — control/pulumi/Pulumi.yaml pins it on an explicit
  pulumi:providers:docker resource. Retry, or install it by hand:
      pulumi plugin install resource docker $DOCKER_PLUGIN_VERSION"
"$pulumi_bin" plugin ls 2>/dev/null \
  | awk -v v="$DOCKER_PLUGIN_VERSION" '$1=="docker" && $2=="resource" && $3==v { found=1 }
                                       END { exit !found }' \
  || die "the docker plugin install reported success and \`pulumi plugin ls\` does not list
  docker/resource/$DOCKER_PLUGIN_VERSION. Something else owns \$PULUMI_HOME ($pulumi_home)."
say "  plugin   docker $DOCKER_PLUGIN_VERSION present"

# --- prove the install ----------------------------------------------------------------------------
#
# EXACT EQUALITY, NOT A GREP. `kontra version` prints `buildinfo.Version()` and nothing else
# (cli/main.go:272), so `$bare` is the whole of correct output. A grep would also pass on
# `dev (ad8871a, dirty)`, and that string is the exact fingerprint of the failure this catches:
# buildinfo.go:78-84 records that `-X` on a wrong symbol path is not an error — the linker ignores
# it, the build succeeds, and the binary reports `dev` for every release forever. So a mismatch here
# means either these are not the release's bytes or the release was linked without its version, and
# both are reasons not to tell somebody the install worked.
step "checking the install"
reported=$("$installed_at" version 2>/dev/null || true)
[ "$reported" = "$bare" ] || die "$installed_at reports \`$reported\`, not \`$bare\`.
  A \`dev (…)\` answer means this binary was not linked by \`kontra release\` — see
  cli/internal/buildinfo/buildinfo.go:78-84. Anything else means the tarball is not release $version."
say "  kontra   $reported"

# THE PATH SHADOW, WHICH IS THE CLASSIC "I INSTALLED IT AND IT STILL SAYS THE OLD VERSION".
# install.sh installs a checkout build to /usr/local/bin/kontra by default (install.sh:15), and this
# file may have just landed in $KONTRA_HOME/bin because that directory was not writable. Then the
# `kontra` an operator types is the other one, reporting `dev (…)`, and every instruction below is
# about a binary they are not running.
on_path=$(command -v kontra 2>/dev/null || true)
if [ -z "$on_path" ]; then
  say ""
  say "  $installed_at is not on PATH. Add it:"
  say "      export PATH=\"\$PATH:$(dirname "$installed_at")\""
elif [ "$on_path" != "$installed_at" ]; then
  say ""
  say "  NOTE: \`kontra\` on your PATH is $on_path ($("$on_path" version 2>/dev/null || echo '?')),"
  say "        not the $reported just installed at $installed_at."
  say "        Put $(dirname "$installed_at") earlier on PATH, or remove the other one."
fi
if [ "${pulumi_path_todo:-no}" = yes ]; then
  say ""
  say "  pulumi is in $pulumi_home/bin, which is not on this shell's PATH. Add it:"
  say "      export PATH=\"\$PATH:$pulumi_home/bin\""
fi

# --- what happens next ----------------------------------------------------------------------------
#
# The state directory, the stack, the `workspaces` config key and the login are all `kontra up`'s —
# control/pulumi/README.md, "What `kontra up` will run against it", lists the five steps and this
# file performs none of them. The one sentence worth carrying here is the trap, because the reader
# most likely to spring it is the one who just installed Pulumi for the first time and tries the
# program by hand.
step "installed"
say "  kontra   $reported   $installed_at"
say "  pulumi   $pulumi_have   $pulumi_bin"
say "  docker   $docker_server"
say "  home     $KONTRA_HOME"
say ""
say "start it:"
say "    kontra up"
say ""
say "\`kontra up\` pins Pulumi's backend to file://$KONTRA_HOME/state before it converges. If you run"
say "pulumi by hand against control/pulumi/ instead, log in FIRST — a pulumi with no backend creates"
say "an ephemeral Pulumi Cloud account and deploys there, state included (workspace.ts:16):"
say "    pulumi login file://$KONTRA_HOME/state"
