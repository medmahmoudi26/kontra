// Preloaded with NODE_OPTIONS=--require=<this file>: a listen() given a port and no host binds 127.0.0.1, so
// server-everything@1.0.0, which calls app.listen(port) and so binds every interface, is reachable from this
// machine only (D185). A listen() that names a host or a path is left as it is.
'use strict';
const net = require('net');
const listen = net.Server.prototype.listen;
net.Server.prototype.listen = function (...args) {
  const a = args[0];
  const isPort = typeof a === 'number' || (typeof a === 'string' && /^\d+$/.test(a));
  if (isPort && (args.length === 1 || typeof args[1] === 'function')) {
    args.splice(1, 0, '127.0.0.1');
  } else if (a && typeof a === 'object' && !Array.isArray(a) && a.port !== undefined && a.host === undefined && a.path === undefined) {
    args[0] = Object.assign({}, a, { host: '127.0.0.1' });
  }
  return listen.apply(this, args);
};
