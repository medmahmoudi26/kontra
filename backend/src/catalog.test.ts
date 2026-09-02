/**
 * The registration gate, on its own — the route's half of this is in server.test.ts.
 *
 * What is being pinned is mostly the SHAPE OF THE REFUSALS: which bodies are refused, which are
 * not, and that the message says the thing an operator has to do next. The two failures behind it
 * were both silent (a cast that let any JSON into the `actors` table, and a re-registration that
 * overwrote a schema a pipeline was typed against), so a gate that refuses the wrong things is
 * only a different silence — a worker that cannot register looks exactly like one nobody started.
 */
import { describe, expect, it } from 'vitest';

import {
  DescriptorRefused,
  VersionImmutable,
  parseDescriptor,
  parseWorkflowDescriptor,
  refuseSchemaChange,
  type Descriptor,
} from './catalog';
import type { ActorRecord } from './db/repo';

const HOST = { type: 'object', properties: { host: { type: 'string' } }, required: ['host'] };

/** A descriptor as an SDK emits one, with whatever operations the case needs. */
const descriptor = (operations: Array<Record<string, unknown>>): Record<string, unknown> => ({
  key: 'demo@1.2.3',
  name: 'demo',
  version: '1.2.3',
  schemaVersion: 'kontra.actor.v1',
  operations,
});

/** What the catalog already holds — the store's own `savedAt` is the only thing it adds. */
const stored = (d: Descriptor): ActorRecord => ({ ...d, savedAt: 1 });

describe('parseDescriptor', () => {
  it('refuses operations that are not operations, saying where', () => {
    // The exact body the old cast let through: `(body.operations ?? []) as ActorOperation[]` is
    // erased at build time, so this registered with a 200 and the Actors page rendered it.
    const err = (() => {
      try {
        parseDescriptor(descriptor([1, 2, 3] as unknown as Array<Record<string, unknown>>));
      } catch (e) {
        return e;
      }
    })();
    expect(err).toBeInstanceOf(DescriptorRefused);
    expect((err as Error).message).toContain('/operations/0');
  });

  it('refuses a Method with no name, and a descriptor with no version', () => {
    expect(() => parseDescriptor(descriptor([{ input: HOST }]))).toThrow(DescriptorRefused);
    expect(() => parseDescriptor({ key: 'demo@1.2.3', name: 'demo' })).toThrow(DescriptorRefused);
  });

  it('refuses an input that is not a JSON Schema document, naming the Method and the field', () => {
    // Opaque does not mean unread: the catalog forwards this document to a client that will
    // compile it, so a `type` no dialect defines is a 500 on the execution path later.
    expect(() => parseDescriptor(descriptor([{ name: 'fetch', input: { type: 'nonsense' } }])))
      .toThrow(/Method "fetch" input is not a JSON Schema document/);
    // …and one that is not a document at all — the wire slot is a Struct.
    expect(() => parseDescriptor(descriptor([{ name: 'fetch', output: 'string' }]))).toThrow(
      DescriptorRefused
    );
  });

  it('does not read INSIDE a schema document it accepts', () => {
    // `properties.host.type` being a nonsense TYPE would fail the meta-schema; a property named
    // whatever, with a description nobody validates, is the emitter's business.
    const d = parseDescriptor(
      descriptor([{ name: 'fetch', input: { type: 'object', properties: { host: {} }, 'x-kontra': 7 } }])
    );
    expect(d.operations[0]?.input).toEqual({ type: 'object', properties: { host: {} }, 'x-kontra': 7 });
  });

  it('accepts a schema branded with a dialect this ajv does not hold', () => {
    // Ajv THROWS (`no schema with key or ref`) for a `$schema` it has no meta-schema for, rather
    // than answering false. Treating that as "not a JSON Schema document" would take a real actor
    // out of the catalog over its emitter's choice of draft.
    const seven = { $schema: 'http://json-schema.org/draft-07/schema#', type: 'object' };
    expect(parseDescriptor(descriptor([{ name: 'fetch', input: seven }])).operations[0]?.input).toEqual(
      seven
    );
  });

  it('refuses a key that is not {name}@{version}', () => {
    // Not pedantry: the immutability check looks a registration up BY key, so a row keyed any
    // other way is a second entry for one actor and the gate is bypassable by spelling.
    expect(() => parseDescriptor({ ...descriptor([]), key: 'demo' })).toThrow(/must be "demo@1.2.3"/);
  });

  it('keeps digest and source ABSENT when the worker omitted them', () => {
    // `upsertActor` keeps a previously registered digest only on `a.digest ?? prev.digest`.
    // An undefined-valued key would still work there; a "" would unpin, which is why both SDKs
    // omit rather than send one, and why absence has to survive this narrowing.
    const d = parseDescriptor(descriptor([]));
    expect('digest' in d).toBe(false);
    expect('source' in d).toBe(false);
    expect(parseDescriptor({ ...descriptor([]), digest: '' }).digest).toBe('');
  });

  it('drops a field it does not know rather than refusing the actor', () => {
    // proto3 ignores unknown fields and the wire is JSON (ADR 0002): an orchestrator that refused
    // a descriptor because a newer SDK grew a field would empty a fleet's catalog over something
    // it can safely not store.
    const d = parseDescriptor({ ...descriptor([{ name: 'fetch', tomorrow: true }]), tomorrow: 'x' });
    expect(d).not.toHaveProperty('tomorrow');
    expect(d.operations[0]).toEqual({ name: 'fetch' });
  });
});

describe('refuseSchemaChange (ADR 0004)', () => {
  const previous = (operations: Array<Record<string, unknown>>): ActorRecord =>
    stored(parseDescriptor(descriptor(operations)));

  it('allows a re-registration that says the same thing', () => {
    // A worker restarting posts its descriptor again on every boot. This is the common case and
    // the one the gate must never touch.
    const ops = [{ name: 'fetch', input: HOST, output: { type: 'object' } }];
    expect(() => refuseSchemaChange(previous(ops), parseDescriptor(descriptor(ops)))).not.toThrow();
  });

  it('allows the same schema re-emitted with its keys in another order', () => {
    // Go marshals a map in sorted key order, Python a dict in insertion order. Comparing the raw
    // JSON would call a re-ordered but identical schema a change and demand a bump for nothing.
    const before = previous([{ name: 'fetch', input: { type: 'object', properties: { a: { type: 'string' } } } }]);
    const after = parseDescriptor(
      descriptor([{ name: 'fetch', input: { properties: { a: { type: 'string' } }, type: 'object' } }])
    );
    expect(() => refuseSchemaChange(before, after)).not.toThrow();
  });

  it('allows a Method that advertises nothing, on both sides', () => {
    // `probe` in the golden descriptor: neither `takes` nor `emits`, so it registers with no
    // schemas at all. Unset must compare equal to unset, or the actor could never restart.
    const ops = [{ name: 'probe' }];
    expect(() => refuseSchemaChange(previous(ops), parseDescriptor(descriptor(ops)))).not.toThrow();
  });

  it('refuses a changed input schema, naming the Method and the fix', () => {
    const before = previous([{ name: 'fetch', input: HOST }]);
    const after = parseDescriptor(
      descriptor([{ name: 'fetch', input: { type: 'object', properties: { host: { type: 'integer' } } } }])
    );
    expect(() => refuseSchemaChange(before, after)).toThrow(VersionImmutable);
    try {
      refuseSchemaChange(before, after);
    } catch (e) {
      expect((e as Error).message).toContain('Method "fetch"');
      expect((e as Error).message).toContain('input');
      expect((e as Error).message).toMatch(/bump/);
      expect((e as Error).message).toContain('ADR 0004');
    }
  });

  it('refuses a schema APPEARING on a Method that had none — the dev loop this costs', () => {
    // Adding `takes=` to an existing Method and re-serving without bumping. This is the accepted
    // cost: the failure is loud, at registration, instead of silent and inside a later run.
    const before = previous([{ name: 'fetch' }]);
    const after = parseDescriptor(descriptor([{ name: 'fetch', input: HOST }]));
    expect(() => refuseSchemaChange(before, after)).toThrow(VersionImmutable);
  });

  it('refuses a changed params schema — ADR 0004 names params too', () => {
    const before = previous([{ name: 'fetch', params: { type: 'object' } }]);
    const after = parseDescriptor(descriptor([{ name: 'fetch', params: { type: 'object', properties: { depth: { type: 'integer' } } } }]));
    expect(() => refuseSchemaChange(before, after)).toThrow(/params/);
  });

  it('refuses a Method the catalog holds and this registration drops', () => {
    // A Method disappearing from a version is what a caller meets as a dispatch to a name that no
    // longer resolves — and it is how a RENAME would otherwise pass as an add plus a silent drop.
    const before = previous([{ name: 'fetch', input: HOST }, { name: 'title' }]);
    const after = parseDescriptor(descriptor([{ name: 'fetch', input: HOST }]));
    expect(() => refuseSchemaChange(before, after)).toThrow(/Method "title"/);
  });

  it('allows a NEW Method on an existing version', () => {
    // The digest can land before the descriptor: `setActorDigest` creates a row with
    // `operations: []`, and the registration that follows would be refused as an addition.
    const before = previous([]);
    expect(() =>
      refuseSchemaChange(before, parseDescriptor(descriptor([{ name: 'fetch', input: HOST }])))
    ).not.toThrow();
  });

  it('allows a re-worded description — that is not a schema change', () => {
    // Making a docstring typo demand a version bump would teach everyone to bump for nothing.
    const before = previous([{ name: 'fetch', description: 'Fetch each host once.', input: HOST }]);
    const after = parseDescriptor(
      descriptor([{ name: 'fetch', description: 'Fetch every host exactly once.', input: HOST }])
    );
    expect(() => refuseSchemaChange(before, after)).not.toThrow();
  });

  it('has nothing to say about an actor the catalog has never seen', () => {
    expect(() => refuseSchemaChange(undefined, parseDescriptor(descriptor([{ name: 'fetch' }])))).not.toThrow();
  });
});

/**
 * The caller's half. Screened the same way an actor's is, and gated differently: a workflow has no
 * version, so a re-registration overwrites rather than being refused — see parseWorkflowDescriptor.
 */
describe('parseWorkflowDescriptor', () => {
  it('keeps the five fields the descriptor defines and drops the rest', () => {
    const d = parseWorkflowDescriptor({
      name: 'NsCheck',
      description: 'Check every domain delegation.',
      // THE QUEUE IS A REAL FIELD NOW, and this test is where that landed: `queue` used to be the
      // EXAMPLE of an unknown field to drop. It is the address half of starting a workflow — the
      // type says what Temporal routes on, this says where — and dropping it is what let the Run
      // button send `Canary` to a queue whose worker had never heard of it.
      queue: 'canary',
      input: { type: 'object' },
      output: { type: 'object' },
      // A field a newer SDK might add. proto3 ignores what it does not know, and refusing this
      // would take a fleet's workflows out of the catalog over something droppable.
      retentionDays: 30,
    });
    expect(d).toEqual({
      name: 'NsCheck',
      description: 'Check every domain delegation.',
      queue: 'canary',
      input: { type: 'object' },
      output: { type: 'object' },
    });
  });

  it('leaves the queue ABSENT when a worker did not name one', () => {
    // Absent and empty are different answers, and the Run form reads them differently: absent
    // means "nobody has said", which is a field to leave alone, and '' would be a value to put in
    // it — replacing a usable queue with a blank one. An older SDK sends neither.
    expect(parseWorkflowDescriptor({ name: 'NsCheck' })).not.toHaveProperty('queue');
    expect(parseWorkflowDescriptor({ name: 'NsCheck', queue: '' })).not.toHaveProperty('queue');
  });

  it('refuses a descriptor with no name', () => {
    // The name is the type Temporal routes on AND the primary key of the row, so a nameless
    // registration has nothing to key on — and a row keyed '' would be every one of them at once.
    expect(() => parseWorkflowDescriptor({ description: 'x' })).toThrow(DescriptorRefused);
    expect(() => parseWorkflowDescriptor({ name: '' })).toThrow(DescriptorRefused);
  });

  it('refuses an input that is not a JSON Schema document, naming the Workflow', () => {
    // Named as a Workflow, not as a Method: one helper screens both halves of the catalog, and an
    // operator sent looking through an Actor for `NsCheck` will not find it there.
    expect(() => parseWorkflowDescriptor({ name: 'NsCheck', input: { type: 'nonsense' } })).toThrow(
      /Workflow "NsCheck" input is not a JSON Schema document/
    );
    expect(() => parseWorkflowDescriptor({ name: 'NsCheck', output: 'string' })).toThrow(
      DescriptorRefused
    );
  });

  it('keeps an absent description absent, and an empty one present', () => {
    // "The author wrote no docstring" and "the author wrote a blank one" are different facts that
    // the page draws differently, so the key's presence has to survive the trip through here.
    expect('description' in parseWorkflowDescriptor({ name: 'Ping' })).toBe(false);
    expect(parseWorkflowDescriptor({ name: 'Ping', description: '' }).description).toBe('');
  });

  it('registers a workflow that declares no types at all', () => {
    // An unannotated run method is a workflow that advertises nothing, not one that may not exist.
    // Refusing it would leave a workflow that serves perfectly and appears nowhere.
    expect(parseWorkflowDescriptor({ name: 'Untyped' })).toEqual({ name: 'Untyped' });
  });

  it('carries the import error a watch-mode serve posts when the file no longer imports', () => {
    // A broken file is a state, not a silence (instrument-panel slice 03). Watch mode posts the
    // import error keyed by the type, with no schema — the store overwrites input/output to NULL and
    // the page draws the error instead of the last good form. The queue rides along so the poller
    // signal survives while the form is replaced.
    const d = parseWorkflowDescriptor({
      name: 'NsCheck',
      queue: 'wf-nscheck-abc',
      error: 'workflow.py no longer imports: SyntaxError: unexpected EOF',
    });
    expect(d).toEqual({
      name: 'NsCheck',
      queue: 'wf-nscheck-abc',
      error: 'workflow.py no longer imports: SyntaxError: unexpected EOF',
    });
    expect(d).not.toHaveProperty('input');
    expect(d).not.toHaveProperty('output');
  });

  it('leaves the error ABSENT when the file imports, and drops an empty one', () => {
    // Absent and '' both mean "the file imports", and only a real message is a broken state the
    // page should draw — the same truthy rule `queue` follows.
    expect(parseWorkflowDescriptor({ name: 'NsCheck' })).not.toHaveProperty('error');
    expect(parseWorkflowDescriptor({ name: 'NsCheck', error: '' })).not.toHaveProperty('error');
  });

  it('accepts the schema a dict-annotated workflow really emits', () => {
    // Not a hypothetical: this is what pydantic derives for `dict | None`, and it is what
    // `.kontra/workflows/ping` registers today.
    const d = parseWorkflowDescriptor({
      name: 'Ping',
      input: { anyOf: [{ type: 'object', additionalProperties: true }, { type: 'null' }] },
      output: { type: 'object', additionalProperties: true },
    });
    expect(d.input).toEqual({
      anyOf: [{ type: 'object', additionalProperties: true }, { type: 'null' }],
    });
  });
});
