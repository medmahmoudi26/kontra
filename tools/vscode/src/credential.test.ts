import { strict as assert } from 'node:assert';
import { test } from 'node:test';
import { credentialPrefill } from './credential.ts';

test('fills from a credential-shaped clipboard', () => {
  assert.equal(credentialPrefill('GEgX4gBYYACxeikVqRXs'), 'GEgX4gBYYACxeikVqRXs');
  assert.equal(credentialPrefill('  GEgX4gBYYACxeikVqRXs\n'), 'GEgX4gBYYACxeikVqRXs');
});

test('leaves the box empty for ordinary copied text', () => {
  // The box is MASKED, so anything filled in is invisible. These must not become password guesses.
  assert.equal(credentialPrefill(''), '');
  assert.equal(credentialPrefill('   '), '');
  assert.equal(credentialPrefill('docker compose up -d'), '');
  assert.equal(credentialPrefill('line one\nline two'), '');
  assert.equal(credentialPrefill('x'.repeat(129)), '');
});

test('a path is whitespace-free and short, so it DOES fill — and that is the trade', () => {
  // Stated rather than hidden: the rule cannot tell a token from a slug. It only has to keep
  // prose and multi-line snippets out; a wrong single word costs one refused login.
  assert.equal(credentialPrefill('/tmp/x'), '/tmp/x');
});
