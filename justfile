# Kontra repo task runner. The seams: sdk/python/ (the `actorkit` author surface) +
# runtime/python/ (the `internals` engine, shipped by the same distribution `kontra-sdk`),
# control/orchestrator/ + shared/core/ (the TS orchestrator and the shared kernel the console imports;
# the console itself is the kontra-console repository),
# contracts/ (buf-owned proto envelope). Control-plane infra still lives in the Makefile
# (make up = control plane); this justfile is the dev/test/CI surface.
#
#   just install        # editable install of the sdk/python seam with extras
#   just test           # fast pytest suite (excludes e2e)
#   just conformance     # the language-neutral claim-check codec corpus (Python harness)
#   just ts-test        # orchestrator vitest (includes the TS conformance test)

# venv-local binaries; the kontra launcher puts the sdk/python seam on PYTHONPATH.
venv := ".venv"
py := venv / "bin" / "python"
pytest := venv / "bin" / "pytest"
# buf is vendored into the venv (.venv/bin/buf); fall back to PATH if absent.
buf := venv / "bin" / "buf"

# Default: list available recipes.
default:
    @just --list

# --- sdk/python seam -------------------------------------------------------

# Editable install of the sdk/python seam (`kontra-sdk`) with dev + seaweed extras.
install:
    {{py}} -m pip install -e './sdk/python[dev,seaweed]'

# Fast unit/integration suite — everything EXCEPT the e2e marker.
test:
    {{pytest}} -m 'not e2e'

# The actor engine suite: the batch pipeline, exactly-once commits, isolation and the three
# state tiers. Runs in the one venv.
test-engine:
    {{pytest}} tests/test_actor_engine.py -q

# Language-neutral claim-check codec conformance corpus (Python side of the
# byte-compatible wire contract pinned by shared/conformance/codec/fixtures.json).
conformance:
    {{py}} shared/conformance/codec/harness.py

# --- proto envelope (buf-owned) --------------------------------------------

# Lint the proto envelope (RunEnvelope/StepOptions) in the `contracts` module.
buf-lint:
    {{buf}} lint

# Build/compile-check the proto envelope.
buf-build:
    {{buf}} build

# Regenerate the COMMITTED stubs: Python (sdk/python/kontra/v1) + TS types-only
# (control/orchestrator/_gen). CI runs this then `git diff --exit-code` to fail on drift, so
# run it after editing any .proto and commit the result.
buf-generate:
    {{buf}} generate

# THE PROTO GATE — lint + breaking in one, the same command CI runs (Makefile: proto-check).
#
# This recipe used to be `buf breaking --against '.git#branch=main'`, which is two problems in one
# line: it needs a LOCAL `main` branch, and a fresh clone has `origin/main` and no local one — so
# the recipe failed on any machine that never checked main out; and where main does exist it is
# whatever was last pulled, not the point this branch forked from. There is one baseline now, in
# scripts/proto-check.sh, so `just` and CI cannot disagree about what "breaking" means.
proto-check:
    make proto-check

# --- orchestrator (TS) seam ------------------------------------------------

# Orchestrator vitest run (src/codec/conformance.test.ts is the TS conformance gate).
ts-test:
    cd control/orchestrator && pnpm test

# --- go seams (handler + cli; two modules under the root go.work) -----------

go-test:
    cd runtime/handler && go build ./... && go vet ./... && go test ./...
    cd cli && go build ./... && go vet ./... && go test ./...

# --- formatting / linting placeholders -------------------------------------

# Format both Python seams (ruff installed on demand — not vendored in the venv).
fmt:
    {{py}} -m pip install -q ruff && {{py}} -m ruff format sdk/python runtime/python conformance tests

# Lint both Python seams (advisory until a ruff config lands).
lint:
    {{py}} -m pip install -q ruff && {{py}} -m ruff check sdk/python runtime/python conformance tests
