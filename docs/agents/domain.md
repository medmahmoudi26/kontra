# Domain Docs

How the engineering skills should consume this repo's domain documentation when exploring the codebase. **This is a multi-context repo** (one context per seam — e.g. `contracts/`, `sdk/python/`, `orchestrator/`, `conformance/`, `infra/`).

## Before exploring, read these

- **`CONTEXT-MAP.md`** at the repo root — it points at one `CONTEXT.md` per context. Read each one relevant to the topic you're about to work in.
- The per-context **`CONTEXT.md`** files it references.
- **`docs/adr/`** at the root — system-wide architectural decisions. Read the ADRs that touch the area you're about to work in. **Only the records at the top level describe the running system**; `docs/adr/legacy/` is the pre-v2 corpus, archived by ADR 0024 and readable as evidence about *why*, never as authority about *what*. Do not implement from a legacy record.
- **`<seam>/docs/adr/`** — if a context keeps its own context-scoped decisions, read those too.

If any of these files don't exist yet, **proceed silently**. Don't flag their absence; don't suggest creating them upfront. The producer skill (`/grill-with-docs`) creates `CONTEXT-MAP.md` and the per-context `CONTEXT.md` files lazily, when terms or decisions actually get resolved. (Today only the root `docs/adr/` exists — that's expected.)

## File structure

Multi-context repo (presence of `CONTEXT-MAP.md` at the root):

```
/
├── CONTEXT-MAP.md                     ← points at each context's CONTEXT.md
├── docs/adr/                          ← system-wide decisions (exists today)
│   ├── 0001-nexus-operations-namespace-per-author.md
│   └── ...
├── contracts/
│   └── CONTEXT.md
├── sdk/python/
│   ├── CONTEXT.md
│   └── docs/adr/                      ← context-specific decisions (optional)
└── orchestrator/
    └── CONTEXT.md
```

## Use the glossary's vocabulary

When your output names a domain concept (in an issue title, a refactor proposal, a hypothesis, a test name), use the term as defined in the relevant `CONTEXT.md`. Don't drift to synonyms the glossary explicitly avoids.

If the concept you need isn't in the glossary yet, that's a signal — either you're inventing language the project doesn't use (reconsider) or there's a real gap (note it for `/grill-with-docs`).

## Flag ADR conflicts

If your output contradicts an existing ADR, surface it explicitly rather than silently overriding:

> _Contradicts ADR-0007 (refs as currency) — but worth reopening because…_
