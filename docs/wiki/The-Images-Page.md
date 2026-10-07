# The Images page

> [!WARNING]
> **Not shipped.** There is no Images entry in the console, and none of the `/api/images/<sub>`
> routes exist. This page describes what that surface is **for** — the four questions it answers and
> where each answer comes from — so the shape is settled before it is built. Nothing here can be
> clicked today; [what to use instead](#what-answers-these-questions-today) is at the bottom.

The Images page answers one question that nothing in kontra answers today: **why is the registry this
big, and what is safe to delete?** It is a top-level nav entry between Actors and Datasets, with four
sub-tabs.

| sub-tab | the question it answers |
|---|---|
| **Runtimes** | what OS layers do my actors sit on, and which actors are behind a newer one? |
| **Actor images** | what did each version cost, what is it built on, and what is keeping it alive? |
| **Layers** | which layers are the bytes, and are they shared or are they one actor's alone? |
| **Storage** | how much is on disk, how much did dedupe save, and what goes at the next GC? |

## Runtimes

One row per runtime **and major**: name, description, `provides` chips, current version and short
digest, size, a signed badge, published date, and **"N actors · M behind"**.

That last column is the point of the tab. "M behind" counts the actor versions whose recorded
`runtime.digest` is not the digest the `:<major>` tag points at now — which is the whole of rebase
detection, rendered. The row action is **"Rebase M actors"**, behind a confirmation that *lists the
actor versions it would move*.

Two states this tab has to get right:

- **Empty.** A runtimes prefix with no images is the normal state of a fresh install, not an error. It
  says so, and shows the mirror command rather than an empty table.
- **Edited elsewhere.** The header links to the `kontra-runtimes` repository, labelled "Edit
  runtimes", because runtimes are changed in their repository and never in the UI
  ([[Writing-a-Runtime]]).

## Actor images

Grouped by actor, newest version first. Per row: version, short digest with a **current** or
**superseded** badge, the runtime it was built on, **unique size / total size**, pushed date, last
used, a **rebase available** badge, and the in-use reasons as chips — `catalog`, `2 machines`,
`run r_8f3c1a`.

`unique` against `total` is the number that stops a wrong deletion: a 175 MiB image whose unique bytes
are 0.4 MiB costs almost nothing to keep, and deleting it reclaims almost nothing.

**Row expansion is the layer stack**, drawn bottom to top — runtime, then deps, then app — each bar
labelled with its size and a "shared by N images" count. Hovering a layer highlights every other image
that shares it, which is how "why is this 40 GB" becomes visible rather than inferred.

Actions: **Rebase**, and **Delete version** — disabled with a tooltip naming what keeps it alive, and
otherwise behind a confirmation that names the version *and* the digest. Filters: actor, runtime,
"rebase available", "not used in 30 days".

## Layers

Sorted by **size × reference count**, so the biggest wins are at the top. Columns: kind, source
(`heroku/python:venv`, `runtime python-browser:1`, `app`), size, referenced by N images, and the image
list on expand.

The tab exists to make one distinction readable: a **deps** layer referenced once is a lockfile nobody
else shares — the thing to look at — while many **app** layers are fine, because they are small. A
sort by size alone hides that.

## Storage

Registry bytes on disk, the bytes dedupe saved, and the bytes reclaimable at the next GC. Then the
retention policy **in effect**, read-only, as a table; last and next GC times; and a **Run GC now**
button.

Two honest readings this tab has to support:

- Retention defaults to **`dryrun`**, so "reclaimable at next GC" is today a report of what *would*
  go, not a countdown. The tab has to say which mode it is reading.
- The dedupe saving is real and measured: the same content that occupied **7.9 GiB** under
  `registry:2` occupies **4.7 GiB** in zot — about 40%, before any retention deletes anything.

## Where the answers come from

| data | source |
|---|---|
| repositories, tags, manifests, sizes, referrers | zot's **search** extension (GraphQL), which is compiled into the pinned build |
| **layer kinds** | each image's `io.buildpacks.lifecycle.metadata`, read from the **config blob** |
| **in use** | the catalog's current digest — which is also all the `inuse-` reconciler can read. Placements and in-retention runs are wanted here too and neither is readable from a table, so those chips have no source yet ([[Durability-and-Failures]]) |

**Never parse layer tarballs.** Everything above comes from manifests, config blobs and the control
plane's own records.

### Two traps, both measured

**The metadata records `diff_id`s, not layer digests.** `io.buildpacks.lifecycle.metadata`'s
`app[].sha`, `buildpacks[].layers.*.sha` and `runImage.topLayer` are **uncompressed** digests; a
manifest's layers are **compressed**. Measured on a real pack image: 3 of 3 metadata shas matched
`config.rootfs.diff_ids`, and **0 of 3** matched the manifest's layer digests. The join is
`config.rootfs.diff_ids[i] ↔ manifest.layers[i]`, by index, across two parallel ordered arrays.
Matching the metadata sha straight against layer digests yields `kind: "unknown"` for 100% of layers
and looks like it is working.

**zot's GraphQL `Labels` field is empty.** Measured: it returned `''` for an image carrying ten
labels, `io.buildpacks.lifecycle.metadata` among them. The label lives in the config blob, reachable at
`/v2/<repo>/blobs/<configDigest>` with the digest from GraphQL's `Manifests.ConfigDigest`.

## Reading rules the page owes the operator

- Byte sizes in **binary** units (MiB, GiB) with one decimal. The console's shared formatter,
  `byteWords` in `packages/core/src/panels/chrome/format.ts`, already labels binary units honestly —
  `B`, `KiB`, `MiB` — but **stops at MiB**, so a 4.7 GiB registry total renders as `4812.8 MiB`. This
  page needs a GiB step added there, not a fourth formatter. The two that *are* dishonest divide by
  1024 and label the result `KB`/`MB`, and both are local to one view rather than shared:
  `packages/svelte/src/datasets/Datasets.svelte`'s `bytes()` and
  `packages/core/src/panels/folderWorkbench.ts`'s `fmtBytes`.
- Digests short — 12 hex — with copy-full on click. Copy needs the `execCommand` fallback, not
  `navigator.clipboard` alone: that API requires a secure context, `localhost` counts, and a
  Controller reached over its VPC address on plain HTTP does not.
- **Every destructive action names what it affects and what is keeping it alive.** A delete that is
  merely refused teaches nothing; one that says *"placed on 2 machines"* ends the question.
- Tables page server-side at 200 rows.

## What it is waiting on

Not a list of tasks — these are the reasons the page cannot simply be drawn:

- **The routes.** None of `/api/images/runtimes|actors|layers|storage|rebase|gc` exist, and the auth
  gate answers 403 to any `/api` path that has not declared a posture, so they are unreachable until
  declared as well as written.
- **The scope.** The design gates mutations on an `images:admin` scope. Two scope strings exist (`console`,
  `infra`), the only mint path grants `console`, and there is no user→scope mapping — so as specified,
  *no console session could ever press the buttons*. That contradiction needs settling before the
  page, not during it.
- **Rebase itself.** There is no `kontra rebase`, no detection reconciler, and no digest history on a
  catalog entry — so "M behind" and "superseded" have nothing to read yet ([[Runtimes]]).
- **`signed`.** Nothing signs an actor or runtime image today, so the badge would be uniformly false.
- **The nav entry is two repositories.** A ninth surface declared only in the console 404s on every
  cold load and every pasted link, because the server's SPA fallback holds the surface list.

## What answers these questions today

```sh
# every repository the registry holds (add -u pull:… if the registry passwords are set)
curl -s http://127.0.0.1:5000/v2/_catalog | python3 -m json.tool

# what the registry is, and which extensions it was compiled with — never needs a credential
curl -s http://127.0.0.1:5000/v2/_zot/ext/mgmt
```

`GET /api/images` already lists built images, labels each one as an actor Image or a Bundle, and
carries a **per-image byte total** — the manifest's config size plus its layer sizes, which is what a
pull would transfer. It is on by default; `?facts=0` drops it and the digest to save one manifest
round trip per tag. The console's **Actors** page renders that list.

What it does **not** carry is the per-layer breakdown, `unique` versus `total` bytes, the runtime an
image was built on, in-use reasons, or any storage total — which is the gap this page exists to close.
One byte number per tag answers "how big is this image"; it cannot answer "what would deleting it
actually reclaim".

---

**See also:** [[Runtimes]] · [[Writing-a-Runtime]] · [[Deployment]] · [[Dashboard]] ·
[ADR 0061](../adr/0061-buildpacks-runtimes-and-the-image-store.md)
