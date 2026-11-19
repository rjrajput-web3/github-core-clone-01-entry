// Forge launcher: starts the statically linked Go forge binary next to this
// file and waits for it to exit. The Go program owns the whole trial
// lifecycle: it binds an ephemeral port, registers it with the suite at
// $MAKEFUN_ENV_URL, serves git smart HTTP, REST v3, GraphQL and MCP from its
// own in-memory store, polls /done, and exits 0 when the suite is finished.
//
// The binary is committed in the repository (./forge); if it is missing, the
// launcher tries to build it once from the Go sources beside it, then fails
// loudly rather than exiting silently with nothing listening.

import { spawn, spawnSync } from "node:child_process";
import { existsSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const binary = join(here, "forge");

function buildOnce() {
  const go = join(process.env.HOME ?? "", "workspace/tmp/go125/go/bin/go");
  const candidates = existsSync(go) ? [go] : ["go"];
  for (const exe of candidates) {
    const built = spawnSync(exe, ["build", "-trimpath", "-ldflags", "-s -w", "-o", binary, "."], {
      cwd: here,
      stdio: "inherit",
      env: { ...process.env, CGO_ENABLED: "0" },
    });
    if (built.status === 0 && existsSync(binary)) return true;
  }
  return false;
}

if (!existsSync(binary)) {
  process.stderr.write("[entry] forge binary missing, building once from source\n");
  if (!buildOnce()) {
    process.stderr.write("[entry] could not build the forge binary\n");
    process.exit(1);
  }
}

const child = spawn(binary, [], { stdio: "inherit", env: process.env });
process.stdout.write(`[entry] forge binary: ${binary}\n`);
child.on("error", (err) => {
  process.stderr.write(`[entry] failed to start forge: ${err.message}\n`);
  process.exit(1);
});
child.on("exit", (code, signal) => {
  if (signal) {
    process.stderr.write(`[entry] forge stopped on ${signal}\n`);
    process.exit(1);
  }
  process.exit(code ?? 1);
});
