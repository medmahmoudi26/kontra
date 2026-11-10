// Server-Sent Events read from a fetch Response's body (the HTML standard's "event stream"
// interpretation). The thread's stream (GET /v1/tasks/{id}/events) is fetched by api.js, so the
// thread names its own reconnect point (`after`), and read here: lines end with CRLF, LF or CR;
// a line starting with ":" is a comment (the stream's keep-alive); "data" lines are joined with
// "\n"; an empty line ends an event, which is dispatched only when it has data. "retry" and any
// other field are ignored: the thread decides itself when to reconnect.

/** The events of `body` (a ReadableStream of bytes), as { id, event, data }: `id` the last "id"
 * field seen (null before one; the server's is the event's seq), `event` its type ("message"
 * unless named), `data` its data lines joined. Ends with the stream; `signal` aborting ends it. */
export async function* readEvents(body, { signal } = {}) {
  const reader = body.getReader();
  const decoder = new TextDecoder();
  let buf = "";
  let lastId = null;
  let type = "";
  let data = [];
  let had = false;
  const field = (line) => {
    if (line === "") {
      const out = had ? { id: lastId, event: type || "message", data: data.join("\n") } : null;
      type = "";
      data = [];
      had = false;
      return out;
    }
    if (line.startsWith(":")) return null;
    const colon = line.indexOf(":");
    const name = colon < 0 ? line : line.slice(0, colon);
    let value = colon < 0 ? "" : line.slice(colon + 1);
    if (value.startsWith(" ")) value = value.slice(1);
    if (name === "data") {
      data.push(value);
      had = true;
    } else if (name === "event") type = value;
    else if (name === "id" && !value.includes("\0")) lastId = value;
    return null;
  };
  try {
    for (;;) {
      if (signal && signal.aborted) return;
      const { value, done } = await reader.read();
      if (done) break;
      buf += decoder.decode(value, { stream: true });
      // A CR at the end may be the first half of a CRLF: keep it for the next chunk.
      for (;;) {
        const m = /\r\n|\n|\r(?!$)/.exec(buf);
        if (!m) break;
        const line = buf.slice(0, m.index);
        buf = buf.slice(m.index + m[0].length);
        const ev = field(line);
        if (ev) yield ev;
      }
    }
    buf += decoder.decode();
    // The stream ended: a pending CR ends its line; an event with no blank line after it is
    // incomplete and dropped, as the standard says.
    for (const line of buf.split(/\r\n|\n|\r/).slice(0, -1)) {
      const ev = field(line);
      if (ev) yield ev;
    }
  } finally {
    try {
      await reader.cancel();
    } catch {
      // released or errored already
    }
  }
}
