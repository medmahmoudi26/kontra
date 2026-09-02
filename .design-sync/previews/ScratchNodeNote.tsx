import { ReactFlow, ScratchPortsProvider, scratchNodeTypes } from '@kontra/frontend';

/**
 * A Scratch node only renders truthfully INSIDE a canvas: every port is a React Flow
 * `Handle`, and a Handle reads the flow store through context to place itself on the
 * node's border. Rendered bare it would stack its ports at the origin, so every cell
 * here is a real (static) canvas holding the real node component.
 */
function Canvas({
  nodes,
  ports,
  edges = [],
  h = 128,
}: {
  nodes: unknown[];
  ports: Record<string, unknown>;
  edges?: unknown[];
  h?: number;
}) {
  return (
    <div className="dark">
      <div className="rounded-lg bg-background p-3 text-foreground">
        <div style={{ width: '100%', height: h }}>
          <ScratchPortsProvider value={new Map(Object.entries(ports))}>
            <ReactFlow
              nodes={nodes}
              edges={edges}
              nodeTypes={scratchNodeTypes}
              fitView
              /* Padding is a fraction of the VIEWPORT, so 0.25 spends half the card on
                 margin and fitView zooms the node down to a sixth of its real size. A
                 node has to be read at roughly 1:1 or the ports are decoration. */
              fitViewOptions={{ padding: 0.08, maxZoom: 1, minZoom: 0.9 }}
              nodesDraggable={false}
              nodesConnectable={false}
              panOnDrag={false}
              zoomOnScroll={false}
              proOptions={{ hideAttribution: true }}
            />
          </ScratchPortsProvider>
        </div>
      </div>
    </div>
  );
}

/** `noteSlot` puts notes 200 apart in a row of their own, because a note is 188 wide and a
 *  26px cascade buried each one's text under the next. Laid out the same way here. */
function noteNode(id: string, text: string, x = 0, y = 0) {
  return { id, type: 'note', position: { x, y }, data: { note: { id, at: { x, y }, text } } };
}

function actorNode(id: string, actor: string, version: string, method: string, x = 0, y = 0) {
  return {
    id,
    type: 'actor',
    position: { x, y },
    data: { node: { id, kind: 'actor', at: { x, y }, actor, version, method } },
  };
}

/**
 * WHAT NO SCHEMA HOLDS. A note carries the paging width, the failure policy and the reason —
 * the parts of a campaign that live in a caller's head and in no catalog entry — and they go
 * to the agent VERBATIM, so what is typed is what is sent, with no formatting step in between
 * to be surprised by.
 */
export function AFreeNote() {
  return (
    <Canvas
      h={118}
      ports={{}}
      nodes={[noteNode('k1', 're-page delegation’s output before ask sees it.')]}
    />
  );
}

/**
 * NO HANDLES, DELIBERATELY. The Actor beside it draws a dot per declared field and one per
 * side for the whole node; the note draws none. The server's parse builds an edge's endpoints
 * out of NODE ids only, so an edge drawn to a note is dropped on the way in — a handle here
 * would offer a connection that silently does not survive the save.
 */
export function NoHandlesByDesign() {
  return (
    <Canvas
      h={190}
      nodes={[
        actorNode('n1', 'nscheck', '0.1.0', 'ask', 0, 0),
        noteNode('k1', 'ask emits a row per (domain, ns) — failures included.', 300, 6),
      ]}
      ports={{
        n1: {
          in: { declared: false, why: 'this Method declares no fields', ports: [] },
          out: { declared: false, why: 'this Method declares no fields', ports: [] },
        },
      }}
    />
  );
}

/**
 * WHERE NOTES LAND, and why the geometry is not arbitrary. `noteSlot` puts them in a row of
 * their own BELOW the node grid, four across at 200px — both mistakes were made getting here:
 * the first note covered a node, then a 26px cascade buried each note's text under the next.
 * A note is 188 wide, so 200 apart leaves every one of them readable at once.
 */
export function NotesLandInTheirOwnRow() {
  return (
    <Canvas
      h={130}
      ports={{}}
      nodes={[
        noteNode('k1', 'a refusing NS is a finding you push, not a drop.', 0, 0),
        noteNode('k2', 'shard 25 — past the measured 200/1000 guard.', 200, 0),
        noteNode('k3', 'one wave per shard; retry the drops singly.', 400, 0),
      ]}
    />
  );
}

/**
 * A note the moment it is placed, beside one that has been written. Empty is the state every
 * note starts in — it is created by the toolbar and typed into on the canvas, so a placeholder
 * pretending to content would be the first thing an author had to delete.
 */
export function JustPlaced() {
  return (
    <Canvas
      h={130}
      ports={{}}
      nodes={[
        noteNode('k1', 'targets is the operator’s list. Never sealed here.', 0, 0),
        noteNode('k2', '', 210, 0),
      ]}
    />
  );
}
