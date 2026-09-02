/**
 * The workflow scan, against the files it exists for.
 *
 * `examples/python/workflows/*.py` are read from disk rather than inlined: they are the canonical
 * examples, they are what gets copied into `.kontra/workflows/`, and the two questions this module
 * answers about them — what can I start, what has to be up first — are the ones a wrong answer
 * misleads an operator about. An inlined copy would keep passing after someone edited the real
 * file.
 */

import { readFileSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

import { describe, expect, it } from 'vitest';

import {
  moduleAliases,
  stringBindings,
  stripCommentsAndDocstrings,
  workflowDefns,
  workflowRefs,
} from './workflowSource';

const here = path.dirname(fileURLToPath(import.meta.url));
const example = (name: string) =>
  readFileSync(path.resolve(here, '../../../examples/python/workflows', name), 'utf8');

// `nscheck/workflow.py`, not `nscheck.py`: it moved into the folder layout registration expects,
// so it can carry a `workflow.json` — which is where its queue is now derived from.
const NSCHECK = example('nscheck/workflow.py');
const DNSSWEEP = example('dnssweep.py');

describe('the fleet example', () => {
  it('finds the type the filename gets WRONG', () => {
    // `nscheck.py` title-cases to `Nscheck`; the class is `NsCheck`. Temporal accepts a start for
    // either and only a worker that registered the name picks the task up, so the wrong one is a
    // run that hangs with no error anywhere.
    expect(workflowDefns(NSCHECK)).toEqual(['NsCheck']);
  });

  it('lists the actor it dispatches to, the datasets it moves, and the fleet it provisions', () => {
    const refs = workflowRefs(NSCHECK);
    expect(refs).toContainEqual({ kind: 'fleet', name: 'nscheck', version: '0.1.0' });
    expect(refs).toContainEqual({ kind: 'actor', name: 'nscheck', version: '0.1.0' });
    // The literal DEFAULT of `catalog.dataset(req.get("dataset") or "domains")` — the honest
    // answer for a checklist, since the real name may come from the input.
    expect(refs).toContainEqual({ kind: 'dataset', name: 'domains' });
  });

  it('finds the fleet a hold/place workflow provisions, where the scope line names no actor', () => {
    // ADR 0037's other door. `fleet.hold(tag="dns", machines=4)` says nothing about what runs on
    // the Machines — that moved to `place()` — so a scan that only reads the `as` line reports a
    // file that provisions nothing and tells a reader to go start an actor whose Machines this
    // file was about to create itself. That is the OPPOSITE prerequisite, which is the whole
    // reason `kind: 'fleet'` is a separate kind from `kind: 'actor'`.
    const src = [
      'from actorkit import catalog, fleet',
      '',
      'async def run(self, req):',
      '    async with fleet.hold(tag="dns", machines=4) as f:',
      '        await f.place("nscheck", "0.1.0", sessions=8)',
      '        await f.ready()',
      '        async with catalog.actor("nscheck", "0.1.0") as ns:',
      '            pass',
    ].join('\n');
    expect(workflowRefs(src)).toContainEqual({ kind: 'fleet', name: 'nscheck', version: '0.1.0' });
  });

  it('does not invent a fleet from a `place` in a file that never imported it', () => {
    // THE CONTROL. `.place(` on any receiver is a broad pattern on purpose — a scope handle's name
    // is not knowable from a regex — so the import is what bounds it. Without that bound, any
    // `board.place("e4", "queen")` in any workflow becomes a Fleet on somebody's checklist.
    const src = 'from actorkit import catalog\n\nasync def run(self):\n    board.place("e4", "queen")\n';
    expect(workflowRefs(src).some((r) => r.kind === 'fleet')).toBe(false);
  });

  it('FINDS THE DATASET IT WRITES, not just the one it reads', () => {
    // MEASURED on a real fleet sweep: the Workflows page watched this file write 623 rows into
    // `lame` and never named it. The input dataset is a literal so the scan saw it; the OUTPUT is
    // `out_name = req.get("into") or "lame"` and then `catalog.dataset(out_name)`, which read as
    // an unresolvable expression. A run-output panel that cannot name the output is the one thing
    // it exists to do.
    expect(NSCHECK).toContain('out_name = req.get("into") or "lame"');
    expect(workflowRefs(NSCHECK)).toContainEqual({ kind: 'dataset', name: 'lame', writes: true });
  });

  it('KNOWS WHICH WAY EACH DATASET GOES', () => {
    // MEASURED on the run after that one: reading and writing a Dataset are the same call up to
    // the `.writer()`, so the Workflows header summed this file's 400-row INPUT list into its
    // "units committed" and announced 1,023 committed units one second after Run — before a single
    // Machine existed. A number that large and that wrong, on the stat used to judge whether a
    // run is working, is worse than no number.
    const refs = workflowRefs(NSCHECK);
    expect(refs.find((r) => r.name === 'lame')?.writes).toBe(true);
    expect(refs.find((r) => r.name === 'domains')?.writes).toBeUndefined();
  });

  it('DOES NOT MISTAKE THE DOCSTRING FOR CODE', () => {
    // This is the whole reason the strip exists. nscheck.py's docstring contains the very commands
    // this page generates, an ASCII diagram naming its methods, and prose about dispatch. A scan
    // that read prose would list prerequisites nobody referenced — and would keep looking right.
    expect(NSCHECK).toContain('kontra workflow start examples/python/workflows/nscheck');
    const clean = stripCommentsAndDocstrings(NSCHECK);
    expect(clean).not.toContain('kontra workflow start examples/python/workflows/nscheck');
    // …while the code that follows survives intact.
    expect(clean).toContain('catalog.actor("nscheck", "0.1.0")');
  });
});

describe('the actor-only example', () => {
  it('finds both actors it drives and does not invent a fleet', () => {
    expect(workflowDefns(DNSSWEEP)).toEqual(['DnsSweep']);
    const refs = workflowRefs(DNSSWEEP);
    expect(refs).toContainEqual({ kind: 'actor', name: 'dnsfacts', version: '0.1.0' });
    expect(refs).toContainEqual({ kind: 'actor', name: 'probe', version: '0.1.0' });
    expect(refs.some((r) => r.kind === 'fleet')).toBe(false);
  });
});

describe('stripCommentsAndDocstrings', () => {
  it('keeps single-line strings, which is where the names live', () => {
    expect(stripCommentsAndDocstrings('x = catalog.actor("probe", "0.1.0")  # comment')).toBe(
      'x = catalog.actor("probe", "0.1.0")  '
    );
  });

  it('blanks a docstring to spaces rather than deleting it', () => {
    // Deleting would splice the line before onto the line after, which can manufacture a call
    // that is not in the file.
    const out = stripCommentsAndDocstrings('a = 1\n"""catalog.actor("ghost")"""\nb = 2\n');
    expect(out).not.toContain('ghost');
    expect(out.split('\n')).toHaveLength(4);
  });

  it('survives an unterminated docstring instead of throwing', () => {
    // A file being EDITED is the normal case on this page — half-typed triple quotes included.
    expect(() => stripCommentsAndDocstrings('"""never closed')).not.toThrow();
  });
});

describe('workflowDefns', () => {
  it('honours an explicit name= over the class name', () => {
    expect(workflowDefns('@workflow.defn(name="Sweep2")\nclass SweepImpl:\n    pass\n')).toEqual([
      'Sweep2',
    ]);
  });

  it('returns every definition, in source order', () => {
    const src = '@workflow.defn\nclass First:\n    pass\n\n@workflow.defn\nclass Second:\n    pass\n';
    expect(workflowDefns(src)).toEqual(['First', 'Second']);
  });

  it('is not fooled by an undecorated class above the workflow', () => {
    expect(workflowDefns('class Helper:\n    pass\n\n@workflow.defn\nclass Real:\n    pass\n')).toEqual(
      ['Real']
    );
  });

  it('finds nothing in a file with no workflow, rather than guessing', () => {
    expect(workflowDefns('x = 1\n')).toEqual([]);
  });
});

describe('moduleAliases', () => {
  it('follows an alias, because it silently changes every call site', () => {
    expect(moduleAliases('from actorkit import catalog as cat\n', 'catalog')).toEqual(['cat']);
    expect(workflowRefs('from actorkit import catalog as cat\nx = cat.actor("probe", "0.1.0")\n')).toEqual([
      { kind: 'actor', name: 'probe', version: '0.1.0' },
    ]);
  });

  it('picks one module out of a multi-name import', () => {
    expect(moduleAliases('from actorkit import catalog, fleet\n', 'fleet')).toEqual(['fleet']);
    expect(moduleAliases('from actorkit import catalog, fleet\n', 'catalog')).toEqual(['catalog']);
  });

  it('assumes the canonical spelling when no import is recognisable', () => {
    // Listing nothing at all would be worse: a file with `catalog.actor(...)` in it references an
    // actor whatever its imports look like.
    expect(moduleAliases('x = 1\n', 'catalog')).toEqual(['catalog']);
  });
});

describe('workflowRefs', () => {
  it('dedupes a name used twice and keeps source order', () => {
    const src =
      'from actorkit import catalog\n' +
      'a = catalog.dataset("targets")\n' +
      'b = catalog.actor("probe", "0.1.0")\n' +
      'c = catalog.dataset("targets")\n';
    expect(workflowRefs(src)).toEqual([
      { kind: 'dataset', name: 'targets' },
      { kind: 'actor', name: 'probe', version: '0.1.0' },
    ]);
  });

  it('does not read a keyword argument as a positional name', () => {
    // `tag="dns"` is not the actor's name, and reading it as one puts a thing that does not
    // exist on the checklist.
    expect(workflowRefs('catalog.actor(name="probe", version="0.1.0")\n')).toEqual([
      { kind: 'actor', name: 'probe', version: '0.1.0' },
    ]);
  });

  it('says nothing about a handle built from a variable', () => {
    // Invisible without running the file, and a wrong prerequisite is worse than a missing one.
    expect(workflowRefs('catalog.actor(cfg.name, cfg.version)\n')).toEqual([]);
  });
});

describe('which way a dataset goes', () => {
  it('marks a `.writer()` handle as an output', () => {
    expect(workflowRefs('catalog.dataset("out").writer()\n')).toEqual([
      { kind: 'dataset', name: 'out', writes: true },
    ]);
  });

  it('tolerates the whitespace an author actually writes', () => {
    expect(workflowRefs('catalog.dataset("out") .writer ()\n')[0]?.writes).toBe(true);
    expect(workflowRefs('async with catalog.dataset("out").writer() as w:\n')[0]?.writes).toBe(true);
  });

  it('leaves a plain handle unmarked — reading is the default, not a claim', () => {
    expect(workflowRefs('catalog.dataset("in")\n')).toEqual([{ kind: 'dataset', name: 'in' }]);
  });

  // Read from the text immediately after the closing paren, not from the line: two handles on one
  // line must not lend each other a direction.
  it('does not lend one handle another handle’s writer', () => {
    const refs = workflowRefs('a = catalog.dataset("in"); b = catalog.dataset("out").writer()\n');
    expect(refs.find((r) => r.name === 'in')?.writes).toBeUndefined();
    expect(refs.find((r) => r.name === 'out')?.writes).toBe(true);
  });

  // A name that appears both ways in one file is an OUTPUT: the run writes it, whatever else it
  // does, and the summary that matters must not depend on which line the scan reached first.
  it('keeps the writer reading when a name is used both ways', () => {
    const readFirst = 'catalog.dataset("both")\ncatalog.dataset("both").writer()\n';
    const writeFirst = 'catalog.dataset("both").writer()\ncatalog.dataset("both")\n';
    expect(workflowRefs(readFirst)).toEqual([{ kind: 'dataset', name: 'both', writes: true }]);
    expect(workflowRefs(writeFirst)).toEqual([{ kind: 'dataset', name: 'both', writes: true }]);
  });

  it('never marks an actor, whatever follows it', () => {
    expect(workflowRefs('catalog.actor("probe", "0.1.0").writer()\n')[0]?.writes).toBeUndefined();
  });
});

describe('one-hop string bindings', () => {
  it('follows a name bound to a literal', () => {
    expect(workflowRefs('out = "lame"\nx = catalog.dataset(out)\n')).toEqual([
      { kind: 'dataset', name: 'lame' },
    ]);
  });

  it('follows the `or`-default idiom every example writes its output name with', () => {
    expect(workflowRefs('out = req.get("into") or "lame"\nx = catalog.dataset(out)\n')).toEqual([
      { kind: 'dataset', name: 'lame' },
    ]);
  });

  it('REFUSES a name rebound later, rather than picking one of the two', () => {
    // Two answers and no way to tell which reaches the call. A wrong prerequisite sends an
    // operator looking for a Dataset that was never written.
    const src = 'out = "first"\nout = "second"\nx = catalog.dataset(out)\n';
    expect(workflowRefs(src)).toEqual([]);
    expect(stringBindings(src).has('out')).toBe(false);
  });

  it('refuses a name built from another name — one hop, not a resolver', () => {
    expect(workflowRefs('a = "x"\nb = a\nc = catalog.dataset(b)\n')).toEqual([]);
  });

  it('does not confuse a keyword argument for an assignment', () => {
    // `catalog.actor(name="probe")` is not a binding of `name`, and treating it as one would put
    // `probe` behind every later bare `name` in the file.
    expect(stringBindings('x = catalog.actor(name="probe")\n').has('name')).toBe(false);
  });
});
