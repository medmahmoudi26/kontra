# `make check` is what CI runs and what runs before every PR.
.PHONY: check check-specs check-migrations check-go test-integration test-race test-e2e pentest-image pentest

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
# npm (Node 22) for the stock MCP servers, installed from examples/mcp's locks: an install that fails fails the
# test, unless KONTRA_E2E_OFFLINE=1 skips it (D189). Extra arguments go to `go test` (e.g.
# ARGS='-run TestRelayResume -v').
test-e2e:
	GO="$(GO)" scripts/test_e2e.sh $(ARGS)

# The pentest (M3 WP-10, D168, D186): PRD §12's list and M3's own surface against the compose team gate, built from
# this repository under the compose project m3pentest. pentest-image builds kontra-gate:m3 once, with the docker
# driver's builder (BUILDX_BUILDER=default: never another project's builder); `go test` never builds it. pentest runs
# test/pentest with -timeout $(KONTRA_PENTEST_TIMEOUT) (default 90m, D186) and, whatever happens to go test, tears the
# project and its networks down in the trap and removes the run's directory (KONTRA_E2E_SESSION names it). The host
# processes the run starts die with the test binary (a parent-death signal). KONTRA_PENTEST_REPORT names the file the probe table is written to.
KONTRA_PENTEST_TIMEOUT ?= 90m
PENTEST_COMPOSE = docker compose -p m3pentest -f compose.yaml -f compose.pentest.yaml

pentest-image:
	BUILDX_BUILDER=default KONTRA_PENTEST_DIR=/nonexistent KONTRA_PENTEST_TEMPORAL_PORT=1 KONTRA_PENTEST_S3_ACCESS=x \
	  KONTRA_PENTEST_S3_SECRET=x KONTRA_PEER_SECRET=x $(PENTEST_COMPOSE) build gate

pentest:
	@set -u; \
	session="pentest-$$$$"; dir="$${TMPDIR:-/tmp}/kontra-e2e-$$session"; \
	clean() { \
	  BUILDX_BUILDER=default KONTRA_PENTEST_DIR=/nonexistent KONTRA_PENTEST_TEMPORAL_PORT=1 KONTRA_PENTEST_S3_ACCESS=x \
	    KONTRA_PENTEST_S3_SECRET=x KONTRA_PEER_SECRET=x $(PENTEST_COMPOSE) down -v --remove-orphans --timeout 10 >/dev/null 2>&1; \
	  docker network rm m3pentest_net m3pentest_other >/dev/null 2>&1; rm -rf "$$dir"; true; }; \
	trap clean EXIT INT TERM; \
	KONTRA_E2E_SESSION="$$session" KONTRA_PENTEST=1 $(GO) test -tags pentest -p 1 -count=1 -timeout $(KONTRA_PENTEST_TIMEOUT) ./test/pentest -v
