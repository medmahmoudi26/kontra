/**
 * The ids of the handles an edge is drawn between.
 *
 * A HANDLE PER FIELD, which is v1's `canvas/handles.ts` decision arrived at again: it encoded a
 * field path into the handle id (`in:results`, `out:rows`) because an edge there was a schema
 * mapping and the store had to read the two field names back out of a connection. That is exactly
 * what an edge is here now — `fetch → title` is ambiguous about which of three fields carries the
 * page, and `fetch.body → title.html` is not, so the agent writing code from the drawing stops
 * having to infer it.
 *
 * AND A WHOLE-NODE HANDLE PER SIDE, which v1 did not have and which three ordinary states need: a
 * Method declaring neither `takes=` nor `emits=`, a workflow annotated `dict` (both the ones this
 * repo ships are), and a node whose Method has not been picked yet. A canvas that could not connect
 * those until their types were known would refuse the exact state people draw in.
 *
 * THE IDS ARE BUILT IN TWO PLACES AND MUST AGREE: `ScratchNodes.tsx` draws the handle, and
 * `scratchFlow.ts` names the handle each stored edge lands on. React Flow drops an edge whose named
 * handle does not exist — error 008, no line — so a second spelling is an edge that silently is not
 * there, which is why both sides call these functions rather than writing the strings.
 *
 * NOTHING HERE JUDGES AN EDGE. The typing is a drawing aid (ADR 0026): `scratchPorts.ts` says what
 * is odd about an edge and nothing anywhere refuses one. `isValidConnection` is deliberately not
 * wired — it is the one React Flow hook that would turn a thinking surface into a validator.
 */

import type { ScratchNode } from '../run/api';

/**
 * What a whole-node handle belongs to.
 *
 * ONE PER KIND, and a Dataset is no longer split in two. It used to be `dataset-read` and
 * `dataset-write`, because the author declared a `direction` and that decided which of the node's
 * two handles meant anything. The direction is now derived from which side edges land on
 * (`scratchFlow.ts:withDerivedDirections`), so a Dataset written by one step and read by the next is
 * an ordinary drawing rather than a node whose own kind contradicts one of its edges.
 */
export type ScratchHandleKind = 'actor' | 'workflow' | 'dataset';

export const handleKind = (node: ScratchNode): ScratchHandleKind => node.kind;

/** `in:` for the target side, `out:` for the source side. A kind never contains `:`, so the prefix
 *  parses unambiguously — and the ids are what React Flow puts in `data-handleid`, which is how a
 *  drawn edge names its ends in the DOM. */
export const inHandle = (kind: ScratchHandleKind): string => `in:${kind}`;
export const outHandle = (kind: ScratchHandleKind): string => `out:${kind}`;

/** The handle for one FIELD. `field:` is what tells the two apart, and no kind is spelled `field`,
 *  so `in:field:body` cannot collide with a whole-node id however a schema names its properties. */
export const fieldHandle = (side: 'in' | 'out', field: string): string => `${side}:field:${field}`;

/**
 * The field a handle id names, or nothing for a whole-node handle.
 *
 * SLICED, NOT SPLIT ON `:`. A JSON Schema property may be called anything at all — `a:b` is a legal
 * key — and splitting would hand back `a` for it, which is a field the node does not have and an
 * edge that lands nowhere.
 */
export function handleField(id: string | null | undefined): string | undefined {
  if (!id) return undefined;
  for (const prefix of ['in:field:', 'out:field:']) {
    if (id.startsWith(prefix)) return id.slice(prefix.length) || undefined;
  }
  return undefined;
}
