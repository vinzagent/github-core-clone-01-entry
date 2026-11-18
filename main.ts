// main.ts — a thin launcher. The forge itself is a statically linked Go
// binary committed next to this file as ./forge-bin (built from the Go
// sources in this repository: main.go, app.go, forge.go, ops.go, rest.go,
// gitproto.go, graphql.go, mcp.go, web.go). This file only spawns that
// binary with the same environment and stdio, forwards its exit code, and
// dies with it. All behavior — the HTTP surfaces and the env lifecycle
// (register /done) — lives in the binary.

import { spawn } from 'node:child_process';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const binary = join(dirname(fileURLToPath(import.meta.url)), 'forge-bin');
const child = spawn(binary, [], { stdio: 'inherit', env: process.env });

child.on('error', (err) => {
  process.stderr.write(`fatal: ${err.message}\n`);
  process.exit(1);
});

child.on('exit', (code, signal) => {
  if (signal) {
    process.kill(process.pid, signal);
    return;
  }
  process.exit(code ?? 1);
});
