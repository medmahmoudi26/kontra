/**
 * THE ACTOR CATALOG — what a worker says it is, and what the design surfaces are typed against.
 *
 * Four routes on `/api/actors`: the list the Actors page draws, the registration a worker POSTs on
 * serve, the OCI digest that same worker pushes once it knows its own image, and the delete an
 * operator makes when a version is gone for good.
 *
 * WHAT IT NEEDS: the {@link Repo}, and nothing else. No Temporal, no object store, no lake — a
 * catalog is a table, and the whole point of `buildServer` touching Temporal lazily is that this
 * surface answers on an appliance with no cluster running at all.
 *
 * OPEN, ALL FOUR. `auth.ts` records that most of this API is unauthenticated; these predate that
 * record and are named in it. Registration in particular MUST stay open — a worker on a fleet host
 * has no operator credential to send, so gating it would take a whole fleet out of the catalog
 * rather than out of the cluster.
 */

import type { FastifyInstance } from 'fastify';

import {
  DescriptorRefused,
  VersionImmutable,
  parseDescriptor,
  refuseSchemaChange,
} from '../catalog';
import { compareWithPreceding } from '../compat';
import type { Repo } from '../db/repo';

export function registerCatalogRoutes(app: FastifyInstance, repo: Repo): void {
  app.get('/api/actors', async () => repo.listActors());

  /**
   * REGISTRATION IS SCREENED (src/catalog.ts), on the way in and against what is already stored.
   *
   * This route used to check three strings and then cast: `(body.operations ?? []) as
   * ActorOperation[]`, which is erased at build time, so any JSON registered with a 200 and the
   * first reader of an operation was the first check. And a re-registration of an existing
   * `(name, version)` overwrote whatever schema was there, so an actor whose types were edited
   * and re-served replaced the shape a designed pipeline was typed against with no event
   * recording it (ADR 0004). Both refusals name what is wrong; the second names the Method.
   *
   * AND THEN IT REPORTS WITHOUT REFUSING (src/compat.ts). The two gates above say nothing about a
   * NEW version, so `probe@0.2.0` could drop a field `0.1.0` emitted and every caller reading it
   * found out inside a run. The cross-version comparison runs after them and cannot fail the
   * request: a breaking change in a new version is what versions are FOR, and a gate that refused
   * it would be one somebody had to be able to bypass. What it produces is stored on the row and
   * drawn on the Actors page beside the version that introduced it.
   */
  app.post('/api/actors', async (req, reply) => {
    try {
      const descriptor = parseDescriptor(req.body);
      refuseSchemaChange(repo.getActor(descriptor.key), descriptor);
      // The whole catalog, because "which version comes before this one" is a question about the
      // catalog; `compareWithPreceding` picks the same-name rows out of it.
      const { findings } = compareWithPreceding(descriptor, repo.listActors());
      return repo.upsertActor(
        // ABSENT rather than an empty array when nothing was reported — see ActorRecord: the row
        // must not grow a key that reads as "compared, and compatible".
        findings.length > 0 ? { ...descriptor, incompatibilities: findings } : descriptor
      );
    } catch (err) {
      // 409, not 400: the descriptor is well-formed and the conflict is with the version already
      // in the catalog, which is a different thing for a client to report than a bad body.
      if (err instanceof VersionImmutable) return reply.code(409).send({ error: err.message });
      if (err instanceof DescriptorRefused) return reply.code(400).send({ error: err.message });
      throw err;
    }
  });

  // Worker self-registration of its OCI image digest (ADR 0011). Updates ONLY the
  // digest (preserving any catalogued schemas), so a worker closes the identity loop on
  // startup without clobbering the design-tool's upload.
  //
  // DELIBERATELY NOT SCREENED by the gate above. This body states no schema, so there is nothing
  // for ADR 0004 to be violated by — and it is posted on every worker start, so refusing it
  // would take a whole fleet out of the catalog over a contract it never claims to describe.
  app.post('/api/actors/:key/digest', async (req, reply) => {
    const { key } = req.params as { key: string };
    const body = req.body as Partial<{ name: string; version: string; digest: string }>;
    if (!body?.name || !body.version || !body.digest) {
      return reply.code(400).send({ error: 'digest registration requires name, version, digest' });
    }
    return repo.setActorDigest({ key, name: body.name, version: body.version, digest: body.digest });
  });

  app.delete('/api/actors/:key', async (req, reply) => {
    const { key } = req.params as { key: string };
    return repo.deleteActor(key) ? { ok: true } : reply.code(404).send({ error: 'not found' });
  });
}
