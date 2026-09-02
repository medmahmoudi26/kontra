/**
 * Markdown, rendered under the assumption that it came from a scanner (ADR 0020, slice 7b).
 *
 * This is the ONE renderer for every markdown surface on the Dashboard — a pane's widget, the detail
 * drawer, the topology diagram, the run summaries. One renderer on purpose: three of those four
 * sources are ours and one is attacker-influenced, and a component with a "trusted" mode is a
 * component someone eventually renders untrusted content through.
 *
 * WHAT A PANE CAN CONTAIN. A crawled `<title>`, a response header, a scan finding, a URL — all
 * chosen by whoever is being scanned. So:
 *
 *  - **No raw HTML, and no `rehype-raw`.** react-markdown 9's default turns an HTML node into a TEXT
 *    node, so `<img src=x onerror=alert(1)>` in pane text renders as the characters `<img …>` and
 *    never as an element. Note what is NOT set: `skipHtml`. It would DELETE the HTML instead, and
 *    deleting it hides what a scanner actually saw — the operator should read the payload, in the
 *    same way `HealthChips` shows a failing signal's sentence rather than a red dot.
 *  - **No remote images, ever.** A remote `![](https://target/1x1.gif)` is a pixel request to a host
 *    of the target's choosing: a beacon that confirms an operator looked at the output, with the
 *    Controller's egress IP attached. {@link safeUrl} drops the URL and {@link BLOCKED_IMAGE_TESTID}
 *    renders the alt text in its place, so the document still says an image was there.
 *  - **Links are rendered, never followed.** No `target`, `rel="noopener noreferrer"` always, and
 *    only `http`/`https`/`mailto` survive — `javascript:` and `data:` hrefs are dropped, and so are
 *    relative ones, which would otherwise point at our own API.
 *  - **The text is capped** ({@link MARKDOWN_TEXT_CAP}) with a visible notice. A pane that emits a
 *    megabyte of markdown must not become the page's memory profile.
 *  - **Mermaid is a separate, lazily-loaded component** with its own caps — see `MermaidBlock`.
 *
 * `widgets/untrusted.test.ts` greps this directory for `rehype-raw`, `dangerouslySetInnerHTML`,
 * `innerHTML` and `fetch(`, so the controls above cannot be undone by an import.
 */

import ReactMarkdown, { type Components } from 'react-markdown';
import remarkGfm from 'remark-gfm';
import MermaidBlock from './MermaidBlock';

/**
 * The most markdown this renders, in UTF-16 code units.
 *
 * Deliberately the same number as `parseWidget`'s cap and enforced independently of it: the widget
 * path refuses over-cap panes before this component is reached, and this cap is what protects the
 * three surfaces that do not go through `parseWidget` (a run with 4000 nodes, say). Truncation here
 * keeps a NOTICE, because a silently shortened document is worse than a bounded one.
 */
export const MARKDOWN_TEXT_CAP = 64 * 1024;

/** The most a `data:` image may weigh. Small: a legitimate inline image on a terminal wall is a
 * sparkline or a QR code, and anything larger belongs in the lake. */
export const DATA_IMAGE_CAP = 32 * 1024;

/** `data:` images that are allowed at all: raster only. SVG is excluded BY NAME — an SVG document is
 * markup with its own script and its own external references, which is the whole thing this file is
 * built to keep out. */
const DATA_IMAGE = /^data:image\/(?:png|jpeg|gif|webp);base64,[A-Za-z0-9+/=]+$/;

/** URL schemes a link may use. `mailto:` because a summary naming an on-call address is useful; no
 * `irc:`/`xmpp:` (react-markdown's own default allows them) because nothing here produces them and
 * every allowed scheme is a handler on the operator's machine. */
const LINK_SCHEMES = new Set(['http:', 'https:', 'mailto:']);

export const BLOCKED_IMAGE_TESTID = 'blocked-image';

/**
 * Admit a URL, or drop it.
 *
 * `key` is the attribute react-markdown is about to set (`href`, `src`, …). The two are treated
 * differently for one reason: a `src` is fetched by the browser WITHOUT a click, and an `href` is
 * not. So `src` is a beacon and `href` is a choice.
 *
 * Deny by default: an attribute this does not recognise gets no URL at all.
 */
export function safeUrl(url: string, key: string): string | undefined {
  const raw = url.trim();
  if (raw === '') return undefined;
  // Control characters in a URL are how an attacker gets past a naive scheme test
  // (`java\nscript:`); markdown decodes entities before we see this, so the check has to be on the
  // final string.
  for (let i = 0; i < raw.length; i += 1) {
    if (raw.charCodeAt(i) < 0x20 || raw.charCodeAt(i) === 0x7f) return undefined;
  }

  if (key === 'src' || key === 'srcset' || key === 'poster' || key === 'background') {
    // No network. A `data:` raster under the cap is the only image this page will ever show, because
    // it carries no request.
    if (raw.length > DATA_IMAGE_CAP) return undefined;
    return DATA_IMAGE.test(raw) ? raw : undefined;
  }

  if (key !== 'href' && key !== 'cite') return undefined;

  const scheme = /^([A-Za-z][A-Za-z0-9+.-]*):/.exec(raw)?.[1]?.toLowerCase();
  // No scheme means relative, which resolves against the SPA's own origin — a link into our API
  // rather than out to the web. Dropped: nothing that renders through here has a reason to emit one.
  if (scheme === undefined) return undefined;
  return LINK_SCHEMES.has(`${scheme}:`) ? raw : undefined;
}

/** The minimal shape of the hast node react-markdown hands a component. Declared structurally rather
 * than imported from `hast` so {@link mermaidChartOf} is a pure function a node-environment test can
 * call with a literal. */
export interface HastLike {
  type?: string;
  tagName?: string;
  properties?: Record<string, unknown> | undefined;
  children?: readonly HastLike[] | undefined;
  value?: string | undefined;
}

/**
 * The chart source of a ```mermaid fence, read off the `pre` node — or `null` for any other block.
 *
 * Taken from the hast node rather than from React children because the children are already
 * elements: reconstructing a string from them means walking rendered output, and a fence's text is
 * right here, exactly as the author wrote it.
 */
export function mermaidChartOf(node: HastLike | undefined): string | null {
  const code = node?.children?.find((c) => c.tagName === 'code');
  if (!code) return null;
  const classes = code.properties?.['className'];
  const names = Array.isArray(classes) ? classes.map(String) : typeof classes === 'string' ? [classes] : [];
  if (!names.includes('language-mermaid')) return null;
  const text = (code.children ?? [])
    .filter((c) => c.type === 'text')
    .map((c) => c.value ?? '')
    .join('');
  const chart = text.replace(/\s+$/, '');
  return chart === '' ? null : chart;
}

/** Truncate at a line boundary, so a cut document is not a cut construct. */
export function capMarkdown(source: string, cap = MARKDOWN_TEXT_CAP): { body: string; dropped: number } {
  if (source.length <= cap) return { body: source, dropped: 0 };
  const head = source.slice(0, cap);
  const lastBreak = head.lastIndexOf('\n');
  const body = lastBreak > cap / 2 ? head.slice(0, lastBreak) : head;
  return { body, dropped: source.length - body.length };
}

const COMPONENTS: Components = {
  a({ href, children }) {
    // `href` has already been through `safeUrl`; it is `undefined` when the URL was refused, and an
    // anchor with no href cannot navigate anywhere. NO `target`, and `rel` on every anchor — the
    // second is meaningless without the first, which is the point: nothing here opens a window.
    if (typeof href !== 'string' || href === '') {
      return <span data-testid="blocked-link">{children}</span>;
    }
    return (
      <a href={href} rel="noopener noreferrer" title={href} className="underline underline-offset-2">
        {children}
      </a>
    );
  },

  img({ src, alt }) {
    // Reached with `src === undefined` for every remote image, because `safeUrl` refused it. What is
    // rendered instead is the alt text and a sentence — never the URL, which would put an attacker's
    // string in front of an operator as if it were a place to go.
    if (typeof src !== 'string' || src === '') {
      return (
        <span
          data-testid={BLOCKED_IMAGE_TESTID}
          className="rounded border border-dashed border-zinc-500 px-1 text-[11px] text-zinc-400"
        >
          {alt ? `${alt} — ` : ''}remote image blocked; nothing was requested
        </span>
      );
    }
    return <img src={src} alt={alt ?? ''} className="max-h-64 max-w-full" data-testid="widget-image" />;
  },

  pre({ node, children }) {
    const chart = mermaidChartOf(node as HastLike | undefined);
    if (chart !== null) return <MermaidBlock chart={chart} className="my-2" />;
    return (
      <pre className="my-2 overflow-x-auto rounded border border-zinc-700/60 bg-black/40 p-2 text-[11px] leading-snug">
        {children}
      </pre>
    );
  },

  code({ className, children }) {
    // Inside a `pre` this is the fence's body (and carries `language-*`); on its own it is inline
    // code. v9 dropped the `inline` prop, and the distinction does not matter for styling here.
    return <code className={className ?? 'rounded bg-black/40 px-1 text-[11px]'}>{children}</code>;
  },

  table({ children }) {
    return (
      <div className="my-2 overflow-x-auto">
        <table className="w-full border-collapse text-[11px]">{children}</table>
      </div>
    );
  },
  th({ children }) {
    return <th className="border border-zinc-700/60 px-1.5 py-0.5 text-left font-medium">{children}</th>;
  },
  td({ children }) {
    return <td className="border border-zinc-700/60 px-1.5 py-0.5 align-top">{children}</td>;
  },

  h1({ children }) {
    return <h1 className="mt-2 mb-1 text-sm font-semibold">{children}</h1>;
  },
  h2({ children }) {
    return <h2 className="mt-2 mb-1 text-[13px] font-semibold">{children}</h2>;
  },
  h3({ children }) {
    return <h3 className="mt-2 mb-1 text-xs font-semibold uppercase tracking-wide">{children}</h3>;
  },
  p({ children }) {
    return <p className="my-1 text-xs leading-relaxed">{children}</p>;
  },
  ul({ children }) {
    return <ul className="my-1 list-disc pl-4 text-xs">{children}</ul>;
  },
  ol({ children }) {
    return <ol className="my-1 list-decimal pl-4 text-xs">{children}</ol>;
  },
  li({ children }) {
    return <li className="my-0.5">{children}</li>;
  },
  blockquote({ children }) {
    return <blockquote className="my-1 border-l-2 border-zinc-600 pl-2 text-xs italic">{children}</blockquote>;
  },
  hr() {
    return <hr className="my-2 border-zinc-700/60" />;
  },
};

export interface MarkdownProps {
  source: string;
  className?: string;
  /** Overridden by the widget path so a tile's markdown carries the tile's test hook. */
  testId?: string;
}

export default function Markdown({ source, className, testId = 'markdown' }: MarkdownProps): JSX.Element {
  const { body, dropped } = capMarkdown(source);
  return (
    <div className={className} data-testid={testId}>
      <ReactMarkdown
        remarkPlugins={[remarkGfm]}
        // NOTHING HERE. A `rehypePlugins={[rehypeRaw]}` is the one line that would turn every string
        // above into markup, which is why the list is written out empty rather than omitted.
        rehypePlugins={[]}
        urlTransform={(url, key) => safeUrl(url, key)}
        components={COMPONENTS}
      >
        {body}
      </ReactMarkdown>

      {dropped > 0 && (
        <p data-testid="markdown-truncated" className="mt-1 text-[11px] text-amber-300">
          {`${dropped} more characters were not rendered — this document is past the ${MARKDOWN_TEXT_CAP}-character cap.`}
        </p>
      )}
    </div>
  );
}
