// A task's thread as data (#/t/<task>, views/thread.js): what each event of the gate's log says
// (relay.md §5.2: an A2A StreamResponse per event, its parts read in the v1 and v0.3 shapes), and
// results folded by artifact id as A2A folds them.

import { a2uiMessages, isA2uiPart } from "./a2uiwire.js";
import { ap2Of } from "./ap2.js";
import { requestedForm } from "./agentstack.js";
import { base64Size } from "./files.js";

// ------------------------------------------------------------------ what an event shows

const isObj = (v) => v !== null && typeof v === "object" && !Array.isArray(v);
const isScalar = (v) => v === null || typeof v === "string" || typeof v === "number" || typeof v === "boolean";

/** The A2A project's experimental Structured Input extension (v1): an agent's form is a data part
 * tagged `input_schema` under this URI in the part's metadata, its invalid answer a part tagged
 * `validation_error`. */
export const STRUCTURED_INPUT = "https://github.com/a2aproject/experimental-ext-structured-input/tree/main/v1";
/** A form part's JSON the thread still draws, and a flat object it shows as names and values. */
export const FORM_MAX = 64 * 1024;
export const KV_MAX = 40;

const json = (v) => {
  try {
    return JSON.stringify(v, null, 2);
  } catch {
    return String(v);
  }
};

/** A flat object (at most KV_MAX keys, scalar values) as [name, value] rows, or null. */
export function flatRows(data) {
  if (!isObj(data)) return null;
  const entries = Object.entries(data);
  if (!entries.length || entries.length > KV_MAX || !entries.every(([, v]) => isScalar(v))) return null;
  return entries.map(([k, v]) => [k, v === null ? "null" : typeof v === "string" ? v : JSON.stringify(v)]);
}

/** The longest Structured Input request id the console sends back (the reply route's bound). */
export const INPUT_ID_MAX = 4096;
// eslint-disable-next-line no-control-regex
const CONTROL = /[\x00-\x1f\x7f]/;

/** The form a data part carries, or null: the A2A samples' form part (`{type: "form", form,
 * form_data, instructions}`, which the A2A Inspector draws) or a Structured Input `input_schema`
 * part (its data is the schema; its tag's `id`, which the extension makes optional, is kept for
 * the answer, and `badId` says one was given that the console cannot send back). */
export function formOf(p) {
  const tag = isObj(p.metadata) && isObj(p.metadata[STRUCTURED_INPUT]) ? p.metadata[STRUCTURED_INPUT] : null;
  if (tag && tag.type === "input_schema" && isObj(p.data)) {
    const given = tag.id !== undefined && tag.id !== null;
    const ok = typeof tag.id === "string" && tag.id.length > 0 && tag.id.length <= INPUT_ID_MAX && !CONTROL.test(tag.id);
    return { schema: p.data, initial: null, instructions: null, inputId: ok ? tag.id : null, badId: given && !ok, convention: "structured" };
  }
  const d = p.data;
  if (isObj(d) && d.type === "form" && isObj(d.form)) {
    return {
      schema: d.form,
      initial: isObj(d.form_data) ? d.form_data : null,
      instructions: typeof d.instructions === "string" && d.instructions.trim() ? d.instructions.slice(0, 2000) : null,
      inputId: null,
      badId: false,
      convention: "samples",
    };
  }
  return null;
}

/** A Structured Input `validation_error` part's errors ([{at, error}]), or null. */
function validationOf(p) {
  const tag = isObj(p.metadata) && isObj(p.metadata[STRUCTURED_INPUT]) ? p.metadata[STRUCTURED_INPUT] : null;
  if (!tag || tag.type !== "validation_error") return null;
  const errors = isObj(p.data) && Array.isArray(p.data.errors) ? p.data.errors : [];
  return errors.filter(isObj).slice(0, 50).map((e) => ({
    at: typeof e.instanceLocation === "string" ? e.instanceLocation : "",
    error: typeof e.error === "string" ? e.error.slice(0, 300) : "invalid",
  }));
}

const BLOB = "kontra-blob:";

/** The media type of a part sealed end to end (spec/ext-sealed.md §3): its content is ciphertext
 * only the parties' nodes can open. The console never decrypts it. */
export const SEALED_TYPE = "application/vnd.kontra.sealed+age";
/** The part metadata key a sealed part carries: `{original}`, the original media type or kind. */
export const SEALED_KEY = "kontra.sealed";

/** A sealed part's envelope, as the thread labels it, or null for any other part: { original,
 * name, type, size, metadata } -- its original media type (or kind), its filename, its envelope
 * type, the ciphertext's size in bytes (null when the gate stores it apart), its metadata as it
 * came. Its content is never read. */
export function sealedOf(p) {
  if (!isObj(p)) return null;
  const f = isObj(p.file) ? p.file : p;
  const type = [p.mediaType, f.mimeType, f.mediaType].find((x) => typeof x === "string" && x) || "";
  const tag = isObj(p.metadata) && isObj(p.metadata[SEALED_KEY]) ? p.metadata[SEALED_KEY] : null;
  if (type.split(";")[0].trim().toLowerCase() !== SEALED_TYPE && !tag) return null;
  const raw = typeof p.raw === "string" ? p.raw : typeof f.bytes === "string" ? f.bytes : null;
  return {
    original: tag && typeof tag.original === "string" ? tag.original.slice(0, 200) : null,
    name: [p.filename, f.name, f.filename].find((x) => typeof x === "string" && x) || null,
    type: type || SEALED_TYPE,
    size: raw !== null ? base64Size(raw) : null,
    metadata: isObj(p.metadata) ? p.metadata : null,
  };
}

/** A file part (A2A 1.0 `raw` or `url` with `filename` and `mediaType`; v0.3 `file` with `bytes`
 * or `uri`, `name` and `mimeType`) as { name, type, raw, ref, href, url }: `raw` its base64 text,
 * `ref` the gate's blob ref (`kontra-blob:<ref>`, fetched through the console API), `href` an
 * https link the agent wrote, `url` anything else (shown as text, never as a link). */
export function fileOf(p) {
  const f = isObj(p.file) ? p.file : p;
  const name = [f.filename, f.name, p.filename].find((x) => typeof x === "string" && x) || null;
  const type = [f.mediaType, f.mimeType, p.mediaType].find((x) => typeof x === "string" && x) || "";
  const out = { name, type, raw: null, ref: null, href: null, url: null };
  const raw = typeof p.raw === "string" ? p.raw : typeof f.bytes === "string" ? f.bytes : null;
  const url = typeof p.url === "string" ? p.url : typeof f.uri === "string" ? f.uri : null;
  if (raw !== null) out.raw = raw;
  else if (url !== null && url.startsWith(BLOB) && /^[a-z2-7]{26}$/.test(url.slice(BLOB.length))) out.ref = url.slice(BLOB.length);
  else if (url !== null && url.startsWith("https://")) out.href = url;
  else if (url !== null) out.url = url.slice(0, 500);
  return out;
}

/** The text, data, forms and files of an A2A message's or artifact's parts (v1 and v0.3 shapes):
 *
 * * `{text}`;
 * * A2UI (js/a2uiwire.js): `{data, a2ui}` (its messages, or null when they are not A2UI's), or the
 *   person's own answer to a surface, `{data, actions}`;
 * * an agent's form: `{data, form}` (formOf);
 * * a Structured Input validation error: `{data, invalid}` ([{at, error}]);
 * * any other data part: `{data, rows}` (a flat object's names and values, else null), `data`
 *   being its JSON as it came;
 * * a part sealed end to end: `{sealed}` (sealedOf: its envelope, never its content);
 * * a file: `{file}` (fileOf).
 */
export function partsOf(parts) {
  const out = [];
  for (const p of Array.isArray(parts) ? parts : []) {
    if (!p || typeof p !== "object") continue;
    const sealed = sealedOf(p);
    if (sealed) out.push({ sealed });
    else if (typeof p.text === "string") out.push(typeof p.mediaType === "string" && p.mediaType ? { text: p.text, type: p.mediaType } : { text: p.text });
    else if (p.data !== undefined && isA2uiPart(p)) {
      const messages = a2uiMessages(p.data);
      const acts = messages && messages.every((m) => isObj(m.action) || isObj(m.userAction))
        ? messages.map((m) => (isObj(m.action) ? m.action : m.userAction)) : null;
      out.push(acts ? { data: json(p.data), actions: acts } : { data: json(p.data), a2ui: messages });
    } else if (p.data !== undefined && ap2Of(p.data)) {
      out.push({ data: json(p.data), ap2: ap2Of(p.data) });
    } else if (p.data !== undefined) {
      const form = formOf(p);
      const invalid = form ? null : validationOf(p);
      if (form) out.push({ data: json(p.data), form });
      else if (invalid) out.push({ data: json(p.data), invalid });
      else out.push({ data: json(p.data), rows: flatRows(p.data) });
    } else if (p.file || p.url !== undefined || p.raw !== undefined) {
      out.push({ file: fileOf(p) });
    }
  }
  return out;
}

const STATE_OF = (s) => String(s || "").replace(/^TASK_STATE_/, "").toLowerCase().replaceAll("_", "-");
const ROLE_OF = (r) => (String(r || "").toLowerCase().includes("user") ? "you" : "svc");

/** A state in words, for the events log. */
export const STATE_WORDS = Object.freeze({
  submitted: "sent to the agent",
  working: "the agent is working",
  "input-required": "the agent asks for an answer",
  "auth-required": "the agent asks for an authorization",
  completed: "done",
  failed: "failed",
  canceled: "canceled",
  rejected: "the agent refused it",
});
const stateWords = (s) => STATE_WORDS[s] || s || "updated";

/** One A2A message as the thread reads it: who wrote it, its parts, its metadata (an AgentStack form
 * the agent asks with, or the person's answer to one: js/agentstack.js), its id. */
function messageOf(m, role = ROLE_OF(m.role)) {
  return {
    role,
    parts: partsOf(m.parts),
    metadata: isObj(m.metadata) ? m.metadata : null,
    id: typeof m.messageId === "string" ? m.messageId : null,
    raw: m,
  };
}

/** What one event says: { line } for the log (in words), { state } of a task or status event,
 * and { messages, artifacts } of its content when the reader has it (`payload`). A message keeps
 * its metadata (messageOf); one with no part is kept when it asks with a form. An artifact is
 * { id, name, append, lastChunk, parts }: the thread keeps them by id, as A2A folds them (an
 * unseen id is a new result, append adds parts, anything else replaces them). Never throws on a
 * shape it does not know. */
/** The gate's metadata key on the status events of a node's reconnect wait (kontra relay.md §6.4,
 * D341): `{state: "waiting", since, until}` when the agent's node disconnected, `{state: "resumed",
 * at}` when it came back, `{state: "ended", at}` on a task that ended during the wait. */
export const RECONNECT = "kontra.reconnect";

/** A status update's reconnect marker: `{state, until}` (`until` in ms, waiting only), or null. */
export function reconnectOf(st) {
  const m = isObj(st) && isObj(st.metadata) && isObj(st.metadata[RECONNECT]) ? st.metadata[RECONNECT] : null;
  if (!m || typeof m.state !== "string") return null;
  if (m.state !== "waiting") return { state: m.state, until: null };
  const until = typeof m.until === "string" ? Date.parse(m.until) : NaN;
  return Number.isFinite(until) ? { state: "waiting", until } : null;
}

export function describe(ev) {
  const p = ev.payload && typeof ev.payload === "object" ? ev.payload : null;
  const messages = [];
  const artifacts = [];
  let line = ev.kind;
  let state = null;
  let reconnect = null;
  if (ev.kind === "task") {
    line = "The task was created";
    const task = p && p.task;
    if (task && Array.isArray(task.history)) for (const m of task.history) if (isObj(m)) messages.push(messageOf(m));
    if (task && task.status) {
      state = STATE_OF(task.status.state);
      line = `The task was created: ${stateWords(state)}`;
    }
  } else if (ev.kind === "status") {
    const st = p && (p.statusUpdate || p.status_update);
    state = st && st.status ? STATE_OF(st.status.state) : null;
    line = state ? `Now ${stateWords(state)}` : "The status changed";
    reconnect = reconnectOf(st);
    if (reconnect && reconnect.state === "waiting") line = "The node disconnected";
    else if (reconnect && reconnect.state === "resumed") line = "The node reconnected";
    // A status's message is the agent's (A2A's TaskStatus.message), whatever role it names: the
    // person's own messages reach the thread as `message` events and in the task's history, which
    // is where the gate writes them; never as a status the agent wrote.
    if (st && st.status && isObj(st.status.message)) messages.push(messageOf(st.status.message, "svc"));
  } else if (ev.kind === "message") {
    const m = p && p.message;
    line = m && ROLE_OF(m.role) === "svc" ? "A message from the agent" : "A message to the agent";
    if (isObj(m)) messages.push(messageOf(m));
  } else if (ev.kind === "artifact") {
    const a = p && (p.artifactUpdate || p.artifact_update);
    const art = a && a.artifact;
    const name = art && (art.name || art.artifactId);
    line = name ? `A result: ${String(name)}` : "A result";
    if (art) {
      artifacts.push({
        id: typeof art.artifactId === "string" && art.artifactId ? art.artifactId : null,
        name: String(art.name || art.artifactId || "result"),
        append: a.append === true,
        lastChunk: a.lastChunk === true ? true : a.lastChunk === false ? false : null,
        parts: partsOf(art.parts),
        // The parts as the agent sent them, for the thread's "{}".
        source: Array.isArray(art.parts) ? art.parts : [],
      });
    }
  } else if (ev.kind === "metadata") {
    line = "The task's metadata changed";
  } else if (ev.kind === "input_request" || ev.kind === "input_response" || ev.kind === "result") {
    line = ev.kind.replace("_", " ");
  }
  return { line, state, ...(reconnect ? { reconnect } : {}), messages: messages.filter((m) => m.parts.length || requestedForm(m.metadata)), artifacts };
}

// ------------------------------------------------------------------ results, as A2A folds them

/** One piece of a file's bytes: `{raw}` (base64 text) or `{ref}` (a file the gate keeps), or null
 * for a file with no bytes here (a link). */
export const pieceOf = (f) => (f && typeof f.raw === "string" ? { raw: f.raw } : f && f.ref ? { ref: f.ref } : null);

/** A result as the thread keeps it: `prev` (null for an id not seen yet) folded with
 * artifact `a` (describe's), as A2A and a2a-go fold them: `append: false` replaces the parts and
 * the name, `append: true` adds parts. A file part an append adds right after the result's last
 * file part, of the same name and type, is that file's next chunk: its bytes follow in order,
 * whether they came inline or the gate keeps them (a chunk over 256 KiB is `kontra-blob:<ref>`),
 * and the file downloads as one. Each file part carries `chunks` ([{raw} | {ref}], in order).
 * Returns { name, parts, chunked, lastChunk }; `prev` is never changed. */
export function foldResult(prev, a) {
  const r = prev
    ? { name: prev.name, parts: [...prev.parts], chunked: prev.chunked, lastChunk: prev.lastChunk }
    : { name: a.name, parts: [], chunked: false, lastChunk: null };
  if (a.append) {
    r.chunked = true;
  } else {
    r.parts = [];
    r.name = a.name;
  }
  for (const part of a.parts) {
    const piece = part.file ? pieceOf(part.file) : null;
    const last = r.parts[r.parts.length - 1];
    if (a.append && piece && last && last.file && Array.isArray(last.file.chunks) && last.file.chunks.length
      && last.file.name === part.file.name && last.file.type === part.file.type) {
      r.parts[r.parts.length - 1] = { file: { ...last.file, chunks: [...last.file.chunks, piece] } };
      continue;
    }
    r.parts.push(part.file ? { file: { ...part.file, chunks: piece ? [piece] : [] } } : part);
  }
  if (a.lastChunk !== null && a.lastChunk !== undefined) r.lastChunk = a.lastChunk;
  return r;
}

/** The kind of a folded result's part, as its Artifacts entry draws it: text, data (a plain data part), file, or other
 * (an A2UI surface, a form, an actions part: drawn by the part renderer). */
export function partKind(p) {
  if (!isObj(p)) return "other";
  if (p.file) return "file";
  if (p.text !== undefined) return "text";
  if (p.data !== undefined && !p.a2ui && !p.form && !p.actions) return "data";
  return "other";
}

/** A folded result's parts as its Artifacts entry draws them (spec: Streamed Text Shown Continuously): runs of
 * consecutive parts of one kind. A text run is the parts' texts joined as they came, nothing trimmed or put
 * between them, so a stream sent in pieces reads as the one text it is; its `type` is its first part's. A run of
 * another kind keeps its parts. Pure: [{ kind, parts, text?, type? }]. */
export function runsOf(parts) {
  const out = [];
  for (const p of Array.isArray(parts) ? parts : []) {
    const kind = partKind(p);
    const last = out[out.length - 1];
    if (last && last.kind === kind && kind !== "other") {
      last.parts.push(p);
      if (kind === "text") last.text += String(p.text);
      continue;
    }
    out.push(kind === "text" ? { kind, parts: [p], text: String(p.text), type: p.type || null } : { kind, parts: [p] });
  }
  return out;
}

// ------------------------------------------------------------------ the A2A wire (D357, D360)

/** The actors of the A2A wire, as the gate records an origin: the caller (the client that called the gate), the
 * callee (the service's agent) and the gate itself. A label never shows these role words: it names the party. */
export const ACTORS = Object.freeze({ caller: "caller", callee: "callee", gate: "gate" });

/** Who an actor is, by name (spec: A2A Labels On Every Item): the calling principal as the gate stores it
 * (`anonymous`, `key:<prefix>`, `oidc:sub:<sub>`, `node:<key id>`), the service's name, or `Gate`. `parties`
 * is { principal, service }, already in the host's display form; a party the record does not name is "unknown". */
export function partyOf(actor, parties) {
  const p = isObj(parties) ? parties : {};
  if (actor === "gate") return "Gate";
  if (actor === "caller") return str(p.principal) || "unknown";
  if (actor === "callee") return str(p.service) || "unknown";
  return null;
}

/** A2A's own names of a logged event's StreamResponse member: the object the caller received. */
const MEMBER_TYPES = Object.freeze({ task: "Task", message: "Message", statusUpdate: "TaskStatusUpdateEvent", artifactUpdate: "TaskArtifactUpdateEvent" });

const str = (v) => (typeof v === "string" && v ? v : null);

/** The A2A object a payload holds for the caller ("Task", "Message", "TaskStatusUpdateEvent",
 * "TaskArtifactUpdateEvent"), or null. */
export function callerTypeOf(payload) {
  if (!isObj(payload)) return null;
  for (const [member, type] of Object.entries(MEMBER_TYPES)) if (isObj(payload[member])) return type;
  return null;
}

/** The ids an event's object carries: the task's, as the caller saw them (gate ids). */
function idsOf(payload) {
  if (!isObj(payload)) return { taskId: null, contextId: null };
  if (isObj(payload.task)) return { taskId: str(payload.task.id), contextId: str(payload.task.contextId) };
  for (const m of ["statusUpdate", "artifactUpdate", "message"]) {
    if (isObj(payload[m])) return { taskId: str(payload[m].taskId), contextId: str(payload[m].contextId) };
  }
  return { taskId: null, contextId: null };
}

/** The A2A members that place a caller's message (D360): its contextId, its taskId (null: it named
 * none, so the agent may answer it with a new task or a Message), and the tasks it refers to. */
export function routingOf(m) {
  if (!isObj(m)) return null;
  return {
    contextId: str(m.contextId),
    taskId: str(m.taskId),
    referenceTaskIds: Array.isArray(m.referenceTaskIds) ? m.referenceTaskIds.filter((x) => typeof x === "string").slice(0, 20) : [],
  };
}

/** One logged event as the A2A wire labels it: who sent which A2A object in which method (its
 * origin, the gate's record: never inferred), the object the caller received for it, and whether the
 * gate converted one into the other (D5); its ids, its exchange and its validation.
 *
 * { actor, type, method, callerType, convertedTo, exchangeId, contextAssigned, ids, validation,
 *   recorded, label } -- `recorded` false for an event logged before origins were (its label then
 * names the caller-facing object only). */
export function wireOf(ev, parties) {
  const payload = isObj(ev && ev.payload) ? ev.payload : null;
  const callerType = str(ev && ev.caller_type) || callerTypeOf(payload);
  const o = isObj(ev && ev.origin) ? ev.origin : null;
  const actor = o && Object.hasOwn(ACTORS, o.actor) ? o.actor : null;
  const type = (o && str(o.type)) || callerType;
  const out = {
    actor,
    type,
    method: o ? str(o.method) : null,
    callerType,
    convertedTo: o ? str(o.converted_to) : null,
    exchangeId: o ? str(o.exchange_id) : null,
    contextAssigned: o && typeof o.context_assigned === "boolean" ? o.context_assigned : null,
    ids: idsOf(payload),
    validation: isObj(ev && ev.validation) ? ev.validation : null,
    recorded: Boolean(actor),
  };
  out.party = actor ? partyOf(actor, parties) : null;
  out.typeName = type || String((ev && ev.kind) || "event");
  out.label = actor ? `${out.party} · ${out.typeName}` : out.typeName;
  return out;
}

/** One exchange of a timeline (a call that carried no event) as the wire labels it, by name: the principal's
 * call to the gate (`anonymous · GetTask`), or the gate's call to the service (`Gate → red · GetTask`).
 * `parties` as partyOf takes it; an exchange's own `principal` and `service` fill what it lacks. */
export function wireOfExchange(x, parties) {
  const callee = x && x.leg === "callee";
  const method = str(x && x.method) || "unknown";
  const p = { principal: str(x && x.principal), service: str(x && x.service), ...(isObj(parties) ? Object.fromEntries(Object.entries(parties).filter(([, v]) => str(v))) : {}) };
  const party = callee ? `Gate → ${partyOf("callee", p)}` : partyOf("caller", p);
  return {
    actor: callee ? "gate" : "caller",
    type: method,
    method,
    party,
    typeName: method,
    exchangeId: str(x && x.id),
    label: `${party} · ${method}`,
    failed: Boolean(x && x.failed),
    outcome: x ? [x.http_status ? String(x.http_status) : null, x.rpc_code !== null && x.rpc_code !== undefined ? `rpc ${x.rpc_code}` : null, str(x.reason)].filter(Boolean).join(" · ") : "",
  };
}

/** A validation result in a few words: "valid A2A 1.0", "invalid A2A 1.0: 2 issues", "not validated: …". */
export function validationWords(v) {
  if (!isObj(v)) return null;
  if (!v.validated) return str(v.note) || "not validated";
  if (v.valid) return `valid A2A ${v.version}`;
  const n = Array.isArray(v.issues) ? v.issues.length : 0;
  return `invalid A2A ${v.version}: ${n} issue${n === 1 ? "" : "s"}`;
}
