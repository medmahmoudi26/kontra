/**
 * THE TEMPLATE OF A RUN NOTHING PINNED, found from its workflow type (issue: CLI-started runs).
 *
 * Only `POST /api/runs` pins `report.md`, because it is the one start path that knows the folder.
 * `kontra workflow start` dials Temporal directly, so its runs reached the renderer with nothing
 * pinned and got the DEFAULT report — the redditscan run's Scout document rendered as a generic run
 * summary, with a warning saying the template "could not be found afterwards either".
 *
 * It can be found. A run's workflow type is the `@workflow.defn` class, `workflow.json` names that
 * class beside every workflow folder, and the run's namespace says which workspace to look in. So
 * this looks for exactly one folder in that workspace declaring the type, and answers with its
 * `report.md`. Two folders declaring one type is ambiguous and answers nothing — a guess between two
 * documents is worse than the default, which at least says it is the default.
 *
 * THE CALLER PINS WHAT THIS FINDS, on first render, so a report does not change under a run that has
 * already been rendered once. That is later than "at start", and the difference is one edit window.
 */
import { existsSync, readdirSync, readFileSync } from 'node:fs';
import path from 'node:path';

import { ADDRESS_PREFIX, LEGACY_WORKSPACE, workspaceRoot, workspacesParent } from '../workspaces';

export interface FolderTemplate {
  /** The workflow folder the type was found in. */
  folder: string;
  /** `report.md`'s text, or undefined when the folder has none (the default report is right). */
  text?: string;
  /** The workspace's name, for the pin's record. */
  workspace: string;
}

/** The folder holding one namespace's workspace, or undefined when there is none on this host. */
export function workspaceDirFor(namespace: string, env: NodeJS.ProcessEnv = process.env): string | undefined {
  const parent = workspacesParent(env);
  if (namespace.startsWith(ADDRESS_PREFIX)) {
    return parent ? path.join(parent, namespace.slice(ADDRESS_PREFIX.length)) : undefined;
  }
  if (parent) return path.join(parent, LEGACY_WORKSPACE);
  // No named-workspace layout: the single tree is the legacy namespace's.
  return workspaceRoot(env) || undefined;
}

/** The one workflow folder in `namespace`'s workspace whose `workflow.json` declares `type`. */
export function folderTemplateFor(
  type: string,
  namespace: string,
  env: NodeJS.ProcessEnv = process.env
): FolderTemplate | undefined {
  if (!type) return undefined;
  const dir = workspaceDirFor(namespace, env);
  if (!dir) return undefined;
  const workflows = path.join(dir, 'workflows');
  let entries: string[];
  try {
    entries = readdirSync(workflows);
  } catch {
    return undefined;
  }
  const matches: string[] = [];
  for (const name of entries) {
    const folder = path.join(workflows, name);
    try {
      const manifest = JSON.parse(readFileSync(path.join(folder, 'workflow.json'), 'utf8')) as {
        workflow?: unknown;
      };
      if (typeof manifest.workflow === 'string' && manifest.workflow.trim() === type) matches.push(folder);
    } catch {
      // Not a workflow folder, or a manifest that does not parse: it declares nothing.
    }
  }
  if (matches.length !== 1) return undefined;
  const folder = matches[0]!;
  const report = path.join(folder, 'report.md');
  const workspace = path.basename(dir);
  return existsSync(report)
    ? { folder, text: readFileSync(report, 'utf8'), workspace }
    : { folder, workspace };
}
