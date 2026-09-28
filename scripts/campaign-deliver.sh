#!/usr/bin/env bash
# campaign-deliver.sh — everything between "the run finished" and "here is the report".
#
#   scripts/campaign-deliver.sh campaign-1790599185
#
# ONE SCRIPT BECAUSE THE ORDER MATTERS AND IS NOT OBVIOUS. Four of these steps have a prerequisite
# that is invisible if you get it wrong:
#
#   1. The SPA bundle is swapped FIRST. The served console is older than its source and renders
#      every Input field as "not sent" — so a screenshot taken before the swap says the campaign
#      ran with no arguments.
#   2. Screenshots come BEFORE anything that could age the run out of `/api/runs`. That list is
#      how the run page gets its two status chips; a run missing from it renders them blank and
#      shows "Temporal has dropped this execution" where the Input should be.
#   3. The facts are collected from the lake, not from the screenshots, and every figure is one
#      SQL statement so the report can be re-derived by whoever is reading it.
#   4. The report is generated last, from the facts plus the shots.
#
# Safe to re-run: every step overwrites its own output and none of them touch the lake.
set -euo pipefail

RUN="${1:?usage: campaign-deliver.sh <run-id>}"
ROOT="/root/oss/kontra"
SHOTS="${SHOTS:-/tmp/campaign-shots}"
OUT="${OUT:-$ROOT/.scratch/campaign-report.html}"
OBS="${OBS:-observations_h1}"
POINTS="${POINTS:-injection_points_h1}"
LEADS="${LEADS:-desync_leads_h1}"
TECHNIQUES="${TECHNIQUES:-techniques_h1}"
SUMMARY="${SUMMARY:-campaign_summary_h1}"

cd "$ROOT"
say() { printf '\n\033[1m==> %s\033[0m\n' "$*"; }

say "1/4  swapping in the rebuilt console bundle"
if [ -f /tmp/spa-build/kontra-spa.tar.gz ]; then
  docker cp /tmp/spa-build/kontra-spa.tar.gz kontra-api:/var/lib/kontra/bundles/kontra-spa.tar.gz
  docker compose restart orchestrator-api >/dev/null
  # The API is also the materializer; give it a moment to come back before driving a browser at it.
  until curl -sf http://localhost:8088/api/health >/dev/null 2>&1; do sleep 2; done
  echo "    console rebuilt and serving"
else
  echo "    SKIPPED: /tmp/spa-build/kontra-spa.tar.gz is missing — run 'kontra bundle spa --out /tmp/spa-build'"
  echo "    the Input card will read 'not sent' for every field on the screenshots"
fi

say "2/4  screenshots, while the run is still in /api/runs"
PASS="$(docker exec kontra-api sed -n 's/^admin\t//p' /var/lib/kontra/console-password | tr -d '\r')"
SHOTS="$SHOTS" RUN="$RUN" KONTRA_PASS="$PASS" \
  OBS="$OBS" POINTS="$POINTS" LEADS="$LEADS" \
  node scripts/campaign-shots.mjs

say "3/4  collecting the facts from the lake"
python3 scripts/campaign-facts.py --run "$RUN" \
  --obs "$OBS" --points "$POINTS" --leads "$LEADS" --techniques "$TECHNIQUES" \
  --out /tmp/campaign-facts.json

say "4/4  generating the report"
python3 scripts/campaign-report.py \
  --facts /tmp/campaign-facts.json --shots "$SHOTS" --out "$OUT"

say "done"
printf '  report   %s\n  shots    %s\n  facts    %s\n  summary  %s (query it: kontra dataset query %s --sql "SELECT * FROM %s")\n' \
  "$OUT" "$SHOTS" /tmp/campaign-facts.json "$SUMMARY" "$SUMMARY" "$SUMMARY"
