# `make check` is what CI runs and what runs before every PR.
.PHONY: check check-specs check-migrations check-go test-integration test-race test-e2e tck test-kits pentest-image pentest

# The system Go may be older than the module's toolchain (go 1.26, a2a-go v2.7.0 needs it); pin the one
# the module names so every go command, and gofmt, come from the same toolchain.
GOTOOLCHAIN ?= go1.26.10
export GOTOOLCHAIN
GO ?= go

check: check-specs check-migrations check-go

# Schemas are valid JSON Schema 2020-12, fixtures validate, links and decision references resolve.
check-specs:
	uv run --no-project --with jsonschema==4.26.0 python scripts/check_specs.py

# 0001.. apply up, down and up again on scratch Postgres 16 and 17 (Cloud runs 16, compose ships 17).
check-migrations:
	KONTRA_TEST_PG_IMAGE=postgres:16-alpine scripts/check_migrations.sh
	KONTRA_TEST_PG_IMAGE=postgres:17-alpine scripts/check_migrations.sh

# gofmt (the toolchain's own), vet, build, and the unit tests. -short skips integration and e2e tests,
# which need Postgres, Temporal and gate subprocesses.
check-go:
	@out="$$("$$($(GO) env GOROOT)/bin/gofmt" -l .)"; \
	  if [ -n "$$out" ]; then echo "gofmt -l reports unformatted files:"; echo "$$out"; exit 1; fi
	$(GO) vet ./...
	$(GO) build ./...
	$(GO) test -short ./...

# Integration tests (no -short): every package but test/e2e, one at a time (-p 1), against $KONTRA_TEST_PG
# or, when it is unset, one throwaway postgres:17-alpine container (KONTRA_TEST_PG_IMAGE=postgres:16-alpine
# for 16). The e2e tests have their own target.
test-integration:
	GO="$(GO)" scripts/test_integration.sh

# The integration tests under the race detector (cgo; CI's race job, on Postgres 17).
test-race:
	CGO_ENABLED=1 GO="$(GO)" scripts/test_integration.sh -race

# End-to-end tests (test/e2e, spec/relay.md §16, D80): per test, two kontra-gate replicas built from
# ./cmd/kontra-gate as processes, a Temporal development server from $KONTRA_TEMPORAL_CLI (required: the
# pinned CLI 1.9.1, Server 1.32.0) and a fresh database on $KONTRA_TEST_PG (a server) or, when it is unset,
# a postgres:17-alpine container. One test at a time, never beside another integration run. The script
# fails the run when any process or container the harness started outlives it. The M3 tests also need node and
# npm (Node 22) for the stock MCP servers, installed from examples/mcp's locks, and Node 24 for
# server-everything@2.0.0 ($$KONTRA_NODE24, or node on PATH; KONTRA_REQUIRE_NODE24=1 fails its test without it, as CI
# sets): an install that fails fails the
# test, unless KONTRA_E2E_OFFLINE=1 skips it (D189). Extra arguments go to `go test` (e.g.
# ARGS='-run TestRelayResume -v').
test-e2e:
	GO="$(GO)" scripts/test_e2e.sh $(ARGS)

# The pinned A2A TCK against the gate (spec/relay.md §16 tck_jsonrpc, tck_rest, tck_behind_caddy; D18, DM4-12),
# one variant per run: VARIANT=direct (the default) or tunnel (the seller examples/sellers/tck on the direct
# backend, or hosted through `kontra connect`), caddy (the direct backend behind examples/front/caddy), auth (behind
# a loopback front that adds a tck API key: ListTasks and push through the TCK, nightly). scripts/tck.sh starts a
# Temporal development server from $KONTRA_TEMPORAL_CLI, a fresh database on $KONTRA_TEST_PG (or a postgres:17-alpine
# container), the gate, the seller, the node and Caddy ($KONTRA_CADDY, 2.11.7) as processes, runs the TCK checkout
# $KONTRA_TCK_DIR (a2a-tck d4d6fae) under uv for each binding with test/tck/kontra_xfail.py enforcing
# test/tck/expected-failures.txt, and removes everything it started. One run at a time, never beside the e2e tests.
VARIANT ?= direct
tck:
	GO="$(GO)" scripts/tck.sh $(VARIANT)

# The gate-run kits (M4 WP-08, DM4-31): test/vps and test/demo with Docker (KONTRA_KIT_TESTS=1). The gate image is built
# once from the working tree with the docker driver's builder (kontra-gate:kit-<pid>) for both packages; then compose.yaml's
# ACME variant comes up and writes its cache, scripts/team-gate-vps.sh --host runs its steps 4 and 5 against a stand-in
# VPS on this host, the clean laptop's image is checked, and scripts/demo-clean-laptop.sh is rehearsed against a local
# multi-tenant gate answering for gateway.kontra.run on a docker network (about 40 minutes; needs the internet for Go
# modules and npm). Every image, container and volume is removed. One run at a time, never beside the e2e tests or a
# pentest; nothing touches staging, DigitalOcean or Cloudflare (the runs are WP-09's).
test-kits:
	@set -u; img="kontra-gate:kit-$$$$"; \
	clean() { docker image rm "$$img" >/dev/null 2>&1; true; }; trap clean EXIT INT TERM; \
	BUILDX_BUILDER=default docker build -q --build-arg VERSION=kit -t "$$img" . && \
	KONTRA_KIT_TESTS=1 KONTRA_KIT_GATE_IMAGE="$$img" $(GO) test -p 1 -count=1 -timeout 120m ./test/vps ./test/demo -v $(ARGS)

# The pentest (M3 WP-10, D168, D186): PRD §12's list and M3's own surface against the compose team gate, built from
# this repository under the compose project $(KONTRA_PENTEST_PROJECT) (m3pentest by default). pentest-image builds the
# gate's image, $(KONTRA_PENTEST_IMAGE) (kontra-gate:m3 by default), once, with the docker driver's builder
# (BUILDX_BUILDER=default: never another project's builder); `go test` never builds it. Another checkout running its
# pentest on the same Docker sets both to names of its own: one project would tear the other's stack down, one tag
# run the other's gate. pentest runs
# test/pentest with -timeout $(KONTRA_PENTEST_TIMEOUT) (default 90m, D186) and, whatever happens to go test, tears the
# project and its networks down in the trap and removes the run's directory (KONTRA_E2E_SESSION names it). The host
# processes the run starts die with the test binary (a parent-death signal). KONTRA_PENTEST_REPORT names the file the probe table is written to.
KONTRA_PENTEST_TIMEOUT ?= 90m
KONTRA_PENTEST_PROJECT ?= m3pentest
KONTRA_PENTEST_IMAGE ?= kontra-gate:m3
export KONTRA_PENTEST_PROJECT KONTRA_PENTEST_IMAGE
PENTEST_COMPOSE = docker compose -p $(KONTRA_PENTEST_PROJECT) -f compose.yaml -f compose.pentest.yaml

pentest-image:
	BUILDX_BUILDER=default KONTRA_PENTEST_DIR=/nonexistent KONTRA_PENTEST_TEMPORAL_PORT=1 KONTRA_PENTEST_S3_ACCESS=x \
	  KONTRA_PENTEST_S3_SECRET=x KONTRA_PEER_SECRET=x $(PENTEST_COMPOSE) build gate

pentest:
	@set -u; \
	session="pentest-$$$$"; dir="$${TMPDIR:-/tmp}/kontra-e2e-$$session"; \
	clean() { \
	  BUILDX_BUILDER=default KONTRA_PENTEST_DIR=/nonexistent KONTRA_PENTEST_TEMPORAL_PORT=1 KONTRA_PENTEST_S3_ACCESS=x \
	    KONTRA_PENTEST_S3_SECRET=x KONTRA_PEER_SECRET=x $(PENTEST_COMPOSE) down -v --remove-orphans --timeout 10 >/dev/null 2>&1; \
	  docker network rm $(KONTRA_PENTEST_PROJECT)_net $(KONTRA_PENTEST_PROJECT)_other >/dev/null 2>&1; rm -rf "$$dir"; true; }; \
	trap clean EXIT INT TERM; \
	KONTRA_E2E_SESSION="$$session" KONTRA_PENTEST=1 $(GO) test -tags pentest -p 1 -count=1 -timeout $(KONTRA_PENTEST_TIMEOUT) ./test/pentest -v
