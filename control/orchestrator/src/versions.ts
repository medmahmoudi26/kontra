// MOVED TO @kontra/core. This re-export exists so the orchestrator's own imports did not have to
// change in the commit that extracted the shared kernel — see core/src/index.ts for what belongs
// there and why. Import from '@kontra/core' in new code; this file can go once nothing uses it.
export * from '@kontra/core/versions';
