# web: the console's protocol renderer

The browser console a gate serves in the local development profile (`profile: local`): the task list,
conversations, a task's thread built live from its event log, A2UI surfaces, AP2 parts, A2A traffic and MCP
calls. It is plain ES modules with no build step, embedded in `kontra-gate` by the `web` Go package
(`web.go`), and loads nothing from another origin.

The console talks to the gate's console API (`/v1/*`, `internal/console`), whose shapes follow Kontra Cloud's
control plane so the same files can serve both: a task's events are the gate's event-log rows (`seq`, `kind`,
`payload`) as Server-Sent Events. `GET /v1/info` says what the backend offers; the console hides what it lacks.

## Provenance

Lifted from Kontra Cloud's console, `kontra-cloud` `gate/static` at commit **`8a54a12`** (dev, equal in tree to
main `4ba9bdd`), then stripped of what is Cloud's own: quotes, escrow, Checkout, disputes and review, passkeys,
device and job keys, `age` encryption, billing and sign-in, and the private decision citations. Kontra Cloud
will later serve these files from this repository at a pinned commit, so a fix lands once.

| Here | From `gate/static` at `8a54a12` |
|---|---|
| `static/index.html`, `static/console.css`, `static/favicon.svg` | same paths |
| `static/js/{a2ui,a2uiwire,agentform,agentstack,api,app,dom,files,formfields,main,nav,results,routes,sse,taskevents}.js` | `js/` |
| `static/js/views/{cards,common,outputs,request,tasks,thread}.js` | `js/views/` |
| `static/vendor/a2ui/*` (A2UI web renderer 0.15.0: Apache-2.0, with its NOTICE and third-party licenses) | `vendor/a2ui/` |

Not taken: `inputschema.js` (the input-schema extension, withdrawn by D350), `prices.js` (money), and every
module of sign-in, keys, billing, disputes and the catalog. `js/agentschema.js` keeps the half of `inputschema.js`
that draws and answers an agent's own form on the thread (the A2A samples' form part, Structured Input's
`input_schema`); a first message is text, or the card's AgentStack initial form. `api.js`, `app.js`, `main.js`,
`nav.js` and `routes.js` were rewritten for the gate's console API, and `views/{tasks,conversations,traffic,calls,
services}.js` are this repository's own list views. `js/ap2.js` (AP2 v0.2.0 data parts, classified by the keys
`examples/sellers/ap2-merchant` emits and decoded for display, never verified) and `views/ap2.js` (their cards) are
new here too.

## Tests

`node --test web/test/*.test.mjs` (node:test, no dependencies): the module graph is closed, the console's code
never parses a string as HTML and calls only the operations of `routes.js`, and the DOM-free modules behave as
Kontra Cloud's console tests say (`sse`, `taskevents`, `files`, `a2uiwire`, `agentschema`, `agentstack`, and `ap2`).
`make check-web` runs them. In a browser, against a gate in the local development profile: `make test-browser`
(test/browser, Playwright).
