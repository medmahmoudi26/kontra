# reporting

The documented example of a workflow that writes a report.

It dispatches nothing and touches no target — the numbers are constants, so the rendered report is
byte-identical on every run and a test can assert on it. What it demonstrates is the contract: the
workflow computes everything and returns it, and `report.md` only presents what came back.

## What to look at

`workflow.py` returns a typed `EnrichResult`. Every field in it is read by `report.md` and nothing
else is — there is no way for a template to ask a question, which is what makes a report reproducible.

`report.md` is Liquid over Markdown. The `{% if result %}` branch is not decoration: `result` is
`null` for a run that failed, was cancelled or timed out, and the `{% else %}` branch is what those
runs render.

`result.sample_request` carries a credential on purpose. The `http` block redacts it before anything
is stored, so the report, both exports and the MCP tool all show `[redacted]` — and the original is
reachable only through the audited reveal route.

## Try it

    kontra workflow serve reporting            # lints report.md before anything runs
    kontra workflow start reporting --wait
    kontra report preview <run-id>             # render an edited template, store nothing
