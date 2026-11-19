// GitForge entrypoint (launcher): the MakeFun env runs `node main.ts`.
// This file only spawns the statically linked Go binary (./forge) in the
// same directory and waits for it to exit, forwarding its exit code. The
// binary itself owns the port, the /register handshake and the /done poll
// (see main.go). The binary is built from the Go sources in this repo with:
//
//   CGO_ENABLED=0 go build -ldflags="-s -w" -o forge .
//
// (see build.sh). It is committed alongside the sources so the entry is
// self-contained: no toolchain is needed at grading time.

import { spawn } from 'node:child_process';
import { chmodSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const dir = dirname(fileURLToPath(import.meta.url));
const binary = join(dir, 'forge');

try {
  chmodSync(binary, 0o755);
} catch {
  // The exec bit is already set on the committed binary; keep going.
}

const child = spawn(binary, [], { stdio: 'inherit', env: process.env });
child.on('error', (err) => {
  process.stderr.write(`[entry] failed to start ${binary}: ${err.message}\n`);
  process.exit(1);
});
child.on('exit', (code, signal) => {
  if (signal) {
    process.kill(process.pid, signal);
    return;
  }
  process.exit(code ?? 1);
});

// If the harness stops us, stop the child too.
for (const sig of ['SIGINT', 'SIGTERM'] as const) {
  process.on(sig, () => {
    child.kill(sig);
  });
}
