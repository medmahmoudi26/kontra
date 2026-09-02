/**
 * The local Pulumi dashboard, served at `/infra`.
 *
 * A single self-contained page rather than a route in the React SPA, for one reason: the SPA
 * bakes its bearer token at BUILD time, and the infra routes deliberately take a different,
 * narrower token than the workbench (see infraRoutes.ts). Baking the credential that can spend
 * money into a JS bundle would undo that split. The page asks for the token, keeps it in
 * localStorage, and sends it per request.
 *
 * It reads only. Converging is `kontra fleet`, which goes through Temporal so the operation has
 * an id, a retry policy and a record — none of which a browser button would have.
 */

export const INFRA_DASHBOARD_HTML = `<!doctype html>
<html lang="en"><head>
<meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>kontra — fleet state (Pulumi)</title>
<style>
  :root { color-scheme: light dark;
    --bg:#0f1115; --fg:#e6e6e6; --dim:#8b93a1; --line:#262b36; --card:#161a22; --accent:#7aa2f7; }
  @media (prefers-color-scheme: light) {
    :root { --bg:#fbfbfd; --fg:#1a1d23; --dim:#606875; --line:#e2e5ea; --card:#fff; --accent:#2f5fd0; }
  }
  * { box-sizing: border-box; }
  body { margin:0; background:var(--bg); color:var(--fg);
    font:14px/1.5 ui-monospace,SFMono-Regular,Menlo,monospace; }
  header { padding:16px 20px; border-bottom:1px solid var(--line); display:flex;
    gap:16px; align-items:baseline; flex-wrap:wrap; }
  h1 { font-size:15px; margin:0; font-weight:600; letter-spacing:.02em; }
  .dim { color:var(--dim); }
  main { padding:20px; max-width:1100px; }
  .card { background:var(--card); border:1px solid var(--line); border-radius:6px;
    padding:14px 16px; margin-bottom:14px; }
  .row { display:flex; gap:10px; align-items:center; flex-wrap:wrap; }
  table { border-collapse:collapse; width:100%; font-size:13px; }
  th,td { text-align:left; padding:6px 10px; border-bottom:1px solid var(--line);
    vertical-align:top; }
  th { color:var(--dim); font-weight:500; }
  code { color:var(--accent); }
  button, input { font:inherit; background:var(--card); color:var(--fg);
    border:1px solid var(--line); border-radius:4px; padding:5px 10px; }
  button { cursor:pointer; }
  .scroll { overflow-x:auto; }
  .synthetic td { color:var(--dim); }
  .err { color:#f77; }
</style></head><body>
<header>
  <h1>kontra — fleet state</h1>
  <span class="dim">Pulumi checkpoint, read-only · converge with <code>kontra fleet</code></span>
  <span class="row" style="margin-left:auto">
    <button id="refresh">refresh</button><button id="forget">forget token</button>
  </span>
</header>
<main><div id="out" class="dim">loading…</div></main>
<script>
const out = document.getElementById('out');
const KEY = 'kontra.infra.token';

function token() {
  let t = localStorage.getItem(KEY);
  if (!t) {
    // The infra routes take KONTRA_STATE_TOKEN specifically, not the workbench's token.
    t = prompt('KONTRA_STATE_TOKEN (the infra routes fail closed):') || '';
    if (t) localStorage.setItem(KEY, t);
  }
  return t;
}

async function api(path) {
  const r = await fetch(path, { headers: { Authorization: 'Bearer ' + token() } });
  if (r.status === 401 || r.status === 403) { localStorage.removeItem(KEY); throw new Error('unauthorized — token cleared, refresh to re-enter'); }
  if (!r.ok) throw new Error(path + ': HTTP ' + r.status);
  return r.json();
}

const esc = (s) => String(s).replace(/[&<>]/g, (c) => ({ '&':'&amp;','<':'&lt;','>':'&gt;' }[c]));

function detail(d) {
  const keys = Object.keys(d);
  if (!keys.length) return '<span class="dim">—</span>';
  return keys.map((k) => esc(k) + '=<code>' + esc(Array.isArray(d[k]) ? d[k].join(',') : d[k]) + '</code>').join('<br>');
}

function renderStack(s) {
  // Synthetic resources (the Stack, the providers) have no cloud counterpart. They are shown
  // because the resource count is otherwise confusing, and dimmed because they cost nothing.
  const rows = s.resources.map((r) => \`<tr class="\${r.synthetic ? 'synthetic' : ''}">
      <td><code>\${esc(r.name)}</code></td><td>\${esc(r.type)}</td>
      <td>\${esc(r.id || '—')}</td><td>\${detail(r.detail)}</td>
      <td class="dim">\${esc((r.modified || r.created || '').slice(0, 19).replace('T', ' '))}</td></tr>\`).join('');
  const outs = Object.entries(s.outputs)
    .map(([k, v]) => \`<tr><td><code>\${esc(k)}</code></td><td>\${esc(typeof v === 'object' ? JSON.stringify(v) : v)}</td></tr>\`)
    .join('');
  return \`<div class="card">
    <div class="row"><strong>\${esc(s.fqn)}</strong>
      <span class="dim">engine \${esc(s.version || '?')} · updated \${esc((s.updated||'').slice(0,19).replace('T',' '))}
        · \${s.resources.filter((r) => !r.synthetic).length} cloud resource(s)</span></div>
    <div class="scroll"><table><thead><tr><th>resource</th><th>type</th><th>id</th><th>detail</th><th>changed</th></tr></thead>
      <tbody>\${rows}</tbody></table></div>
    \${outs ? '<h3 style="font-size:13px;margin:14px 0 6px">outputs</h3><div class="scroll"><table><tbody>' + outs + '</tbody></table></div>' : ''}
  </div>\`;
}

async function load() {
  out.innerHTML = '<span class="dim">loading…</span>';
  try {
    const { stacks } = await api('/api/infra/stacks');
    if (!stacks.length) { out.innerHTML = '<div class="card dim">No stack has ever been converged. <code>kontra fleet up --count 1 --role crawl</code></div>'; return; }
    const states = await Promise.all(stacks.map((f) => api('/api/infra/stacks/' + encodeURIComponent(f) + '/state')));
    out.innerHTML = states.map(renderStack).join('');
  } catch (e) {
    out.innerHTML = '<div class="card err">' + esc(e.message) + '</div>';
  }
}

document.getElementById('refresh').onclick = load;
document.getElementById('forget').onclick = () => { localStorage.removeItem(KEY); load(); };
load();
// A converge takes minutes and the checkpoint is rewritten as it goes, so this is a live view
// rather than a snapshot somebody has to remember to reload.
setInterval(load, 10000);
</script></body></html>`;
