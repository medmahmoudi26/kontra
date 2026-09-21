#!/usr/bin/env sh
# Install everything `reddit` needs, wherever it is going to run.
#
# ONE script, both Targets: `kontra deploy` runs it inside the actor's image for a container
# Target, and machine.ts runs the same script over SSH on a bare Machine (step 4 of the
# Machine install). A Dockerfile could only express one of the two.
#
# Idempotent, rootful-ok, and quiet about which Target it is on.
set -e

PY="${KONTRA_PYTHON:-python3}"

# THE BROWSER GOES SOMEWHERE BOTH USERS CAN SEE. Camoufox resolves its install dir as
# `platformdirs.user_cache_dir("camoufox")` — i.e. $XDG_CACHE_HOME, else $HOME/.cache — and it
# takes NO dedicated env var of its own. So a build that runs as one user and a run that
# happens as another finds no browser and dies at @actor.load, which is the same trap
# webcrawl pins PLAYWRIGHT_BROWSERS_PATH for. Pin it to a system path on both sides: the
# actor re-exports this exact value in open_browser().
CACHE_ROOT="${KONTRA_CAMOUFOX_CACHE:-/opt/kontra-cache}"
export XDG_CACHE_HOME="$CACHE_ROOT"
mkdir -p "$CACHE_ROOT"

echo "[deploy.sh] reddit: installing dependencies (target=${KONTRA_TARGET:-container})"

# APT, WITH A REAL WAIT. MEASURED on kf-reddit-01: this script died instantly with
#   Could not get lock /var/lib/apt/lists/lock. It is held by process 1377 (apt-get)
# even though `-o DPkg::Lock::Timeout=600` was set. That option waits for the DPKG lock; it does
# NOT wait for apt's own lists lock, and the two are taken by different things. The framework's
# step 1 gets away with it because it runs seconds after boot while the lock is still free —
# deploy.sh runs after the bundle fetch, by which time unattended-upgrades has woken up. So poll
# instead of trusting the flag, and let cloud-init finish first when it is still running.
command -v cloud-init >/dev/null 2>&1 && cloud-init status --wait >/dev/null 2>&1 || true

apt_retry() {
    # ~5 minutes. Returns non-zero only if every attempt failed, so the CALLER decides whether
    # that is fatal — a stale index is survivable, a missing libgtk is not.
    n=0
    while [ "$n" -lt 30 ]; do
        if apt-get -o DPkg::Lock::Timeout=60 "$@"; then return 0; fi
        n=$((n + 1))
        echo "[deploy.sh] apt busy (attempt $n/30), waiting 10s" >&2
        sleep 10
    done
    return 1
}

# Camoufox is a FIREFOX build, so this is the Firefox shared-library set, not Chromium's.
if command -v apt-get >/dev/null 2>&1; then
    export DEBIAN_FRONTEND=noninteractive

    # A stale index is survivable — the packages below may already be cached — so this one is
    # allowed to give up. It is the INSTALL that must not.
    apt_retry update || echo "[deploy.sh] apt update failed after retries; continuing" >&2

    # FATAL if it ultimately fails. A missing libgtk/libasound does not fail here, it fails much
    # later as an unreadable Camoufox launch error inside @actor.load, on every Unit.
    apt_retry install -y --no-install-recommends \
        ca-certificates fonts-liberation libgtk-3-0 libdbus-glib-1-2 libx11-xcb1 \
        libxt6 libxcomposite1 libxdamage1 libxfixes3 libxrandr2 libgbm1 libpci3 \
        libxkbcommon0 libpango-1.0-0 libcairo2

    # libasound2 was renamed libasound2t64 in Ubuntu 24.04. Try the modern name, fall back to the
    # old one, so this script spans both images rather than pinning the fleet to one.
    apt_retry install -y --no-install-recommends libasound2t64 \
        || apt_retry install -y --no-install-recommends libasound2

    rm -rf /var/lib/apt/lists/*
fi

# [geoip] is not optional for this actor: CAMOUFOX_OPTS sets `geoip: True` so the spoofed
# timezone/locale line up with the exit IP, and without the extra that option raises at launch.
"$PY" -m pip install --no-cache-dir --break-system-packages "camoufox[geoip]" 2>/dev/null \
    || "$PY" -m pip install --no-cache-dir "camoufox[geoip]"

# Fetch the browser itself (~150 MB) at BUILD time, not first-run time. A Machine that
# downloads Firefox inside @actor.load spends the session's first minute on it and does it
# again on every reload.
"$PY" -m camoufox fetch

echo "[deploy.sh] reddit: camoufox under $CACHE_ROOT/camoufox"
