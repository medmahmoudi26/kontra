# `make check` is what CI runs and what runs before every PR.
.PHONY: check check-specs check-migrations check-go

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
