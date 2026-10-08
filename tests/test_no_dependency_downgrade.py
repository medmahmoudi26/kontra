"""`scripts/assert-no-dependency-downgrade.py` is the guard for a mistake that cost a whole night.

An `osv-scanner` fix applied as `go get pkg@v1.97.3` across every module that named the package
downgraded `runtime/handler` from 1.105.0, because `go get @vX` pins exactly X in both directions.
Nothing failed to build — the handler is the one module a container compiles with `GOWORK=off`, so
its go.mod is the truth there while every local build uses the workspace maximum. The symptom was a
real run retrying `kontra.store_blob` nine times against `not found: S3100Continue`.

The version ordering is the part worth testing hardest: a comparator that gets pre-releases or
pseudo-versions backwards would report downgrades that are upgrades and miss the ones that are not.
"""

import importlib.util
from pathlib import Path

import pytest

_ROOT = Path(__file__).resolve().parent.parent


def _load():
    spec = importlib.util.spec_from_file_location(
        "nodowngrade", _ROOT / "scripts" / "assert-no-dependency-downgrade.py"
    )
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


nd = _load()


# ── parsing a go.mod ────────────────────────────────────────────────────────────────────────────

GO_MOD = """module github.com/medmahmoudi26/kontra/runtime/handler

go 1.26.8

require (
\tgithub.com/aws/aws-sdk-go-v2/service/s3 v1.105.0
\tgithub.com/docker/docker v27.5.1+incompatible
\tgoogle.golang.org/grpc v1.83.2 // indirect
)

replace github.com/medmahmoudi26/kontra/sdk/go => ../../sdk/go

// github.com/commented/out v9.9.9
"""


def test_requires_reads_versions_and_skips_everything_else():
    got = nd.requires(GO_MOD)
    assert got == {
        "github.com/aws/aws-sdk-go-v2/service/s3": "v1.105.0",
        "github.com/docker/docker": "v27.5.1+incompatible",
        "google.golang.org/grpc": "v1.83.2",
    }


def test_the_go_directive_is_not_a_dependency():
    """`go 1.26.8` has the shape of a require line and is not one. A comparator fed it would see
    `1.26.8 -> 1.26.4` as a downgrade of a module called `go`, which is a different (real) problem
    with its own test in cli/gotoolchain_test.go."""
    assert "go" not in nd.requires(GO_MOD)
    assert not any(k == "go" for k in nd.requires("go 1.26.8\n"))


def test_a_replace_arrow_is_not_a_requirement():
    assert nd.requires("replace example.com/a v1.0.0 => example.com/b v2.0.0\n") == {}


# ── the ordering ────────────────────────────────────────────────────────────────────────────────


@pytest.mark.parametrize(
    "lower, higher",
    [
        ("v1.97.3", "v1.105.0"),  # THE ONE. 97 < 105 numerically; lexically it is the reverse.
        ("v1.9.0", "v1.10.0"),
        ("v0.54.0", "v0.56.0"),
        ("v1.83.1", "v1.83.2"),
        ("v2.0.0-beta.8", "v2.0.0"),  # a pre-release precedes its release
        ("v2.0.0-beta.2", "v2.0.0-beta.8"),
        # Pseudo-versions are pre-releases whose first field is a timestamp.
        ("v0.0.0-20260407181057-edd947d743d2", "v0.0.0-20260811170210-91f6fe1d10ab"),
        ("v1.31.0-155.0-rc.20260411113212", "v1.32.0-162.1"),
    ],
)
def test_ordering(lower, higher):
    assert nd.sortable(lower) < nd.sortable(higher), f"{lower} should sort below {higher}"


def test_incompatible_is_metadata_and_does_not_order():
    assert nd.sortable("v27.5.1+incompatible") == nd.sortable("v27.5.1")


def test_equal_versions_are_equal():
    assert nd.sortable("v1.105.0") == nd.sortable("v1.105.0")


# ── the sweep that caused this, reconstructed ───────────────────────────────────────────────────


def _trees(before: dict[str, str], after: dict[str, str]) -> dict:
    """The two injected readers, over in-memory trees — the alternative is a test that tests git."""
    return {"before_at": lambda ref, path: before.get(path), "now_at": after.get}


def test_the_real_regression_is_caught():
    base = {"runtime/handler/go.mod": "require (\n\tgithub.com/aws/aws-sdk-go-v2/service/s3 v1.105.0\n)\n"}
    head = {"runtime/handler/go.mod": "require (\n\tgithub.com/aws/aws-sdk-go-v2/service/s3 v1.97.3\n)\n"}
    got = nd.downgrades("origin/dev", ["runtime/handler/go.mod"], **_trees(base, head))
    assert got == [
        ("runtime/handler/go.mod", "github.com/aws/aws-sdk-go-v2/service/s3", "v1.105.0", "v1.97.3")
    ]


def test_an_upgrade_is_not_a_downgrade():
    base = {"runtime/go/go.mod": "require (\n\tgithub.com/aws/aws-sdk-go-v2/service/s3 v1.96.0\n)\n"}
    head = {"runtime/go/go.mod": "require (\n\tgithub.com/aws/aws-sdk-go-v2/service/s3 v1.105.0\n)\n"}
    assert nd.downgrades("origin/dev", ["runtime/go/go.mod"], **_trees(base, head)) == []


def test_a_new_dependency_is_not_a_downgrade():
    t = _trees({"cli/go.mod": "require (\n)\n"}, {"cli/go.mod": "require (\n\tx.io/y v1.0.0\n)\n"})
    assert nd.downgrades("origin/dev", ["cli/go.mod"], **t) == []


def test_a_module_that_did_not_exist_on_the_base_is_skipped():
    """A new module's every dependency would otherwise read as appearing from nowhere."""
    t = _trees({}, {"new/go.mod": "require (\n\tx.io/y v1.0.0\n)\n"})
    assert nd.downgrades("origin/dev", ["new/go.mod"], **t) == []


def test_a_deleted_module_is_a_deletion_not_a_downgrade():
    t = _trees({"old/go.mod": "require (\n\tx.io/y v9.0.0\n)\n"}, {})
    assert nd.downgrades("origin/dev", ["old/go.mod"], **t) == []


# ── the file that actually ships ────────────────────────────────────────────────────────────────


def test_every_listed_go_mod_exists():
    """A path that stopped existing would be skipped silently, and the guard would cover one module
    fewer than it claims — which is how a sweep slips through the next time."""
    for path in nd.GO_MODS:
        assert (_ROOT / path).is_file(), f"{path} is in GO_MODS and not in the tree"


def test_the_list_covers_every_go_mod_in_the_tree():
    """Discovered rather than trusted: a module added without a line here is unguarded."""
    found = {
        str(p.relative_to(_ROOT))
        for p in _ROOT.rglob("go.mod")
        if not any(part in {"node_modules", ".git", "vendor", "dist", ".scratch"} for part in p.parts)
    }
    missing = found - set(nd.GO_MODS)
    assert not missing, f"go.mod files not covered by the downgrade guard: {sorted(missing)}"
