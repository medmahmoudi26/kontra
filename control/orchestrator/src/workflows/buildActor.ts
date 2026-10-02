/**
 * `buildActorWorkflow` — the API asking the infra role to build an actor image.
 *
 * THIS IS A CHANNEL, NOT A PROCESS, for the same reason `serveDevWorkflow` is: `kontra-api` has no
 * Docker socket and must not get one, and `kontra deploy` drives `docker build` and `docker push`.
 * The queue is how the two containers already talk, so the build uses it rather than growing a
 * second HTTP listener on the one container that must not have a port.
 *
 * ONE ATTEMPT. A build fails on a compile error, a missing dependency, an unreachable registry or a
 * full disk — none of which heal on retry, and all of which are sentences an operator needs to read
 * now rather than in three timeouts' time. The push is also not free to repeat.
 */

import * as wf from '@temporalio/workflow';
import type * as activities from '../activities/buildActor';
import type { BuildActorInput, BuildActorResult } from '../activities/buildActor';

/**
 * Thirty minutes, no heartbeat.
 *
 * SERVE-DEV'S TWO MINUTES IS THE WRONG BUDGET HERE and it would fail the largest actors only. A
 * build compiles the Go handler and runs the actor's own `deploy.sh`: measured on this install,
 * `crawl4ai` and `reddit` are 2.1–2.3 GB images because they install Playwright and Chromium, and
 * a cold one of those is comfortably past ten minutes. A timeout that kills a build at minute two
 * reports "build failed" for a build that was working, which is the most expensive wrong sentence
 * this surface can produce.
 *
 * No heartbeat because the activity is a single spawn that either returns or is killed by this
 * budget — a heartbeat would be ceremony over a call with nothing to report between its ends. The
 * CLI's own output is the progress, and it comes back in `detail`.
 */
const { buildActor } = wf.proxyActivities<typeof activities>({
  startToCloseTimeout: '30 minutes',
  retry: { maximumAttempts: 1 },
});

export async function buildActorWorkflow(input: BuildActorInput): Promise<BuildActorResult> {
  return buildActor(input);
}
