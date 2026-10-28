# test/tck

The pinned A2A TCK against the gate (spec/relay.md §0, §16: `tck_jsonrpc`, `tck_rest`, `tck_behind_caddy`; D18,
DM4-12).

- `kontra_xfail.py`: a pytest plugin (from M0's research) that marks every node id listed in
  `$TCK_EXPECTED_FAILURES` `xfail(strict=True)`. A listed test that passes fails the run, and so does an entry that
  names no collected test. Loaded with `-p kontra_xfail` and `PYTHONPATH` naming this directory.
- `expected-failures.txt`: the expected failures, each with its reason and upstream issue.

`make tck VARIANT=direct|tunnel|caddy` (`scripts/tck.sh`, M4 WP-02) runs the TCK at
`d4d6fae7887f88ef607fb461c13e3b0e1802f19b` against a `tck` service with `allow: [anonymous]`, hosted by the
`examples/sellers/tck` seller on the direct or the tunnel backend, or behind `examples/front/caddy`, one run per
binding (`--transport jsonrpc`, then `http_json`).
