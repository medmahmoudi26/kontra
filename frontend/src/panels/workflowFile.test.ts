/**
 * What the Workflows surface knows about a workflow file, beyond parsing its source.
 *
 * All three of these reach something OUTSIDE the browser — a Temporal workflow type, a workflow's
 * one argument, and the bytes written back to a file the worker will execute — so all three fail
 * somewhere other than here when they are wrong.
 *
 * They lived on the Catalog page while that page was the editor. The editor is the Workflows
 * surface now and the Catalog is gone; these moved to the module that already owned the scan they
 * delegate to.
 */

import { describe, expect, it } from 'vitest';
import {
  PY_INDENT,
  guessTypeFromFilename as guessType,
  parseInput,
  queueDigest,
  typeFromSource,
} from './workflowSource';

describe('typeFromSource', () => {
  // Temporal accepts a start for ANY type name; only a worker that registered it will pick the
  // task up. So a wrong type here does not fail — it hangs, with no error anywhere, which is why
  // the name is read from the thing that actually registers it.
  it('reads the decorated class, which the filename gets WRONG', () => {
    const src = '@workflow.defn\nclass NsCheck:\n    pass\n';
    expect(typeFromSource(src, 'nscheck.py')).toBe('NsCheck');
    // The bug this exists to prevent, pinned: the filename alone says `Nscheck`, and nothing is
    // registered under that.
    expect(guessType('nscheck.py')).toBe('Nscheck');
  });

  it('honours an explicit name= on the decorator over the class name', () => {
    const src = '@workflow.defn(name="Sweep")\nclass DnsSweepImpl:\n    pass\n';
    expect(typeFromSource(src, 'dnssweep.py')).toBe('Sweep');
  });

  it('skips blank lines and comments between the decorator and the class', () => {
    const src = '@workflow.defn\n\n# the caller\nclass NsCheck:\n    pass\n';
    expect(typeFromSource(src, 'x.py')).toBe('NsCheck');
  });

  it('takes the FIRST definition when a file holds several', () => {
    const src = '@workflow.defn\nclass First:\n    pass\n\n@workflow.defn\nclass Second:\n    pass\n';
    expect(typeFromSource(src, 'x.py')).toBe('First');
  });

  it('is not fooled by a bare class with no decorator', () => {
    // A helper class above the workflow must not become the type.
    const src = 'class Helper:\n    pass\n\n@workflow.defn\nclass Real:\n    pass\n';
    expect(typeFromSource(src, 'x.py')).toBe('Real');
  });

  it('falls back to the filename when the source says nothing yet', () => {
    // A new empty file still prefills something editable rather than nothing.
    expect(typeFromSource('', 'dns_sweep.py')).toBe('DnsSweep');
  });
});

describe('guessType', () => {
  it('title-cases the stem across - and _', () => {
    expect(guessType('dns_sweep.py')).toBe('DnsSweep');
    expect(guessType('dns-sweep.py')).toBe('DnsSweep');
  });

  it('leaves an already-capitalised stem alone', () => {
    expect(guessType('NsCheck.py')).toBe('NsCheck');
  });

  it('does not invent a type from an empty name', () => {
    expect(guessType('.py')).toBe('');
  });
});

describe('parseInput', () => {
  it('treats blank as NO argument, not as null and not as {}', () => {
    // `run(self)` taking nothing must be startable from this page. `{}` would be an argument.
    const got = parseInput('   ');
    expect(got.ok).toBe(true);
    expect(got.value).toBeUndefined();
  });

  it('parses an object and reports where invalid JSON went wrong', () => {
    expect(parseInput('{"machines": 4}').value).toEqual({ machines: 4 });
    const bad = parseInput('{machines: 4}');
    expect(bad.ok).toBe(false);
    expect(bad.error).toBeTruthy();
  });

  it('keeps a literal zero, which is not the same as absent', () => {
    // The distinction that cost four Droplets on the workflow side: `machines: 0` must survive
    // as 0 all the way to the workflow, not be flattened into a falsy nothing.
    expect(parseInput('{"machines": 0}').value).toEqual({ machines: 0 });
  });
});

describe('queueDigest — the registered digest the read-only viewer shows (ADR 0030)', () => {
  it('reads the 12-hex digest out of a derived workflow queue', () => {
    // The queue a served workflow polls is `wf-<name>-<digest12>` (GitHub #15); the suffix is the
    // identity of the code that is actually registered, which is what the viewer shows beside the
    // bytes on disk.
    expect(queueDigest('wf-nscheck-a1b2c3d4e5f6')).toBe('a1b2c3d4e5f6');
  });

  it('takes the TRUE trailing digest even when the name itself ends in -<12 hex>', () => {
    // A name is `[A-Za-z0-9._-]`, so it can end in something that looks like a digest. Anchored to
    // the end, the real digest still wins rather than the name's lookalike tail.
    expect(queueDigest('wf-a-0123456789ab-0123456789cd')).toBe('0123456789cd');
  });

  it('is empty when nothing has registered — a missing digest is not a blank one', () => {
    // No worker serving means no queue means no digest; the viewer draws "not served yet", never an
    // empty digest that reads as pinned-to-nothing.
    expect(queueDigest(undefined)).toBe('');
    expect(queueDigest('')).toBe('');
    // An actor's shared queue `<name>-<version>` is not a workflow digest queue and yields nothing.
    expect(queueDigest('probe-0.1.0')).toBe('');
  });
});

describe('the editor indents with SPACES', () => {
  it('is four spaces, never a tab', () => {
    // This is the file the worker imports. A tab mixed into a space-indented module is a
    // TabError at import time — which presents as a worker that dies on boot, in a tmux window,
    // and says nothing about where it came from.
    expect(PY_INDENT).toBe('    ');
    expect(PY_INDENT).not.toContain('\t');
  });
});
