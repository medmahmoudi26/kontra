/**
 * `serveDevWorkflow` — the API asking the infra role to start a serve-dev Worker.
 *
 * THIS IS A CHANNEL, NOT A PROCESS. It exists because `kontra-api` has no Docker socket and must
 * not get one: it is the container with the published port and the untrusted HTTP input, and a
 * read-write socket there is root on the host. `kontra-infra` already holds the socket, publishes
 * no port, and is reachable only over the Compose network. The queue is how the two already talk
 * (`infraRoutes.ts:startStackOp` spends real money the same way), so serve-dev uses it rather than
 * growing a second listener on the one container that must not have one.
 *
 * ONE ATTEMPT, AND THAT IS DELIBERATE. The failures this reports — no such source, the folder is
 * gone, the worker died on boot with an import error — are not transient, and retrying an import
 * error three times only delays the sentence an operator needs to read. A serve is also not
 * idempotent in the way a converge is: `devDriver.Start` REPLACES the pair, so a retry after a
 * partial success would restart a Worker somebody is already using.
 *
 * THE WORKFLOW ID IS THE SOURCE ID, so pressing Serve twice on one folder cannot race two starts
 * against each other — the second is refused by Temporal rather than by a check somebody remembered
 * to write. `FAIL` and not `USE_EXISTING`: the caller wants an answer about the serve it asked for.
 */

import * as wf from '@temporalio/workflow';
import type * as activities from '../activities/serveDev';
import type { ServeDevInput, ServeDevResult } from '../activities/serveDev';

/**
 * Two minutes, no heartbeat.
 *
 * serve-dev returns as soon as the containers are created and confirmed alive — it does not wait
 * for the Worker to finish, because the Worker never finishes. What the budget has to cover is a
 * cold image pull on the first serve of an installation, which is why it is minutes and not
 * seconds. A heartbeat would be ceremony over a call that either returns quickly or is wrong.
 */
const { serveDev } = wf.proxyActivities<typeof activities>({
  startToCloseTimeout: '2 minutes',
  retry: { maximumAttempts: 1 },
});

export async function serveDevWorkflow(input: ServeDevInput): Promise<ServeDevResult> {
  return serveDev(input);
}
