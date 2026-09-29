/**
 * Reading ONE fact out of `docker-compose.yml`: which host ports a service publishes.
 *
 * ── WHY THIS IS CODE AND NOT A COMMENT ──────────────────────────────────────────────────────────
 *
 * Two services in this stack are reachable with NO credential of their own and are contained by
 * exactly one thing: they publish no host port, so only the compose network can reach them, with
 * the orchestrator as the authenticated front door. `porter` executes arbitrary SQL over every
 * workspace's data and `porter serve` has no flag that is a credential — twelve of them, and not
 * one is auth, TLS, or a sandbox control.
 *
 * That containment is a real boundary and it is also ONE ABSENT LINE. A single `ports:` entry
 * added while debugging — in good faith, in thirty seconds, by someone who wanted to point a
 * client at it — converts an unauthenticated SQL engine into a listener on the host network,
 * which is precisely what this file's own header says loopback exists to prevent. A comment
 * saying "do not add this" is read by whoever is already being careful.
 *
 * ── A DELIBERATELY SMALL PARSER ─────────────────────────────────────────────────────────────────
 *
 * No YAML dependency, because adding one to the runtime graph to police a boundary would be its
 * own kind of cost, and because the shape read here is fixed: compose services are a two-space
 * mapping under `services:`, and `ports:` is a four-space key inside one. What it must NOT do is
 * match the word in prose — this compose file discusses ports at length, including in the comment
 * above the very service this guards — so comments are stripped before anything is matched.
 *
 * It is strict about what it does not understand: a service it cannot find is an error rather
 * than an empty answer, because "no ports" and "no such service" must never read alike. A rename
 * then fails the test loudly instead of passing it vacuously, which is the only way a guard like
 * this stays true after the thing it guards moves.
 */

/** A service block's lines, comments and blanks removed, with indentation intact. */
export function composeService(yaml: string, service: string): string[] {
  const lines = yaml.split('\n');
  const head = new RegExp(`^ {2}${service.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}:\\s*$`);
  const start = lines.findIndex((l) => head.test(l));
  if (start === -1) {
    throw new Error(
      `docker-compose.yml has no service "${service}" — if it was renamed, this guard has to be ` +
        'pointed at the new name, because a guard that silently matches nothing passes forever'
    );
  }

  const out: string[] = [];
  for (const raw of lines.slice(start + 1)) {
    const line = raw.replace(/\r$/, '');
    if (line.trim() === '') continue;
    // A comment can say anything, including the name of the key this is looking for.
    if (/^\s*#/.test(line)) continue;
    // Back at two-space indent: the next service, or the end of `services:`.
    if (!/^ {3}/.test(line)) break;
    out.push(line);
  }
  return out;
}

/**
 * The host ports a service publishes, as written. `[]` means it publishes none, which is the
 * answer that matters here.
 *
 * Both spellings compose accepts are read — the block sequence and the inline flow list — so a
 * published port cannot hide behind a change of style.
 */
export function publishedPorts(yaml: string, service: string): string[] {
  const body = composeService(yaml, service);
  const out: string[] = [];
  for (let i = 0; i < body.length; i += 1) {
    const m = /^ {4}ports:\s*(.*)$/.exec(body[i]!);
    if (!m) continue;

    const inline = m[1]!.trim();
    if (inline !== '') {
      // `ports: ["8080:8080"]` / `ports: [8080]`
      out.push(
        ...inline
          .replace(/^\[|\]$/g, '')
          .split(',')
          .map((s) => s.trim().replace(/^['"]|['"]$/g, ''))
          .filter((s) => s !== '')
      );
      continue;
    }
    // A block sequence, or the long form with `target:`/`published:` keys under it.
    for (let j = i + 1; j < body.length; j += 1) {
      const item = body[j]!;
      if (!/^ {6}/.test(item)) break;
      const seq = /^ {6}-\s*(.+)$/.exec(item);
      if (seq) out.push(seq[1]!.trim().replace(/^['"]|['"]$/g, ''));
      const pub = /^ {8}published:\s*(.+)$/.exec(item);
      if (pub) out.push(pub[1]!.trim().replace(/^['"]|['"]$/g, ''));
    }
  }
  return out;
}
