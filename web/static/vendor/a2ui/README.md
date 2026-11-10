# The A2UI web renderer — `a2ui-0.15.0.js`

The thread draws an agent's [A2UI](https://a2ui.org) surfaces (D-M5-179) with the official web
renderer: `@a2ui/web_core`'s `MessageProcessor`, data binding, basic-catalog APIs and functions, and
the Lit element its components extend, and `@a2ui/lit`'s `<a2ui-surface>`. The console has no build
step (PRD §13) and loads nothing from a CDN (`script-src 'self'`), and these packages are ES modules
with bare imports (`lit`, `zod`, `@preact/signals-core`, …) that a browser resolves only through an
import map (an inline script, which the CSP forbids) or a bundler: so they are bundled here once, into
one ES module that `js/a2ui.js` imports, and served from `'self'` like every other byte of the console.
Nothing in it is changed.

| | |
|---|---|
| What | `@a2ui/web_core` 0.15.0 and `@a2ui/lit` 0.15.0 (Google LLC, Apache-2.0), with lit-html 3.3.3, lit-element 4.2.2, @lit/reactive-element 2.1.2, zod 3.25.76, zod-to-json-schema 3.25.2, @preact/signals-core 1.14.5 and redos-detector 6.1.4 |
| Upstream | https://github.com/a2ui-project/a2ui (`typescript/core/web_core`, `typescript/renderers/lit`); npm `@a2ui/web_core@0.15.0` (`sha512-S3k9rAJ9O55z5TlDeWy22ly3yQnDLO+XBWihlL5JQSuyxaCUmd7jgFnxO4eq/Bn6dSgdBF0WG8cve/euMoz8SA==`), `@a2ui/lit@0.15.0` (`sha512-JKLdpj9zOzJepJlx2rD2fuHeWNZ7PevWJ+0ENNXzZcI/KT9SsKXNDt3Z8N/89XqepHdfaug/yDIOcaJnNJl7og==`) |
| Build | `scripts/vendor-a2ui/build`: `npm ci --ignore-scripts` of `scripts/vendor-a2ui/package-lock.json` (every package's version and npm integrity hash pinned), then `esbuild entry.js --bundle --format=esm --platform=browser --target=es2022 --legal-comments=eof --charset=utf8` (esbuild 0.28.2, also locked). `scripts/vendor-a2ui/build --check` rebuilds it and compares byte for byte; `make vendor` runs it, in `make check` and CI's `vendor` job (D-M5-181) |
| File | `a2ui-0.15.0.js`, 925,836 bytes, not minified |
| sha256 | `69d5eb0cd8eee6604f5fa6f5712e831d27fdc356b4b5cf8dbe4bd818214a6ba1` |
| Licences | Apache-2.0 (`LICENSE`); `NOTICE`; the dependencies' BSD-3-Clause, MIT, ISC, BSD-2-Clause and 0BSD texts in `THIRD-PARTY-LICENSES`; the upstream `@license` headers at the end of the file |
| Built | 2026-11-02 |

**What it holds, and what it does not.** `scripts/vendor-a2ui/entry.js` imports each module it needs
from its own file, not from a package index: `@a2ui/web_core`'s and `@a2ui/lit`'s indexes also build
the basic catalog from web_core's own elements, which the console never registers -- their markdown
directive is Lit's `unsafeHTML`, their Text binds a `style` attribute and their styles are adopted
stylesheets -- and a bundler keeps what an index's top-level calls reach. So the bundle holds no
`unsafeHTML`, `unsafeSVG` or markdown renderer, no basic-catalog element, and no `fetch`, `eval` or
`new Function`; its only `window.open` is web_core's `openUrl`, which the console's catalog replaces
(`js/a2ui.js`: http(s) only, on a person's click). `tests/test_console_js.py` holds all of this, the
pin and the CSP to it.

**Trusted Types.** Lit creates one policy, `lit-html` (`createHTML: s => s`), which it uses only for
its templates' source text: an `html` template's strings, which Lit refuses unless they are a real
template literal's (`raw`), never a value. With `trusted-types 'none'` that creation throws and the
module does not load, so the console's CSP names that one policy (`trusted-types lit-html`; no
`'allow-duplicates'`, no other name: D-M5-180, C63). The thread imports `js/a2ui.js` only when it
shows A2UI, so a browser that refused it would leave the rest of the console as it is.

**Upgrading.** Change the versions in `scripts/vendor-a2ui/package.json`, `npm install
--package-lock-only` there, run `scripts/vendor-a2ui/build`, rename the file (the version is in its
name) and `js/a2ui.js`'s import, and update this table, `NOTICE`, `THIRD-PARTY-LICENSES` (what the
bundle now holds: esbuild's `--metafile` says) and `tests/test_console_js.py`'s pin. Read the
renderer's changes against the console's catalog: an element's base class, the controller's props and
the processor's validation are what `js/a2ui.js` relies on.
