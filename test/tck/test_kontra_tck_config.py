"""Self-test of kontra_tck_config.py, the template fill of scripts/tck.sh (M4 WP-02's review), run by scripts/tck.sh
with the other self-tests of test/tck, in the TCK's own environment."""

from __future__ import annotations

import json
import re
import stat
from pathlib import Path

import pytest

import kontra_tck_config as kc

HERE = Path(__file__).resolve().parent

# A KONTRA_TEST_PG with several query parameters ('&', which a sed replacement turns into the matched text), and the
# other characters a sed s||| replacement or a YAML double-quoted scalar treat specially.
DSN = ('postgres://kontra:p|a\\s"s$1@127.0.0.1:5432/kontra_tck_x'
       '?sslmode=disable&connect_timeout=5&application_name=@SELLER@')
VALUES = {
    "LISTEN": "127.0.0.1:40001", "PUBLIC_URL": "https://127.0.0.1:40002", "POSTGRES": DSN,
    "TEMPORAL": "127.0.0.1:40003", "SELLER": "http://127.0.0.1:40004",
}


def scalar(text: str, key: str) -> str:
    """The value of the double-quoted YAML scalar `key: "..."` in text (\\\\ and \\" are its only escapes here, as
    in JSON)."""
    m = re.search(rf'^{key}: "((?:[^"\\]|\\.)*)"$', text, re.MULTILINE)
    assert m, f"no {key}: in\n{text}"
    return json.loads(f'"{m.group(1)}"')


def test_values_are_inserted_literally() -> None:
    out = kc.fill('# fills @POSTGRES@\npostgres: "@POSTGRES@"\nseller: "@SELLER@"\n', VALUES)
    assert scalar(out, "postgres") == DSN
    assert scalar(out, "seller") == VALUES["SELLER"]
    # a value is never scanned for placeholders again: the DSN's own "@SELLER@" stays
    assert "application_name=@SELLER@" in out


@pytest.mark.parametrize("template", sorted(p.name for p in HERE.glob("gate-*.yaml")))
def test_every_template_fills(template: str) -> None:
    out = kc.fill((HERE / template).read_text(), VALUES)
    assert scalar(out, "postgres") == DSN
    assert scalar(out, "listen") == VALUES["LISTEN"]
    assert scalar(out, "public_url") == VALUES["PUBLIC_URL"]
    left = set(kc.PLACEHOLDER.findall(out)) - {"SELLER"}  # the DSN's own
    assert not left, f"{template}: {left} left"


def test_a_placeholder_without_a_value_is_an_error() -> None:
    with pytest.raises(kc.FillError, match="@SECRET@"):
        kc.fill('listen: "@LISTEN@"\nkey: "@SECRET@"\n', VALUES)


def test_a_control_character_is_refused() -> None:
    with pytest.raises(kc.FillError, match="control character"):
        kc.fill('postgres: "@POSTGRES@"\n', {"POSTGRES": "postgres://x\nlisten: 0.0.0.0:1"})


def test_main_reads_the_environment_and_writes_0600(tmp_path: Path, capsys: pytest.CaptureFixture[str]) -> None:
    template = tmp_path / "gate.yaml"
    template.write_text('postgres: "@POSTGRES@"\nlisten: "@LISTEN@"\n')
    out = tmp_path / "kontra.yaml"
    env = {kc.PREFIX + k: v for k, v in VALUES.items()} | {"KONTRA_TEST_PG": "not read"}
    assert kc.main(["kontra_tck_config.py", str(template), str(out)], env) == 0
    assert scalar(out.read_text(), "postgres") == DSN
    assert stat.S_IMODE(out.stat().st_mode) == 0o600
    assert kc.main(["kontra_tck_config.py", str(template), str(out)], {kc.PREFIX + "LISTEN": "x"}) == 2
    assert "@POSTGRES@" in capsys.readouterr().err
