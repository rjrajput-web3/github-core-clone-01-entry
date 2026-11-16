# GitForge — Standalone GitHub-Compatible Git Forge

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/rjrajput-web3/github-core-clone-01-entry/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/rjrajput-web3/github-core-clone-01-entry/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A fast, dependency-free git forge for [the contest](https://make.fun/bounty/github-core-clone-01):
git smart HTTP (v0 + v2, upload-pack and receive-pack), GitHub REST v3, GraphQL v4
and MCP over streamable HTTP — all in one Node process, all in memory, tuned for
the contest's four timed workloads (push, clone, read, write).

## Run it

```sh
node --experimental-strip-types main.ts
```

The platform runs `node /bot/main.ts` on Node 22.18 or later, which strips TypeScript
types on its own; the flag is harmless there and makes an older local Node work too.
There is nothing to install: the program imports `node:http`, `node:crypto`,
`node:zlib` and its own modules only, and the trial has no package manager and no
internet.

## The lifecycle (main.ts)

The env is a server the harness starts beside yours. Its origin is in
`MAKEFUN_ENV_URL` (`http://store:8080` in a trial).

1. **Bind an ephemeral port.** `server.listen(0)` lets the kernel pick one; read it
   with `server.address().port`. Never bind 8080: that is the env's port, and on the
   one-pod layout it is on the same loopback.
2. **Register.** `POST $MAKEFUN_ENV_URL/register` with `{"port": <the port you bound>}`,
   as JSON. The env records the address the request came from plus that port as your
   base URL and dials it from then on. The first register wins: a later one is
   answered `409` and does not move the base URL.
3. **Wait for done.** `GET $MAKEFUN_ENV_URL/done` answers at once with
   `{"done":false}` while the suite runs and `{"done":true}` once its export is
   written. Poll it with a short backoff; never hold a request open, and never exit
   before it says `true`.
4. **Exit 0** when it does. Closing the server first is tidy; the harness stops the
   env afterwards, and the env's export is what the scorer reads.

A failed poll or register is retried with a short exponential backoff. If
`MAKEFUN_ENV_URL` is unset there is nothing to talk to, so `main.ts` exits 1 at once.

## The surface

| Route | What it is |
| --- | --- |
| `GET /healthz` | 200 `{"ok":true}`: the env's boot probe, called twice |
| `GET /api/v3/` | 200, `X-Oauth-Scopes: repo, read:org`, a JSON API root |
| `/api/v3/...` | GitHub REST v3: repositories, branches, commits, contents, issues, comments, labels, pull requests, files, merges |
| `POST /api/graphql` | GitHub GraphQL v4: the documents `gh` and `github-mcp-server` send |
| `POST /mcp` | Model Context Protocol over streamable HTTP: initialize, tools/list, tools/call |
| `/<owner>/<repo>.git/...` | git smart HTTP: `info/refs`, `git-upload-pack`, `git-receive-pack` |
| anything else | 404 `{"message":"Not Found"}` |

## How it is fast

- **Response caching:** per-issue JSON strings cached and invalidated on mutation;
  per-repo fetch and ls-refs responses cached and invalidated on ref changes;
  v0/v2 advertisements cached per repo.
- **A fast git-object layer:** a content-addressed store with lazy commit/tree
  parsing, memoized tree flattening, and a pack reader that recovers each deflated
  object's length by re-deflating (Node's deflate is deterministic, so the
  re-deflated bytes are byte-identical).
- **Minimal per-request allocation:** manual URL and query parsing on hot paths,
  shared header objects, `setNoDelay` on every socket, no framework, no
  dependencies.

## Layout

- `main.ts`: the lifecycle above.
- `app.ts`: the HTTP server and the REST v3 routes.
- `store.ts`: the in-memory forge state (objects, refs, issues, pulls, labels).
- `git.ts`: pkt-lines, pack reading/writing, and the smart-HTTP handlers.
- `graphql.ts`: a small GraphQL parser and executor for the v4 documents.
- `mcp.ts`: the MCP tools over JSON-RPC 2.0.
- `package.json`, `tsconfig.json`, `LICENSE` (MIT): the entry's metadata and licence.

## Referral

Built on Make.fun — referral code `9hqpawrx`.
