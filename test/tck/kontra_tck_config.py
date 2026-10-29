"""Fills a gate configuration template of scripts/tck.sh (test/tck/gate-<variant>.yaml; M4 WP-02, D291).

    python3 test/tck/kontra_tck_config.py <template> <output>

Each placeholder ``@NAME@`` of the template is replaced by the value of the environment variable
``KONTRA_TCK_FILL_<NAME>`` (the values are not on the command line: the database URL carries a password), in one pass
and literally: a value is never read as a pattern (``&``, ``|`` and ``\\`` in a database URL with several query
parameters survive, which a sed replacement corrupted) and never scanned for placeholders again. The templates put
every placeholder inside a YAML double-quoted scalar, so a value is escaped for one (``\\`` and ``"``), and a value
with a control character is refused. A placeholder with no value is an error (exit status 2), a value for a
placeholder the template lacks is not (gate-tunnel.yaml has no @SELLER@); the output is written with mode 0600 (it
holds the database URL).
"""

from __future__ import annotations

import os
import re
import sys
from collections.abc import Mapping

PLACEHOLDER = re.compile(r"@([A-Z][A-Z_]*)@")
PREFIX = "KONTRA_TCK_FILL_"
_CONTROL = re.compile(r"[\x00-\x1f\x7f]")


class FillError(ValueError):
    """A template and values that do not fit."""


def yaml_quoted(value: str) -> str:
    """value escaped for the inside of a YAML double-quoted scalar."""
    if _CONTROL.search(value):
        raise FillError(f"a value has a control character: {value!r}")
    return value.replace("\\", "\\\\").replace('"', '\\"')


def fill(template: str, values: Mapping[str, str]) -> str:
    """template with every @NAME@ replaced by values[NAME], escaped (yaml_quoted). Raises FillError when a placeholder
    has no value."""
    names = set(PLACEHOLDER.findall(template))
    missing = sorted(names - set(values))
    if missing:
        raise FillError(f"the template has placeholders with no value: {', '.join('@' + n + '@' for n in missing)}")
    escaped = {n: yaml_quoted(values[n]) for n in names}
    return PLACEHOLDER.sub(lambda m: escaped[m.group(1)], template)


def main(argv: list[str], env: Mapping[str, str]) -> int:
    if len(argv) != 3:
        print(f"usage: {argv[0]} <template> <output>  (values in {PREFIX}<NAME>)", file=sys.stderr)
        return 2
    _, template_path, out_path = argv
    values = {k[len(PREFIX):]: v for k, v in env.items() if k.startswith(PREFIX)}
    try:
        with open(template_path, encoding="utf-8") as fh:
            text = fill(fh.read(), values)
    except (OSError, FillError) as e:
        print(f"kontra_tck_config: {template_path}: {e}", file=sys.stderr)
        return 2
    fd = os.open(out_path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, "w", encoding="utf-8") as fh:
        fh.write(text)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv, os.environ))
