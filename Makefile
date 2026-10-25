# `make check` is what CI runs and what runs before every PR.
.PHONY: check check-specs check-migrations

check: check-specs check-migrations

# Schemas are valid JSON Schema 2020-12, fixtures validate, links and decision references resolve.
check-specs:
	uv run --no-project --with jsonschema==4.26.0 python scripts/check_specs.py

# 0001.. apply up, down and up again on scratch Postgres 16 and 17 (Cloud runs 16, compose ships 17).
check-migrations:
	KONTRA_TEST_PG_IMAGE=postgres:16-alpine scripts/check_migrations.sh
	KONTRA_TEST_PG_IMAGE=postgres:17-alpine scripts/check_migrations.sh
