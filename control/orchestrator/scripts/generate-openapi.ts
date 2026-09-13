/**
 * Write `docs/openapi.json` from the live route table.
 *
 * Run it after adding or moving a route; `openapi.test.ts` is what fails if you forget.
 */
import { mkdirSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';

import { buildOpenApi } from '../src/openapi';
import { buildServer } from '../src/server';

const out = join(__dirname, '..', '..', '..', 'docs', 'openapi.json');
const app = buildServer({});
const doc = buildOpenApi(app, process.env.KONTRA_VERSION ?? '0.1.0');
mkdirSync(dirname(out), { recursive: true });
writeFileSync(out, JSON.stringify(doc, null, 2) + '\n');
console.log(`${out} — ${Object.keys(doc.paths).length} paths`);
void app.close();
