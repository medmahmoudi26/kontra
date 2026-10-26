#!/usr/bin/env bash
# The helloworld seller: a2a-samples samples/python/agents/helloworld at 6603ba3, run unmodified (PRD §18:
# "can the helloworld sample still run unmodified?"). It is a2a-sdk 1.1.0 with a 1.0 card, JSON-RPC at
# http://127.0.0.1:9999; its __main__.py fixes that address, so it cannot be moved. The gate reaches it through
# `kontra connect` (M2), never directly.
#
#   A2A_SAMPLES   an a2a-samples checkout at 6603ba3 (default: a clone of that commit kept in
#                 ${XDG_CACHE_HOME:-$HOME/.cache}/kontra/a2a-samples, made on first run)
#   UV            the uv binary (default: uv on PATH)
#
# Nothing in the checkout is written: uv builds the environment in its own cache (--no-project: the
# repository's samples/python/pyproject.toml is not synced), and Python writes no __pycache__. The toolchain is
# pinned without touching the sample: CPython 3.12 and the requirements resolved as of the day after the
# commit (--exclude-newer), so the sample's unpinned uvicorn and sse-starlette resolve to what it was written
# against.
set -euo pipefail

pin=6603ba3f2c31a7ef33e70b9d8b5b5f8be42ac9a3
repo=https://github.com/a2aproject/a2a-samples
sample=samples/python/agents/helloworld
python=3.12
exclude_newer=2026-08-05T00:00:00Z

die() { echo "run.sh: $*" >&2; exit 1; }

uv="${UV:-uv}"
command -v "$uv" >/dev/null ||
  die "$uv not found: install uv (https://docs.astral.sh/uv/getting-started/installation/), or set UV to its path"

# The sample binds 127.0.0.1:9999 and nothing else. Anything answering there would take the sidecar's traffic.
if (exec 3<>/dev/tcp/127.0.0.1/9999) 2>/dev/null; then
  die "127.0.0.1:9999 is taken: the helloworld sample listens there and only there; stop what holds it first (ss -ltnp 'sport = :9999', or lsof -i :9999)"
fi

command -v git >/dev/null || die "git not found: it is needed to fetch and check the a2a-samples checkout"

dir="${A2A_SAMPLES:-${XDG_CACHE_HOME:-$HOME/.cache}/kontra/a2a-samples}"
if [[ -z "${A2A_SAMPLES:-}" && ! -e "$dir" ]]; then
  echo "run.sh: fetching $repo at ${pin:0:7} into $dir" >&2
  tmp="$dir.partial.$$"
  rm -rf "$tmp"
  mkdir -p "$(dirname "$dir")"
  trap 'rm -rf "$tmp"' EXIT
  git init -q "$tmp"
  git -C "$tmp" fetch -q --depth 1 "$repo" "$pin" || die "could not fetch $repo at $pin"
  git -C "$tmp" -c advice.detachedHead=false checkout -q FETCH_HEAD
  mv "$tmp" "$dir"
  trap - EXIT
fi

[[ -d "$dir/$sample" ]] || die "$dir has no $sample: set A2A_SAMPLES to an a2a-samples checkout"
head="$(git -C "$dir" rev-parse HEAD 2>/dev/null)" || die "$dir is not a git checkout: set A2A_SAMPLES to an a2a-samples checkout at $pin"
[[ "$head" == "$pin" ]] ||
  die "$dir is at $head, not at the pinned $pin: git -C $dir fetch $repo $pin && git -C $dir checkout $pin"
# Unmodified means exactly that: no change, no extra file, not even an ignored one, in the sample's directory.
changes="$(git -C "$dir" status --porcelain --ignored -- "$sample")"
[[ -z "$changes" ]] || die "$dir/$sample is not the unmodified sample (git -C $dir status --ignored -- $sample):
$changes"

cd "$dir/$sample"
export PYTHONDONTWRITEBYTECODE=1
exec "$uv" run --no-project --python "$python" --exclude-newer "$exclude_newer" \
  --with-requirements requirements.txt python __main__.py
