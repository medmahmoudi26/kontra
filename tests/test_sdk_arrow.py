"""THE ARROW: `runtime/` may import `sdk/`, and `sdk/` may import nothing of `runtime/`.

That is the entire reason `actorkit/` became two directories rather than one with a naming
convention inside it. A directory split nobody checks is a sentence in a README that stops being
true in a month — the previous split (`lib/` vs `internals/`) was violated in five places by the
time it was measured, and each violation was invisible because `import` is not a thing anyone
greps for when they need one symbol.

So it is asked here, three ways, because each way catches something the others cannot:

  1. THE AST, over every file under sdk/python/actorkit. Catches a module-scope import even when
     the module is never imported by this suite, and catches a `TYPE_CHECKING` import, which no
     runtime check can see and which is exactly how a type-only edge grows into a real one.
  2. A FRESH INTERPRETER with `internals` made unimportable. Catches what the AST cannot: an
     `importlib.import_module("internals.x")`, a `__getattr__` that reaches, a re-export chain.
  3. sys.modules AFTER `import actorkit`. Catches the dependency the arrow exists to prevent —
     an author surface that cannot be imported without a Temporal client, a Redis client or an S3
     client in the process.

THE ONE EDGE THAT IS ALLOWED, and only deferred. `actor.serve()` and `catalog.serve()` are the
entry-point handoff — by definition the line where an author stops writing code and gives
the process to the engine. Both import the runtime INSIDE the function body, so `import actorkit`
never reaches it. Anything else, at any scope, fails.

The Go half of this is sdk/go/arrow_test.go, which asks `go list -deps` the same question.
"""
from __future__ import annotations

import ast
import pathlib
import subprocess
import sys

import pytest

ROOT = pathlib.Path(__file__).resolve().parent.parent
SDK = ROOT / "sdk" / "python" / "actorkit"
RUNTIME_PKG = "internals"

#: The entry-point handoffs, as (module file, enclosing function). Deferred imports of the runtime
#: are permitted at exactly these two places and nowhere else. Adding to this list is the reviewable
#: event — it means a second place in the author surface now needs the engine, which is the claim
#: that has to be argued rather than assumed.
ALLOWED_HANDOFFS = {
    ("actor.py", "serve"),        # Actor.serve()          -> internals.temporal.host.serve
    ("catalog.py", "serve"),      # catalog.serve()        -> internals.temporal.wfhost.serve_workflows
}

#: Infrastructure clients an author surface must not carry. Temporal is deliberately absent: the
#: workflow-facing half of this package (`catalog`, `hitl`, `narrate`, `contract`) is written
#: against temporalio's workflow API, which is the substrate an author writes in, not a detail
#: leaking upward. What the package promises instead is that plain `import actorkit` costs none of
#: it — asserted separately below, and the reason the verbs resolve lazily.
BANNED_MODULES = {
    "redis": "a Redis client (the durable state tiers are the runtime's)",
    "boto3": "an object-store client (the blob plane is the runtime's)",
    "botocore": "an object-store client (the blob plane is the runtime's)",
}


def _sdk_files() -> list[pathlib.Path]:
    files = sorted(p for p in SDK.rglob("*.py") if "__pycache__" not in p.parts)
    assert files, f"no SDK modules found under {SDK} — the layout moved and this test did not"
    return files


def _imports(tree: ast.AST) -> list[tuple[ast.stmt, str, bool, str | None]]:
    """Every import in `tree` as (node, top-level module name, deferred?, enclosing function).

    `deferred` means the statement is inside a function body — the only form the handoff may take.
    A `class` body is NOT deferred: it runs at import.
    """
    found: list[tuple[ast.stmt, str, bool, str | None]] = []

    def walk(node: ast.AST, fn: str | None) -> None:
        for child in ast.iter_child_nodes(node):
            inner = fn
            if isinstance(child, (ast.FunctionDef, ast.AsyncFunctionDef)):
                inner = fn or child.name
            elif isinstance(child, ast.Import):
                for alias in child.names:
                    found.append((child, alias.name.split(".")[0], fn is not None, fn))
            elif isinstance(child, ast.ImportFrom):
                # `from . import x` (level > 0) is intra-package and names no other seam.
                if child.level == 0 and child.module:
                    found.append((child, child.module.split(".")[0], fn is not None, fn))
            walk(child, inner)

    walk(tree, None)
    return found


@pytest.mark.parametrize("path", _sdk_files(), ids=lambda p: p.name)
def test_no_module_under_sdk_imports_the_runtime_at_module_scope(path: pathlib.Path) -> None:
    """THE ARROW, STATICALLY. A module-scope `internals` import anywhere under sdk/ fails here,
    and so does a deferred one outside the two entry-point handoffs."""
    tree = ast.parse(path.read_text(), filename=str(path))
    for node, mod, deferred, fn in _imports(tree):
        if mod != RUNTIME_PKG:
            continue
        where = f"{path.relative_to(ROOT)}:{node.lineno}"
        if not deferred:
            pytest.fail(
                f"{where} imports `{RUNTIME_PKG}` at module scope. The arrow is runtime -> sdk and "
                f"never the reverse: move what you need into the author surface (Unit/Batch/Dataset "
                f"live in actorkit.batch, the type derivation in actorkit.schema), or — if this IS "
                f"the entry-point handoff — defer it inside the function that hands control over."
            )
        if (path.name, fn) not in ALLOWED_HANDOFFS:
            pytest.fail(
                f"{where} defers an import of `{RUNTIME_PKG}` inside `{fn}()`, which is not one of "
                f"the entry-point handoffs {sorted(ALLOWED_HANDOFFS)}. A deferred import is still an "
                f"edge; it is tolerated only where an author has already handed over the process."
            )


@pytest.mark.parametrize("path", _sdk_files(), ids=lambda p: p.name)
def test_no_module_under_sdk_carries_an_infrastructure_client(path: pathlib.Path) -> None:
    """The second half of the same claim, and the one that says what the arrow is FOR: an author
    surface that links a Redis or S3 client cannot be imported by a caller who has neither."""
    tree = ast.parse(path.read_text(), filename=str(path))
    for node, mod, _deferred, _fn in _imports(tree):
        why = BANNED_MODULES.get(mod)
        if why:
            pytest.fail(f"{path.relative_to(ROOT)}:{node.lineno} imports `{mod}` — {why}")


def test_every_sdk_module_imports_with_the_runtime_made_unimportable() -> None:
    """THE ARROW, EXECUTED. The AST above reads import statements; this runs them.

    A fresh interpreter, with a meta-path finder that refuses `internals` and everything under it,
    imports every module in the author surface. Anything that reaches the runtime at import — via
    `importlib`, a module `__getattr__`, a re-export, a decorator that resolves one — dies here and
    nowhere else.

    A SUBPROCESS, because this session has already imported `internals` (most of this suite does),
    so the question is unanswerable in-process.
    """
    modules = sorted(p.stem for p in _sdk_files() if p.stem != "__init__")
    probe = f"""
import importlib, sys

class Refuse:
    def find_module(self, name, path=None):
        return None
    def find_spec(self, name, path=None, target=None):
        if name == {RUNTIME_PKG!r} or name.startswith({RUNTIME_PKG + "."!r}):
            raise ImportError(
                "the SDK reached the runtime at import time: " + name
            )
        return None

sys.meta_path.insert(0, Refuse())
sys.modules.pop({RUNTIME_PKG!r}, None)

import actorkit
for m in {modules!r}:
    importlib.import_module("actorkit." + m)
print("ok")
"""
    out = subprocess.run(
        [sys.executable, "-c", probe],
        cwd=ROOT,
        env=_env_with_this_sessions_path(),
        capture_output=True,
        text=True,
    )
    assert out.returncode == 0, (
        "importing the author surface reached the runtime:\n" + out.stderr[-3000:]
    )
    assert out.stdout.strip().endswith("ok")


def test_import_actorkit_costs_no_temporal_no_redis_and_no_object_store() -> None:
    """WHAT THE ARROW BUYS, stated as the property an author can rely on: `import actorkit` is the
    vocabulary and nothing else. It is what the Temporal workflow sandbox re-imports per instance,
    what the CLI imports to read a manifest, and what a test imports to build a stub — none of
    which are entitled to a Temporal client, a Redis connection or an S3 session.

    `speak` and `ask` are resolved by a module `__getattr__` for exactly this reason, so naming one
    DOES pay for temporalio; that half is asserted in test_speak_and_ask.py.
    """
    probe = (
        "import sys, actorkit;"
        "print(' '.join(sorted(m for m in ('temporalio','redis','boto3','botocore')"
        "                      if m in sys.modules)) or 'clean')"
    )
    out = subprocess.run(
        [sys.executable, "-c", probe],
        cwd=ROOT,
        env=_env_with_this_sessions_path(),
        capture_output=True,
        text=True,
        check=True,
    )
    assert out.stdout.strip() == "clean", (
        f"`import actorkit` dragged in {out.stdout.strip()}"
    )


def _env_with_this_sessions_path() -> dict[str, str]:
    """PYTHONPATH from this session's own sys.path, so a subprocess resolves the SAME checkout.

    Since `actorkit/` stopped being a repo-root directory, running from the repo root no longer
    puts the package on the path by itself — conftest.py does, and a subprocess inherits none of it.
    """
    import os

    return {**os.environ, "PYTHONPATH": os.pathsep.join(p for p in sys.path if p)}
