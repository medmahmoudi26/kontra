# Kontra's build and infra targets (see docs/wiki/Deployment.md).
#
# **THE LOCAL CONTROL PLANE IS `kontra up`, AND IT IS NOT IN THIS FILE.** One process: Temporal,
# the object store, the state store, the payload codec, the OCI registry, and the orchestrator as
# a supervised child. No containers, one data directory (ADR 0031). `scripts/parity-gate.sh` is
# what proves it, and is the command to run after touching anything on that path.
#
#   make up | up-d | down    # the CLOUD CONTROLLER's compose stack — see the note above `up`
.PHONY: up up-d down logs tmux-dir ui api bundle proto-check

# THE TMUX SOCKET DIRECTORY, MADE BEFORE COMPOSE CAN MAKE IT WRONG.
#
# `orchestrator-infra` bind-mounts the host's tmux socket dir so the panels streamer can watch
# local `kontra-*` sessions (ADR 0020 mode `local`). Docker creates a MISSING bind source itself,
# as root and mode 0755 — and tmux refuses a socket directory that is not 0700, so the first
# `tmux` after a reboot fails with a permissions error and the operator has no reason to connect
# that to a compose mount. `/tmp` is cleared on reboot, so this is not a rare case: it is every
# reboot.
#
# `install -d -m 700` is idempotent and fixes the mode on a directory docker already created.
tmux-dir:
	@install -d -m 700 "$${TMUX_TMPDIR:-/tmp}/tmux-$$(id -u)"

# THE CLOUD CONTROLLER, NOT THE LOCAL CONTROL PLANE. **For local development the command is
# `kontra up`** — one process holding Temporal, the object store, the state store, the codec, the
# OCI registry and the orchestrator, with no containers at all (ADR 0031).
#
# What these two targets start is what is LEFT in docker-compose.yml: `orchestrator-infra`, which
# holds the Pulumi engine, the fleet key and the cloud credential the appliance deliberately does
# not ship (ADR 0034 §1), and `orchestrator-probe`. That is the deployment that runs cloud
# runs, and it is why the file survives; its header says so at length.
#
# THEY WILL NOT GIVE YOU A CONTROL PLANE. `temporal`, `seaweed` and `redis` are not services here
# any more, so a `make up-d` on its own leaves you with two containers dialling a
# `host.docker.internal` that nothing is answering on. Run `kontra up --bind 172.17.0.1` beside
# them — and tell it to leave the third orchestrator role alone, or both poll `kontra-infra`:
#
#   KONTRA_ORCHESTRATOR_ROLES=api,materializer kontra up --bind 172.17.0.1
up: tmux-dir
	docker compose up

up-d: tmux-dir
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
CONSOLE ?= ../kontra-console

ui:
	@test -f .env || { echo "no .env at the repo root — the SPA would build with an EMPTY explore token"; exit 1; }
	@test -d "$(CONSOLE)" || { \
	  echo "no console checkout at $(CONSOLE) — the SPA lives in kontra-console since ADR 0038."; \
	  echo "  git clone https://github.com/medmahmoudi26/kontra-console $(CONSOLE)"; \
	  echo "  or:  make ui CONSOLE=/path/to/kontra-console"; \
	  exit 1; }
	pnpm --filter @kontra/core run build
	cd "$(CONSOLE)" && VITE_KONTRA_EXPLORE_TOKEN="$$(grep '^KONTRA_EXPLORE_TOKEN=' "$(CURDIR)/.env" | cut -d= -f2-)" pnpm run build
	@api=$$(docker ps -q --filter label=com.docker.compose.service=orchestrator-api | head -1); \
	  if [ -z "$$api" ]; then \
	    echo "no orchestrator-api container (it left docker-compose.yml — ADR 0031 §1)."; \
	    echo "  the build above IS the deploy for 'kontra up --orchestrator=local'."; \
	    echo "  for the hydrated bundle instead:  kontra bundle spa  (then restart 'kontra up')"; \
	    exit 0; \
	  fi; \
	  docker exec "$$api" rm -rf /app/web/dist/assets; \
	  docker cp "$(CONSOLE)"/dist/. "$$api":/app/web/dist/ && echo "deployed the SPA to $$api"

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
	  docker cp control/orchestrator/dist/. "$$cid":/app/dist/ && docker restart "$$cid" >/dev/null && echo "deployed and restarted $$svc ($$cid)"; \
	done
	@old=$$(docker ps -q --filter label=com.docker.compose.service=orchestrator-materializer | head -1); \
	  test -z "$$old" || { \
	    docker cp control/orchestrator/dist/. "$$old":/app/dist/ && docker restart "$$old" >/dev/null; \
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
# `actorkit.testing`'s stubs — so it is a real gate on the actor-facing API and, in the ADR's own
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
#                                                   in-process against actorkit.testing's stubs,
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
