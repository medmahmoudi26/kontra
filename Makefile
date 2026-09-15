# Kontra's build and infra targets (see docs/wiki/Deployment.md).
#
# **THE LOCAL CONTROL PLANE IS THE COMPOSE CLUSTER (ADR 0047).** `make up` starts
# docker-compose.yml, which includes docker-compose.quickstart.yml. `kontra up` remains in the
# binary and is not a supported install path.
.PHONY: up up-d down logs ui api bundle proto-check image release worker-base

# THERE IS NO `tmux-dir` TARGET ANY MORE, and its absence is the fix rather than a deletion.
#
# It prepared the HOST's tmux socket directory, for a bind mount docker-compose.yml never had:
# the comment described `orchestrator-infra` mounting it so the panels streamer could watch local
# sessions, and no such volume was ever declared. The served worker does not run on the host either
# — it runs in orchestrator-api, which is the only place with the CLI, python3 and the SDK — so a
# socket on the host would have been the wrong server to watch even if it had been mounted.
#
# The two containers share one tmux server over the `tmux-sock` VOLUME now (TMUX_TMPDIR in
# docker-compose.yml). Docker owns that directory's mode, so there is nothing to prepare here.

# THE COMPOSE CLUSTER (ADR 0047), including orchestrator-infra for dockerFleet and optional
# DigitalOcean. `kontra up` is not this target.
up:
	docker compose up

up-d:
	docker compose up -d

# THE SPA, WITHOUT THE 13-MINUTE IMAGE BUILD.
#
# `docker compose build orchestrator-api` runs `vite build` INSIDE the image. Measured on a 4 GB
# controller: load average 137, the API stopped answering, Temporal was restarted by the kernel and
# an in-flight fleet run sat idle for 45 minutes. It is a build-time cost, not a runtime one — the
# control plane itself is under 1 GB resident — but it must not run next to anything live.
#
# The container serves plain files from /app/web/dist, so iterating does not need the image at all.
# `VITE_KONTRA_EXPLORE_TOKEN` is injected at BUILD time and the query routes are bearer-gated, so a
# bundle built without it renders a Datasets console that 401s and silently shows an empty grid.
#
# The container is found BY LABEL, not by `docker compose ps`. Compose derives its project name
# from the DIRECTORY, so a checkout under a git worktree resolves a different project than the one
# the stack is actually running as, and `ps -q` answers with nothing — which `docker cp` reports as
# "must specify at least one container source", naming neither the cause nor the fix.
# `assets/` IS CLEARED BEFORE THE COPY, because `docker cp` merges and never deletes. Vite names
# every chunk by content hash, so each redeploy left its whole build behind: 22 ActorsPage-*.js and
# 4 ScratchPage-*.js had accumulated in a container serving exactly one of each. Nothing was
# BROKEN by that — index.html names the one graph to load — but it made the deployed bundle
# unusable as evidence. Grepping the served assets for a feature hits a fossil and answers "yes"
# whichever build is live, which is how a stale deploy gets confirmed as a fresh one. It also grows
# without bound on a box that has already gone down once on disk.
#
# index.html is NOT the thing to clear: it is overwritten by the copy, and it is the only file that
# says which chunks are current.
#
# AND THE CONTAINER MAY NOT EXIST AT ALL ANY MORE (ADR 0031 §1, issue 14). `orchestrator-api` left
# docker-compose.yml: the control plane is `kontra up`'s supervised child. On that topology the
# vite build above is already the whole deploy — `--orchestrator=local` serves
# `frontend/dist` straight out of the checkout — so the absence of a container is a
# SUCCESS to report, not a failure to exit on. It used to `exit 1` with a sentence naming a
# service this file no longer defines, which is the trap this slice was told not to reproduce.
# THE SOURCE IS IN ANOTHER REPOSITORY NOW (ADR 0038/0041). `frontend/` moved to kontra-console, so
# this target builds from a SIBLING CHECKOUT and says so plainly when there is not one — the failure
# it replaces was `cd: frontend: No such file or directory`, which names neither the cause nor
# where the code went. Override with `make ui CONSOLE=/path/to/kontra-console`.
#
# `@kontra/core` IS BUILT FIRST, and that is not belt-and-braces: the console links it from this
# checkout, so a console built against a stale `core/dist` is a console that disagrees with the
# orchestrator it is about to be deployed next to.
#
# ABSOLUTE, AND THAT IS A FIX RATHER THAN A STYLE. `release` below runs its compiler from `cli/`,
# so a relative `../kontra-console` meant two different directories in the same target: the guard
# checked `<repo>/../kontra-console/dist` and PASSED, and the command that followed looked in
# `<repo>/cli/../kontra-console/dist` and failed with an unrelated message about a missing SPA. A
# guard that tests a path its command does not use is worse than no guard — it reports the healthy
# case and then fails somewhere the operator has no reason to connect to it.
CONSOLE ?= $(abspath $(CURDIR)/../kontra-console)

# ── THE CONTAINER IMAGE, FROM A RELEASE — the compose install path ─────────────────────────────
#
# Two steps and the order is the point: `kontra release` produces exactly what a user downloads,
# and the image is built FROM that tarball. The container and the download are then the same bytes,
# which is what makes this "verify our builds" rather than a second way to compile.
#
# THE SPA IS THE ONE THING THE RELEASE DOES NOT BUILD ITSELF (its toolchain lives in another
# repository — ADR 0038), so `KONTRA_CONSOLE_DIST` points at a build you have. `make ui` makes one.
# `CONSOLE` is defined once, above — a second `?=` here was a no-op that read like the definition.
PLATFORM ?= linux/amd64
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.0.0-dev)

# ── THE SPA, FROM THE SIBLING CHECKOUT — and this is what makes `make image` one command ────────
#
# The console is a separate repository (ADR 0038) and the release ARCHIVES a vite output it does not
# build. That used to mean three commands nobody had been told about, and a fresh clone died on the
# first of them with a message that assumed the checkout already existed.
#
# IT MUST BE A SIBLING NAMED `kontra-console`, AND THIS CHECKOUT MUST BE NAMED `kontra`. The console
# resolves `@kontra/core` as `link:../kontra/shared/core` — a filesystem path, not a published
# package — so both names are load-bearing and a clone into `kontra-oss/` fails inside pnpm rather
# than here.
#
# IDEMPOTENT. Both installs are no-ops once done, so `make image` twice costs one build.
$(CONSOLE):
	@test -d "$(CONSOLE)" || { \
	  echo "no console checkout at $(CONSOLE) — the SPA lives in kontra-console since ADR 0038."; \
	  echo ""; \
	  echo "  git clone https://github.com/medmahmoudi26/kontra-console $(CONSOLE)"; \
	  echo ""; \
	  echo "It has to sit BESIDE this checkout and be named kontra-console, because the console"; \
	  echo "resolves @kontra/core as link:../kontra/shared/core. Or point at one you have:"; \
	  echo "  make image CONSOLE=/path/to/kontra-console"; \
	  exit 1; }

# The HOST-side SPA build. `make image` no longer needs it — it builds the SPA in Docker — but
# `make ui` (deploy into a running container) and `make image-from-release` still do.
console: $(CONSOLE)
	pnpm install --frozen-lockfile
	pnpm --filter @kontra/core run build
	cd "$(CONSOLE)" && pnpm install --frozen-lockfile && pnpm run build

release: console
	cd cli && KONTRA_CONSOLE_DIST="$(CONSOLE)/dist" go run . release \
	  --repo .. --platform $(PLATFORM) --version $(VERSION) --out ../build/release

# THE TARBALL IS FOUND, NOT NAMED. It carries its version and platform in the filename, and a
# hard-coded one is how an image quietly keeps shipping last month's binary. Exactly one must match:
# a `build/release` with two is ambiguous and saying so beats picking.
# ── THE ONE THAT NEEDS ONLY DOCKER ──────────────────────────────────────────────────────────────
#
# Everything is built INSIDE the image: both pnpm installs, the SPA, the Go binary and the
# orchestrator bundle. No Go, no Node, no pnpm on your machine.
#
# THAT IS NOT A CONVENIENCE, IT IS THE ONLY WAY THIS BUILDS ON macOS. The console has ten pairs of
# files whose names differ only in case (`WorkflowThread.tsx` beside `workflowThread.ts`, and nine
# more). On a case-sensitive filesystem they are two modules; on the case-INSENSITIVE one macOS
# gives you by default, `tsc` resolves `./WorkflowThread` to both and stops with 102 errors about
# names that "differ only in casing". Copying the sources onto the image's own layer makes the
# compiler see what Linux CI sees, and nothing about the console had to change.
#
# THE CONTEXT IS THE PARENT DIRECTORY because the console is a separate repository and `link:` is a
# filesystem path — both checkouts have to be visible to one build.
image: $(CONSOLE)
	DOCKER_BUILDKIT=1 docker build -f control/images/Dockerfile.selfcontained \
	  --build-arg VERSION=$(VERSION) \
	  -t "$${KONTRA_IMAGE:-kontra:latest}" "$(dir $(CURDIR))"
	$(MAKE) worker-base
	@echo
	@echo "  docker compose -f docker-compose.quickstart.yml up -d"
	@echo "  open http://127.0.0.1:8088"

worker-base:
	docker build -f control/images/Dockerfile.workerbase -t kontra-worker-base:1 .

# `make image-from-release` — the OLDER path, kept because it is the one that proves the container
# and the published tarball are the same bytes. It needs the host toolchain; `make image` does not.
image-from-release: release
	@set -eu; \
	  n=$$(ls build/release/kontra_*.tar.gz 2>/dev/null | wc -l); \
	  [ "$$n" = 1 ] || { echo "expected exactly one tarball in build/release, found $$n"; exit 1; }; \
	  tarball=$$(ls build/release/kontra_*.tar.gz); \
	  echo "==> image from $$tarball"; \
	  docker build -f control/images/Dockerfile.appliance --build-arg TARBALL="$$tarball" \
	    -t "$${KONTRA_IMAGE:-kontra:latest}" .
	@echo
	@echo "  docker compose up -d      # the control plane"
	@echo "  open http://127.0.0.1:8088"

# A `.env` GUARD USED TO BE HERE AND ADR 0045 INVERTED IT. It refused to build without one,
# because "the SPA would build with an EMPTY explore token" — back when the console authenticated
# with `VITE_KONTRA_EXPLORE_TOKEN` baked in at build time. The console signs in for a session token
# at runtime now, and `vite.config.ts` REFUSES to build if that variable is set at all: a token in
# the artifact cannot rotate, is identical for every operator, and outlives the container in any
# image that keeps it. So an absent `.env` is the correct state, and the guard was stopping a fresh
# clone from building for a reason that had become the opposite of true.
# WHERE THE CONTAINER ACTUALLY SERVES THE SPA FROM. This said `/app/web/dist`, which is the
# APPLIANCE's layout and does not exist in `Dockerfile.orchestrator` — its WORKDIR is
# /src/control/orchestrator and `server.ts` resolves `web/dist` relative to that. So `make ui`
# deleted nothing and copied into a path docker created on the fly, and the console in the browser
# never changed.
ORCH_WEB := /src/control/orchestrator/web/dist

# AND THE SERVER'S OWN PATH, which had the same phantom. `api` copied into `/app/dist` — the
# APPLIANCE's layout, absent from `Dockerfile.orchestrator`, whose WORKDIR is
# /src/control/orchestrator. `docker cp` CREATES a missing destination rather than refusing, so the
# target printed "deployed and restarted" and restarted the container onto the code it already had.
# MEASURED: `docker exec kontra-api ls -d /app/dist` -> no such file, while the process runs from
# /src/control/orchestrator/dist. A deploy that reports success and changes nothing is worse than
# one that fails, and it is the same mistake `ORCH_WEB` above exists to correct.
ORCH_DIST := /src/control/orchestrator/dist

ui: console
	@api=$$(docker ps -q --filter label=com.docker.compose.service=orchestrator-api | head -1); \
	  if [ -z "$$api" ]; then \
	    echo "no orchestrator-api container (it left docker-compose.yml — ADR 0031 §1)."; \
	    echo "  the build above IS the deploy for 'kontra up --orchestrator=local'."; \
	    echo "  for the hydrated bundle instead:  kontra bundle spa  (then restart 'kontra up')"; \
	    exit 0; \
	  fi; \
	  docker exec "$$api" rm -rf $(ORCH_WEB)/assets; \
	  docker cp "$(CONSOLE)"/dist/. "$$api":$(ORCH_WEB)/ && echo "deployed the SPA to $$api"

# THE SERVER, WITHOUT THE IMAGE BUILD EITHER — the `ui` target's twin.
#
# `tsc` writes plain JS into control/orchestrator/dist, which is exactly what the containers run, so a
# backend change reaches a live stack by copying files and restarting a node process. Every
# container gets the whole tree because they run DIFFERENT code out of it: HTTP routes and the
# dataset/fleet activities are orchestrator-api's — API and materializer are two ROLES of one
# process since ADR 0031 §1 — and the Monitor's tmux streamer and the Pulumi engine are
# orchestrator-infra's. Copying only the file you think you changed is how a shared module goes
# stale in one of them.
#
# TWO SERVICES, NOT THREE, and that is the merge rather than an omission: `orchestrator-materializer`
# left docker-compose.yml. A stack still running one from before the merge will fail the check
# below — recreate it with `make up-d`, because that container is running a role this one now
# serves and two pollers on `kontra-materializer` is exactly what the merge removed.
#
# ORCHESTRATOR-INFRA IS ON THIS LIST, and it was missing. It runs `panels/` — the discovery loop,
# the list-panes parse, the SSH transport — so a change to how the Monitor finds sessions reached
# the API, passed its tests, and left the streamer running the previous build. The symptom is the
# worst kind: a served worker that is up and polling, and a wall that does not show it, with nothing
# anywhere reporting a version difference.
#
# BY LABEL, not by `docker compose ps`, for the reason recorded on `ui`.
#
# A restart, not a recreate: hot-copied files survive the former and are lost to the latter.
#
# ONE OF THE TWO IS NOT A CONTAINER ANY MORE (ADR 0031 §1, issue 14). `orchestrator-api` left
# docker-compose.yml, and on the appliance the `tsc` above is the whole deploy: `kontra up
# --orchestrator=local` runs `control/orchestrator/dist` out of the checkout and prints on every start
# which orchestrator it chose. So a missing api container is REPORTED and skipped rather than
# failing the target — a recipe that exits 1 naming a service this file no longer defines is the
# same silent-mismatch trap that `kontra infra up` reverting a hot-copied container was.
api:
	cd control/orchestrator && pnpm exec tsc
	@for svc in orchestrator-api orchestrator-infra; do \
	  cid=$$(docker ps -q --filter label=com.docker.compose.service=$$svc | head -1); \
	  if [ -z "$$cid" ]; then \
	    if [ "$$svc" = "orchestrator-api" ]; then \
	      echo "no orchestrator-api container (it left docker-compose.yml — ADR 0031 §1)."; \
	      echo "  the tsc above IS the deploy for 'kontra up --orchestrator=local'; restart it to pick it up."; \
	      echo "  for the hydrated bundle instead:  kontra bundle orchestrator"; \
	      continue; \
	    fi; \
	    echo "no running $$svc container — start the stack with 'make up-d' first"; exit 1; \
	  fi; \
	  docker cp control/orchestrator/dist/. "$$cid":$(ORCH_DIST)/ && docker restart "$$cid" >/dev/null && echo "deployed and restarted $$svc ($$cid)"; \
	done
	@old=$$(docker ps -q --filter label=com.docker.compose.service=orchestrator-materializer | head -1); \
	  test -z "$$old" || { \
	    docker cp control/orchestrator/dist/. "$$old":$(ORCH_DIST)/ && docker restart "$$old" >/dev/null; \
	    echo "also refreshed orchestrator-materializer ($$old) — a PRE-MERGE container this compose"; \
	    echo "  file no longer defines. It is polling kontra-materializer beside orchestrator-api's"; \
	    echo "  own materializer role; recreate the stack ('make up-d') to be rid of it."; \
	  }

# THE APPLIANCE BUNDLE (ADR 0031 §2) — a pinned Node runtime, the compiled orchestrator and its
# native addons, as one content-addressed tar.gz with a manifest.
#
# NOT PART OF `make up`, and not a dependency of anything here. This target produces an ARTIFACT
# for the binary to carry; the compose control plane above runs the orchestrator out of an image
# and needs none of it. Building one takes about four minutes and half a gigabyte of working
# space, which is not a thing to do as a side effect of another verb.
#
# The work is in `kontra bundle orchestrator` rather than in this recipe, because a bundle whose
# digest is meaningful needs a deterministic tar writer, a digest-verified fetch and a credential
# scan — none of which fits in a make recipe, and all of which have tests
# (cli/appliance/*bundle*_test.go).
#
#   make bundle                  # build for this host into build/bundles/
#   make bundle OUT=/mnt/big     # somewhere with more room
#   make bundle PLATFORM=linux/arm64
BUNDLE_FLAGS = $(if $(OUT),--out $(OUT)) $(if $(PLATFORM),--platform $(PLATFORM))
bundle:
	cd cli && go build -o ../build/kontra-bundler .
	./build/kontra-bundler bundle orchestrator $(BUNDLE_FLAGS)

down:
	docker compose down

logs:
	docker compose logs -f

# THE PROTO GATE — `buf lint` + `buf breaking` over the `contracts` module.
#
# buf was installed and configured (buf.yaml: lint BASIC, breaking FILE) and NOTHING RAN IT from
# here: the only breaking check lived inline in the CI workflow, spelled differently, and was
# advisory. A contract checked when somebody remembers is not a contract, and a check that exists
# only in a YAML file is one nobody can run before pushing.
#
# The work is in scripts/proto-check.sh, because picking the baseline is the whole job and it does
# not fit in a recipe line: `buf breaking` with no ref diffs the working tree against HEAD, so an
# already-committed break sits on both sides and PASSES (measured, buf 1.70). The gate diffs
# against the merge base with the branch this forked from, and refuses a baseline it cannot
# resolve — a comparison against nothing is indistinguishable from a clean one.
#
# Overrides only reach the script if they are EXPORTED — `make proto-check BUF=...` sets a make
# variable, and a recipe that never sees it would silently run the wrong buf:
#   make proto-check BUF=/path/to/buf          the buf to run (default: PATH, .venv, GOPATH/bin)
#   make proto-check PROTO_BASELINE=<ref>      what to diff against (default: origin/main)
export BUF
export PROTO_BASELINE
proto-check:
	@bash scripts/proto-check.sh

# An example actor's OWN tests, one process per directory.
#
# They cannot join the root `pytest` run: the actor registry is a process-global singleton (one
# actor per worker is the production shape), so importing webcrawl and crawl4ai into the same
# session raises `@actor.method name 'crawl' declared twice`. So `testpaths` stays ["tests"] and
# the examples get their own processes here.
#
# This target exists because it was missing. The ADR 0028 migration to
# `async def m(self, batch, dataset)` left both actors' tests on the old one-arg call, and every
# mandated suite stayed green for a week because nothing ran them.
#
# `PYTHON` is resolved rather than hardcoded to `python`. CI gets a bare `python` from
# `actions/setup-python`, but a Debian host has only `python3` — so the target that exists to stop
# these tests being skipped silently failed on this very box with `/bin/sh: python: not found`,
# which is exactly the shape of failure it was written to prevent. Override for a venv:
# actors are in kontra-actors and run their suites there.
PYTHON ?= $(shell command -v python || command -v python3)

# `E2E=1` IS WHAT MAKES THIS A GATE ON THE BINARY (ADR 0031 §5, issue 18).
#
# The default run deselects `e2e` and every example actor runs in-process against
# `kontra.testing`'s stubs — so it is a real gate on the actor-facing API and, in the ADR's own
# words, "it would go green against a binary that never started". `E2E=1` stops deselecting, which
# turns on the tests that dial a control plane at `$$KONTRA_ADDRESS` and dispatch a real Batch to
# a real Worker.
#
# THE TWO RUNS ARE THE COMPARISON. `scripts/parity-gate.sh` runs this target twice on one commit —
# once with no control plane, once against `kontra up` — which is the "parity is a comparison
# rather than an assertion" the issue asks for, in the only form still available now that compose
# no longer defines a control plane to be the other half of it.
#
# An e2e test with nothing to dial SKIPS, so `E2E=1` on a bare machine is green and empty. That is
# the trap, not the feature: the gate counts the legs that actually ran rather than reading this
# target's exit code, and you should too.
# TWO MARKERS, NOT ONE, and the second exists because the first meant two things.
#
#   default    -m 'not e2e and not control_plane'   the SDK-side contract: every example actor
#                                                   in-process against kontra.testing's stubs,
#                                                   no browser, no control plane
#   E2E=1      -m 'not e2e'                          the same set PLUS the tests that dial a real
#                                                   control plane at $$KONTRA_ADDRESS
#
# `e2e` still means "brings its own heavy runtime" — crawl4ai and webcrawl mark their live tests
# that way for Playwright, and CI installs no Chromium — so E2E=1 does NOT select it. Selecting
# plain `-m e2e` to reach the appliance leg pulled the browser tests in with it and failed on
# `BrowserType.launch: Executable doesn't exist`, which says nothing about the binary.
EXAMPLES_MARK = $(if $(E2E),-m 'not e2e',-m 'not e2e and not control_plane')

# EXIT 5 IS NOT A FAILURE HERE, AND FINDING THAT OUT COST A GREEN SUITE.
#
# pytest returns 5 for "no tests were collected", which is what a directory whose ONLY test is
# `e2e`-marked produces under the default deselect — `examples/python/beacon` became exactly that
# the moment its e2e leg was added, and the loop below turned that 5 into a failed target while
# every suite in it had passed. So the codes are read one at a time instead of `|| rc=1`.
#
# What this deliberately does NOT do is treat 5 as success silently: it says which directory ran
# nothing, because "the suite is green and it ran no tests" is this file's own recorded failure —
# the ADR 0028 migration left two example suites broken for a week because nothing ran them.

# ONE VERB FOR "RUN THE EXAMPLES", fanning out to the two languages. The split exists because the
# two are built by completely different commands, not because they are different kinds of gate:
# both exist to stop an example actor rotting on a green branch. CI runs the halves separately —
# the Python job has no Go and the Go job has no venv — and `scripts/parity-gate.sh` wants the
# Python half alone, since its subject is parity against the appliance binary.
# test-examples, test-examples-python and test-examples-go MOVED TO kontra-actors (ADR 0038).
# There are no actors in this repository to run them against — `testdata/fixtureactor` is a
# fixture, exercised by `tests/test_fixture_actor_e2e.py` and by scripts/parity-gate.sh, not by a
# suite of its own. The `GOWORK=off` / `GOTOOLCHAIN=auto` / one-cd-per-module reasoning that used
# to live in test-examples-go went with it; that repo's CI carries the note.


GO_EXAMPLES ?= examples/go/*/

test-examples-go:
	@command -v go >/dev/null || { echo "no go on PATH" >&2; exit 1; }
	@rc=0; for d in $(GO_EXAMPLES); do \
	  test -f "$$d/go.mod" || continue; \
	  echo "== $$d"; \
	  ( cd "$$d" && GOWORK=off go build ./... && GOWORK=off go vet ./... && GOWORK=off go test ./... ) || rc=1; \
	done; exit $$rc
