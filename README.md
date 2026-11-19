# GitForge — Standalone GitHub-Compatible Git Forge

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/rjrajput-web3/github-core-clone-01-entry/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/rjrajput-web3/github-core-clone-01-entry/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A fast, dependency-free git forge for [the contest](https://make.fun/bounty/github-core-clone-01):
git smart HTTP (v0 + v2, upload-pack and receive-pack), GitHub REST v3, GraphQL v4
and MCP over streamable HTTP — all in one process, all in memory, tuned for
the contest's four timed workloads (push, clone, read, write).

The core is a statically linked Go binary (`forge`, built from the `.go` sources
in this repo with `CGO_ENABLED=0 go build -ldflags="-s -w"`); `main.ts` is a thin
Node launcher the platform runs (`node /bot/main.ts`), which spawns `./forge`
and forwards its exit code. Go's goroutines serve the 50-way concurrent clones
in parallel, and its pack reader recovers each deflated entry's length exactly
with a counting reader — no re-deflate, no binary search.

## Run it

```sh
node --experimental-strip-types main.ts
```

The platform runs `node /bot/main.ts` on Node 22.18 or later, which strips TypeScript
types on its own. There is nothing to install: the launcher imports
`node:child_process`, `node:fs` and `node:path` only, and the binary is
self-contained (statically linked, no libc, no network at run time).

To rebuild the binary from source (Go 1.25+):

```sh
./build.sh
```

## The lifecycle (main.ts + main.go)

The env is a server the harness starts beside yours. Its origin is in
`MAKEFUN_ENV_URL` (`http://store:8080` in a trial).

1. **Bind an ephemeral port.** The binary listens on `127.0.0.1:0` and lets the
   kernel pick one. Never bind 8080: that is the env's port, and on the
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
`MAKEFUN_ENV_URL` is unset there is nothing to talk to, so the binary exits 1
at once.

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

- **Go core, parallel clones:** the 50 concurrent clone exchanges run on
  goroutines across the available CPUs; the single-threaded Node version
  serialized them.
- **Exact pack lengths:** the pack reader counts the bytes a `compress/zlib`
  stream consumes (via an `io.ByteReader`), so each entry is inflated once —
  no re-deflate, no binary search.
- **Response caching:** per-issue JSON cached and invalidated on mutation;
  per-repo fetch and ls-refs responses cached and invalidated on ref changes;
  v0/v2 advertisements cached per repo.
- **A fast git-object layer:** a content-addressed store with lazy commit/tree
  parsing, memoized tree flattening, and delta application for pushed packs.
- **Minimal per-request allocation:** manual URL and query parsing on hot paths,
  `Content-Length` set on every response, no framework, no dependencies.

## Layout

- `main.ts`: the Node launcher — spawns `./forge`, forwards its exit code.
- `main.go`: the binary's entrypoint — the lifecycle above.
- `http.go`: the HTTP server and the REST v3 routes.
- `store.go`: the in-memory forge state (objects, refs, issues, pulls, labels).
- `git.go`: pkt-lines, pack reading/writing, and the smart-HTTP handlers.
- `graphql.go`: a small GraphQL parser and executor for the v4 documents.
- `mcp.go`: the MCP tools over JSON-RPC 2.0.
- `forge`: the statically linked binary built from the `.go` sources
  (`./build.sh` reproduces it byte-for-byte modulo the build ID).
- `package.json`, `build.sh`, `go.mod`, `LICENSE` (MIT): metadata and licence.

## Referral

Built on Make.fun — referral code `9hqpawrx`.
