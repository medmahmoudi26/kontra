import assert from 'node:assert/strict';
import { test } from 'node:test';

import { actorDirFor, actorKeyFrom, isInside } from './locate.ts';

test('finds the nearest actor.json above a file', () => {
  const present = new Set(['/w/myactor/actor.json']);
  const exists = (p: string) => present.has(p);
  assert.equal(actorDirFor('/w/myactor/actor.py', exists), '/w/myactor');
  // A nested file still resolves to the actor above it.
  assert.equal(actorDirFor('/w/myactor/lib/deep/thing.py', exists), '/w/myactor');
});

test('takes the NEAREST one when actors nest', () => {
  const present = new Set(['/w/actor.json', '/w/inner/actor.json']);
  const exists = (p: string) => present.has(p);
  assert.equal(actorDirFor('/w/inner/actor.py', exists), '/w/inner');
});

test('gives up rather than walking forever', () => {
  // THE TERMINATION CASE. `path.dirname('/')` is `/`, so a naive loop never ends — and the symptom
  // would be an editor command that hangs rather than one that says "not an actor".
  assert.equal(actorDirFor('/somewhere/else/file.py', () => false), undefined);
});

test('builds the catalog key', () => {
  assert.equal(actorKeyFrom({ name: 'nscheck', version: '0.1.0' }), 'nscheck@0.1.0');
  // A version-less manifest is addressable by bare name, which the catalog accepts.
  assert.equal(actorKeyFrom({ name: 'nscheck' }), 'nscheck');
  assert.equal(actorKeyFrom({ name: '  spaced  ', version: ' 1.0 ' }), 'spaced@1.0');
});

test('refuses a manifest with no name', () => {
  // Not "" and not a crash: an actor addressed by nothing is not addressable, and the caller shows
  // a message naming the file rather than opening an empty pane.
  assert.equal(actorKeyFrom({ version: '0.1.0' }), undefined);
  assert.equal(actorKeyFrom({ name: '   ' }), undefined);
  assert.equal(actorKeyFrom(null), undefined);
  assert.equal(actorKeyFrom('not an object'), undefined);
});

test('isInside does not admit a sibling that shares a prefix', () => {
  assert.equal(isInside('/srv/kontra/actors/hello/actor.py', '/srv/kontra/actors/hello'), true);
  assert.equal(isInside('/srv/kontra/actors/hello/sub/x.py', '/srv/kontra/actors/hello'), true);
  // `startsWith` would say true for this one, and a save here would reload an unrelated pane.
  assert.equal(isInside('/srv/kontra/actors/hello-evil/actor.py', '/srv/kontra/actors/hello'), false);
  assert.equal(isInside('/srv/kontra/actors/hello', '/srv/kontra/actors/hello'), false);
  assert.equal(isInside('/etc/passwd', '/srv/kontra/actors/hello'), false);
});
