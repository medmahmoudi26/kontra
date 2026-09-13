# 44. The Python SDK is called `kontra`, and `actorkit` becomes an alias that says so

## Status

**Accepted.** Renames the Python author surface. Depends on **0023 §9** (there is one deployed kind,
so "actor" no longer distinguishes anything), **0035 §2** (the arrow runs runtime → sdk) and **0042**
(a directory says where its contents run). Supersedes no ADR; the import name it changes was
deliberately *frozen* by 0042's restructure, and this unfreezes it on purpose.

## Context

The two SDKs disagree about the name of the same surface, and only one of them is defensible.

    Go       import "github.com/medmahmoudi26/kontra/sdk/go/catalog"     catalog.Dataset(…)
             import "github.com/medmahmoudi26/kontra/sdk/go/narrate"     narrate.Speak(…)

    Python   from actorkit import catalog, fleet, speak                  catalog.dataset(…)

The *usage* already matches — both say `catalog.…` — so the asymmetry is narrower than it looks. What
differs is the namespace an author types to get there, and Python's names the wrong half of what is
behind it.

**`actorkit` describes one of two surfaces.** Its own `__init__.py` says so:

> Two surfaces, one package:
>   • `actor`   — the one deployed kind: an Actor with one or more Methods (ADR 0023 §9)
>   • `catalog` — the CALLER's side: orchestrate deployed Actors from your own workflow

A workflow is not an Actor. It is the *caller* — it holds a **Fleet**, opens **Sessions**, dispatches
**Methods** and publishes **Datasets**, and it runs on the control machine rather than on a
**Machine**. Every workflow in `kontra-workflows` opens with `from actorkit import catalog, fleet,
speak`, and not one of them defines an Actor.

**And "actor" stopped being a distinction in 0023.** The name was earned when there were three
deployed kinds — Actor, Activity, Monitor — and a kit for the actor one meant something. 0023 §9
collapsed them: *"One deployed kind: the Actor."* A package named for the only kind there is names
nothing.

The word survives correctly everywhere else — **Actor** is the domain term, `@actor.method` is the
decorator, `actor.py` is the module. What is wrong is the package that contains both surfaces being
named after one of them.

## Decision

**The Python SDK is `kontra`.** `from kontra import catalog, fleet, speak`, mirroring the Go module's
root package, which has been `package kontra` since it existed.

**`actorkit` remains, as an alias that keeps working and says what to do.** Re-exports everything and
emits a `DeprecationWarning` naming the replacement, once per process. Removing it is a separate
decision on a separate day; this ADR does not schedule it.

That is not politeness. The 0042 restructure moved `sdk/python/` and deliberately did **not** move the
import name, recording why: *"an actor written against `import actorkit` is untouched by the split."*
Every Actor anyone has written imports it, including ones outside these repositories that we cannot
edit and do not know about. A rename without an alias is a silent break in somebody else's code, at
`load()`, on a Machine, mid-run.

**The Temporal sandbox passthrough carries both names.** `DEFAULT_PASSTHROUGH = ("actorkit",)` is what
stops the workflow sandbox re-importing the SDK per instance. A rename that missed it would not fail
a test — it would make every workflow slower and subtly re-initialised, which is the kind of
regression that surfaces as an unrelated timeout weeks later.

## Considered options

**Leave it.** Free, and the argument for it is that the name is API and API should be boring. Rejected
because the cost is paid by every new reader: the first thing a workflow author does is import a
package named after the thing they are not writing, and the Go author beside them does not.

**Rename to `kontra_sdk` or `kontrakit`.** Avoids the namespace hazard below and aligns with nothing.
A suffix exists to disambiguate from something, and there is nothing to disambiguate from.

**Move only the caller surface — `kontra.catalog`, leave `actorkit.actor`.** Two packages, two install
targets, and a seam down the middle of one SDK. The two surfaces already share `Batch`, `Slot`,
`schema` and the retry types; splitting the package to fix a name would be the tail wagging the dog.

## Consequences

- **`import kontra` can be shadowed by the repository directory, and this was measured rather than
  reasoned about.** From the parent of the checkouts:

      $ cd /root/oss && python3 -c "import sys; sys.path.insert(0,'.'); import kontra; print(kontra.__path__)"
      _NamespacePath(['/root/oss/kontra'])

  Python 3's implicit namespace packages make any directory importable, so a `kontra/` beside a
  `kontra-console/` becomes the package when the parent is on `sys.path`. `actorkit` could not
  collide because nothing is named that.

  It is the ordinary hazard of a project whose package matches its repository — `requests` and
  `flask` have it too — and it needs the *parent* directory on the path, which is not how anyone runs
  a checkout. It is recorded because when it does bite, `import kontra` SUCCEEDS and the failure
  arrives later as a missing attribute, which is a bad five minutes for whoever meets it first.

  **This repository has already paid for this exact class once**, which is why the consequence is
  written rather than waved at. `sdk/python/pyproject.toml` records it: the old layout put the
  package under a seam directory that was itself called `actorkit/`, setuptools' editable finder sits
  *after* the ordinary path finder, "so a checkout resolved `import actorkit` to the SEAM DIR and got
  a package with no modules in it". The fix was to name the directory after the package so there was
  nothing left to shadow. This ADR reopens the same hazard one level up — not inside the checkout,
  but at its parent — and the note now lives in that same file, beside the incident it rhymes with.

- **157 import statements across three repositories**, plus `kontra-actors` and `kontra-workflows`,
  which are separate repos on their own branches. The alias is what makes that sequencing survivable:
  the SDK can land before its consumers without a flag day.

- **Documentation is not swept.** ADRs, incident notes and corpus `history` fields describing what was
  true in 2026-08 keep saying `actorkit`, for the same reason the `campaign` tombstone keeps saying
  `campaign`. A rename that edits the historical record makes the record useless.

- **The alias is a place a warning can be missed.** A `DeprecationWarning` is invisible by default in
  many runners, so the alias also states itself in its module docstring, and the SDK's own suite
  asserts both that the alias works and that it warns.
