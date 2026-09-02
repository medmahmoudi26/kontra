# Building with the kontra Dashboard system

kontra runs a job over a batch of inputs. An **Actor** is a deployed worker declaring one or
more **Methods**. A **Run** is one execution of a caller's own workflow, which pages a
**Dataset** into **Batches** and calls Methods — a Batch in, a Batch out.

This is a **control plane**, not a spreadsheet and not a canvas: persistent left nav →
resource list → detail page with tabs → status pills → live telemetry. Calm and dense.
**Dark-first.** Monospace for every identifier (actor names, queues, run ids, digests);
proportional for prose. Nothing blinks; live things update in place.

**There is a canvas, and it does not run anything.** The EXECUTION editor was removed on
purpose — a Run is one execution of a caller's workflow, never something this UI starts —
and **Scratch** replaced it: a drawing that produces code. Build canvases with the
`ScratchNode*` components inside `ReactFlow` (both are exported), and `ScratchInspector`
for the sidecar. **Never put a run, play or dispatch affordance on a node**; that is the
exact mistake the distinction exists to prevent. The stylesheet still carries dead classes
from the old editor (`.canvas`, `.actor-node`, `.io-row`, `.mapping-table`, `.link-panel`,
`.seed-table`, `.panel.inspector`) — **do not use them**; Scratch touches none of them.

**Nothing is a fixed width.** Every sidecar resizes, folds to a rail and can be docked to
either edge (`SideResizer` / `SideRail` / `SideDockControls` + `useSideDock`), and stacked
panes reorder and fold (`PaneStack` + `usePaneOrder`). A panel with a hardcoded `w-[264px]`
and no handle is the thing those replaced — do not build one.

## Setup

No provider is needed. Two rules:

**1. Dark is an ancestor class.** The dark variant compiles to `&:is(.dark *)`, so `dark:`
utilities need a `.dark` ANCESTOR — putting `.dark` on the element itself does not style it.

```jsx
<div className="dark">
  <div className="bg-background text-foreground p-4">{/* build here */}</div>
</div>
```

**2. `SideNav`, `WorkerPane` and `FolderWorkbench` read the store, not props.** Seed it once
at module scope — it is a singleton, so two instances cannot show different state:

```js
useAppStore.setState({ catalog: [...], view: 'actors', theme: 'dark' })
```

**3. A sidecar's layout is per-panel state, not a prop.** `useSideDock('<panel>', { width,
side, collapsed })` owns it and persists it; the panel renders `SideRail` when collapsed,
and otherwise itself plus a `SideResizer` on the edge it is NOT docked to (a handle on the
wrong side drags the panel away from the pointer). `PaneStack` is the same shape for a
column of panes, keyed by pane name — never by index.

## Styling idiom

Tailwind v4 utilities over shadcn/ui CSS-variable tokens. **The stylesheet is a STATIC
compile: only utilities actually used exist in it.** `bg-red-500`, `p-8` and `grid-cols-3`
are NOT in the sheet and will silently do nothing. Stay inside the vocabulary below; for
anything else use `style={{ … }}` with a token `var(--*)`, which is what the shipped
components do for one-off pixel dimensions.

| Family | Available |
|---|---|
| surface | `bg-background` `bg-card` `bg-muted` `bg-popover` `bg-accent` `bg-secondary` `bg-primary` `bg-transparent` |
| text | `text-foreground` `text-muted-foreground` `text-card-foreground` `text-primary` `text-destructive` `text-xs` `text-sm` `text-lg` `text-2xl` · arbitrary px from `text-[8px]` to `text-[16px]` (and `text-[23px]`) |
| border | `border` `border-border` `border-input` `border-primary` `border-solid` `border-dashed` `border-transparent` `border-b` `border-t` `border-l` `border-r` `border-x` `border-y` · `border-b-2` `border-t-2` `border-l-2` `border-r-2` `border-t-primary` |
| radius | `rounded` `rounded-md` `rounded-lg` `rounded-xl` `rounded-full` `rounded-br` |
| spacing | `gap-0.5 gap-1 gap-1.5 gap-2 gap-2.5 gap-3 gap-3.5 gap-4 gap-6` · `p-0.5 p-1 p-1.5 p-2 p-2.5 p-3 p-4 p-5` · `px-0.5 … px-6` · `py-px py-0.5 py-1 py-1.5 py-2 py-3` · `mt-0.5 mt-1.5 mt-2.5 mt-3.5` |
| layout | `flex` `flex-col` `flex-wrap` `flex-1` `grid` `grid-cols-2` `grid-cols-[auto_1fr]` `items-center` `items-baseline` `justify-between` `self-stretch` `min-h-0` `min-w-0` `shrink-0` |
| type | `font-mono` `font-medium` `font-semibold` `tabular-nums` `uppercase` `tracking-wide` `tracking-wider` `truncate` `break-all` `line-clamp-2` `whitespace-nowrap` |
| resize chrome | `cursor-col-resize` `cursor-row-resize` `cursor-grab` `active:cursor-grabbing` `touch-none` `opacity-50` |
| state hues | `bg-emerald-500/15 text-emerald-400` · `bg-red-500/15 text-red-700` · `bg-amber-500/15 text-amber-300` · `bg-rose-500/15 text-rose-500` · `bg-sky-500/10 text-sky-500` · `bg-violet-500/10 text-violet-500` · `bg-zinc-500/10` |

Tokens (defined for light and `.dark`): `--background --foreground --card --card-foreground
--popover --primary --primary-foreground --secondary --muted --muted-foreground --accent
--destructive --border --input --ring --ok --radius`.

**Identifiers get `font-mono`** — run ids, queue names, actor names, digests, tmux targets.
Counts get `tabular-nums`.

**Gotcha:** an unlayered `input[type="text"], input[type="number"], select, textarea
{ font: inherit }` rule outranks Tailwind, so `font-mono`/`text-xs` on a `<textarea>` or an
`<Input>` do nothing. Put the font on the **parent** and let it inherit.

## Invariants — do not flatten these

- **Dataset lifecycle is three states plus the absence of one**: `open` (amber), `sealed`
  (green), `abandoned` (struck-through grey), and `no lifecycle` (dashed). Never collapse to
  two — an `open` Dataset drawn as `sealed` reads as a finished result while its producer may
  still be writing, or may have died. Use `datasetBadge(state)`, which returns
  `{ label, className, title }`.
- **Health is tri-state, never boolean**: `ok` (solid green ●), `bad` (solid red ▲),
  `unknown` (DASHED, unfilled, `?`). "Nobody looked" must never render as "fine". Use
  `chipState(ok)` / `HealthChips`.
- **Dropped and isolated units must be impossible to miss.** A run that isolated everything
  and a run that legitimately found nothing must not look the same.

## Where the truth is

Read `_ds/<folder>/styles.css` and its `@import` closure (`fonts/fonts.css`,
`_ds_bundle.css`) for the real stylesheet, and each component's `<Name>.prompt.md` and
`<Name>.d.ts` for its API. Those beat this summary.

## The shell every surface is built in

A page is a resizable sidecar, its handle, and the content. `order` moves the whole group
across the page so the content section never has to know which side it is on.

```jsx
function Surface() {
  const dock = useSideDock('workflows', { width: 264, side: 'left', collapsed: false });
  if (dock.collapsed) {
    return <SideRail label="Workflows" badge={7} onExpand={() => dock.setCollapsed(false)} />;
  }
  const aside = (
    <aside style={{ flex: `0 0 ${dock.width}px`, width: dock.width }}
           className="flex min-h-0 flex-col">
      <div className="flex items-baseline gap-2 border-b border-border px-3.5 py-2.5">
        <span className="text-[9.5px] uppercase tracking-wide text-muted-foreground">Workflows</span>
        <SideDockControls dock={dock} label="the workflow list" />
      </div>
      {/* rows */}
    </aside>
  );
  const handle = <SideResizer width={dock.width} onWidth={dock.setWidth}
                              side={dock.side} label="the workflow list" />;
  return (
    <div className={`flex min-h-0 ${dock.side === 'left' ? 'border-r' : 'border-l'} border-border`}
         style={{ order: dock.side === 'left' ? -1 : 1 }}>
      {dock.side === 'left' ? <>{aside}{handle}</> : <>{handle}{aside}</>}
    </div>
  );
}
```

## Idiomatic example

**There is no `Card` component, and there is deliberately no `Textarea`.** The three shadcn
primitives this system ships are `Badge`, `Button` and `Input`. A panel is a plain element
wearing the app's own surface classes — `rounded-xl border border-border bg-card`, which is
exactly what `ActorCard` is built from — so a card is a shape, not an import. Reach for
`ActorCard`, `WorkerPane`, `PaneStack` or `TileWall` when one of them already IS the surface
you are drawing; compose the classes yourself when none is.

```jsx
<div className="dark">
  <div className="bg-background text-foreground p-4 flex flex-col gap-3">
    <div className="overflow-hidden rounded-xl border border-border bg-card">
      <div className="flex items-start gap-3 px-3.5 py-3">
        <div className="min-w-0 flex-1">
          <div className="flex items-center justify-between gap-3">
            <span className="font-mono text-[12.5px] font-semibold">subdomains.subfinder</span>
            <Badge variant="success">sealed</Badge>
          </div>
          <p className="m-0 mt-1.5 font-mono text-[10.5px] text-muted-foreground">
            37,412 rows · written 6 minutes ago
          </p>
        </div>
      </div>
      <div className="flex gap-2 px-3.5 pb-3">
        <Button size="sm">Query</Button>
        <Button size="sm" variant="outline">Copy name</Button>
      </div>
    </div>
    <HealthChips health={{ reachable: 'ok', session: 'present', poller: 'none', loads: 'failing' }} />
  </div>
</div>
```
