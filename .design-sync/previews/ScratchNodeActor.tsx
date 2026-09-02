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

const field = (name: string, type: string, required = true, declared = true) => ({
  name,
  type,
  required,
  declared,
});

function actorNode(id: string, actor: string, version: string, method: string, x = 0, y = 0) {
  return {
    id,
    type: 'actor',
    position: { x, y },
    data: { node: { id, kind: 'actor', at: { x, y }, actor, version, method } },
  };
}

/**
 * `probe@0.2.0.fetch()` — the ordinary case. THE PORTS ARE THE SIGNATURE OF THE METHOD
 * THIS NODE SELECTED: `fetch` takes a URL and emits a body, so those are the dots an edge
 * can land on. Switching the Method in the inspector reshapes them, because it changes
 * what the step takes and emits.
 */
export function MethodSignature() {
  return (
    <Canvas
      nodes={[actorNode('n1', 'probe', '0.2.0', 'fetch')]}
      ports={{
        n1: {
          in: { declared: true, why: '', ports: [field('url', 'string'), field('cap', 'integer', false)] },
          out: {
            declared: true,
            why: '',
            ports: [field('body', 'string'), field('truncated', 'boolean', false)],
          },
        },
      }}
    />
  );
}

/**
 * An edge attaches one output FIELD to one input field. `fetch → title` cannot say which
 * value carries the page; `fetch.body → title.html` can, and the agent writing code from
 * the drawing stops having to infer it.
 */
export function FieldToField() {
  return (
    <Canvas
      h={210}
      nodes={[
        actorNode('n1', 'probe', '0.2.0', 'fetch', 0, 0),
        actorNode('n2', 'extract', '0.1.0', 'title', 320, 20),
      ]}
      edges={[
        {
          id: 'e1',
          source: 'n1',
          target: 'n2',
          sourceHandle: 'out:field:body',
          targetHandle: 'in:field:html',
        },
      ]}
      ports={{
        n1: {
          in: { declared: true, why: '', ports: [field('url', 'string')] },
          out: { declared: true, why: '', ports: [field('body', 'string')] },
        },
        n2: {
          in: { declared: true, why: '', ports: [field('html', 'string')] },
          out: { declared: true, why: '', ports: [field('title', 'string'), field('lang', 'string', false)] },
        },
      }}
    />
  );
}

/**
 * A node whose Method is not picked yet. NOT AN ERROR — it is the state people draw in,
 * and the whole-node handle on each side is how "these two are connected, I have not said
 * how yet" stays sayable. The sentence says which of the three ordinary reasons it is.
 */
export function NoMethodChosenYet() {
  return (
    <Canvas
      nodes={[actorNode('n1', 'probe', '0.2.0', '')]}
      ports={{
        n1: {
          in: { declared: false, why: 'no Method chosen yet', ports: [] },
          out: { declared: false, why: 'no Method chosen yet', ports: [] },
        },
      }}
    />
  );
}

/**
 * A Method that declares neither `takes=` nor `emits=` — `examples/go/nscheck` is one.
 * An empty field table would be indistinguishable from a Method that takes nothing, so
 * the absence is drawn as absence and the node stays connectable whole.
 */
export function DeclaresNoFields() {
  return (
    <Canvas
      nodes={[actorNode('n1', 'nscheck', '0.1.0', 'lame')]}
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
 * A field STRANDED by a Method change: the author's edge names `body`, and the Method
 * they switched to does not. The port comes back dashed and amber rather than
 * disappearing — React Flow drops an edge whose handle is gone, so vanishing would take
 * the line off the canvas while leaving it in the document. Mark, never refuse.
 */
export function StrandedByAMethodChange() {
  return (
    <Canvas
      nodes={[actorNode('n1', 'probe', '0.2.0', 'head')]}
      ports={{
        n1: {
          in: { declared: true, why: '', ports: [field('url', 'string')] },
          out: {
            declared: true,
            why: '',
            ports: [
              field('status', 'integer'),
              field('headers', 'object'),
              field('body', 'string', false, false),
            ],
          },
        },
      }}
    />
  );
}
