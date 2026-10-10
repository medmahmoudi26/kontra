/**
 * The namespace of the workflow that scheduled the activity running now (ADR 0051).
 *
 * An activity runs in its caller's namespace, and anything it does in Temporal on that caller's
 * behalf has to happen there too. A Lease held for a Fleet in `ws-hello`, or a liveness check of its
 * holders, read in the install's legacy namespace instead would find nothing and conclude the
 * holder is gone. The Fleet would then be torn down under a running Run. So this reads the
 * activity's own info, not the environment.
 *
 * Outside an activity, in tests and one-off scripts, it falls back to the legacy namespace, which is
 * what every caller read before isolation.
 */
import { Context } from '@temporalio/activity';

export function activityNamespace(): string {
  try {
    const ns = Context.current().info.workflowNamespace;
    if (ns) return ns;
  } catch {
    // Not inside an activity.
  }
  return (process.env.KONTRA_NAMESPACE ?? '').trim() || 'default';
}
