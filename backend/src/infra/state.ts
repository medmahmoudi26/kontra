/**
 * Reading Pulumi's DIY state, so the fleet is visible in a browser (ADR 0019).
 *
 * WHY THIS EXISTS AT ALL. Pulumi's own web console is not an option here, and the reason is
 * structural rather than a matter of effort: the console is a client of the Pulumi Cloud **API
 * service**, which is a licensed, self-hosted product backed by MySQL. It has no mode that
 * reads a `file://` backend. So the choice is between the console and the backend — and the
 * backend was chosen by measurement (see workspace.ts: DIY S3 on SeaweedFS silently stops
 * accepting writes; `file://` does not).
 *
 * What a self-managed backend gives instead is the checkpoint itself, which is a plain JSON
 * document and contains everything the console would show for a stack: every resource, its
 * type, its URN, its provider inputs, and the stack outputs. This module reads it. The engine
 * is not involved and nothing here can mutate a stack.
 *
 * The format is `version: 3` and is what `pulumi stack export` emits, which is the closest
 * thing to a contract the DIY backend has. {@link readStack} degrades to `null` rather than
 * throwing when a stack has never been converged, because "no fleet yet" is a normal answer.
 */

import { readdir, readFile, stat } from 'node:fs/promises';
import * as path from 'node:path';
// `./paths` and not `./workspace`: this module READS the checkpoint, it never runs an engine,
// and importing one to learn a directory put Pulumi in the API's module graph.
import { stateDir } from './paths';

/** One resource as the checkpoint records it — trimmed to what an operator reads. */
export interface StackResource {
  urn: string;
  type: string;
  /** The resource's own name, the tail of the URN. */
  name: string;
  /** The provider's id — a droplet number, a command's generated id. */
  id?: string;
  /** True for the synthetic Stack and provider resources, which have no cloud counterpart. */
  synthetic: boolean;
  created?: string;
  modified?: string;
  /** A few fields worth surfacing without dumping every provider input. */
  detail: Record<string, unknown>;
}

export interface StackState {
  fqn: string;
  project: string;
  stack: string;
  /** Pulumi engine version that last wrote the checkpoint. */
  version?: string;
  updated?: string;
  outputs: Record<string, unknown>;
  resources: StackResource[];
}

/** The fields worth showing per resource type. Everything else stays out: a provider's raw
 * inputs run to hundreds of keys and drown the handful that answer "what is this machine". */
const SHOWN = [
  'name',
  'region',
  'size',
  'image',
  'ipv4Address',
  'ipv4AddressPrivate',
  'status',
  'tags',
  'memory',
  'vcpus',
  'priceMonthly',
] as const;

function stacksRoot(): string {
  return path.join(stateDir(), '.pulumi', 'stacks');
}

/** Every stack the backend holds, as `project/stack`. */
export async function listStacks(): Promise<string[]> {
  const root = stacksRoot();
  const out: string[] = [];
  let projects: string[];
  try {
    projects = await readdir(root);
  } catch {
    return out; // nothing has ever been converged
  }
  for (const project of projects) {
    let files: string[];
    try {
      files = await readdir(path.join(root, project));
    } catch {
      continue;
    }
    for (const f of files) {
      // `.bak` and `.attrs` are the backend's own bookkeeping, not stacks.
      if (f.endsWith('.json')) out.push(`${project}/${f.slice(0, -'.json'.length)}`);
    }
  }
  return out.sort();
}

/** Read one stack's checkpoint. `null` when it has never been converged. */
export async function readStack(fqn: string): Promise<StackState | null> {
  const [project, stack] = fqn.split('/');
  if (!project || !stack) return null;
  const file = path.join(stacksRoot(), project, `${stack}.json`);

  let raw: string;
  let updated: string | undefined;
  try {
    raw = await readFile(file, 'utf8');
    updated = (await stat(file)).mtime.toISOString();
  } catch {
    return null;
  }

  const doc = JSON.parse(raw) as {
    checkpoint?: { latest?: { manifest?: { version?: string }; resources?: RawResource[] } };
  };
  const latest = doc.checkpoint?.latest;
  const resources = latest?.resources ?? [];

  const out: StackState = {
    fqn,
    project,
    stack,
    version: latest?.manifest?.version,
    updated,
    outputs: {},
    resources: [],
  };

  for (const r of resources) {
    // The synthetic Stack resource is where stack OUTPUTS live — they are not a separate
    // section of the document, which is easy to miss when reading the format.
    if (r.type === 'pulumi:pulumi:Stack') {
      out.outputs = r.outputs ?? {};
    }
    const synthetic = r.custom !== true;
    const detail: Record<string, unknown> = {};
    for (const k of SHOWN) {
      const v = (r.outputs ?? {})[k] ?? (r.inputs ?? {})[k];
      if (v !== undefined && v !== null && v !== '') detail[k] = v;
    }
    out.resources.push({
      urn: r.urn,
      type: r.type,
      name: r.urn.split('::').pop() ?? r.urn,
      id: r.id,
      synthetic,
      created: r.created,
      modified: r.modified,
      detail,
    });
  }
  return out;
}

interface RawResource {
  urn: string;
  type: string;
  id?: string;
  custom?: boolean;
  created?: string;
  modified?: string;
  inputs?: Record<string, unknown>;
  outputs?: Record<string, unknown>;
}
