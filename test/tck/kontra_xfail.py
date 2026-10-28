"""pytest plugin: mark TCK node IDs listed in $TCK_EXPECTED_FAILURES as strict xfail.

File format, one entry per line:  <pytest node id>  # <reason>   (blank lines and '#' lines ignored)
An entry that unexpectedly passes becomes XPASS(strict) -> FAILED, so stale entries are caught.
"""
from __future__ import annotations

import os

import pytest


def _load(path: str) -> dict[str, str]:
    out: dict[str, str] = {}
    with open(path, encoding="utf-8") as fh:
        for raw in fh:
            line = raw.strip()
            if not line or line.startswith("#"):
                continue
            node, _, reason = line.partition("#")
            out[node.strip()] = reason.strip() or "listed in expected-failures file"
    return out


def pytest_collection_modifyitems(config: pytest.Config, items: list[pytest.Item]) -> None:
    path = os.environ.get("TCK_EXPECTED_FAILURES")
    if not path:
        return
    expected = _load(path)
    seen = set()
    for item in items:
        reason = expected.get(item.nodeid)
        if reason is not None:
            seen.add(item.nodeid)
            item.add_marker(pytest.mark.xfail(reason=reason, strict=True, run=True))
    missing = sorted(set(expected) - seen)
    if missing:
        raise pytest.UsageError("expected-failures entries match no collected test: " + ", ".join(missing))
