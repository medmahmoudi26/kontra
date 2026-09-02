import { Markdown } from '@kontra/frontend';

function Frame({ children }: { children: React.ReactNode }) {
  return (
    <div className="dark">
      <div className="bg-background text-foreground rounded-lg p-4">{children}</div>
    </div>
  );
}

/** The detail a tile shows for one worker: a definition table, identifiers in monospace. */
export function TerminalDetail() {
  return (
    <Frame>
      <Markdown
        source={[
          '### subfinder-02',
          '',
          '| field | value |',
          '| --- | --- |',
          '| actor | `subfinder@v0.3.1` |',
          '| queue | `kontra-subfinder` |',
          '| machine | `10.124.0.7` |',
          '| session | `kontra-subfinder:0` |',
          '| digest | `sha256:9f2c1ab…4e77` |',
          '',
          'Attach with:',
          '',
          '```sh',
          'ssh root@10.124.0.7 -t tmux attach -t kontra-subfinder',
          '```',
        ].join('\n')}
      />
    </Frame>
  );
}

/** A Run summary — the shape an operator reads after a dispatch settles. */
export function RunSummary() {
  return (
    <Frame>
      <Markdown
        source={[
          '## Run `run_01J8XV9B7TM`',
          '',
          '**37,412** units committed of **37,412** · **0** isolated · **4** dropped',
          '',
          '| stage | actor | method | in | out |',
          '| --- | --- | --- | --- | --- |',
          '| 1 | `bbscope` | `programs` | 1 | 119 |',
          '| 2 | `subfinder` | `enumerate` | 119 | 37,412 |',
          '| 3 | `httpx` | `probe` | 37,412 | 12,880 |',
          '',
          '> Dropped units are not a rounding error: four batches were isolated after a',
          '> worker died mid-flight and their inputs were never re-queued.',
        ].join('\n')}
      />
    </Frame>
  );
}

/** Prose, lists and inline code — the typography path, on the real font stack. */
export function ProseAndLists() {
  return (
    <Frame>
      <Markdown
        source={[
          '# Method `enumerate`',
          '',
          'Resolves every subdomain for one apex and emits one row per host. A Batch in,',
          'a Batch out — the caller owns the loop and this Method never sees the whole Dataset.',
          '',
          '**takes**',
          '',
          '- `apex` — the registrable domain, e.g. `example.com`',
          '- `sources` — optional list of resolvers; defaults to the actor’s compiled-in set',
          '',
          '**emits**',
          '',
          '1. `host` — the fully-qualified name',
          '2. `source` — which resolver first reported it',
          '3. `first_seen` — epoch millis, set at emit time',
          '',
          'Output is durable at emit time, so the Dataset is queryable *while the Run is',
          'still going*.',
        ].join('\n')}
      />
    </Frame>
  );
}

/** A fenced code block: the generated caller the playground hands over. */
export function CodeBlock() {
  return (
    <Frame>
      <Markdown
        source={[
          'The composed chain, as a caller:',
          '',
          '```python',
          'from kontra import workflow, Dataset',
          '',
          '@workflow',
          'async def enumerate_scope(ctx):',
          '    apexes = Dataset("scope.apexes").pages(size=200)',
          '    async for batch in apexes:',
          '        hosts = await ctx.call("subfinder", "enumerate", batch)',
          '        await ctx.call("httpx", "probe", hosts)',
          '```',
        ].join('\n')}
      />
    </Frame>
  );
}
