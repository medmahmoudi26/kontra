-- The canonical paid-scope extract: bbscope's Postgres -> TSV for scripts/normalize-scope.py.
--
--   psql "$BBSCOPE_DSN" -tAF$'\t' -f scripts/extract-scope.sql \
--     | python3 scripts/normalize-scope.py \
--     | xargs -I{} kontra dataset create {} scope_paid
--
-- THE FILTER LIVES HERE, not in the bbscope poll. Poll with `--category all` and NO `-b`:
-- `-b` filters at FETCH time, so it does not merely narrow one export, it narrows the DATABASE —
-- a `-b` poll on 2026-08-04 dropped `targets_raw` from 42,800 rows to 15,615 and erased every
-- in-scope-but-unpaid asset, `api.example.com` included. Fetch everything, decide here, and the
-- other reading stays one query away instead of one 20-minute re-poll away.
--
-- WHICH ASSETS COUNT AS "PAID" — the distinction that actually bites:
--
-- HackerOne marks eligibility PER ASSET, and its two flags mean different things:
--   eligible_for_submission  -> the green "In scope" badge. You may test it; safe harbour applies.
--   eligible_for_bounty      -> whether that particular asset pays.
--
-- example-bounty publishes 135 assets: 22 submittable AND bounty-eligible, 87 submittable with NO
-- bounty, 26 not submittable. `api.example.com` is Critical severity, in scope, and pays nothing —
-- as is `*.example.com` itself. Filtering on the per-asset bounty flag therefore drops most of a
-- paying program's attack surface, including its widest wildcards.
--
-- The rule is "no VDPs, only paid programs" — a PROGRAM-level rule. So: keep every asset a
-- program accepts submissions on, provided that program pays a bounty on at least one asset.
-- A program that pays on nothing is a VDP and is excluded entirely.
--
-- `is_bbp` is still carried through to the dataset, so a run that wants only
-- bounty-eligible targets filters on it at dispatch time rather than re-deriving scope.
SELECT
    -- Targets are free text from the platform and at least one begins with a literal TAB, which
    -- shifts every column right and silently reassigns target -> platform -> program. Two rows
    -- reached the scope table that way on 2026-08-04; they classified `unexpandable` with an
    -- empty seed, so they could never be scanned, but the metadata was wrong. Neutralise the
    -- delimiter at the source.
    translate(t.target, E'\t\n\r', '   '),
    p.platform,
    p.handle,
    p.url,
    t.is_bbp
FROM targets_raw t
JOIN programs p ON p.id = t.program_id
WHERE t.in_scope = 1
  AND p.disabled = 0
  AND p.is_ignored = 0
  -- not a VDP: this program pays a bounty on at least one of its assets
  AND EXISTS (
        SELECT 1 FROM targets_raw x
         WHERE x.program_id = t.program_id AND x.is_bbp = 1
      );
