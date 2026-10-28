# test/tck

The pinned A2A TCK against the gate (spec/relay.md §0, §16: `tck_jsonrpc`, `tck_rest`, `tck_behind_caddy`; D18,
DM4-12, DM4-24): `make tck VARIANT=direct|tunnel|caddy|auth` ([`scripts/tck.sh`](../../scripts/tck.sh)), and the
`tck` workflow ([`.github/workflows/tck.yml`](../../.github/workflows/tck.yml)) on pull requests that change the gate
(direct, tunnel, caddy) and nightly (the four).

| Variant | What the TCK talks to | Named test |
|---|---|---|
| `direct` | the gate itself, `tck` on the direct backend (`gate-direct.yaml`) | `tck_jsonrpc`, `tck_rest` |
| `tunnel` | the gate itself, `tck` hosted through `kontra connect` (`gate-tunnel.yaml`) | `tck_jsonrpc`, `tck_rest` |
| `caddy` | [`examples/front/caddy`](../../examples/front/caddy/README.md) terminating TLS, the direct backend behind it (`gate-caddy.yaml`) | `tck_behind_caddy` |
| `auth` | a loopback Caddy that adds `Authorization: Bearer <a tck API key>` (`front-auth.Caddyfile`, `gate-auth.yaml`) | nightly: ListTasks and push |

The seller is [`examples/sellers/tck`](../../examples/sellers/tck/README.md) (a2a-go v2.7.0). Each run is one variant:
a fresh database, a Temporal development server, the gate, the seller, the node and Caddy as processes from the run's
directory, then the TCK once per binding (`--transport jsonrpc`, then `http_json`), then everything removed; the run
fails if a process naming its directory survives (pgrep). It takes about 90 s.

```sh
export KONTRA_TEMPORAL_CLI=<the pinned temporal CLI 1.9.1>   # required
export KONTRA_TCK_DIR=<a2a-tck checked out at d4d6fae>        # required; its tracked files unmodified
export KONTRA_TEST_PG=postgres://…                            # optional: else a postgres:17-alpine container
export KONTRA_CADDY=<caddy 2.11.7>                            # caddy and auth (else caddy on PATH)
make tck VARIANT=caddy
```

The reports land in `$KONTRA_TCK_DIR/reports`: `<variant>-<binding>.xml` (JUnit), `.html` (pytest-html),
`-compatibility.json`/`.html` (the TCK's own report), and `<variant>-logs/` (the processes' logs; no credential is
logged). Gate on the exit status, not on the TCK's percentages (a2a-tck#234).

## The expected outcomes

- [`expected-failures.txt`](expected-failures.txt): every test that does not pass, failures and skips alike, with its
  reason (an a2a-tck issue or a decision), per variant (`[direct tunnel caddy]`, `[auth]`) and binding. An expected
  skip is a line `skip <node id>  # <reason>`. None is a gate defect: TCK bugs (a2a-tck#231, #242), a2a-go v2.7.0's
  (D7: SendMessage's `historyLength`, a body read whatever its Content-Type), D5 (a seller's Message is answered as a
  task), D20 (the anonymous principal is refused ListTasks and push configs; the TCK sends no credential,
  a2a-tck#163), and the errors the gate's card rules out (it pushes and streams, D16, D52; no extended card, D52).
- [`kontra_xfail.py`](kontra_xfail.py): a pytest plugin, loaded into the TCK's pytest with `-p kontra_xfail` and
  `PYTHONPATH` naming this directory, `TCK_EXPECTED_FAILURES` naming the file and `TCK_VARIANT` the variant. It
  marks the expected failures of the run's binding `xfail(strict=True)` and fails the run when the outcomes differ
  from the file in any way: an expected failure that passed (strict XPASS), failed with an error, or skipped; a test
  not listed that ended xfailed (the TCK xfails SHOULD requirements itself); an expected skip that did not skip; a
  test of the run's binding that skipped and is not listed (the TCK skips whenever a prerequisite request fails or
  answers an unexpected state, so a gate regression would otherwise hide as a skip); an entry naming no collected
  test (the TCK collects every binding in every run). A test's binding is its id's suffix (`[jsonrpc]`,
  `-http_json]`), else its directory (`tests/compatibility/jsonrpc/`), else its function's name suffix (`_jsonrpc`,
  `_rest`) or its class's name prefix (`TestJsonRpc…`, `TestRest…`), which the TCK's unparametrized tests in its
  shared directories carry; the other bindings' tests skip "not configured" and need no entry. A malformed file (no
  reason, no reference, a duplicate, an unknown variant) is a usage error.
- [`kontra_tck_ids.py`](kontra_tck_ids.py): a pytest plugin (`-p kontra_tck_ids`) that gives each test fresh TCK
  message ids (below).
- [`kontra_tck_config.py`](kontra_tck_config.py): fills `gate-<variant>.yaml` for the script, literally (values from
  `KONTRA_TCK_FILL_<NAME>`, escaped for YAML's double-quoted scalars), mode 0600.
- `test_*.py`: the self-tests of the plugins, the fill and the script's text, with pytester, in the TCK's own
  environment; `scripts/tck.sh` runs them before the TCK.

```sh
PYTHONPATH=test/tck uv run --frozen --no-sync --project "$KONTRA_TCK_DIR" python -m pytest -p pytester \
  -p no:cacheprovider test/tck
```

## The TCK's message ids

The pinned TCK's message ids are one per session, not one per call (`tck_id`, a2a-tck `tck/requirements/base.py`;
#250 fixed it for follow-ups only): every prerequisite task it creates with the same scenario sends the same request
with the same id. The gate answers an identical request as a resend with the first one's task (spec/relay.md §15;
A2A §3.3.1 lets an agent detect duplicates by message id), so once an early test had completed or canceled the
session's `tck-input-required` task, the tests that need a fresh one skipped ("Expected task state … INPUT_REQUIRED
but got COMPLETED"): CORE-CANCEL-001, CORE-MULTI-005, STREAM-SUB-001/002 and the history tests never ran through the
gate. `kontra_tck_ids.py` gives `tck.requirements.base._SESSION` a fresh value for each test (an autouse fixture; no
TCK file is changed), so each test's prerequisites are new tasks and those tests run (DM4-24); the ids the TCK's
requirement catalogue computed at import keep the session's value. The TCK found one gate defect through its
resends: a blocking resend of a task at input-required waited for a turn that had ended, until the caller's timeout
(DM4-23, fixed, `internal/relay` `TestBlockingResendAtTurnEnd`); WP-02's review found the same for a quoted task,
which the TCK never creates (DM4-23, fixed, `TestBlockingResendOfAQuotedTask`).

Re-pin when a2a-tck #200 (credentials), #232 (v1.0.1 codes), #243 (camelCase) and #197 (extensions) merge, and when
the message ids are per call (then drop `kontra_tck_ids.py`); the strict XPASS of each entry, and "listed as a skip,
but passed", then say which to delete.
