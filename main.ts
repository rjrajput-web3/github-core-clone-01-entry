// Launcher: starts the forge binary, registers its port with the trial
// environment, then waits until the environment reports the trial is done.
// Written as plain JavaScript inside the .ts file so it runs under any
// Node 22 without type-stripping flags.
import { spawn } from "node:child_process";
import { fileURLToPath } from "node:url";
import path from "node:path";

const here = path.dirname(fileURLToPath(import.meta.url));
const forgePath = path.join(here, "forge");
const envUrl = process.env.MAKEFUN_ENV_URL;

const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

async function postJson(url, body) {
  const res = await fetch(url, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  return res;
}

async function main() {
  const forge = spawn(forgePath, [], {
    stdio: ["ignore", "pipe", "inherit"],
    env: { ...process.env },
  });

  let port = 0;
  const portPromise = new Promise((resolve, reject) => {
    let buf = "";
    forge.stdout.on("data", (chunk) => {
      buf += chunk.toString();
      const m = buf.match(/FORGE_PORT=(\d+)/);
      if (m) {
        port = Number(m[1]);
        resolve(port);
      }
    });
    forge.on("error", reject);
    forge.on("exit", (code) => {
      if (!port) reject(new Error("forge exited before reporting a port (code " + code + ")"));
    });
  });

  if (!envUrl) {
    // No trial environment: run the forge in the foreground.
    forge.stdout.pipe(process.stdout);
    const code = await new Promise((resolve) => forge.on("exit", resolve));
    process.exit(code ?? 0);
  }

  try {
    await portPromise;
  } catch (err) {
    console.error("[launcher] forge failed to start:", err.message);
    process.exit(1);
  }

  // Register the port with the trial environment, retrying briefly.
  let registered = false;
  for (let attempt = 0; attempt < 30 && !registered; attempt++) {
    try {
      const res = await postJson(envUrl + "/register", { port });
      if (res.ok || res.status === 200 || res.status === 204) {
        registered = true;
      } else {
        await sleep(500);
      }
    } catch {
      await sleep(500);
    }
  }
  if (!registered) {
    console.error("[launcher] could not register port with the environment");
    forge.kill();
    process.exit(1);
  }

  // Long-poll until the trial is done.
  while (true) {
    try {
      const res = await fetch(envUrl + "/done");
      if (res.ok) {
        const body = await res.json();
        if (body && body.done === true) break;
      }
    } catch {
      // Transient network hiccup: keep polling.
    }
    await sleep(250);
  }

  forge.kill();
  process.exit(0);
}

main();
