// Preloaded with NODE_OPTIONS=--require=<this file>: a TCP listen() that names no host, which Node binds on every
// interface, binds 127.0.0.1 instead, so server-everything@1.0.0, which calls app.listen(port), is reachable from this
// machine only (D185). Every such form is covered: listen(), listen(cb), listen(null|undefined[, cb]),
// listen(port[, backlog][, cb]), listen(port, ''|null|undefined[, backlog][, cb]) and listen({port, host: <none, '' or
// null>, ...}[, cb]), where a port that is omitted, undefined or null is Node's arbitrary port, 0; the port, backlog
// and callback are kept. A listen() that names a host, a path (a pipe or a Unix socket), a handle or an fd is left as
// it is, and so is {} with neither port nor path, which Node refuses.
'use strict';
const net = require('net');
const listen = net.Server.prototype.listen;
const LOOPBACK = '127.0.0.1';

// Node's own reading (lib/net.js): a string that is not a non-negative number names a path.
const toNumber = (x) => ((x = Number(x)) >= 0 ? x : false);
const isPipeName = (s) => typeof s === 'string' && toNumber(s) === false;

function loopback(args) {
  const n = args.length;
  const a = args[0];
  if (a !== null && typeof a === 'object') {
    // (options[, cb]) or (handle[, backlog][, cb])
    if (a._handle || a.handle || typeof a.fd === 'number') return args;
    const tcp = typeof a.port === 'number' || typeof a.port === 'string' || a.port === null || ('port' in a && a.port === undefined);
    if (!tcp || a.host) return args;
    const o = Object.assign({}, a, { host: LOOPBACK });
    if (o.port === undefined || o.port === null) o.port = 0;
    return [o, ...args.slice(1)];
  }
  if (isPipeName(a)) return args; // (path[, backlog][, cb])
  // ([port][, host][, backlog][, cb]): Node takes args[1] as the host only when it is a string.
  if (n > 1 && typeof args[1] === 'string' && args[1] !== '') return args;
  const port = n === 0 || a === undefined || a === null || typeof a === 'function' ? 0 : a;
  const backlog = toNumber(n > 1 && args[1]) || toNumber(n > 2 && args[2]);
  const out = [port, LOOPBACK];
  if (backlog) out.push(backlog);
  if (n > 0 && typeof args[n - 1] === 'function') out.push(args[n - 1]);
  return out;
}

net.Server.prototype.listen = function (...args) {
  return listen.apply(this, loopback(args));
};
