# 7. Refs as the sole workflow currency / CAS run-independence

## Status

Settled. Artifacts + ref-out are **built in Python**. TS ref-out is interpreter design (not yet built).

## Context

Gotcha #2 from the design review: if references do not flow OUT of a workflow step, the TS/Python `DataConverter` rehydrates whole node outputs back into workflow memory — multi-MB payloads against the ~2MB Temporal limit. Separately, the old whole-payload codec bundled bulk units together with the run-specific `task_queue`, leaving per-run residual in the CAS object and defeating cross-run dedup.

## Decision

- **References are the only payloads that flow through workflow code.** The `EntryWorkflow` / interpreter must offload its output and return `{ref, unitCount}` rather than raw lists.
- Payloads **strip run-scoped fields** (the explicit Artifact path separates data from routing) so identical content content-addresses to the **same CAS object across runs** — re-running the same batch adds zero new objects.

## Consequences

- Workflow memory stays bounded: the `DataConverter` no longer rehydrates whole node outputs into the workflow.
- Cross-run dedup works because CAS keys are content-only; a stripped Artifact addresses identically regardless of which run produced it.
- The interpreter contract changes shape: nodes emit `{ref, unitCount}`, not lists.
- Python has Artifacts + ref-out built; the TS interpreter still needs ref-out wired, so the guarantee is currently one-sided.
