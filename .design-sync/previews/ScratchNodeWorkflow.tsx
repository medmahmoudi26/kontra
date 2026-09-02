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

/** A node with nothing on either side, and the ONE sentence that says which absence it is. */
const nothing = (why: string) => ({
  in: { declared: false, why, ports: [] },
  out: { declared: false, why, ports: [] },
});

function workflowNode(id: string, file: string, x = 0, y = 0) {
  return {
    id,
    type: 'workflow',
    position: { x, y },
    data: { node: { id, kind: 'workflow', at: { x, y }, file } },
  };
}

function datasetNode(id: string, name: string, direction: 'in' | 'out', x = 0, y = 0) {
  return {
    id,
    type: 'dataset',
    position: { x, y },
    data: { node: { id, kind: 'dataset', at: { x, y }, name, direction } },
  };
}

/**
 * THE ORDINARY CASE, AND IT IS AN ABSENCE. A workflow node's ports come from the
 * `WorkflowDescriptor` a WORKER pushed when it served the file — so a file sitting in
 * `.kontra/workflows/` that nobody has served has told the catalog nothing, and the node
 * has no field to offer. Not a fault: it is what every workflow looks like before the
 * first `kontra workflow serve`, and the whole-node handles keep it connectable meanwhile.
 */
export function NoContractRegistered() {
  return (
    <Canvas
      nodes={[workflowNode('n1', 'nscheck')]}
      ports={{ n1: nothing('no worker has registered its contract') }}
    />
  );
}

/**
 * SERVED, REGISTERED, AND STILL NO FIELDS — a different fact from the one above, and the
 * one BOTH workflows this repo ships actually produce. `async def run(self, req: dict)`
 * derives `{"type": "object", "additionalProperties": true}`: a declared shape that names
 * nothing, i.e. anything at all fits. An empty field table would read as the opposite.
 */
export function AnnotatedDict() {
  return (
    <Canvas nodes={[workflowNode('n1', 'ping')]} ports={{ n1: nothing('declares no fields') }} />
  );
}

/**
 * A workflow whose author annotated a real model instead of `dict`, so the descriptor
 * carries a schema and the node draws a handle per field. This is what `sweep.py` takes
 * and returns today — a shard width in, a tally out — and typing the annotation is all it
 * costs to make each of those an edge somebody can land on.
 */
export function ContractRegistered() {
  return (
    <Canvas
      h={150}
      nodes={[workflowNode('n1', 'sweep.py')]}
      ports={{
        n1: {
          in: {
            declared: true,
            why: '',
            ports: [field('hosts', 'string[]'), field('shard', 'integer', false)],
          },
          out: {
            declared: true,
            why: '',
            ports: [
              field('hosts', 'integer'),
              field('waves', 'integer'),
              field('checked', 'integer'),
              field('still_dropped', 'integer'),
            ],
          },
        },
      }}
    />
  );
}

/**
 * A file may declare several `@workflow.defn` classes and each registers separately, so
 * two registered types can share one file's name. Picking either one's schemas here would
 * be a contract chosen by array order — the node says so instead and stays connectable.
 */
export function SeveralTypesShareTheName() {
  return (
    <Canvas
      nodes={[workflowNode('n1', 'dnssweep.py')]}
      ports={{ n1: nothing('several registered types share this name') }}
    />
  );
}

/**
 * The state a sketch is actually drawn in: a Dataset in the lake feeding a caller workflow
 * nobody has served yet. The Dataset offers its columns AND its whole-node handle; the
 * workflow can only offer the whole node — so the edge lands whole, which is how
 * "these two are connected, I have not said how yet" stays sayable.
 */
export function ReadingADatasetWholeNode() {
  return (
    <Canvas
      h={210}
      nodes={[datasetNode('n1', 'targets', 'in', 0, 0), workflowNode('n2', 'dnssweep.py', 330, 30)]}
      edges={[
        {
          id: 'e1',
          source: 'n1',
          target: 'n2',
          sourceHandle: 'out:dataset',
          targetHandle: 'in:workflow',
        },
      ]}
      ports={{
        n1: {
          in: {
            declared: true,
            why: '',
            ports: [field('host', 'VARCHAR', false), field('source', 'VARCHAR', false)],
          },
          out: {
            declared: true,
            why: '',
            ports: [field('host', 'VARCHAR', false), field('source', 'VARCHAR', false)],
          },
        },
        n2: nothing('no worker has registered its contract'),
      }}
    />
  );
}
