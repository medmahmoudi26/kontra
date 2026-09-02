/**
 * Compile-time congruence guard for the generated boundary types — enforced by
 * `tsc --noEmit` (the CI `typecheck` step), NOT vitest (this is not a *.test.ts, so
 * it is never collected at runtime; TS types are erased and cannot be introspected
 * at runtime anyway). The Python peer is tests/test_workflows_client.py.
 *
 * WHAT IS STILL GUARDED HERE, now that the orchestrator no longer builds an `EntryInput`. The
 * dispatch is the CALLER'S (ADR 0023 §12) — Python writes the wire, the Go handler reads it, and
 * neither goes through this process. What this side still touches is the claim-check `BareRef`:
 * the codec encodes payloads into one and the materializer resolves a Method call's output
 * manifest by its `sha256`. So the guard is that a `BareRef` really is `{sha256, size, meta}` —
 * a proto change to those three fields must fail to compile here rather than surface as a
 * materialization that silently addresses nothing.
 */
import type { BareRef } from './_gen/kontra/v1/entry';

/** The ref shape the codec writes and the materializer reads (codec/claimCheck.ts). */
interface ClaimCheckRefShape {
  sha256: string;
  size: number;
  meta: Record<string, string>;
}

type AssertEqual<A, B> = [A] extends [B] ? ([B] extends [A] ? true : never) : never;
const _bareRefIsTheClaimCheckRef: AssertEqual<BareRef, ClaimCheckRefShape> = true;

void _bareRefIsTheClaimCheckRef;
