#!/usr/bin/env bash
# campaign-progress.sh — what is this campaign doing, right now, in one screen.
#
#   scripts/campaign-progress.sh campaign-1790599185
#   watch -n30 scripts/campaign-progress.sh campaign-1790599185
#
# WHY THIS EXISTS. A campaign is a parent workflow whose work all happens in CHILDREN, and neither
# surface answers the two questions anybody actually has of a long run:
#
#   the run page  shows the PARENT, which writes no dataset until the very end, so its Dataset
#                 drawer is empty for hours and its Progress region shows fleet steps, not programs.
#   the log rail  is per-run, and the interesting lines are on the children — and Temporal
#                 SUPPRESSES logging during replay, so a worker restart silently erases the
#                 backlog of "started" lines for everything already in flight.
#
# So this reads the two authorities directly: Temporal for which children exist and in what state,
# and the lake for how many rows each has actually landed. Nothing here is inferred.
set -euo pipefail

RUN="${1:?usage: campaign-progress.sh <campaign-run-id>}"
cd "$(dirname "$0")/.."

count() {
  docker exec kontra-temporal tctl --ns default wf list \
    --query "WorkflowId STARTS_WITH '$RUN-$1' AND ExecutionStatus='$2'" 2>/dev/null |
    tail -n +2 | grep -c . || true
}

# One dataset listing, reused — it is the slowest call here and asking twice doubles the wait.
LIST="$(timeout 150 kontra dataset list 2>/dev/null || true)"
rows_for() { awk -v p="$1" '$2==p && $6=="open" {s+=$5} END {print s+0}' <<<"$LIST"; }

printf '\n\033[1m%s\033[0m\n' "$RUN"

for phase in surface hunt; do
  d=$(count "$phase" Completed); r=$(count "$phase" Running); f=$(count "$phase" Failed)
  [ "$((d + r + f))" -eq 0 ] && continue
  printf '\n  %-8s %s done · %s running · %s failed\n' "$phase" "$d" "$r" "$f"
  for W in $(docker exec kontra-temporal tctl --ns default wf list \
               --query "WorkflowId STARTS_WITH '$RUN-$phase' AND ExecutionStatus='Running'" 2>/dev/null |
             tail -n +2 | awk -F'|' '{gsub(/ /,"",$2); print $2}'); do
    prog="${W#"$RUN-$phase-"}"
    prog="${prog#crlf-}"; prog="${prog#cl0-}"
    printf '      %-26s %8s events\n' "$prog" "$(rows_for "http_events_$prog")"
  done
done

# THE TOTALS THAT MATTER, off the lake rather than off a log line. `open` filters to datasets this
# installation is still writing, which excludes every partition a previous campaign left behind.
printf '\n  lake: %s event row(s) across %s crawl dataset(s)\n' \
  "$(awk '$2 ~ /^http_events_/ && $6=="open" {s+=$5} END {print s+0}' <<<"$LIST")" \
  "$(awk '$2 ~ /^http_events_/ && $6=="open"' <<<"$LIST" | grep -c . || true)"

# The two failure modes that stall a run silently, each with the window it was seen in. Both read
# as "slow" rather than "stuck" if you only watch row counts.
printf '  last 10m: %s voided batch(es), %s materializer OOM(s)\n\n' \
  "$(docker logs kontra-campaign-worker --since 10m 2>&1 | grep -c 'batch voided' || true)" \
  "$(docker logs kontra-api --since 10m 2>&1 | grep -c 'Out of Memory' || true)"
