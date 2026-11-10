#!/usr/bin/env bash
# The console in a browser (test/browser): pytest with Playwright for Python under uv, against a gate in the local
# development profile that test/browser/conftest.py starts. Needs $KONTRA_TEST_PG (a Postgres server where the user may
# create databases), $KONTRA_TEMPORAL_CLI (the pinned Temporal CLI 1.9.1), Go, Node 22 with npm (for the MCP service),
# and Chromium for Playwright 1.64.0 (`uv run --no-project --with playwright==1.64.0 playwright install chromium`).
# Extra arguments go to pytest (e.g. -k a2ui -x).
set -euo pipefail
cd "$(dirname "$0")/.."
: "${KONTRA_TEST_PG:?set KONTRA_TEST_PG to a postgres:// URL of a server where the user may create databases}"
: "${KONTRA_TEMPORAL_CLI:?set KONTRA_TEMPORAL_CLI to the pinned Temporal CLI (1.9.1)}"
export GO="${GO:-go}"
exec uv run --no-project --with playwright==1.64.0 --with pytest==8.4.2 --with 'psycopg[binary]==3.2.10' \
  python -m pytest -p no:cacheprovider test/browser "$@"
