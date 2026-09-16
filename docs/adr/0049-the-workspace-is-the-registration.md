# 49. The workspace is the registration

Date: 2026-09-16

## Status

Accepted.

Replaces the registration half of the source registry — the `register`/`forget` verbs and the stored
paths behind them. It depends on the named-workspace machinery
[ADR 0047](0047-local-cluster-and-docker-fleet.md) introduced, and promotes it from "an extra root
Compose can mount" to the only one.

## Context

A folder became an Actor or a Workflow in one of two ways: it sat under a conventional root
(`~/.kontra/actors`, `~/.kontra/workflows`) and was discovered, or somebody ran
`kontra actor register <dir>` and the orchestrator wrote its absolute path into the database.

The second way was deliberate and the reasoning was good: your code lives in your checkout, beside
the actors it drives, and moving it into a directory the platform chose is a tax. Registration
recorded a path and `serve` was allowed to run code from a registered folder and nowhere else — an
allowlist an operator fills, rather than one constant the code picks.

**Measured on a live install, four weeks in.** Four registered folders, pointing into three
different checkouts:

    desync     /root/oss/kontra-actors/go/desync
    nscheck    /root/kontra-local/examples/go/nscheck
    reddit     /root/kontra-local/examples/private/reddit-scrapper
    redditapi  /root/kontra-local/examples/private/reddit-api

…and, on the workflow side, `crawl` — pointing at `/root/oss/kontra-workflows/python`, a directory
deleted weeks earlier. It listed. It rendered a card. It offered a Run button. Pressing anything on
it produced `404 — crawl: no such workflow`. The operator's words were: *"which must not exist in
the first place, if the file doesn't exist in the workspace, it just shouldn't be registered nor
shown."*

That is not a bug in the listing code — the listing was faithful. **A recorded path outlives the
directory it names**, and everything downstream had to carry the consequence: an `absent` flag on
the Source, a state for every surface to render, a `forget` verb to clean up by hand, and a `serve`
route that refuses with a 404 for a folder the console itself drew.

The registry cost one more thing that is harder to see: the boundary `serve` runs code through was a
**database table**. A security boundary that can only be audited with a SQL query, and that drifts
from the filesystem by construction.

## Decision

**1. The workspace is the registration.** A folder is an Actor or a Workflow because it is at
`<workspace>/actors/<name>` or `<workspace>/workflows/<name>` and carries its kind's marker. There
is no other way, and there is nothing else to do.

**2. `register` and `forget` are gone** — the CLI verbs, the `POST /api/sources/:kind` and
`DELETE /api/sources/:kind/:id` routes, the stored rows, and the `absent` state that existed to
describe a registration whose folder had left. The routes answer **410 Gone** with the sentence that
replaces them rather than 404, because a script that still posts is doing something that used to
work.

**3. The listing is the filesystem, read on every request.** Adding a folder adds it; deleting it
removes it; `git checkout` of a branch without it is not a state anybody has to repair. No cache:
"the console shows my new actor without a restart" is the property this is for, and a directory read
is cheap. The content digest — which is not cheap, because a Go actor folder carries a binary — stays
out of the listing.

**4. Discovery keys on the CODE, not the manifest.** `workflow.py` makes a folder a workflow;
`workflow.json` makes it complete. A folder with code and no manifest is listed and incomplete, and
the acts that need a version refuse by name. It used to key on the manifest, which under this
decision would mean: drop your code in the workspace, nothing appears, and no error anywhere says
why.

**5. The workspace is a visible directory beside the install, not a hidden one and not a Docker
volume.** The default is `./workspaces` in the directory where `docker compose up` was run, mounted
at the same path inside and out. You edit this code in your own editor, so it has to be somewhere
you can see from where you started the thing; and the control plane hands these paths to the CLI and
to tmux panes, so a different path inside the container makes every one of them unopenable.

*The previous default was `../workspaces.kontra` — beside the checkout, one level up, and dot-named.*
Two of those three properties made it hard to find on purpose, which is the opposite of what a
directory holding your code should be.

**6. Switching workspaces is a control-plane fact, exposed as a selector in the console's header.**
`.current` in the workspace parent is what discovery reads, so the console writes that file and
reloads. It is deliberately not a client-side filter: a worker serving code from another workspace is
still serving it, and a console that pretended otherwise would show an inventory nothing else agrees
with.

**7. `kontra <kind> init <dir>` keeps the half of `register` that was a service** — writing the
starter manifest — with no side effect on the control plane. Making a folder well-formed and making
it served are two different acts, and coupling them is what produced entries for code nobody could
open.

## Consequences

**Code outside the workspace is not reachable, and that is the trade.** The registry existed to let
code live anywhere; the mount replaces it. "Wherever your code lives" is now expressed once, in the
compose file, instead of once per folder in a database. An operator with actors in three checkouts
either mounts three workspaces and switches, or moves the code.

**An Actor in the catalog with no folder in the workspace can be listed and not served.** The
catalog is what a running worker registered about itself — 29 entries on the install above, going
back months — and the workspace is the code on this disk now. The Actors surface says which is which,
because "press Serve" and "go and find the code" are different instructions.

**The `sources` table is no longer read.** It is left in place rather than migrated away: an
operator who rolls back to a build with `register` in it gets their rows, and nothing in the new code
path is confused by a table it never opens.

**`serve`'s allowlist is a directory now.** Auditable with `ls`, and it cannot drift from what is on
disk, because it is what is on disk.

## What this does not decide

Whether a workspace should be able to nest — a folder of folders, so one mount can hold several
teams' code — and whether the catalog should hide Actors whose code is not in the current workspace
rather than showing them with an explanation. Both are answerable once people have used one
workspace for a while.
