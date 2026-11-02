# A2UI v0.9.1 schemas (vendored)

Copied unmodified from the A2UI repository, <https://github.com/a2ui-project/a2ui>, at commit
`c1baa4ee9c0cf2241a5b4b666005b62695480519` (2 Nov 2026), under its Apache License 2.0 ([`LICENSE`](LICENSE), the
repository's own). v0.9.1 is the A2UI production release ("This specification is closed",
`specification/v0_9_1/README.md`).

| Here | There |
|---|---|
| `json/*.json` | `specification/v0_9_1/json/` (the message, list and capabilities schemas the example's messages use) |
| `catalogs/basic/catalog.json` | `specification/v0_9_1/catalogs/basic/catalog.json` (the basic catalog, `catalogId` `https://a2ui.org/specification/v0_9/catalogs/basic/catalog.json`) |
| `LICENSE` | `LICENSE` |

`SHA256SUMS` pins each file; `make check-specs` (`scripts/check_specs.py`) checks the sums and validates the example's
fixtures against these schemas with a JSON Schema 2020-12 validator. The schemas name the catalog as `catalog.json`
relative to their `$id`s; the check serves the basic catalog under that name too, as the A2UI repository's own test
runner does (`specification/v0_9_1/test/run_tests.py`).
