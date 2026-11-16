// main.ts — entrypoint. Binds an ephemeral port, registers it with the trial
// environment (when MAKEFUN_ENV_URL is set), then long-polls /done until the
// trial finishes. Without MAKEFUN_ENV_URL it just serves on a printed port,
// which is the local-development mode.

import type { AddressInfo } from 'node:net';
import { Store } from './store.ts';
import { createApp } from './app.ts';

const sleep = (ms: number): Promise<void> => new Promise((resolve) => setTimeout(resolve, ms));

async function main(): Promise<void> {
  const store = new Store();
  const server = createApp(store);
  const envUrl = (process.env.MAKEFUN_ENV_URL ?? '').replace(/\/+$/, '');

  await new Promise<void>((resolve) => {
    server.listen(0, '127.0.0.1', () => resolve());
  });
  const address = server.address() as AddressInfo;
  const port = address.port;

  if (envUrl === '') {
    console.log(`forge listening on http://127.0.0.1:${port}`);
    return;
  }

  // Register this port with the environment; the first registration wins.
  for (let attempt = 0; attempt < 60; attempt++) {
    try {
      const res = await fetch(`${envUrl}/register`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ port }),
      });
      if (res.ok || res.status === 409) break;
    } catch {
      // The environment may not be listening yet; keep retrying.
    }
    await sleep(500);
  }

  // Long-poll until the trial is done, then exit cleanly.
  for (;;) {
    try {
      const res = await fetch(`${envUrl}/done`);
      if (res.ok) {
        const data = (await res.json()) as { done?: unknown };
        if (data.done === true) break;
      }
    } catch {
      // Transient; keep polling.
    }
    await sleep(1000);
  }

  await new Promise<void>((resolve) => {
    server.close(() => resolve());
  });
  process.exit(0);
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
