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

/** A lake COLUMN, as a port. Never `required`: the lake reports a name and a DuckDB type, and
 *  nothing in a parquet schema says a caller has to fill it. */
const column = (name: string, type: string) => ({ name, type, required: false, declared: true });

/** A node with nothing on either side, and the ONE sentence that says which absence it is. */
const nothing = (why: string) => ({
  in: { declared: false, why, ports: [] },
  out: { declared: false, why, ports: [] },
});

/**
 * A Dataset's TWO SIDES ARE THE SAME COLUMNS, because they are the same table: writing into
 * `url` and reading out of `url` are one column seen from two ends. Giving each side its own
 * list would invite them to differ.
 */
const bothSides = (ports: ReturnType<typeof column>[]) => ({
  in: { declared: true, why: '', ports: [...ports] },
  out: { declared: true, why: '', ports: [...ports] },
});

const CRAWL_PAGES = [
  column('url', 'VARCHAR'),
  column('status', 'BIGINT'),
  column('title', 'VARCHAR'),
  column('body', 'STRUCT(body_len BIGINT, body_preview VARCHAR)'),
  column('fetched_at', 'TIMESTAMP'),
];

function datasetNode(id: string, name: string, direction: 'in' | 'out', x = 0, y = 0) {
  return {
    id,
    type: 'dataset',
    position: { x, y },
    data: { node: { id, kind: 'dataset', at: { x, y }, name, direction } },
  };
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
 * THE PORTS ARE THE REAL LAKE COLUMNS, with their real DuckDB types — not a shape somebody
 * typed into the palette. `body` is a `STRUCT(...)` two hundred characters wide and the node
 * is 216 pixels, so the label is elided to `STRUCT…` and the full type is in its `title`.
 *
 * And the direction is said in WORDS. Reading a Dataset and writing one are the same call up
 * to `.writer()`, which is the distinction that once put a thousand rows on a screen nothing
 * had written.
 */
export function WrittenTo() {
  return (
    <Canvas
      h={150}
      nodes={[datasetNode('n1', 'crawl_pages', 'out')]}
      ports={{ n1: bothSides(CRAWL_PAGES) }}
    />
  );
}

/**
 * The same node, drawn as READ FROM — the only difference is which side the author's edges
 * landed on. Same columns on both sides, because it is the same table either way.
 */
export function ReadFrom() {
  return (
    <Canvas
      h={140}
      nodes={[datasetNode('n1', 'dns_facts', 'in')]}
      ports={{
        n1: bothSides([
          column('host', 'VARCHAR'),
          column('addrs', 'VARCHAR[]'),
          column('cname', 'VARCHAR'),
          column('ok', 'BOOLEAN'),
        ]),
      }}
    />
  );
}

/**
 * DIRECTION IS DERIVED, NOT CHOSEN. The palette used to offer `IN` and `OUT` and the author
 * picked before drawing anything. Here `probe@0.2.0.fetch()` lands an edge on this node's
 * INPUT side, so `withDerivedDirections` reads it as written to and the node says so —
 * `url → url`, one column, not a guess about the whole table.
 */
export function DirectionIsDerived() {
  return (
    <Canvas
      h={210}
      nodes={[
        actorNode('n1', 'probe', '0.2.0', 'fetch', 0, 10),
        datasetNode('n2', 'crawl_pages', 'out', 330, 0),
      ]}
      edges={[
        {
          id: 'e1',
          source: 'n1',
          target: 'n2',
          sourceHandle: 'out:field:url',
          targetHandle: 'in:field:url',
        },
      ]}
      ports={{
        n1: {
          in: {
            declared: true,
            why: '',
            ports: [
              { name: 'url', type: 'string', required: false, declared: true },
              { name: 'host', type: 'string', required: false, declared: true },
            ],
          },
          out: {
            declared: true,
            why: '',
            ports: [
              { name: 'url', type: 'string', required: true, declared: true },
              { name: 'status', type: 'integer', required: true, declared: true },
              { name: 'body', type: 'string', required: true, declared: true },
            ],
          },
        },
        n2: bothSides(CRAWL_PAGES),
      }}
    />
  );
}

/**
 * BEFORE `/api/datasets/schema` ANSWERS. The lake listing polls and the column call is a
 * second question; until it comes back the page holds an empty array, and reading that as
 * "this Dataset has no columns" is a claim about the lake made without having looked.
 */
export function LakeHasNotAnsweredYet() {
  return (
    <Canvas
      nodes={[datasetNode('n1', 'crawl_pages', 'out')]}
      ports={{ n1: nothing('the lake has not answered with its columns yet') }}
    />
  );
}

/**
 * A Dataset nothing has written YET — the ordinary way an output is drawn, before the run
 * that creates it exists. The lake has answered and does not have this name, which is a
 * different sentence from the one above and the reason both are worth having.
 */
export function NothingHasWrittenThisName() {
  return (
    <Canvas
      nodes={[datasetNode('n1', 'lame', 'out')]}
      ports={{ n1: nothing('nothing in the lake has this name yet') }}
    />
  );
}
