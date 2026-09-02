"""The proto gate — `make proto-check` (scripts/proto-check.sh) — held to the two things that
make it a gate rather than a command that usually exits 0.

`buf` was installed and configured, and no command in the repo ran both halves of it: the breaking
check existed only inline in the CI workflow, spelled differently from the justfile's, against a
third baseline, and it was advisory (`|| echo "::warning::"`). So this file asserts BOTH halves of
"it runs": the target catches a real break, and CI still calls the target — a step deleted from
ci.yml is otherwise a change no test in this repo notices.

The failure the rest of these tests are about is quieter: `buf breaking` exits 0 when the baseline
is the wrong commit. Measured on a fixture (test_a_committed_break_is_invisible_against_head),
`--against '.git'` and `--against '.git#ref=HEAD'` both pass a renumbered field that is already
committed, because it is on both sides of the diff. So the baseline is the merge base with the
branch this forked from, and a baseline that cannot be resolved is an ERROR — the cases below are
the three ways this repo can produce one: an unresolvable ref, a shallow clone with no common
ancestor, a commit that predates the contracts.

Most of these run the script with BUF=/bin/true: they exercise the resolution that happens BEFORE
buf is invoked, and /bin/true is what a deleted guard would fall through to — so a test that
passes with a stubbed buf is a test that proves the guard, not the linter.
"""

import os
import shutil
import subprocess
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parent.parent
SCRIPT = ROOT / "scripts" / "proto-check.sh"
CI = ROOT / ".github" / "workflows" / "ci.yml"
MAKEFILE = ROOT / "Makefile"

GIT_ID = ["-c", "user.email=gate@kontra.test", "-c", "user.name=proto gate"]


def find_buf() -> str | None:
    """The same places scripts/proto-check.sh looks. GOPATH/bin is not on PATH on the machine this
    repo is developed on, which is the whole reason the script searches at all."""
    candidates = [
        os.environ.get("BUF"),
        shutil.which("buf"),
        str(ROOT / ".venv" / "bin" / "buf"),
        os.path.expanduser("~/go/bin/buf"),
    ]
    for c in candidates:
        if c and os.access(c, os.X_OK):
            return c
    return None


def run_gate(cwd: Path, **env_overrides) -> subprocess.CompletedProcess:
    env = {**os.environ, **{k: v for k, v in env_overrides.items() if v is not None}}
    return subprocess.run(
        ["bash", str(SCRIPT)], cwd=cwd, env=env, capture_output=True, text=True
    )


def git(repo: Path, *args) -> str:
    return subprocess.run(
        ["git", *args], cwd=repo, capture_output=True, text=True, check=True
    ).stdout.strip()


BUF_YAML = """version: v2
modules:
  - path: proto
lint:
  use:
    - BASIC
breaking:
  use:
    - FILE
"""

THING_PROTO = """syntax = "proto3";

package fixture.v1;

message Thing {
  string name = 1;
  string label = 2;
}
"""


def make_repo(tmp_path: Path, with_protos: bool = True) -> Path:
    """A minimal buf module in its own git repo. Hermetic on purpose: the renumbering test has to
    edit a .proto in the working tree, and doing that to contracts/ would race every other change
    in the checkout."""
    repo = tmp_path / "fixture"
    (repo / "proto" / "fixture" / "v1").mkdir(parents=True)
    subprocess.run(["git", "init", "-q", "-b", "main", str(repo)], check=True)
    (repo / "buf.yaml").write_text(BUF_YAML)
    (repo / "README.md").write_text("fixture\n")
    if with_protos:
        (repo / "proto" / "fixture" / "v1" / "thing.proto").write_text(THING_PROTO)
    subprocess.run(["git", "add", "-A"], cwd=repo, check=True)
    subprocess.run(["git", *GIT_ID, "commit", "-qm", "baseline"], cwd=repo, check=True)
    return repo


# --- the gate runs where the other checks run -----------------------------------------


def test_ci_calls_the_make_target():
    """A gate only in a Makefile is a gate that runs when somebody remembers. The proto job used
    to inline `buf lint` and `buf breaking --against <a URL>`, a second spelling with its own
    baseline, and nothing tied the two together."""
    ci = CI.read_text()
    assert "make proto-check" in ci, "the CI proto job no longer runs the gate"
    assert "proto-check:" in MAKEFILE.read_text(), "the Makefile has no proto-check target"
    # fetch-depth: 0 is not optional here — the gate resolves its baseline from this clone.
    assert "fetch-depth: 0" in ci


def test_the_ci_gate_is_not_advisory():
    """It WAS advisory (`|| echo "::warning::"`) while ADR 0023's flag day landed, and the comment
    said to drop that once main carried the deletion. An advisory gate reports a break in a log
    nobody opens and merges it anyway."""
    step = [ln for ln in CI.read_text().splitlines() if "make proto-check" in ln]
    assert step, "no CI step runs make proto-check"
    for ln in step:
        assert "||" not in ln, f"the proto gate is swallowing its own failure: {ln.strip()}"
    assert "continue-on-error" not in CI.read_text()


def test_ci_does_not_keep_a_second_baseline():
    """Two spellings of `buf breaking` is two answers to "breaking against what". The inline
    `--against` in the workflow is gone; the baseline lives in scripts/proto-check.sh."""
    assert "buf breaking --against" not in CI.read_text()


# --- a missing buf, and a baseline that isn't there ------------------------------------


def test_a_missing_buf_is_reported_plainly(tmp_path):
    """buf is not on PATH on the machine this repo is developed on (it is in GOPATH/bin), so a
    target that assumed PATH would fail with `buf: command not found` and no hint. Run with
    nothing on PATH and no HOME to search: the gate must name the tool and how to get it."""
    repo = make_repo(tmp_path)
    env = {"PATH": "/usr/bin:/bin", "HOME": str(tmp_path / "empty-home")}
    r = subprocess.run(
        ["bash", str(SCRIPT)], cwd=repo, env=env, capture_output=True, text=True
    )
    assert r.returncode != 0
    assert "buf not found" in r.stderr
    assert "go install github.com/bufbuild/buf" in r.stderr


def test_an_explicit_buf_that_is_not_there_fails(tmp_path):
    """An override that is silently ignored runs a DIFFERENT buf than the one asked for — and
    version is exactly what buf overrides are for (CI pins 1.70.0)."""
    repo = make_repo(tmp_path)
    r = run_gate(repo, BUF=str(tmp_path / "nope" / "buf"))
    assert r.returncode != 0
    assert "not an executable" in r.stderr


def test_an_unresolvable_baseline_is_an_error_not_a_pass(tmp_path):
    """A ref that does not resolve has to stop the gate, because the fallbacks available to a
    script that shrugs are all quiet: `--against '.git#ref='` with an empty value, or the plain
    `.git` spelling that diffs against HEAD and exits 0 on a committed break. With BUF=/bin/true a
    gate that skipped this check exits 0 here."""
    repo = make_repo(tmp_path)
    r = run_gate(repo, BUF="/bin/true", PROTO_BASELINE="refs/heads/no-such-branch")
    assert r.returncode != 0
    assert "no baseline ref resolves" in r.stderr
    assert "no-such-branch" in r.stderr


def test_a_baseline_with_no_protos_is_an_error_not_a_pass(tmp_path):
    """A baseline from before the contracts existed reads no contract at all. buf 1.70 does refuse
    it ("Module "path: "proto"" had no .proto files", measured) — but that message names an input,
    not which one or at which commit, and it is a buf behaviour nothing here pins. The gate
    answers it itself, with the sha in the message."""
    repo = make_repo(tmp_path, with_protos=False)
    empty = git(repo, "rev-parse", "HEAD")
    (repo / "proto" / "fixture" / "v1" / "thing.proto").write_text(THING_PROTO)
    r = run_gate(repo, BUF="/bin/true", PROTO_BASELINE=empty)
    assert r.returncode != 0
    assert "carries no .proto" in r.stderr


def test_no_common_ancestor_is_an_error(tmp_path):
    """The shape a shallow CI checkout takes: the ref resolves and shares no history with HEAD.
    `git merge-base` then prints nothing and exits 1 — which unchecked leaves `--against
    '.git#ref='` (buf: "invalid options") or, one shrug further, the bare `.git` that buf reads as
    HEAD and passes."""
    repo = make_repo(tmp_path)
    subprocess.run(["git", "checkout", "-q", "--orphan", "unrelated"], cwd=repo, check=True)
    (repo / "README.md").write_text("unrelated root\n")
    subprocess.run(["git", "add", "-A"], cwd=repo, check=True)
    subprocess.run(["git", *GIT_ID, "commit", "-qm", "unrelated"], cwd=repo, check=True)
    r = run_gate(repo, BUF="/bin/true", PROTO_BASELINE="main")
    assert r.returncode != 0
    assert "no common ancestor" in r.stderr


# --- and it catches a real break --------------------------------------------------------


@pytest.mark.skipif(find_buf() is None, reason="buf is not installed on this machine")
def test_a_renumbered_field_fails_the_gate(tmp_path):
    """The demonstration the gate is worth having: renumber a field and the target goes red. It
    passes on the same fixture one edit earlier, so this is the gate discriminating rather than
    the gate always failing."""
    repo = make_repo(tmp_path)
    buf = find_buf()

    clean = run_gate(repo, BUF=buf, PROTO_BASELINE="main")
    assert clean.returncode == 0, clean.stdout + clean.stderr

    proto = repo / "proto" / "fixture" / "v1" / "thing.proto"
    proto.write_text(THING_PROTO.replace("string label = 2;", "string label = 3;"))
    broken = run_gate(repo, BUF=buf, PROTO_BASELINE="main")
    assert broken.returncode != 0
    assert 'field "2" with name "label"' in broken.stdout + broken.stderr
    # and it says which check failed and what to do about it
    assert "BUF BREAKING FAILED" in broken.stderr
    assert "reserved" in broken.stderr


@pytest.mark.skipif(find_buf() is None, reason="buf is not installed on this machine")
def test_a_committed_break_is_invisible_against_head(tmp_path):
    """WHY THE BASELINE IS THE FORK POINT AND NOT HEAD. Commit the renumbering, put one ordinary
    commit on top, and `buf breaking --against '.git#ref=HEAD'` exits 0: the break is in the
    working tree AND in HEAD, so the diff is empty. That is the state a branch is in by the time
    anyone reviews it. The gate diffs against the merge base and catches the same break."""
    repo = make_repo(tmp_path)
    buf = find_buf()
    fork = git(repo, "rev-parse", "HEAD")
    subprocess.run(["git", "checkout", "-qb", "feature"], cwd=repo, check=True)

    proto = repo / "proto" / "fixture" / "v1" / "thing.proto"
    proto.write_text(THING_PROTO.replace("string label = 2;", "string label = 3;"))
    subprocess.run(["git", "add", "-A"], cwd=repo, check=True)
    subprocess.run(["git", *GIT_ID, "commit", "-qm", "renumber"], cwd=repo, check=True)
    (repo / "README.md").write_text("later, unrelated work\n")
    subprocess.run(["git", "add", "-A"], cwd=repo, check=True)
    subprocess.run(["git", *GIT_ID, "commit", "-qm", "later"], cwd=repo, check=True)

    blind = subprocess.run(
        [buf, "breaking", "--against", ".git#ref=HEAD"], cwd=repo, capture_output=True, text=True
    )
    assert blind.returncode == 0, "buf grew a HEAD baseline that sees committed breaks"

    caught = run_gate(repo, BUF=buf, PROTO_BASELINE=fork)
    assert caught.returncode != 0
    assert 'field "2" with name "label"' in caught.stdout + caught.stderr


@pytest.mark.skipif(find_buf() is None, reason="buf is not installed on this machine")
def test_a_branch_is_not_blamed_for_what_main_did_after_it_forked(tmp_path):
    """WHY THE FORK POINT AND NOT `origin/main` ITSELF — the justfile used to say
    `--against '.git#branch=main'`. Let main add a field after the fork and leave this branch's
    protos untouched: against main's TIP buf reports `Previously present field "9" ... was
    deleted`, a break the branch never made (measured). Against the merge base it is clean, which
    is the difference between a gate people fix and a gate people silence."""
    repo = make_repo(tmp_path)
    buf = find_buf()
    fork = git(repo, "rev-parse", "HEAD")

    proto = repo / "proto" / "fixture" / "v1" / "thing.proto"
    proto.write_text(THING_PROTO.replace("string name = 1;", "string name = 1;\n  string extra = 9;"))
    subprocess.run(["git", "add", "-A"], cwd=repo, check=True)
    subprocess.run(["git", *GIT_ID, "commit", "-qm", "main adds a field"], cwd=repo, check=True)

    subprocess.run(["git", "checkout", "-q", "-b", "mine", fork], cwd=repo, check=True)
    (repo / "README.md").write_text("my work, no protos touched\n")
    subprocess.run(["git", "add", "-A"], cwd=repo, check=True)
    subprocess.run(["git", *GIT_ID, "commit", "-qm", "my work"], cwd=repo, check=True)

    tip = subprocess.run(
        [buf, "breaking", "--against", ".git#branch=main"], cwd=repo, capture_output=True, text=True
    )
    assert tip.returncode != 0 and 'field "9"' in tip.stdout

    forked = run_gate(repo, BUF=buf, PROTO_BASELINE="main")
    assert forked.returncode == 0, forked.stdout + forked.stderr


@pytest.mark.skipif(find_buf() is None, reason="buf is not installed on this machine")
def test_a_lint_violation_fails_the_gate(tmp_path):
    """The other half of the target, and a different message: lint failures are fixed in the
    .proto or excepted in buf.yaml, never by picking another baseline."""
    repo = make_repo(tmp_path)
    (repo / "proto" / "fixture" / "v1" / "thing.proto").write_text(
        THING_PROTO.replace("package fixture.v1;", "package wrong.place.v1;")
    )
    r = run_gate(repo, BUF=find_buf(), PROTO_BASELINE="main")
    assert r.returncode != 0
    assert "BUF LINT FAILED" in r.stderr


def test_the_make_target_reaches_the_script_and_resolves_a_baseline_here():
    """`make proto-check`, not the script: the target is what CI runs, and BUF/PROTO_BASELINE only
    reach it because the Makefile exports them — a `make proto-check BUF=...` that quietly ran a
    different buf looks identical from the outside.

    BUF=/bin/true on purpose. Running real buf over the real module means cloning this repo's .git
    (~8s, most of it history), and that comparison is the gate itself: CI runs it on every push
    and `make proto-check` runs it on demand. What is worth a unit test is the wiring — the target
    exists, the script path is right, the override is exported, and origin/main resolves to a
    merge base in an ordinary checkout. Real buf runs against the fixtures above.

    Skipped where the baseline is unreachable (a shallow clone — e.g. the CI python job, which
    checks out with the default fetch-depth: 1)."""
    if subprocess.run(
        ["git", "merge-base", "HEAD", "origin/main"], cwd=ROOT, capture_output=True
    ).returncode != 0:
        pytest.skip("no merge base with origin/main in this checkout")
    r = subprocess.run(
        ["make", "proto-check"],
        cwd=ROOT,
        env={**os.environ, "BUF": "/bin/true"},
        capture_output=True,
        text=True,
    )
    assert r.returncode == 0, r.stdout + r.stderr
    # It prints the baseline it used, because a passing breaking check is otherwise
    # indistinguishable from one that had nothing to read.
    assert "baseline origin/main" in r.stdout
