import { afterEach, describe, expect, it } from 'vitest';
import { spawnSync } from 'node:child_process';
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { callerFor, serveActor } from './actorControl';
import type { Source } from './sources';

describe('serveActor', () => {
  const made: string[] = [];
  const prevBin = process.env.KONTRA_BIN;

  function folder(name: string): Source {
    const dir = mkdtempSync(path.join(os.tmpdir(), `kontra-serve-${name}-`));
    made.push(dir);
    mkdirSync(dir, { recursive: true });
    writeFileSync(path.join(dir, 'actor.json'), JSON.stringify({ name, version: '0.1.0' }));
    return {
      id: `actor:${name}:1f3k`,
      kind: 'actor',
      name,
      path: dir,
      version: '0.1.0',
      description: '',
      registeredAt: 1,
    };
  }

  afterEach(() => {
    if (prevBin === undefined) delete process.env.KONTRA_BIN;
    else process.env.KONTRA_BIN = prevBin;
    for (const dir of made.splice(0)) rmSync(dir, { recursive: true, force: true });
  });

  it('reports the session and the attach command on success', async () => {
    // `true` stands in for the CLI: this asserts the spawn/exit-0 path and what comes back from
    // it, not what `kontra serve --actor` does — that is the CLI's own tests' job.
    process.env.KONTRA_BIN = 'true';
    const got = await serveActor(folder('probe'));
    expect(got.session).toBe('probe-0_1_0');
    expect(got.attach).toBe('tmux attach -t probe-0_1_0');
    // The attach command must name the session the Monitor will find the pane under, or the
    // console and the terminal are two different workers.
    expect(got.attach).toContain(got.session);
  });

  it('spawns the verb the CLI actually has — `serve`, not the renamed `run`', async () => {
    // THE ARGV IS A CONTRACT ACROSS TWO LANGUAGES, and nothing in TypeScript checks it. `run`
    // became `serve` in cli/main.go, where the old word is a REDIRECT and not an alias — so this
    // array would have kept passing every test in this file and started failing the moment the CLI
    // was rebuilt, with `serve failed (exit 1)` and a sentence about a verb only this file said.
    const dir = mkdtempSync(path.join(os.tmpdir(), 'kontra-argv-'));
    made.push(dir);
    const log = path.join(dir, 'argv');
    const bin = path.join(dir, 'record');
    writeFileSync(bin, `#!/bin/sh\nprintf '%s\\n' "$@" > "${log}"\n`, { mode: 0o755 });
    process.env.KONTRA_BIN = bin;

    await serveActor(folder('probe'));

    const argv = readFileSync(log, 'utf8').trim().split('\n');
    expect(argv[0]).toBe('serve');
    // The rest of the line is the local-only contract this button is deliberately limited to: one
    // machine, in a session the Monitor can find.
    expect(argv).toEqual(['serve', '--actor', expect.any(String), '--mode', 'local', '--tmux']);
  });

  it('says a folder that has gone away is gone, rather than blaming the binary', async () => {
    // MEASURED: node reports a missing `cwd` as `spawn <bin> ENOENT` — the same errno and the same
    // sentence as a missing binary. A registration outlives its directory on purpose (a rename, a
    // `git checkout`, an unmounted volume), so this is an ordinary Tuesday and not a corner case,
    // and "is kontra on PATH?" sends the operator to look for the wrong thing.
    process.env.KONTRA_BIN = 'true';
    const gone = folder('probe');
    rmSync(gone.path, { recursive: true, force: true });
    await expect(serveActor(gone)).rejects.toThrow(/is not on this machine any more/);
    await expect(serveActor(gone)).rejects.toThrow(gone.path);
  });

  it('refuses an absent folder without asking the filesystem twice', async () => {
    // `SourceStore.list` already re-read the folder and marked the row; the flag is the fresher
    // fact, and a row drawn as `absent` that then served would make the badge a lie.
    const listed = { ...folder('probe'), absent: true };
    await expect(serveActor(listed)).rejects.toThrow(/is not on this machine any more/);
  });

  it('fails the serve when the CLI exits non-zero, carrying its last lines', async () => {
    // A worker that dies at import — a stray tab, a missing package — is exactly what this button
    // exists to make visible, and the evidence is the last thing the command printed.
    process.env.KONTRA_BIN = 'false';
    await expect(serveActor(folder('probe'))).rejects.toThrow(/serve failed \(exit 1\)/);
  });

  it('surfaces a missing binary as a fixable message, not a bare errno', async () => {
    process.env.KONTRA_BIN = 'kontra-does-not-exist-anywhere';
    await expect(serveActor(folder('probe'))).rejects.toThrow(/KONTRA_BIN/);
  });
});

describe('callerFor', () => {
  const source = callerFor('probe', '0.1.0', 'head', [{ url: 'https://a.test' }]);

  it('names the actor, the version and the Method in a real dispatch', () => {
    expect(source).toContain('catalog.actor("probe", "0.1.0")');
    // THE CALLABLE-HANDLE SPELLING, `handle.<method>(batch)` returning `(results, dropped)` (ADR
    // 0028 §4). This once read `probe.head(batch)` against a handle with NO `__getattr__` and every
    // file raised `AttributeError` in the worker; the interim fix `probe.dispatch(batch,
    // method="head")` has since been retired by the callable handle. The `read by Python` block
    // below EXECUTES this against the real SDK, so a spelling the parser accepts but the worker
    // rejects fails the test where it is cheap, not minutes later in a tmux pane.
    expect(source).toContain('results, dropped = await probe.head(batch)');
  });

  it('defines the class, and points `workflow start` at the FOLDER (not the class)', () => {
    // The @workflow.defn class is still what the worker registers and what `register --init` reads
    // into workflow.json — but `start` takes the FOLDER now (GitHub #15): the queue is derived from
    // the folder's content, never typed, so the docstring names the file, not the class.
    expect(source).toContain('class ProbeHead:');
    expect(source).toContain('kontra workflow start workflow.py');
    expect(source).not.toContain('--queue');
  });

  it('embeds the Batch as a module-level constant', () => {
    // At module level the JSON's own indentation is correct as printed. Inside the method body it
    // has to be re-indented to the statement's column, and the first version of this got that
    // wrong — it parsed, and looked like a mistake.
    expect(source).toContain('BATCH = [\n    {\n        "url": "https://a.test"');
    expect(source).toContain('batch = units if units is not None else BATCH');
  });

  it('reports the drop count the tuple return forces the caller to name', () => {
    // `(results, dropped)` is the forcing function (ADR 0028 §4): a caller cannot reach `results`
    // without binding `dropped`, so the generated `run` names both and folds the count into its
    // return — a run that dropped every Unit and one that found nothing are told apart here.
    expect(source).toContain('"out": len(results), "dropped": len(dropped)');
  });

  it('names an output Dataset with the positional second argument', () => {
    // Given a Dataset name, the caller opens its writer and hands it to the Method as the SECOND
    // positional argument (ADR 0028 §2) — not a keyword, not a `.publish()` in the loop — so the
    // rows land under a name a query can reach as they are pushed.
    const named = callerFor('probe', '0.1.0', 'head', [{ url: 'https://a.test' }], 'lame');
    expect(named).toContain('async with catalog.dataset("lame").writer() as out:');
    expect(named).toContain('results, dropped = await probe.head(batch, out)');
  });

  it('has no line indented in a way Python would reject', () => {
    // The real check is the one a Python parser makes, and it ran: all three shapes below were
    // written out and `ast.parse`d. This holds the property that check found.
    const body = source.split('\n').filter((l) => l.startsWith('    ') && l.trim() !== '');
    for (const line of body) {
      expect(line.length - line.trimStart().length).toBeGreaterThanOrEqual(4);
    }
  });

  it('says that it IS what the button runs, and when to own it instead', () => {
    /* THIS ASSERTION USED TO SAY THE OPPOSITE, and the reversal is the whole of ADR 0033. It
       pinned the sentence explaining why this file was code rather than a button — true while the
       page could only generate. The page calls the Method now, through a workflow that makes the
       SAME call on the SAME handle (§3), so a file still claiming the button does not exist would
       be the one place in the feature that contradicts it. What the file has to carry instead is
       the thing an operator cannot see from the page: that copying it is not a second way to do
       this, and that owning it is how you get the second call the probe refuses (§1). */
    expect(source).toContain('ADR 0033');
    expect(source).toContain('what that button runs');
    expect(source).toContain('nowhere to put a second Method');
  });

  it('makes an identifier out of an actor name that is not one', () => {
    const dashed = callerFor('crawl4ai-canon', '0.1.0', 'run', []);
    expect(dashed).toContain('crawl4ai_canon = catalog.actor("crawl4ai-canon", "0.1.0")');
    expect(dashed).toContain('results, dropped = await crawl4ai_canon.run(batch)');
    // `Crawl4aiCanon`, not `Crawl4AiCanon`: the split is on non-alphanumerics, so `crawl4ai` is
    // one part and only its first letter is raised. Ugly, legal, and stable.
    expect(dashed).toContain('class Crawl4aiCanonRun:');
  });

  it('calls a Method whose name Python cannot spell as an attribute', () => {
    // `core.Registry.AddMethod` in the Go SDK takes any non-empty string, so `dns-facts` is a
    // Method a worker really does self-register — and `probe.dns-facts(batch)` is a SyntaxError,
    // not a slightly-wrong call. `getattr` takes the name as a plain string and reaches the same
    // callable-handle path, so the tuple return is identical.
    const dashed = callerFor('probe', '0.1.0', 'dns-facts', []);
    expect(dashed).toContain('results, dropped = await getattr(probe, "dns-facts")(batch)');
    expect(dashed).toContain('class ProbeDnsFacts:');
    expect(dashed).not.toContain('probe.dns-facts');
  });

  it('puts every name in an ESCAPED literal, so a quote cannot become code', () => {
    // The actor's name and version are read off `actor.json` on the operator's disk, and the file
    // this writes is one they are about to run. A bare `"` in either used to close the string it
    // sat in and leave the rest of the line as source.
    const hostile = callerFor('pr"obe', '0.1\\0', 'head', []);
    expect(hostile).toContain('catalog.actor("pr\\"obe", "0.1\\\\0")');
  });

  it('keeps a leading-digit name a legal identifier and a legal class', () => {
    const numeric = callerFor('4chan', '1.0.0', 'scrape', []);
    expect(numeric).toContain('a_4chan = catalog.actor');
    // The `Call` prefix is what makes it legal — a class cannot start with a digit. The `c` stays
    // lowercase because raising `4` is a no-op, which is fine: legality is the requirement.
    expect(numeric).toContain('class Call4chanScrape:');
  });

  it('serves itself, so the generated file is runnable as written', () => {
    expect(source).toContain('catalog.serve([ProbeHead])');
  });

  it('names its two commands `kontra`, and only those two', () => {
    /* THE FILE CARRIES ITS OWN INSTRUCTIONS, and since ADR 0033 §6 that is the only place they
       live. The page used to read these lines back out of the docstring (`methodCall.ts:
       callerCommands`) to print them beside a save target; both went with the write-into-a-folder
       flow, because a file you copy carries its instructions with it and a page that retyped them
       was two strings to keep in step with a third. The shape is still a contract — the operator
       who copies this reads these two lines and nothing else tells them how to serve it. */
    const commands = source
      .split('\n')
      .map((l) => l.trim())
      .filter((l) => l.startsWith('kontra '));
    expect(commands).toHaveLength(2);
    // Both verbs take the FOLDER (`workflow.py`) and neither takes a `--queue` (GitHub #15).
    expect(commands[0]).toMatch(/^kontra workflow serve workflow\.py /);
    expect(commands[1]).toMatch(/^kontra workflow start workflow\.py /);
    expect(commands.join('\n')).not.toContain('--queue');
  });
});

/**
 * The generated file, RUN by Python against the real SDK — not parsed.
 *
 * PARSING IS NOT THE ACCEPTANCE TEST, RUNNING IS. This generator has shipped two files that
 * `ast.parse` called good and the worker then killed: `probe.head(batch)` against a handle with no
 * `__getattr__` (AttributeError, minutes later, in a tmux pane), and a Batch holding a boolean that
 * spelled `true` — a legal `ast.Name` that raises `NameError` at import. A check that only parses
 * cannot see either. So this block imports actorkit, execs the generated module (which is where a
 * bad literal dies), and DRIVES its workflow's `run` against the real `ActorHandle` — the one wire
 * seam, the Nexus dispatch, stubbed the way every SDK test stubs it, so the callable handle, the
 * `__getattr__` Method resolution, the `(results, dropped)` destructure and the named-Dataset
 * publish are all the real code paths. A spelling the parser accepts but the worker rejects fails
 * HERE.
 */
describe('the generated file, run by Python', () => {
  // THIS checkout's SDK seams, ahead of anything already on the path. They MUST be on PYTHONPATH
  // or `import kontra` resolves to the venv's editable install, which on a machine with a
  // leftover worktree points at STALE code — an `ActorHandle` from before the callable handle,
  // whose missing `__getattr__` makes every Method call raise, failing this test on the environment
  // rather than on the generated file. Two entries, not one: `sdk/python` carries `actorkit` and
  // `runtime/python` carries `internals`, which the SDK reaches at `serve()`. It used to be one —
  // the repo root — because the seam directory `actorkit/` was itself the package there.
  const repoRoot = path.resolve(fileURLToPath(import.meta.url), '../../..');
  const pyEnv = {
    ...process.env,
    PYTHONPATH: [
      path.join(repoRoot, 'sdk', 'python'),
      path.join(repoRoot, 'runtime', 'python'),
      path.join(repoRoot, 'sdk', 'python', '_gen'),
      process.env.PYTHONPATH,
    ]
      .filter(Boolean)
      .join(path.delimiter),
  };

  /**
   * The interpreter that can run the harness below. `python3` on PATH usually cannot — the SDK's
   * deps live in the repo's venv — so `KONTRA_PYTHON` is tried first, then the venv interpreter,
   * then bare `python3`, each probed under the same cwd and PYTHONPATH the run uses. A run with
   * none of them is a BROKEN CHECK, not a passing one, and says so: this repo ships a Python SDK,
   * so an interpreter that imports it exists wherever these tests are meant to run.
   *
   * THE PROBE IMPORTS temporalio TOO, AND IT HAS TO. `from kontra import catalog` used to double
   * as "the SDK's dependencies are installed here", and it no longer does — deliberately: the
   * sdk/runtime split makes `import kontra` provably free of temporalio, Redis and the object
   * store, asserted by tests/test_sdk_arrow.py. So a bare `python3` with only PYTHONPATH set now
   * PASSES the old probe and then dies inside the harness on `No module named 'temporalio'` — six
   * failures blaming the generated file for a missing dependency. A probe must ask for what the
   * thing it is selecting an interpreter for actually needs.
   */
  function pythonWithActorkit(): string {
    const candidates = [
      process.env.KONTRA_PYTHON,
      path.join(repoRoot, '.venv', 'bin', 'python'),
      'python3',
    ].filter((c): c is string => Boolean(c));
    for (const bin of candidates) {
      const probe = spawnSync(bin, ['-c', 'from kontra import catalog; import temporalio'], {
        cwd: repoRoot,
        env: pyEnv,
        encoding: 'utf8',
      });
      if (!probe.error && probe.status === 0) return bin;
    }
    throw new Error(
      'this check runs the generated caller against the real SDK and needs an interpreter with the ' +
        'SDK INSTALLED — `import kontra` alone is not enough, since that costs no temporalio by ' +
        'design. Set KONTRA_PYTHON, or `pip install -e ./sdk/python[dev]` into the repo venv.'
    );
  }

  /**
   * Exec the generated source as a module, then await its workflow's `run`. Returns what `run`
   * returned (its counts dict), or throws with exactly what Python said — an import-time `NameError`
   * from a bad literal, or a worker-time `AttributeError` from a spelling the handle cannot serve.
   *
   * The module is exec'd with `__name__` set (temporalio checks a workflow method's `__module__`),
   * and the one Nexus seam — `ActorHandle.dispatch_ref` — plus `workflow.execute_activity`/`info`
   * are stubbed, the same infrastructure-free stand-ins the SDK's own tests use, so no cluster is
   * needed to reach the real callable-handle path.
   */
  function pythonRuns(source: string): Record<string, unknown> {
    const harness = [
      'import sys, json, asyncio, inspect, datetime',
      'src = sys.stdin.read()',
      'from kontra import catalog',
      'from temporalio import workflow as _wf',
      'async def _fake_dispatch_ref(self, units, **kw):',
      '    n = units.n if isinstance(units, catalog.Batch) else len(list(units))',
      '    return {"sha256": "deadbeef", "size": 0, "meta": {"n": str(n), "done": "true"}}',
      'catalog.ActorHandle.dispatch_ref = _fake_dispatch_ref',
      'async def _fake_activity(name, arg, **kw):',
      '    if name == catalog.RESOLVE_BATCH_ACTIVITY: return {"ref": None}',
      '    if name == catalog.PUBLISH_BATCH_ACTIVITY: return {"rows": 0}',
      '    return []',
      '_wf.execute_activity = _fake_activity',
      'class _Info:',
      '    workflow_id = "wf-test"',
      '    start_time = datetime.datetime.now(datetime.timezone.utc)',
      '_wf.info = lambda: _Info()',
      'g = {"__name__": "generated_caller"}',
      'exec(compile(src, "generated_caller.py", "exec"), g)',   // a bad literal dies HERE
      'cls = next(v for v in g.values() if inspect.isclass(v)',
      '           and getattr(v, "__module__", "") == "generated_caller" and hasattr(v, "run"))',
      'out = asyncio.run(cls().run())',                          // a bad spelling dies HERE
      'json.dump(out, sys.stdout)',
    ].join('\n');
    // A FILE, NOT `python -c`. A worker imports a workflow from a file, and temporalio treats a
    // module run via `-c` as its own sandbox context — enough to make the real `ActorHandle`'s
    // `__getattr__` disappear and every Method call raise AttributeError, which would be this test
    // failing on an artefact of how it invokes Python rather than on the generated file. Run from a
    // file, the invocation matches a real `kontra workflow serve`.
    const dir = mkdtempSync(path.join(os.tmpdir(), 'kontra-caller-run-'));
    try {
      const harnessPath = path.join(dir, 'run_caller.py');
      writeFileSync(harnessPath, harness);
      const py = spawnSync(pythonWithActorkit(), [harnessPath], {
        input: source,
        cwd: repoRoot,
        env: pyEnv,
        encoding: 'utf8',
      });
      if (py.error) throw new Error(`could not run the generated caller: ${py.error.message}`);
      if (py.status !== 0) throw new Error(py.stderr.trim());
      return JSON.parse(py.stdout) as Record<string, unknown>;
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  }

  it('runs against the real SDK handle and reports the Batch it was handed', () => {
    const units = [{ url: 'https://a.test', depth: 2 }, { url: 'https://b.test', depth: 0 }];
    // The stubbed dispatch returns as many results as it was handed, so `out` echoes the Batch size
    // — which only happens if `probe.head(batch)` resolved on the real handle and the tuple bound.
    expect(pythonRuns(callerFor('probe', '0.1.0', 'head', units))).toEqual({
      units: 2,
      out: 2,
      dropped: 0,
    });
  });

  it('runs with a boolean, a null and a nested object in the Batch', () => {
    // MEASURED: `BATCH = [{"ok": true}]` parses (a bare `true` is an ast.Name) and dies at IMPORT
    // with `NameError: name 'true' is not defined`. Running the file is what catches it — a boolean
    // and a null are two form controls, and a nested object is the third shape, all in one Batch.
    const units = [{ ok: true, stale: false, seen: null, target: { host: 'a.test', ports: [80, 443] } }];
    expect(pythonRuns(callerFor('probe', '0.1.0', 'head', units))).toEqual({
      units: 1,
      out: 1,
      dropped: 0,
    });
  });

  it('runs the getattr form for a Method name Python cannot spell', () => {
    // `getattr(probe, "dns-facts")(batch)` is the reachable spelling for a Method the Go SDK
    // registers and Python cannot write as an attribute. Running it proves the getattr path builds
    // the same callable-handle call, not just that the string is a legal argument.
    expect(pythonRuns(callerFor('probe', '0.1.0', 'dns-facts', [{ a: 1 }]))).toEqual({
      units: 1,
      out: 1,
      dropped: 0,
    });
  });

  it('runs the named-Dataset form, opening a writer and publishing', () => {
    // The positional second argument (ADR 0028 §2) drives the writer's whole lifecycle — open,
    // publish, seal — through the real `DatasetWriter`, so a form that generated a broken `async
    // with` or passed the destination wrong fails here rather than at the first real publish.
    expect(pythonRuns(callerFor('probe', '0.1.0', 'head', [{ a: 1 }], 'lame'))).toEqual({
      units: 1,
      out: 1,
      dropped: 0,
    });
  });

  it('runs for an empty Batch, and for names that are not identifiers', () => {
    expect(pythonRuns(callerFor('crawl4ai-canon', '0.1.0', 'run', []))).toEqual({
      units: 0,
      out: 0,
      dropped: 0,
    });
    expect(pythonRuns(callerFor('4chan', '1.0.0', 'scrape', [{ a: 1 }]))).toEqual({
      units: 1,
      out: 1,
      dropped: 0,
    });
  });

  it('runs with the characters that end a Python string early in the Batch', () => {
    // A quote, a backslash, a newline and a non-ASCII byte, in one Unit. If `JSON.stringify`'s
    // escapes were not the ones Python reads, the module would die at exec — so a clean run is the
    // assertion.
    const units = [{ q: 'he said "hi"', p: 'C:\\tmp\\x', nl: 'one\ntwo', u: 'héllo — 世界' }];
    expect(pythonRuns(callerFor('probe', '0.1.0', 'head', units))).toEqual({
      units: 1,
      out: 1,
      dropped: 0,
    });
  });
});
