// node loopback_check.cjs <loopback.cjs>: checks the D185 preload without binding anything. The real
// net.Server.prototype.listen is replaced by a recorder before the preload wraps it; each call's arguments, as the
// caller gave them and as the preload passes them on, are read the way Node's listen reads them (net._normalizeArgs,
// then listen's own rules for an arbitrary port and the backlog). A TCP listen Node would bind on every interface must
// come out bound to 127.0.0.1 with the same port, backlog and callback; every other call must mean what it meant.
// Exits 1, naming each form that fails.
'use strict';
const net = require('net');
const path = require('path');
const util = require('util');

let passed;
net.Server.prototype.listen = function (...args) {
  passed = args;
  return this;
};
require(path.resolve(process.argv[2]));

const toNumber = (x) => ((x = Number(x)) >= 0 ? x : false);

// binds is what Node's net.Server.prototype.listen (v22, lib/net.js) does with args: a TCP bind on host:port ('' is
// every interface), a path, a handle, or a refusal.
function binds(args) {
  const [options, cb] = net._normalizeArgs(args);
  const o = options._handle || options.handle || options;
  if (o !== options || typeof o.fd === 'number') return { handle: true, cb };
  const backlog = o.backlog || toNumber(args.length > 1 && args[1]) || toNumber(args.length > 2 && args[2]) || 0;
  let port = o.port;
  if (args.length === 0 || typeof args[0] === 'function' || (port === undefined && 'port' in o) || port === null) port = 0;
  if (typeof port === 'number' || typeof port === 'string') return { tcp: true, host: o.host || '', port: Number(port), backlog, cb };
  if (o.path) return { path: o.path, backlog, cb };
  return { refused: true };
}

const fn = () => {};
const forms = {
  'listen()': [],
  'listen(3000)': [3000],
  "listen('3000')": ['3000'],
  'listen(3000, cb)': [3000, fn],
  'listen(3000, 511)': [3000, 511],
  'listen(3000, 511, cb)': [3000, 511, fn],
  "listen(3000, '')": [3000, ''],
  "listen(3000, '', 511, cb)": [3000, '', 511, fn],
  'listen(3000, null, 511)': [3000, null, 511],
  'listen(3000, undefined, cb)': [3000, undefined, fn],
  'listen(null)': [null],
  'listen(undefined)': [undefined],
  'listen(cb)': [fn],
  'listen(null, cb)': [null, fn],
  'listen({port: 3000})': [{ port: 3000 }],
  "listen({port: 3000, host: ''})": [{ port: 3000, host: '' }],
  'listen({port: 3000, host: null})': [{ port: 3000, host: null }],
  'listen({port: 3000, host: undefined})': [{ port: 3000, host: undefined }],
  'listen({port: 3000, backlog: 5}, cb)': [{ port: 3000, backlog: 5 }, fn],
  'listen({port: undefined})': [{ port: undefined }],
  'listen({port: null}, cb)': [{ port: null }, fn],
  // Left as they are: a named host, a path, a handle, and {} (which Node refuses).
  "listen(3000, 'localhost')": [3000, 'localhost'],
  "listen(3000, '::1', 511, cb)": [3000, '::1', 511, fn],
  "listen({port: 3000, host: '0.0.0.0'})": [{ port: 3000, host: '0.0.0.0' }],
  "listen('/tmp/x.sock')": ['/tmp/x.sock'],
  "listen({path: '/tmp/x.sock'})": [{ path: '/tmp/x.sock' }],
  'listen({fd: 3})': [{ fd: 3 }],
  'listen({})': [{}],
};

let failed = 0;
for (const [form, args] of Object.entries(forms)) {
  passed = undefined;
  new net.Server().listen(...args);
  const before = binds(args);
  const after = passed && binds(passed);
  const want = before.tcp && before.host === '' ? Object.assign({}, before, { host: '127.0.0.1' }) : before;
  if (!after || !util.isDeepStrictEqual(after, want)) {
    failed++;
    console.log(`${form}: passed on ${util.inspect(passed)}, which binds ${util.inspect(after)}; want ${util.inspect(want)}`);
  }
}
if (failed) {
  console.log(`${failed} of ${Object.keys(forms).length} forms fail`);
  process.exit(1);
}
console.log(`ok: ${Object.keys(forms).length} forms`);
