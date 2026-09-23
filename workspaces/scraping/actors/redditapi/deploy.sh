#!/usr/bin/env sh
# Install everything `redditapi` needs. ONE script, both Targets.
#
# Note how much smaller this is than the browser transport's: no Firefox shared libraries, no
# 150 MB browser fetch, no apt at all. That difference IS the argument for this actor — a Machine
# is ready in seconds and fits in 1 GB.
set -e
PY="${KONTRA_PYTHON:-python3}"
echo "[deploy.sh] redditapi: installing dependencies (target=${KONTRA_TARGET:-container})"
"$PY" -m pip install --no-cache-dir --break-system-packages "httpx>=0.27" 2>/dev/null \
    || "$PY" -m pip install --no-cache-dir "httpx>=0.27"
echo "[deploy.sh] redditapi: ready"
