/**
 * The one sentence every route module in this directory puts in an error body.
 *
 * IT LIVES HERE RATHER THAN IN `server.ts` BECAUSE THE ARROW ONLY POINTS ONE WAY. `server.ts`
 * imports every module in `routes/`; a module importing `errMessage` back out of it would close
 * that loop, and a CommonJS cycle between the file that builds the app and the files that fill it
 * resolves to `undefined` at exactly the moment a handler is trying to explain a failure. So the
 * helper sits below both, and `server.ts` imports it from here like everybody else.
 *
 * WHAT IT IS FOR, stated because a one-line function invites being inlined: every 502 on this API
 * is `{ error: '<what we were doing>: <what went wrong>' }`, and the second half is whatever the
 * thrown thing had to say. A raw `String(err)` on an `Error` prepends `Error: `, which is how a
 * message reads twice in a body an operator is trying to parse. `visibility.ts` carries its own
 * copy for its own logging; that one is not a route and deliberately not coupled to this.
 */
export function errMessage(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}
