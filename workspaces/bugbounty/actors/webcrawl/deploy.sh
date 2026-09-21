#!/usr/bin/env sh
# Install everything `webcrawl` needs, wherever it is going to run.
#
# ONE script, both Targets (infra/CONTEXT.md): `kontra deploy` runs it inside the actor's
# image while building a container Target, and the machine Target runs the same script over
# SSH on a bare Machine. That is the whole reason it is a script and not a Dockerfile — a
# Dockerfile can only express one of those two.
#
# It must therefore be:
#   idempotent  — a re-deploy re-runs it on a Machine that already has Chromium
#   rootful-ok  — it runs as root in a build and as root on a fresh Machine
#   quiet about the difference — $KONTRA_TARGET is available but nothing here needs it
#
# Set -e only: `set -u` would trip over the unset optional env this script deliberately
# tolerates.
set -e

PY="${KONTRA_PYTHON:-python3}"

echo "[deploy.sh] webcrawl: installing dependencies (target=${KONTRA_TARGET:-container})"

# Chromium's own shared libraries. `playwright install --with-deps` would do this too, but it
# assumes apt and a network it can reach as root; doing it explicitly keeps the failure
# readable when a Machine's package index is stale.
if command -v apt-get >/dev/null 2>&1; then
    export DEBIAN_FRONTEND=noninteractive
    apt-get update
    apt-get install -y --no-install-recommends \
        ca-certificates fonts-liberation \
        libasound2 libatk-bridge2.0-0 libatk1.0-0 libatspi2.0-0 libcairo2 libcups2 \
        libdbus-1-3 libdrm2 libgbm1 libglib2.0-0 libnspr4 libnss3 libpango-1.0-0 \
        libx11-6 libxcb1 libxcomposite1 libxdamage1 libxext6 libxfixes3 libxkbcommon0 \
        libxrandr2 xdg-utils
    rm -rf /var/lib/apt/lists/*
fi

# Pinned. Playwright refuses to drive a browser build it was not shipped against, so an
# unpinned install here means the next `apt-get`-fresh Machine silently gets a different
# browser from the one the image was tested with.
# `simhash` backs the frontier's near-duplicate suppression. Pinned to the same range crawl4ai
# pins, because dedupe.py is a verbatim copy of crawl4ai's fingerprinting and a different minor
# version of a SIMILARITY function does not fail loudly — it returns plausible numbers.
"$PY" -m pip install --no-cache-dir --break-system-packages "playwright==1.58.0" "simhash>=2.1,<3" 2>/dev/null \
    || "$PY" -m pip install --no-cache-dir "playwright==1.58.0" "simhash>=2.1,<3"

# Chromium only. `playwright install` with no argument also fetches Firefox and WebKit —
# ~500 MB this actor never opens, on every Machine in the Fleet.
#
# PLAYWRIGHT_BROWSERS_PATH is pinned to a system path rather than left at $HOME/.cache: the
# build and the run are not guaranteed to share a HOME (a container that later runs as a
# different user finds no browser and fails at @actor.load, not at build).
export PLAYWRIGHT_BROWSERS_PATH="${PLAYWRIGHT_BROWSERS_PATH:-/opt/ms-playwright}"
"$PY" -m playwright install chromium

echo "[deploy.sh] webcrawl: chromium at $PLAYWRIGHT_BROWSERS_PATH"
