// What an agent returns, on a task's page: a result (an A2A artifact, folded by id) as one section
// of the Results card -- a file is one row (its name, size and type, Open for text, Download), text
// is text (a Markdown subset when it says it is Markdown), data reads by its shape (tables, a grid
// of names and values, the rest one click away) -- each with its "{}" toggle. A file's bytes are
// read, opened (text only, never HTML) and saved in the page; one the gate stores apart
// (`kontra-blob:<ref>`) is fetched through the console API. A part sealed end to end is a labelled
// envelope: its metadata shows, its ciphertext is never opened, decoded or offered.

import { gate } from "../app.js";
import { errorText, h, icon } from "../dom.js";
import { httpsHost, isTextual, piecesOf, piecesSize, previewOf, readPieces, saveBytes, sizeText } from "../files.js";
import { cellText, isMarkdown, markdownNodes, shapeOf } from "../results.js";
import { clamped, jsonBox, phone, rawToggle } from "./cards.js";

/** One file as one row: its icon, name, size and type; Open (text) and Download. `ctx`: { taskId,
 * signal }; `extra`: nodes at the row's end (its "{}"). */
export function fileRow(f, ctx, { under = null, receiving = false, extra = [] } = {}) {
  const name = f.name || under || "file";
  const pieces = piecesOf(f);
  const kept = pieces.filter((p) => p.ref).length;
  const size = piecesSize(pieces);
  const preview = h("div", { class: "stack", hidden: true });
  const err = h("p", { class: "ferr", role: "alert" });
  let bytes = null;
  const read = async () => {
    if (!bytes) bytes = await readPieces(gate, ctx.taskId, pieces, { signal: ctx.signal });
    return bytes;
  };
  const sz = [kept ? `kept by the gate${pieces.length > 1 ? ` · ${pieces.length} chunks` : ""}` : pieces.length ? sizeText(size) : "", f.type].filter(Boolean).join(" · ");
  const acts = [];
  if (pieces.length) {
    if (isTextual(f.type)) {
      const open = h("button", { class: "btn sm", type: "button", text: "Open", dataset: { act: "open" } });
      open.addEventListener("click", async () => {
        err.textContent = "";
        if (preview.childElementCount) {
          preview.replaceChildren();
          preview.hidden = true;
          open.textContent = "Open";
          return;
        }
        open.disabled = true;
        try {
          const { text, more } = previewOf(await read(), f.type);
          preview.replaceChildren(jsonBox(text), ...(more ? [h("p", { class: "small", text: "The first 1 MiB: download it for the rest." })] : []));
          preview.hidden = false;
          open.textContent = "Close";
        } catch (e) {
          err.textContent = errorText(e);
        } finally {
          open.disabled = false;
        }
      });
      acts.push(open);
    }
    const dl = h("button", { class: "btn sm", type: "button", text: "Download", dataset: { act: "download" } });
    dl.addEventListener("click", async () => {
      err.textContent = "";
      dl.disabled = true;
      try {
        saveBytes(await read(), name);
      } catch (e) {
        err.textContent = errorText(e);
      } finally {
        dl.disabled = false;
      }
    });
    acts.push(dl);
  } else if (f.href) {
    acts.push(h("a", { class: "btn sm", href: f.href, target: "_blank", rel: "noopener noreferrer", text: `Open on ${httpsHost(f.href) || "the agent's site"}` }));
  } else if (f.url) {
    acts.push(h("span", { class: "small", text: f.url }));
  }
  const fname = h("span", { class: "fn", text: name });
  fname.style.overflowWrap = "anywhere";
  fname.style.minWidth = "0";
  const spacer = h("span", {});
  spacer.style.flex = "1 1 auto";
  const top = h("div", { class: "top" }, [
    icon("download", 14),
    fname,
    sz ? h("span", { class: "sz", text: sz }) : null,
    receiving ? h("span", { class: "tag", text: "receiving" }) : null,
    spacer,
    ...acts,
    ...extra,
  ]);
  const art = h("div", { class: "art", dataset: { file: name } }, [top, preview, err]);
  art.style.padding = "9px 12px";
  art.style.gap = "8px";
  return art;
}

/** A sealed part (taskevents.js sealedOf) as a labelled envelope: "sealed", its original type,
 * name and size, and its metadata behind a "{}". Nothing of its content is read. */
export function sealedRow(env) {
  const raw = rawToggle("The sealed part's metadata", () => env.metadata);
  const what = [env.original ? `was ${env.original}` : null, env.size !== null ? sizeText(env.size) : null, env.type].filter(Boolean).join(" · ");
  const label = h("span", { class: "fn", text: env.name ? `Sealed · ${env.name}` : "Sealed" });
  label.style.overflowWrap = "anywhere";
  label.style.minWidth = "0";
  const spacer = h("span", {});
  spacer.style.flex = "1 1 auto";
  const top = h("div", { class: "top" }, [icon("lock", 14), label, what ? h("span", { class: "sz", text: what }) : null, spacer, raw.button]);
  const note = h("p", { class: "small", text: "Sealed end to end: only the parties' nodes can open it." });
  const art = h("div", { class: "art", dataset: { sealed: "1" } }, [top, note, raw.box]);
  art.style.padding = "9px 12px";
  art.style.gap = "6px";
  return art;
}

function table(t) {
  const head = h("thead", {}, [h("tr", {}, t.columns.map((c) => {
    const th = h("th", { text: c.replace(/_/g, " ") });
    // The head stays in view while the rows scroll under it (CSSOM: console.css has no sticky th).
    th.style.position = "sticky";
    th.style.top = "0";
    th.style.zIndex = "1";
    return th;
  }))]);
  const body = h("tbody", {}, t.rows.map((r) => h("tr", {}, r.map((v, i) => {
    const { text, full } = cellText(v);
    const td = h("td", { class: t.numeric[i] ? "m" : null, text });
    if (full) td.title = full;
    return td;
  }))));
  const more = t.total > t.rows.length || t.hidden
    ? h("p", { class: "more", text: [t.total > t.rows.length ? `showing ${t.rows.length} of ${t.total}` : null, t.hidden ? `${t.hidden} more column${t.hidden === 1 ? "" : "s"} in {}` : null].filter(Boolean).join(" · ") })
    : null;
  // At most about ten rows high: the rest scroll inside it.
  const wrap = h("div", { class: "tablewrap" }, [h("table", {}, [head, body])]);
  wrap.style.maxHeight = "372px";
  wrap.style.overflowY = "auto";
  wrap.style.scrollbarColor = "var(--line-2) transparent";
  return h("div", { class: "stack", dataset: { table: t.key || "" } }, [
    t.key ? h("span", { class: "small", text: t.key.replace(/_/g, " ") }) : null,
    wrap,
    more,
  ]);
}

/** A data part by its shape (results.js shapeOf): its tables, its numbers as tiles, its text as
 * names and values, the rest one click away. */
export function dataView(json) {
  let data;
  try {
    data = JSON.parse(json);
  } catch {
    return jsonBox(json);
  }
  const shape = shapeOf(data);
  const nodes = shape.tables.map(table);
  const tiles = shape.scalars.filter(([, v]) => typeof v === "number" || typeof v === "boolean");
  const words = shape.scalars.filter(([, v]) => !(typeof v === "number" || typeof v === "boolean"));
  if (tiles.length) {
    const stats = h("div", { class: "stats", dataset: { stats: "1" } }, tiles.map(([k, v]) => h("div", { class: "stat" }, [
      h("b", { text: k.replace(/_/g, " ") }),
      h("span", { text: typeof v === "number" ? v.toLocaleString("en-US") : v ? "yes" : "no" }),
    ])));
    // A figure or three keep a tile's width: one number is not a banner. Rows are
    // full: as many columns as divide the tiles evenly (at most 3 on a phone, 6 else), so no row
    // ends in an empty cell.
    if (tiles.length < 4) stats.style.maxWidth = `${tiles.length * 180}px`;
    const most = phone() ? 3 : 6;
    let cols = 0;
    for (let c = Math.min(most, tiles.length); c > 1 && !cols; c -= 1) if (tiles.length % c === 0) cols = c;
    if (cols) stats.style.gridTemplateColumns = `repeat(${cols}, minmax(0, 1fr))`;
    nodes.unshift(stats);
  }
  if (words.length) {
    const kv = h("div", { class: "terms2 kv" }, words.map(([k, v]) => {
      const { text, full } = cellText(v);
      const span = h("span", { text: full ? full.slice(0, 300) : text });
      span.style.overflowWrap = "anywhere";
      span.style.textAlign = "right";
      return h("div", { class: "tr" }, [h("b", { text: k.replace(/_/g, " ") }), span]);
    }));
    nodes.splice(tiles.length ? 1 : 0, 0, kv);
  }
  if (shape.rest) {
    const keys = Object.keys(shape.rest);
    const { button, box } = rawToggle(`The rest: ${keys.join(", ")}`, () => shape.rest);
    button.textContent = `+ ${keys.slice(0, 4).join(", ")}${keys.length > 4 ? "…" : ""}`;
    button.style.fontFamily = "inherit";
    button.style.paddingLeft = "0";
    button.style.alignSelf = "flex-start";
    nodes.push(button, box);
  }
  if (!nodes.length) nodes.push(jsonBox(json));
  const view = h("div", { class: "stack" }, nodes);
  view.style.gap = "10px";
  return view;
}

/** A text part: Markdown when its type or its result's name says so, else plain text; clamped to
 * about twelve lines. */
export function textView(part, name) {
  const nodes = isMarkdown(part.type, name) ? markdownNodes(part.text) : [part.text];
  return clamped(nodes, 12, { cls: "plain" });
}
