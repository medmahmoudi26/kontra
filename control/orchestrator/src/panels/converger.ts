/**
 * Starting the session converge from the streamer (ADR 0020).
 *
 * The streamer can OFFER a converge because a Terminal that cannot attach is the drift detector,
 * and "no session — converge" has to be an action rather than a dead end. What it may not do is
 * decide what the converge contains: the Machine, the session name and the windows are all built
 * from the Fleet inventory server-side, and the only thing a browser contributes is which Machine.
 * That is the same rule `machine.ts` states about its install script — the caller picks WHICH, never
 * WHAT.
 *
 * A DELIBERATELY SEPARATE, DEFAULT-CONVERTER CLIENT. `temporalClient.ts` builds a client around the
 * claim-check data converter and pulls in the object store, DuckDB blob helpers and OTel with it.
 * The infra worker runs with the DEFAULT converter (see `infra.ts`), the payload here is four short
 * strings, and this process should not import a query engine to start a workflow.
 */

import { Client, Connection } from '@temporalio/client';
import { tmuxWorkflowId } from './ids';
import { DEFAULT_WINDOWS } from './converge';
import type { MachineTarget } from './discovery';
import { sshAddress } from './discovery';

/** The workflow's registered type name — its exported function name in `workflows/infra.ts`. A
 * string, not an import: the streamer must not pull workflow code into its bundle. */
const TMUX_SESSION_WORKFLOW = 'tmuxSessionWorkflow';

export interface Converger {
  converge(machine: MachineTarget): Promise<{ workflowId: string }>;
  close(): Promise<void>;
}

/**
 * A lazily-connected converger.
 *
 * Lazy because the streamer must boot and serve `health` with no Temporal running — the same
 * property `temporalClient.ts` keeps for the API. A dial failure surfaces on the converge that
 * needed it, as a message on that tile, and never as a dead wall.
 */
export function temporalConverger(options?: {
  address?: string;
  namespace?: string;
  queue?: string;
}): Converger {
  const address = options?.address ?? process.env.KONTRA_ADDRESS ?? 'localhost:7233';
  const namespace = options?.namespace ?? process.env.KONTRA_NAMESPACE ?? 'default';
  const taskQueue = options?.queue ?? process.env.KONTRA_INFRA_QUEUE ?? 'kontra-infra';

  let clientPromise: Promise<Client> | null = null;
  let connection: Connection | null = null;

  const client = async (): Promise<Client> => {
    if (!clientPromise) {
      clientPromise = (async () => {
        const conn = await Connection.connect({ address });
        connection = conn;
        return new Client({ connection: conn, namespace });
      })().catch((err: unknown) => {
        // Do not memoise a failure: a Temporal that was down when the first tile asked must not
        // stay "down" for the life of the process.
        clientPromise = null;
        throw err;
      });
    }
    return clientPromise;
  };

  return {
    async converge(machine: MachineTarget): Promise<{ workflowId: string }> {
      const c = await client();
      const workflowId = tmuxWorkflowId(machine.machine);
      try {
        await c.workflow.start(TMUX_SESSION_WORKFLOW, {
          taskQueue,
          workflowId,
          // One writer per Machine, structurally. A second concurrent converge would race on
          // `has-session` and produce duplicate windows.
          workflowIdConflictPolicy: 'FAIL',
          args: [
            {
              machine: machine.machine,
              host: sshAddress(machine),
              // Half of this Machine's identity for the transport's known-hosts file — see
              // `panels/ssh.ts`. Without it a converge dials the recycled VPC address alone.
              publicIp: machine.publicIp,
              session: machine.session,
              windows: [...DEFAULT_WINDOWS],
            },
          ],
        });
      } catch (err) {
        // An already-running converge is the desired state, not a failure to report as one.
        if (!isAlreadyStarted(err)) throw err;
      }
      return { workflowId };
    },
    async close(): Promise<void> {
      const conn = connection;
      connection = null;
      clientPromise = null;
      if (conn) await conn.close().catch(() => undefined);
    },
  };
}

function isAlreadyStarted(err: unknown): boolean {
  const name = (err as { name?: string } | null)?.name ?? '';
  const message = String((err as { message?: string } | null)?.message ?? '');
  return (
    name === 'WorkflowExecutionAlreadyStartedError' ||
    /already started|already running/i.test(message)
  );
}
