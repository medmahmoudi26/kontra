# Console chat: conversations, tasks and artifacts

**Draft, 19 Nov 2026. For the owner's review. Nothing here is built.**

This note specifies a change to the console in [`web/`](../web/README.md): a chat that lists every conversation,
agent-to-agent ones included, a side panel for a conversation's tasks and artifacts, and a new set of sections.
The prototype is [`prototypes/console-chat.html`](prototypes/console-chat.html): one file, no build step, no network.
Open it in a browser (`?theme=light` for the light theme). Every agent, task, artifact, node and MCP server in it is
mock data, and every reply is a scripted stream.

Where this note and the prototype disagree, this note wins. The API mapping below was read from
`web/static/js/routes.js` and `api.js` at `d864f2d`; nothing in `internal/console` was changed or checked beyond that.

## 1. What changes

| Today (`web/static/js/nav.js`) | Proposed |
|---|---|
| Contexts (landing page) | **Chat**: the contexts become the conversation list, in a rail beside the transcript |
| Tasks | **Tasks**: every task, with the task operations of §4 |
| (none) | **Artifacts**: every artifact, with a viewer |
| Cards | **Agents** (A2A agent cards) and **MCP** (MCP servers), two sections |
| (none) | **Nodes**: the nodes connected to the gate and what each hosts |
| (none) | **Catalog**: Kontra Cloud only; hidden when `/v1/info` does not offer it |
| Traffic, MCP calls, Playground | Not decided (§9.2). The prototype links to Traffic from a read-only conversation |

Section order: Chat, Nodes, Agents, MCP, Catalog, Tasks, Artifacts.

## 2. The owner's decisions of 19 Nov

These are the decisions the prototype was drawn from. They get their numbered entries in
[`decisions.md`](decisions.md) with the change that implements them.

1. Conversations between agents are listed in the chat's navigation. Clicking one shows its transcript.
2. The chat does not say "caller" or "callee". It reads as a chat: the side that opened the conversation is on the
   right, the agent that answers is on the left. This narrows D360 for the chat transcript only; A2A's names stay in
   the raw view (§3.3), in Traffic and in the Playground.
3. A task and a plain message look different.
4. A2UI surfaces and artifacts are drawn in the transcript.
5. JSON and typed input are drawn as a tree that collapses and expands, not as text.
6. Markdown in an artifact is rendered without being asked.
7. The chat has Tasks and Artifacts tabs. They open a side panel with the conversation's tasks and artifacts, and the
   task operations (get status, subscribe) run from there.
8. "Tasks & usage" becomes two sections, Tasks and Artifacts, listing all tasks and all artifacts.
9. "Network" becomes Nodes. Agents (agent cards, as the local gate's Cards page shows them) and MCP (MCP servers)
   are added.

## 3. Chat

### 3.1 The conversation list

One row per A2A context, newest activity first, from `GET /v1/contexts`.

- A row shows who opened the conversation and which agent answers (`blue-team/triage → redteam/recon`), the last
  line, the state of its latest task, and when it was last active. A context without a task shows no state.
- A filter: All, Yours, Agents. "Yours" is a context whose caller is the console's own principal. "Agents" is every
  other one.
- A sealed context says so in the row.
- "New chat" opens an empty conversation (§3.6).

### 3.2 The header

The two parties, the state of the latest task, the `contextId`, and two buttons, "Tasks n" and "Artifacts n", which
open the side panel (§4).

### 3.3 The transcript

Built from `GET /v1/contexts/{service}/{caller}/{context}/timeline`, then live from each open task's
`GET /v1/tasks/{task_id}/events`.

| A2A item | Drawn as |
|---|---|
| `Message` from the side that opened the conversation | Bubbles on the right, one per part |
| `Message` from the agent, with no task | A bubble on the left, tagged "Message" |
| `Task` | A card on the left: task id, state, the status messages as a list, the artifacts, the A2UI surface. It collapses |
| `TaskStatusUpdateEvent` | A new line in its task's list, and the card's state |
| `TaskArtifactUpdateEvent` | The artifact inside its task's card; an `append` grows it in place |
| Text part | Text |
| Data part | A JSON tree (§6.1) |
| File part | A chip with the file's name and media type |
| A2UI data part (`application/a2ui+json`) | The surface, drawn by `js/a2ui.js` as today; a "JSON" button shows its messages as a tree |

- **Raw.** Every message and every task has a "Raw" button that shows the A2A object as a tree. This is where A2A's
  own names are read.
- **Streaming.** Text grows in place with a caret. A working task's state pulses. An artifact says "streaming" until
  its last chunk.
- **AP2 parts** keep today's cards (`views/ap2.js`). The prototype has none.

### 3.4 The composer

Shown only in the console's own conversations.

- Two formats, Text and JSON. JSON is a monospace box that starts from an example of the agent's card and says, as
  the person types, whether it is valid JSON.
- Text that parses as a JSON object or array is sent as a data part too.
- Enter sends, Shift+Enter is a new line. In JSON, Ctrl+Enter or Cmd+Enter sends. Sending is off while a reply streams.
- It maps to today's routes: `POST /v1/tasks` (`text` or `data`) for a first message,
  `POST /v1/tasks/{task_id}/messages` for a task that waits for input (text, or an A2UI action), and
  `POST /v1/tasks/{task_id}/continue` for a message in an ended task's context.

### 3.5 Conversations between agents

Read-only. No composer, a line saying the person is watching, and A2UI surfaces drawn but disabled. A link opens
Traffic on that context.

A sealed conversation shows what the gate has: who sent what, when, and how many parts of which kind. No content and
no Raw. See §9.1.

### 3.6 A new chat

The person picks an agent, reads its card's description, and gets the examples of its skills (`skills[].examples`)
as buttons. This is the plain first message of 7 Nov (D350), unchanged: an example that parses as JSON is offered as
a typed request, and no form is drawn before the agent asks for one.

## 4. The side panel

Opened from the header. Two tabs. On a narrow screen it covers the chat and has a close button. It replaces the
Artifacts column and the "Transcript | Artifacts" switch of today's thread.

**Tasks.** One card per task of the conversation: id, state, the last status message, and three operations.

| Operation | Call | Shows |
|---|---|---|
| Get status | `GET /v1/tasks/{task_id}` | The answer as a JSON tree, with the time it was read |
| Subscribe | `GET /v1/tasks/{task_id}/events` (Server-Sent Events) | A log of events as they arrive: kind and a one-line summary. It closes when the task ends, or when the person unsubscribes |
| Cancel | `POST /v1/tasks/{task_id}/cancel` | The new state. Offered only when the task's `actions` include `cancel` |

The prototype refuses a subscription to an ended task with A2A's `UnsupportedOperationError`. Whether these are the
console API's reads or real A2A calls is open (§9.3).

**Artifacts.** The conversation's artifacts as a list, and the selected one below it, rendered by §6. Each artifact
in the transcript has a "Panel" button that opens it there.

## 5. The other sections

- **Tasks.** `GET /v1/tasks`. A filter (All, Active, Ended), the same task card as the panel, and "Open chat", which
  goes to the conversation with the panel open.
- **Artifacts.** A list beside a viewer. No route lists artifacts across tasks today: the console folds them from each
  task's events. This needs one, for example `GET /v1/artifacts` returning the task, context, service, name, media
  types and whether the artifact is complete (§9.6).
- **Agents.** One card per A2A service, from `GET /v1/services` and `GET /v1/services/{name}/cards`: name, version,
  description, node, provider, streaming, push notifications, extensions, the URL on the gate (`/agents/{name}`), and
  each skill with its tags and examples. "Chat" starts a conversation with it. "Card JSON" shows the served card as a
  tree. The comparison of the callee's card with the served card stays reachable from the card.
- **MCP.** One card per MCP service: the node, the kind of backend, the URL on the gate (`/mcp/{name}`), and the
  tools. A tool the gate denies (`services.<name>.tools.deny`) says so. "tools/list JSON" shows the list with each
  input schema. Where the tool list comes from is open (§9.5).
- **Nodes.** One card per connected node, from `GET /v1/tunnels`: its name, since when it is connected, and the
  services it hosts, A2A and MCP.
- **Catalog.** Kontra Cloud's. Out of this note.

## 6. Rendering rules

Both renderers build nodes and set text. Neither parses a string as HTML: `web/test/static.test.mjs` keeps passing.

### 6.1 JSON tree

- An object or an array is a row that opens and closes. Closed, it shows `{…} 3 keys` or `[…] 5 items`.
- The first two levels are open by default.
- "Expand all" and "Collapse all" on every tree. Collapse all keeps the root open.
- Strings are green, numbers blue, `true`, `false` and `null` amber. Colour is never the only cue: the JSON syntax is
  drawn as written.
- A string longer than 220 characters is cut, with an ellipsis.
- Used for: data parts, JSON artifacts, A2UI messages, the Raw view, Get status, agent cards, `tools/list`.

### 6.2 Markdown

- Applies to a text part whose media type is `text/markdown`.
- Rendered: headings (three levels), paragraphs, bulleted and numbered lists, bold, inline code, fenced code.
- A "Source" button shows the text as sent.
- Rendering runs on every chunk, so an artifact reads as markdown while it streams.
- Not rendered in the prototype: links, tables, italics, images. They show as text. An agent's markdown is not
  trusted: a link must never navigate without the person seeing its target, and no image is fetched from another
  origin (the console's `img-src` is `'self'`).

## 7. The prototype

What it shows: the conversation list and its filter; a task waiting on an A2UI form, answered, then streaming to its
end; a plain message beside a task; text, JSON and file parts; the Raw view; the JSON composer; a conversation between
two agents streaming two tasks, with a replay; a sealed conversation; the side panel with the three task operations;
the Tasks, Artifacts, Agents, MCP and Nodes sections; markdown rendered while it streams.

What it is not: the script at the bottom of the file has a small template runtime and the mock scripts. Neither is
to be ported. The console's renderer is `web/static/js`, plain ES modules with no build step, and the colours and
components are those of `web/static/console.css`, copied into the file.

The raw A2A objects in it use A2A 1.0's field names but were written by hand, not validated.

## 8. Suggested slices

1. The sections and the Chat shell: the rail from `/v1/contexts`, the transcript from the timeline, no new rendering.
2. The transcript's rules of §3.3: bubbles by side, the task card, no caller or callee labels.
3. The JSON tree, then every place of §6.1.
4. Markdown in artifacts.
5. The side panel and the task operations.
6. The Tasks and Artifacts sections, with the artifacts route.
7. Agents, MCP and Nodes.

## 9. Open questions

1. **Sealed conversations.** The gate stores ciphertext and the browser holds no node key. Either the console shows
   metadata only (as the prototype does), or the node keeps its own record and the console reads it through the tunnel.
2. **Traffic, MCP calls and Playground.** They are not in the new section list. Keep them as sections, or reach them
   from Chat, MCP and Agents?
3. **Task operations.** The console API reads the gate's record, which works for any conversation the person may see.
   Real A2A calls (`GetTask`, `SubscribeToTask`, `CancelTask` on the service's own route, as the Playground sends
   them) show up in Traffic, but a task is scoped to its caller, so they only work in the console's own conversations.
4. **Who sees which conversation.** On a team gate, every context. On Kontra Cloud, only the contexts where one of the
   person's agents is a party.
5. **MCP tools.** Whether `/v1/services` can return a server's tool list, and from which read of `tools/list`.
6. **The artifacts route.** Its shape, and whether an artifact's parts come with it or from the task.
7. **D360.** Confirm that it is narrowed for the chat transcript only (§2.2).
