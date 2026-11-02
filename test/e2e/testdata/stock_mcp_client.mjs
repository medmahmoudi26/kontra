// A stock MCP client calling the gate's own /mcp (test/e2e gate_mcp_stock_client, spec/relay.md §12.2; D324): the
// MCP TypeScript SDK, unmodified, loaded from an npm install of its pinned lock. Its Client lists the tools and
// validates every tool result's structuredContent against the tool's outputSchema (with its own Ajv), throwing when it
// does not match: so a call that answers is one whose structuredContent the stock validator accepted.
//
//   node stock_mcp_client.mjs <install dir> <v1|v2> <endpoint> < input.json
//
// v1 is @modelcontextprotocol/sdk 1.x on the 2025-11-25 era (initialize, a session); v2 is @modelcontextprotocol/client
// 2.x pinned to 2026-07-28 (server/discover, no session) and declaring URL and form elicitation without the tasks
// extension, as Claude Code does. The bearer is $KONTRA_E2E_MCP_KEY. input.json is {calls, check}: calls a list of
// {tool, arguments}, where an argument "$taskId" is the previous result's structuredContent.taskId; check {good, bad},
// values the client's own validator is asked about against hire's outputSchema (good ones it must accept, bad ones
// refuse: the control that the validator the calls went through is live). Each line of stdout is one JSON object:
// the tools listed, each call's {result} or {error, code}, then {check: {good: [valid…], bad: [valid…]}}.
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';

const [dir, sdk, endpoint] = process.argv.slice(2);
const load = (p) => import(pathToFileURL(join(dir, 'node_modules', p)).href);
let Client, Transport;
if (sdk === 'v2') {
  ({ Client, StreamableHTTPClientTransport: Transport } = await load('@modelcontextprotocol/client/dist/index.mjs'));
} else {
  ({ Client } = await load('@modelcontextprotocol/sdk/dist/esm/client/index.js'));
  ({ StreamableHTTPClientTransport: Transport } = await load('@modelcontextprotocol/sdk/dist/esm/client/streamableHttp.js'));
}
const out = (o) => process.stdout.write(JSON.stringify(o) + '\n');
const options = sdk === 'v2'
  ? { capabilities: { elicitation: { form: {}, url: {} } }, versionNegotiation: { mode: { pin: '2026-07-28' } } }
  : { capabilities: {} };
const client = new Client({ name: 'kontra-e2e-stock-client', version: '1.0.0' }, options);
const transport = new Transport(new URL(endpoint), {
  requestInit: { headers: { Authorization: 'Bearer ' + process.env.KONTRA_E2E_MCP_KEY } },
});
await client.connect(transport);
const listed = await client.listTools();
out({ tools: listed.tools.map((t) => ({ name: t.name, outputSchema: t.outputSchema ?? null })) });
const input = JSON.parse(readFileSync(0, 'utf8'));
let previous;
for (const call of input.calls) {
  const args = Object.fromEntries(Object.entries(call.arguments).map(([k, v]) => [k, v === '$taskId' ? previous?.structuredContent?.taskId : v]));
  try {
    previous = await client.callTool({ name: call.tool, arguments: args });
    out({ result: previous });
  } catch (e) {
    out({ error: String(e?.message ?? e), code: e?.code ?? null });
  }
}
const hire = listed.tools.find((t) => t.name === 'hire');
const validate = client._jsonSchemaValidator.getValidator(hire.outputSchema);
out({ check: { good: input.check.good.map((v) => validate(v).valid), bad: input.check.bad.map((v) => validate(v).valid) } });
await client.close();
