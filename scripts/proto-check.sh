#!/bin/bash
# THE PROTO GATE — `buf lint` + `buf breaking`, run by `make proto-check` and by the `proto` job
# in .github/workflows/ci.yml. buf.yaml lives at the repo ROOT (module path `contracts`), so
# everything here runs from the top level, whatever directory make was invoked from.
#
# THE BASELINE IS THE SUBSTANCE OF THIS SCRIPT. `buf breaking` needs something to diff against,
# and the convenient spellings are wrong in the two ways that matter. Measured, buf 1.70, on a
# fixture repo where a renumbered field was COMMITTED and one more commit added on top:
#
#   --against '.git'             exit 0. No ref means buf diffs the working tree against HEAD, so
#                                a break that is already committed sits on BOTH sides.
#   --against '.git#ref=HEAD'    exit 0. The same comparison, spelled out. A field renumbered two
#                                commits ago reads clean and the branch merges.
#   --against '.git#branch=main' catches that one — and on a fixture where MAIN moved on and the
#                                branch never touched a .proto, it reports `Previously present
#                                field "9" ... was deleted` for a field somebody else ADDED to
#                                main. A gate that blames you for other people's commits is a gate
#                                that gets a `|| true` bolted onto it. It also needs a LOCAL main
#                                branch, which a CI checkout (detached HEAD) does not have.
#
# What you want is the FORK POINT: `git merge-base HEAD <the branch this forked from>`. It answers
# "what does THIS branch change about the contract" — the question a reviewer is asking — and
# answers it the same after ten commits as after one, whatever main did meanwhile. Sitting on an
# up-to-date main the merge base IS the tip, so an uncommitted edit is still checked.
#
# AND A BASELINE IT CANNOT READ MUST BE AN ERROR, NOT A PASS: an unresolvable ref, no common
# ancestor (the shape a shallow CI clone takes), a baseline commit carrying no protos. buf itself
# refuses the last one today ("Module "path: "contracts"" had no .proto files") but it names an
# input, not WHICH input or at which commit, and nothing pins that behaviour — it is the only
# thing standing between a mistyped PROTO_BASELINE and a green gate. The checks below answer it
# here instead. The gate also PRINTS what it compared against, because the one thing you cannot
# tell from a passing breaking check is whether it had anything to read.
#
# Overrides, both honoured from the environment (the Makefile exports them):
#   BUF=/path/to/buf          skip the search below
#   PROTO_BASELINE=<ref>      the branch this work forked from; CI passes the PR's base branch
set -uo pipefail

fail() { echo "proto-check: $*" >&2; exit 1; }

root=$(git rev-parse --show-toplevel 2>/dev/null) ||
  fail "not inside a git checkout — 'buf breaking' has no history to diff against here"
cd "$root" || fail "cannot enter $root"

# buf IS NOT RELIABLY ON PATH, and a target that assumes it is only works on the machine it was
# written on. CI installs it with buf-setup-action (on PATH, version-pinned), the justfile vendors
# a copy into .venv/bin, and `go install` drops it in GOPATH/bin — which is on nobody's PATH by
# default and is exactly where this repo's copy lives.
buf=${BUF:-}
if [ -z "$buf" ]; then
  gobin=$(go env GOPATH 2>/dev/null)
  [ -n "$gobin" ] && gobin="$gobin/bin/buf"
  for cand in "$(command -v buf 2>/dev/null)" ".venv/bin/buf" "$gobin" "$HOME/go/bin/buf"; do
    if [ -n "$cand" ] && [ -x "$cand" ]; then
      buf=$cand
      break
    fi
  done
fi
if [ -z "$buf" ]; then
  fail "buf not found. Looked on PATH, in .venv/bin/buf, and in \$(go env GOPATH)/bin/buf.
  Install it:  go install github.com/bufbuild/buf/cmd/buf@v1.70.0   (the version CI pins)
  or point at one:  make proto-check BUF=/path/to/buf"
fi
[ -x "$buf" ] || fail "BUF=$buf is not an executable"

# The default is the fork point against the default branch. `origin/main` first because that is
# what a clone has and it cannot go stale; bare `main` as the fallback for a checkout with no
# remote (a fixture repo, a mirror).
if [ -n "${PROTO_BASELINE:-}" ]; then
  candidates=("$PROTO_BASELINE")
else
  candidates=(origin/main main)
fi
ref=""
for c in "${candidates[@]}"; do
  if git rev-parse --verify --quiet "$c^{commit}" >/dev/null; then
    ref=$c
    break
  fi
done
[ -n "$ref" ] || fail "no baseline ref resolves (tried: ${candidates[*]}).
  Fetch it:  git fetch origin main
  In CI, check out with fetch-depth: 0 — a shallow clone has no branch to fork from.
  Or name one:  make proto-check PROTO_BASELINE=<ref>"

# RESOLVED TO A SHA ON PURPOSE. buf's git input hands the ref straight to git inside a fresh
# clone, where a revision EXPRESSION is not a ref: `.git#ref=HEAD^` dies with "pathspec 'HEAD^'
# did not match any file(s) known to git". merge-base gives a full commit id, so PROTO_BASELINE
# can be a branch, a sha or `HEAD^` and buf still gets something it can check out.
base=$(git merge-base HEAD "$ref" 2>/dev/null) ||
  fail "HEAD and $ref have no common ancestor — usually a shallow clone (git fetch --unshallow,
  or actions/checkout with fetch-depth: 0). Refusing to guess a baseline."
[ -n "$base" ] || fail "git merge-base HEAD $ref produced nothing"

# A BASELINE WITH NO CONTRACTS IN IT, CAUGHT HERE RATHER THAN HOPED FOR. Pointed at a commit
# from before the protos existed, buf 1.70 fails with `Module "path: "contracts"" had no .proto
# files` — which does not say whether the empty side was the baseline or the working tree, and is
# a buf behaviour this repo does not control. Said here it names the commit.
# THE BASELINE'S OWN buf.yaml, NOT THE WORKING TREE'S. These are different files whenever the
# module MOVES, and reading the wrong one turns a rename into a hard failure that blames the
# baseline: `shared/contracts` was `contracts` until ADR 0041's restructure, so this check looked
# for the new path in a commit that has the old one and refused every ref in history. buf itself
# was never confused — it checks out the baseline WITH its buf.yaml, which is self-consistent —
# so the gate was failing on its own pre-flight and not on the contract.
paths=$(git show "$base:buf.yaml" 2>/dev/null | sed -n 's/^[[:space:]]*-[[:space:]]*path:[[:space:]]*//p')
# No buf.yaml at the baseline at all: fall back to the working tree's, which is the pre-buf.yaml
# case the original line assumed.
[ -n "$paths" ] || paths=$(sed -n 's/^[[:space:]]*-[[:space:]]*path:[[:space:]]*//p' buf.yaml)
[ -n "$paths" ] || paths="."
# shellcheck disable=SC2086 -- $paths is a list of module paths and must word-split
if ! git ls-tree -r --name-only "$base" -- $paths | grep -q '\.proto$'; then
  fail "baseline $base carries no .proto under: $paths
  There is no contract at that commit to compare against, so a 'clean' result would mean nothing.
  Pick a baseline that has the contracts (PROTO_BASELINE=<ref>)."
fi

echo "proto gate: $("$buf" --version 2>/dev/null) | baseline $ref -> $(git log -1 --format='%h %s' "$base")"

"$buf" lint
status=$?
if [ $status -ne 0 ]; then
  echo "
proto-check: BUF LINT FAILED (the violations above). Fix the .proto — or, if the rule genuinely
  does not apply to this file, add it under lint.except in buf.yaml WITH the reason (the RPC_*
  entries there are the precedent, not a licence)." >&2
  exit $status
fi

"$buf" breaking --against ".git#ref=$base"
status=$?
if [ $status -ne 0 ]; then
  echo "
proto-check: BUF BREAKING FAILED against $base ($ref). Each line above is a consumer that would
  read the wrong bytes: a reused or renumbered field number, a deleted field/message/file, a
  changed type. Append a NEW field number instead of reusing one, and put a deleted number in
  'reserved'. If the break is deliberate it needs an ADR and a flag day (ADR 0023 is what one
  looks like) — not a silenced gate." >&2
  exit $status
fi

echo "proto gate: OK — lint clean, no breaking change against $base"
