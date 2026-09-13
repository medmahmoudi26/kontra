/**
 * UPLOADS — the bytes behind a `File` or `Folder` field on a Method or workflow form.
 *
 * ── WHY THIS EXISTS AT ALL ──────────────────────────────────────────────────────────────────────
 *
 * An actor that takes a file cannot be driven from the console without one. The author writes
 * `takes=kontra.File`, the form draws a drop zone, and the bytes have to go SOMEWHERE before the
 * run starts — because what a workflow argument can carry is a value, and a 40 MB corpus inlined
 * into one is a 40 MB workflow history entry that Temporal will refuse or the codec will offload a
 * second time.
 *
 * ── IT IS THE SAME CAS THE CODEC AND THE ACTORS ALREADY SHARE ───────────────────────────────────
 *
 * `ObjectStore.putContentAddressed` writes to `cas/<sha[:2]>/<sha>` under the configured prefix,
 * which is byte-for-byte where the claim-check codec puts an offloaded payload and exactly where
 * `kontra.fetch_blob` looks. So an uploaded file is ALREADY dereferenceable by every actor in every
 * language, with no new plane, no new key convention and no second thing to configure. The upload
 * is a claim-check the operator performed by hand.
 *
 * CONTENT-ADDRESSED MEANS RE-UPLOADING IS FREE and re-running is exact. Dropping the same corpus
 * twice writes once; a run repeated a week later names the same sha and gets the same bytes or a
 * loud miss, never a file somebody replaced under the same name.
 *
 * ── WHAT IT DELIBERATELY IS NOT ─────────────────────────────────────────────────────────────────
 *
 * NOT A FILE MANAGER. There is no listing, no delete and no browse: an upload is an INPUT to one
 * run, and a route that enumerated the CAS would be handing out every payload the codec has ever
 * offloaded — datasets, batches, other tenants' arguments — through a form control. `GET` answers
 * exactly one sha, and only to a caller that already knows it.
 *
 * NOT MULTIPART. The body is the bytes and the name is a query parameter, which needs no parser
 * plugin, no dependency, and no boundary handling. The browser sends one `fetch` per file; a folder
 * is N of those, because a folder in a browser IS N files (`webkitdirectory`) and pretending
 * otherwise would mean inventing an archive format on the way in and out.
 *
 * FAIL-CLOSED (`checkBearer`, `STATE_TOKEN_VARS`), because this WRITES to the object store. The run
 * surface's opt-in token is the wrong model for a route that accepts bytes from anybody who can
 * reach the port.
 */

import type { FastifyInstance } from 'fastify';

import { STATE_TOKEN_VARS, checkBearer } from '../auth';
import { ObjectStore } from '../codec/objectStore';

/**
 * The biggest single file the form will take, in bytes.
 *
 * BOUNDED BY THE SERVER'S OWN BODY LIMIT, which is 32 MiB (`server.ts`'s `Fastify({ bodyLimit })`).
 * Stating it here as well is not redundant: Fastify's refusal is a bare `FST_ERR_CTP_BODY_TOO_LARGE`
 * with no mention of what the limit is or which file broke it, and an operator who has just dragged
 * a corpus in deserves the number. The check below never fires in practice — Fastify rejects first —
 * and it is the sentence the route would otherwise not have.
 */
export const MAX_UPLOAD_BYTES = 32 * 1024 * 1024;

/** What an upload answers with, and what the form then carries as the field's value. */
export interface UploadedBlob {
  /** The name as the operator's filesystem had it. Carried for the ACTOR to read — never used to
   *  address anything, because a name is not unique and is not ours to trust. */
  name: string;
  /** Hex sha256 of the bytes. THIS is the address: `kontra.fetch_blob` takes it. */
  sha256: string;
  size: number;
  /** What the browser said it was. A hint for the actor, never a validation — `text/csv` from a
   *  drag is the OS's guess, and refusing on it would refuse correct files. */
  contentType: string;
}

/**
 * A filename, reduced to something safe to hand on.
 *
 * IT IS NOT A PATH AND MUST NOT BECOME ONE. Nothing here uses the name to address a byte — the sha
 * does that — but the name travels into a workflow argument, into a dataset, and into whatever the
 * actor does with it, and an author who joins it onto a directory should not find `../../etc/passwd`
 * on the other end. So: the last segment, no separators, no NUL, bounded.
 *
 * A FOLDER DROP KEEPS ITS RELATIVE PATH, and that is the one exception — `corpus/2026/a.txt` is
 * what makes a folder a folder rather than a bag of names. Those arrive already split by the client
 * into `{folder, name}` and only the LAST segment comes through here, so the rule is unchanged.
 */
export function safeName(raw: string): string {
  const last = raw.split(/[/\\]/).pop() ?? '';
  // WRITTEN AS ESCAPES, NEVER AS THE CHARACTERS THEMSELVES. A literal NUL and a literal DEL in a
  // character class do not survive being written, pasted or diffed — this line was briefly
  // `/[^@-^_]/`, a class that stripped nearly every printable byte and would have renamed
  // `hosts.txt` to `upload`. Control characters and DEL, and nothing else.
  const cleaned = last.replace(/[\u0000-\u001f\u007f]/g, '').trim();
  return cleaned === '' || cleaned === '.' || cleaned === '..' ? 'upload' : cleaned.slice(0, 255);
}

export function registerUploadRoutes(app: FastifyInstance, store?: ObjectStore): void {
  const objects = store ?? new ObjectStore();

  /* THE BYTES ARRIVE RAW. Fastify has no parser for `application/octet-stream`, so without this it
     answers 415 to every upload — and the parser is registered on the app rather than per route
     because that is the only scope Fastify offers for one. */
  app.addContentTypeParser(
    'application/octet-stream',
    { parseAs: 'buffer' },
    (_req, body, done) => done(null, body)
  );

  app.post('/api/uploads', async (req, reply) => {
    const denied = checkBearer(req.headers.authorization, STATE_TOKEN_VARS);
    if (denied) return reply.code(denied.code).send(denied.body);

    if (!objects.enabled) {
      // NOT A 500. An installation with no object store configured is a real configuration, and the
      // honest answer names what is missing rather than looking like a crash.
      return reply
        .code(503)
        .send({ error: 'no object store is configured — set KONTRA_S3_ENDPOINT to accept uploads' });
    }

    const body = req.body;
    if (!Buffer.isBuffer(body)) {
      return reply.code(415).send({
        error: 'send the file as the raw body with Content-Type: application/octet-stream',
      });
    }
    // AN EMPTY FILE IS REFUSED, and it is refused here rather than stored. Zero bytes
    // content-address perfectly well, so this is not a technical limit — it is that an empty file
    // in a form is almost always a drag that did not pick anything up, and storing it turns that
    // mistake into a run that fails much later for a reason nobody can trace back to the drop.
    if (body.length === 0) return reply.code(400).send({ error: 'that file is empty' });
    if (body.length > MAX_UPLOAD_BYTES) {
      return reply.code(413).send({
        error: `that file is ${body.length} bytes; the limit is ${MAX_UPLOAD_BYTES}`,
      });
    }

    const query = req.query as { name?: string; type?: string };
    const name = safeName(query.name ?? '');
    const sha256 = await objects.putContentAddressed(new Uint8Array(body));
    const answer: UploadedBlob = {
      name,
      sha256,
      size: body.length,
      contentType: typeof query.type === 'string' ? query.type.slice(0, 128) : '',
    };
    return reply.send(answer);
  });
}
