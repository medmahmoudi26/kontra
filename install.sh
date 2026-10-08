#!/usr/bin/env bash
# Auto-install + codegen for kontra-local.
#   - creates .venv and installs the package (editable, default finder mode)
#   - fetches the buf binary into .venv/bin
#   - generates proto stubs (sdk/python/kontra/v1/ + control/orchestrator/_gen/)
set -euo pipefail
cd "$(dirname "$0")"

PY=.venv/bin/python
PIP=.venv/bin/pip
BUF=.venv/bin/buf

# Where the CLI lands. Overridable for a checkout you do not want on PATH:
#   CLI_DEST=$PWD/cli/kontra ./install.sh
CLI_DEST="${CLI_DEST:-/usr/local/bin/kontra}"

# The Go this repo is built with. `go.work` says 1.26.8 and Ubuntu 24.04 ships 1.22, so the
# distro package is not an option — this is fetched from upstream and checksummed, the same
# shape as the buf step below. Bump both the version and its digests together.
GO_VERSION="${GO_VERSION:-1.26.8}"
GO_SHA256_amd64=d0f743b33e8d8945e6b1f432edd15785c70507121d6e2a723b21285eddf8b57b
GO_SHA256_arm64=211ffced9dcb9633a55eac6364816ec0ddd951389a740e88fa8b3337971bdda0

# The one buf plugin that is `local:` rather than `remote:` — see the codegen step below.
PLUGIN_MOD=github.com/cludden/protoc-gen-go-temporal

# --- preflight ------------------------------------------------------------------------------
#
# WHAT A BARE MACHINE ACTUALLY HAS, MEASURED. On a stock Ubuntu 24.04 droplet: `git`, `python3`
# and `curl` are present; `pip3`, `docker`, `go`, `node`, `pnpm`, `make` and `gcc` are not. This
# script used to die on its first line of work with a message from PYTHON about `ensurepip` —
# naming a package, not a next step, and saying nothing about the Go and Docker walls waiting
# two and twenty lines later. Onboarding was: hit a wall, install one thing, hit the next.
#
# AND THE FIRST FAILURE POISONED THE RETRY. `python3 -m venv` leaves a .venv/ behind even when
# it fails for want of `ensurepip`, the guard below was `[ -d .venv ]`, so the run after
# `apt install python3-venv` SKIPPED creation, kept the pip-less venv and failed differently and
# further in. A first run that breaks the second is worse than one that refuses to start.
#
# So: find everything missing at once, install it where we can, and where we cannot, print the
# exact commands rather than a package name. `KONTRA_BOOTSTRAP=no` turns the installing off and
# leaves the reporting on, for a machine whose packages are somebody else's business.
need_cmd() { command -v "$1" >/dev/null 2>&1; }

# Go is a VERSION check, not a presence check: a distro Go that cannot build the modules is a
# harder failure to read than no Go at all — it fails later, inside a build, naming a language
# feature instead of a toolchain.
go_new_enough() {
  need_cmd go || return 1
  local have want
  have="$(go env GOVERSION 2>/dev/null | sed 's/^go//')" || return 1
  want="$GO_VERSION"
  [ "$(printf '%s\n%s\n' "$want" "$have" | sort -V | head -1)" = "$want" ]
}

# `python3 -m venv` can be present and still not work — Debian and Ubuntu ship the module and
# withhold `ensurepip`. Ask the question the script actually depends on.
venv_works() { python3 -c 'import ensurepip' >/dev/null 2>&1; }

apt_install() {
  echo "    apt-get install $*"
  DEBIAN_FRONTEND=noninteractive $SUDO apt-get install -y -q "$@" >/dev/null
}

install_go() {
  local arch tarball sha
  case "$(uname -m)" in
    x86_64) arch=amd64; sha="$GO_SHA256_amd64" ;;
    aarch64|arm64) arch=arm64; sha="$GO_SHA256_arm64" ;;
    *) echo "    no pinned Go for $(uname -m) — install Go $GO_VERSION yourself"; return 1 ;;
  esac
  tarball="/tmp/go${GO_VERSION}.linux-${arch}.tar.gz"
  echo "    fetching Go $GO_VERSION ($arch)"
  curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${arch}.tar.gz" -o "$tarball"
  echo "${sha}  ${tarball}" | sha256sum -c - >/dev/null
  $SUDO rm -rf /usr/local/go
  $SUDO tar -C /usr/local -xzf "$tarball"
  rm -f "$tarball"
  export PATH="/usr/local/go/bin:$PATH"
  # A login shell has to find it too, or the next command an operator types is `go: not found`
  # in a fresh terminal, with no clue that this script already put it on disk.
  if [ -d /etc/profile.d ] && [ ! -e /etc/profile.d/go.sh ]; then
    echo 'export PATH=$PATH:/usr/local/go/bin' | $SUDO tee /etc/profile.d/go.sh >/dev/null
  fi
}

SUDO=""
[ "$(id -u)" = 0 ] || SUDO="sudo"

# NODE IS A PREREQUISITE OF THE CONTROL PLANE, and this script did not know it (issue #2).
#
# The orchestrator is a Node process. With no published release there is no bundle to fall back on,
# so `kontra up` on a clone needs `control/orchestrator/dist` — which needs node and pnpm. Without
# this check the script finished clean, printed "run kontra up, open :8088", and `kontra up` then
# died naming a bundle that does not exist and a checkout that was never compiled. Preflight's whole
# promise is that you learn the WHOLE bill now.
#
# `engines.node` is ">=22.13.0" and the appliance pins that exact version
# (`cli/appliance/bundle/pins.go`), so 22 is the floor rather than a preference.
NODE_MAJOR_MIN=22
node_new_enough() {
  need_cmd node || return 1
  local major
  major="$(node -p 'process.versions.node.split(".")[0]' 2>/dev/null || echo 0)"
  [ "$major" -ge "$NODE_MAJOR_MIN" ] 2>/dev/null
}

missing=()
venv_works    || missing+=(python3-venv)
go_new_enough || missing+=(go)
node_new_enough || missing+=(node)
need_cmd docker || missing+=(docker)

if [ ${#missing[@]} -gt 0 ]; then
  echo "==> prerequisites: ${missing[*]}"
  if [ "${KONTRA_BOOTSTRAP:-yes}" = no ] || ! need_cmd apt-get; then
    # Everything, in one message. The point of preflight is that you learn the whole bill now.
    echo
    echo "    install these, then re-run ./install.sh:"
    for m in "${missing[@]}"; do
      case "$m" in
        python3-venv) echo "      python3-venv   apt install python3-venv   (Debian/Ubuntu)" ;;
        go)           echo "      go >= $GO_VERSION   https://go.dev/dl/  (the distro package is older)" ;;
        node)         echo "      node >= $NODE_MAJOR_MIN    https://nodejs.org/  then: corepack enable"
                      echo "                     (Ubuntu's nodejs is 18; the orchestrator needs 22)" ;;
        docker)       echo "      docker         https://docs.docker.com/engine/install/" ;;
      esac
    done
    echo
    echo "    or let this script do it:  KONTRA_BOOTSTRAP=yes ./install.sh   (needs apt + sudo)"
    exit 1
  fi
  $SUDO apt-get update -q >/dev/null
  for m in "${missing[@]}"; do
    case "$m" in
      python3-venv) apt_install python3-venv ;;
      # docker.io + the v2 compose PLUGIN. `docker-compose` (the v1 python script) is a
      # different command and `docker compose` — which is what the Makefile and `kontra infra`
      # call — does not exist without the plugin.
      docker)       apt_install docker.io docker-compose-v2 ;;
      go)           install_go ;;
      # NOT APT-INSTALLED, deliberately. `apt install nodejs` on Ubuntu 24.04 gives Node 18, and
      # the orchestrator's `engines.node` is ">=22.13.0" — so the package manager's answer here is
      # a build that fails later for a reason that looks nothing like a stale Node. Named, not
      # guessed at, which is what preflight already does for anything it cannot install.
      node)         echo "    node >= $NODE_MAJOR_MIN is NOT installed from apt (the distro package is 18)."
                    echo "      https://nodejs.org/ or nvm, then:  corepack enable"
                    echo "    the CLI will still build; the CONTROL PLANE will not."
                    ;;
    esac
  done
fi
need_cmd go || export PATH="/usr/local/go/bin:$PATH"

echo "==> python venv + install (core + seaweed)"
# TESTED FOR PIP, NOT FOR THE DIRECTORY. A venv that failed to build still leaves the directory,
# so `[ -d .venv ]` treated the wreckage of a previous run as a working environment and every
# later step failed pointing somewhere else. Rebuilt rather than repaired: the failure mode this
# replaces is a half-built venv, and nothing here can tell which half.
if [ ! -x "$PIP" ]; then
  rm -rf .venv
  python3 -m venv .venv
fi
"$PIP" install -q --upgrade pip
# Default finder mode (do NOT use editable_mode=compat: breaks the kontra console
# script), so keep the editable core as the LAST pip step.
"$PIP" install -q -e "./sdk/python[dev,seaweed]"

# ONE venv. There used to be two (.venv + .venv-actor) because the old actor runtime pinned a
# protobuf the Temporal SDK could not share an interpreter with. That runtime is gone; the actor
# host is a Temporal activity worker and needs the same temporalio the rest of the repo has.
echo "==> actor host runtime (temporalio + redis + boto3, into .venv)"
.venv/bin/pip install -q temporalio redis boto3 pydantic

echo "==> buf"
if [ ! -x "$BUF" ]; then
  OS="$(uname -s)"; ARCH="$(uname -m)"
  curl -fsSL "https://github.com/bufbuild/buf/releases/latest/download/buf-${OS}-${ARCH}" -o "$BUF"
  chmod +x "$BUF"
fi
"$BUF" --version

# THE ONE PLUGIN BUF CANNOT FETCH ITSELF. Every other entry in buf.gen.yaml is `remote:` and is
# pulled from the BSR; `protoc-gen-go_temporal` is `local:`, so buf expects to find it on PATH and
# says only `executable file not found in $PATH` when it does not — which on a fresh machine is
# the third wall in a row, after a run that has already taken a minute. buf.gen.yaml has carried
# the `go install` line as a comment for as long as it has been local; a comment is not an
# install step.
#
# AT THE VERSION THE MODULE GRAPH ALREADY PINS, not @latest: `runtime/handler/go.mod` requires
# protoc-gen-go-temporal v1.24.0, and generated code that drifts from the module compiled against
# it is a drift-check failure in CI (`buf generate` + `git diff --exit-code`) that reads as
# somebody hand-editing generated files. `go install` inside the module resolves that pin.
# THE DUCKDB CLI, WHICH IS NOT THE EMBEDDED DUCKDB. The orchestrator has its own via
# `@duckdb/node-api`; this is the standalone binary `kontra dataset|db|monitor` shell out to for
# every load and query of the lake. Nothing installed it, so `kontra dataset create` — the step
# between a fresh control plane and having anything for a run to work on — failed at USE
# time on a machine that had just been told installation was done. The error names the tool and
# its URL, which is better than most, but a prerequisite discovered after install is a
# prerequisite install.sh should have handled.
#
# The `.gz` asset, deliberately: the `.zip` would add `unzip`, which a minimal Ubuntu also lacks.
# Pinned rather than `latest` because the CLI and the embedded engine read the same DuckLake
# catalog — this pair (CLI 1.5.5, node-api 1.5.4-r.1) is the one in use on the machine this repo
# is developed on.
DUCKDB_VERSION="${DUCKDB_VERSION:-1.5.5}"
DUCKDB_DEST="${DUCKDB_DEST:-/usr/local/bin/duckdb}"
if ! command -v duckdb >/dev/null 2>&1; then
  echo "==> duckdb CLI $DUCKDB_VERSION -> $DUCKDB_DEST"
  case "$(uname -m)" in
    x86_64) duck_arch=amd64 ;;
    aarch64|arm64) duck_arch=arm64 ;;
    *) duck_arch="" ;;
  esac
  if [ -n "$duck_arch" ]; then
    curl -fsSL "https://github.com/duckdb/duckdb/releases/download/v${DUCKDB_VERSION}/duckdb_cli-linux-${duck_arch}.gz" \
      | gunzip > /tmp/duckdb.$$
    $SUDO install -m 0755 /tmp/duckdb.$$ "$DUCKDB_DEST"
    rm -f /tmp/duckdb.$$
    "$DUCKDB_DEST" --version
  else
    echo "    no duckdb build for $(uname -m) — install it yourself: https://install.duckdb.org"
  fi
fi

echo "==> protoc-gen-go_temporal (the one local buf plugin)"
export PATH="$(go env GOPATH)/bin:$PATH"
if ! command -v protoc-gen-go_temporal >/dev/null 2>&1; then
  # `pkg@version`, NOT a bare `go install` inside runtime/handler/. The handler requires this module for
  # the runtime package it generates against, so its go.sum carries only what the HANDLER
  # compiles — building the plugin BINARY pulls sprig, protopatch, jennifer, durafmt and pflag,
  # none of which are in there, and the failure is seven `missing go.sum entry` lines that read
  # like a corrupt checkout. `pkg@version` resolves against the plugin's own go.mod instead.
  #
  # The version is READ from the pin rather than written here twice: a second copy of "v1.24.0"
  # in this file is a copy that will be wrong the first time somebody bumps the module.
  plugin_ver="$( cd runtime/handler && GOWORK=off go list -m -f '{{.Version}}' "$PLUGIN_MOD" )"
  echo "    $PLUGIN_MOD@$plugin_ver (pinned by runtime/handler/go.mod)"
  GOWORK=off go install "${PLUGIN_MOD}/cmd/protoc-gen-go_temporal@${plugin_ver}"
fi

echo "==> generate workflow contract stubs"
"$BUF" generate

# The CLI goes ON PATH, here, every time. Leaving it as "cd cli && go build" is how a
# two-day-old binary keeps answering while the checkout has moved on — and the symptom is an
# error about code that no longer exists, which sends you looking in the wrong place.
echo "==> kontra CLI -> $CLI_DEST"
( cd cli && GOWORK=off go build -o kontra . )
install -m 0755 cli/kontra "$CLI_DEST"
"$CLI_DEST" help >/dev/null && echo "installed $("$CLI_DEST" --version 2>/dev/null || echo kontra)"

# .kontra/ — this installation's own configuration and code. AFTER the CLI is on PATH, because
# the CLI owns what goes in it: a heredoc here would be a second definition of the config format
# that drifts from the one that reads it.
#
# Idempotent and non-destructive — an existing config.yaml holds credentials somebody typed in and
# is never rewritten, so this is safe on every re-run of install.sh.
echo "==> .kontra/ (config + your workflows and actors)"
"$CLI_DEST" init

# THE CONTROL PLANE, COMPILED — the step whose absence made this whole script a lie (issue #2).
#
# `kontra up` runs `control/orchestrator/dist/src/main.js`. With no published release there is no
# bundle to hydrate instead, so on a clone that file is the control plane and nothing else built it.
# The script finished clean, said "run kontra up, open :8088", and `kontra up` died.
#
# `run build`, NEVER `exec tsc`. `package.json`'s `build` is `pnpm --filter @kontra/core run build
# && tsc`, and the orchestrator imports `@kontra/core` — so bare `tsc` is the second half of a
# two-step build run without the first, and fails with about a hundred errors on a clean checkout.
# `cli/orchestrator.go` names the same command in its refusal, for the same reason.
ORCHESTRATOR_BUILT=no
if node_new_enough; then
  if ! need_cmd pnpm; then
    # `corepack` ships WITH node and is the supported way to get pnpm; asking for a global npm
    # install instead is how two pnpm versions end up on one machine.
    echo "==> pnpm (via corepack)"
    corepack enable >/dev/null 2>&1 || $SUDO corepack enable >/dev/null 2>&1 || true
  fi
  if need_cmd pnpm; then
    echo "==> orchestrator (the control plane kontra up runs)"
    ( cd control/orchestrator && pnpm install --frozen-lockfile >/dev/null && pnpm run build >/dev/null ) \
      && ORCHESTRATOR_BUILT=yes \
      || echo "    build FAILED — kontra up will refuse until \`pnpm --dir control/orchestrator run build\` succeeds"
  else
    echo "==> orchestrator: SKIPPED — no pnpm (run \`corepack enable\`, then re-run ./install.sh)"
  fi
else
  echo "==> orchestrator: SKIPPED — node >= $NODE_MAJOR_MIN is not installed"
fi

echo
# WHAT IT SAYS DEPENDS ON WHAT IT DID. The old text promised `kontra up` unconditionally, which is
# the sentence that sent somebody to an error message instead of a console.
if [ "$ORCHESTRATOR_BUILT" = no ]; then
  echo "done — BUT THE CONTROL PLANE IS NOT BUILT, so \`kontra up\` will refuse."
  echo "  install node >= $NODE_MAJOR_MIN, then:"
  echo "    corepack enable && pnpm --dir control/orchestrator install && pnpm --dir control/orchestrator run build"
  echo
fi
# EVERY PATH BELOW HAS TO EXIST. These said `examples/python/beacon` and
# `examples/python/workflows/nscheck`, and there is no `examples/` directory in this repository —
# ADR 0038 moved the actors and workflows to repositories of their own, and this text did not move
# with them. Four commands that cannot run, printed as the last thing a first-time user reads.
echo "done. an actor is TWO processes — itself (a Temporal activity worker) + the Go handler:"
echo "  kontra up                                     # the control plane. Blocks; leave it running"
echo "  open http://localhost:8088                    # the console (the login was printed above)"
echo
echo "this repository ships NO actors and NO workflows (ADR 0038). Clone one and copy a folder in:"
echo "  git clone https://github.com/medmahmoudi26/kontra-actors    ~/kontra-actors"
echo "  git clone https://github.com/medmahmoudi26/kontra-workflows ~/kontra-workflows"
echo "  cp -r ~/kontra-actors/python/beacon        ~/.kontra/actors/"
echo "  cp -r ~/kontra-workflows/python/nscheck    ~/.kontra/workflows/"
echo
echo "  kontra serve --actor ~/.kontra/actors/beacon  # the actor, both halves"
echo "  kontra workflow serve nscheck                 # the caller loop, which is YOURS"
echo "  kontra workflow start nscheck --wait          # dispatch, from it"
