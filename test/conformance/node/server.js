#!/usr/bin/env node
'use strict';

// Minimal HTTP server whose shutdown behaviour is selected with --mode.
//
// This mirrors the Go reference server mode for mode, so the conformance
// harness can assert that ShutdownCheck reaches the same verdict for the same
// defect regardless of stack.
//
// It also documents the trap that is specific to Node: server.close() waits for
// in-flight requests but does NOT close idle keep-alive sockets, so a server
// that only calls close() never finishes shutting down and gets killed.

const http = require('http');

function parseArgs(argv) {
  const args = { addr: '127.0.0.1:0', mode: 'correct', work: 60, 'lame-duck': 400 };
  for (let i = 0; i < argv.length; i += 2) {
    const key = argv[i].replace(/^--?/, '');
    args[key] = argv[i + 1];
  }
  return args;
}

const args = parseArgs(process.argv.slice(2));
const [host, port] = args.addr.split(':');
const workMs = Number(args.work);
const lameDuckMs = Number(args['lame-duck']);
const mode = args.mode;

let ready = true;
let stalling = false;

const server = http.createServer((req, res) => {
  if (req.url === '/readyz') {
    res.writeHead(ready ? 200 : 503).end();
    return;
  }

  if (stalling) {
    // Accepted and never answered: the caller waits out its whole timeout,
    // which is worse for it than failing fast.
    return;
  }

  setTimeout(() => {
    res.writeHead(200, { 'Content-Type': 'text/plain' }).end('ok\n');
  }, workMs);
});

server.listen(Number(port), host, () => {
  const bound = server.address();
  console.log(`listening on ${bound.address}:${bound.port}`);
});

const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

// drainGracefully is the correct sequence. closeIdleConnections is the part
// people miss: without it close() waits forever on sockets nobody is using.
function drainGracefully() {
  return new Promise((resolve) => {
    server.close(resolve);
    if (server.closeIdleConnections) {
      server.closeIdleConnections();
    }
  });
}

if (mode === 'ignore-signal') {
  // Installing a handler that does nothing is how a service ends up needing
  // SIGKILL; the default disposition would at least have exited.
  process.on('SIGTERM', () => {});
  setInterval(() => {}, 1 << 30);
} else {
  process.on('SIGTERM', onSigterm);
  process.on('SIGINT', onSigterm);
}

async function onSigterm() {
  switch (mode) {
    case 'instant-close':
      // Readiness never flips and the listener shuts at once, so traffic still
      // being routed here is refused.
      await drainGracefully();
      break;

    case 'no-readiness-flip':
      await sleep(lameDuckMs);
      await drainGracefully();
      break;

    case 'abrupt-reset':
      ready = false;
      // Close immediately: destroys live connections rather than letting them finish.
      server.close();
      if (server.closeAllConnections) {
        server.closeAllConnections();
      }
      break;

    case 'slow-drain':
      ready = false;
      await sleep(lameDuckMs);
      await sleep(120000);
      await drainGracefully();
      break;

    case 'early-exit':
      ready = false;
      await sleep(lameDuckMs);
      process.exit(0);
      break;

    case 'listener-never-closes':
      ready = false;
      setInterval(() => {}, 1 << 30);
      break;

    case 'slow-readiness':
      await sleep(3000);
      ready = false;
      await drainGracefully();
      break;

    case 'readiness-flap':
      ready = false;
      await sleep(300);
      // Recovering re-registers a terminating instance with the balancer.
      ready = true;
      await sleep(lameDuckMs);
      await drainGracefully();
      break;

    case 'nonzero-exit':
      ready = false;
      await sleep(lameDuckMs);
      await drainGracefully();
      process.exit(3);
      break;

    case 'accept-no-response':
      ready = false;
      stalling = true;
      setInterval(() => {}, 1 << 30);
      break;

    case 'close-without-idle':
      // The Node-specific trap: correct-looking, but idle keep-alive sockets
      // keep the callback from ever firing.
      ready = false;
      await sleep(lameDuckMs);
      server.close(() => process.exit(0));
      break;

    default:
      // 1. signal arrived  2. stop advertising readiness  3. keep serving while
      // de-registration propagates  4-6. stop accepting and drain  7. exit.
      ready = false;
      await sleep(lameDuckMs);
      await drainGracefully();
      process.exit(0);
  }
}
