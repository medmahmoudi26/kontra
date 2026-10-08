#!/usr/bin/env bash
# build-cluster-images.sh — every image docker-compose.yml needs, built from THIS commit.
#
#   scripts/build-cluster-images.sh <sibling-dir>
#
# `<sibling-dir>` is the parent directory holding `kontra/` and `kontra-console/`, because that is
# `Dockerfile.selfcontained`'s build context: the console resolves `@kontra/core` as
# `link:../kontra/shared/core` — a filesystem path, not a published package — so both checkouts
# have to be visible to ONE build and both directory names are load-bearing.
#
# A SCRIPT RATHER THAN A `run:` BLOCK, because two CI jobs need the same images and a duplicated
# thirty lines of YAML is a thing that rots in one copy. It is also the only way this is runnable
# locally, which is the same argument `scripts/assert-install-is-self-contained.py` makes for
# itself: a checker embedded in a workflow is something nothing can run and nothing can test.
#
# `:ci` RATHER THAN `:dev`. `:dev` is what ghcr serves and what compose defaults to, so tagging it
# here would make `pull_policy: never` succeed against whatever was pulled last. The install under
# test has to name these tags explicitly.
set -euo pipefail

SIBLING="${1:?usage: build-cluster-images.sh <dir containing kontra/ and kontra-console/>}"
SIBLING="$(cd "$SIBLING" && pwd)"
REPO="$SIBLING/kontra"
VERSION="${KONTRA_CI_VERSION:-0.0.0-ci}"

for d in "$REPO" "$SIBLING/kontra-console"; do
  [ -d "$d" ] || { echo "no $d — Dockerfile.selfcontained needs both checkouts side by side" >&2; exit 2; }
done

echo "==> kontra:ci from source, in docker (context $SIBLING)"
DOCKER_BUILDKIT=1 docker build -f "$REPO/control/images/Dockerfile.selfcontained" \
  --build-arg VERSION="$VERSION" -t kontra:ci "$SIBLING"

# THE ORCHESTRATOR TAKES ITS CONSOLE OUT OF `kontra:ci`, so the order is mandatory, not tidy:
# `Dockerfile.orchestrator:2-3` is `ARG SPA_IMAGE` / `FROM ${SPA_IMAGE} AS spa-src`.
echo "==> kontra-orchestrator:ci"
docker build -f "$REPO/control/images/Dockerfile.orchestrator" \
  --build-arg SPA_IMAGE=kontra:ci -t kontra-orchestrator:ci "$REPO"

# The log shipper and its parser. It is an image rather than two bind mounts because
# `tests/test_logline.py` loads the parser BY PATH and it must stay a file — see
# control/images/Dockerfile.logship. Without it, `pull_policy: never` fails on a missing image.
echo "==> kontra-logship:ci"
docker build -f "$REPO/control/images/Dockerfile.logship" -t kontra-logship:ci "$REPO"

# PORTER, WHICH IS THE ONE IMAGE HERE NOT BUILT FROM THIS COMMIT — and it is built anyway, for a
# reason worth stating. `Dockerfile.porter` clones `github.com/TFMV/porter` at a pinned ref, so
# nothing about this commit changes what comes out. It is here because the alternative is not
# available: compose starts porter unconditionally (no `profiles:`) and its default tag is served
# from ghcr, so under `KONTRA_PULL_POLICY=never` a missing local copy fails the whole `up` with
#   Error response from daemon: No such image: ghcr.io/medmahmoudi26/kontra-porter:dev
echo "==> kontra-porter:ci"
docker build -f "$REPO/control/images/Dockerfile.porter" -t kontra-porter:ci "$REPO"

# ── WHAT WAS BUILT, AS AN ASSERTION AND NOT A SUMMARY LINE ──────────────────────────────────────
#
# THIS WAS A DECORATIVE `docker images | grep` AND IT FAILED THE WHOLE SCRIPT. Two things met: the
# `grep` was anchored to two leading spaces from a `--format '  {{.Repository}}…'` template, and
# DOCKER STRIPS LEADING WHITESPACE FROM A FORMAT STRING — measured with `cat -A`, the lines begin at
# the repository name. So the anchor could never match, `grep` exited 1 on no match, `pipefail`
# turned that into the script's exit code, and both CI jobs that call this died AFTER building every
# image, having printed the word "built:" and nothing else.
#
# The lesson is the shape rather than the regex: a line that only REPORTS must not be able to fail
# the thing it reports on. So this does not report — it asserts, by name, with `docker image
# inspect`, which cannot match nothing and says which image is absent when one is.
echo
for img in kontra:ci kontra-orchestrator:ci kontra-logship:ci kontra-porter:ci; do
  size=$(docker image inspect "$img" --format '{{.Size}}' 2>/dev/null) || {
    echo "::error::$img was not built, and this script claimed to build it" >&2
    exit 1
  }
  printf 'built %-28s %s MB\n' "$img" "$((size / 1000000))"
done
