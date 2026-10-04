# kontra

## Branches

**Work on `dev`. Never commit to `main`.**

`dev` is the default branch and the base for every pull request. `main` is what has been released —
it moves only by merging `dev`, and only deliberately.

Branch protection is not available on this repository (it needs GitHub Pro for a private repo), so
**nothing mechanically stops a push to `main`.** This paragraph is the enforcement. If you find
yourself on `main`, switch before you commit:

```sh
git switch dev        # or: git switch -c <topic> dev
```

Same two branches, same rule, in all five repositories: `kontra`, `kontra-actors`,
`kontra-workflows`, `kontra-console`, `kontra-cloud`.

## Agent skills

### Workflows

**These are plain Temporal workflows. There is no kontra workflow framework — use Temporal's own
primitives rather than inventing structure.** A workflow is a unit of history and retry: decompose
with child workflows and `continue_as_new`, never with helper methods or a context object, and never
by turning a paging loop into an activity. Read `docs/agents/workflows.md` **before** writing or
restructuring anything under `workspaces/*/workflows/`.

### Issue tracker

Issues and PRDs live as markdown files under `.scratch/<feature-slug>/` in this repo. See `docs/agents/issue-tracker.md`.

### Triage labels

The five canonical triage roles, used verbatim (`needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`). See `docs/agents/triage-labels.md`.

### Domain docs

Multi-context: `CONTEXT-MAP.md` at the root points to one `CONTEXT.md` per seam. See `docs/agents/domain.md`.
