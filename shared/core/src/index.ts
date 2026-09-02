/**
 * @kontra/core — the shared kernel.
 *
 * WHAT BELONGS HERE, and it is a narrow rule: code that answers "how is a Run READ" and depends on
 * nothing but its own arguments. The orchestrator serves it; the console renders it; both must
 * agree, and a second implementation of any of it is a drift nobody would see.
 *
 * WHAT DOES NOT. Anything that reaches a filesystem, a Pulumi stack, a secret store or a Temporal
 * client. Those stay in the orchestrator — `panels/pollers.ts` and `actorControl.ts` are the two
 * that had a pure half worth extracting and a server half that had to stay, and each re-exports
 * what came here so their own callers did not have to move.
 *
 * WHY IT IS A PACKAGE RATHER THAN A DIRECTORY. The console lives in its own repository
 * (kontra-console) and used to reach these through a `@core/*` path alias into `control/orchestrator/src` —
 * which typechecked only on a machine that had the orchestrator's node_modules, and failed in CI
 * with eight `TS2307`s that no developer could reproduce.
 *
 * THE PACKAGE SHIPS `dist/`, NOT `src/`, AND CARRIES A `typesVersions` MAP. Two resolvers in the
 * chain are fussy in opposite directions: Temporal's Workflow bundler excludes node_modules from
 * its TypeScript loader, so a `.ts` entry point resolves and is then never transpiled; and the
 * orchestrator's `moduleResolution: "Node"` is node10, which does not read `exports` at all, so
 * `@kontra/core/panels/ids` has no types without `typesVersions`. Both failures are invisible to
 * `tsc --noEmit` on the package itself.
 *
 * `vocabulary` STAYS IN THIS REPOSITORY WHATEVER ELSE MOVES: its test reads
 * `cli/warden_workflow.go`'s BYTES to pin the Warden's five kind spellings, so a rename in Go is a
 * red test here. That contract cannot cross a repository boundary.
 */

export * from './vocabulary';
export * from './history';
export * from './transcript';
export * from './scratch';
export * from './versions';
export * from './compat';
export * from './contract/types';
export * from './contract/datasets';
export * from './queues';
export * from './secrets';
export * from './caller';
