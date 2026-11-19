# Console chat: conversations, tasks and artifacts

**Draft, 19 Nov 2026; the open questions settled on 20 Nov (§9). Nothing here is built.**

This note specifies a change to the console in [`web/`](../web/README.md): a chat that lists every conversation,
agent-to-agent ones included, a side panel for a conversation's tasks and artifacts, and a new set of sections.
The prototype is [`prototypes/console-chat.html`](prototypes/console-chat.html): one file, no build step, no network.
Open it in a browser (`?theme=light` for the light theme). Every agent, task, artifact, node and MCP server in it is
mock data, and every reply is a scripted stream.

Where this note and the prototype disagree, this note wins. The API mapping below was read from
`web/static/js/routes.js` and `api.js` at `d864f2d`; §9's answers were checked against `internal/console`,
`internal/mcprelay` and [`relay.md`](../spec/relay.md) §11.3, §13.6 and §18 at the same commit, and against Kontra
Cloud's task routes (`gate/tasks/thread.py` at `8f6dc2a`).

The console runs in two places, and the answers differ between them:

- **The local gate** (`profile: local`, D352): the gate's own console on loopback. Every client is the one
  `anonymous` caller, so it sees every conversation and every caller shows as `anonymous`. Sealed services are refused
  at load, so no conversation is sealed.
- **Kontra Cloud**: the same `web/` files over Cloud's control plane, with sign-in, parties and sealing.

A team gate (any other profile) serves no console.

## 1. What changes

| Today (`web/static/js/nav.js`) | Proposed |
|---|---|
| Contexts (landing page) | **Chat**: the contexts become the conversation list, in a rail beside the transcript |
| Tasks | **Tasks**: every task, with the task operations of §4 |
| (none) | **Artifacts**: every artifact, with a viewer |
| Cards | **Agents** (A2A agent cards) and **MCP** (MCP servers), two sections |
| (none) | **Nodes**: the nodes connected to the gate and what each hosts |
| (none) | **Catalog**: Kontra Cloud only; hidden when `/v1/info` does not offer it |
| Traffic, MCP calls | **Traffic**: the A2A exchanges and the MCP tool calls in one live list (§5) |
| Playground | Reached from each agent's card in Agents ("Playground"), no longer a section (§5) |

Section order: Chat, Nodes, Agents, MCP, Catalog, Tasks, Artifacts, Traffic.

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

### 2.1 The owner's decisions of 20 Nov

10. D360 is narrowed for the chat transcript only (item 2, confirmed). Everywhere else the console keeps A2A's names.
11. Traffic stays a section: the A2A and MCP traffic through the gate, in real time (§5).

The rest of §9 was settled from the code and the decisions already made (D352, D353, D360, `ext-sealed.md`, Cloud's task
routes), under the owner's standing instruction.

## 3. Chat

### 3.1 The conversation list

One row per A2A context, newest activity first, from `GET /v1/contexts`.

- A row shows who opened the conversation and which agent answers, the last line, the state of its latest task, and
  when it was last active. A context without a task shows no state.
- Who opened it is the principal the gate recorded, by name, never a role word. On the local gate that is
  `anonymous` for every row, including the console's own and an agent's call through its node
  ([`relay.md`](../spec/relay.md) §18.1). On Kontra Cloud it is the party and, for a node's call, the node
  (`acme · node k8s`). The gate records which principal called, not which agent behind a node did.
- A filter: All, Console, Agents. "Console" is a context with a task the console started: `via: console` on the local
  gate (`console_tasks`) and the tasks started from Cloud's console. "Agents" is every other one. The caller cannot
  tell them apart on the local gate, where every caller is `anonymous`. `GET /v1/contexts` gains a `console` boolean per
  row for this.
- A sealed context says so in the row, and so does a context the person sees without its content (§3.5).
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

Read-only. No composer, a line saying the person is watching, and A2UI surfaces drawn but disabled. "Open in Traffic"
opens Traffic filtered on that context.

**A conversation without content.** The console API says per task whether its content can be read (`content`, as
today's task view does). It cannot in two cases:

- **Sealed** (Kontra Cloud only). The gate relays ciphertext that only the two nodes can open, so Cloud gives the
  console each event's kind and time, never a payload (`thread.py`: "every reader of a sealed task").
- **The callee's owner on Kontra Cloud.** The owner of the called service reads a task's metadata, never its
  content, sealed or not.

Such a conversation is drawn from what the console has: the parties, each event as its A2A type and time
(`Message`, `TaskStatusUpdateEvent`, `TaskArtifactUpdateEvent`), the task cards with their ids and states, and a line
that says why there is no content. No bubble text, no Raw, no artifact viewer, and nothing can be answered (§9.1).

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
| Get status | `GET /v1/tasks/{task_id}`: a read of the gate's record | The task's A2A `Task` (its snapshot) as a JSON tree, labelled `Task` with the time it was read |
| Subscribe | `GET /v1/tasks/{task_id}/events` (Server-Sent Events): a read of the gate's record | A log of the events as they arrive: their A2A type and a one-line summary. On an ended task the stream replays nothing new, sends `end` and closes, and the log says so. It closes when the task ends, or when the person unsubscribes |
| Cancel | `POST /v1/tasks/{task_id}/cancel`, which sends a real A2A `CancelTask` to the service's route, as the caller | The new state. Offered only when the task's `actions` include `cancel`: the console's own task, not sealed, not ended |

Get status and Subscribe read the gate's record, so they work on every conversation the person may see, and they do
not appear in Traffic. Cancel is the one real A2A call and does appear there. A real `GetTask` or `SubscribeToTask` is
the Playground's (§9.3).

**Artifacts.** The conversation's artifacts as a list, and the selected one below it, rendered by §6. Each artifact
in the transcript has a "Panel" button that opens it there.

## 5. The other sections

- **Tasks.** `GET /v1/tasks`. A filter (All, Active, Ended), the same task card as the panel, and "Open chat", which
  goes to the conversation with the panel open.
- **Artifacts.** A list beside a viewer, from a new route, `GET /v1/artifacts` (§9.6). A2A has no method that lists
  artifacts, so the route belongs to the console API only.
- **Agents.** One card per A2A service, from `GET /v1/services` and `GET /v1/services/{name}/cards`: name, version,
  description, node, provider, streaming, push notifications, extensions, the URL on the gate (`/agents/{name}`), and
  each skill with its tags and examples. "Chat" starts a conversation with it. "Card JSON" shows the served card as a
  tree. The comparison of the callee's card with the served card stays reachable from the card. "Playground" opens
  today's Playground on that service (`#/play/{name}`), unchanged.
- **MCP.** One card per MCP service: the node, the kind of backend, the URL on the gate (`/mcp/{name}`), and the
  tools from `GET /v1/services/{name}/tools` (§9.5). A tool the service's `tools` policy denies says so. "tools/list
  JSON" shows the list with each input schema.
- **Nodes.** One card per connected node, from `GET /v1/tunnels`: its name, since when it is connected, and the
  services it hosts, A2A and MCP.
- **Catalog.** Kontra Cloud's. Out of this note.
- **Traffic.** Today's Traffic and MCP calls in one list, newest first: every A2A exchange on both legs, refused ones
  included (`GET /v1/exchanges`, labelled `Caller · <method>` and `Gate → Callee · <method>` as today, D360), and every
  MCP tool call (`GET /v1/calls`, `Caller · tools/call <tool>`).
  - A filter (All, A2A, MCP), today's filters on leg, method, context and failures, and the context filter that
    "Open in Traffic" sets.
  - Each A2A row opens its exchange (`#/x/{id}`: bodies, frames, validation) as today.
  - **Live.** New rows arrive as they are recorded, from a new route, `GET /v1/traffic/events` (Server-Sent Events:
    `event: exchange` or `call`, `data` the list's row). It is woken by `NOTIFY kontra_traffic`, which the traffic
    recorder sends after each batch it writes and the calls writer after each row. The route resumes from the last row
    seen (`Last-Event-ID`), like the task events. "Pause" holds new rows and counts them; resuming shows them.
  - A call shows once it ends, because the record writes its row then (D353). A stream that is still open is live in
    Chat, through its task's events.
  - MCP shows what the gate records: tool calls ([`relay.md`](../spec/relay.md) §11.4). Other MCP methods
    (`initialize`, `tools/list`, `resources/*`) are not recorded today. Recording them is a change to the traffic
    record, not to this one.

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
the Tasks, Artifacts, Agents, MCP and Nodes sections; markdown rendered while it streams; Traffic, live, with its
protocol filter, pause, and the context filter "Open in Traffic" sets; the Playground button on an agent's card.

The mock is drawn as Kontra Cloud would show it to the party `acme`: every conversation listed is one where acme is
the caller or owns the service called (§9.4). On the local gate every caller would read `anonymous`.

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
7. Agents, MCP and Nodes, with the tools route and the Playground reached from Agents.
8. Traffic: one list of A2A exchanges and MCP calls, then live with `GET /v1/traffic/events`.

## 9. The open questions, settled (20 Nov)

1. **Sealed conversations: the console shows metadata only.** It is not given the content through the node. The gate
   holds no key that opens a part and stores ciphertext only (`ext-sealed.md` §2). For the console to read content
   through the tunnel, the node would have to send plaintext across the gate, which is exactly what sealing rules out.
   Kontra Cloud's rule also stands: the gate never dials a node. The console is given what Cloud gives every reader of a
   sealed task: each event's kind and time, the task's ids and states (§3.5). The same view serves the callee's owner
   on Cloud, who never reads content. On the local gate the question does not arise, because a sealed service is
   refused at load ([`relay.md`](../spec/relay.md) §18.1). The parties read their content where it is opened, on
   their nodes.
2. **Traffic, MCP calls and Playground** (the owner, 20 Nov, for Traffic). Traffic stays as a section for the live A2A
   and MCP traffic, and MCP calls merges into it (§5). The Playground is reached from each agent's card. It sends A2A
   methods by hand to one service, and Agents is where a service is picked. That is also where the A2A Inspector
   starts: from an agent's card.
3. **Task operations: Get status and Subscribe read the gate's record; Cancel is a real A2A call.** That is how the
   console already works (`internal/console/prompt.go`, D352). Reading the record works for every conversation the
   person may see, including one between agents and one they only watch, and it keeps the console out of the traffic
   it shows. A real `GetTask` or `SubscribeToTask` is scoped to the caller that created the task, so it would fail on
   every conversation the console did not start. Cancel acts, so it goes through the gate as a client would (Admission,
   the traffic record) and only on the console's own tasks. Anyone who wants the real calls on the wire has the
   Playground. A subscription to an ended task is not refused with `UnsupportedOperationError`, as the first prototype
   had it: the events route sends `end` and closes (§4).
4. **Who sees which conversation.**
   - **Local gate:** every conversation. Every client is the one `anonymous` caller (D352).
   - **Team gate:** no console.
   - **Kontra Cloud:** the rule its task routes already apply, per context. The person's party reads in full (unless
     sealed) the conversations it opened, from the console, an API key or one of its nodes. It reads without content the
     conversations its services received. Every other conversation is `404`, as for a task. Cloud needs a contexts
     route for this. Today it has `GET /v1/tasks` and no `/v1/contexts`.
5. **MCP tools: `GET /v1/services/{name}/tools`, from the list the gate already drains.**
   - The gate drains each MCP service's `tools/list` and caches the upstream's list per service. It drops denied tools
     only when it shows the list to a client (`internal/mcprelay` `ToolsCache`, [`relay.md`](../spec/relay.md) §11.3),
     so it holds both the tools and the reason one is hidden.
   - The route answers the cached list. Each tool has its input schema and `denied: true` when the service's `tools`
     policy refuses it, plus when the list was drained.
   - When the cache holds no list, the console fills it as any client would: it sends `tools/list` through the gate's
     own `/mcp/{name}` route as the shared caller, as prompting does for A2A.
   - The console never calls a node directly.
   - On Kontra Cloud, where the control plane cannot read a replica's cache, the list is the one a `tools/list` as the
     person returns: denied tools are absent, and the card shows the policy's globs instead.
6. **The artifacts route: `GET /v1/artifacts`, metadata only.** It lists the artifacts in the tasks' snapshots
   (A2A's `Task.artifacts`), newest first, in keyset pages like the other lists. Each row has `artifact_id`, `name`,
   `description`, `task_id`, `context_id`, `service`, the caller, the media types and kinds of its parts, their size,
   and `complete`: the task has ended, or the artifact's last update said `lastChunk`. It filters on `service`, `task`
   and `context`. The parts are read from the task (`GET /v1/tasks/{task_id}`), so they are stored and read in one
   place, under the task's own access rule. A sealed task's artifacts are listed without name or description (D220),
   with the envelope's media type and no content.
7. **D360** (the owner, 20 Nov): narrowed for the chat transcript only, as §2 item 2 says.
